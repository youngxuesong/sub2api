# Route Failover V2 Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the disabled-by-default Route Failover V2 policy snapshot, state stores, failure model, and protocol-neutral `RouteExecutor` without changing live handler routing.

**Architecture:** V2 is additive beside the current `RouteFailoverPlanner`. PostgreSQL is read only by a background snapshot loader, Redis owns distributed circuit/sticky state, and a bounded in-process store keeps requests available during Redis failures. Handlers will later supply a `RouteForwarder`; the executor alone owns route/account budgets and cross-route replay decisions.

**Tech Stack:** Go 1.26.5, PostgreSQL, Redis Lua scripts, `database/sql`, `go-sqlmock`, `miniredis`, Wire, Testify.

---

## File Map

- Create `backend/migrations/193_route_failover_v2.sql`: additive V2 policy/candidate schema and compatible V1 data copy.
- Create `backend/migrations/route_failover_v2_migration_test.go`: migration immutability and schema contract tests.
- Create `backend/internal/service/route_executor_types.go`: stable V2 domain types, defaults, and interfaces shared by all later plans.
- Create `backend/internal/service/route_policy_snapshot.go`: last-valid immutable snapshot and asynchronous refresh.
- Create `backend/internal/service/route_failure_classifier.go`: maps existing errors into route failure, replay, and commit semantics.
- Create `backend/internal/service/route_state_resilient.go`: Redis-to-local degradation policy and alert counters.
- Create `backend/internal/service/route_executor.go`: candidate ordering, budgets, leases, attempts, stickiness, and audit result.
- Create focused tests matching each new service file.
- Create `backend/internal/repository/route_policy_repo.go`: V2 snapshot and stable-candidate CRUD.
- Create `backend/internal/repository/route_state_store.go`: atomic Redis circuits, leases, and sticky bindings.
- Create focused repository tests for SQL and Redis behavior.
- Modify `backend/internal/config/config.go`: disabled/shadow/enforce runtime mode and degradation limits.
- Modify `backend/internal/repository/wire.go`, `backend/internal/service/wire.go`, `backend/cmd/server/wire_gen.go`: construct V2 without injecting it into handlers yet.
- Keep `backend/migrations/192_route_failover.sql` byte-for-byte unchanged.
- Keep `backend/internal/service/route_failover.go` and `backend/internal/repository/route_failover_*.go` available for rollback until the text-protocol plan removes V1 from the hot path.

### Task 1: Add the forward-only V2 schema

**Files:**
- Create: `backend/migrations/193_route_failover_v2.sql`
- Create: `backend/migrations/route_failover_v2_migration_test.go`
- Test: `backend/migrations/route_failover_v2_migration_test.go`

- [ ] **Step 1: Write the failing migration contract test**

```go
package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration193AddsStableRoutePoliciesAndCandidates(t *testing.T) {
	content, err := FS.ReadFile("193_route_failover_v2.sql")
	require.NoError(t, err)
	sql := string(content)

	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS route_policies")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS route_candidates")
	require.Contains(t, sql, "CHECK (role IN ('primary', 'fallback'))")
	require.Contains(t, sql, "WHERE role = 'primary'")
	require.Contains(t, sql, "INSERT INTO route_policies")
	require.Contains(t, sql, "FROM route_failover_policies")
	require.Contains(t, sql, "INSERT INTO route_candidates")
	require.Contains(t, sql, "to_regclass('public.route_failover_policies')")
	require.NotContains(t, strings.ToUpper(sql), "DROP TABLE")
	require.NotContains(t, strings.ToUpper(sql), "ALTER TABLE ROUTE_FAILOVER_POLICIES")
}
```

- [ ] **Step 2: Run the test and verify the new migration is missing**

Run: `cd backend; go test ./migrations -run TestMigration193AddsStableRoutePoliciesAndCandidates -count=1`

Expected: FAIL because `193_route_failover_v2.sql` does not exist.

- [ ] **Step 3: Add the additive schema and V1-compatible data copy**

```sql
CREATE TABLE IF NOT EXISTS route_policies (
    id BIGSERIAL PRIMARY KEY,
    source_group_id BIGINT NOT NULL UNIQUE REFERENCES groups(id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    max_route_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_route_attempts BETWEEN 1 AND 10),
    max_account_attempts_per_route INTEGER NOT NULL DEFAULT 2 CHECK (max_account_attempts_per_route BETWEEN 1 AND 10),
    max_total_attempts INTEGER NOT NULL DEFAULT 4 CHECK (max_total_attempts BETWEEN 1 AND 20),
    failover_timeout_ms INTEGER NOT NULL DEFAULT 20000 CHECK (failover_timeout_ms > 0),
    failure_threshold INTEGER NOT NULL DEFAULT 5 CHECK (failure_threshold > 0),
    failure_window_seconds INTEGER NOT NULL DEFAULT 60 CHECK (failure_window_seconds > 0),
    open_cooldown_seconds INTEGER NOT NULL DEFAULT 60 CHECK (open_cooldown_seconds > 0),
    recovery_success_threshold INTEGER NOT NULL DEFAULT 2 CHECK (recovery_success_threshold > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS route_candidates (
    id BIGSERIAL PRIMARY KEY,
    policy_id BIGINT NOT NULL REFERENCES route_policies(id) ON DELETE CASCADE,
    role VARCHAR(16) NOT NULL CHECK (role IN ('primary', 'fallback')),
    target_group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE RESTRICT,
    priority INTEGER NOT NULL DEFAULT 0,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    model_mapping JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (policy_id, target_group_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS route_candidates_one_primary_idx
    ON route_candidates (policy_id) WHERE role = 'primary';
CREATE INDEX IF NOT EXISTS route_candidates_policy_order_idx
    ON route_candidates (policy_id, role, priority, id);
CREATE INDEX IF NOT EXISTS route_candidates_target_group_idx
    ON route_candidates (target_group_id);

DO $$
BEGIN
    IF to_regclass('public.route_failover_policies') IS NOT NULL THEN
        INSERT INTO route_policies (
            source_group_id, enabled, max_route_attempts, max_account_attempts_per_route,
            max_total_attempts, failure_threshold, failure_window_seconds,
            open_cooldown_seconds, recovery_success_threshold, created_at, updated_at
        )
        SELECT source_group_id, enabled, max_attempts, 2, GREATEST(max_attempts, 4),
               failure_threshold, window_seconds, open_cooldown_seconds,
               success_threshold, created_at, updated_at
        FROM route_failover_policies
        ON CONFLICT (source_group_id) DO NOTHING;
    END IF;
END $$;

INSERT INTO route_candidates (policy_id, role, target_group_id, priority, enabled, model_mapping)
SELECT p.id, 'primary', p.source_group_id, 0, TRUE, '{}'::jsonb
FROM route_policies p
ON CONFLICT (policy_id, target_group_id) DO NOTHING;

DO $$
BEGIN
    IF to_regclass('public.route_failover_targets') IS NOT NULL
       AND to_regclass('public.route_failover_policies') IS NOT NULL THEN
        INSERT INTO route_candidates (policy_id, role, target_group_id, priority, enabled, model_mapping, created_at, updated_at)
        SELECT p.id, 'fallback', t.target_group_id, t.priority, t.enabled, t.model_mapping, t.created_at, t.updated_at
        FROM route_failover_targets t
        JOIN route_failover_policies old_p ON old_p.id = t.policy_id
        JOIN route_policies p ON p.source_group_id = old_p.source_group_id
        ON CONFLICT (policy_id, target_group_id) DO NOTHING;
    END IF;
END $$;
```

The guarded copy keeps upgrades compatible when migration 192 was previously applied, while a clean V2 installation also succeeds when the uncommitted V1 migration is absent. Never require V1 tables for creating or loading V2 state.

- [ ] **Step 4: Run migration tests**

Run: `cd backend; go test ./migrations -run 'TestMigration193|TestMigration173' -count=1`

Expected: PASS, proving the new migration is embedded and older migration contracts still pass.

- [ ] **Step 5: Commit the schema**

```bash
git add backend/migrations/193_route_failover_v2.sql backend/migrations/route_failover_v2_migration_test.go
git commit -m "feat(db): add route failover v2 schema"
```

### Task 2: Define one stable V2 domain contract

**Files:**
- Create: `backend/internal/service/route_executor_types.go`
- Create: `backend/internal/service/route_executor_types_test.go`

- [ ] **Step 1: Write failing tests for defaults, canonical health keys, and replay gates**

