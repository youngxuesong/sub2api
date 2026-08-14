package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type routeBillingConflictRepo struct {
	UsageBillingRepository
}

type routeBillingReplayRepo struct {
	UsageBillingRepository
}

func (routeBillingReplayRepo) Apply(context.Context, *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	return &UsageBillingApplyResult{Applied: false}, nil
}

func (routeBillingConflictRepo) Apply(context.Context, *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	return nil, ErrUsageBillingRequestConflict
}

func TestAPIKeyRouteMetricsSnapshot(t *testing.T) {
	metrics := NewAPIKeyRouteMetrics()
	metrics.RecordRouteRequest()
	metrics.RecordFallbackSuccess()
	metrics.RecordCandidateSkip(RouteFailureUnsupportedModel)
	metrics.RecordCandidateSkip(RouteFailureAdmission)
	metrics.RecordCircuitTransition(RouteCircuitOpened)
	metrics.RecordCircuitTransition(RouteCircuitHalfOpen)
	metrics.RecordCircuitTransition(RouteCircuitClosed)
	metrics.RecordStickyHit()
	metrics.RecordInvalidSticky()
	metrics.RecordConfigValidationFailure()
	metrics.RecordStoreError()
	metrics.ObserveSwitchDuration(1250 * time.Millisecond)
	metrics.RecordBillingIdempotencyConflict()

	require.Equal(t, APIKeyRouteMetricsSnapshot{
		RouteRequests:               1,
		FallbackSuccesses:           1,
		CandidateSkips:              map[RouteFailureClass]uint64{RouteFailureUnsupportedModel: 1, RouteFailureAdmission: 1},
		CircuitTransitions:          map[RouteCircuitTransition]uint64{RouteCircuitOpened: 1, RouteCircuitHalfOpen: 1, RouteCircuitClosed: 1},
		StickyHits:                  1,
		InvalidStickyRecords:        1,
		ConfigValidationFailures:    1,
		StoreErrors:                 1,
		SwitchDurationCount:         1,
		SwitchDurationTotal:         1250 * time.Millisecond,
		BillingIdempotencyConflicts: 1,
	}, metrics.Snapshot())
}

