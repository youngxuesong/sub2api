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

func TestAPIKeyRepositoryCreateWithRouteDoesNotMutateCallerBeforeCommit(t *testing.T) {
	f := newAPIKeyRouteRepoFixture(t, 2)
	key := f.newKey("commit-failure", f.groups[0])
	before := *key
	functionName := "fail_api_key_route_commit_" + f.suffix
	triggerName := "fail_api_key_route_commit_trigger_" + f.suffix

	_, err := integrationDB.ExecContext(f.ctx, fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'forced deferred API key route failure';
END;
$$`, functionName))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), fmt.Sprintf(
			"DROP TRIGGER IF EXISTS %s ON api_keys", triggerName,
		))
		_, _ = integrationDB.ExecContext(context.Background(), fmt.Sprintf(
			"DROP FUNCTION IF EXISTS %s()", functionName,
		))
	})
	_, err = integrationDB.ExecContext(f.ctx, fmt.Sprintf(`
CREATE CONSTRAINT TRIGGER %s
AFTER INSERT ON api_keys
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
WHEN (NEW.key = '%s')
EXECUTE FUNCTION %s()`, triggerName, key.Key, functionName))
	require.NoError(t, err)

	err = f.repo.CreateWithRoute(f.ctx, key, []int64{f.groups[1]})
	require.Error(t, err)
	require.Equal(t, before, *key)

	exists, existsErr := f.repo.ExistsByKey(f.ctx, key.Key)
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

func TestAPIKeyRepositoryUpdateWithRouteReplacementAlwaysIncrementsVersion(t *testing.T) {
	f := newAPIKeyRouteRepoFixture(t, 3)
	key := f.newKey("implicit-version", f.groups[0])
	require.NoError(t, f.repo.CreateWithRoute(f.ctx, key, []int64{f.groups[1]}))

	// ReplaceTargets is the repository boundary's indication that the route
	// configuration changed; callers must not be able to accidentally skip the
	// version bump by omitting IncrementVersion.
	require.NoError(t, f.repo.UpdateWithRoute(f.ctx, key, service.APIKeyUpdateFields{}, service.APIKeyRouteMutation{
		ReplaceTargets: true,
		TargetGroupIDs: []int64{f.groups[2]},
	}))

	loaded, err := f.repo.GetByID(f.ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), loaded.RouteConfigVersion)
	require.Equal(t, []int64{f.groups[2]}, fallbackGroupIDs(loaded))
}

func TestAPIKeyRepositoryUpdateWithRouteRollsBackOnTargetInsertFailure(t *testing.T) {
	f := newAPIKeyRouteRepoFixture(t, 3)
	key := f.newKey("rollback", f.groups[0])
	require.NoError(t, f.repo.CreateWithRoute(f.ctx, key, []int64{f.groups[1], f.groups[2]}))

	key.Name = "changed inside transaction"
	err := f.repo.UpdateWithRoute(f.ctx, key, service.APIKeyUpdateFields{
		Name:        true,
		RouteConfig: true,
	}, service.APIKeyRouteMutation{
		ReplaceTargets:   true,
		TargetGroupIDs:   []int64{f.groups[1], f.groups[1]},
		IncrementVersion: true,
	})
	require.Error(t, err)

	loaded, loadErr := f.repo.GetByID(f.ctx, key.ID)
	require.NoError(t, loadErr)
	require.Equal(t, "rollback", loaded.Name)
	require.Equal(t, int64(1), loaded.RouteConfigVersion)
	require.Equal(t, []int64{f.groups[1], f.groups[2]}, fallbackGroupIDs(loaded))
}

func TestAPIKeyRepositoryRouteUsesOuterEntTransaction(t *testing.T) {
	f := newAPIKeyRouteRepoFixture(t, 2)
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(f.ctx, tx)
	// Use the normal repository client. The transaction is supplied only via
	// context, which is how production callers compose repository operations.
	txRepo := NewAPIKeyRepository(f.client, integrationDB).(*apiKeyRepository)
	key := f.newKey("outer-tx", f.groups[0])

	require.NoError(t, txRepo.CreateWithRoute(txCtx, key, []int64{f.groups[1]}))
	_, err := f.repo.GetByKey(f.ctx, key.Key)
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)

	require.NoError(t, tx.Commit())
	loaded, err := f.repo.GetByKey(f.ctx, key.Key)
	require.NoError(t, err)
	require.Equal(t, key.ID, loaded.ID)
}

func TestAPIKeyRepositoryUpdateWithRouteDoesNotMutateCallerOnFailure(t *testing.T) {
	f := newAPIKeyRouteRepoFixture(t, 3)
	key := f.newKey("caller-snapshot", f.groups[0])
	require.NoError(t, f.repo.CreateWithRoute(f.ctx, key, []int64{f.groups[1]}))
	key.Name = "updated name"
	before := *key
	functionName := "fail_api_key_route_update_commit_" + f.suffix
	triggerName := "fail_api_key_route_update_commit_trigger_" + f.suffix

	_, err := integrationDB.ExecContext(f.ctx, fmt.Sprintf(`
CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'forced deferred API key route update failure';
END;
$$`, functionName))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), fmt.Sprintf(
			"DROP TRIGGER IF EXISTS %s ON api_keys", triggerName,
		))
		_, _ = integrationDB.ExecContext(context.Background(), fmt.Sprintf(
			"DROP FUNCTION IF EXISTS %s()", functionName,
		))
	})
	_, err = integrationDB.ExecContext(f.ctx, fmt.Sprintf(`
CREATE CONSTRAINT TRIGGER %s
AFTER UPDATE ON api_keys
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
WHEN (NEW.id = %d)
EXECUTE FUNCTION %s()`, triggerName, key.ID, functionName))
	require.NoError(t, err)

	err = f.repo.UpdateWithRoute(f.ctx, key, service.APIKeyUpdateFields{
		Name:        true,
		RouteConfig: true,
	}, service.APIKeyRouteMutation{
		ReplaceTargets:   true,
		TargetGroupIDs:   []int64{f.groups[2]},
		IncrementVersion: true,
	})
	require.Error(t, err)
	require.Equal(t, before.ID, key.ID)
	require.Equal(t, before.Name, key.Name)
	require.Equal(t, before.UpdatedAt, key.UpdatedAt)
	require.Equal(t, before.RouteConfigVersion, key.RouteConfigVersion)
	require.Equal(t, before.FallbackTargets, key.FallbackTargets)
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
	for i, target := range byKey.FallbackTargets {
		require.NotNil(t, target.Group)
		require.Equal(t, i+1, target.Priority)
	}

	listed, _, err := f.repo.ListByUserID(f.ctx, f.userID, pagination.PaginationParams{Page: 1, PageSize: 10}, service.APIKeyListFilters{})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, want, fallbackGroupIDs(&listed[0]))

	byFullKey, err := f.repo.GetByKey(f.ctx, key.Key)
	require.NoError(t, err)
	require.Equal(t, want, fallbackGroupIDs(byFullKey))

	all, err := f.repo.ListAllByUserID(f.ctx, f.userID, service.APIKeyListFilters{})
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, want, fallbackGroupIDs(&all[0]))

	byGroup, _, err := f.repo.ListByGroupID(f.ctx, f.groups[0], pagination.PaginationParams{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.Len(t, byGroup, 1)
	require.Equal(t, want, fallbackGroupIDs(&byGroup[0]))

	searched, err := f.repo.SearchAPIKeys(f.ctx, f.userID, "loads", 10)
	require.NoError(t, err)
	require.Len(t, searched, 1)
	require.Equal(t, want, fallbackGroupIDs(&searched[0]))
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