```go
package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultRoutePolicyUsesApprovedBudgets(t *testing.T) {
	p := DefaultRoutePolicy(17)
	require.Equal(t, int64(17), p.SourceGroupID)
	require.Equal(t, 3, p.MaxRouteAttempts)
	require.Equal(t, 2, p.MaxAccountAttemptsPerRoute)
	require.Equal(t, 4, p.MaxTotalAttempts)
	require.Equal(t, 20*time.Second, p.FailoverTimeout)
	require.Equal(t, time.Hour, p.RouteStickyTTL)
}

func TestRouteHealthKeysUseRequestedModelForMappedCandidates(t *testing.T) {
	c := RouteCandidate{ID: 9, PolicyID: 3, EffectiveModel: "provider-sonnet"}
	keys := NewRouteHealthKeys(3, c, "/v1/messages", "claude-sonnet")
	require.Equal(t, "route:circuit:3:9", keys.Candidate)
	require.Equal(t, "route:capability:3:9:/v1/messages:claude-sonnet", keys.Capability)
}

func TestRouteFailureCanReplayRequiresSafeUncommittedOutcome(t *testing.T) {
	f := RouteFailure{Class: RouteFailureProvider, ReplaySafety: ReplaySafe, CommitState: SemanticUncommitted}
	require.True(t, f.CanReplayAcrossRoute())
	f.CommitState = SemanticCommitted
	require.False(t, f.CanReplayAcrossRoute())
	f.CommitState = SemanticUncommitted
	f.ReplaySafety = ReplayUnknown
	require.False(t, f.CanReplayAcrossRoute())
}
```

- [ ] **Step 2: Run the focused tests and verify undefined V2 types**

Run: `cd backend; go test ./internal/service -run 'TestDefaultRoutePolicy|TestRouteHealthKeys|TestRouteFailureCanReplay' -count=1`

Expected: FAIL with undefined `DefaultRoutePolicy`, `RouteCandidate`, and `RouteFailure`.

- [ ] **Step 3: Add the protocol-neutral types and interfaces**

```go
package service

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type RouteCandidateRole string
const (
	RouteCandidatePrimary RouteCandidateRole = "primary"
	RouteCandidateFallback RouteCandidateRole = "fallback"
)

type RouteFailureClass string
const (
	RouteFailureRequest RouteFailureClass = "request"
	RouteFailureAccount RouteFailureClass = "account"
	RouteFailureCapacity RouteFailureClass = "capacity"
	RouteFailureRoute RouteFailureClass = "route"
	RouteFailureProvider RouteFailureClass = "provider"
	RouteFailureInfrastructure RouteFailureClass = "infrastructure"
	RouteFailureCanceled RouteFailureClass = "canceled"
	RouteFailureUnknown RouteFailureClass = "unknown"
)

type RouteCircuitScope string
const (
	RouteCircuitNone RouteCircuitScope = "none"
	RouteCircuitCandidate RouteCircuitScope = "candidate"
	RouteCircuitCapability RouteCircuitScope = "capability"
)

type ReplaySafety string
const (
	ReplaySafe ReplaySafety = "safe"
	ReplayUnsafe ReplaySafety = "unsafe"
	ReplayUnknown ReplaySafety = "unknown"
)

type SemanticCommitState string
const (
	SemanticUncommitted SemanticCommitState = "uncommitted"
	SemanticCommitted SemanticCommitState = "committed"
	SemanticCommitUnknown SemanticCommitState = "unknown"
)

type UpstreamAcceptance string
const (
	UpstreamNotAccepted UpstreamAcceptance = "not_accepted"
	UpstreamAccepted UpstreamAcceptance = "accepted"
	UpstreamAcceptanceUnknown UpstreamAcceptance = "unknown"
)

type RoutePolicy struct {
	ID int64
	SourceGroupID int64
	Enabled bool
	MaxRouteAttempts int
	MaxAccountAttemptsPerRoute int
	MaxTotalAttempts int
	FailoverTimeout time.Duration
	FailureThreshold int
	FailureWindow time.Duration
	OpenCooldown time.Duration
	RecoverySuccessThreshold int
	HalfOpenConcurrency int
	RouteStickyTTL time.Duration
	Candidates []RouteCandidate
}

type RouteCandidate struct {
	ID int64
	PolicyID int64
	Role RouteCandidateRole
	TargetGroupID int64
	Priority int
	Enabled bool
	ModelMapping map[string]string
	EffectiveModel string
}

type RouteGroupMeta struct {
	ID int64
	Platform string
	SubscriptionType string
	Status string
}

type RoutePolicySnapshot struct {
	Version int64
	LoadedAt time.Time
	Policies map[int64]RoutePolicy
	Groups map[int64]RouteGroupMeta
}

type RouteRequest struct {
	SourceGroupID int64
	RequestedModel string
	Endpoint string
	SessionHash string
	Stream bool
	RequestID string
	IdempotencyKey string
}

type RouteResolvedCandidate struct {
	PolicyID int64
	CandidateID int64
	Role RouteCandidateRole
	SourceGroupID int64
	EffectiveGroupID int64
	RequestedModel string
	EffectiveModel string
	Endpoint string
}

type RouteAttempt struct {
	Route RouteResolvedCandidate
	Account *Account
	Selection *AccountSelectionResult
	RouteAttempt int
	AccountAttempt int
	TotalAttempt int
}

type RouteFailure struct {
	Err error
	Class RouteFailureClass
	Reason string
	CircuitScope RouteCircuitScope
	ReplaySafety ReplaySafety
	UpstreamAcceptance UpstreamAcceptance
	CommitState SemanticCommitState
}

func (f RouteFailure) CanReplayAcrossRoute() bool {
	return (f.Class == RouteFailureCapacity || f.Class == RouteFailureRoute || f.Class == RouteFailureProvider) &&
		f.ReplaySafety == ReplaySafe && f.CommitState == SemanticUncommitted
}

type RouteAttemptOutcome struct {
	Value any
	Failure *RouteFailure
}

type RouteExecutionAudit struct {
	SourceGroupID int64
	EffectiveGroupID int64
	PolicyID int64
	CandidateID int64
	RequestedModel string
	EffectiveModel string
	FallbackUsed bool
	RouteAttemptCount int
	AccountAttemptCount int
	FallbackReason string
}

type RouteExecutionResult struct {
	Value any
	Failure *RouteFailure
	Audit RouteExecutionAudit
}

type RouteForwarder interface {
	SelectAccount(context.Context, RouteResolvedCandidate, map[int64]struct{}) (*AccountSelectionResult, error)
	AcquireAccount(context.Context, *AccountSelectionResult) (func(), *RouteFailure)
	Forward(context.Context, RouteAttempt) RouteAttemptOutcome
	SemanticCommitted() <-chan struct{}
}

type RoutePolicyRepository interface {
	LoadRoutePolicySnapshot(context.Context) (RoutePolicySnapshot, error)
}

type RouteHealthKeys struct { Candidate, Capability string }
func NewRouteHealthKeys(policyID int64, candidate RouteCandidate, endpoint, requestedModel string) RouteHealthKeys {
	endpoint = strings.TrimSpace(endpoint)
	requestedModel = strings.TrimSpace(requestedModel)
	return RouteHealthKeys{
		Candidate: fmt.Sprintf("route:circuit:%d:%d", policyID, candidate.ID),
		Capability: fmt.Sprintf("route:capability:%d:%d:%s:%s", policyID, candidate.ID, endpoint, requestedModel),
	}
}

func DefaultRoutePolicy(sourceGroupID int64) RoutePolicy {
	return RoutePolicy{SourceGroupID: sourceGroupID, MaxRouteAttempts: 3,
		MaxAccountAttemptsPerRoute: 2, MaxTotalAttempts: 4,
		FailoverTimeout: 20*time.Second, FailureThreshold: 5,
		FailureWindow: time.Minute, OpenCooldown: time.Minute,
		RecoverySuccessThreshold: 2, HalfOpenConcurrency: 1, RouteStickyTTL: time.Hour}
}
```

- [ ] **Step 4: Run the type tests**

