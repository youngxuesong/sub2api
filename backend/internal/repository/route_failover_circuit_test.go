package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRouteFailoverCircuitOpensAndAllowsSingleHalfOpenProbe(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	now := time.Unix(1_700_000_000, 0)
	circuit := newRouteFailoverCircuit(client, func() time.Time { return now })
	policy := service.RouteFailoverPolicy{
		ID: 1, FailureThreshold: 2, SuccessThreshold: 1,
		Window: time.Minute, OpenCooldown: 30 * time.Second, HalfOpenLease: 10 * time.Second,
	}
	target := service.RouteFailoverTarget{ID: 9, TargetGroupID: 2}
	ctx := context.Background()

	permit, err := circuit.Allow(ctx, policy, target, "claude-sonnet")
	require.NoError(t, err)
	require.True(t, permit.Allowed)
	require.NoError(t, circuit.RecordFailure(ctx, policy, target, "claude-sonnet", ""))
	require.NoError(t, circuit.RecordFailure(ctx, policy, target, "claude-sonnet", ""))

	permit, err = circuit.Allow(ctx, policy, target, "claude-sonnet")
	require.NoError(t, err)
	require.False(t, permit.Allowed)

	now = now.Add(31 * time.Second)
	first, err := circuit.Allow(ctx, policy, target, "claude-sonnet")
	require.NoError(t, err)
	require.True(t, first.Allowed)
	require.True(t, first.HalfOpen)
	require.NotEmpty(t, first.LeaseID)
	second, err := circuit.Allow(ctx, policy, target, "claude-sonnet")
	require.NoError(t, err)
	require.False(t, second.Allowed)

	require.NoError(t, circuit.RecordSuccess(ctx, policy, target, "claude-sonnet", first.LeaseID))
	permit, err = circuit.Allow(ctx, policy, target, "claude-sonnet")
	require.NoError(t, err)
	require.True(t, permit.Allowed)
	require.False(t, permit.HalfOpen)
}

func TestRouteFailoverCircuitIgnoresExpiredHalfOpenLeaseResults(t *testing.T) {
	for _, outcome := range []string{"success", "failure"} {
		t.Run(outcome, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			now := time.Unix(1_700_000_000, 0)
			circuit := newRouteFailoverCircuit(client, func() time.Time { return now })
			policy := service.RouteFailoverPolicy{
				ID: 1, FailureThreshold: 1, SuccessThreshold: 1,
				Window: time.Minute, OpenCooldown: time.Second, HalfOpenLease: time.Second,
			}
			target := service.RouteFailoverTarget{ID: 9, TargetGroupID: 2}
			ctx := context.Background()

			require.NoError(t, circuit.RecordFailure(ctx, policy, target, "model", ""))
			now = now.Add(2 * time.Second)
			first, err := circuit.Allow(ctx, policy, target, "model")
			require.NoError(t, err)
			require.True(t, first.HalfOpen)

			server.FastForward(2 * time.Second)
			second, err := circuit.Allow(ctx, policy, target, "model")
			require.NoError(t, err)
			require.True(t, second.HalfOpen)
			require.NotEqual(t, first.LeaseID, second.LeaseID)

			if outcome == "success" {
				require.NoError(t, circuit.RecordSuccess(ctx, policy, target, "model", first.LeaseID))
			} else {
				require.NoError(t, circuit.RecordFailure(ctx, policy, target, "model", first.LeaseID))
			}
			blocked, err := circuit.Allow(ctx, policy, target, "model")
			require.NoError(t, err)
			require.False(t, blocked.Allowed, "an expired probe result must not override the active lease")

			require.NoError(t, circuit.RecordSuccess(ctx, policy, target, "model", second.LeaseID))
			allowed, err := circuit.Allow(ctx, policy, target, "model")
			require.NoError(t, err)
			require.True(t, allowed.Allowed)
		})
	}
}
