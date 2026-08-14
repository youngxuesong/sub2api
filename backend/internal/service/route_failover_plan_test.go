package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type apiKeyRouteStickyStub struct {
	binding      *APIKeyRouteStickyBinding
	getErr       error
	setErr       error
	refreshErr   error
	deleteErr    error
	setCalls     int
	refreshCalls int
	deleteCalls  int
	lastSet      APIKeyRouteStickyBinding
	lastSetTTL   time.Duration
}

func (s *apiKeyRouteStickyStub) Get(context.Context, int64, string) (*APIKeyRouteStickyBinding, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.binding == nil {
		return nil, nil
	}
	copyBinding := *s.binding
	return &copyBinding, nil
}

func (s *apiKeyRouteStickyStub) Set(_ context.Context, _ int64, _ string, binding APIKeyRouteStickyBinding, ttl time.Duration) error {
	s.setCalls++
	s.lastSet = binding
	s.lastSetTTL = ttl
	return s.setErr
}

func (s *apiKeyRouteStickyStub) Refresh(context.Context, int64, string, time.Duration) error {
	s.refreshCalls++
	return s.refreshErr
}

func (s *apiKeyRouteStickyStub) Delete(context.Context, int64, string) error {
	s.deleteCalls++
	return s.deleteErr
}

type apiKeyRouteCircuitStub struct {
	allowByGroup map[int64]bool
	allowErr     error
	allowCalls   []int64
	failures     []int64
	successes    []int64
}

func (s *apiKeyRouteCircuitStub) Allow(_ context.Context, groupID int64, _ string) (bool, bool, string, error) {
	s.allowCalls = append(s.allowCalls, groupID)
	if s.allowErr != nil {
		return false, false, "", s.allowErr
	}
	allowed, exists := s.allowByGroup[groupID]
	if !exists {
		allowed = true
	}
	return allowed, false, "lease", nil
}

func (s *apiKeyRouteCircuitStub) RecordSuccess(_ context.Context, groupID int64, _, _ string) error {
	s.successes = append(s.successes, groupID)
	return nil
}

func (s *apiKeyRouteCircuitStub) RecordFailure(_ context.Context, groupID int64, _, _ string) error {
	s.failures = append(s.failures, groupID)
	return nil
}

type apiKeyRouteSubscriptionRepoStub struct {
	UserSubscriptionRepository
	subscriptions map[int64]*UserSubscription
	err           error
}

func (s apiKeyRouteSubscriptionRepoStub) GetActiveByUserIDAndGroupID(_ context.Context, _, groupID int64) (*UserSubscription, error) {
	if s.err != nil {
		return nil, s.err
	}
	subscription := s.subscriptions[groupID]
	if subscription == nil {
		return nil, ErrSubscriptionNotFound
	}
	copySubscription := *subscription
	return &copySubscription, nil
}

func routePlanTestKey(targets ...APIKeyFailoverTarget) *APIKey {
	primaryID := int64(1)
	return &APIKey{
		ID: 10, UserID: 20, GroupID: &primaryID, RouteConfigVersion: 7,
		User:            &User{ID: 20, Status: StatusActive, AllowedGroups: []int64{2, 3, 4, 5, 6}},
		Group:           &Group{ID: primaryID, Status: StatusActive, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeStandard},
		FallbackTargets: targets,
	}
}

func routePlanTarget(id int64, priority int) APIKeyFailoverTarget {
	return APIKeyFailoverTarget{
		ID: id * 10, TargetGroupID: id, Priority: priority,
		Group: &Group{ID: id, Status: StatusActive, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeStandard},
	}
}

func nextRouteGroup(t *testing.T, plan *APIKeyRoutePlan) *APIKeyRouteCandidate {
	t.Helper()
	candidate, ok := plan.Next(context.Background())
	require.True(t, ok)
	require.NotNil(t, candidate)
	return candidate
}