Run: `cd backend; go test ./internal/service -run 'TestDefaultRoutePolicy|TestRouteHealthKeys|TestRouteFailureCanReplay' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the domain contract**

```bash
git add backend/internal/service/route_executor_types.go backend/internal/service/route_executor_types_test.go
git commit -m "feat: define route executor v2 contract"
```

### Task 3: Load immutable snapshots and preserve candidate IDs

**Files:**
- Create: `backend/internal/repository/route_policy_repo.go`
- Create: `backend/internal/repository/route_policy_repo_test.go`
- Create: `backend/internal/service/route_policy_snapshot.go`
- Create: `backend/internal/service/route_policy_snapshot_test.go`

- [ ] **Step 1: Write failing repository tests for one snapshot query and stable updates**

```go
func TestRoutePolicyRepositoryLoadsGroupsInSameSnapshotQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rows := sqlmock.NewRows([]string{
		"policy_id", "source_group_id", "enabled", "max_route_attempts",
		"max_account_attempts_per_route", "max_total_attempts", "failover_timeout_ms",
		"failure_threshold", "failure_window_seconds", "open_cooldown_seconds",
		"recovery_success_threshold", "candidate_id", "role", "target_group_id",
		"priority", "candidate_enabled", "model_mapping", "group_platform",
		"group_subscription_type", "group_status", "version",
	}).AddRow(1, 10, true, 3, 2, 4, 20000, 5, 60, 60, 2,
		101, "primary", 10, 0, true, []byte(`{}`), "anthropic", "standard", "active", 99)
	mock.ExpectQuery("FROM route_policies p").WillReturnRows(rows)
	repo := newRoutePolicyRepository(db)
	snapshot, err := repo.LoadRoutePolicySnapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, "anthropic", snapshot.Groups[10].Platform)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRoutePolicyRepositoryUpdatesCandidateByStableID(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO route_policies").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(7))
	mock.ExpectExec("UPDATE route_candidates").WithArgs("fallback", int64(12), 1, true, sqlmock.AnyArg(), int64(44), int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM route_candidates").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	repo := newRoutePolicyRepository(db)
	err = repo.SaveRoutePolicy(context.Background(), service.RoutePolicy{SourceGroupID: 10, Candidates: []service.RouteCandidate{{ID: 44, Role: service.RouteCandidateFallback, TargetGroupID: 12, Priority: 1, Enabled: true}}})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
```

- [ ] **Step 2: Run repository tests and verify the V2 repository is undefined**

Run: `cd backend; go test ./internal/repository -run 'TestRoutePolicyRepository' -count=1`

Expected: FAIL with undefined `newRoutePolicyRepository`.

- [ ] **Step 3: Implement one joined snapshot query and transactional stable-ID CRUD**

```go
const routePolicySnapshotQuery = `
SELECT p.id, p.source_group_id, p.enabled, p.max_route_attempts,
       p.max_account_attempts_per_route, p.max_total_attempts, p.failover_timeout_ms,
       p.failure_threshold, p.failure_window_seconds, p.open_cooldown_seconds,
       p.recovery_success_threshold, c.id, c.role, c.target_group_id, c.priority,
       c.enabled, c.model_mapping, g.platform, g.subscription_type, g.status,
       COALESCE((EXTRACT(EPOCH FROM MAX(GREATEST(p.updated_at, c.updated_at, g.updated_at)) OVER ()) * 1000000)::BIGINT, 0)
FROM route_policies p
JOIN route_candidates c ON c.policy_id = p.id
JOIN groups g ON g.id = c.target_group_id
ORDER BY p.source_group_id, CASE c.role WHEN 'primary' THEN 0 ELSE 1 END, c.priority, c.id`

type routePolicyRepository struct{ db *sql.DB }
func NewRoutePolicyRepository(db *sql.DB) *routePolicyRepository { return newRoutePolicyRepository(db) }
func newRoutePolicyRepository(db *sql.DB) *routePolicyRepository { return &routePolicyRepository{db: db} }

func (r *routePolicyRepository) SaveRoutePolicy(ctx context.Context, p service.RoutePolicy) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil { return err }
	defer tx.Rollback()
	var policyID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO route_policies
        (source_group_id, enabled, max_route_attempts, max_account_attempts_per_route,
         max_total_attempts, failover_timeout_ms, failure_threshold,
         failure_window_seconds, open_cooldown_seconds, recovery_success_threshold, updated_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NOW())
        ON CONFLICT (source_group_id) DO UPDATE SET enabled=EXCLUDED.enabled,
        max_route_attempts=EXCLUDED.max_route_attempts,
        max_account_attempts_per_route=EXCLUDED.max_account_attempts_per_route,
        max_total_attempts=EXCLUDED.max_total_attempts,
        failover_timeout_ms=EXCLUDED.failover_timeout_ms,
        failure_threshold=EXCLUDED.failure_threshold,
        failure_window_seconds=EXCLUDED.failure_window_seconds,
        open_cooldown_seconds=EXCLUDED.open_cooldown_seconds,
        recovery_success_threshold=EXCLUDED.recovery_success_threshold, updated_at=NOW()
        RETURNING id`, p.SourceGroupID, p.Enabled, p.MaxRouteAttempts,
		p.MaxAccountAttemptsPerRoute, p.MaxTotalAttempts, p.FailoverTimeout.Milliseconds(),
		p.FailureThreshold, int(p.FailureWindow.Seconds()), int(p.OpenCooldown.Seconds()),
		p.RecoverySuccessThreshold).Scan(&policyID)
	if err != nil { return fmt.Errorf("save route policy: %w", err) }
	kept := make([]int64, 0, len(p.Candidates))
	for _, c := range p.Candidates {
		mapping, err := json.Marshal(c.ModelMapping)
		if err != nil { return fmt.Errorf("encode route candidate mapping: %w", err) }
		if c.ID > 0 {
			result, err := tx.ExecContext(ctx, `UPDATE route_candidates SET role=$1,
                target_group_id=$2, priority=$3, enabled=$4, model_mapping=$5, updated_at=NOW()
                WHERE id=$6 AND policy_id=$7`, c.Role, c.TargetGroupID, c.Priority, c.Enabled, mapping, c.ID, policyID)
			if err != nil { return fmt.Errorf("update route candidate: %w", err) }
			if n, _ := result.RowsAffected(); n != 1 { return fmt.Errorf("route candidate %d does not belong to policy %d", c.ID, policyID) }
			kept = append(kept, c.ID)
			continue
		}
		var id int64
		err = tx.QueryRowContext(ctx, `INSERT INTO route_candidates
            (policy_id, role, target_group_id, priority, enabled, model_mapping)
            VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, policyID, c.Role,
			c.TargetGroupID, c.Priority, c.Enabled, mapping).Scan(&id)
		if err != nil { return fmt.Errorf("insert route candidate: %w", err) }
		kept = append(kept, id)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM route_candidates
        WHERE policy_id=$1 AND NOT (id = ANY($2))`, policyID, pq.Array(kept))
	if err != nil { return fmt.Errorf("delete removed route candidates: %w", err) }
	return tx.Commit()
}
```

Implement `LoadRoutePolicySnapshot` by starting each scanned policy with `DefaultRoutePolicy(sourceGroupID)`, overriding every persisted database value, cloning every mapping, populating `Policies[sourceGroupID]` and `Groups[targetGroupID]`, and converting database seconds/milliseconds to `time.Duration`. This ensures the fixed one-hour sticky TTL and one-probe half-open concurrency come from the same backend default even though they are not editable database columns. Define the admin-only interface alongside the read interface:

```go
type RoutePolicyConfigRepository interface {
	GetRoutePolicy(context.Context, int64) (*RoutePolicy, error)
	SaveRoutePolicy(context.Context, RoutePolicy) error
	DeleteRoutePolicy(context.Context, int64) error
}
```

`routePolicyRepository` implements both interfaces. Because `NewRoutePolicyRepository` returns the concrete pointer, `backend/internal/repository/wire.go` binds that one provider to `service.RoutePolicyRepository` and `service.RoutePolicyConfigRepository`; it must not construct two repository instances.

- [ ] **Step 4: Write the failing last-valid snapshot test**

```go
func TestRoutePolicySnapshotKeepsLastValidValue(t *testing.T) {
	repo := &routePolicyRepoStub{snapshot: RoutePolicySnapshot{Version: 7, Policies: map[int64]RoutePolicy{10: {ID: 1, SourceGroupID: 10}}}}
	loader := NewRoutePolicySnapshotLoader(repo, time.Minute)
	require.NoError(t, loader.Reload(context.Background()))
	repo.err = errors.New("postgres unavailable")
	require.Error(t, loader.Reload(context.Background()))
	snapshot, ok := loader.Current()
	require.True(t, ok)
	require.Equal(t, int64(7), snapshot.Version)
}
```

- [ ] **Step 5: Implement atomic last-valid snapshot refresh**

```go
type RoutePolicySnapshotLoader struct {
	repo RoutePolicyRepository
	refreshEvery time.Duration
	current atomic.Pointer[RoutePolicySnapshot]
	reloadMu sync.Mutex
	refreshing atomic.Bool
}

func (l *RoutePolicySnapshotLoader) Reload(ctx context.Context) error {
	l.reloadMu.Lock()
	defer l.reloadMu.Unlock()
	next, err := l.repo.LoadRoutePolicySnapshot(ctx)
	if err != nil { return err }
	next.LoadedAt = time.Now()
	l.current.Store(&next)
	return nil
}

func (l *RoutePolicySnapshotLoader) Current() (RoutePolicySnapshot, bool) {
	current := l.current.Load()
	if current == nil { return RoutePolicySnapshot{}, false }
	return *current, true
}
```

Add `RefreshIfStale` using one asynchronous five-second timeout and `refreshing.CompareAndSwap(false, true)`. Never call the repository from `Current` or candidate resolution.

- [ ] **Step 6: Run repository and snapshot tests**

Run: `cd backend; go test ./internal/repository ./internal/service -run 'TestRoutePolicyRepository|TestRoutePolicySnapshot' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit persistence and snapshots**

```bash
git add backend/internal/repository/route_policy_repo.go backend/internal/repository/route_policy_repo_test.go backend/internal/service/route_policy_snapshot.go backend/internal/service/route_policy_snapshot_test.go
git commit -m "feat: add stable route policy snapshots"
```

### Task 4: Implement distributed circuits, stickiness, and explicit Redis degradation

**Files:**
- Create: `backend/internal/repository/route_state_store.go`
- Create: `backend/internal/repository/route_state_store_test.go`
- Modify: `backend/internal/service/route_executor_types.go`
- Create: `backend/internal/service/route_state_resilient.go`
- Create: `backend/internal/service/route_state_resilient_test.go`

- [ ] **Step 1: Write failing Redis state tests**

```go
func TestRouteStateStoreAcquiresOneHalfOpenLeaseAndReleasesUnusedLease(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	now := time.Unix(1_700_000_000, 0)
	store := newRedisRouteStateStore(client, func() time.Time { return now })
	settings := service.RouteCircuitSettings{FailureThreshold: 1, FailureWindow: time.Minute, OpenCooldown: 30*time.Second, RecoverySuccessThreshold: 1, LeaseTTL: 15*time.Second}
	ctx := context.Background()
	_, err := store.RecordFailure(ctx, "route:circuit:1:2", settings, "provider")
	require.NoError(t, err)
	now = now.Add(31 * time.Second)
	first, err := store.AcquirePermit(ctx, "route:circuit:1:2", settings)
	require.NoError(t, err)
	require.True(t, first.Allowed)
	require.NotEmpty(t, first.LeaseID)
	second, err := store.AcquirePermit(ctx, "route:circuit:1:2", settings)
	require.NoError(t, err)
	require.False(t, second.Allowed)
	require.NoError(t, store.ReleaseLease(ctx, "route:circuit:1:2", first.LeaseID))
	third, err := store.AcquirePermit(ctx, "route:circuit:1:2", settings)
	require.NoError(t, err)
	require.True(t, third.Allowed)
}

func TestRouteStateStoreStickyRoundTripUsesOneHourTTL(t *testing.T) {
	server := miniredis.RunT(t)
	store := newRedisRouteStateStore(redis.NewClient(&redis.Options{Addr: server.Addr()}), time.Now)
	binding := service.RouteStickyBinding{PolicyID: 1, CandidateID: 2, EffectiveGroupID: 3, EffectiveModel: "mapped"}
	require.NoError(t, store.PutSticky(context.Background(), 10, "session", binding, time.Hour))
	got, ok, err := store.GetSticky(context.Background(), 10, "session")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, binding, got)
}

