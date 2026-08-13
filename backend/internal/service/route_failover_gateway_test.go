package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type routeFailoverGatewayGroupRepo struct {
	GroupRepository
	groups map[int64]*Group
}

func (r routeFailoverGatewayGroupRepo) GetByID(_ context.Context, id int64) (*Group, error) {
	return r.GetByIDLite(context.Background(), id)
}

func (r routeFailoverGatewayGroupRepo) GetByIDLite(_ context.Context, id int64) (*Group, error) {
	group := r.groups[id]
	if group == nil {
		return nil, ErrGroupNotFound
	}
	clone := *group
	return &clone, nil
}

type routeFailoverGatewayAccountRepo struct {
	AccountRepository
	byGroup map[int64][]Account
}

func (r routeFailoverGatewayAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, groupID int64, _ string) ([]Account, error) {
	return append([]Account(nil), r.byGroup[groupID]...), nil
}

func (r routeFailoverGatewayAccountRepo) ListSchedulableByGroupIDAndPlatforms(_ context.Context, groupID int64, _ []string) ([]Account, error) {
	return append([]Account(nil), r.byGroup[groupID]...), nil
}

func (r routeFailoverGatewayAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	for _, accounts := range r.byGroup {
		for i := range accounts {
			if accounts[i].ID == id {
				clone := accounts[i]
				return &clone, nil
			}
		}
	}
	return nil, ErrAccountNotFound
}

type routeFailoverGatewayCircuit struct {
	allows   int
	targets  []int64
	failures int
}

func (c *routeFailoverGatewayCircuit) Allow(_ context.Context, effectiveGroupID int64, _ string) (bool, bool, string, error) {
	c.allows++
	c.targets = append(c.targets, effectiveGroupID)
	return true, false, "lease", nil
}

func (c *routeFailoverGatewayCircuit) RecordSuccess(context.Context, int64, string, string) error {
	return nil
}

func (c *routeFailoverGatewayCircuit) RecordFailure(context.Context, int64, string, string) error {
	c.failures++
	return nil
}

func TestGatewaySelectionNeverChangesRequestedGroup(t *testing.T) {
	groups, planner, circuit := routeFailoverBoundaryFixture(t, PlatformAnthropic)
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		2: {{ID: 22, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	svc := &GatewayService{accountRepo: accounts, groupRepo: groups, routeFailoverPlanner: planner}
	groupID := int64(1)

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "claude-sonnet", nil, "", 10)

	require.Error(t, err)
	require.Nil(t, selection)
	require.Zero(t, circuit.allows)
}

func TestOpenAISelectionNeverChangesRequestedGroup(t *testing.T) {
	_, planner, circuit := routeFailoverBoundaryFixture(t, PlatformOpenAI)
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		2: {{ID: 22, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	svc := &OpenAIGatewayService{accountRepo: accounts, routeFailoverPlanner: planner}
	groupID := int64(1)

	selection, _, err := svc.SelectAccountWithSchedulerForCapabilityWithRouteFailover(
		context.Background(), &groupID, "", "", "gpt-source", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, true, PlatformOpenAI,
	)

	require.Error(t, err)
	require.Nil(t, selection)
	require.Zero(t, circuit.allows)
}

func TestGroupLocalExhaustionReturnsNoAvailableAccounts(t *testing.T) {
	groups, planner, circuit := routeFailoverBoundaryFixture(t, PlatformAnthropic)
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		2: {{ID: 22, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	svc := &GatewayService{accountRepo: accounts, groupRepo: groups, routeFailoverPlanner: planner}
	groupID := int64(1)

	selection, err := svc.SelectAccountForModelWithRouteFailover(context.Background(), &groupID, "", "claude-sonnet", nil)

	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, selection)
	require.Zero(t, circuit.allows)
}

func TestRouteFailoverCircuitFailureScopeOnlyCountsProviderErrors(t *testing.T) {
	for _, test := range []struct {
		name  string
		scope GatewayFailureScope
		want  bool
	}{
		{name: "unspecified", want: false},
		{name: "route", scope: GatewayFailureScopeRoute, want: true},
		{name: "provider", scope: GatewayFailureScopeProvider, want: true},
		{name: "account", scope: GatewayFailureScopeAccount, want: false},
		{name: "request", scope: GatewayFailureScopeRequest, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := &UpstreamFailoverError{Scope: test.scope}
			require.Equal(t, test.want, err.ShouldRecordRouteFailover())
		})
	}
}

func routeFailoverBoundaryFixture(t *testing.T, platform string) (routeFailoverGatewayGroupRepo, *RouteFailoverPlanner, *routeFailoverGatewayCircuit) {
	t.Helper()
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: platform, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: platform, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
		Targets: []RouteFailoverTarget{{ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true}},
	}}}}
	circuit := &routeFailoverGatewayCircuit{}
	planner := NewRouteFailoverPlanner(policyRepo, groups, circuit, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	return groups, planner, circuit
}
