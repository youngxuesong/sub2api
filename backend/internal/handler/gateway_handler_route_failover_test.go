package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestEffectiveSelectionGroupIDUsesFallbackGroup(t *testing.T) {
	sourceGroupID := int64(1)
	selection := &service.AccountSelectionResult{EffectiveGroupID: 2, RouteTargetID: 7}

	groupID := effectiveSelectionGroupID(selection, &sourceGroupID)

	require.NotNil(t, groupID)
	require.Equal(t, int64(2), *groupID)
}

func TestRouteFailoverForwardBodyUsesMappedModel(t *testing.T) {
	body := []byte(`{"model":"gemini-source","contents":[]}`)
	selection := &service.AccountSelectionResult{EffectiveModel: "gemini-fallback", RouteTargetID: 7}

	effectiveModel, forwardBody := routeFailoverForwardBody(&service.GatewayService{}, selection, "gemini-source", body)

	require.Equal(t, "gemini-fallback", effectiveModel)
	require.JSONEq(t, `{"model":"gemini-fallback","contents":[]}`, string(forwardBody))
}

func TestRouteFailoverForwardBodyOverridesSourceChannelMapping(t *testing.T) {
	body := []byte(`{"model":"source-channel-model","messages":[]}`)
	selection := &service.AccountSelectionResult{EffectiveModel: "target-channel-model", RouteTargetID: 7}

	effectiveModel, forwardBody := routeFailoverForwardBody(&service.GatewayService{}, selection, "public-model", body)

	require.Equal(t, "target-channel-model", effectiveModel)
	require.JSONEq(t, `{"model":"target-channel-model","messages":[]}`, string(forwardBody))
}

func TestGeminiForwardBodyCleansSignatureBeforeModelMapping(t *testing.T) {
	body := []byte(`{"model":"gemini-source","thoughtSignature":"account-bound","contents":[]}`)
	selection := &service.AccountSelectionResult{EffectiveModel: "gemini-fallback", RouteTargetID: 7}

	effectiveModel, forwardBody := prepareGeminiForwardBody(&service.GatewayService{}, selection, "gemini-source", body, true)

	require.Equal(t, "gemini-fallback", effectiveModel)
	require.JSONEq(t, `{"model":"gemini-fallback","thoughtSignature":"skip_thought_signature_validator","contents":[]}`, string(forwardBody))
}
