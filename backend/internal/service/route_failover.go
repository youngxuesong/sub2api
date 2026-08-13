package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var ErrRouteFailoverPolicyNotFound = infraerrors.NotFound("ROUTE_FAILOVER_POLICY_NOT_FOUND", "route failover policy not found")

// RouteFailoverPolicy describes an ordered set of fallback groups for one source group.
// The source group remains the billing and authorization group for the request.
type RouteFailoverPolicy struct {
	ID               int64                 `json:"id"`
	SourceGroupID    int64                 `json:"source_group_id"`
	Enabled          bool                  `json:"enabled"`
	MaxAttempts      int                   `json:"max_attempts"`
	FailureThreshold int                   `json:"failure_threshold"`
	SuccessThreshold int                   `json:"success_threshold"`
	Window           time.Duration         `json:"-"`
	OpenCooldown     time.Duration         `json:"-"`
	HalfOpenLease    time.Duration         `json:"-"`
	Targets          []RouteFailoverTarget `json:"targets"`
}

type RouteFailoverTarget struct {
	ID            int64             `json:"id"`
	PolicyID      int64             `json:"policy_id"`
	TargetGroupID int64             `json:"target_group_id"`
	Priority      int               `json:"priority"`
	Enabled       bool              `json:"enabled"`
	ModelMapping  map[string]string `json:"model_mapping"`
}

type RouteFailoverSnapshot struct {
	Version  int64
	Policies []RouteFailoverPolicy
}

type RouteFailoverPermit struct {
	Allowed  bool
	HalfOpen bool
	LeaseID  string
}

type RouteFailoverCandidate struct {
	GroupID         int64
	Model           string
	CircuitModel    string
	TargetID        int64
	IsFallback      bool
	SnapshotVersion int64
	LeaseID         string
}

type RouteFailoverPolicyRepository interface {
	LoadSnapshot(ctx context.Context) (RouteFailoverSnapshot, error)
}

type RouteFailoverConfigRepository interface {
	RouteFailoverPolicyRepository
	GetBySourceGroupID(ctx context.Context, sourceGroupID int64) (*RouteFailoverPolicy, error)
	Save(ctx context.Context, policy *RouteFailoverPolicy) (*RouteFailoverPolicy, error)
	Delete(ctx context.Context, sourceGroupID int64) error
}

type RouteFailoverGroupRepository interface {
	GetByIDLite(ctx context.Context, id int64) (*Group, error)
}

type RouteFailoverCircuit interface {
	Allow(ctx context.Context, effectiveGroupID int64, requestedModel string) (allowed, halfOpen bool, leaseID string, err error)
	RecordSuccess(ctx context.Context, effectiveGroupID int64, requestedModel, leaseID string) error
	RecordFailure(ctx context.Context, effectiveGroupID int64, requestedModel, leaseID string) error
}

type RouteFailureClass string

const (
	RouteFailureCapacity         RouteFailureClass = "capacity"
	RouteFailureConnection       RouteFailureClass = "connection"
	RouteFailureTimeout          RouteFailureClass = "timeout"
	RouteFailureUpstream429      RouteFailureClass = "upstream_429"
	RouteFailureUpstream5xx      RouteFailureClass = "upstream_5xx"
	RouteFailureUnsupportedModel RouteFailureClass = "unsupported_model"
	RouteFailureAdmission        RouteFailureClass = "admission"
	RouteFailureBusiness         RouteFailureClass = "business"
	RouteFailureCanceled         RouteFailureClass = "canceled"
)

type RouteUsageAudit struct {
	SourceGroupID  *int64
	FallbackUsed   bool
	AttemptCount   int
	FallbackReason string
	StickyHit      bool
}

type APIKeyRouteCandidate struct {
	EffectiveAPIKey  *APIKey
	Subscription     *UserSubscription
	SourceGroupID    int64
	EffectiveGroupID int64
	RequestedModel   string
	IsFallback       bool
	StickyHit        bool
	CircuitLeaseID   string
	Priority         int
}

