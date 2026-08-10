package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const invalidRouteFailoverPolicyReason = "INVALID_ROUTE_FAILOVER_POLICY"

type RouteFailoverManager struct {
	repo      RouteFailoverConfigRepository
	groupRepo RouteFailoverGroupRepository
	planner   *RouteFailoverPlanner
}

func NewRouteFailoverManager(repo RouteFailoverConfigRepository, groupRepo RouteFailoverGroupRepository, planner *RouteFailoverPlanner) *RouteFailoverManager {
	return &RouteFailoverManager{repo: repo, groupRepo: groupRepo, planner: planner}
}

func (m *RouteFailoverManager) Get(ctx context.Context, sourceGroupID int64) (*RouteFailoverPolicy, error) {
	return m.repo.GetBySourceGroupID(ctx, sourceGroupID)
}

func (m *RouteFailoverManager) Save(ctx context.Context, sourceGroupID int64, policy RouteFailoverPolicy) (*RouteFailoverPolicy, error) {
	policy.SourceGroupID = sourceGroupID
	applyRouteFailoverDefaults(&policy)
	if err := m.validate(ctx, &policy); err != nil {
		return nil, err
	}
	saved, err := m.repo.Save(ctx, &policy)
	if err != nil {
		return nil, err
	}
	if m.planner != nil {
		_ = m.planner.Reload(ctx)
	}
	return saved, nil
}

func (m *RouteFailoverManager) Delete(ctx context.Context, sourceGroupID int64) error {
	if err := m.repo.Delete(ctx, sourceGroupID); err != nil {
		return err
	}
	if m.planner != nil {
		_ = m.planner.Reload(ctx)
	}
	return nil
}

func applyRouteFailoverDefaults(policy *RouteFailoverPolicy) {
	if policy.MaxAttempts == 0 {
		policy.MaxAttempts = 3
	}
	if policy.FailureThreshold == 0 {
		policy.FailureThreshold = 5
	}
	if policy.SuccessThreshold == 0 {
		policy.SuccessThreshold = 2
	}
	if policy.Window == 0 {
		policy.Window = time.Minute
	}
	if policy.OpenCooldown == 0 {
		policy.OpenCooldown = time.Minute
	}
	if policy.HalfOpenLease == 0 {
		policy.HalfOpenLease = 15 * time.Second
	}
}

func (m *RouteFailoverManager) validate(ctx context.Context, policy *RouteFailoverPolicy) error {
	if policy.MaxAttempts < 1 || policy.MaxAttempts > 10 {
		return invalidRouteFailoverPolicy("max_attempts must be between 1 and 10")
	}
	if policy.FailureThreshold < 1 || policy.SuccessThreshold < 1 {
		return invalidRouteFailoverPolicy("failure and success thresholds must be positive")
	}
	if policy.Window <= 0 || policy.OpenCooldown <= 0 || policy.HalfOpenLease <= 0 {
		return invalidRouteFailoverPolicy("circuit durations must be positive")
	}
	source, err := m.groupRepo.GetByIDLite(ctx, policy.SourceGroupID)
	if err != nil {
		return err
	}
	if source.Platform == PlatformComposite {
		return invalidRouteFailoverPolicy("composite source groups do not support route failover")
	}
	if err := m.validateTargetIDs(ctx, policy); err != nil {
		return err
	}
	seen := make(map[int64]struct{}, len(policy.Targets))
	for i := range policy.Targets {
		target := &policy.Targets[i]
		if target.TargetGroupID == policy.SourceGroupID {
			return invalidRouteFailoverPolicy("source group cannot be its own failover target")
		}
		if _, duplicate := seen[target.TargetGroupID]; duplicate {
			return invalidRouteFailoverPolicy("duplicate failover target group %d", target.TargetGroupID)
		}
		seen[target.TargetGroupID] = struct{}{}
		group, groupErr := m.groupRepo.GetByIDLite(ctx, target.TargetGroupID)
		if groupErr != nil {
			return groupErr
		}
		if !group.IsActive() {
			return invalidRouteFailoverPolicy("failover target group %d is inactive", group.ID)
		}
		if group.Platform != source.Platform {
			return invalidRouteFailoverPolicy("failover target group %d must use platform %s", group.ID, source.Platform)
		}
		if group.SubscriptionType != source.SubscriptionType {
			return invalidRouteFailoverPolicy("failover target group %d must use subscription type %s", group.ID, source.SubscriptionType)
		}
		for from, to := range target.ModelMapping {
			if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
				return invalidRouteFailoverPolicy("model mappings require non-empty source and target models")
			}
		}
	}
	return nil
}

func (m *RouteFailoverManager) validateTargetIDs(ctx context.Context, policy *RouteFailoverPolicy) error {
	requested := make(map[int64]struct{}, len(policy.Targets))
	for _, target := range policy.Targets {
		if target.ID <= 0 {
			continue
		}
		if _, duplicate := requested[target.ID]; duplicate {
			return invalidRouteFailoverPolicy("duplicate failover target id %d", target.ID)
		}
		requested[target.ID] = struct{}{}
	}
	if len(requested) == 0 {
		return nil
	}

	existing, err := m.repo.GetBySourceGroupID(ctx, policy.SourceGroupID)
	if err != nil {
		if errors.Is(err, ErrRouteFailoverPolicyNotFound) {
			return invalidRouteFailoverPolicy("failover target ids require an existing policy")
		}
		return err
	}
	existingIDs := make(map[int64]struct{}, len(existing.Targets))
	for _, target := range existing.Targets {
		existingIDs[target.ID] = struct{}{}
	}
	for targetID := range requested {
		if _, ok := existingIDs[targetID]; !ok {
			return invalidRouteFailoverPolicy("failover target id %d does not belong to source group %d", targetID, policy.SourceGroupID)
		}
	}
	return nil
}

func invalidRouteFailoverPolicy(format string, args ...any) error {
	return infraerrors.BadRequest(invalidRouteFailoverPolicyReason, fmt.Sprintf(format, args...))
}