func TestRouteStateStoreListsCapabilityCircuitsWithHardLimit(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	ctx := context.Background()
	require.NoError(t, client.HSet(ctx, "route:capability:1:2:/v1/messages:m1", "state", "open").Err())
	require.NoError(t, client.HSet(ctx, "route:capability:1:2:/v1/messages:m2", "state", "closed").Err())
	store := newRedisRouteStateStore(client, time.Now)
	states, err := store.ListCircuits(ctx, "route:capability:1:2:", 1)
	require.NoError(t, err)
	require.Len(t, states, 1)
}

func TestRouteStateStoreAllowsOneHalfOpenProbeAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	now := time.Unix(1_700_000_000, 0)
	newStore := func() *redisRouteStateStore {
		return newRedisRouteStateStore(redis.NewClient(&redis.Options{Addr: server.Addr()}), func() time.Time { return now })
	}
	settings := service.RouteCircuitSettings{FailureThreshold: 1, FailureWindow: time.Minute, OpenCooldown: time.Second, RecoverySuccessThreshold: 1, LeaseTTL: 15*time.Second}
	_, err := newStore().RecordFailure(context.Background(), "route:circuit:1:2", settings, "provider")
	require.NoError(t, err)
	now = now.Add(2 * time.Second)
	type result struct { allowed bool; err error }
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, store := range []*redisRouteStateStore{newStore(), newStore()} {
		wg.Add(1)
		go func(store *redisRouteStateStore) {
			defer wg.Done()
			permit, err := store.AcquirePermit(context.Background(), "route:circuit:1:2", settings)
			results <- result{allowed: permit.Allowed, err: err}
		}(store)
	}
	wg.Wait()
	close(results)
	allowed := 0
	for value := range results { require.NoError(t, value.err); if value.allowed { allowed++ } }
	require.Equal(t, 1, allowed)
}
```

- [ ] **Step 2: Run the Redis tests and verify the store is undefined**

Run: `cd backend; go test ./internal/repository -run TestRouteStateStore -count=1`

Expected: FAIL with undefined `newRedisRouteStateStore`.

- [ ] **Step 3: Implement the state contract and Redis Lua operations**

Add the following public contract to `backend/internal/service/route_executor_types.go`; implement it in `backend/internal/repository/route_state_store.go` so repository code depends inward on service types:

```go
type RouteCircuitSettings struct {
	FailureThreshold int
	FailureWindow time.Duration
	OpenCooldown time.Duration
	RecoverySuccessThreshold int
	LeaseTTL time.Duration
}
type RouteCircuitTransition struct { Changed bool; From, To string }
type RouteCircuitPermit struct { Allowed bool; HalfOpen bool; LeaseID string; Mode RouteStateMode; Transition RouteCircuitTransition }
type RouteCircuitInspection struct { Allowed bool; HalfOpenReady bool; State, LastFailureReason string; LastFailureAt time.Time }
type RouteCircuitSnapshot struct { Key, State, LastFailureReason string; LastFailureAt time.Time }
type RouteStickyBinding struct { PolicyID, CandidateID, EffectiveGroupID int64; EffectiveModel string }
type RouteStateStore interface {
	Inspect(context.Context, string, RouteCircuitSettings) (RouteCircuitInspection, error)
	AcquirePermit(context.Context, string, RouteCircuitSettings) (RouteCircuitPermit, error)
	ReleaseLease(context.Context, string, string) error
	RecordSuccess(context.Context, string, RouteCircuitSettings, string) (RouteCircuitTransition, error)
	RecordFailure(context.Context, string, RouteCircuitSettings, string) (RouteCircuitTransition, error)
	GetSticky(context.Context, int64, string) (RouteStickyBinding, bool, error)
	PutSticky(context.Context, int64, string, RouteStickyBinding, time.Duration) error
	DeleteSticky(context.Context, int64, string) error
	ListCircuits(context.Context, string, int) ([]RouteCircuitSnapshot, error)
}
```

Use Redis hashes for `state/opened_at/successes/last_failure/last_failure_at`, sorted sets for the sliding failure window, and `SET NX PX` for `route:lease:{health_key}`. Lua returns the prior and resulting state so transitions are counted once by the caller, and must compare the lease token before deleting it. On every circuit mutation, expire both the hash and failure-window set after `max(24h, 2*(failure_window+open_cooldown))` so removed policies/candidates do not leak keys forever. Sticky values are JSON at `route:sticky:{source_group}:{sha256(session_hash)}` and use the policy TTL. `ListCircuits` is admin-only: use cursor-based `SCAN MATCH <prefix>* COUNT 100`, stop at the caller's positive hard limit (500 in the runtime endpoint), then pipeline `HMGET`; it never appears in `Execute` or `Preview`.

- [ ] **Step 4: Write the failing degradation test**

```go
func TestResilientRouteStateFallsBackLocallyAndTightensBudget(t *testing.T) {
	remote := &routeStateStub{err: errors.New("redis unavailable")}
	local := NewLocalRouteStateStore(30*time.Second, time.Now)
	state := NewResilientRouteStateStore(remote, local)
	permit, mode, err := state.AcquirePermit(context.Background(), "route:circuit:1:2", RouteCircuitSettings{})
	require.NoError(t, err)
	require.True(t, permit.Allowed)
	require.Equal(t, RouteStateDegraded, mode)
	require.Equal(t, RouteStateDegraded, state.Stats().Mode)
	require.Equal(t, int64(1), state.Stats().StoreErrors)
	require.Equal(t, 2, TightenRouteTotalAttempts(4, 2, mode))
}
```

- [ ] **Step 5: Implement the bounded local fallback and counters**

```go
type RouteStateMode string
const (
	RouteStateDistributed RouteStateMode = "distributed"
	RouteStateDegraded RouteStateMode = "degraded"
)

