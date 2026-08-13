//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type apiKeyRouteGroupRepoStub struct {
	groupRepoStubForGroupUpdate
	groups       map[int64]*Group
	errorsByID   map[int64]error
	getByIDCalls int
}

func (s *apiKeyRouteGroupRepoStub) GetByID(_ context.Context, id int64) (*Group, error) {
	s.getByIDCalls++
	if err := s.errorsByID[id]; err != nil {
		return nil, err
	}
	group, ok := s.groups[id]
	if !ok {
		return nil, ErrGroupNotFound
	}
	clone := *group
	return &clone, nil
}

type apiKeyRouteRateRepoStub struct {
	userGroupRateRepoStubForGroupRate
	rates         map[int64]float64
	errorsByGroup map[int64]error
	getCalls      int
}

func (s *apiKeyRouteRateRepoStub) GetByUserAndGroup(_ context.Context, _, groupID int64) (*float64, error) {
	s.getCalls++
	if err := s.errorsByGroup[groupID]; err != nil {
		return nil, err
	}
	rate, ok := s.rates[groupID]
	if !ok {
		return nil, nil
	}
	return &rate, nil
}

type apiKeyRouteRepoStub struct {
	*apiKeyRepoStub
	stored               *APIKey
	createCalls          int
	createWithRouteCalls int
	updateCalls          int
	updateWithRouteCalls int
	createdTargetIDs     []int64
	updatedFields        APIKeyUpdateFields
	updatedRoute         APIKeyRouteMutation
}

func newAPIKeyRouteRepoStub(key *APIKey) *apiKeyRouteRepoStub {
	return &apiKeyRouteRepoStub{apiKeyRepoStub: &apiKeyRepoStub{}, stored: cloneAPIKeyRouteTestKey(key)}
}

func cloneAPIKeyRouteTestKey(key *APIKey) *APIKey {
	if key == nil {
		return nil
	}
	clone := *key
	clone.FallbackTargets = append([]APIKeyFailoverTarget(nil), key.FallbackTargets...)
	return &clone
}

func (s *apiKeyRouteRepoStub) Create(_ context.Context, key *APIKey) error {
	s.createCalls++
	if key.ID == 0 {
		key.ID = 100
	}
	if key.RouteConfigVersion == 0 {
		key.RouteConfigVersion = 1
	}
	s.stored = cloneAPIKeyRouteTestKey(key)
	return nil
}

func (s *apiKeyRouteRepoStub) CreateWithRoute(_ context.Context, key *APIKey, targetGroupIDs []int64) error {
	s.createWithRouteCalls++
	s.createdTargetIDs = append([]int64(nil), targetGroupIDs...)
	if key.ID == 0 {
		key.ID = 100
	}
	if key.RouteConfigVersion == 0 {
		key.RouteConfigVersion = 1
	}
	key.FallbackTargets = routeTestTargets(key.ID, targetGroupIDs)
	s.stored = cloneAPIKeyRouteTestKey(key)
	return nil
}

func (s *apiKeyRouteRepoStub) GetByID(_ context.Context, _ int64) (*APIKey, error) {
	if s.stored == nil {
		return nil, ErrAPIKeyNotFound
	}
	return cloneAPIKeyRouteTestKey(s.stored), nil
}

func (s *apiKeyRouteRepoStub) Update(_ context.Context, key *APIKey, fields APIKeyUpdateFields) error {
	s.updateCalls++
	s.updatedFields = fields
	if fields.RequireRouteConfigVersion && s.stored != nil && s.stored.RouteConfigVersion != key.RouteConfigVersion {
		return ErrAPIKeyRouteConfigConflict
	}
	s.stored = cloneAPIKeyRouteTestKey(key)
	return nil
}

func (s *apiKeyRouteRepoStub) UpdateWithRoute(_ context.Context, key *APIKey, fields APIKeyUpdateFields, route APIKeyRouteMutation) error {
	s.updateWithRouteCalls++
	s.updatedFields = fields
	s.updatedRoute = route
	if fields.RequireRouteConfigVersion && s.stored != nil && s.stored.RouteConfigVersion != key.RouteConfigVersion {
		return ErrAPIKeyRouteConfigConflict
	}
	if route.IncrementVersion {
		key.RouteConfigVersion++
	}
	if route.ReplaceTargets {
		key.FallbackTargets = routeTestTargets(key.ID, route.TargetGroupIDs)
	}
	if route.ClearRiskAcknowledgement {
		key.FailoverRiskAcknowledgedAt = nil
	} else if route.RiskAcknowledgedAt != nil {
		key.FailoverRiskAcknowledgedAt = route.RiskAcknowledgedAt
	}
	s.stored = cloneAPIKeyRouteTestKey(key)
	return nil
}