type APIKeyRouteAttemptFailure struct {
	Class             RouteFailureClass
	StatusCode        int
	ReplaySafe        bool
	SemanticCommitted bool
	Cause             error
}

const apiKeyRouteSwitchBudget = 20 * time.Second

type APIKeyRoutePlanner struct {
	sticky        APIKeyRouteStickyStore
	circuit       RouteFailoverCircuit
	subscriptions *SubscriptionService
	now           func() time.Time
}

type apiKeyRoutePlanTarget struct {
	apiKey       *APIKey
	groupID      int64
	priority     int
	isFallback   bool
	stickyHit    bool
	subscription *UserSubscription
}

type APIKeyRoutePlan struct {
	planner        *APIKeyRoutePlanner
	apiKeyID       int64
	routeVersion   int64
	sourceGroupID  int64
	sessionHash    string
	requestedModel string
	targets        []apiKeyRoutePlanTarget
	nextIndex      int
	attempted      map[int64]struct{}
	switchDeadline time.Time
	audit          RouteUsageAudit
}

func NewAPIKeyRoutePlanner(sticky APIKeyRouteStickyStore, circuit RouteFailoverCircuit, subscriptions *SubscriptionService) *APIKeyRoutePlanner {
	return &APIKeyRoutePlanner{sticky: sticky, circuit: circuit, subscriptions: subscriptions, now: time.Now}
}

func (p *APIKeyRoutePlanner) NewPlan(ctx context.Context, apiKey *APIKey, primarySubscription *UserSubscription, sessionHash, requestedModel string) *APIKeyRoutePlan {
	if p == nil {
		p = NewAPIKeyRoutePlanner(nil, nil, nil)
	}
	if p.now == nil {
		p.now = time.Now
	}
	plan := &APIKeyRoutePlan{
		planner: p, sessionHash: sessionHash, requestedModel: requestedModel,
		attempted: make(map[int64]struct{}), audit: RouteUsageAudit{AttemptCount: 0},
	}
	if apiKey == nil || apiKey.GroupID == nil || apiKey.Group == nil || apiKey.User == nil {
		return plan
	}
	plan.apiKeyID = apiKey.ID
	plan.routeVersion = apiKey.RouteConfigVersion
	plan.sourceGroupID = *apiKey.GroupID
	plan.audit.SourceGroupID = routeGroupIDPointer(plan.sourceGroupID)
	primary := cloneEffectiveAPIKey(apiKey)
	plan.targets = append(plan.targets, apiKeyRoutePlanTarget{
		apiKey: primary, groupID: plan.sourceGroupID, subscription: cloneUserSubscription(primarySubscription),
	})

	targets := cloneAndSortAPIKeyTargets(apiKey.FallbackTargets)
	seen := map[int64]struct{}{plan.sourceGroupID: {}}
	for _, target := range targets {
		if target.TargetGroupID <= 0 || target.Group == nil {
			continue
		}
		if _, duplicate := seen[target.TargetGroupID]; duplicate {
			continue
		}
		seen[target.TargetGroupID] = struct{}{}
		plan.targets = append(plan.targets, apiKeyRoutePlanTarget{
			apiKey: effectiveAPIKeyForTarget(apiKey, target), groupID: target.TargetGroupID,
			priority: target.Priority, isFallback: true,
		})
	}

	plan.applySticky(ctx)
	return plan
}

