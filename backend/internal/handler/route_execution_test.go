package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRouteExecutionCreatesFreshGroupLocalState(t *testing.T) {
	route := newRouteExecution(routeExecutionTestPlan(2, 3), 7)

	primary, ok := route.next(context.Background())
	require.True(t, ok)
	require.Equal(t, int64(1), primary.EffectiveGroupID)
	require.NotNil(t, route.local)
	require.Equal(t, 7, route.local.MaxSwitches)
	primaryLocal := route.local
	primaryLocal.FailedAccountIDs[101] = struct{}{}
	primaryLocal.SameAccountRetryCount[101] = 2

	require.True(t, route.capacityFailed(context.Background(), service.ErrNoAvailableAccounts))
	fallback, ok := route.next(context.Background())
	require.True(t, ok)
	require.Equal(t, int64(2), fallback.EffectiveGroupID)
	require.NotSame(t, primaryLocal, route.local)
	require.Equal(t, 7, route.local.MaxSwitches)
	require.Empty(t, route.local.FailedAccountIDs)
	require.Empty(t, route.local.SameAccountRetryCount)
}

func TestRouteExecutionRouteStickyDoesNotImplyAccountBinding(t *testing.T) {
	sticky := &routeExecutionStickyStore{binding: &service.APIKeyRouteStickyBinding{
		RouteConfigVersion: 7,
		EffectiveGroupID:   2,
	}}
	route := newRouteExecution(routeExecutionTestPlanWithSticky(sticky, 2), 7)

	candidate, ok := route.next(context.Background())
	require.True(t, ok)
	require.True(t, candidate.StickyHit)
	require.False(t, route.local.hasBoundSession)
}

func TestRouteExecutionGroupLocalRetryDoesNotAdvanceRoute(t *testing.T) {
	route := newRouteExecution(routeExecutionTestPlan(2), 3)
	primary, ok := route.next(context.Background())
	require.True(t, ok)

	route.local.SameAccountRetryCount[101]++
	candidate, ok := route.next(context.Background())
	require.False(t, ok)
	require.Nil(t, candidate)
	require.Same(t, primary, route.candidate)
	require.Equal(t, 1, route.plan.Audit().AttemptCount)

	require.True(t, route.capacityFailed(context.Background(), service.ErrNoAvailableAccounts))
	fallback, ok := route.next(context.Background())
	require.True(t, ok)
	require.Equal(t, int64(2), fallback.EffectiveGroupID)
}

func TestRouteExecutionAdmissionSkipAdvancesWithoutTouchingCircuit(t *testing.T) {
	circuit := &routeExecutionCircuitStub{}
	route := newRouteExecution(routeExecutionTestPlanWithCircuit(circuit, 2, 3), 3)

	primary, ok := route.next(context.Background())
	require.True(t, ok)
	require.Equal(t, int64(1), primary.EffectiveGroupID)
	require.True(t, route.capacityFailed(context.Background(), errors.New("primary capacity")))

	fallback, ok := route.next(context.Background())
	require.True(t, ok)
	require.Equal(t, int64(2), fallback.EffectiveGroupID)
	require.True(t, route.candidateSkipped(context.Background(), service.RouteFailureAdmission, errors.New("fallback quota")))

	next, ok := route.next(context.Background())
	require.True(t, ok)
	require.Equal(t, int64(3), next.EffectiveGroupID)
	require.Empty(t, circuit.failures)
}

func TestRouteExecutionReleasesAttemptResourcesBeforeRouteAdvancement(t *testing.T) {
	route := newRouteExecution(routeExecutionTestPlan(2), 3)
	_, ok := route.next(context.Background())
	require.True(t, ok)
	released := false
	route.releaseBeforeAdvance(func() { released = true })

	require.True(t, route.capacityFailed(context.Background(), service.ErrNoAvailableAccounts))
	require.True(t, released)
	_, ok = route.next(context.Background())
	require.True(t, ok)
}

func TestRouteExecutionReleasesAttemptResourcesOnTerminalFailure(t *testing.T) {
	tests := []struct {
		name string
		ctx  func() context.Context
		err  error
	}{
		{
			name: "business error",
			ctx:  context.Background,
			err:  &service.UpstreamFailoverError{StatusCode: http.StatusBadRequest},
		},
		{
			name: "cancellation",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			err: context.Canceled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route := newRouteExecution(routeExecutionTestPlan(2), 3)
			_, ok := route.next(context.Background())
			require.True(t, ok)
			released := false
			route.releaseBeforeAdvance(func() { released = true })

			require.False(t, route.upstreamFailed(test.ctx(), test.err, true))
			require.True(t, released)
		})
	}
}

