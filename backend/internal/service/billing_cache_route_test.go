package service

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type routeBillingCacheStub struct{ BillingCache }

func (routeBillingCacheStub) GetUserBalance(context.Context, int64) (float64, error) {
	return 100, nil
}

type routeRPMCacheStub struct {
	groupCalls int32
	userCalls  int32
}

func (s *routeRPMCacheStub) IncrementUserGroupRPM(context.Context, int64, int64) (int, error) {
	return int(atomic.AddInt32(&s.groupCalls, 1)), nil
}

func (s *routeRPMCacheStub) IncrementUserRPM(context.Context, int64) (int, error) {
	return int(atomic.AddInt32(&s.userCalls, 1)), nil
}

func (s *routeRPMCacheStub) GetUserGroupRPM(context.Context, int64, int64) (int, error) {
	return 0, nil
}
func (s *routeRPMCacheStub) GetUserRPM(context.Context, int64) (int, error) { return 0, nil }

func TestBillingCacheRouteFallbackCountsGroupRPMWithoutCountingUserRPMAgain(t *testing.T) {
	rpm := &routeRPMCacheStub{}
	svc := NewBillingCacheService(routeBillingCacheStub{}, nil, nil, nil, rpm, nil, &config.Config{}, nil)
	t.Cleanup(svc.Stop)

	user := &User{ID: 20, RPMLimit: 100}
	primary := &Group{ID: 1, SubscriptionType: SubscriptionTypeStandard, RPMLimit: 100}
	fallback := &Group{ID: 2, SubscriptionType: SubscriptionTypeStandard, RPMLimit: 100}
	primaryID := primary.ID
	apiKey := &APIKey{ID: 10, GroupID: &primaryID, User: user, Group: primary}

	require.NoError(t, svc.CheckBillingEligibility(context.Background(), user, apiKey, primary, nil, PlatformOpenAI))
	require.NoError(t, svc.CheckRouteCandidateEligibility(
		context.Background(), user, apiKey, fallback, nil, PlatformOpenAI,
		BillingAdmissionOptions{CountUserRPM: false},
	))

	require.EqualValues(t, 2, atomic.LoadInt32(&rpm.groupCalls))
	require.EqualValues(t, 1, atomic.LoadInt32(&rpm.userCalls))
}

func TestBillingCacheRoutePrimaryAdmissionCountsUserRPM(t *testing.T) {
	rpm := &routeRPMCacheStub{}
	svc := NewBillingCacheService(routeBillingCacheStub{}, nil, nil, nil, rpm, nil, &config.Config{}, nil)
	t.Cleanup(svc.Stop)

	user := &User{ID: 20, RPMLimit: 100}
	group := &Group{ID: 1, SubscriptionType: SubscriptionTypeStandard, RPMLimit: 100}
	apiKey := &APIKey{ID: 10, User: user, Group: group}

	require.NoError(t, svc.CheckRouteCandidateEligibility(
		context.Background(), user, apiKey, group, nil, PlatformOpenAI,
		BillingAdmissionOptions{CountUserRPM: true},
	))
	require.EqualValues(t, 1, atomic.LoadInt32(&rpm.groupCalls))
	require.EqualValues(t, 1, atomic.LoadInt32(&rpm.userCalls))
}
