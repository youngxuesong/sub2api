package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type routeFailoverPolicyRepoStub struct {
	snapshot RouteFailoverSnapshot
	err      error
}

func (r *routeFailoverPolicyRepoStub) LoadSnapshot(context.Context) (RouteFailoverSnapshot, error) {
	if r.err != nil {
		return RouteFailoverSnapshot{}, r.err
	}
	return r.snapshot, nil
}

type blockingRouteFailoverPolicyRepo struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	snapshot RouteFailoverSnapshot
}

func (r *blockingRouteFailoverPolicyRepo) LoadSnapshot(context.Context) (RouteFailoverSnapshot, error) {
	close(r.started)
	<-r.release
	close(r.finished)
	return r.snapshot, nil
}

func TestRouteFailoverPlannerDoesNotBlockRequestOnInitialReload(t *testing.T) {
	repo := &blockingRouteFailoverPolicyRepo{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
		snapshot: RouteFailoverSnapshot{Version: 1, Policies: []RouteFailoverPolicy{{
			ID: 1, SourceGroupID: 1, Enabled: true,
			Targets: []RouteFailoverTarget{{ID: 2, TargetGroupID: 2, Enabled: true}},
		}}},
	}
	planner := NewRouteFailoverPlanner(repo, nil, nil, time.Minute)

	candidates, err := planner.Candidates(context.Background(), 1, "claude-sonnet")

	require.NoError(t, err)
	require.Len(t, candidates, 1)
	select {
	case <-repo.started:
	case <-time.After(time.Second):
		t.Fatal("initial reload did not start asynchronously")
	}
	close(repo.release)
	select {
	case <-repo.finished:
	case <-time.After(time.Second):
		t.Fatal("initial reload did not finish")
	}
}

type routeFailoverGroupRepoStub struct {
	groups map[int64]*Group
	calls  *int
}

func (r routeFailoverGroupRepoStub) GetByIDLite(_ context.Context, id int64) (*Group, error) {
	if r.calls != nil {
		*r.calls++
	}
	group := r.groups[id]
	if group == nil {
		return nil, ErrGroupNotFound
	}
	clone := *group
	return &clone, nil
}

func TestRouteFailoverPlannerCandidatesUseSnapshotWithoutGroupQueries(t *testing.T) {
	repo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{
		Version: 8,
		Policies: []RouteFailoverPolicy{{
			ID: 1, SourceGroupID: 1, Enabled: true, MaxAttempts: 2,
			Targets: []RouteFailoverTarget{{ID: 2, TargetGroupID: 2, Enabled: true}},
		}},
	}}
	groupQueries := 0
	planner := NewRouteFailoverPlanner(repo, routeFailoverGroupRepoStub{calls: &groupQueries}, routeFailoverCircuitStub{}, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))

	candidates, err := planner.Candidates(context.Background(), 1, "claude-sonnet")

	require.NoError(t, err)
	require.Len(t, candidates, 2)
	require.Equal(t, int64(2), candidates[1].GroupID)
	require.Zero(t, groupQueries)
}

type routeFailoverCircuitStub struct {
	open map[int64]bool
	err  error
}

type routeFailoverRecordingCircuit struct {
	allowModel   string
	recordModel  string
	recordFailed bool
}

func (s *routeFailoverRecordingCircuit) Allow(_ context.Context, _ RouteFailoverPolicy, _ RouteFailoverTarget, model string) (RouteFailoverPermit, error) {
	s.allowModel = model
	return RouteFailoverPermit{Allowed: true, LeaseID: "lease-1"}, nil
}

func (s *routeFailoverRecordingCircuit) RecordSuccess(context.Context, RouteFailoverPolicy, RouteFailoverTarget, string, string) error {
	return nil
}

func (s *routeFailoverRecordingCircuit) RecordFailure(_ context.Context, _ RouteFailoverPolicy, _ RouteFailoverTarget, model, _ string) error {
	s.recordModel = model
	s.recordFailed = true
	return nil
}

func (s routeFailoverCircuitStub) Allow(_ context.Context, _ RouteFailoverPolicy, target RouteFailoverTarget, _ string) (RouteFailoverPermit, error) {
	if s.err != nil {
		return RouteFailoverPermit{}, s.err
	}
	return RouteFailoverPermit{Allowed: !s.open[target.TargetGroupID]}, nil
}

