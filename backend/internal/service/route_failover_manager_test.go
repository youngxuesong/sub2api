package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type routeFailoverConfigRepoStub struct {
	saved *RouteFailoverPolicy
}

func (r *routeFailoverConfigRepoStub) LoadSnapshot(context.Context) (RouteFailoverSnapshot, error) {
	return RouteFailoverSnapshot{}, nil
}
func (r *routeFailoverConfigRepoStub) GetBySourceGroupID(context.Context, int64) (*RouteFailoverPolicy, error) {
	if r.saved == nil {
		return nil, ErrRouteFailoverPolicyNotFound
	}
	return r.saved, nil
}
func (r *routeFailoverConfigRepoStub) Save(_ context.Context, policy *RouteFailoverPolicy) (*RouteFailoverPolicy, error) {
	clone := *policy
	r.saved = &clone
	return &clone, nil
}
func (r *routeFailoverConfigRepoStub) Delete(context.Context, int64) error { return nil }

func TestRouteFailoverManagerRejectsUnsafeTargets(t *testing.T) {
	groups := routeFailoverGroupRepoStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		2: {ID: 2, Platform: PlatformOpenAI, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		3: {ID: 3, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeStandard, Status: StatusActive},
		4: {ID: 4, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		5: {ID: 5, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusDisabled},
	}}
	manager := NewRouteFailoverManager(&routeFailoverConfigRepoStub{}, groups, nil)
	base := RouteFailoverPolicy{
		Enabled: true, MaxAttempts: 2, FailureThreshold: 3, SuccessThreshold: 1,
		Window: time.Minute, OpenCooldown: time.Minute, HalfOpenLease: 10 * time.Second,
	}

	tests := []struct {
		name    string
		targets []RouteFailoverTarget
	}{
		{name: "source group", targets: []RouteFailoverTarget{{TargetGroupID: 1, Enabled: true}}},
		{name: "cross platform", targets: []RouteFailoverTarget{{TargetGroupID: 2, Enabled: true}}},
		{name: "cross subscription type", targets: []RouteFailoverTarget{{TargetGroupID: 3, Enabled: true}}},
		{name: "duplicate", targets: []RouteFailoverTarget{{TargetGroupID: 4, Enabled: true}, {TargetGroupID: 4, Enabled: true}}},
		{name: "inactive", targets: []RouteFailoverTarget{{TargetGroupID: 5, Enabled: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := base
			policy.Targets = tt.targets
			_, err := manager.Save(context.Background(), 1, policy)
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		})
	}
}

func TestRouteFailoverManagerRejectsCompositeSourceAsBadRequest(t *testing.T) {
	groups := routeFailoverGroupRepoStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformComposite, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		2: {ID: 2, Platform: PlatformComposite, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
	}}
	manager := NewRouteFailoverManager(&routeFailoverConfigRepoStub{}, groups, nil)

	_, err := manager.Save(context.Background(), 1, RouteFailoverPolicy{
		Targets: []RouteFailoverTarget{{TargetGroupID: 2, Enabled: true}},
	})

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
}

func TestRouteFailoverManagerAppliesSafeDefaults(t *testing.T) {
	repo := &routeFailoverConfigRepoStub{}
	groups := routeFailoverGroupRepoStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
	}}
	manager := NewRouteFailoverManager(repo, groups, nil)

	saved, err := manager.Save(context.Background(), 1, RouteFailoverPolicy{
		Targets: []RouteFailoverTarget{{TargetGroupID: 2, Enabled: true}},
	})
	require.NoError(t, err)
	require.Equal(t, 3, saved.MaxAttempts)
	require.Equal(t, 5, saved.FailureThreshold)
	require.Equal(t, time.Minute, saved.Window)
	require.False(t, saved.Enabled)
}

func TestRouteFailoverManagerRejectsTargetIDOutsideCurrentPolicy(t *testing.T) {
	repo := &routeFailoverConfigRepoStub{saved: &RouteFailoverPolicy{
		ID: 10, SourceGroupID: 1,
		Targets: []RouteFailoverTarget{{ID: 99, PolicyID: 10, TargetGroupID: 2, Enabled: true}},
	}}
	groups := routeFailoverGroupRepoStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		3: {ID: 3, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
	}}
	manager := NewRouteFailoverManager(repo, groups, nil)

	_, err := manager.Save(context.Background(), 1, RouteFailoverPolicy{
		Targets: []RouteFailoverTarget{{ID: 100, TargetGroupID: 3, Enabled: true}},
	})

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
}

func TestRouteFailoverManagerAllowsExistingTargetIDToChangeGroup(t *testing.T) {
	repo := &routeFailoverConfigRepoStub{saved: &RouteFailoverPolicy{
		ID: 10, SourceGroupID: 1,
		Targets: []RouteFailoverTarget{{ID: 99, PolicyID: 10, TargetGroupID: 2, Enabled: true}},
	}}
	groups := routeFailoverGroupRepoStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		2: {ID: 2, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
		3: {ID: 3, Platform: PlatformAnthropic, SubscriptionType: SubscriptionTypeSubscription, Status: StatusActive},
	}}
	manager := NewRouteFailoverManager(repo, groups, nil)

	saved, err := manager.Save(context.Background(), 1, RouteFailoverPolicy{
		Targets: []RouteFailoverTarget{{ID: 99, TargetGroupID: 3, Enabled: true}},
	})

	require.NoError(t, err)
	require.Equal(t, int64(99), saved.Targets[0].ID)
	require.Equal(t, int64(3), saved.Targets[0].TargetGroupID)
}
