package service

import (
	"context"
	"sort"
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