func TestRouteFailoverPlannerCandidatesOrderSnapshotTargetsAndAdmitCircuit(t *testing.T) {
	repo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{
		Version: 7,
		Policies: []RouteFailoverPolicy{{
			ID:            11,
			SourceGroupID: 1,
			Enabled:       true,
			Targets: []RouteFailoverTarget{
				{ID: 31, TargetGroupID: 3, Priority: 20, Enabled: true},
				{ID: 21, TargetGroupID: 2, Priority: 10, Enabled: true, ModelMapping: map[string]string{"claude-sonnet": "claude-sonnet-backup"}},
			},
		}},
	}}
	groups := routeFailoverGroupRepoStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		3: {ID: 3, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		4: {ID: 4, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		5: {ID: 5, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeStandard, Status: StatusActive},
	}}
	planner := NewRouteFailoverPlanner(repo, groups, routeFailoverCircuitStub{open: map[int64]bool{3: true}}, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))

	candidates, err := planner.Candidates(context.Background(), 1, "claude-sonnet")
	require.NoError(t, err)
	require.Equal(t, []RouteFailoverCandidate{
		{GroupID: 1, Model: "claude-sonnet", SnapshotVersion: 7},
		{GroupID: 2, Model: "claude-sonnet-backup", CircuitModel: "claude-sonnet", TargetID: 21, IsFallback: true, SnapshotVersion: 7},
		{GroupID: 3, Model: "claude-sonnet", CircuitModel: "claude-sonnet", TargetID: 31, IsFallback: true, SnapshotVersion: 7},
	}, candidates)
	admitted, allowed := planner.Admit(context.Background(), 1, candidates[1])
	require.True(t, allowed)
	require.Equal(t, candidates[1], admitted)
	_, allowed = planner.Admit(context.Background(), 1, candidates[2])
	require.False(t, allowed)
}

func TestRouteFailoverPlannerKeepsLastSnapshotWhenReloadFails(t *testing.T) {
	repo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{
		Version: 3,
		Policies: []RouteFailoverPolicy{{
			ID: 1, SourceGroupID: 1, Enabled: true,
			Targets: []RouteFailoverTarget{{ID: 2, TargetGroupID: 2, Priority: 1, Enabled: true}},
		}},
	}}
	groups := routeFailoverGroupRepoStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
	}}
	planner := NewRouteFailoverPlanner(repo, groups, routeFailoverCircuitStub{}, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))
	repo.err = errors.New("postgres unavailable")
	require.Error(t, planner.Reload(context.Background()))

	candidates, err := planner.Candidates(context.Background(), 1, "claude-sonnet")
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	_, allowed := planner.Admit(context.Background(), 1, candidates[1])
	require.True(t, allowed)
	require.Equal(t, int64(2), candidates[1].GroupID)
}

func TestRouteFailoverPlannerFailsOpenWhenCircuitStoreUnavailable(t *testing.T) {
	repo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{
		Policies: []RouteFailoverPolicy{{
			ID: 1, SourceGroupID: 1, Enabled: true,
			Targets: []RouteFailoverTarget{{ID: 2, TargetGroupID: 2, Priority: 1, Enabled: true}},
		}},
	}}
	groups := routeFailoverGroupRepoStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
	}}
	planner := NewRouteFailoverPlanner(repo, groups, routeFailoverCircuitStub{err: errors.New("redis unavailable")}, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))

	candidates, err := planner.Candidates(context.Background(), 1, "claude-sonnet")
	require.NoError(t, err)
	require.Len(t, candidates, 2)
}

func TestRouteFailoverPlannerUsesRequestedModelForCircuitAdmissionAndOutcome(t *testing.T) {
	repo := &routeFailoverPolicyRepoStub{snapshot: RouteFailoverSnapshot{
		Policies: []RouteFailoverPolicy{{
			ID: 1, SourceGroupID: 1, Enabled: true,
			Targets: []RouteFailoverTarget{{
				ID: 2, TargetGroupID: 2, Priority: 1, Enabled: true,
				ModelMapping: map[string]string{"gpt-source": "gpt-fallback"},
			}},
		}},
	}}
	groups := routeFailoverGroupRepoStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		2: {ID: 2, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
	}}
	circuit := &routeFailoverRecordingCircuit{}
	planner := NewRouteFailoverPlanner(repo, groups, circuit, time.Minute)
	require.NoError(t, planner.Reload(context.Background()))

	candidates, err := planner.Candidates(context.Background(), 1, "gpt-source")
	require.NoError(t, err)
	require.Len(t, candidates, 2)
	require.Equal(t, "gpt-fallback", candidates[1].Model)
	admitted, allowed := planner.Admit(context.Background(), 1, candidates[1])
	require.True(t, allowed)
	planner.RecordFailure(context.Background(), 1, admitted)

	require.True(t, circuit.recordFailed)
	require.Equal(t, "gpt-source", circuit.allowModel)
	require.Equal(t, circuit.allowModel, circuit.recordModel)
}
