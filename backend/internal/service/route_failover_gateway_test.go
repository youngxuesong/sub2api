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

type routeFailoverGatewayChannelRepo struct {
	ChannelRepository
	channels  []Channel
	platforms map[int64]string
}

func (r routeFailoverGatewayChannelRepo) ListAll(context.Context) ([]Channel, error) {
	return append([]Channel(nil), r.channels...), nil
}

func (r routeFailoverGatewayChannelRepo) GetGroupPlatforms(context.Context, []int64) (map[int64]string, error) {
	return r.platforms, nil
}

type routeFailoverGatewayCircuit struct {
	successes int
	failures  int
	allows    int
	targets   []int64
}

func TestReleaseUnadmittedRouteSelectionReleasesOnce(t *testing.T) {
	releases := 0
	selection := &AccountSelectionResult{ReleaseFunc: func() { releases++ }}

	releaseUnadmittedRouteSelection(selection)
	releaseUnadmittedRouteSelection(selection)

	require.Equal(t, 1, releases)
	require.Nil(t, selection.ReleaseFunc)
}

func (c *routeFailoverGatewayCircuit) Allow(_ context.Context, effectiveGroupID int64, _ string) (bool, bool, string, error) {
	c.allows++
	c.targets = append(c.targets, effectiveGroupID)
	return true, false, "lease", nil
}

