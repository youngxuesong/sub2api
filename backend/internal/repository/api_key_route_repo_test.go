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
