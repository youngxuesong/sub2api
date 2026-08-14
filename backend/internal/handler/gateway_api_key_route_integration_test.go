//go:build unit

package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type apiKeyRouteGroupRepo struct {
	*fakeGroupRepo
	groups map[int64]*service.Group
}

func (r *apiKeyRouteGroupRepo) GetByID(_ context.Context, id int64) (*service.Group, error) {
	return r.groups[id], nil
}

func (r *apiKeyRouteGroupRepo) GetByIDLite(ctx context.Context, id int64) (*service.Group, error) {
	return r.GetByID(ctx, id)
}

type apiKeyRouteRecordingUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
}

func (u *apiKeyRouteRecordingUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	return u.response(accountID), nil
}

func (u *apiKeyRouteRecordingUpstream) DoWithTLS(_ *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.response(accountID), nil
}

func (u *apiKeyRouteRecordingUpstream) response(accountID int64) *http.Response {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"x-request-id": []string{"route-fallback-success"},
		},
		Body: io.NopCloser(bytes.NewBufferString("event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_route","type":"message","role":"assistant","content":[],"model":"claude-test","stop_reason":"","usage":{"input_tokens":1}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n")),
	}
}

func (u *apiKeyRouteRecordingUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func TestGatewayHandlerChatCompletionsAPIKeyRouteUsesFallbackAfterPrimaryCapacity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	primaryID := int64(6101)
	fallbackID := int64(6102)
	primary := &service.Group{ID: primaryID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	fallback := &service.Group{ID: fallbackID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := &service.Account{
		ID: 6202, Name: "fallback-account", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Credentials:   map[string]any{"api_key": "upstream-test-key"},
		AccountGroups: []service.AccountGroup{{AccountID: 6202, GroupID: fallbackID}},
	}

	scheduler := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
	upstream := &apiKeyRouteRecordingUpstream{}
	groupRepo := &apiKeyRouteGroupRepo{
		fakeGroupRepo: &fakeGroupRepo{group: primary},
		groups:        map[int64]*service.Group{primaryID: primary, fallbackID: fallback},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gateway := service.NewGatewayService(
		nil, groupRepo, nil, nil, nil, nil, nil, nil, cfg, scheduler,
		nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := &GatewayHandler{
		gatewayService:      gateway,
		billingCacheService: billing,
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
		routePlanner:        service.NewAPIKeyRoutePlanner(nil, nil, nil),
		maxAccountSwitches:  0,
		cfg:                 cfg,
	}

	user := &service.User{ID: 6301, Balance: 100, Concurrency: 10, Status: service.StatusActive}
	apiKey := &service.APIKey{
		ID: 6401, UserID: user.ID, User: user, GroupID: &primaryID, Group: primary, Status: service.StatusActive,
		RouteConfigVersion: 1,
		FallbackTargets:    []service.APIKeyFailoverTarget{{TargetGroupID: fallbackID, Priority: 1, Group: fallback}},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(
		`{"model":"claude-test","messages":[{"role":"user","content":"hello"}],"stream":false}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: user.ID, Concurrency: user.Concurrency})

	h.ChatCompletions(c)

	require.Equal(t, []int64{account.ID}, upstream.calls(), "the production handler must advance from the empty primary group to the configured fallback group")
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestGatewayHandlerResponsesAPIKeyRouteUsesFallbackAfterPrimaryCapacity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	primaryID := int64(6111)
	fallbackID := int64(6112)
	primary := &service.Group{ID: primaryID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	fallback := &service.Group{ID: fallbackID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := &service.Account{
		ID: 6212, Name: "responses-fallback-account", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Credentials:   map[string]any{"api_key": "upstream-test-key"},
		AccountGroups: []service.AccountGroup{{AccountID: 6212, GroupID: fallbackID}},
	}

	scheduler := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
	upstream := &apiKeyRouteRecordingUpstream{}
	groupRepo := &apiKeyRouteGroupRepo{
		fakeGroupRepo: &fakeGroupRepo{group: primary},
		groups:        map[int64]*service.Group{primaryID: primary, fallbackID: fallback},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gateway := service.NewGatewayService(
		nil, groupRepo, nil, nil, nil, nil, nil, nil, cfg, scheduler,
		nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	h := &GatewayHandler{
		gatewayService:      gateway,
		billingCacheService: billing,
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
		routePlanner:        service.NewAPIKeyRoutePlanner(nil, nil, nil),
		maxAccountSwitches:  0,
		cfg:                 cfg,
	}

	user := &service.User{ID: 6311, Balance: 100, Concurrency: 10, Status: service.StatusActive}
	apiKey := &service.APIKey{
		ID: 6411, UserID: user.ID, User: user, GroupID: &primaryID, Group: primary, Status: service.StatusActive,
		RouteConfigVersion: 1,
		FallbackTargets:    []service.APIKeyFailoverTarget{{TargetGroupID: fallbackID, Priority: 1, Group: fallback}},
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"claude-test","input":"hello","stream":false}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: user.ID, Concurrency: user.Concurrency})

	h.Responses(c)

	require.Equal(t, []int64{account.ID}, upstream.calls(), "the production Responses handler must advance from the empty primary group to the configured fallback group")
	require.Equal(t, http.StatusOK, recorder.Code)
}