func retryableRouteFailure(class RouteFailureClass) APIKeyRouteAttemptFailure {
	return APIKeyRouteAttemptFailure{Class: class, ReplaySafe: true, RecordCircuit: true}
}

func TestAPIKeyRoutePlanPrimaryOnlyAndOrderedFallbacks(t *testing.T) {
	key := routePlanTestKey(routePlanTarget(3, 2), routePlanTarget(2, 1))
	planner := NewAPIKeyRoutePlanner(nil, nil, nil)
	plan := planner.NewPlan(context.Background(), key, nil, "", "gpt-5")

	primary := nextRouteGroup(t, plan)
	require.Equal(t, int64(1), primary.EffectiveGroupID)
	require.False(t, primary.IsFallback)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))

	first := nextRouteGroup(t, plan)
	require.Equal(t, int64(2), first.EffectiveGroupID)
	require.Equal(t, 1, first.Priority)
	require.True(t, plan.Fail(context.Background(), first, retryableRouteFailure(RouteFailureConnection)))

	second := nextRouteGroup(t, plan)
	require.Equal(t, int64(3), second.EffectiveGroupID)
	require.Equal(t, 2, second.Priority)
	plan.Succeed(context.Background(), second)

	_, ok := plan.Next(context.Background())
	require.False(t, ok)
	require.Equal(t, RouteUsageAudit{
		SourceGroupID: routePlanInt64Ptr(1), FallbackUsed: true, AttemptCount: 3,
		FallbackReason: string(RouteFailureCapacity),
	}, plan.Audit())
}

func TestAPIKeyRoutePlanDoesNotSilentlySkipIneligiblePrimary(t *testing.T) {
	key := routePlanTestKey(routePlanTarget(2, 1))
	key.Group.Status = StatusDisabled
	plan := NewAPIKeyRoutePlanner(nil, nil, nil).NewPlan(context.Background(), key, nil, "", "gpt-5")

	primary := nextRouteGroup(t, plan)
	require.Equal(t, int64(1), primary.EffectiveGroupID)
	require.False(t, primary.IsFallback)
	require.Equal(t, 1, plan.Audit().AttemptCount)
}

func TestAPIKeyRoutePlanEffectiveKeyDoesNotMutateAuthSnapshot(t *testing.T) {
	override := 17
	target := routePlanTarget(2, 1)
	target.UserGroupRPMOverride = &override
	key := routePlanTestKey(target)
	planner := NewAPIKeyRoutePlanner(nil, nil, nil)
	plan := planner.NewPlan(context.Background(), key, nil, "", "gpt-5")

	primary := nextRouteGroup(t, plan)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	fallback := nextRouteGroup(t, plan)

	require.NotSame(t, key, fallback.EffectiveAPIKey)
	require.NotSame(t, key.User, fallback.EffectiveAPIKey.User)
	require.Equal(t, int64(2), *fallback.EffectiveAPIKey.GroupID)
	require.Equal(t, int64(2), fallback.EffectiveAPIKey.Group.ID)
	require.Equal(t, &override, fallback.EffectiveAPIKey.User.UserGroupRPMOverride)
	require.Equal(t, int64(1), *key.GroupID)
	require.Nil(t, key.User.UserGroupRPMOverride)
}

func TestAPIKeyRoutePlanStickyFirstAndSuccessRefreshesTTL(t *testing.T) {
	sticky := &apiKeyRouteStickyStub{binding: &APIKeyRouteStickyBinding{RouteConfigVersion: 7, EffectiveGroupID: 3}}
	key := routePlanTestKey(routePlanTarget(2, 1), routePlanTarget(3, 2))
	plan := NewAPIKeyRoutePlanner(sticky, nil, nil).NewPlan(context.Background(), key, nil, "session", "gpt-5")

	candidate := nextRouteGroup(t, plan)
	require.Equal(t, int64(3), candidate.EffectiveGroupID)
	require.True(t, candidate.StickyHit)
	plan.Succeed(context.Background(), candidate)

	require.Equal(t, 1, sticky.refreshCalls)
	require.Zero(t, sticky.setCalls)
	require.True(t, plan.Audit().StickyHit)
}

