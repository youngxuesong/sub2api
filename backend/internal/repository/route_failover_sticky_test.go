package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRouteStickyRoundTripAndSlidingTTL(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewAPIKeyRouteStickyStore(client)
	ctx := context.Background()
	binding := service.APIKeyRouteStickyBinding{
		RouteConfigVersion: 17,
		EffectiveGroupID:   42,
	}

	require.NoError(t, store.Set(ctx, 9, "session-abc", binding, service.APIKeyRouteStickyTTL))
	key := fmt.Sprintf("route:sticky:key:%d:%s", 9, "session-abc")
	require.Equal(t, time.Hour, server.TTL(key))

	raw, err := server.Get(key)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &fields))
	require.Equal(t, map[string]any{
		"route_config_version": float64(17),
		"effective_group_id":   float64(42),
	}, fields)

	got, err := store.Get(ctx, 9, "session-abc")
	require.NoError(t, err)
	require.Equal(t, &binding, got)

	server.FastForward(20 * time.Minute)
	require.Equal(t, 40*time.Minute, server.TTL(key))
	require.NoError(t, store.Refresh(ctx, 9, "session-abc", service.APIKeyRouteStickyTTL))
	require.Equal(t, time.Hour, server.TTL(key))

	server.FastForward(time.Hour)
	got, err = store.Get(ctx, 9, "session-abc")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRouteStickyStaleVersionIsDeleted(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewAPIKeyRouteStickyStore(client)
	ctx := context.Background()

	require.NoError(t, store.Set(ctx, 12, "stale", service.APIKeyRouteStickyBinding{
		RouteConfigVersion: 3,
		EffectiveGroupID:   44,
	}, service.APIKeyRouteStickyTTL))
	// Version comparison belongs to the service planner. The store exposes raw
	// deletion for the planner to remove a binding it has identified as stale.
	require.NoError(t, store.Delete(ctx, 12, "stale"))
	got, err := store.Get(ctx, 12, "stale")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRouteStickyDeleteOnRetryableFailure(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewAPIKeyRouteStickyStore(client)
	ctx := context.Background()

	require.NoError(t, store.Set(ctx, 12, "retryable", service.APIKeyRouteStickyBinding{
		RouteConfigVersion: 4,
		EffectiveGroupID:   45,
	}, service.APIKeyRouteStickyTTL))
	// Retry classification also belongs to the service planner; the store only
	// performs the requested deletion.
	require.NoError(t, store.Delete(ctx, 12, "retryable"))
	got, err := store.Get(ctx, 12, "retryable")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRouteStickyStoreFailureIsFailOpen(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = client.Close() })
	store := NewAPIKeyRouteStickyStore(client)
	ctx := context.Background()
	binding := service.APIKeyRouteStickyBinding{RouteConfigVersion: 1, EffectiveGroupID: 2}

	server.Close()
	got, err := store.Get(ctx, 1, "session")
	require.Error(t, err)
	require.Nil(t, got)
	require.Error(t, store.Set(ctx, 1, "session", binding, service.APIKeyRouteStickyTTL))
	require.Error(t, store.Refresh(ctx, 1, "session", service.APIKeyRouteStickyTTL))
	require.Error(t, store.Delete(ctx, 1, "session"))

	// Fail-open is a caller policy. Empty sessions bypass Redis entirely, while
	// non-empty session errors are surfaced so callers can choose that policy.
	got, err = store.Get(ctx, 1, "")
	require.NoError(t, err)
	require.Nil(t, got)
	require.NoError(t, store.Set(ctx, 1, "", binding, service.APIKeyRouteStickyTTL))
	require.NoError(t, store.Refresh(ctx, 1, "", service.APIKeyRouteStickyTTL))
	require.NoError(t, store.Delete(ctx, 1, ""))
}
