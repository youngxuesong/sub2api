package service

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestRouteFailoverBillingUsesEffectiveSubscriptionGroup(t *testing.T) {
	primaryGroupID := int64(41)
	fallbackGroupID := int64(42)
	primaryAPIKey := &APIKey{
		ID:      501,
		GroupID: &primaryGroupID,
		Group: &Group{
			ID:               primaryGroupID,
			RateMultiplier:   0.5,
			SubscriptionType: SubscriptionTypeStandard,
		},
	}
	fallbackSubscription := &UserSubscription{ID: 4201, GroupID: fallbackGroupID}
	candidate := &APIKeyRouteCandidate{
		EffectiveAPIKey: &APIKey{
			ID:      primaryAPIKey.ID,
			GroupID: &fallbackGroupID,
			Group: &Group{
				ID:               fallbackGroupID,
				RateMultiplier:   1.5,
				SubscriptionType: SubscriptionTypeSubscription,
			},
		},
		Subscription:     fallbackSubscription,
		SourceGroupID:    primaryGroupID,
		EffectiveGroupID: fallbackGroupID,
		IsFallback:       true,
		Priority:         1,
	}
	usageRepo := &routeBillingUsageLogRepo{insertedResult: true}
	subRepo := &routeBillingSubscriptionRepo{}
	svc := newRouteBillingGatewayService(usageRepo, nil, &routeBillingUserRepo{}, subRepo)

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "route-fallback-billing",
			Usage: ClaudeUsage{
				InputTokens:  1000,
				OutputTokens: 100,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:       candidate.EffectiveAPIKey,
		User:         &User{ID: 601},
		Account:      &Account{ID: 701},
		Subscription: candidate.Subscription,
		RouteAudit: RouteUsageAudit{
			SourceGroupID:  &primaryGroupID,
			FallbackUsed:   true,
			AttemptCount:   2,
			FallbackReason: string(RouteFailureUpstream5xx),
			StickyHit:      true,
		},
	})

	require.NoError(t, err)
	require.Equal(t, 0.5, primaryAPIKey.Group.RateMultiplier)
	require.Equal(t, primaryGroupID, *primaryAPIKey.GroupID)
	require.Equal(t, 1, subRepo.incrementCalls)
	require.NotNil(t, usageRepo.lastLog)
	usage := usageRepo.lastLog
	require.Equal(t, fallbackGroupID, *usage.GroupID)
	require.Equal(t, primaryGroupID, *usage.SourceGroupID)
	require.True(t, usage.RouteFallbackUsed)
	require.Equal(t, 2, usage.RouteAttemptCount)
	require.Equal(t, string(RouteFailureUpstream5xx), *usage.RouteFallbackReason)
	require.True(t, usage.RouteStickyHit)
	require.Equal(t, fallbackSubscription.ID, *usage.SubscriptionID)
	require.Equal(t, 1.5, usage.RateMultiplier)
}

func TestRouteFailoverBillingDefaultsPrimaryAuditForOpenAIAndCyber(t *testing.T) {
	groupID := int64(73)
	usageRepo := &routeBillingUsageLogRepo{insertedResult: true}
	svc := newRouteBillingOpenAIService(usageRepo, nil, &routeBillingUserRepo{}, &routeBillingSubscriptionRepo{})
	apiKey := &APIKey{
		ID:      801,
		User:    &User{ID: 802},
		GroupID: &groupID,
		Group:   &Group{ID: groupID, RateMultiplier: 1, SubscriptionType: SubscriptionTypeStandard},
	}

	svc.RecordCyberPolicyUsageLog(context.Background(), CyberPolicyUsageInput{
		APIKey:    apiKey,
		Account:   &Account{ID: 803},
		RequestID: "route-primary-cyber",
		Model:     "gpt-5.4",
	})

	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, groupID, *usageRepo.lastLog.GroupID)
	require.Equal(t, groupID, *usageRepo.lastLog.SourceGroupID)
	require.False(t, usageRepo.lastLog.RouteFallbackUsed)
	require.Equal(t, 1, usageRepo.lastLog.RouteAttemptCount)
	require.Nil(t, usageRepo.lastLog.RouteFallbackReason)
	require.False(t, usageRepo.lastLog.RouteStickyHit)
}