func TestAPIKeyRoutePlanFallbackSuccessWritesStickyBindingButPrimaryDoesNot(t *testing.T) {
	sticky := &apiKeyRouteStickyStub{}
	key := routePlanTestKey(routePlanTarget(2, 1))
	planner := NewAPIKeyRoutePlanner(sticky, nil, nil)

	primaryPlan := planner.NewPlan(context.Background(), key, nil, "primary-session", "gpt-5")
	primaryPlan.Succeed(context.Background(), nextRouteGroup(t, primaryPlan))
	require.Zero(t, sticky.setCalls)

	fallbackPlan := planner.NewPlan(context.Background(), key, nil, "fallback-session", "gpt-5")
	primary := nextRouteGroup(t, fallbackPlan)
	require.True(t, fallbackPlan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	fallback := nextRouteGroup(t, fallbackPlan)
	fallbackPlan.Succeed(context.Background(), fallback)

	require.Equal(t, 1, sticky.setCalls)
	require.Equal(t, APIKeyRouteStickyBinding{RouteConfigVersion: 7, EffectiveGroupID: 2}, sticky.lastSet)
	require.Equal(t, APIKeyRouteStickyTTL, sticky.lastSetTTL)
}

func TestAPIKeyRoutePlanDeletesStaleOrRemovedStickyBinding(t *testing.T) {
	tests := []struct {
		name    string
		binding APIKeyRouteStickyBinding
	}{
		{name: "stale version", binding: APIKeyRouteStickyBinding{RouteConfigVersion: 6, EffectiveGroupID: 2}},
		{name: "removed target", binding: APIKeyRouteStickyBinding{RouteConfigVersion: 7, EffectiveGroupID: 99}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sticky := &apiKeyRouteStickyStub{binding: &tt.binding}
			plan := NewAPIKeyRoutePlanner(sticky, nil, nil).NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1)), nil, "session", "gpt-5")
			require.Equal(t, int64(1), nextRouteGroup(t, plan).EffectiveGroupID)
			require.Equal(t, 1, sticky.deleteCalls)
		})
	}
}

func TestAPIKeyRoutePlanStickyRetryableFailureRebuildsPrimaryFirstWithoutDuplicates(t *testing.T) {
	sticky := &apiKeyRouteStickyStub{binding: &APIKeyRouteStickyBinding{RouteConfigVersion: 7, EffectiveGroupID: 2}}
	key := routePlanTestKey(routePlanTarget(2, 1), routePlanTarget(3, 2), routePlanTarget(2, 3))
	plan := NewAPIKeyRoutePlanner(sticky, nil, nil).NewPlan(context.Background(), key, nil, "session", "gpt-5")

	stickyCandidate := nextRouteGroup(t, plan)
	require.Equal(t, int64(2), stickyCandidate.EffectiveGroupID)
	require.True(t, plan.Fail(context.Background(), stickyCandidate, retryableRouteFailure(RouteFailureTimeout)))
	require.Equal(t, 1, sticky.deleteCalls)

	primary := nextRouteGroup(t, plan)
	require.Equal(t, int64(1), primary.EffectiveGroupID)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	require.Equal(t, int64(3), nextRouteGroup(t, plan).EffectiveGroupID)
	_, ok := plan.Next(context.Background())
	require.False(t, ok)
	require.True(t, plan.Audit().StickyHit)
}

