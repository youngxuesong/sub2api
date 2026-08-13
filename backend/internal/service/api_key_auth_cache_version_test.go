package service

import (
	"context"
	"testing"
)

type authSnapshotRouteRPMRepo struct {
	UserGroupRateRepository
	overrides map[int64]*int
	calls     []int64
}

func (r *authSnapshotRouteRPMRepo) GetRPMOverrideByUserAndGroup(_ context.Context, _ int64, groupID int64) (*int, error) {
	r.calls = append(r.calls, groupID)
	return r.overrides[groupID], nil
}

func TestAPIKeyAuthSnapshotRoundTripsRouteTargets(t *testing.T) {
	primaryID, firstID, secondID := int64(1), int64(2), int64(3)
	firstRPM, secondRPM := 17, 29
	rateRepo := &authSnapshotRouteRPMRepo{overrides: map[int64]*int{firstID: &firstRPM, secondID: &secondRPM}}
	apiKey := &APIKey{
		ID: 10, UserID: 20, Key: "route-key", GroupID: &primaryID, RouteConfigVersion: 4,
		User:  &User{ID: 20, Status: StatusActive, Role: RoleUser},
		Group: &Group{ID: primaryID, Name: "primary", Platform: PlatformOpenAI, Status: StatusActive, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 1},
		FallbackTargets: []APIKeyFailoverTarget{
			{TargetGroupID: firstID, Priority: 1, Group: &Group{ID: firstID, Name: "first", Platform: PlatformOpenAI, Status: StatusActive, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 1.5, RPMLimit: 100}},
			{TargetGroupID: secondID, Priority: 2, Group: &Group{ID: secondID, Name: "second", Platform: PlatformOpenAI, Status: StatusActive, SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 2, AllowLive: true}},
		},
	}

	snapshot := (&APIKeyService{userGroupRateRepo: rateRepo}).snapshotFromAPIKey(t.Context(), apiKey)
	if snapshot.Version != apiKeyAuthSnapshotVersion || snapshot.RouteConfigVersion != 4 || len(snapshot.FallbackTargets) != 2 {
		t.Fatalf("unexpected route snapshot: %#v", snapshot)
	}
	restored := (&APIKeyService{}).snapshotToAPIKey(apiKey.Key, snapshot)
	if len(restored.FallbackTargets) != 2 || restored.FallbackTargets[0].TargetGroupID != firstID || restored.FallbackTargets[1].Priority != 2 {
		t.Fatalf("unexpected restored targets: %#v", restored.FallbackTargets)
	}
	if restored.FallbackTargets[0].UserGroupRPMOverride == nil || *restored.FallbackTargets[0].UserGroupRPMOverride != firstRPM || restored.FallbackTargets[1].Group.AllowLive != true {
		t.Fatalf("route target fields did not round-trip: %#v", restored.FallbackTargets)
	}
	if len(rateRepo.calls) != 3 || rateRepo.calls[0] != primaryID || rateRepo.calls[1] != firstID || rateRepo.calls[2] != secondID {
		t.Fatalf("expected primary and ordered target RPM lookups, got %v", rateRepo.calls)
	}
}

func TestAPIKeyServiceRejectsV19AuthSnapshotWithoutRoutes(t *testing.T) {
	apiKey, ok, err := (&APIKeyService{}).applyAuthCacheEntry("legacy-route-key", &APIKeyAuthCacheEntry{Snapshot: &APIKeyAuthSnapshot{Version: 19}})
	if err != nil || ok || apiKey != nil {
		t.Fatalf("expected v19 auth snapshot to be ignored after route snapshots were added: key=%#v ok=%v err=%v", apiKey, ok, err)
	}
}

func TestAPIKeyService_RejectsV10AuthSnapshotWithoutModelsListConfig(t *testing.T) {
	groupID := int64(9)
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-models-list", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  10,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:          2,
				Status:      StatusActive,
				Role:        RoleUser,
				Balance:     10,
				Concurrency: 3,
			},
			Group: &APIKeyAuthGroupSnapshot{
				ID:               groupID,
				Name:             "openai",
				Platform:         PlatformOpenAI,
				Status:           StatusActive,
				SubscriptionType: SubscriptionTypeStandard,
				RateMultiplier:   1,
			},
		},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatalf("expected v10 auth snapshot to be rejected after models_list_config was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}

func TestAPIKeyService_RejectsV15AuthSnapshotWithoutReasoningEffortPolicy(t *testing.T) {
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-reasoning-mappings", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{Version: 15},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatal("expected v15 auth snapshot to be rejected after reasoning effort policy was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}
