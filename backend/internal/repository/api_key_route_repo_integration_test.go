//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type apiKeyRouteRepoFixture struct {
	ctx    context.Context
	client *dbent.Client
	repo   *apiKeyRepository
	userID int64
	groups []int64
	suffix string
}

func newAPIKeyRouteRepoFixture(t *testing.T, groupCount int) *apiKeyRouteRepoFixture {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	client := testEntClient(t)
	user, err := client.User.Create().
		SetEmail("route-repo-" + suffix + "@test.com").
		SetPasswordHash("hash").
		SetStatus(service.StatusActive).
		SetRole(service.RoleUser).
		Save(ctx)
	require.NoError(t, err)

	fixture := &apiKeyRouteRepoFixture{
		ctx:    ctx,
		client: client,
		repo:   NewAPIKeyRepository(client, integrationDB).(*apiKeyRepository),
		userID: user.ID,
		suffix: suffix,
	}
	for i := 0; i < groupCount; i++ {
		group, createErr := client.Group.Create().
			SetName(fmt.Sprintf("route-repo-%s-%d", suffix, i)).
			SetStatus(service.StatusActive).
			SetPlatform(service.PlatformOpenAI).
			SetSubscriptionType(service.SubscriptionTypeStandard).
			Save(ctx)
		require.NoError(t, createErr)
		fixture.groups = append(fixture.groups, group.ID)
	}

	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE user_id = $1", user.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		for _, groupID := range fixture.groups {
			_, _ = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id = $1", groupID)
		}
	})
	return fixture
}

func (f *apiKeyRouteRepoFixture) newKey(label string, primaryGroupID int64) *service.APIKey {
	return &service.APIKey{
		UserID:  f.userID,
		Key:     fmt.Sprintf("sk-route-%s-%s", f.suffix, label),
		Name:    label,
		GroupID: &primaryGroupID,
		Status:  service.StatusActive,
	}
}

func fallbackGroupIDs(key *service.APIKey) []int64 {
	ids := make([]int64, 0, len(key.FallbackTargets))
	for _, target := range key.FallbackTargets {
		ids = append(ids, target.TargetGroupID)
	}
	return ids
}

func TestAPIKeyRepositoryCreateWithRouteIsAtomic(t *testing.T) {
	f := newAPIKeyRouteRepoFixture(t, 3)

	key := f.newKey("create", f.groups[0])
	require.NoError(t, f.repo.CreateWithRoute(f.ctx, key, []int64{f.groups[1], f.groups[2]}))
	loaded, err := f.repo.GetByID(f.ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{f.groups[1], f.groups[2]}, fallbackGroupIDs(loaded))

	duplicate := f.newKey("duplicate", f.groups[0])
	err = f.repo.CreateWithRoute(f.ctx, duplicate, []int64{f.groups[1], f.groups[1]})
	require.Error(t, err)
	exists, existsErr := f.repo.ExistsByKey(f.ctx, duplicate.Key)
	require.NoError(t, existsErr)
	require.False(t, exists)
}

func TestAPIKeyRepositoryUpdateWithRouteReplacesAndIncrementsVersion(t *testing.T) {
	f := newAPIKeyRouteRepoFixture(t, 4)
	key := f.newKey("update", f.groups[0])
	require.NoError(t, f.repo.CreateWithRoute(f.ctx, key, []int64{f.groups[1], f.groups[2]}))

	require.NoError(t, f.repo.UpdateWithRoute(f.ctx, key, service.APIKeyUpdateFields{RouteConfig: true}, service.APIKeyRouteMutation{
		ReplaceTargets:   true,
		TargetGroupIDs:   []int64{f.groups[3]},
		IncrementVersion: true,
	}))

	loaded, err := f.repo.GetByID(f.ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), loaded.RouteConfigVersion)
	require.Equal(t, []int64{f.groups[3]}, fallbackGroupIDs(loaded))
}

func TestAPIKeyRepositoryLoadsTargetsInPriorityOrder(t *testing.T) {
	f := newAPIKeyRouteRepoFixture(t, 4)
	key := f.newKey("loads", f.groups[0])
	want := []int64{f.groups[3], f.groups[1], f.groups[2]}
	require.NoError(t, f.repo.CreateWithRoute(f.ctx, key, want))

	byID, err := f.repo.GetByID(f.ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, want, fallbackGroupIDs(byID))

	byKey, err := f.repo.GetByKeyForAuth(f.ctx, key.Key)
	require.NoError(t, err)
	require.Equal(t, want, fallbackGroupIDs(byKey))
	for _, target := range byKey.FallbackTargets {
		require.NotNil(t, target.Group)
	}

	listed, _, err := f.repo.ListByUserID(f.ctx, f.userID, pagination.PaginationParams{Page: 1, PageSize: 10}, service.APIKeyListFilters{})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, want, fallbackGroupIDs(&listed[0]))
}

func TestAPIKeyRepositoryListKeysByGroupIDIncludesFallbackReferences(t *testing.T) {
	f := newAPIKeyRouteRepoFixture(t, 3)
	primary := f.newKey("primary", f.groups[0])
	require.NoError(t, f.repo.CreateWithRoute(f.ctx, primary, []int64{f.groups[1]}))
	fallback := f.newKey("fallback", f.groups[2])
	require.NoError(t, f.repo.CreateWithRoute(f.ctx, fallback, []int64{f.groups[0]}))
	primaryAndFallback := f.newKey("dedupe", f.groups[0])
	require.NoError(t, f.repo.CreateWithRoute(f.ctx, primaryAndFallback, []int64{f.groups[0]}))

	keys, err := f.repo.ListKeysByGroupID(f.ctx, f.groups[0])
	require.NoError(t, err)
	require.ElementsMatch(t, []string{primary.Key, fallback.Key, primaryAndFallback.Key}, keys)
}