func TestAPIKeyRoutePlanDeletesStickyBindingWhenTargetIsNoLongerEligible(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*APIKeyFailoverTarget)
	}{
		{name: "inactive", mutate: func(target *APIKeyFailoverTarget) { target.Group.Status = StatusDisabled }},
		{name: "unsupported model", mutate: func(target *APIKeyFailoverTarget) {
			target.Group.ModelsListConfig = GroupModelsListConfig{Enabled: true, Models: []string{"gpt-other"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := routePlanTarget(2, 1)
			tt.mutate(&target)
			sticky := &apiKeyRouteStickyStub{binding: &APIKeyRouteStickyBinding{RouteConfigVersion: 7, EffectiveGroupID: 2}}
			plan := NewAPIKeyRoutePlanner(sticky, nil, nil).NewPlan(context.Background(), routePlanTestKey(target), nil, "session", "gpt-5")

			candidate := nextRouteGroup(t, plan)
			require.Equal(t, int64(1), candidate.EffectiveGroupID)
			require.False(t, candidate.StickyHit)
			require.Equal(t, 1, sticky.deleteCalls)
		})
	}
}

func TestAPIKeyRoutePlanOpenCircuitStickyTargetIsNotReportedAsHit(t *testing.T) {
	sticky := &apiKeyRouteStickyStub{binding: &APIKeyRouteStickyBinding{RouteConfigVersion: 7, EffectiveGroupID: 2}}
	circuit := &apiKeyRouteCircuitStub{allowByGroup: map[int64]bool{2: false}}
	plan := NewAPIKeyRoutePlanner(sticky, circuit, nil).NewPlan(
		context.Background(),
		routePlanTestKey(routePlanTarget(2, 1), routePlanTarget(3, 2)),
		nil,
		"session",
		"gpt-5",
	)

	candidate := nextRouteGroup(t, plan)
	require.Equal(t, int64(1), candidate.EffectiveGroupID)
	require.False(t, candidate.StickyHit)
	require.False(t, plan.Audit().StickyHit)
	require.Equal(t, 1, sticky.deleteCalls)
}

func TestAPIKeyRoutePlanSkipsIneligibleAndUnsupportedTargets(t *testing.T) {
	inactive := routePlanTarget(2, 1)
	inactive.Group.Status = StatusDisabled
	composite := routePlanTarget(3, 2)
	composite.Group.Platform = PlatformComposite
	crossPlatform := routePlanTarget(4, 3)
	crossPlatform.Group.Platform = PlatformAnthropic
	unauthorized := routePlanTarget(5, 4)
	unauthorized.Group.IsExclusive = true
	unsupported := routePlanTarget(6, 5)
	unsupported.Group.ModelsListConfig = GroupModelsListConfig{Enabled: true, Models: []string{"gpt-other"}}
	key := routePlanTestKey(inactive, composite, crossPlatform, unauthorized, unsupported, routePlanTarget(7, 6))
	key.User.AllowedGroups = []int64{2, 3, 4, 6, 7}

	plan := NewAPIKeyRoutePlanner(nil, nil, nil).NewPlan(context.Background(), key, nil, "", "gpt-5")
	primary := nextRouteGroup(t, plan)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	require.Equal(t, int64(7), nextRouteGroup(t, plan).EffectiveGroupID)
}

func TestAPIKeyRoutePlanLoadsSubscriptionForSubscriptionTarget(t *testing.T) {
	target := routePlanTarget(2, 1)
	target.Group.SubscriptionType = SubscriptionTypeSubscription
	sub := &UserSubscription{ID: 44, UserID: 20, GroupID: 2, Status: SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour)}
	subscriptions := &SubscriptionService{userSubRepo: apiKeyRouteSubscriptionRepoStub{subscriptions: map[int64]*UserSubscription{2: sub}}}
	plan := NewAPIKeyRoutePlanner(nil, nil, subscriptions).NewPlan(context.Background(), routePlanTestKey(target), nil, "", "gpt-5")

	primary := nextRouteGroup(t, plan)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	candidate := nextRouteGroup(t, plan)
	require.Equal(t, int64(44), candidate.Subscription.ID)
}