func TestRouteExecutionUpstreamFailureEligibility(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		replaySafe bool
		wantNext   bool
		wantReason string
	}{
		{name: "connection", err: &service.UpstreamFailoverError{StatusCode: 0}, replaySafe: true, wantNext: true, wantReason: string(service.RouteFailureConnection)},
		{name: "timeout", err: &service.UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout}, replaySafe: true, wantNext: true, wantReason: string(service.RouteFailureTimeout)},
		{name: "429", err: &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests}, replaySafe: true, wantNext: true, wantReason: string(service.RouteFailureUpstream429)},
		{name: "5xx", err: &service.UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable}, replaySafe: true, wantNext: true, wantReason: string(service.RouteFailureUpstream5xx)},
		{name: "wrapped eligible error", err: errors.Join(errors.New("forward failed"), &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway}), replaySafe: true, wantNext: true, wantReason: string(service.RouteFailureUpstream5xx)},
		{name: "business 400", err: &service.UpstreamFailoverError{StatusCode: http.StatusBadRequest}, replaySafe: true},
		{name: "business 401", err: &service.UpstreamFailoverError{StatusCode: http.StatusUnauthorized}, replaySafe: true},
		{name: "business 403", err: &service.UpstreamFailoverError{StatusCode: http.StatusForbidden}, replaySafe: true},
		{name: "plain error", err: errors.New("plain failure"), replaySafe: true},
		{name: "not replay safe", err: &service.UpstreamFailoverError{StatusCode: http.StatusBadGateway}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route := newRouteExecution(routeExecutionTestPlan(2), 3)
			_, ok := route.next(context.Background())
			require.True(t, ok)

			require.Equal(t, test.wantNext, route.upstreamFailed(context.Background(), test.err, test.replaySafe))
			if !test.wantNext {
				candidate, nextOK := route.next(context.Background())
				require.False(t, nextOK)
				require.Nil(t, candidate)
				return
			}
			fallback, nextOK := route.next(context.Background())
			require.True(t, nextOK)
			require.Equal(t, int64(2), fallback.EffectiveGroupID)
			require.Equal(t, test.wantReason, route.plan.Audit().FallbackReason)
		})
	}
}

func TestRouteExecutionCredentialAndStopErrorsNeverAdvanceGroups(t *testing.T) {
	tests := []struct {
		name string
		err  *service.UpstreamFailoverError
	}{
		{
			name: "account authentication failure",
			err: &service.UpstreamFailoverError{
				Stage:             service.GatewayFailureStageAccountAuth,
				Scope:             service.GatewayFailureScopeAccount,
				NextAccountAction: service.NextAccountRetry,
			},
		},
		{
			name: "explicit stop",
			err: &service.UpstreamFailoverError{
				NextAccountAction: service.NextAccountStop,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			circuit := &routeExecutionCircuitStub{}
			route := newRouteExecution(routeExecutionTestPlanWithCircuit(circuit, 2), 3)
			_, ok := route.next(context.Background())
			require.True(t, ok)

			require.False(t, route.upstreamFailed(context.Background(), test.err, true))
			candidate, nextOK := route.next(context.Background())
			require.False(t, nextOK)
			require.Nil(t, candidate)
			require.Empty(t, circuit.failures)
			require.Empty(t, route.plan.Audit().FallbackReason)
		})
	}
}

func TestRouteExecutionCircuitRecordingRespectsUpstreamScope(t *testing.T) {
	tests := []struct {
		name        string
		scope       service.GatewayFailureScope
		wantFailure bool
	}{
		{name: "unspecified"},
		{name: "account", scope: service.GatewayFailureScopeAccount},
		{name: "request", scope: service.GatewayFailureScopeRequest},
		{name: "route", scope: service.GatewayFailureScopeRoute, wantFailure: true},
		{name: "provider", scope: service.GatewayFailureScopeProvider, wantFailure: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			circuit := &routeExecutionCircuitStub{}
			route := newRouteExecution(routeExecutionTestPlanWithCircuit(circuit, 2), 3)
			_, ok := route.next(context.Background())
			require.True(t, ok)

			require.True(t, route.upstreamFailed(context.Background(), &service.UpstreamFailoverError{
				StatusCode: http.StatusServiceUnavailable,
				Scope:      test.scope,
			}, true))
			if test.wantFailure {
				require.Equal(t, []int64{1}, circuit.failures)
			} else {
				require.Empty(t, circuit.failures)
			}
		})
	}
}