type RouteRuntimeState interface {
	Inspect(context.Context, string, RouteCircuitSettings) (RouteCircuitInspection, RouteStateMode, error)
	AcquirePermit(context.Context, string, RouteCircuitSettings) (RouteCircuitPermit, RouteStateMode, error)
	ReleasePermit(context.Context, string, RouteCircuitPermit) error
	RecordSuccess(context.Context, string, RouteCircuitSettings, RouteCircuitPermit) (RouteCircuitTransition, RouteStateMode, error)
	RecordFailure(context.Context, string, RouteCircuitSettings, string, RouteCircuitPermit) (RouteCircuitTransition, RouteStateMode, error)
	GetSticky(context.Context, int64, string) (RouteStickyBinding, bool, RouteStateMode, error)
	PutSticky(context.Context, int64, string, RouteStickyBinding, time.Duration) (RouteStateMode, error)
	DeleteSticky(context.Context, int64, string) (RouteStateMode, error)
	ListCircuits(context.Context, string, int) ([]RouteCircuitSnapshot, RouteStateMode, error)
	Stats() RouteStateStats
}

type RouteStateStats struct {
	Mode RouteStateMode
	StoreErrors int64
	DegradedOperations int64
	LastErrorAt time.Time
	LastDistributedSuccessAt time.Time
}

const localRouteStateMaxEntries = 4096

func TightenRouteTotalAttempts(configured, degradedLimit int, mode RouteStateMode) int {
	if configured < 1 { configured = 1 }
	if degradedLimit < 1 { degradedLimit = 1 }
	if mode == RouteStateDegraded && configured > degradedLimit { return degradedLimit }
	return configured
}
```

`LocalRouteStateStore` keeps at most `localRouteStateMaxEntries` circuit entries. Under its mutex, every insertion removes expired entries first and then evicts the oldest `lastTouched` entry until below the cap. Add a test that inserts 4097 unique keys and asserts the in-memory map remains at 4096. Local stickiness is never stored.

`ResilientRouteStateStore` implements `RouteRuntimeState` while delegating to the raw `RouteStateStore` interface above. `AcquirePermit` stamps the returned permit with the store mode that created its lease. `ReleasePermit` and the success/failure completion methods use that stamped origin, so a local lease is still completed locally if Redis recovers mid-attempt, and a distributed lease is not mistaken for a local one if Redis drops. If the original store is unavailable during completion, emit degradation telemetry, update the local circuit conservatively, and let the unreachable lease expire by its 15-second TTL. Its success/failure methods preserve the `RouteCircuitTransition` returned by the chosen store.

For new operations it first calls Redis; on error it emits the rate-limited `route_state.redis_unavailable` alert, increments atomic counters, sets `Stats().Mode` to degraded, records the last error timestamp, and delegates circuits to the expiring local store. The next successful distributed operation sets mode back to distributed and records `LastDistributedSuccessAt`, so runtime health can distinguish a historical error from a current outage. It does not emulate distributed stickiness in degraded mode: sticky reads return no binding and sticky writes are skipped with a degraded-operation count. `RouteExecutor` depends on `RouteRuntimeState` (normally this wrapper, or a test double), never directly on the raw Redis store.

- [ ] **Step 6: Run state-store tests**

Run: `cd backend; go test ./internal/repository ./internal/service -run 'TestRouteStateStore|TestResilientRouteState' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit runtime state**

```bash
git add backend/internal/repository/route_state_store.go backend/internal/repository/route_state_store_test.go backend/internal/service/route_state_resilient.go backend/internal/service/route_state_resilient_test.go
git commit -m "feat: add resilient route state stores"
```

### Task 5: Classify failures without poisoning route health

**Files:**
- Create: `backend/internal/service/route_failure_classifier.go`
- Create: `backend/internal/service/route_failure_classifier_test.go`

- [ ] **Step 1: Write the failure matrix as table-driven tests**

```go
func TestClassifyRouteFailure(t *testing.T) {
	tests := []struct{
		name string
		err error
		committed bool
		accepted UpstreamAcceptance
		want RouteFailureClass
		wantReplay ReplaySafety
	}{
		{"canceled", context.Canceled, false, UpstreamAcceptanceUnknown, RouteFailureCanceled, ReplayUnsafe},
		{"request", &UpstreamFailoverError{StatusCode: 400, Scope: GatewayFailureScopeRequest}, false, UpstreamNotAccepted, RouteFailureRequest, ReplayUnsafe},
		{"account auth", &UpstreamFailoverError{StatusCode: 401, Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeAccount}, false, UpstreamNotAccepted, RouteFailureAccount, ReplaySafe},
		{"provider unavailable", &UpstreamFailoverError{StatusCode: 503, Scope: GatewayFailureScopeProvider}, false, UpstreamNotAccepted, RouteFailureProvider, ReplaySafe},
		{"provider acceptance unknown", &UpstreamFailoverError{StatusCode: 503, Scope: GatewayFailureScopeProvider}, false, UpstreamAcceptanceUnknown, RouteFailureProvider, ReplayUnknown},
		{"provider after token", &UpstreamFailoverError{StatusCode: 503, Scope: GatewayFailureScopeProvider}, true, UpstreamAccepted, RouteFailureProvider, ReplayUnsafe},
		{"unknown", errors.New("opaque"), false, UpstreamAcceptanceUnknown, RouteFailureUnknown, ReplayUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRouteFailure(tt.err, tt.committed, tt.accepted)
			require.Equal(t, tt.want, got.Class)
			require.Equal(t, tt.wantReplay, got.ReplaySafety)
		})
	}
}

func TestRouteCircuitRecordsOnlySafeRouteOrProviderFailures(t *testing.T) {
	require.False(t, ShouldRecordRouteFailure(RouteFailure{Class: RouteFailureAccount, ReplaySafety: ReplaySafe}))
	require.False(t, ShouldRecordRouteFailure(RouteFailure{Class: RouteFailureRequest, ReplaySafety: ReplayUnsafe}))
	require.True(t, ShouldRecordRouteFailure(RouteFailure{Class: RouteFailureProvider, CircuitScope: RouteCircuitCapability, ReplaySafety: ReplaySafe, CommitState: SemanticUncommitted}))
}
```

- [ ] **Step 2: Run the classifier tests and verify they fail**

Run: `cd backend; go test ./internal/service -run 'TestClassifyRouteFailure|TestRouteCircuitRecordsOnly' -count=1`

Expected: FAIL with undefined classifier functions.

- [ ] **Step 3: Implement conservative classification**

```go
func ClassifyRouteFailure(err error, committed bool, accepted UpstreamAcceptance) RouteFailure {
	commit := SemanticUncommitted
	if committed { commit = SemanticCommitted }
	failure := RouteFailure{Err: err, Class: RouteFailureUnknown, CircuitScope: RouteCircuitNone, ReplaySafety: ReplayUnknown, UpstreamAcceptance: accepted, CommitState: commit}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		failure.Class, failure.ReplaySafety = RouteFailureCanceled, ReplayUnsafe
		return failure
	}
	var upstream *UpstreamFailoverError
	if !errors.As(err, &upstream) { return failure }
	switch upstream.Scope {
	case GatewayFailureScopeRequest:
		failure.Class, failure.ReplaySafety = RouteFailureRequest, ReplayUnsafe
	case GatewayFailureScopeAccount:
		failure.Class, failure.ReplaySafety = RouteFailureAccount, ReplaySafe
	case GatewayFailureScopeProvider:
		failure.Class, failure.CircuitScope, failure.ReplaySafety = RouteFailureProvider, RouteCircuitCapability, ReplaySafe
	default:
		failure.Class, failure.ReplaySafety = RouteFailureUnknown, ReplayUnknown
	}
	if committed || accepted == UpstreamAccepted {
		failure.ReplaySafety = ReplayUnsafe
	} else if accepted == UpstreamAcceptanceUnknown &&
		(failure.Class == RouteFailureRoute || failure.Class == RouteFailureProvider) {
		failure.ReplaySafety = ReplayUnknown
	}
	return failure
}

func ShouldRecordRouteFailure(f RouteFailure) bool {
	return (f.Class == RouteFailureRoute || f.Class == RouteFailureProvider) &&
		(f.CircuitScope == RouteCircuitCandidate || f.CircuitScope == RouteCircuitCapability) &&
		f.ReplaySafety == ReplaySafe && f.CommitState == SemanticUncommitted
}
```