func routeTestTargets(apiKeyID int64, groupIDs []int64) []APIKeyFailoverTarget {
	targets := make([]APIKeyFailoverTarget, 0, len(groupIDs))
	for i, groupID := range groupIDs {
		targets = append(targets, APIKeyFailoverTarget{
			APIKeyID:      apiKeyID,
			TargetGroupID: groupID,
			Priority:      i + 1,
		})
	}
	return targets
}

func newAPIKeyRouteTestService(repo *apiKeyRouteRepoStub, groups map[int64]*Group, user *User) *APIKeyService {
	return &APIKeyService{
		apiKeyRepo:        repo,
		userRepo:          &userRepoStub{user: user},
		groupRepo:         &apiKeyRouteGroupRepoStub{groups: groups},
		userSubRepo:       &userSubRepoStubForGroupUpdate{getActiveSub: &UserSubscription{UserID: user.ID}},
		userGroupRateRepo: &apiKeyRouteRateRepoStub{rates: map[int64]float64{2: 1.75}},
		cfg:               testAPIKeyRouteConfig(),
	}
}

func testAPIKeyRouteConfig() *config.Config {
	return &config.Config{Default: config.DefaultConfig{APIKeyPrefix: "sk-"}}
}

func routeTestGroups() map[int64]*Group {
	return map[int64]*Group{
		1: {ID: 1, Name: "primary", Status: StatusActive, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 1},
		2: {ID: 2, Name: "fallback-1", Status: StatusActive, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 1.25},
		3: {ID: 3, Name: "fallback-2", Status: StatusActive, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 1.5},
		4: {ID: 4, Name: "fallback-3", Status: StatusActive, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 1},
		5: {ID: 5, Name: "fallback-4", Status: StatusActive, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 1},
		6: {ID: 6, Name: "fallback-5", Status: StatusActive, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 1},
		7: {ID: 7, Name: "fallback-6", Status: StatusActive, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 1},
	}
}

