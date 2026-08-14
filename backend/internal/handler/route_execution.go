package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

// routeExecution owns the cross-group state for one request. Account retries
// remain in local; only an explicit route-level failure unlocks the next group.
type routeExecution struct {
	plan              *service.APIKeyRoutePlan
	candidate         *service.APIKeyRouteCandidate
	local             *FailoverState
	localSwitchBudget int
	semanticCommitted bool
	readyForNext      bool
	finished          bool
	release           []func()
	attemptedGroups   []int64
	switchStartedAt   time.Time
}

func newRouteExecution(plan *service.APIKeyRoutePlan, localSwitchBudget int) *routeExecution {
	if plan != nil {
		service.RecordAPIKeyRouteRequest()
	}
	return &routeExecution{
		plan: plan, localSwitchBudget: localSwitchBudget, readyForNext: true,
	}
}

func newAPIKeyRouteExecution(
	ctx context.Context,
	planner *service.APIKeyRoutePlanner,
	apiKey *service.APIKey,
	primarySubscription *service.UserSubscription,
	sessionHash string,
	requestedModel string,
	localSwitchBudget int,
) *routeExecution {
	if planner == nil {
		planner = service.NewAPIKeyRoutePlanner(nil, nil, nil)
	}
	return newRouteExecution(
		planner.NewPlan(ctx, apiKey, primarySubscription, sessionHash, requestedModel),
		localSwitchBudget,
	)
}

func (r *routeExecution) next(ctx context.Context) (*service.APIKeyRouteCandidate, bool) {
	if r == nil || r.plan == nil || r.finished {
		return nil, false
	}
	if routeExecutionContextDone(ctx) {
		r.releaseAttempt()
		r.finished = true
		return nil, false
	}
	if !r.readyForNext {
		return nil, false
	}
	candidate, ok := r.plan.Next(ctx)
	if !ok {
		r.finished = true
		return nil, false
	}
	r.candidate = candidate
	r.local = NewFailoverState(r.localSwitchBudget, false)
	r.semanticCommitted = false
	r.readyForNext = false
	r.release = nil
	r.attemptedGroups = append(r.attemptedGroups, candidate.EffectiveGroupID)
	if candidate.StickyHit {
		service.RecordAPIKeyRouteStickyHit()
	}
	return candidate, true
}

func (r *routeExecution) capacityFailed(ctx context.Context, err error) bool {
	return r.fail(ctx, service.APIKeyRouteAttemptFailure{
		Class:      service.RouteFailureCapacity,
		ReplaySafe: true,
		Cause:      err,
	})
}

func (r *routeExecution) candidateSkipped(ctx context.Context, class service.RouteFailureClass, err error) bool {
	if r == nil || r.plan == nil || r.candidate == nil || r.finished {
		return false
	}
	r.releaseAttempt()
	if routeExecutionContextDone(ctx) {
		r.finished = true
		r.recordAttemptFailure(ctx, service.APIKeyRouteAttemptFailure{
			Class: service.RouteFailureCanceled, Cause: context.Canceled,
		}, false)
		return false
	}
	advanced := r.plan.Skip(ctx, r.candidate, class)
	r.recordAttemptFailure(ctx, service.APIKeyRouteAttemptFailure{
		Class: class, ReplaySafe: true, Cause: err,
	}, advanced)
	if !advanced {
		r.finished = true
		return false
	}
	r.readyForNext = true
	return true
}