func TestGatewayRouteFailoverAcquiresLeaseOnlyForAttemptedCandidate(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		3: {ID: 3, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 3,
		Targets: []RouteFailoverTarget{
			{ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true},
			{ID: 8, TargetGroupID: 3, Priority: 2, Enabled: true},
		},
	}}}}
	circuit := &routeFailoverGatewayCircuit{}
	planner := NewRouteFailoverPlanner(policyRepo, groups, circuit, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		2: {{ID: 22, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
		3: {{ID: 33, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	svc := &GatewayService{accountRepo: accounts, groupRepo: groups, routeFailoverPlanner: planner}
	groupID := int64(1)

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "claude-sonnet", nil, "", 10)

	require.NoError(t, err)
	require.Equal(t, int64(22), selection.Account.ID)
	require.Equal(t, []int64{2}, circuit.targets)
}

func TestGatewayRouteFailoverSkipsCapacityFailuresWithoutTouchingCircuit(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		3: {ID: 3, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 3,
		Targets: []RouteFailoverTarget{
			{ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true},
			{ID: 8, TargetGroupID: 3, Priority: 2, Enabled: true},
		},
	}}}}
	circuit := &routeFailoverGatewayCircuit{}
	planner := NewRouteFailoverPlanner(policyRepo, groups, circuit, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		3: {{ID: 33, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	svc := &GatewayService{accountRepo: accounts, groupRepo: groups, routeFailoverPlanner: planner}
	groupID := int64(1)

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "claude-sonnet", nil, "", 10)

	require.NoError(t, err)
	require.Equal(t, int64(33), selection.Account.ID)
	require.Equal(t, []int64{3}, circuit.targets)
	require.Zero(t, circuit.failures)
}

func (c *routeFailoverGatewayCircuit) RecordSuccess(context.Context, int64, string, string) error {
	c.successes++
	return nil
}
func (c *routeFailoverGatewayCircuit) RecordFailure(context.Context, int64, string, string) error {
	c.failures++
	return nil
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

func TestGatewayRouteFailoverUsesFallbackOnlyWhenPrimaryHasNoAccounts(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Version: 4, Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
		Targets: []RouteFailoverTarget{{ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true}},
	}}}}
	circuit := &routeFailoverGatewayCircuit{}
	planner := NewRouteFailoverPlanner(policyRepo, groups, circuit, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		2: {{ID: 22, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	svc := &GatewayService{accountRepo: accounts, groupRepo: groups, routeFailoverPlanner: planner}
	groupID := int64(1)

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "claude-sonnet", nil, "", 10)
	require.NoError(t, err)
	require.Equal(t, int64(22), selection.Account.ID)
	require.Equal(t, int64(2), selection.EffectiveGroupID)
	require.Equal(t, int64(7), selection.RouteTargetID)
	require.Equal(t, "claude-sonnet", selection.EffectiveModel)
	require.Zero(t, circuit.successes, "selecting an account is not an upstream success")
	svc.RecordRouteFailoverSuccess(context.Background(), selection)
	require.Equal(t, 1, circuit.successes)
}

func TestGatewayRouteFailoverDoesNotResolveFallbackWhenPrimaryIsHealthy(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
		Targets: []RouteFailoverTarget{{ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true}},
	}}}}
	circuit := &routeFailoverGatewayCircuit{}
	planner := NewRouteFailoverPlanner(policyRepo, groups, circuit, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		1: {{ID: 11, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	svc := &GatewayService{accountRepo: accounts, groupRepo: groups, routeFailoverPlanner: planner}
	groupID := int64(1)

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "claude-sonnet", nil, "", 10)
	require.NoError(t, err)
	require.Equal(t, int64(11), selection.Account.ID)
	require.Zero(t, circuit.allows, "healthy primary requests must not query fallback circuit state")
}

func TestGatewayRouteFailoverSelectsMappedFallbackForCountTokens(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
		Targets: []RouteFailoverTarget{{
			ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true,
			ModelMapping: map[string]string{"claude-source": "claude-fallback"},
		}},
	}}}}
	planner := NewRouteFailoverPlanner(policyRepo, groups, &routeFailoverGatewayCircuit{}, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		2: {{ID: 22, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	svc := &GatewayService{accountRepo: accounts, groupRepo: groups, routeFailoverPlanner: planner}
	groupID := int64(1)

	selection, err := svc.SelectAccountForModelWithRouteFailover(context.Background(), &groupID, "", "claude-source", nil)
	require.NoError(t, err)
	require.Equal(t, int64(22), selection.Account.ID)
	require.Equal(t, int64(2), selection.EffectiveGroupID)
	require.Equal(t, "claude-fallback", selection.EffectiveModel)
	require.Equal(t, int64(7), selection.RouteTargetID)
}

func TestGatewayRouteFailoverAppliesTargetGroupChannelMapping(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
		Targets: []RouteFailoverTarget{{
			ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true,
			ModelMapping: map[string]string{"claude-source": "claude-route"},
		}},
	}}}}
	planner := NewRouteFailoverPlanner(policyRepo, groups, &routeFailoverGatewayCircuit{}, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		2: {{ID: 22, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	channelService := NewChannelService(routeFailoverGatewayChannelRepo{
		channels: []Channel{{
			ID: 5, Status: StatusActive, GroupIDs: []int64{2},
			ModelMapping: map[string]map[string]string{
				PlatformAnthropic: {"claude-route": "claude-target"},
			},
		}},
		platforms: map[int64]string{2: PlatformAnthropic},
	}, groups, nil, nil)
	svc := &GatewayService{
		accountRepo: accounts, groupRepo: groups, channelService: channelService,
		routeFailoverPlanner: planner,
	}
	groupID := int64(1)

	selection, err := svc.SelectAccountForModelWithRouteFailover(context.Background(), &groupID, "", "claude-source", nil)

	require.NoError(t, err)
	require.Equal(t, "claude-target", selection.EffectiveModel)
}

func TestOpenAIGatewayRouteFailoverSelectsMappedFallback(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
		Targets: []RouteFailoverTarget{{
			ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true,
			ModelMapping: map[string]string{"gpt-source": "gpt-fallback"},
		}},
	}}}}
	planner := NewRouteFailoverPlanner(policyRepo, groups, &routeFailoverGatewayCircuit{}, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
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
	require.NoError(t, err)
	require.Equal(t, int64(22), selection.Account.ID)
	require.Equal(t, int64(2), selection.EffectiveGroupID)
	require.Equal(t, "gpt-fallback", selection.EffectiveModel)
	require.Equal(t, int64(7), selection.RouteTargetID)
}