Add typed constructors with explicit `CircuitScope` for capacity, DNS/TCP/TLS/route-proxy, shared-infrastructure, and request-validation failures. Account, request, capacity, shared-infrastructure, canceled, and unknown constructors always use `RouteCircuitNone`. Explicit candidate-route failures use `RouteCircuitCandidate`; an ordinary provider response defaults conservatively to `RouteCircuitCapability` and is promoted to candidate-wide only when the adapter has provider-wide evidence. Do not infer route or circuit scope from status alone when `UpstreamFailoverError.Scope` is empty.

- [ ] **Step 4: Run classifier tests**

Run: `cd backend; go test ./internal/service -run 'TestClassifyRouteFailure|TestRouteCircuitRecordsOnly' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit failure semantics**

```bash
git add backend/internal/service/route_failure_classifier.go backend/internal/service/route_failure_classifier_test.go
git commit -m "feat: classify route failover outcomes"
```

### Task 6: Build the deep RouteExecutor

**Files:**
- Create: `backend/internal/service/route_executor.go`
- Create: `backend/internal/service/route_executor_test.go`

- [ ] **Step 1: Write failing tests for route and account behavior**

```go
func TestRouteExecutorProviderFailureImmediatelyUsesFallback(t *testing.T) {
	executor, forwarder, state := newRouteExecutorHarness(t)
	forwarder.outcomes = []RouteAttemptOutcome{
		{Failure: &RouteFailure{Err: errors.New("provider down"), Class: RouteFailureProvider, CircuitScope: RouteCircuitCapability, Reason: "503", ReplaySafety: ReplaySafe, CommitState: SemanticUncommitted}},
		{Value: "ok"},
	}
	result := executor.Execute(context.Background(), RouteRequest{SourceGroupID: 10, RequestedModel: "claude-sonnet", Endpoint: "/v1/messages"}, forwarder)
	require.Nil(t, result.Failure)
	require.Equal(t, "ok", result.Value)
	require.Equal(t, []int64{10, 20}, forwarder.selectedGroups)
	require.Equal(t, 2, result.Audit.RouteAttemptCount)
	require.Equal(t, 2, result.Audit.AccountAttemptCount)
	require.True(t, result.Audit.FallbackUsed)
	require.Equal(t, "503", result.Audit.FallbackReason)
	require.Equal(t, 1, state.failureRecords)
}

func TestRouteExecutorAccountFailureRetriesInsideCandidateOnly(t *testing.T) {
	executor, forwarder, state := newRouteExecutorHarness(t)
	forwarder.outcomes = []RouteAttemptOutcome{
		{Failure: &RouteFailure{Err: errors.New("expired token"), Class: RouteFailureAccount, ReplaySafety: ReplaySafe, CommitState: SemanticUncommitted}},
		{Value: "ok"},
	}
	result := executor.Execute(context.Background(), RouteRequest{SourceGroupID: 10, RequestedModel: "claude-sonnet", Endpoint: "/v1/messages"}, forwarder)
	require.Nil(t, result.Failure)
	require.Equal(t, []int64{10, 10}, forwarder.selectedGroups)
	require.Zero(t, state.failureRecords)
}

func TestRouteExecutorNeverSwitchesAfterSemanticCommit(t *testing.T) {
	executor, forwarder, _ := newRouteExecutorHarness(t)
	forwarder.outcomes = []RouteAttemptOutcome{{Failure: &RouteFailure{Err: errors.New("stream failed"), Class: RouteFailureProvider, ReplaySafety: ReplayUnsafe, CommitState: SemanticCommitted}}}
	result := executor.Execute(context.Background(), RouteRequest{SourceGroupID: 10, RequestedModel: "claude-sonnet", Endpoint: "/v1/messages", Stream: true}, forwarder)
	require.NotNil(t, result.Failure)
	require.Equal(t, []int64{10}, forwarder.selectedGroups)
}

func TestRouteExecutorFailoverTimeoutDoesNotCancelCommittedStream(t *testing.T) {
	executor, forwarder, _ := newRouteExecutorHarness(t)
	snapshot, ok := executor.snapshots.Current()
	require.True(t, ok)
	policy := snapshot.Policies[10]
	policy.FailoverTimeout = 20 * time.Millisecond
	snapshot.Policies[10] = policy
	executor.snapshots.current.Store(&snapshot)
	forwarder.forwardFn = func(ctx context.Context, _ RouteAttempt) RouteAttemptOutcome {
		forwarder.MarkSemanticCommitted()
		select {
		case <-time.After(60 * time.Millisecond):
			return RouteAttemptOutcome{Value: "completed-stream"}
		case <-ctx.Done():
			return RouteAttemptOutcome{Failure: &RouteFailure{Err: ctx.Err(), Class: RouteFailureCanceled}}
		}
	}
	result := executor.Execute(context.Background(), RouteRequest{SourceGroupID: 10, Stream: true}, forwarder)
	require.Nil(t, result.Failure)
	require.Equal(t, "completed-stream", result.Value)
}
```

- [ ] **Step 2: Run executor tests and verify `RouteExecutor` is undefined**

Run: `cd backend; go test ./internal/service -run TestRouteExecutor -count=1`

Expected: FAIL with undefined `RouteExecutor`.

- [ ] **Step 3: Implement candidate resolution from memory only**

```go
func resolveRouteCandidates(snapshot RoutePolicySnapshot, req RouteRequest, sticky *RouteStickyBinding) (RoutePolicy, []RouteCandidate) {
	policy, ok := snapshot.Policies[req.SourceGroupID]
	if !ok || !policy.Enabled {
		primary := RouteCandidate{Role: RouteCandidatePrimary, TargetGroupID: req.SourceGroupID, Enabled: true, EffectiveModel: req.RequestedModel}
		return DefaultRoutePolicy(req.SourceGroupID), []RouteCandidate{primary}
	}
	ordered := append([]RouteCandidate(nil), policy.Candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Role != ordered[j].Role { return ordered[i].Role == RouteCandidatePrimary }
		if ordered[i].Priority != ordered[j].Priority { return ordered[i].Priority < ordered[j].Priority }
		return ordered[i].ID < ordered[j].ID
	})
	valid := ordered[:0]
	for _, candidate := range ordered {
		group, exists := snapshot.Groups[candidate.TargetGroupID]
		if !exists || group.Status != StatusActive || !candidate.Enabled { continue }
		source := snapshot.Groups[req.SourceGroupID]
		if group.Platform != source.Platform || group.SubscriptionType != source.SubscriptionType { continue }
		candidate.EffectiveModel = req.RequestedModel
		if mapped, exists := candidate.ModelMapping[req.RequestedModel]; exists { candidate.EffectiveModel = mapped }
		valid = append(valid, candidate)
	}
	if sticky != nil { valid = moveCandidateFirst(valid, sticky.CandidateID) }
	return policy, valid
}
```

- [ ] **Step 4: Implement the attempt loop with one owner for all budgets**

```go
type RouteExecutor struct {
	snapshots *RoutePolicySnapshotLoader
	state RouteRuntimeState
	redisDegradedMaxTotalAttempts int
}

func NewRouteExecutor(snapshots *RoutePolicySnapshotLoader, state RouteRuntimeState, degradedMaxTotalAttempts int) *RouteExecutor {
	if degradedMaxTotalAttempts < 1 { degradedMaxTotalAttempts = 1 }
	return &RouteExecutor{snapshots: snapshots, state: state,
		redisDegradedMaxTotalAttempts: degradedMaxTotalAttempts}
}