func TestAPIKeyRoutePlanSkipsExpiredSubscriptionTarget(t *testing.T) {
	target := routePlanTarget(2, 1)
	target.Group.SubscriptionType = SubscriptionTypeSubscription
	expired := &UserSubscription{ID: 44, UserID: 20, GroupID: 2, Status: SubscriptionStatusActive, ExpiresAt: time.Now().Add(-time.Minute)}
	subscriptions := &SubscriptionService{userSubRepo: apiKeyRouteSubscriptionRepoStub{subscriptions: map[int64]*UserSubscription{2: expired}}}
	plan := NewAPIKeyRoutePlanner(nil, nil, subscriptions).NewPlan(context.Background(), routePlanTestKey(target, routePlanTarget(3, 2)), nil, "", "gpt-5")

	primary := nextRouteGroup(t, plan)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	require.Equal(t, int64(3), nextRouteGroup(t, plan).EffectiveGroupID)
}

func TestAPIKeyRoutePlanSkipsMissingSubscriptionAndOpenCircuitButCircuitErrorsFailOpen(t *testing.T) {
	subscriptionTarget := routePlanTarget(2, 1)
	subscriptionTarget.Group.SubscriptionType = SubscriptionTypeSubscription
	circuit := &apiKeyRouteCircuitStub{allowByGroup: map[int64]bool{3: false}, allowErr: nil}
	subscriptions := &SubscriptionService{userSubRepo: apiKeyRouteSubscriptionRepoStub{subscriptions: map[int64]*UserSubscription{}}}
	key := routePlanTestKey(subscriptionTarget, routePlanTarget(3, 2), routePlanTarget(4, 3))
	plan := NewAPIKeyRoutePlanner(nil, circuit, subscriptions).NewPlan(context.Background(), key, nil, "", "gpt-5")

	primary := nextRouteGroup(t, plan)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	require.Equal(t, int64(4), nextRouteGroup(t, plan).EffectiveGroupID)
	require.Equal(t, 2, plan.Audit().AttemptCount, "open circuit target is skipped, not attempted")

	failingCircuit := &apiKeyRouteCircuitStub{allowErr: errors.New("redis unavailable")}
	plan = NewAPIKeyRoutePlanner(nil, failingCircuit, nil).NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1)), nil, "", "gpt-5")
	primary = nextRouteGroup(t, plan)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	require.Equal(t, int64(2), nextRouteGroup(t, plan).EffectiveGroupID)
}

func TestAPIKeyRoutePlanLocal4xxDoesNotSetFallbackReason(t *testing.T) {
	plan := NewAPIKeyRoutePlanner(nil, nil, nil).NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1)), nil, "", "gpt-5")
	primary := nextRouteGroup(t, plan)
	require.False(t, plan.Fail(context.Background(), primary, APIKeyRouteAttemptFailure{
		Class: RouteFailureConnection, StatusCode: 400, ReplaySafe: true,
	}))
	require.Empty(t, plan.Audit().FallbackReason)
}