func TestAPIKeyRouteValidation(t *testing.T) {
	ctx := context.Background()
	user := &User{ID: 9, Status: StatusActive}
	primary := routeTestGroups()[1]

	t.Run("accepts zero through five ordered targets and user rate override", func(t *testing.T) {
		for count := 0; count <= MaxAPIKeyFallbackGroups; count++ {
			groups := routeTestGroups()
			svc := newAPIKeyRouteTestService(newAPIKeyRouteRepoStub(nil), groups, user)
			ids := []int64{2, 3, 4, 5, 6}[:count]

			targets, err := svc.validateFallbackGroups(ctx, user, primary, ids)
			require.NoError(t, err)
			require.Len(t, targets, count)
			for i := range targets {
				require.Equal(t, ids[i], targets[i].TargetGroupID)
				require.Equal(t, i+1, targets[i].Priority)
			}
			if count > 0 {
				require.Equal(t, 1.75, targets[0].EffectiveRateMultiplier)
			}
		}
	})

	tests := []struct {
		name      string
		ids       []int64
		mutate    func(map[int64]*Group, *User)
		wantError error
	}{
		{name: "rejects sixth target", ids: []int64{2, 3, 4, 5, 6, 7}, wantError: ErrFailoverTooManyTargets},
		{name: "rejects duplicate", ids: []int64{2, 2}, wantError: ErrFailoverDuplicateTarget},
		{name: "rejects primary as target", ids: []int64{1}, wantError: ErrFailoverPrimaryAsTarget},
		{name: "rejects unauthorized exclusive group", ids: []int64{2}, mutate: func(groups map[int64]*Group, _ *User) { groups[2].IsExclusive = true }, wantError: ErrFailoverTargetNotAllowed},
		{name: "rejects inactive group", ids: []int64{2}, mutate: func(groups map[int64]*Group, _ *User) { groups[2].Status = StatusDisabled }, wantError: ErrFailoverTargetIncompatible},
		{name: "rejects composite group", ids: []int64{2}, mutate: func(groups map[int64]*Group, _ *User) { groups[2].Platform = PlatformComposite }, wantError: ErrFailoverTargetIncompatible},
		{name: "rejects cross-platform group", ids: []int64{2}, mutate: func(groups map[int64]*Group, _ *User) { groups[2].Platform = PlatformAnthropic }, wantError: ErrFailoverTargetIncompatible},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := routeTestGroups()
			testUser := *user
			if tt.mutate != nil {
				tt.mutate(groups, &testUser)
			}
			svc := newAPIKeyRouteTestService(newAPIKeyRouteRepoStub(nil), groups, &testUser)
			_, err := svc.validateFallbackGroups(ctx, &testUser, groups[1], tt.ids)
			require.ErrorIs(t, err, tt.wantError)
		})
	}

	t.Run("accepts different subscription type with active subscription", func(t *testing.T) {
		groups := routeTestGroups()
		groups[2].SubscriptionType = SubscriptionTypeSubscription
		svc := newAPIKeyRouteTestService(newAPIKeyRouteRepoStub(nil), groups, user)
		targets, err := svc.validateFallbackGroups(ctx, user, groups[1], []int64{2})
		require.NoError(t, err)
		require.Len(t, targets, 1)
	})

	t.Run("rejects subscription group without active subscription", func(t *testing.T) {
		groups := routeTestGroups()
		groups[2].SubscriptionType = SubscriptionTypeSubscription
		svc := newAPIKeyRouteTestService(newAPIKeyRouteRepoStub(nil), groups, user)
		svc.userSubRepo = &userSubRepoStubForGroupUpdate{getActiveErr: ErrSubscriptionNotFound}
		_, err := svc.validateFallbackGroups(ctx, user, groups[1], []int64{2})
		require.ErrorIs(t, err, ErrFailoverTargetNotAllowed)
	})

	t.Run("propagates group repository infrastructure error", func(t *testing.T) {
		infraErr := errors.New("group database unavailable")
		groups := routeTestGroups()
		svc := newAPIKeyRouteTestService(newAPIKeyRouteRepoStub(nil), groups, user)
		svc.groupRepo = &apiKeyRouteGroupRepoStub{
			groups:     groups,
			errorsByID: map[int64]error{2: infraErr},
		}

		_, err := svc.validateFallbackGroups(ctx, user, groups[1], []int64{2})
		require.ErrorIs(t, err, infraErr)
		require.NotErrorIs(t, err, ErrFailoverTargetNotAllowed)
	})

	t.Run("propagates subscription repository infrastructure error", func(t *testing.T) {
		infraErr := errors.New("subscription database unavailable")
		groups := routeTestGroups()
		groups[2].SubscriptionType = SubscriptionTypeSubscription
		svc := newAPIKeyRouteTestService(newAPIKeyRouteRepoStub(nil), groups, user)
		svc.userSubRepo = &userSubRepoStubForGroupUpdate{getActiveErr: infraErr}

		_, err := svc.validateFallbackGroups(ctx, user, groups[1], []int64{2})
		require.ErrorIs(t, err, infraErr)
		require.NotErrorIs(t, err, ErrFailoverTargetNotAllowed)
	})

	t.Run("checks duplicate IDs before primary compatibility", func(t *testing.T) {
		svc := newAPIKeyRouteTestService(newAPIKeyRouteRepoStub(nil), routeTestGroups(), user)
		_, err := svc.validateFallbackGroups(ctx, user, nil, []int64{2, 2})
		require.ErrorIs(t, err, ErrFailoverDuplicateTarget)
	})
}

func TestAPIKeyRouteCreateValidatesStructureBeforePrimaryCompatibility(t *testing.T) {
	user := &User{ID: 9, Status: StatusActive}

	tests := []struct {
		name      string
		ids       []int64
		wantError error
	}{
		{name: "too many targets", ids: []int64{2, 3, 4, 5, 6, 7}, wantError: ErrFailoverTooManyTargets},
		{name: "duplicate target", ids: []int64{2, 2}, wantError: ErrFailoverDuplicateTarget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newAPIKeyRouteRepoStub(nil)
			svc := newAPIKeyRouteTestService(repo, routeTestGroups(), user)

			_, err := svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
				Name:                     "route key",
				FallbackGroupIDs:         tt.ids,
				FailoverRiskAcknowledged: true,
			})
			require.ErrorIs(t, err, tt.wantError)
			require.Zero(t, repo.createWithRouteCalls)
		})
	}
}