func TestAPIKeyRouteMetricsAreAtomic(t *testing.T) {
	metrics := NewAPIKeyRouteMetrics()
	const workers = 32
	const iterations = 100
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				metrics.RecordRouteRequest()
				metrics.RecordCandidateSkip(RouteFailureAdmission)
				metrics.ObserveSwitchDuration(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	snapshot := metrics.Snapshot()
	require.Equal(t, uint64(workers*iterations), snapshot.RouteRequests)
	require.Equal(t, uint64(workers*iterations), snapshot.CandidateSkips[RouteFailureAdmission])
	require.Equal(t, uint64(workers*iterations), snapshot.SwitchDurationCount)
	require.Equal(t, time.Duration(workers*iterations)*time.Millisecond, snapshot.SwitchDurationTotal)
}

func TestNilAPIKeyRouteMetricsIsSafe(t *testing.T) {
	var metrics *APIKeyRouteMetrics
	metrics.RecordRouteRequest()
	metrics.RecordFallbackSuccess()
	metrics.RecordCandidateSkip(RouteFailureCapacity)
	metrics.RecordCircuitTransition(RouteCircuitOpened)
	metrics.RecordStickyHit()
	metrics.RecordInvalidSticky()
	metrics.RecordConfigValidationFailure()
	metrics.RecordStoreError()
	metrics.ObserveSwitchDuration(time.Second)
	metrics.RecordBillingIdempotencyConflict()
	require.Equal(t, APIKeyRouteMetricsSnapshot{}, metrics.Snapshot())
}

func TestRouteBillingConflictRecordsMetricOnlyForRouteAuditedUsage(t *testing.T) {
	groupID := int64(1)
	params := &postUsageBillingParams{
		Cost:    &CostBreakdown{TotalCost: 1, ActualCost: 1},
		User:    &User{ID: 2},
		APIKey:  &APIKey{ID: 3},
		Account: &Account{ID: 4},
	}
	before := GetAPIKeyRouteMetricsSnapshot()

	_, err := applyUsageBilling(context.Background(), "route-conflict", &UsageLog{
		SourceGroupID: &groupID,
	}, params, &billingDeps{}, routeBillingConflictRepo{})
	require.True(t, errors.Is(err, ErrUsageBillingRequestConflict))

	after := GetAPIKeyRouteMetricsSnapshot()
	require.Equal(t, before.BillingIdempotencyConflicts+1, after.BillingIdempotencyConflicts)

	_, err = applyUsageBilling(context.Background(), "ordinary-conflict", &UsageLog{}, params, &billingDeps{}, routeBillingConflictRepo{})
	require.True(t, errors.Is(err, ErrUsageBillingRequestConflict))
	require.Equal(t, after.BillingIdempotencyConflicts, GetAPIKeyRouteMetricsSnapshot().BillingIdempotencyConflicts)
}

func TestRouteBillingLogRecordsActualMultiplierAndReplayResult(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "route-request-123")))
	sourceGroupID := int64(1)
	effectiveGroupID := int64(2)
	subscriptionID := int64(8)
	usage := &UsageLog{
		APIKeyID:            3,
		RequestID:           "route-billing-replay",
		GroupID:             &effectiveGroupID,
		SourceGroupID:       &sourceGroupID,
		SubscriptionID:      &subscriptionID,
		RouteFallbackUsed:   true,
		RouteAttemptCount:   2,
		RouteFallbackReason: routePlanStringPtr(string(RouteFailureCapacity)),
		RouteStickyHit:      true,
		RateMultiplier:      1.75,
	}
	params := &postUsageBillingParams{
		Cost:    &CostBreakdown{TotalCost: 1, ActualCost: 1.75},
		User:    &User{ID: 2},
		APIKey:  &APIKey{ID: 3},
		Account: &Account{ID: 4},
	}

	applied, err := applyUsageBilling(ctx, usage.RequestID, usage, params, &billingDeps{deferredService: &DeferredService{}}, routeBillingReplayRepo{})
	require.NoError(t, err)
	require.False(t, applied)

	entries := logs.FilterMessage("api_key_route.billing").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "route-request-123", fields["request_id"])
	require.Equal(t, usage.RequestID, fields["billing_request_id"])
	require.EqualValues(t, 3, fields["api_key_id"])
	require.EqualValues(t, 1, fields["source_group_id"])
	require.EqualValues(t, 2, fields["effective_group_id"])
	require.EqualValues(t, 8, fields["effective_subscription_id"])
	require.Equal(t, 1.75, fields["effective_multiplier"])
	require.Equal(t, "replayed", fields["billing_result"])
	require.Equal(t, true, fields["route_fallback_used"])
	require.EqualValues(t, 2, fields["route_attempt_count"])
	require.Equal(t, string(RouteFailureCapacity), fields["route_fallback_reason"])
	require.Equal(t, true, fields["route_sticky_hit"])
}

func TestRouteBillingLegacyPathDoesNotClaimApplied(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core).With(zap.String("request_id", "route-request-legacy")))
	sourceGroupID := int64(1)
	effectiveGroupID := int64(2)
	usage := &UsageLog{
		APIKeyID:      3,
		RequestID:     "route-billing-legacy",
		GroupID:       &effectiveGroupID,
		SourceGroupID: &sourceGroupID,
	}
	params := &postUsageBillingParams{
		Cost:    &CostBreakdown{},
		User:    &User{ID: 2},
		APIKey:  &APIKey{ID: 3},
		Account: &Account{ID: 4},
	}

	_, err := applyUsageBilling(ctx, usage.RequestID, usage, params, &billingDeps{}, nil)
	require.NoError(t, err)

	entries := logs.FilterMessage("api_key_route.billing").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "legacy_unverified", fields["billing_result"])
	require.Equal(t, "route-request-legacy", fields["request_id"])
	require.Equal(t, usage.RequestID, fields["billing_request_id"])
}

func routePlanStringPtr(value string) *string { return &value }