func TestAPIKeyRoutePlanFailureReplayAndCircuitRules(t *testing.T) {
	tests := []struct {
		name       string
		failure    APIKeyRouteAttemptFailure
		wantNext   bool
		wantRecord bool
	}{
		{name: "connection", failure: retryableRouteFailure(RouteFailureConnection), wantNext: true, wantRecord: true},
		{name: "timeout", failure: retryableRouteFailure(RouteFailureTimeout), wantNext: true, wantRecord: true},
		{name: "upstream deadline timeout", failure: APIKeyRouteAttemptFailure{Class: RouteFailureTimeout, ReplaySafe: true, RecordCircuit: true, Cause: context.DeadlineExceeded}, wantNext: true, wantRecord: true},
		{name: "429", failure: APIKeyRouteAttemptFailure{Class: RouteFailureUpstream429, StatusCode: 429, ReplaySafe: true, RecordCircuit: true}, wantNext: true, wantRecord: true},
		{name: "eligible unscoped failure", failure: APIKeyRouteAttemptFailure{Class: RouteFailureUpstream5xx, StatusCode: 503, ReplaySafe: true}, wantNext: true},
		{name: "5xx", failure: retryableRouteFailure(RouteFailureUpstream5xx), wantNext: true, wantRecord: true},
		{name: "capacity", failure: retryableRouteFailure(RouteFailureCapacity), wantNext: true},
		{name: "business", failure: retryableRouteFailure(RouteFailureBusiness)},
		{name: "local 4xx", failure: APIKeyRouteAttemptFailure{Class: RouteFailureConnection, StatusCode: 403, ReplaySafe: true}},
		{name: "unsafe replay", failure: APIKeyRouteAttemptFailure{Class: RouteFailureConnection}},
		{name: "semantic committed", failure: APIKeyRouteAttemptFailure{Class: RouteFailureConnection, ReplaySafe: true, SemanticCommitted: true}},
		{name: "canceled", failure: retryableRouteFailure(RouteFailureCanceled)},
		{name: "canceled cause", failure: APIKeyRouteAttemptFailure{Class: RouteFailureConnection, ReplaySafe: true, Cause: context.Canceled}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			circuit := &apiKeyRouteCircuitStub{}
			plan := NewAPIKeyRoutePlanner(nil, circuit, nil).NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1), routePlanTarget(3, 2)), nil, "", "gpt-5")
			primary := nextRouteGroup(t, plan)
			require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
			fallback := nextRouteGroup(t, plan)
			require.Equal(t, tt.wantNext, plan.Fail(context.Background(), fallback, tt.failure))
			require.Equal(t, tt.wantRecord, len(circuit.failures) == 1)
		})
	}
}

func TestAPIKeyRoutePlanPrimaryProviderOutcomeUpdatesSharedCircuit(t *testing.T) {
	circuit := &apiKeyRouteCircuitStub{}
	key := routePlanTestKey(routePlanTarget(2, 1))

	failedPlan := NewAPIKeyRoutePlanner(nil, circuit, nil).NewPlan(context.Background(), key, nil, "", "gpt-5")
	primary := nextRouteGroup(t, failedPlan)
	require.True(t, failedPlan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureConnection)))
	require.Equal(t, []int64{1}, circuit.failures)

	successPlan := NewAPIKeyRoutePlanner(nil, circuit, nil).NewPlan(context.Background(), key, nil, "", "gpt-5")
	successPlan.Succeed(context.Background(), nextRouteGroup(t, successPlan))
	require.Equal(t, []int64{1}, circuit.successes)
}

func TestAPIKeyRoutePlanSwitchDeadlineStartsOnPrimaryFailure(t *testing.T) {
	now := time.Unix(1000, 0)
	planner := NewAPIKeyRoutePlanner(nil, nil, nil)
	planner.now = func() time.Time { return now }
	plan := planner.NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1)), nil, "", "gpt-5")
	primary := nextRouteGroup(t, plan)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))

	now = now.Add(19 * time.Second)
	require.Equal(t, int64(2), nextRouteGroup(t, plan).EffectiveGroupID)

	plan = planner.NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1)), nil, "", "gpt-5")
	primary = nextRouteGroup(t, plan)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	now = now.Add(21 * time.Second)
	_, ok := plan.Next(context.Background())
	require.False(t, ok)
}

func TestAPIKeyRoutePlanRequestCancellationStopsSwitching(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	plan := NewAPIKeyRoutePlanner(nil, nil, nil).NewPlan(ctx, routePlanTestKey(routePlanTarget(2, 1)), nil, "", "gpt-5")
	primary := nextRouteGroup(t, plan)
	cancel()

	require.False(t, plan.Fail(ctx, primary, retryableRouteFailure(RouteFailureConnection)))
	_, ok := plan.Next(ctx)
	require.False(t, ok)
}