func TestAPIKeyRouteCreateRequiresAckForNonEmptyChain(t *testing.T) {
	groups := routeTestGroups()
	user := &User{ID: 9, Status: StatusActive}
	repo := newAPIKeyRouteRepoStub(nil)
	svc := newAPIKeyRouteTestService(repo, groups, user)
	primaryID := int64(1)

	_, err := svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name:             "route key",
		GroupID:          &primaryID,
		FallbackGroupIDs: []int64{2},
	})
	require.ErrorIs(t, err, ErrFailoverAckRequired)
	require.Zero(t, repo.createWithRouteCalls)

	created, err := svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name:                     "route key",
		GroupID:                  &primaryID,
		FallbackGroupIDs:         []int64{2, 3},
		FailoverRiskAcknowledged: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, repo.createWithRouteCalls)
	require.Equal(t, []int64{2, 3}, repo.createdTargetIDs)
	require.NotNil(t, created.FailoverRiskAcknowledgedAt)
	require.Equal(t, []int64{2, 3}, routeTestTargetIDs(created))
	require.Equal(t, groups[2], created.FallbackTargets[0].Group)
	require.Equal(t, 1, created.FallbackTargets[0].Priority)
	require.Equal(t, 1.75, created.FallbackTargets[0].EffectiveRateMultiplier)
	require.Equal(t, groups[3], created.FallbackTargets[1].Group)
	require.Equal(t, 2, created.FallbackTargets[1].Priority)
	require.Equal(t, 1.5, created.FallbackTargets[1].EffectiveRateMultiplier)
	require.Equal(t, 5, svc.groupRepo.(*apiKeyRouteGroupRepoStub).getByIDCalls)
	require.Equal(t, 3, svc.userGroupRateRepo.(*apiKeyRouteRateRepoStub).getCalls)
}

func TestAPIKeyRouteCreateRateFailureDoesNotCallRouteRepository(t *testing.T) {
	infraErr := errors.New("rate database unavailable")
	groups := routeTestGroups()
	user := &User{ID: 9, Status: StatusActive}
	repo := newAPIKeyRouteRepoStub(nil)
	svc := newAPIKeyRouteTestService(repo, groups, user)
	svc.userGroupRateRepo = &apiKeyRouteRateRepoStub{errorsByGroup: map[int64]error{2: infraErr}}
	primaryID := int64(1)

	_, err := svc.Create(context.Background(), user.ID, CreateAPIKeyRequest{
		Name:                     "route key",
		GroupID:                  &primaryID,
		FallbackGroupIDs:         []int64{2},
		FailoverRiskAcknowledged: true,
	})
	require.ErrorIs(t, err, infraErr)
	require.Zero(t, repo.createWithRouteCalls)
}

func TestAPIKeyRouteUpdateOmittedChainIsUnchanged(t *testing.T) {
	key := routeTestExistingKey()
	repo := newAPIKeyRouteRepoStub(key)
	svc := newAPIKeyRouteTestService(repo, routeTestGroups(), &User{ID: key.UserID, Status: StatusActive})
	name := "renamed"

	updated, err := svc.Update(context.Background(), key.ID, key.UserID, UpdateAPIKeyRequest{Name: &name})
	require.NoError(t, err)
	require.Zero(t, repo.updateWithRouteCalls)
	require.True(t, repo.updatedFields.RequireRouteConfigVersion)
	require.Equal(t, int64(1), updated.RouteConfigVersion)
	require.Equal(t, []int64{2, 3}, routeTestTargetIDs(updated))
	require.Equal(t, routeTestGroups()[2], updated.FallbackTargets[0].Group)
	require.Equal(t, 1, updated.FallbackTargets[0].Priority)
	require.Equal(t, 1.75, updated.FallbackTargets[0].EffectiveRateMultiplier)
	require.Equal(t, routeTestGroups()[3], updated.FallbackTargets[1].Group)
	require.Equal(t, 2, updated.FallbackTargets[1].Priority)
	require.Equal(t, 1.5, updated.FallbackTargets[1].EffectiveRateMultiplier)
}

