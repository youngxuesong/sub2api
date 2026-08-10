package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type routeFailoverRepository struct {
	db *sql.DB
}

func NewRouteFailoverRepository(db *sql.DB) service.RouteFailoverConfigRepository {
	return &routeFailoverRepository{db: db}
}

const routeFailoverSnapshotQuery = `
SELECT p.id, p.source_group_id, p.enabled, p.max_attempts,
       p.failure_threshold, p.success_threshold, p.window_seconds,
       p.open_cooldown_seconds, p.half_open_lease_seconds,
       t.id, t.target_group_id, t.priority, t.enabled, t.model_mapping,
       COALESCE((EXTRACT(EPOCH FROM MAX(GREATEST(p.updated_at, COALESCE(t.updated_at, p.updated_at))) OVER ()) * 1000000)::BIGINT, 0)
FROM route_failover_policies p
LEFT JOIN groups source_group ON source_group.id = p.source_group_id
LEFT JOIN route_failover_targets t ON t.policy_id = p.id
  AND source_group.status = 'active'
  AND source_group.platform <> 'composite'
  AND EXISTS (
      SELECT 1
      FROM groups target_group
      WHERE target_group.id = t.target_group_id
        AND target_group.status = 'active'
        AND target_group.platform = source_group.platform
        AND target_group.subscription_type = source_group.subscription_type
  )
ORDER BY p.source_group_id, t.priority, t.id`

func (r *routeFailoverRepository) LoadSnapshot(ctx context.Context) (service.RouteFailoverSnapshot, error) {
	rows, err := r.db.QueryContext(ctx, routeFailoverSnapshotQuery)
	if err != nil {
		return service.RouteFailoverSnapshot{}, fmt.Errorf("load route failover snapshot: %w", err)
	}
	defer rows.Close()

	snapshot := service.RouteFailoverSnapshot{}
	policyIndex := make(map[int64]int)
	for rows.Next() {
		var policy service.RouteFailoverPolicy
		var targetID, targetGroupID sql.NullInt64
		var targetPriority sql.NullInt64
		var targetEnabled sql.NullBool
		var modelMapping []byte
		var windowSeconds, cooldownSeconds, leaseSeconds int64
		if err := rows.Scan(
			&policy.ID, &policy.SourceGroupID, &policy.Enabled, &policy.MaxAttempts,
			&policy.FailureThreshold, &policy.SuccessThreshold, &windowSeconds,
			&cooldownSeconds, &leaseSeconds, &targetID, &targetGroupID,
			&targetPriority, &targetEnabled, &modelMapping, &snapshot.Version,
		); err != nil {
			return service.RouteFailoverSnapshot{}, fmt.Errorf("scan route failover snapshot: %w", err)
		}
		policy.Window = time.Duration(windowSeconds) * time.Second
		policy.OpenCooldown = time.Duration(cooldownSeconds) * time.Second
		policy.HalfOpenLease = time.Duration(leaseSeconds) * time.Second

		idx, exists := policyIndex[policy.ID]
		if !exists {
			idx = len(snapshot.Policies)
			policyIndex[policy.ID] = idx
			snapshot.Policies = append(snapshot.Policies, policy)
		}
		if targetID.Valid {
			target := service.RouteFailoverTarget{
				ID: targetID.Int64, PolicyID: policy.ID, TargetGroupID: targetGroupID.Int64,
				Priority: int(targetPriority.Int64), Enabled: targetEnabled.Bool,
			}
			if len(modelMapping) > 0 {
				if err := json.Unmarshal(modelMapping, &target.ModelMapping); err != nil {
					return service.RouteFailoverSnapshot{}, fmt.Errorf("decode route failover model mapping: %w", err)
				}
			}
			snapshot.Policies[idx].Targets = append(snapshot.Policies[idx].Targets, target)
		}
	}
	if err := rows.Err(); err != nil {
		return service.RouteFailoverSnapshot{}, fmt.Errorf("iterate route failover snapshot: %w", err)
	}
	return snapshot, nil
}