func (p *APIKeyRoutePlan) Next(ctx context.Context) (*APIKeyRouteCandidate, bool) {
	if p == nil || contextDone(ctx) || p.routeSwitchExpired() {
		return nil, false
	}
	for p.nextIndex < len(p.targets) {
		target := &p.targets[p.nextIndex]
		p.nextIndex++
		if _, alreadyAttempted := p.attempted[target.groupID]; alreadyAttempted {
			continue
		}
		if contextDone(ctx) || p.routeSwitchExpired() {
			return nil, false
		}
		subscription := cloneUserSubscription(target.subscription)
		eligible := true
		if target.isFallback {
			subscription, eligible = p.admitTarget(ctx, target)
		}
		if !eligible {
			if target.stickyHit {
				p.deleteSticky(ctx)
				p.rebuildPrimaryFirst()
			}
			continue
		}
		p.attempted[target.groupID] = struct{}{}
		candidate := &APIKeyRouteCandidate{
			EffectiveAPIKey: target.apiKey, Subscription: subscription,
			SourceGroupID: p.sourceGroupID, EffectiveGroupID: target.groupID,
			RequestedModel: p.requestedModel, IsFallback: target.isFallback,
			StickyHit: target.stickyHit, Priority: target.priority,
		}
		if target.isFallback && p.planner.circuit != nil {
			allowed, _, leaseID, err := p.planner.circuit.Allow(ctx, target.groupID, p.requestedModel)
			if err == nil && !allowed {
				if target.stickyHit {
					p.deleteSticky(ctx)
					p.rebuildPrimaryFirst()
				}
				continue
			}
			if err == nil {
				candidate.CircuitLeaseID = leaseID
			}
		}
		p.audit.AttemptCount++
		return candidate, true
	}
	return nil, false
}

func (p *APIKeyRoutePlan) Skip(ctx context.Context, candidate *APIKeyRouteCandidate, class RouteFailureClass) {
	if p == nil || candidate == nil {
		return
	}
	p.captureFallbackReason(class)
	if candidate.StickyHit {
		p.deleteSticky(ctx)
		p.rebuildPrimaryFirst()
	}
}

func (p *APIKeyRoutePlan) Fail(ctx context.Context, candidate *APIKeyRouteCandidate, failure APIKeyRouteAttemptFailure) bool {
	if p == nil || candidate == nil || contextDone(ctx) {
		return false
	}
	if !failure.ReplaySafe || failure.SemanticCommitted || routeFailureIsLocal4xx(failure) || routeFailureIsCanceled(failure) || !routeFailureAllowsFailover(failure.Class) {
		return false
	}
	p.captureFallbackReason(failure.Class)
	if candidate.EffectiveGroupID == p.sourceGroupID && p.switchDeadline.IsZero() {
		p.switchDeadline = p.planner.now().Add(apiKeyRouteSwitchBudget)
	}
	if candidate.StickyHit {
		p.deleteSticky(ctx)
		p.rebuildPrimaryFirst()
	}
	if routeFailureRecordsCircuit(failure.Class) && p.planner.circuit != nil {
		_ = p.planner.circuit.RecordFailure(ctx, candidate.EffectiveGroupID, p.requestedModel, candidate.CircuitLeaseID)
	}
	return !contextDone(ctx) && !p.routeSwitchExpired() && p.hasRemainingCandidate()
}

func (p *APIKeyRoutePlan) Succeed(ctx context.Context, candidate *APIKeyRouteCandidate) {
	if p == nil || candidate == nil || contextDone(ctx) {
		return
	}
	p.audit.FallbackUsed = candidate.IsFallback
	if p.planner.circuit != nil {
		_ = p.planner.circuit.RecordSuccess(ctx, candidate.EffectiveGroupID, p.requestedModel, candidate.CircuitLeaseID)
	}
	if !candidate.IsFallback || p.sessionHash == "" || p.planner.sticky == nil {
		return
	}
	if candidate.StickyHit {
		_ = p.planner.sticky.Refresh(ctx, p.apiKeyID, p.sessionHash, APIKeyRouteStickyTTL)
		return
	}
	_ = p.planner.sticky.Set(ctx, p.apiKeyID, p.sessionHash, APIKeyRouteStickyBinding{
		RouteConfigVersion: p.routeVersion,
		EffectiveGroupID:   candidate.EffectiveGroupID,
	}, APIKeyRouteStickyTTL)
}

