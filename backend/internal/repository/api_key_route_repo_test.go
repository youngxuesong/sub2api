package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyRepositoryUpdateWithRouteIgnoresUnscopedIncrementVersion(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "route-version-scope@test.com")
	key := &service.APIKey{
		UserID: user.ID,
		Key:    "sk-route-version-scope",
		Name:   "Route Version Scope",
		Status: service.StatusActive,
	}
	require.NoError(t, repo.Create(ctx, key))
	require.Equal(t, int64(1), key.RouteConfigVersion)

	require.NoError(t, repo.UpdateWithRoute(ctx, key, service.APIKeyUpdateFields{}, service.APIKeyRouteMutation{
		IncrementVersion: true,
	}))

	require.Equal(t, int64(1), key.RouteConfigVersion)
	loaded, err := repo.GetByID(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), loaded.RouteConfigVersion)
}

func TestAPIKeyRepositoryUpdateRejectsStaleRouteVersionGuard(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "route-version-conflict@test.com")
	key := &service.APIKey{
		UserID: user.ID,
		Key:    "sk-route-version-conflict",
		Name:   "before",
		Status: service.StatusActive,
	}
	require.NoError(t, repo.Create(ctx, key))
	stale := *key

	require.NoError(t, repo.UpdateWithRoute(ctx, key, service.APIKeyUpdateFields{RouteConfig: true}, service.APIKeyRouteMutation{
		IncrementVersion: true,
	}))
	require.Equal(t, int64(2), key.RouteConfigVersion)

	stale.Name = "stale update"
	err := repo.Update(ctx, &stale, service.APIKeyUpdateFields{
		Name:                      true,
		RequireRouteConfigVersion: true,
	})
	require.ErrorIs(t, err, service.ErrAPIKeyRouteConfigConflict)

	loaded, err := repo.GetByID(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, "before", loaded.Name)
	require.Equal(t, int64(2), loaded.RouteConfigVersion)
}

func TestAPIKeyRepositoryUpdateWithRouteRejectsStaleRouteVersionGuard(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "route-transaction-conflict@test.com")
	key := &service.APIKey{
		UserID: user.ID,
		Key:    "sk-route-transaction-conflict",
		Name:   "before",
		Status: service.StatusActive,
	}
	require.NoError(t, repo.Create(ctx, key))
	stale := *key

	require.NoError(t, repo.UpdateWithRoute(ctx, key, service.APIKeyUpdateFields{RouteConfig: true}, service.APIKeyRouteMutation{
		IncrementVersion: true,
	}))
	require.Equal(t, int64(2), key.RouteConfigVersion)

	err := repo.UpdateWithRoute(ctx, &stale, service.APIKeyUpdateFields{
		RouteConfig:               true,
		RequireRouteConfigVersion: true,
	}, service.APIKeyRouteMutation{
		ReplaceTargets:   true,
		IncrementVersion: true,
	})
	require.ErrorIs(t, err, service.ErrAPIKeyRouteConfigConflict)

	loaded, err := repo.GetByID(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), loaded.RouteConfigVersion)
}

func TestAPIKeyRepositoryGuardOnlyUpdateRejectsStaleRouteVersion(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "route-guard-only-conflict@test.com")
	key := &service.APIKey{
		UserID: user.ID,
		Key:    "sk-route-guard-only-conflict",
		Name:   "before",
		Status: service.StatusActive,
	}
	require.NoError(t, repo.Create(ctx, key))
	stale := *key

	require.NoError(t, repo.UpdateWithRoute(ctx, key, service.APIKeyUpdateFields{RouteConfig: true}, service.APIKeyRouteMutation{
		IncrementVersion: true,
	}))

	err := repo.Update(ctx, &stale, service.APIKeyUpdateFields{RequireRouteConfigVersion: true})
	require.ErrorIs(t, err, service.ErrAPIKeyRouteConfigConflict)
}

func TestAPIKeyRepositoryGuardedUpdatePreservesNotFound(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "route-guard-not-found@test.com")
	key := &service.APIKey{
		UserID: user.ID,
		Key:    "sk-route-guard-not-found",
		Name:   "before",
		Status: service.StatusActive,
	}
	require.NoError(t, repo.Create(ctx, key))
	require.NoError(t, repo.Delete(ctx, key.ID))

	key.Name = "after"
	err := repo.Update(ctx, key, service.APIKeyUpdateFields{
		Name:                      true,
		RequireRouteConfigVersion: true,
	})
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
}