func TestAPIKeyRoutePlanStickyStoreErrorsFailOpen(t *testing.T) {
	before := GetAPIKeyRouteMetricsSnapshot()
	sticky := &apiKeyRouteStickyStub{getErr: errors.New("redis unavailable"), setErr: errors.New("redis unavailable")}
	plan := NewAPIKeyRoutePlanner(sticky, nil, nil).NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1)), nil, "session", "gpt-5")
	require.Equal(t, int64(1), nextRouteGroup(t, plan).EffectiveGroupID)
	after := GetAPIKeyRouteMetricsSnapshot()
	require.Equal(t, before.StoreErrors+1, after.StoreErrors)
}

func TestAPIKeyRoutePlanRecordsCandidateSkipAndInvalidStickyMetrics(t *testing.T) {
	unsupported := routePlanTarget(2, 1)
	unsupported.Group.ModelsListConfig = GroupModelsListConfig{Enabled: true, Models: []string{"gpt-other"}}
	subscriptionTarget := routePlanTarget(3, 2)
	subscriptionTarget.Group.SubscriptionType = SubscriptionTypeSubscription
	circuit := &apiKeyRouteCircuitStub{allowByGroup: map[int64]bool{4: false}}
	subscriptions := &SubscriptionService{userSubRepo: apiKeyRouteSubscriptionRepoStub{subscriptions: map[int64]*UserSubscription{}}}
	key := routePlanTestKey(unsupported, subscriptionTarget, routePlanTarget(4, 3), routePlanTarget(5, 4))
	before := GetAPIKeyRouteMetricsSnapshot()

	plan := NewAPIKeyRoutePlanner(nil, circuit, subscriptions).NewPlan(context.Background(), key, nil, "", "gpt-5")
	primary := nextRouteGroup(t, plan)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	require.Equal(t, int64(5), nextRouteGroup(t, plan).EffectiveGroupID)

	after := GetAPIKeyRouteMetricsSnapshot()
	require.Equal(t, before.CandidateSkips[RouteFailureUnsupportedModel]+1, after.CandidateSkips[RouteFailureUnsupportedModel])
	require.Equal(t, before.CandidateSkips[RouteFailureAdmission]+2, after.CandidateSkips[RouteFailureAdmission])

	stale := &apiKeyRouteStickyStub{
		binding:   &APIKeyRouteStickyBinding{RouteConfigVersion: 6, EffectiveGroupID: 2},
		deleteErr: errors.New("redis unavailable"),
	}
	before = GetAPIKeyRouteMetricsSnapshot()
	NewAPIKeyRoutePlanner(stale, nil, nil).NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1)), nil, "session", "gpt-5")
	after = GetAPIKeyRouteMetricsSnapshot()
	require.Equal(t, before.InvalidStickyRecords+1, after.InvalidStickyRecords)
	require.Equal(t, before.StoreErrors+1, after.StoreErrors)
}

func TestAPIKeyRoutePlanPreservesRequestedModel(t *testing.T) {
	plan := NewAPIKeyRoutePlanner(nil, nil, nil).NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1)), nil, "", "gpt-5-original")
	primary := nextRouteGroup(t, plan)
	require.Equal(t, "gpt-5-original", primary.RequestedModel)
	require.True(t, plan.Fail(context.Background(), primary, retryableRouteFailure(RouteFailureCapacity)))
	require.Equal(t, "gpt-5-original", nextRouteGroup(t, plan).RequestedModel)
}

func TestAPIKeyRoutePlanHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := NewAPIKeyRoutePlanner(nil, nil, nil).NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1)), nil, "", "gpt-5")
	_, ok := plan.Next(ctx)
	require.False(t, ok)

	circuit := &apiKeyRouteCircuitStub{}
	plan = NewAPIKeyRoutePlanner(nil, circuit, nil).NewPlan(context.Background(), routePlanTestKey(routePlanTarget(2, 1)), nil, "", "gpt-5")
	primary := nextRouteGroup(t, plan)
	require.False(t, plan.Fail(ctx, primary, retryableRouteFailure(RouteFailureConnection)))
	require.Empty(t, circuit.failures)
}

func routePlanInt64Ptr(value int64) *int64 { return &value }