func TestRouteExecutionSemanticCommitBlocksReplayButSSECommentDoesNot(t *testing.T) {
	t.Run("semantic output", func(t *testing.T) {
		route := newRouteExecution(routeExecutionTestPlan(2), 3)
		_, ok := route.next(context.Background())
		require.True(t, ok)
		route.markSemanticCommitted()

		require.False(t, route.upstreamFailed(context.Background(), &service.UpstreamFailoverError{
			StatusCode: http.StatusBadGateway, SafeToFailoverAfterWrite: true,
		}, true))
		_, ok = route.next(context.Background())
		require.False(t, ok)
	})

	t.Run("SSE comment only", func(t *testing.T) {
		route := newRouteExecution(routeExecutionTestPlan(2), 3)
		_, ok := route.next(context.Background())
		require.True(t, ok)

		require.True(t, route.upstreamFailed(context.Background(), &service.UpstreamFailoverError{
			StatusCode: http.StatusBadGateway, SafeToFailoverAfterWrite: true,
		}, true))
		_, ok = route.next(context.Background())
		require.True(t, ok)
	})
}

func TestRouteExecutionCancellationStopsImmediately(t *testing.T) {
	route := newRouteExecution(routeExecutionTestPlan(2), 3)
	_, ok := route.next(context.Background())
	require.True(t, ok)
	released := false
	route.releaseBeforeAdvance(func() { released = true })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	candidate, ok := route.next(ctx)
	require.False(t, ok)
	require.Nil(t, candidate)
	require.True(t, released)
}

func TestRouteExecutionNilPlanDoesNotCountRouteRequest(t *testing.T) {
	before := service.GetAPIKeyRouteMetricsSnapshot()

	route := newRouteExecution(nil, 3)
	_, ok := route.next(context.Background())
	require.False(t, ok)

	after := service.GetAPIKeyRouteMetricsSnapshot()
	require.Equal(t, before.RouteRequests, after.RouteRequests)
}

func TestRouteExecutionFailureRecordsCandidateSkipMetric(t *testing.T) {
	before := service.GetAPIKeyRouteMetricsSnapshot()
	route := newRouteExecution(routeExecutionTestPlan(2), 3)
	_, ok := route.next(context.Background())
	require.True(t, ok)

	require.True(t, route.capacityFailed(context.Background(), service.ErrNoAvailableAccounts))

	after := service.GetAPIKeyRouteMetricsSnapshot()
	require.Equal(t, before.CandidateSkips[service.RouteFailureCapacity]+1, after.CandidateSkips[service.RouteFailureCapacity])
}

func TestRouteExecutionNeverAttemptsCandidateTwice(t *testing.T) {
	route := newRouteExecution(routeExecutionTestPlan(2, 2, 3), 3)
	seen := make(map[int64]struct{})
	for {
		candidate, ok := route.next(context.Background())
		if !ok {
			break
		}
		_, duplicate := seen[candidate.EffectiveGroupID]
		require.False(t, duplicate)
		seen[candidate.EffectiveGroupID] = struct{}{}
		if !route.capacityFailed(context.Background(), service.ErrNoAvailableAccounts) {
			break
		}
	}
	require.Equal(t, map[int64]struct{}{1: {}, 2: {}, 3: {}}, seen)
}

func TestRouteExecutionSucceedReturnsUsageAudit(t *testing.T) {
	route := newRouteExecution(routeExecutionTestPlan(2), 3)
	_, ok := route.next(context.Background())
	require.True(t, ok)
	require.True(t, route.capacityFailed(context.Background(), service.ErrNoAvailableAccounts))
	_, ok = route.next(context.Background())
	require.True(t, ok)

	audit := route.succeed(context.Background())
	require.True(t, audit.FallbackUsed)
	require.Equal(t, 2, audit.AttemptCount)
	require.Equal(t, string(service.RouteFailureCapacity), audit.FallbackReason)
	_, ok = route.next(context.Background())
	require.False(t, ok)
}

func TestRouteExecutionLogsRouteAuditWithoutSecrets(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "req-10")))
	plan := routeExecutionTestPlan(2)
	route := newRouteExecution(plan, 3)

	_, ok := route.next(ctx)
	require.True(t, ok)
	require.True(t, route.capacityFailed(ctx, service.ErrNoAvailableAccounts))
	_, ok = route.next(ctx)
	require.True(t, ok)
	route.succeed(ctx)

	entries := logs.FilterMessage("api_key_route.succeeded").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.EqualValues(t, 10, fields["api_key_id"])
	require.EqualValues(t, 1, fields["source_group_id"])
	require.EqualValues(t, 2, fields["effective_group_id"])
	require.EqualValues(t, 7, fields["route_version"])
	require.EqualValues(t, 2, fields["attempt_count"])
	require.Equal(t, "capacity", fields["failure_class"])
	require.NotContains(t, fields, "billing_result")
	require.NotContains(t, fields, "effective_multiplier")
	require.Contains(t, fmt.Sprint(fields["configured_group_ids"]), "1")
	require.Contains(t, fmt.Sprint(fields["configured_group_ids"]), "2")
	require.Contains(t, fmt.Sprint(fields["attempted_group_ids"]), "1")
	require.Contains(t, fmt.Sprint(fields["attempted_group_ids"]), "2")
	require.NotContains(t, fmt.Sprint(entries), "route-secret-must-not-log")
}