func TestAPIKeyRouteUpdateOmittedChainRateFailureDoesNotUpdate(t *testing.T) {
	infraErr := errors.New("rate database unavailable")
	key := routeTestExistingKey()
	repo := newAPIKeyRouteRepoStub(key)
	svc := newAPIKeyRouteTestService(repo, routeTestGroups(), &User{ID: key.UserID, Status: StatusActive})
	svc.userGroupRateRepo = &apiKeyRouteRateRepoStub{errorsByGroup: map[int64]error{2: infraErr}}
	name := "renamed"

	_, err := svc.Update(context.Background(), key.ID, key.UserID, UpdateAPIKeyRequest{Name: &name})
	require.ErrorIs(t, err, infraErr)
	require.Zero(t, repo.updateCalls)
	require.Zero(t, repo.updateWithRouteCalls)
}

func TestAPIKeyRouteUpdateEmptyChainDisablesWithoutAck(t *testing.T) {
	key := routeTestExistingKey()
	repo := newAPIKeyRouteRepoStub(key)
	svc := newAPIKeyRouteTestService(repo, routeTestGroups(), &User{ID: key.UserID, Status: StatusActive})
	empty := []int64{}

	updated, err := svc.Update(context.Background(), key.ID, key.UserID, UpdateAPIKeyRequest{FallbackGroupIDs: &empty})
	require.NoError(t, err)
	require.Equal(t, 1, repo.updateWithRouteCalls)
	require.True(t, repo.updatedRoute.ReplaceTargets)
	require.True(t, repo.updatedRoute.ClearRiskAcknowledgement)
	require.Equal(t, int64(2), updated.RouteConfigVersion)
	require.Empty(t, updated.FallbackTargets)
	require.Nil(t, updated.FailoverRiskAcknowledgedAt)
}

func TestAPIKeyRouteUpdatePrimaryChangeClearsOmittedChain(t *testing.T) {
	key := routeTestExistingKey()
	repo := newAPIKeyRouteRepoStub(key)
	groups := routeTestGroups()
	svc := newAPIKeyRouteTestService(repo, groups, &User{ID: key.UserID, Status: StatusActive})
	newPrimary := int64(4)

	updated, err := svc.Update(context.Background(), key.ID, key.UserID, UpdateAPIKeyRequest{GroupID: &newPrimary})
	require.NoError(t, err)
	require.Equal(t, 1, repo.updateWithRouteCalls)
	require.True(t, repo.updatedRoute.ReplaceTargets)
	require.Empty(t, repo.updatedRoute.TargetGroupIDs)
	require.Equal(t, int64(2), updated.RouteConfigVersion)
	require.Empty(t, updated.FallbackTargets)
}

func TestAPIKeyRouteUpdateUnchangedNonEmptyChainDoesNotRequireNewAck(t *testing.T) {
	key := routeTestExistingKey()
	repo := newAPIKeyRouteRepoStub(key)
	svc := newAPIKeyRouteTestService(repo, routeTestGroups(), &User{ID: key.UserID, Status: StatusActive})
	unchanged := []int64{2, 3}

	updated, err := svc.Update(context.Background(), key.ID, key.UserID, UpdateAPIKeyRequest{FallbackGroupIDs: &unchanged})
	require.NoError(t, err)
	require.Equal(t, 1, repo.updateCalls)
	require.Zero(t, repo.updateWithRouteCalls)
	require.True(t, repo.updatedFields.RequireRouteConfigVersion)
	require.Equal(t, int64(1), updated.RouteConfigVersion)
	require.Equal(t, key.FailoverRiskAcknowledgedAt, updated.FailoverRiskAcknowledgedAt)
	require.Equal(t, routeTestGroups()[2], updated.FallbackTargets[0].Group)
	require.Equal(t, 1.75, updated.FallbackTargets[0].EffectiveRateMultiplier)
	require.Equal(t, routeTestGroups()[3], updated.FallbackTargets[1].Group)
	require.Equal(t, 1.5, updated.FallbackTargets[1].EffectiveRateMultiplier)
}