func (p *APIKeyRoutePlan) Audit() RouteUsageAudit {
	if p == nil {
		return RouteUsageAudit{}
	}
	return p.audit
}

func (p *APIKeyRoutePlan) applySticky(ctx context.Context) {
	if p == nil || p.sessionHash == "" || p.planner.sticky == nil || contextDone(ctx) {
		return
	}
	binding, err := p.planner.sticky.Get(ctx, p.apiKeyID, p.sessionHash)
	if err != nil || binding == nil {
		return
	}
	if binding.RouteConfigVersion != p.routeVersion || binding.EffectiveGroupID == p.sourceGroupID {
		p.deleteSticky(ctx)
		return
	}
	index := -1
	for i := 1; i < len(p.targets); i++ {
		if p.targets[i].groupID == binding.EffectiveGroupID {
			index = i
			break
		}
	}
	if index < 0 {
		p.deleteSticky(ctx)
		return
	}
	if _, eligible := p.admitTarget(ctx, &p.targets[index]); !eligible {
		p.deleteSticky(ctx)
		return
	}
	stickyTarget := p.targets[index]
	stickyTarget.stickyHit = true
	p.targets = append([]apiKeyRoutePlanTarget{stickyTarget}, append(p.targets[:index], p.targets[index+1:]...)...)
	p.audit.StickyHit = true
}

func (p *APIKeyRoutePlan) admitTarget(ctx context.Context, target *apiKeyRoutePlanTarget) (*UserSubscription, bool) {
	if target == nil || target.apiKey == nil || target.apiKey.Group == nil || target.apiKey.User == nil || contextDone(ctx) {
		return nil, false
	}
	group := target.apiKey.Group
	if !isConcreteRouteGroup(group) || group.Platform != p.targetsPrimaryPlatform() || !groupSupportsRequestedModel(group, p.requestedModel) {
		return nil, false
	}
	if !group.IsSubscriptionType() {
		if !target.apiKey.User.CanBindGroup(group.ID, group.IsExclusive) {
			return nil, false
		}
		return nil, true
	}
	if target.subscription != nil && target.subscription.UserID == target.apiKey.User.ID && target.subscription.GroupID == group.ID {
		return cloneUserSubscription(target.subscription), true
	}
	if p.planner.subscriptions == nil {
		return nil, false
	}
	subscription, err := p.planner.subscriptions.GetActiveSubscription(ctx, target.apiKey.User.ID, group.ID)
	if err != nil || subscription == nil {
		return nil, false
	}
	target.subscription = cloneUserSubscription(subscription)
	return cloneUserSubscription(subscription), true
}

func (p *APIKeyRoutePlan) targetsPrimaryPlatform() string {
	for _, target := range p.targets {
		if target.groupID == p.sourceGroupID && target.apiKey != nil && target.apiKey.Group != nil {
			return target.apiKey.Group.Platform
		}
	}
	return ""
}

func (p *APIKeyRoutePlan) rebuildPrimaryFirst() {
	if p == nil {
		return
	}
	reordered := make([]apiKeyRoutePlanTarget, 0, len(p.targets))
	for _, target := range p.targets {
		if target.groupID == p.sourceGroupID {
			target.stickyHit = false
			reordered = append(reordered, target)
			break
		}
	}
	for _, target := range p.targets {
		if target.groupID != p.sourceGroupID {
			target.stickyHit = false
			reordered = append(reordered, target)
		}
	}
	p.targets = reordered
	p.nextIndex = 0
}

func (p *APIKeyRoutePlan) deleteSticky(ctx context.Context) {
	if p == nil || p.sessionHash == "" || p.planner.sticky == nil {
		return
	}
	_ = p.planner.sticky.Delete(ctx, p.apiKeyID, p.sessionHash)
}

func (p *APIKeyRoutePlan) captureFallbackReason(class RouteFailureClass) {
	if p.audit.FallbackReason == "" && routeFailureAllowsFailover(class) {
		p.audit.FallbackReason = string(class)
	}
}