func (r *routeExecution) upstreamFailed(ctx context.Context, err error, replaySafe bool) bool {
	if r == nil || r.finished {
		return false
	}
	if routeExecutionContextDone(ctx) {
		r.releaseAttempt()
		r.finished = true
		r.recordAttemptFailure(ctx, service.APIKeyRouteAttemptFailure{
			Class: service.RouteFailureCanceled, Cause: context.Canceled,
		}, false)
		return false
	}
	var upstreamErr *service.UpstreamFailoverError
	if !errors.As(err, &upstreamErr) || upstreamErr == nil {
		r.releaseAttempt()
		r.finished = true
		r.recordAttemptFailure(ctx, service.APIKeyRouteAttemptFailure{
			Class: service.RouteFailureBusiness, ReplaySafe: replaySafe, Cause: err,
		}, false)
		return false
	}
	if upstreamErr.IsCredentialFailure() || upstreamErr.NextAccountAction == service.NextAccountStop {
		r.releaseAttempt()
		r.finished = true
		r.recordAttemptFailure(ctx, service.APIKeyRouteAttemptFailure{
			Class: service.RouteFailureBusiness, StatusCode: upstreamErr.StatusCode, ReplaySafe: replaySafe, Cause: err,
		}, false)
		return false
	}

	class, eligible := routeExecutionUpstreamFailureClass(err, upstreamErr.StatusCode)
	if !eligible {
		r.releaseAttempt()
		r.finished = true
		r.recordAttemptFailure(ctx, service.APIKeyRouteAttemptFailure{
			Class: class, StatusCode: upstreamErr.StatusCode, ReplaySafe: replaySafe, Cause: err,
		}, false)
		return false
	}
	return r.fail(ctx, service.APIKeyRouteAttemptFailure{
		Class:         class,
		StatusCode:    upstreamErr.StatusCode,
		ReplaySafe:    replaySafe,
		RecordCircuit: upstreamErr.ShouldRecordRouteFailover(),
		Cause:         err,
	})
}

func (r *routeExecution) markSemanticCommitted() {
	if r != nil {
		r.semanticCommitted = true
	}
}

func (r *routeExecution) succeed(ctx context.Context) service.RouteUsageAudit {
	if r == nil || r.plan == nil {
		return service.RouteUsageAudit{}
	}
	r.releaseAttempt()
	if !r.finished && r.candidate != nil && !routeExecutionContextDone(ctx) {
		r.plan.Succeed(ctx, r.candidate)
		if r.candidate.IsFallback {
			service.RecordAPIKeyRouteFallbackSuccess()
		}
		if !r.switchStartedAt.IsZero() {
			service.ObserveAPIKeyRouteSwitchDuration(time.Since(r.switchStartedAt))
		}
		r.logSuccess(ctx)
	}
	r.finished = true
	r.readyForNext = false
	return r.plan.Audit()
}

// releaseBeforeAdvance registers attempt-local resources, such as an account
// concurrency slot or reservation, that must be released before plan.Fail can
// authorize another group.
func (r *routeExecution) releaseBeforeAdvance(release func()) {
	if r != nil && release != nil {
		r.release = append(r.release, release)
	}
}

func (r *routeExecution) fail(ctx context.Context, failure service.APIKeyRouteAttemptFailure) bool {
	if r == nil || r.plan == nil || r.candidate == nil || r.finished {
		return false
	}
	r.releaseAttempt()
	if routeExecutionContextDone(ctx) {
		r.finished = true
		failure.Class = service.RouteFailureCanceled
		failure.Cause = context.Canceled
		r.recordAttemptFailure(ctx, failure, false)
		return false
	}
	failure.SemanticCommitted = r.semanticCommitted
	advanced := r.plan.Fail(ctx, r.candidate, failure)
	r.recordAttemptFailure(ctx, failure, advanced)
	if !advanced {
		r.finished = true
		return false
	}
	if r.switchStartedAt.IsZero() {
		r.switchStartedAt = time.Now()
	}
	r.readyForNext = true
	return true
}

