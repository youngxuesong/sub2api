package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRouteFailoverCircuitUsesGroupAndRequestedModelRedisKeys(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	circuit := newRouteFailoverCircuit(client, time.Now)
	ctx := context.Background()

	for i := 0; i < routeFailoverThreshold; i++ {
		require.NoError(t, circuit.RecordFailure(ctx, 42, "claude-sonnet", ""))
	}
	allowed, _, _, err := circuit.Allow(ctx, 42, "claude-sonnet")
	require.NoError(t, err)
	require.False(t, allowed)

	allowed, _, _, err = circuit.Allow(ctx, 43, "claude-sonnet")
	require.NoError(t, err)
	require.True(t, allowed, "a different effective group must have an isolated circuit")
	allowed, _, _, err = circuit.Allow(ctx, 42, "claude-opus")
	require.NoError(t, err)
	require.True(t, allowed, "a different requested model must have an isolated circuit")

	hash := sha256.Sum256([]byte("claude-sonnet"))
	prefix := "route_failover:group:42:" + hex.EncodeToString(hash[:8])
	require.True(t, server.Exists(prefix+":state"))
	require.True(t, server.Exists(prefix+":failures"))
}

func TestRouteFailoverCircuitOpensAfterFiveFailuresAndRequiresTwoHalfOpenSuccesses(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	now := time.Unix(1_700_000_000, 0)
	circuit := newRouteFailoverCircuit(client, func() time.Time { return now })
	ctx := context.Background()

	for i := 0; i < routeFailoverThreshold-1; i++ {
		require.NoError(t, circuit.RecordFailure(ctx, 2, "model", ""))
		allowed, _, _, err := circuit.Allow(ctx, 2, "model")
		require.NoError(t, err)
		require.True(t, allowed)
	}
	require.NoError(t, circuit.RecordFailure(ctx, 2, "model", ""))
	allowed, _, _, err := circuit.Allow(ctx, 2, "model")
	require.NoError(t, err)
	require.False(t, allowed)

	now = now.Add(routeFailoverCooldown + time.Millisecond)
	allowed, halfOpen, firstLease, err := circuit.Allow(ctx, 2, "model")
	require.NoError(t, err)
	require.True(t, allowed)
	require.True(t, halfOpen)
	require.NotEmpty(t, firstLease)

	allowed, _, _, err = circuit.Allow(ctx, 2, "model")
	require.NoError(t, err)
	require.False(t, allowed, "the half-open lease admits only one probe at a time")

	require.NoError(t, circuit.RecordSuccess(ctx, 2, "model", firstLease))
	allowed, halfOpen, secondLease, err := circuit.Allow(ctx, 2, "model")
	require.NoError(t, err)
	require.True(t, allowed)
	require.True(t, halfOpen)
	require.NoError(t, circuit.RecordSuccess(ctx, 2, "model", secondLease))

	allowed, halfOpen, _, err = circuit.Allow(ctx, 2, "model")
	require.NoError(t, err)
	require.True(t, allowed)
	require.False(t, halfOpen)
}

func TestRouteFailoverCircuitClosedSuccessClearsFailureWindow(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	circuit := newRouteFailoverCircuit(client, time.Now)
	ctx := context.Background()

	for i := 0; i < routeFailoverThreshold-1; i++ {
		require.NoError(t, circuit.RecordFailure(ctx, 7, "model", ""))
	}
	require.NoError(t, circuit.RecordSuccess(ctx, 7, "model", ""))
	require.NoError(t, circuit.RecordFailure(ctx, 7, "model", ""))
	allowed, _, _, err := circuit.Allow(ctx, 7, "model")
	require.NoError(t, err)
	require.True(t, allowed, "a closed-state success must clear prior failures")
}

func TestRouteFailoverCircuitExpiresFailuresOutsideFixedWindow(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	now := time.Unix(1_700_000_000, 0)
	circuit := newRouteFailoverCircuit(client, func() time.Time { return now })
	ctx := context.Background()

	for i := 0; i < routeFailoverThreshold-1; i++ {
		require.NoError(t, circuit.RecordFailure(ctx, 7, "model", ""))
	}
	now = now.Add(routeFailoverWindow + time.Millisecond)
	require.NoError(t, circuit.RecordFailure(ctx, 7, "model", ""))
	allowed, _, _, err := circuit.Allow(ctx, 7, "model")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestRouteFailoverCircuitIgnoresExpiredHalfOpenLeaseResults(t *testing.T) {
	for _, outcome := range []string{"success", "failure"} {
		t.Run(outcome, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			now := time.Unix(1_700_000_000, 0)
			circuit := newRouteFailoverCircuit(client, func() time.Time { return now })
			ctx := context.Background()

			for i := 0; i < routeFailoverThreshold; i++ {
				require.NoError(t, circuit.RecordFailure(ctx, 2, "model", ""))
			}
			now = now.Add(routeFailoverCooldown + time.Millisecond)
			_, _, firstLease, err := circuit.Allow(ctx, 2, "model")
			require.NoError(t, err)
			server.FastForward(routeFailoverLease + time.Millisecond)
			_, _, secondLease, err := circuit.Allow(ctx, 2, "model")
			require.NoError(t, err)

			if outcome == "success" {
				require.NoError(t, circuit.RecordSuccess(ctx, 2, "model", firstLease))
			} else {
				require.NoError(t, circuit.RecordFailure(ctx, 2, "model", firstLease))
			}
			allowed, _, _, err := circuit.Allow(ctx, 2, "model")
			require.NoError(t, err)
			require.False(t, allowed)

			require.NoError(t, circuit.RecordSuccess(ctx, 2, "model", secondLease))
			server.FastForward(routeFailoverLease + time.Millisecond)
			_, _, thirdLease, err := circuit.Allow(ctx, 2, "model")
			require.NoError(t, err)
			require.NoError(t, circuit.RecordSuccess(ctx, 2, "model", thirdLease))
		})
	}
}