func (p *APIKeyRoutePlan) routeSwitchExpired() bool {
	return p != nil && !p.switchDeadline.IsZero() && !p.planner.now().Before(p.switchDeadline)
}

func (p *APIKeyRoutePlan) hasRemainingCandidate() bool {
	for i := p.nextIndex; i < len(p.targets); i++ {
		if _, attempted := p.attempted[p.targets[i].groupID]; !attempted {
			return true
		}
	}
	return false
}

func effectiveAPIKeyForTarget(source *APIKey, target APIKeyFailoverTarget) *APIKey {
	copyKey := cloneEffectiveAPIKey(source)
	if copyKey == nil || source.User == nil {
		return copyKey
	}
	copyKey.User.UserGroupRPMOverride = cloneIntPointer(target.UserGroupRPMOverride)
	groupID := target.TargetGroupID
	copyKey.GroupID = &groupID
	copyKey.Group = cloneGroup(target.Group)
	return copyKey
}

func cloneEffectiveAPIKey(source *APIKey) *APIKey {
	if source == nil {
		return nil
	}
	copyKey := *source
	copyKey.GroupID = cloneRouteInt64Pointer(source.GroupID)
	copyKey.User = cloneUser(source.User)
	copyKey.Group = cloneGroup(source.Group)
	copyKey.FallbackTargets = cloneAndSortAPIKeyTargets(source.FallbackTargets)
	return &copyKey
}

func cloneUser(source *User) *User {
	if source == nil {
		return nil
	}
	copyUser := *source
	copyUser.AllowedGroups = append([]int64(nil), source.AllowedGroups...)
	copyUser.UserGroupRPMOverride = cloneIntPointer(source.UserGroupRPMOverride)
	if source.GroupRates != nil {
		copyUser.GroupRates = make(map[int64]float64, len(source.GroupRates))
		for groupID, rate := range source.GroupRates {
			copyUser.GroupRates[groupID] = rate
		}
	}
	return &copyUser
}

func cloneGroup(source *Group) *Group {
	if source == nil {
		return nil
	}
	copyGroup := *source
	copyGroup.ModelsListConfig.Models = append([]string(nil), source.ModelsListConfig.Models...)
	copyGroup.SupportedModelScopes = append([]string(nil), source.SupportedModelScopes...)
	return &copyGroup
}

func cloneAndSortAPIKeyTargets(targets []APIKeyFailoverTarget) []APIKeyFailoverTarget {
	cloned := make([]APIKeyFailoverTarget, len(targets))
	for i, target := range targets {
		cloned[i] = target
		cloned[i].Group = cloneGroup(target.Group)
		cloned[i].UserGroupRPMOverride = cloneIntPointer(target.UserGroupRPMOverride)
	}
	sort.SliceStable(cloned, func(i, j int) bool {
		if cloned[i].Priority != cloned[j].Priority {
			return cloned[i].Priority < cloned[j].Priority
		}
		return cloned[i].ID < cloned[j].ID
	})
	return cloned
}

func cloneUserSubscription(source *UserSubscription) *UserSubscription {
	if source == nil {
		return nil
	}
	copySubscription := *source
	return &copySubscription
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneRouteInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func routeGroupIDPointer(value int64) *int64 {
	copyValue := value
	return &copyValue
}

func groupSupportsRequestedModel(group *Group, requestedModel string) bool {
	if group == nil || requestedModel == "" || !group.CustomModelsListEnabled() {
		return true
	}
	for _, model := range group.ModelsListConfig.Models {
		if strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(requestedModel)) {
			return true
		}
	}
	return false
}

func routeFailureAllowsFailover(class RouteFailureClass) bool {
	switch class {
	case RouteFailureCapacity, RouteFailureConnection, RouteFailureTimeout, RouteFailureUpstream429, RouteFailureUpstream5xx:
		return true
	default:
		return false
	}
}