func (r *routeFailoverRepository) GetBySourceGroupID(ctx context.Context, sourceGroupID int64) (*service.RouteFailoverPolicy, error) {
	snapshot, err := r.LoadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	for i := range snapshot.Policies {
		if snapshot.Policies[i].SourceGroupID == sourceGroupID {
			return &snapshot.Policies[i], nil
		}
	}
	return nil, service.ErrRouteFailoverPolicyNotFound
}

func (r *routeFailoverRepository) Save(ctx context.Context, policy *service.RouteFailoverPolicy) (*service.RouteFailoverPolicy, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var policyID int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO route_failover_policies (
    source_group_id, enabled, max_attempts, failure_threshold, success_threshold,
    window_seconds, open_cooldown_seconds, half_open_lease_seconds, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NOW())
ON CONFLICT (source_group_id) DO UPDATE SET
    enabled=EXCLUDED.enabled, max_attempts=EXCLUDED.max_attempts,
    failure_threshold=EXCLUDED.failure_threshold, success_threshold=EXCLUDED.success_threshold,
    window_seconds=EXCLUDED.window_seconds, open_cooldown_seconds=EXCLUDED.open_cooldown_seconds,
    half_open_lease_seconds=EXCLUDED.half_open_lease_seconds, updated_at=NOW()
RETURNING id`, policy.SourceGroupID, policy.Enabled, policy.MaxAttempts,
		policy.FailureThreshold, policy.SuccessThreshold, int64(policy.Window/time.Second),
		int64(policy.OpenCooldown/time.Second), int64(policy.HalfOpenLease/time.Second)).Scan(&policyID)
	if err != nil {
		return nil, fmt.Errorf("save route failover policy: %w", err)
	}
	savedTargetIDs := make([]int64, 0, len(policy.Targets))
	for _, target := range policy.Targets {
		mapping, marshalErr := json.Marshal(target.ModelMapping)
		if marshalErr != nil {
			return nil, fmt.Errorf("encode route failover model mapping: %w", marshalErr)
		}
		var targetID int64
		if target.ID > 0 {
			err = tx.QueryRowContext(ctx, `
UPDATE route_failover_targets
SET target_group_id=$3, priority=$4, enabled=$5, model_mapping=$6, updated_at=NOW()
WHERE id=$1 AND policy_id=$2
RETURNING id`, target.ID, policyID, target.TargetGroupID, target.Priority, target.Enabled, string(mapping)).Scan(&targetID)
			if err != nil {
				return nil, fmt.Errorf("update route failover target %d: %w", target.ID, err)
			}
		} else {
			err = tx.QueryRowContext(ctx, `
INSERT INTO route_failover_targets (policy_id, target_group_id, priority, enabled, model_mapping)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (policy_id, target_group_id) DO UPDATE SET
    priority=EXCLUDED.priority, enabled=EXCLUDED.enabled,
    model_mapping=EXCLUDED.model_mapping, updated_at=NOW()
RETURNING id`, policyID, target.TargetGroupID, target.Priority, target.Enabled, string(mapping)).Scan(&targetID)
			if err != nil {
				return nil, fmt.Errorf("save route failover target: %w", err)
			}
		}
		savedTargetIDs = append(savedTargetIDs, targetID)
	}
	deleteQuery := `DELETE FROM route_failover_targets WHERE policy_id=$1`
	deleteArgs := []any{policyID}
	if len(savedTargetIDs) > 0 {
		placeholders := make([]string, 0, len(savedTargetIDs))
		for i, targetID := range savedTargetIDs {
			placeholders = append(placeholders, fmt.Sprintf("$%d", i+2))
			deleteArgs = append(deleteArgs, targetID)
		}
		deleteQuery += ` AND id NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	if _, err = tx.ExecContext(ctx, deleteQuery, deleteArgs...); err != nil {
		return nil, fmt.Errorf("delete removed route failover targets: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit route failover policy: %w", err)
	}
	return r.GetBySourceGroupID(ctx, policy.SourceGroupID)
}

func (r *routeFailoverRepository) Delete(ctx context.Context, sourceGroupID int64) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM route_failover_policies WHERE source_group_id=$1`, sourceGroupID)
	if err != nil {
		return fmt.Errorf("delete route failover policy: %w", err)
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr == nil && affected == 0 {
		return service.ErrRouteFailoverPolicyNotFound
	}
	return nil
}