func (e *RouteExecutor) Execute(ctx context.Context, req RouteRequest, forwarder RouteForwarder) RouteExecutionResult {
	snapshot, loaded := e.snapshots.Current()
	if !loaded { return e.executeSourceOnly(ctx, req, forwarder) }
	e.snapshots.RefreshIfStale()
	policy, candidates := resolveRouteCandidates(snapshot, req, nil)
	failoverCtx, stopFailoverBudget := context.WithTimeout(ctx, policy.FailoverTimeout)
	defer stopFailoverBudget()
	fallbackReason := ""
	sticky, stickyOK := e.readSticky(failoverCtx, req)
	if stickyOK && sticky.PolicyID == policy.ID && stickyMatchesResolvedCandidate(sticky, candidates) {
		candidates = moveCandidateFirst(candidates, sticky.CandidateID)
		if len(candidates) > 0 && candidates[0].ID == sticky.CandidateID && candidates[0].Role == RouteCandidateFallback {
			fallbackReason = "sticky_binding"
		}
	} else if stickyOK {
		e.deleteSticky(failoverCtx, req)
	}
	totalAttempts := 0
	routeAttempts := 0
	last := RouteExecutionResult{Failure: &RouteFailure{Err: ErrNoAvailableAccounts, Class: RouteFailureCapacity, ReplaySafety: ReplaySafe, CommitState: SemanticUncommitted}}
	for _, candidate := range candidates {
		if routeAttempts >= policy.MaxRouteAttempts || totalAttempts >= policy.MaxTotalAttempts || failoverCtx.Err() != nil { break }
		resolved := resolveCandidate(req, policy, candidate)
		keys := NewRouteHealthKeys(policy.ID, candidate, req.Endpoint, req.RequestedModel)
		allowed, mode := e.inspectAttemptCircuits(failoverCtx, keys, policy)
		if !allowed { continue }
		maxTotal := TightenRouteTotalAttempts(policy.MaxTotalAttempts, e.redisDegradedMaxTotalAttempts, mode)
		excluded := make(map[int64]struct{})
		candidateCalls := 0
		for candidateCalls < policy.MaxAccountAttemptsPerRoute && totalAttempts < maxTotal && failoverCtx.Err() == nil {
			selection, err := forwarder.SelectAccount(failoverCtx, resolved, excluded)
			if err != nil { last.Failure = NewCapacityRouteFailure(err); break }
			release, acquireFailure := forwarder.AcquireAccount(failoverCtx, selection)
			if acquireFailure != nil {
				last.Failure = acquireFailure
				if acquireFailure.Class == RouteFailureAccount { excluded[selection.Account.ID] = struct{}{}; continue }
				if !acquireFailure.CanReplayAcrossRoute() { return last }
				break
			}
			leases, permitAllowed, permitMode := e.acquireAttemptPermits(failoverCtx, keys, policy)
			maxTotal = TightenRouteTotalAttempts(maxTotal, e.redisDegradedMaxTotalAttempts, permitMode)
			if !permitAllowed {
				if release != nil { release() }
				last.Failure = NewCapacityRouteFailure(ErrRouteCircuitOpen)
				break
			}
			attemptRouteNumber := routeAttempts + 1
			candidateCalls++
			totalAttempts++
			if candidateCalls == 1 { routeAttempts++ }
			attemptCtx, cancelAttempt := context.WithCancel(ctx)
			guardDone, guardExited := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(guardExited)
				select {
				case <-failoverCtx.Done(): cancelAttempt()
				case <-forwarder.SemanticCommitted():
				case <-guardDone:
				}
			}()
			outcome := forwarder.Forward(attemptCtx, RouteAttempt{Route: resolved, Account: selection.Account, Selection: selection, RouteAttempt: attemptRouteNumber, AccountAttempt: candidateCalls, TotalAttempt: totalAttempts})
			close(guardDone)
			<-guardExited
			cancelAttempt()
			if release != nil { release() }
			last = resultFromOutcome(req, resolved, routeAttempts, totalAttempts, fallbackReason, outcome)
			if outcome.Failure != nil && failoverCtx.Err() != nil && ctx.Err() == nil {
				outcome.Failure = NewFailoverBudgetExceeded(failoverCtx.Err())
				last.Failure = outcome.Failure
			}
			if outcome.Failure == nil { e.recordSuccessAndSticky(ctx, keys, leases, policy, req, resolved); return last }
			if outcome.Failure.Class == RouteFailureAccount {
				e.releaseAttemptLeases(ctx, keys, leases)
				excluded[selection.Account.ID] = struct{}{}
				continue
			}
			if ShouldRecordRouteFailure(*outcome.Failure) {
				e.recordRouteFailure(ctx, keys, leases, policy, *outcome.Failure)
			} else {
				e.releaseAttemptLeases(ctx, keys, leases)
			}
			if !outcome.Failure.CanReplayAcrossRoute() { return last }
			break
		}
		if last.Failure == nil || !last.Failure.CanReplayAcrossRoute() { break }
		fallbackReason = strings.TrimSpace(last.Failure.Reason)
		if fallbackReason == "" { fallbackReason = string(last.Failure.Class) }
	}
	if failoverCtx.Err() != nil && ctx.Err() == nil {
		last.Failure = NewFailoverBudgetExceeded(failoverCtx.Err())
	}
	return last
}
```

`inspectAttemptCircuits` performs read-only circuit checks before account selection. After the account and its concurrency slot are acquired, `acquireAttemptPermits` atomically rechecks both candidate and capability circuits and acquires any half-open leases immediately before `Forward`. If either permit denies or its lease acquisition fails, it releases the account slot and any lease already acquired. `recordRouteFailure` switches on `RouteFailure.CircuitScope`: candidate-wide failures update only the candidate circuit and release any capability lease; capability-isolated failures update only the endpoint/requested-model circuit and release any candidate lease. Success updates both permit keys. Account, request, capacity, infrastructure, canceled, unknown, replay-unsafe, and committed outcomes update neither and release every acquired lease before returning or retrying.

The failover timeout guards policy-state reads, account acquisition, and the period before the first semantic output. `Forward` receives a caller-derived cancelable context without the failover deadline. A small guard cancels that attempt if the failover budget expires first, but exits permanently when `SemanticCommitted()` closes, so a valid stream can continue beyond 20 seconds under the original client context. Account and route attempt counters increment only immediately before a real `Forward` call; circuit skips and failed slot acquisition do not inflate audit counts.

`resultFromOutcome` always copies source group, effective group, policy ID, candidate ID, requested/effective model, and the two real-attempt counters into `RouteExecutionAudit`. It sets `FallbackUsed` from `resolved.Role == RouteCandidateFallback`, carries the preceding failure reason into a successful fallback, and uses `sticky_binding` when a sticky fallback was first. It never substitutes the effective group for the billing source group.

Sticky reads/writes are skipped when `SessionHash` is blank. A binding is accepted only when its policy ID, candidate ID, effective group, and effective model still match the current resolved candidate; otherwise the disposable binding is deleted and normal primary-first ordering is used.

- [ ] **Step 5: Add tests for unused leases, requested-model keys, stickiness, and every budget**

```go
func TestRouteExecutorReleasesFirstLeaseWhenSecondPermitDenies(t *testing.T) {
	executor, forwarder, state := newRouteExecutorHarness(t)
	state.permits = []RouteCircuitPermit{
		{Allowed: true, HalfOpen: true, LeaseID: "lease-1"},
		{Allowed: false},
	}
	_ = executor.Execute(context.Background(), RouteRequest{SourceGroupID: 10, RequestedModel: "client-model", Endpoint: "/v1/messages"}, forwarder)
	require.Contains(t, state.releasedLeases, "lease-1")
	require.Zero(t, forwarder.forwardCalls)
}

func TestRouteExecutorStickyFallbackStaysFirst(t *testing.T) {
	executor, forwarder, state := newRouteExecutorHarness(t)
	state.sticky = RouteStickyBinding{PolicyID: 1, CandidateID: 2, EffectiveGroupID: 20, EffectiveModel: "mapped"}
	forwarder.outcomes = []RouteAttemptOutcome{{Value: "ok"}}
	result := executor.Execute(context.Background(), RouteRequest{SourceGroupID: 10, RequestedModel: "client-model", Endpoint: "/v1/messages", SessionHash: "s"}, forwarder)
	require.Nil(t, result.Failure)
	require.Equal(t, []int64{20}, forwarder.selectedGroups)
}

func TestRouteExecutorReleasesBothHalfOpenLeasesForAccountFailure(t *testing.T) {
	executor, forwarder, state := newRouteExecutorHarness(t)
	state.permits = []RouteCircuitPermit{
		{Allowed: true, HalfOpen: true, LeaseID: "candidate-lease"},
		{Allowed: true, HalfOpen: true, LeaseID: "capability-lease"},
	}
	forwarder.outcomes = []RouteAttemptOutcome{{Failure: &RouteFailure{Err: errors.New("account auth"), Class: RouteFailureAccount, ReplaySafety: ReplaySafe, CommitState: SemanticUncommitted}}}
	_ = executor.Execute(context.Background(), RouteRequest{SourceGroupID: 10, RequestedModel: "client-model", Endpoint: "/v1/messages"}, forwarder)
	require.Subset(t, state.releasedLeases, []string{"candidate-lease", "capability-lease"})
}

