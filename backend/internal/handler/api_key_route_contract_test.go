package handler

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestAPIKeyRouteContractRequestBinding(t *testing.T) {
	var omitted UpdateAPIKeyRequest
	if err := json.Unmarshal([]byte(`{}`), &omitted); err != nil || omitted.FallbackGroupIDs != nil {
		t.Fatalf("omitted fallback_group_ids must remain nil: %#v, %v", omitted, err)
	}
	var empty UpdateAPIKeyRequest
	if err := json.Unmarshal([]byte(`{"fallback_group_ids":[]}`), &empty); err != nil || empty.FallbackGroupIDs == nil || len(*empty.FallbackGroupIDs) != 0 {
		t.Fatalf("empty fallback_group_ids must be distinguishable: %#v, %v", empty, err)
	}
}

func TestAPIKeyRouteContractResponse(t *testing.T) {
	key := dto.APIKeyFromService(&service.APIKey{RouteConfigVersion: 2, FallbackTargets: []service.APIKeyFailoverTarget{{Priority: 1, EffectiveRateMultiplier: 1.5, Group: &service.Group{ID: 12, Name: "Stable OpenAI", Platform: service.PlatformOpenAI, SubscriptionType: service.SubscriptionTypeStandard}}}})
	if !key.FailoverEnabled || key.RouteConfigVersion != 2 || len(key.FallbackGroups) != 1 {
		t.Fatalf("missing fallback response fields: %#v", key)
	}
	group := key.FallbackGroups[0]
	if group.ID != 12 || group.RateMultiplier != 1.5 || group.Priority != 1 {
		t.Fatalf("unexpected fallback group projection: %#v", group)
	}
}