func TestRouteFailoverBillingOpenAIPersistsEffectiveRouteAudit(t *testing.T) {
	primaryGroupID := int64(81)
	fallbackGroupID := int64(82)
	reason := string(RouteFailureTimeout)
	usageRepo := &routeBillingUsageLogRepo{insertedResult: true}
	svc := newRouteBillingOpenAIService(usageRepo, nil, &routeBillingUserRepo{}, &routeBillingSubscriptionRepo{})

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "route-openai-fallback",
			Model:     "gpt-5.4",
			Usage:     OpenAIUsage{InputTokens: 1000, OutputTokens: 100},
			Duration:  time.Second,
		},
		APIKey: &APIKey{
			ID:      901,
			GroupID: &fallbackGroupID,
			Group:   &Group{ID: fallbackGroupID, RateMultiplier: 1.5, SubscriptionType: SubscriptionTypeStandard},
		},
		User:    &User{ID: 902},
		Account: &Account{ID: 903},
		RouteAudit: RouteUsageAudit{
			SourceGroupID:  &primaryGroupID,
			FallbackUsed:   true,
			AttemptCount:   2,
			FallbackReason: reason,
		},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, fallbackGroupID, *usageRepo.lastLog.GroupID)
	require.Equal(t, primaryGroupID, *usageRepo.lastLog.SourceGroupID)
	require.True(t, usageRepo.lastLog.RouteFallbackUsed)
	require.Equal(t, 2, usageRepo.lastLog.RouteAttemptCount)
	require.Equal(t, reason, *usageRepo.lastLog.RouteFallbackReason)
	require.Equal(t, 1.5, usageRepo.lastLog.RateMultiplier)
}

func TestRouteFailoverBillingDuplicateRequestChargesAndPersistsOnce(t *testing.T) {
	groupID := int64(91)
	usageRepo := &routeBillingDedupUsageRepo{seen: make(map[string]struct{})}
	billingRepo := &routeBillingDedupRepo{seen: make(map[string]struct{})}
	svc := newRouteBillingGatewayService(
		usageRepo,
		billingRepo,
		&routeBillingUserRepo{},
		&routeBillingSubscriptionRepo{},
	)
	input := &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "route-billing-idempotency",
			Model:     "claude-sonnet-4",
			Usage:     ClaudeUsage{InputTokens: 1000, OutputTokens: 100},
			Duration:  time.Second,
		},
		APIKey:  &APIKey{ID: 911, GroupID: &groupID, Group: &Group{ID: groupID, RateMultiplier: 1}},
		User:    &User{ID: 912},
		Account: &Account{ID: 913},
	}

	require.NoError(t, svc.RecordUsage(context.Background(), input))
	require.NoError(t, svc.RecordUsage(context.Background(), input))
	require.Equal(t, 1, billingRepo.applied)
	require.Equal(t, 1, usageRepo.inserted)
}

type routeBillingUsageLogRepo struct {
	UsageLogRepository
	insertedResult bool
	calls          int
	lastLog        *UsageLog
}

func (r *routeBillingUsageLogRepo) Create(_ context.Context, log *UsageLog) (bool, error) {
	r.calls++
	r.lastLog = log
	return r.insertedResult, nil
}

type routeBillingUserRepo struct{ UserRepository }

func (r *routeBillingUserRepo) DeductBalance(context.Context, int64, float64) error { return nil }

type routeBillingSubscriptionRepo struct {
	UserSubscriptionRepository
	incrementCalls int
}

func (r *routeBillingSubscriptionRepo) IncrementUsage(context.Context, int64, float64) error {
	r.incrementCalls++
	return nil
}

func newRouteBillingGatewayService(usageRepo UsageLogRepository, billingRepo UsageBillingRepository, userRepo UserRepository, subRepo UserSubscriptionRepository) *GatewayService {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1.1
	return NewGatewayService(
		nil, nil, usageRepo, billingRepo, userRepo, subRepo, nil, nil, cfg,
		nil, nil, NewBillingService(cfg, nil), nil, &BillingCacheService{}, nil,
		nil, &DeferredService{}, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil,
	)
}

func newRouteBillingOpenAIService(usageRepo UsageLogRepository, billingRepo UsageBillingRepository, userRepo UserRepository, subRepo UserSubscriptionRepository) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1.1
	return NewOpenAIGatewayService(
		nil, usageRepo, billingRepo, userRepo, subRepo, nil, nil, cfg,
		nil, nil, NewBillingService(cfg, nil), nil, &BillingCacheService{}, nil,
		&DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
}

type routeBillingDedupUsageRepo struct {
	UsageLogRepository
	mu       sync.Mutex
	seen     map[string]struct{}
	inserted int
}

func (r *routeBillingDedupUsageRepo) Create(_ context.Context, log *UsageLog) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := log.RequestID + ":" + strconv.FormatInt(log.APIKeyID, 10)
	if _, exists := r.seen[key]; exists {
		return false, nil
	}
	r.seen[key] = struct{}{}
	r.inserted++
	return true, nil
}

type routeBillingDedupRepo struct {
	UsageBillingRepository
	mu      sync.Mutex
	seen    map[string]struct{}
	applied int
}

func (r *routeBillingDedupRepo) Apply(_ context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := cmd.RequestID + ":" + strconv.FormatInt(cmd.APIKeyID, 10)
	if _, exists := r.seen[key]; exists {
		return &UsageBillingApplyResult{Applied: false}, nil
	}
	r.seen[key] = struct{}{}
	r.applied++
	return &UsageBillingApplyResult{Applied: true}, nil
}