func TestRouteExecutionLogsEachFailedAttemptClassAndStatus(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "req-attempts")))
	route := newRouteExecution(routeExecutionTestPlan(2), 3)

	_, ok := route.next(ctx)
	require.True(t, ok)
	require.True(t, route.upstreamFailed(ctx, &service.UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable}, true))

	entries := logs.FilterMessage("api_key_route.attempt_failed").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "req-attempts", fields["request_id"])
	require.EqualValues(t, 10, fields["api_key_id"])
	require.EqualValues(t, 1, fields["source_group_id"])
	require.EqualValues(t, 1, fields["effective_group_id"])
	require.Equal(t, string(service.RouteFailureUpstream5xx), fields["failure_class"])
	require.EqualValues(t, http.StatusServiceUnavailable, fields["failure_status"])
	require.Equal(t, true, fields["route_advanced"])
	require.NotContains(t, fmt.Sprint(entries), "route-secret-must-not-log")
}

func routeExecutionTestPlan(fallbackGroupIDs ...int64) *service.APIKeyRoutePlan {
	return routeExecutionTestPlanWithSticky(nil, fallbackGroupIDs...)
}

func routeExecutionTestPlanWithSticky(sticky service.APIKeyRouteStickyStore, fallbackGroupIDs ...int64) *service.APIKeyRoutePlan {
	return routeExecutionTestPlanWithStores(sticky, nil, fallbackGroupIDs...)
}

func routeExecutionTestPlanWithCircuit(circuit service.RouteFailoverCircuit, fallbackGroupIDs ...int64) *service.APIKeyRoutePlan {
	return routeExecutionTestPlanWithStores(nil, circuit, fallbackGroupIDs...)
}

func routeExecutionTestPlanWithStores(sticky service.APIKeyRouteStickyStore, circuit service.RouteFailoverCircuit, fallbackGroupIDs ...int64) *service.APIKeyRoutePlan {
	primaryGroupID := int64(1)
	key := &service.APIKey{
		ID: 10, UserID: 20, Key: "route-secret-must-not-log", GroupID: &primaryGroupID, RouteConfigVersion: 7,
		User:  &service.User{ID: 20, Status: service.StatusActive, AllowedGroups: []int64{2, 3, 4, 5, 6}},
		Group: &service.Group{ID: primaryGroupID, Status: service.StatusActive, Platform: service.PlatformOpenAI, SubscriptionType: service.SubscriptionTypeStandard},
	}
	for priority, groupID := range fallbackGroupIDs {
		key.FallbackTargets = append(key.FallbackTargets, service.APIKeyFailoverTarget{
			ID: int64(priority + 1), TargetGroupID: groupID, Priority: priority + 1,
			Group: &service.Group{ID: groupID, Status: service.StatusActive, Platform: service.PlatformOpenAI, SubscriptionType: service.SubscriptionTypeStandard},
		})
	}
	sessionHash := ""
	if sticky != nil {
		sessionHash = "session-hash"
	}
	return service.NewAPIKeyRoutePlanner(sticky, circuit, nil).NewPlan(context.Background(), key, nil, sessionHash, "gpt-5")
}

type routeExecutionCircuitStub struct {
	failures []int64
}

func (s *routeExecutionCircuitStub) Allow(context.Context, int64, string) (bool, bool, string, error) {
	return true, false, "lease", nil
}

func (s *routeExecutionCircuitStub) RecordSuccess(context.Context, int64, string, string) error {
	return nil
}

func (s *routeExecutionCircuitStub) RecordFailure(_ context.Context, groupID int64, _, _ string) error {
	s.failures = append(s.failures, groupID)
	return nil
}

type routeExecutionStickyStore struct {
	binding *service.APIKeyRouteStickyBinding
}

func (s *routeExecutionStickyStore) Get(context.Context, int64, string) (*service.APIKeyRouteStickyBinding, error) {
	if s.binding == nil {
		return nil, nil
	}
	binding := *s.binding
	return &binding, nil
}

func (*routeExecutionStickyStore) Set(context.Context, int64, string, service.APIKeyRouteStickyBinding, time.Duration) error {
	return nil
}

func (*routeExecutionStickyStore) Refresh(context.Context, int64, string, time.Duration) error {
	return nil
}

func (*routeExecutionStickyStore) Delete(context.Context, int64, string) error { return nil }