func TestRouteExecutorRejectsStickyBindingAfterMappingChanges(t *testing.T) {
	executor, forwarder, state := newRouteExecutorHarness(t)
	state.sticky = RouteStickyBinding{PolicyID: 1, CandidateID: 2, EffectiveGroupID: 20, EffectiveModel: "old-mapping"}
	forwarder.outcomes = []RouteAttemptOutcome{{Value: "ok"}}
	result := executor.Execute(context.Background(), RouteRequest{SourceGroupID: 10, RequestedModel: "client-model", Endpoint: "/v1/messages", SessionHash: "s"}, forwarder)
	require.Nil(t, result.Failure)
	require.Equal(t, []int64{10}, forwarder.selectedGroups)
	require.Equal(t, 1, state.deletedSticky)
}

func TestRouteExecutorPerformsZeroPolicyQueriesOnRequestPath(t *testing.T) {
	repo := &countingRoutePolicyRepository{snapshot: routeExecutorTestSnapshot()}
	loader := NewRoutePolicySnapshotLoader(repo, time.Hour)
	require.NoError(t, loader.Reload(context.Background()))
	executor, forwarder := newRouteExecutorWithLoadedSnapshot(t, loader)
	baseline := repo.loadCalls.Load()
	for i := 0; i < 10; i++ {
		forwarder.outcomes = []RouteAttemptOutcome{{Value: "ok"}}
		result := executor.Execute(context.Background(), RouteRequest{SourceGroupID: 10, RequestedModel: "client-model", Endpoint: "/v1/messages"}, forwarder)
		require.Nil(t, result.Failure)
	}
	require.Equal(t, baseline, repo.loadCalls.Load())
}
```

- [ ] **Step 6: Run all core executor tests with race detection**

Run: `cd backend; go test -race ./internal/service -run 'TestRouteExecutor|TestRoutePolicySnapshot|TestClassifyRouteFailure|TestResilientRouteState' -count=1`

Expected: PASS with no race reports.

- [ ] **Step 7: Commit the executor**

```bash
git add backend/internal/service/route_executor.go backend/internal/service/route_executor_test.go
git commit -m "feat: add route failover v2 executor"
```

### Task 7: Add disabled/shadow/enforce configuration and Wire construction

**Files:**
- Modify: `backend/internal/config/config.go`
- Create: `backend/internal/config/route_failover_v2_test.go`
- Modify: `backend/internal/repository/wire.go`
- Modify: `backend/internal/service/wire.go`
- Modify: `backend/cmd/server/wire_gen.go`

- [ ] **Step 1: Write failing config default and validation tests**

```go
func TestRouteFailoverV2DefaultsOff(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	setDefaults()
	var cfg Config
	require.NoError(t, viper.Unmarshal(&cfg))
	require.Equal(t, "off", cfg.Gateway.RouteFailoverV2.Mode)
	require.Equal(t, 60, cfg.Gateway.RouteFailoverV2.SnapshotRefreshSeconds)
	require.Equal(t, 2, cfg.Gateway.RouteFailoverV2.RedisDegradedMaxTotalAttempts)
}

func TestRouteFailoverV2RejectsUnknownMode(t *testing.T) {
	require.ErrorContains(t, validateRouteFailoverV2(RouteFailoverV2Config{Mode: "sometimes"}), "gateway.route_failover_v2.mode")
}
```

- [ ] **Step 2: Run config tests and verify the field is undefined**

Run: `cd backend; go test ./internal/config -run TestRouteFailoverV2 -count=1`

Expected: FAIL with missing `RouteFailoverV2`.

- [ ] **Step 3: Add the explicit mode configuration**

```go
type RouteFailoverV2Config struct {
	Mode string `mapstructure:"mode"`
	SnapshotRefreshSeconds int `mapstructure:"snapshot_refresh_seconds"`
	RedisLocalStateTTLSeconds int `mapstructure:"redis_local_state_ttl_seconds"`
	RedisDegradedMaxTotalAttempts int `mapstructure:"redis_degraded_max_total_attempts"`
	DebugHeaders bool `mapstructure:"debug_headers"`
}
```

Add `RouteFailoverV2 RouteFailoverV2Config` to `GatewayConfig`. Defaults are `off`, `60`, `30`, `2`, and `false`. Validation accepts only `off`, `shadow`, or `enforce`; production configuration never defaults to `shadow` or `enforce`.

Register those defaults in `setDefaults` with `viper.SetDefault`, and call a focused `validateRouteFailoverV2` helper from `Config.Validate`. Validation also requires all three integer settings to be positive so local-state expiry, refresh cadence, and the degraded-attempt cap cannot silently become zero.

- [ ] **Step 4: Add providers without changing handler constructors**

```go
func ProvideRoutePolicySnapshotLoader(repo RoutePolicyRepository, cfg *config.Config) *RoutePolicySnapshotLoader {
	refresh := time.Minute
	if cfg != nil && cfg.Gateway.RouteFailoverV2.SnapshotRefreshSeconds > 0 {
		refresh = time.Duration(cfg.Gateway.RouteFailoverV2.SnapshotRefreshSeconds) * time.Second
	}
	loader := NewRoutePolicySnapshotLoader(repo, refresh)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = loader.Reload(ctx)
	return loader
}

func ProvideRouteExecutor(loader *RoutePolicySnapshotLoader, state RouteStateStore, cfg *config.Config) *RouteExecutor {
	return NewRouteExecutor(loader, NewResilientRouteStateStore(state,
		NewLocalRouteStateStore(time.Duration(cfg.Gateway.RouteFailoverV2.RedisLocalStateTTLSeconds)*time.Second, time.Now)),
		cfg.Gateway.RouteFailoverV2.RedisDegradedMaxTotalAttempts)
}
```

Register `NewRoutePolicyRepository` and `NewRedisRouteStateStore` in `backend/internal/repository/wire.go`; register the loader and executor in `backend/internal/service/wire.go`. Do not add `*RouteExecutor` to either handler constructor in this task.

- [ ] **Step 5: Regenerate Wire and run construction tests**

Run: `cd backend; go generate ./cmd/server`

Expected: `backend/cmd/server/wire_gen.go` updates without dependency cycles.

Run: `cd backend; go test ./internal/config ./internal/repository ./internal/service ./cmd/server -run 'TestRouteFailoverV2|TestRoutePolicy|TestRouteState|TestRouteExecutor' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit feature construction**

```bash
git add backend/internal/config/config.go backend/internal/config/route_failover_v2_test.go backend/internal/repository/wire.go backend/internal/service/wire.go backend/cmd/server/wire_gen.go
git commit -m "feat: wire disabled route failover v2 core"
```

### Task 8: Run the core acceptance gate

**Files:**
- Test only; no source changes expected.

- [ ] **Step 1: Prove migration 192 was not modified by this plan**

Run: `if (Test-Path backend/migrations/192_route_failover.sql) { (Get-FileHash -Algorithm SHA256 backend/migrations/192_route_failover.sql).Hash.ToLowerInvariant() } else { 'absent' }`

Expected: exactly `35bc4205f0a11213af7848c0573930b066ce86496ce88ad09cc4fcd16a8a411c` when the existing workspace file is present; `absent` is valid in an isolated clean worktree because migration 193 does not require V1. No other hash is accepted.

- [ ] **Step 2: Run all V2-focused backend tests**

Run: `cd backend; go test -race ./migrations ./internal/config ./internal/repository ./internal/service -run 'Route(FailoverV2|Policy|State|Executor|Failure)' -count=1`

Expected: PASS with no race reports.

- [ ] **Step 3: Run the full backend suite on Go 1.26.5**

Run: `cd backend; go test ./...`

Expected: PASS. If the local toolchain is still Go 1.26.4, run this command in the project container or CI using Go 1.26.5 and attach that output before merging.

- [ ] **Step 4: Verify the feature is not in any handler hot path**

Run: `rg -n "RouteExecutor" backend/internal/handler`

Expected: no matches.

- [ ] **Step 5: Record the core completion commit**

```bash
git status --short
git log -8 --oneline
```

Expected: the core plan's commits are present; no unrelated dirty files were staged or committed.