func TestOpenAIGatewayRouteFailoverAppliesTargetGroupChannelMapping(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
		Targets: []RouteFailoverTarget{{
			ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true,
			ModelMapping: map[string]string{"gpt-source": "gpt-route"},
		}},
	}}}}
	planner := NewRouteFailoverPlanner(policyRepo, groups, &routeFailoverGatewayCircuit{}, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		2: {{ID: 22, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	channelService := NewChannelService(routeFailoverGatewayChannelRepo{
		channels: []Channel{{
			ID: 5, Status: StatusActive, GroupIDs: []int64{2},
			ModelMapping: map[string]map[string]string{
				PlatformOpenAI: {"gpt-route": "gpt-target"},
			},
		}},
		platforms: map[int64]string{2: PlatformOpenAI},
	}, groups, nil, nil)
	svc := &OpenAIGatewayService{
		accountRepo: accounts, channelService: channelService, routeFailoverPlanner: planner,
	}
	groupID := int64(1)

	selection, _, err := svc.SelectAccountWithSchedulerForCapabilityWithRouteFailover(
		context.Background(), &groupID, "", "", "gpt-source", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, true, PlatformOpenAI,
	)

	require.NoError(t, err)
	require.Equal(t, "gpt-target", selection.EffectiveModel)
}

func TestOpenAIGatewayRouteFailoverSkipsCapacityFailuresWithoutTouchingCircuit(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		3: {ID: 3, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 3,
		Targets: []RouteFailoverTarget{
			{ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true},
			{ID: 8, TargetGroupID: 3, Priority: 2, Enabled: true},
		},
	}}}}
	circuit := &routeFailoverGatewayCircuit{}
	planner := NewRouteFailoverPlanner(policyRepo, groups, circuit, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	svc := &OpenAIGatewayService{
		accountRepo: routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
			3: {{ID: 33, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
		}},
		routeFailoverPlanner: planner,
	}
	groupID := int64(1)

	selection, _, err := svc.SelectAccountWithSchedulerForCapabilityWithRouteFailover(
		context.Background(), &groupID, "", "", "gpt-source", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, true, PlatformOpenAI,
	)

	require.NoError(t, err)
	require.Equal(t, int64(33), selection.Account.ID)
	require.Equal(t, []int64{3}, circuit.targets)
	require.Zero(t, circuit.failures)
}

func TestOpenAIGatewayCapabilitySchedulerDoesNotOptIntoRouteFailover(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
		Targets: []RouteFailoverTarget{{ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true}},
	}}}}
	circuit := &routeFailoverGatewayCircuit{}
	planner := NewRouteFailoverPlanner(policyRepo, groups, circuit, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	svc := &OpenAIGatewayService{
		accountRepo: routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
			2: {{ID: 22, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
		}},
		routeFailoverPlanner: planner,
	}
	groupID := int64(1)

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), &groupID, "", "", "gpt-source", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, true, PlatformOpenAI,
	)

	require.Error(t, err)
	require.Nil(t, selection)
	require.Zero(t, circuit.allows)
}

func TestOpenAIGatewayRouteFailoverDoesNotResolveFallbackWhenPrimaryIsHealthy(t *testing.T) {
	groups := routeFailoverGatewayGroupRepo{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
		2: {ID: 2, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive, Hydrated: true},
	}}
	policyRepo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{Policies: []RouteFailoverPolicy{{
		ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
		Targets: []RouteFailoverTarget{{ID: 7, TargetGroupID: 2, Priority: 1, Enabled: true}},
	}}}}
	circuit := &routeFailoverGatewayCircuit{}
	planner := NewRouteFailoverPlanner(policyRepo, groups, circuit, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	accounts := routeFailoverGatewayAccountRepo{byGroup: map[int64][]Account{
		1: {{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1}},
	}}
	svc := &OpenAIGatewayService{accountRepo: accounts, routeFailoverPlanner: planner}
	groupID := int64(1)

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(), &groupID, "", "", "gpt-source", nil,
		OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
		false, false, true, PlatformOpenAI,
	)
	require.NoError(t, err)
	require.Equal(t, int64(11), selection.Account.ID)
	require.Zero(t, circuit.allows, "healthy OpenAI requests must not query fallback circuit state")
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