func TestAPIKeyRouteUpdateChangedNonEmptyChainRequiresAckAndIncrementsVersion(t *testing.T) {
	key := routeTestExistingKey()
	groups := routeTestGroups()
	changed := []int64{3, 2}

	repo := newAPIKeyRouteRepoStub(key)
	svc := newAPIKeyRouteTestService(repo, groups, &User{ID: key.UserID, Status: StatusActive})
	_, err := svc.Update(context.Background(), key.ID, key.UserID, UpdateAPIKeyRequest{FallbackGroupIDs: &changed})
	require.ErrorIs(t, err, ErrFailoverAckRequired)
	require.Zero(t, repo.updateWithRouteCalls)

	repo = newAPIKeyRouteRepoStub(key)
	svc = newAPIKeyRouteTestService(repo, groups, &User{ID: key.UserID, Status: StatusActive})
	updated, err := svc.Update(context.Background(), key.ID, key.UserID, UpdateAPIKeyRequest{
		FallbackGroupIDs:         &changed,
		FailoverRiskAcknowledged: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, repo.updateWithRouteCalls)
	require.True(t, repo.updatedRoute.IncrementVersion)
	require.Equal(t, int64(2), updated.RouteConfigVersion)
	require.Equal(t, changed, routeTestTargetIDs(updated))
	require.NotNil(t, updated.FailoverRiskAcknowledgedAt)
	require.Equal(t, groups[3], updated.FallbackTargets[0].Group)
	require.Equal(t, 1, updated.FallbackTargets[0].Priority)
	require.Equal(t, 1.5, updated.FallbackTargets[0].EffectiveRateMultiplier)
	require.Equal(t, groups[2], updated.FallbackTargets[1].Group)
	require.Equal(t, 2, updated.FallbackTargets[1].Priority)
	require.Equal(t, 1.75, updated.FallbackTargets[1].EffectiveRateMultiplier)
	require.Equal(t, 3, svc.groupRepo.(*apiKeyRouteGroupRepoStub).getByIDCalls)
	require.Equal(t, 2, svc.userGroupRateRepo.(*apiKeyRouteRateRepoStub).getCalls)
}

func TestAPIKeyRouteUpdateRateFailureDoesNotCallRouteRepository(t *testing.T) {
	infraErr := errors.New("rate database unavailable")
	key := routeTestExistingKey()
	repo := newAPIKeyRouteRepoStub(key)
	svc := newAPIKeyRouteTestService(repo, routeTestGroups(), &User{ID: key.UserID, Status: StatusActive})
	svc.userGroupRateRepo = &apiKeyRouteRateRepoStub{errorsByGroup: map[int64]error{3: infraErr}}
	changed := []int64{3, 2}

	_, err := svc.Update(context.Background(), key.ID, key.UserID, UpdateAPIKeyRequest{
		FallbackGroupIDs:         &changed,
		FailoverRiskAcknowledged: true,
	})
	require.ErrorIs(t, err, infraErr)
	require.Zero(t, repo.updateWithRouteCalls)
}

func TestProvideAPIKeyServiceRequiresRouteRepository(t *testing.T) {
	require.Panics(t, func() {
		ProvideAPIKeyService(&apiKeyRepoStub{}, nil, nil, nil, nil, nil, nil, nil, nil)
	})
	require.NotPanics(t, func() {
		ProvideAPIKeyService(newAPIKeyRouteRepoStub(nil), nil, nil, nil, nil, nil, nil, nil, nil)
	})
}

func routeTestExistingKey() *APIKey {
	primaryID := int64(1)
	acknowledgedAt := time.Now().Add(-time.Hour)
	return &APIKey{
		ID:                         10,
		UserID:                     9,
		Key:                        "sk-route-existing",
		Name:                       "existing",
		GroupID:                    &primaryID,
		Status:                     StatusActive,
		RouteConfigVersion:         1,
		FailoverRiskAcknowledgedAt: &acknowledgedAt,
		FallbackTargets:            routeTestTargets(10, []int64{2, 3}),
	}
}

func routeTestTargetIDs(key *APIKey) []int64 {
	ids := make([]int64, 0, len(key.FallbackTargets))
	for _, target := range key.FallbackTargets {
		ids = append(ids, target.TargetGroupID)
	}
	return ids
}