func routeFailureRecordsCircuit(class RouteFailureClass) bool {
	switch class {
	case RouteFailureConnection, RouteFailureTimeout, RouteFailureUpstream429, RouteFailureUpstream5xx:
		return true
	default:
		return false
	}
}

func routeFailureIsLocal4xx(failure APIKeyRouteAttemptFailure) bool {
	return failure.StatusCode >= 400 && failure.StatusCode < 500 && failure.Class != RouteFailureUpstream429
}

func routeFailureIsCanceled(failure APIKeyRouteAttemptFailure) bool {
	return errors.Is(failure.Cause, context.Canceled)
}

func contextDone(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

type routeFailoverSnapshotState struct {
	version  int64
	policies map[int64]RouteFailoverPolicy
}

// RouteFailoverPlanner keeps the last valid database snapshot in process memory.
// Refresh failures never replace a usable snapshot.
type RouteFailoverPlanner struct {
	policyRepo RouteFailoverPolicyRepository
	groupRepo  RouteFailoverGroupRepository
	circuit    RouteFailoverCircuit
	refreshTTL time.Duration

	mu         sync.RWMutex
	snapshot   routeFailoverSnapshotState
	loadedAt   time.Time
	reloadMu   sync.Mutex
	refreshing atomic.Bool
}

func NewRouteFailoverPlanner(policyRepo RouteFailoverPolicyRepository, groupRepo RouteFailoverGroupRepository, circuit RouteFailoverCircuit, refreshTTL time.Duration) *RouteFailoverPlanner {
	return &RouteFailoverPlanner{
		policyRepo: policyRepo,
		groupRepo:  groupRepo,
		circuit:    circuit,
		refreshTTL: refreshTTL,
		snapshot:   routeFailoverSnapshotState{policies: make(map[int64]RouteFailoverPolicy)},
	}
}

func (p *RouteFailoverPlanner) Reload(ctx context.Context) error {
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()

	snapshot, err := p.policyRepo.LoadSnapshot(ctx)
	if err != nil {
		return err
	}
	state := routeFailoverSnapshotState{
		version:  snapshot.Version,
		policies: make(map[int64]RouteFailoverPolicy, len(snapshot.Policies)),
	}
	for _, policy := range snapshot.Policies {
		policy.Targets = cloneAndSortRouteTargets(policy.Targets)
		state.policies[policy.SourceGroupID] = policy
	}

	p.mu.Lock()
	p.snapshot = state
	p.loadedAt = time.Now()
	p.mu.Unlock()
	return nil
}

func (p *RouteFailoverPlanner) Candidates(ctx context.Context, sourceGroupID int64, model string) ([]RouteFailoverCandidate, error) {
	p.refreshIfStale(ctx)

	p.mu.RLock()
	policy, ok := p.snapshot.policies[sourceGroupID]
	version := p.snapshot.version
	p.mu.RUnlock()

	candidates := []RouteFailoverCandidate{{GroupID: sourceGroupID, Model: model, SnapshotVersion: version}}
	if !ok || !policy.Enabled {
		return candidates, nil
	}
	maxCandidates := policy.MaxAttempts
	if maxCandidates <= 0 || maxCandidates > len(policy.Targets)+1 {
		maxCandidates = len(policy.Targets) + 1
	}
	seen := map[int64]struct{}{sourceGroupID: {}}
	for _, target := range policy.Targets {
		if len(candidates) >= maxCandidates {
			break
		}
		if !target.Enabled || target.TargetGroupID <= 0 {
			continue
		}
		if _, duplicate := seen[target.TargetGroupID]; duplicate {
			continue
		}
		seen[target.TargetGroupID] = struct{}{}

		mappedModel := model
		if mapped, exists := target.ModelMapping[model]; exists && mapped != "" {
			mappedModel = mapped
		}
		candidates = append(candidates, RouteFailoverCandidate{
			GroupID: target.TargetGroupID, Model: mappedModel, CircuitModel: model, TargetID: target.ID,
			IsFallback: true, SnapshotVersion: version,
		})
	}
	return candidates, nil
}

// Admit checks circuit state immediately before a fallback candidate is attempted.
// Candidate planning remains side-effect free so unused half-open leases are never acquired.
func (p *RouteFailoverPlanner) Admit(ctx context.Context, sourceGroupID int64, candidate RouteFailoverCandidate) (RouteFailoverCandidate, bool) {
	if !candidate.IsFallback || candidate.TargetID == 0 {
		return candidate, true
	}
	p.mu.RLock()
	policy, exists := p.snapshot.policies[sourceGroupID]
	version := p.snapshot.version
	p.mu.RUnlock()
	if !exists || !policy.Enabled || version != candidate.SnapshotVersion {
		return candidate, false
	}
	for _, target := range policy.Targets {
		if target.ID != candidate.TargetID || !target.Enabled {
			continue
		}
		if p.circuit == nil {
			return candidate, true
		}
		allowed, _, leaseID, err := p.circuit.Allow(ctx, target.TargetGroupID, candidate.CircuitModel)
		if err != nil {
			return candidate, true
		}
		if !allowed {
			return candidate, false
		}
		candidate.LeaseID = leaseID
		return candidate, true
	}
	return candidate, false
}

func (p *RouteFailoverPlanner) RecordFailure(ctx context.Context, sourceGroupID int64, candidate RouteFailoverCandidate) {
	p.record(ctx, sourceGroupID, candidate, false)
}

func (p *RouteFailoverPlanner) RecordSuccess(ctx context.Context, sourceGroupID int64, candidate RouteFailoverCandidate) {
	p.record(ctx, sourceGroupID, candidate, true)
}

func (p *RouteFailoverPlanner) record(ctx context.Context, sourceGroupID int64, candidate RouteFailoverCandidate, success bool) {
	if !candidate.IsFallback || candidate.TargetID == 0 {
		return
	}
	if p.circuit == nil {
		return
	}
	p.mu.RLock()
	policy, exists := p.snapshot.policies[sourceGroupID]
	p.mu.RUnlock()
	if !exists {
		return
	}
	for _, target := range policy.Targets {
		if target.ID != candidate.TargetID {
			continue
		}
		if success {
			_ = p.circuit.RecordSuccess(ctx, target.TargetGroupID, candidate.CircuitModel, candidate.LeaseID)
		} else {
			_ = p.circuit.RecordFailure(ctx, target.TargetGroupID, candidate.CircuitModel, candidate.LeaseID)
		}
		return
	}
}

func (p *RouteFailoverPlanner) refreshIfStale(_ context.Context) {
	if p.policyRepo == nil {
		return
	}
	p.mu.RLock()
	stale := p.loadedAt.IsZero() || (p.refreshTTL > 0 && time.Since(p.loadedAt) >= p.refreshTTL)
	p.mu.RUnlock()
	if !stale {
		return
	}
	if !p.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer p.refreshing.Store(false)
		// Never block a gateway request on PostgreSQL. The provider performs an
		// eager reload during startup; this path only repairs an empty/stale
		// snapshot in the background after startup or a transient DB outage.
		refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Reload(refreshCtx)
	}()
}

func cloneAndSortRouteTargets(targets []RouteFailoverTarget) []RouteFailoverTarget {
	cloned := make([]RouteFailoverTarget, len(targets))
	for i := range targets {
		cloned[i] = targets[i]
		if targets[i].ModelMapping != nil {
			cloned[i].ModelMapping = make(map[string]string, len(targets[i].ModelMapping))
			for source, target := range targets[i].ModelMapping {
				cloned[i].ModelMapping[source] = target
			}
		}
	}
	sort.SliceStable(cloned, func(i, j int) bool {
		if cloned[i].Priority != cloned[j].Priority {
			return cloned[i].Priority < cloned[j].Priority
		}
		return cloned[i].ID < cloned[j].ID
	})
	return cloned
}