func (r *routeExecution) recordAttemptFailure(ctx context.Context, failure service.APIKeyRouteAttemptFailure, advanced bool) {
	if r == nil || r.candidate == nil || r.candidate.EffectiveAPIKey == nil {
		return
	}
	service.RecordAPIKeyRouteCandidateSkip(failure.Class)
	key := r.candidate.EffectiveAPIKey
	fields := []zap.Field{
		zap.Int64("api_key_id", key.ID),
		zap.Int64("source_group_id", r.candidate.SourceGroupID),
		zap.Int64("effective_group_id", r.candidate.EffectiveGroupID),
		zap.Int64("route_version", key.RouteConfigVersion),
		zap.Bool("sticky_hit", r.candidate.StickyHit),
		zap.Int("attempt_count", r.plan.Audit().AttemptCount),
		zap.String("failure_class", string(failure.Class)),
		zap.Int("failure_status", failure.StatusCode),
		zap.Bool("replay_safe", failure.ReplaySafe),
		zap.Bool("semantic_committed", failure.SemanticCommitted),
		zap.Bool("route_advanced", advanced),
		zap.Strings("attempted_group_ids", routeExecutionStringIDs(r.attemptedGroups)),
		zap.Strings("configured_group_ids", routeExecutionConfiguredGroupIDs(r.candidate.SourceGroupID, key)),
	}
	logger.FromContext(ctx).Warn("api_key_route.attempt_failed", fields...)
}

func (r *routeExecution) logSuccess(ctx context.Context) {
	if r == nil || r.candidate == nil || r.candidate.EffectiveAPIKey == nil {
		return
	}
	key := r.candidate.EffectiveAPIKey
	fields := []zap.Field{
		zap.Int64("api_key_id", key.ID),
		zap.Int64("source_group_id", r.candidate.SourceGroupID),
		zap.Int64("effective_group_id", r.candidate.EffectiveGroupID),
		zap.Int64("route_version", key.RouteConfigVersion),
		zap.Bool("sticky_hit", r.candidate.StickyHit),
		zap.Int("attempt_count", r.plan.Audit().AttemptCount),
		zap.Strings("attempted_group_ids", routeExecutionStringIDs(r.attemptedGroups)),
		zap.String("failure_class", r.plan.Audit().FallbackReason),
		zap.Strings("configured_group_ids", routeExecutionConfiguredGroupIDs(r.candidate.SourceGroupID, key)),
	}
	if r.candidate.Subscription != nil {
		fields = append(fields, zap.Int64("effective_subscription_id", r.candidate.Subscription.ID))
	}
	logger.FromContext(ctx).Info("api_key_route.succeeded", fields...)
}

func routeExecutionConfiguredGroupIDs(sourceGroupID int64, key *service.APIKey) []string {
	if key == nil {
		return nil
	}
	result := make([]string, 0, len(key.FallbackTargets)+1)
	seen := map[int64]struct{}{sourceGroupID: {}}
	result = append(result, strconv.FormatInt(sourceGroupID, 10))
	for _, target := range key.FallbackTargets {
		if _, duplicate := seen[target.TargetGroupID]; duplicate {
			continue
		}
		seen[target.TargetGroupID] = struct{}{}
		result = append(result, strconv.FormatInt(target.TargetGroupID, 10))
	}
	return result
}

func routeExecutionStringIDs(ids []int64) []string {
	result := make([]string, len(ids))
	for i, id := range ids {
		result[i] = strconv.FormatInt(id, 10)
	}
	return result
}

func (r *routeExecution) releaseAttempt() {
	if r == nil {
		return
	}
	releases := r.release
	r.release = nil
	for i := len(releases) - 1; i >= 0; i-- {
		releases[i]()
	}
}

func routeExecutionUpstreamFailureClass(err error, statusCode int) (service.RouteFailureClass, bool) {
	if errors.Is(err, context.DeadlineExceeded) || statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout {
		return service.RouteFailureTimeout, true
	}
	if statusCode <= 0 {
		return service.RouteFailureConnection, true
	}
	if statusCode == http.StatusTooManyRequests {
		return service.RouteFailureUpstream429, true
	}
	if statusCode >= http.StatusInternalServerError && statusCode <= 599 {
		return service.RouteFailureUpstream5xx, true
	}
	return service.RouteFailureBusiness, false
}

func routeExecutionContextDone(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
