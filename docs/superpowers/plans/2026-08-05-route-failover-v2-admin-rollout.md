# Route Failover V2 Administration and Rollout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add auditable V2 policy administration, runtime visibility, usage attribution, and gated rollout procedures for test server 73 and production server 46.

**Architecture:** The existing admin-only group route is upgraded in place to the V2 contract, while candidate IDs remain stable across edits. Usage keeps `group_id` as the source billing identity and adds separate effective-route fields. Runtime metrics are in-process counters plus Redis circuit snapshots; the frontend reads backend defaults and never invents policy values.

**Tech Stack:** Go 1.26.5, PostgreSQL/Ent, Gin, Redis, Vue 3, TypeScript 5.6, Vitest, Tailwind, Docker Compose.

---

## File Map

- Create `backend/migrations/194_route_failover_usage_audit.sql`: additive usage audit columns.
- Create `backend/migrations/route_failover_usage_audit_migration_test.go`: migration contract.
- Modify `backend/ent/schema/usage_log.go` and generated Ent files: audit fields.
- Modify `backend/internal/service/usage_log.go`, `gateway_usage_billing.go`, `openai_gateway_usage.go`: carry `RouteUsageFields` without changing billing identity.
- Modify `backend/internal/repository/usage_log_repo_insert.go`, `usage_log_repo_query.go`: insert, batch, and scan new fields in lockstep.
- Create `backend/internal/service/route_policy_manager.go`: V2 defaults, validation, preview, stable-candidate save, and admin audit.
- Create focused service tests.
- Modify `backend/internal/service/admin_service.go`, `admin_group.go`, `wire.go`: use V2 manager.
- Modify `backend/internal/handler/admin/group_handler.go`: V2 CRUD/defaults/preview/runtime endpoints.
- Modify `backend/internal/server/routes/admin.go`: register admin-only endpoints.
- Create `backend/internal/service/route_metrics.go`: required counters, latency summaries, snapshot/store health.
- Modify `backend/internal/service/route_executor.go`: emit metrics and last failure reason.
- Modify frontend types, API, modal, tests, and translations.
- Modify `deploy/config.example.yaml` and `deploy/.env.example`: documented disabled defaults.
- Create `ops/route-failover-v2/README.md`: 73 test gates and 46 production gates.
- Create `tools/route_failover_v2_verify.py`: read-only health, policy, runtime, and usage verification.

### Task 1: Add route audit fields without changing source billing

**Files:**
- Create: `backend/migrations/194_route_failover_usage_audit.sql`
- Create: `backend/migrations/route_failover_usage_audit_migration_test.go`
- Modify: `backend/ent/schema/usage_log.go`
- Modify: generated files under `backend/ent/usagelog/` and `backend/ent/usage_log*.go`

- [ ] **Step 1: Write the failing migration contract test**

```go
package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration194AddsRouteAuditWithoutReplacingBillingGroup(t *testing.T) {
	content, err := FS.ReadFile("194_route_failover_usage_audit.sql")
	require.NoError(t, err)
	sql := string(content)
	for _, column := range []string{
		"effective_group_id", "route_policy_id", "route_candidate_id",
		"fallback_used", "route_attempt_count", "account_attempt_count",
		"fallback_reason", "effective_model",
	} {
		require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS "+column)
	}
	require.NotContains(t, sql, "DROP COLUMN")
	require.NotContains(t, sql, "RENAME COLUMN group_id")
}
```

- [ ] **Step 2: Run the migration test and verify 194 is missing**

Run: `cd backend; go test ./migrations -run TestMigration194 -count=1`

Expected: FAIL because the migration file does not exist.

- [ ] **Step 3: Add the forward-only audit columns**

```sql
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS effective_group_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS route_policy_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS route_candidate_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS fallback_used BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS route_attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS account_attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS fallback_reason VARCHAR(100);
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS effective_model VARCHAR(100);
```

Do not add foreign keys to policy/candidate audit IDs: policy deletion must not erase or invalidate historical usage. Do not add a blocking index in this migration; operational queries use request ID and time-range indexes already present.

- [ ] **Step 4: Add matching optional Ent fields**

```go
field.Int64("effective_group_id").Optional().Nillable(),
field.Int64("route_policy_id").Optional().Nillable(),
field.Int64("route_candidate_id").Optional().Nillable(),
field.Bool("fallback_used").Default(false),
field.Int("route_attempt_count").Default(0),
field.Int("account_attempt_count").Default(0),
field.String("fallback_reason").MaxLen(100).Optional().Nillable(),
field.String("effective_model").MaxLen(100).Optional().Nillable(),
```

- [ ] **Step 5: Regenerate Ent and run migration tests**

Run: `cd backend; go generate ./ent`

Expected: generated usage-log builders and fields update.

Run: `cd backend; go test ./migrations ./ent/... -run 'TestMigration194|TestUsageLog' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the audit schema**

```bash
git add backend/migrations/194_route_failover_usage_audit.sql backend/migrations/route_failover_usage_audit_migration_test.go backend/ent/schema/usage_log.go backend/ent
git commit -m "feat(db): add route usage audit fields"
```

### Task 2: Persist route audit on every successful usage record

**Files:**
- Modify: `backend/internal/service/usage_log.go`
- Modify: `backend/internal/service/gateway_usage_billing.go`
- Modify: `backend/internal/service/openai_gateway_usage.go`
- Modify: `backend/internal/repository/usage_log_repo_insert.go`
- Modify: `backend/internal/repository/usage_log_repo_query.go`
- Modify: `backend/internal/service/gateway_record_usage_test.go`
- Create: `backend/internal/repository/usage_log_route_audit_test.go`

- [ ] **Step 1: Write a failing service test proving source and effective groups differ**

```go
func TestGatewayRecordUsageKeepsSourceBillingAndPersistsEffectiveRoute(t *testing.T) {
	repo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newGatewayRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	sourceGroupID := int64(10)
	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{RequestID: "req-route", Model: "client-model", UpstreamModel: "mapped-model"},
		APIKey: &APIKey{ID: 1, GroupID: &sourceGroupID, Group: &Group{ID: sourceGroupID}},
		User: &User{ID: 2}, Account: &Account{ID: 3},
		RouteUsageFields: RouteUsageFields{EffectiveGroupID: 20, RoutePolicyID: 7,
			RouteCandidateID: 9, FallbackUsed: true, RouteAttemptCount: 2,
			AccountAttemptCount: 2, FallbackReason: "provider_503", EffectiveModel: "mapped-model"},
	})
	require.NoError(t, err)
	require.Equal(t, int64(10), *repo.log.GroupID)
	require.Equal(t, int64(20), *repo.log.EffectiveGroupID)
	require.True(t, repo.log.FallbackUsed)
}
```

- [ ] **Step 2: Run the service test and verify route fields are undefined**

Run: `cd backend; go test ./internal/service -run TestGatewayRecordUsageKeepsSourceBilling -count=1`

Expected: FAIL with undefined `RouteUsageFields`.

- [ ] **Step 3: Add one embedded usage contract shared by gateway services**

```go
type RouteUsageFields struct {
	EffectiveGroupID int64
	RoutePolicyID int64
	RouteCandidateID int64
	FallbackUsed bool
	RouteAttemptCount int
	AccountAttemptCount int
	FallbackReason string
	EffectiveModel string
}

func RouteUsageFromAudit(a RouteExecutionAudit) RouteUsageFields {
	return RouteUsageFields{EffectiveGroupID: a.EffectiveGroupID, RoutePolicyID: a.PolicyID,
		RouteCandidateID: a.CandidateID, FallbackUsed: a.FallbackUsed,
		RouteAttemptCount: a.RouteAttemptCount, AccountAttemptCount: a.AccountAttemptCount,
		FallbackReason: a.FallbackReason, EffectiveModel: a.EffectiveModel}
}
```

Embed `RouteUsageFields` in both `RecordUsageInput` and `OpenAIRecordUsageInput`. Add pointers for nullable IDs/models to `UsageLog`; keep `GroupID: apiKey.GroupID` unchanged in both billing implementations.

- [ ] **Step 4: Add a failing insert-order test**

```go
func TestPrepareUsageLogInsertIncludesRouteAuditInDeclaredOrder(t *testing.T) {
	log := &service.UsageLog{UserID: 1, APIKeyID: 2, AccountID: 3,
		EffectiveGroupID: ptrInt64(20), RoutePolicyID: ptrInt64(7), RouteCandidateID: ptrInt64(9),
		FallbackUsed: true, RouteAttemptCount: 2, AccountAttemptCount: 3,
		FallbackReason: ptrString("provider_503"), EffectiveModel: ptrString("mapped-model")}
	prepared := prepareUsageLogInsert(log)
	require.Len(t, prepared.args, len(usageLogInsertArgTypes))
	require.Equal(t, int64(20), prepared.args[56])
	require.Equal(t, int64(7), prepared.args[57])
	require.Equal(t, int64(9), prepared.args[58])
	require.Equal(t, true, prepared.args[59])
	require.Equal(t, 2, prepared.args[60])
	require.Equal(t, 3, prepared.args[61])
}
```

- [ ] **Step 5: Update every insert, batch, and scan list in lockstep**

Append the eight columns immediately before `created_at` in:

```go
"bigint",  // effective_group_id
"bigint",  // route_policy_id
"bigint",  // route_candidate_id
"boolean", // fallback_used
"integer", // route_attempt_count
"integer", // account_attempt_count
"text",    // fallback_reason
"text",    // effective_model
```

Update `prepareUsageLogInsert().args`, `createSingle`, `execUsageLogInsertNoResult`, both batch CTE column lists, both batch `INSERT` lists, `usageLogSelectColumns`, and `scanUsageLog`. The final argument count becomes 65. Extend the file's existing order-consistency tests so future columns cannot drift.

- [ ] **Step 6: Run usage persistence and billing tests**

Run: `cd backend; go test ./internal/service ./internal/repository -run 'TestGatewayRecordUsageKeepsSourceBilling|TestPrepareUsageLogInsertIncludesRouteAudit|UsageLog.*Insert|UsageLog.*Batch' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit route usage attribution**

```bash
git add backend/internal/service/usage_log.go backend/internal/service/gateway_usage_billing.go backend/internal/service/openai_gateway_usage.go backend/internal/repository/usage_log_repo_insert.go backend/internal/repository/usage_log_repo_query.go backend/internal/service/gateway_record_usage_test.go backend/internal/repository/usage_log_route_audit_test.go
git commit -m "feat: persist route execution usage audit"
```

### Task 3: Replace V1 admin management with stable V2 policy management

**Files:**
- Create: `backend/internal/service/route_policy_manager.go`
- Create: `backend/internal/service/route_policy_manager_test.go`
- Modify: `backend/internal/service/admin_service.go`
- Modify: `backend/internal/service/admin_group.go`
- Modify: `backend/internal/service/wire.go`
- Modify: `backend/cmd/server/wire_gen.go`

- [ ] **Step 1: Write failing validation and stable-ID tests**

```go
func TestRoutePolicyManagerRejectsCompositeAndMissingPrimary(t *testing.T) {
	groups := &routePolicyGroupRepoStub{groups: map[int64]*Group{
		1: {ID: 1, Platform: PlatformComposite, SubscriptionType: SubscriptionTypeStandard, Status: StatusActive},
	}}
	manager := NewRoutePolicyManager(&routePolicyConfigRepoStub{}, groups, nil, nil)
	_, err := manager.Save(context.Background(), 1, RoutePolicy{Enabled: true})
	require.ErrorContains(t, err, "composite")
}

func TestRoutePolicyManagerRequiresExplicitStablePrimary(t *testing.T) {
	manager := newRoutePolicyManagerForTest(t)
	_, err := manager.Save(context.Background(), 1, RoutePolicy{Enabled: true, Candidates: []RouteCandidate{
		{Role: RouteCandidateFallback, TargetGroupID: 2, Enabled: true},
	}})
	require.ErrorContains(t, err, "exactly one primary")
}

func TestRoutePolicyManagerPreservesCandidateIDsOnEdit(t *testing.T) {
	repo := &routePolicyConfigRepoStub{saved: RoutePolicy{ID: 7, SourceGroupID: 1, Candidates: []RouteCandidate{
		{ID: 70, PolicyID: 7, Role: RouteCandidatePrimary, TargetGroupID: 1, Enabled: true},
		{ID: 71, PolicyID: 7, Role: RouteCandidateFallback, TargetGroupID: 2, Enabled: true},
	}}}
	manager := newRoutePolicyManagerWithRepo(t, repo)
	saved, err := manager.Save(context.Background(), 1, repo.saved)
	require.NoError(t, err)
	require.Equal(t, []int64{70, 71}, []int64{saved.Candidates[0].ID, saved.Candidates[1].ID})
}
```

- [ ] **Step 2: Run manager tests and verify the V2 manager is undefined**

Run: `cd backend; go test ./internal/service -run TestRoutePolicyManager -count=1`

Expected: FAIL with undefined `RoutePolicyManager`.

- [ ] **Step 3: Implement defaults and validation from one backend source**

```go
type RoutePolicyDefaults struct {
	MaxRouteAttempts int `json:"max_route_attempts"`
	MaxAccountAttemptsPerRoute int `json:"max_account_attempts_per_route"`
	MaxTotalAttempts int `json:"max_total_attempts"`
	FailoverTimeoutMS int `json:"failover_timeout_ms"`
	FailureThreshold int `json:"failure_threshold"`
	FailureWindowSeconds int `json:"failure_window_seconds"`
	OpenCooldownSeconds int `json:"open_cooldown_seconds"`
	RecoverySuccessThreshold int `json:"recovery_success_threshold"`
	HalfOpenConcurrency int `json:"half_open_concurrency"`
	RouteStickyTTLSeconds int `json:"route_sticky_ttl_seconds"`
}

func DefaultRoutePolicyValues() RoutePolicyDefaults {
	p := DefaultRoutePolicy(0)
	return RoutePolicyDefaults{MaxRouteAttempts: p.MaxRouteAttempts,
		MaxAccountAttemptsPerRoute: p.MaxAccountAttemptsPerRoute,
		MaxTotalAttempts: p.MaxTotalAttempts, FailoverTimeoutMS: int(p.FailoverTimeout.Milliseconds()),
		FailureThreshold: p.FailureThreshold, FailureWindowSeconds: int(p.FailureWindow.Seconds()),
		OpenCooldownSeconds: int(p.OpenCooldown.Seconds()),
		RecoverySuccessThreshold: p.RecoverySuccessThreshold,
		HalfOpenConcurrency: p.HalfOpenConcurrency,
		RouteStickyTTLSeconds: int(p.RouteStickyTTL.Seconds())}
}
```

Validation rules are exact: source and targets exist, are active concrete groups, use the same platform and subscription type, have exactly one enabled primary pointing to the source group, contain no duplicate target group, use positive budgets with `max_total_attempts >= max_route_attempts`, and contain no blank mapping keys, blank mapping values, or duplicate normalized mapping keys. Phase 1 rejects a fallback target with `ClaudeCodeOnly=true`, because access-policy redirects are resolved once before health routing and an exact candidate must never redirect to a hidden group. Candidate IDs greater than zero must already belong to this policy; omitted IDs insert new candidates. `DefaultRoutePolicyValues` is only a JSON projection of `DefaultRoutePolicy`, not a second set of constants.

- [ ] **Step 4: Reload the last-valid snapshot only after a committed save/delete**

```go
func (m *RoutePolicyManager) Save(ctx context.Context, sourceGroupID int64, policy RoutePolicy) (*RoutePolicy, error) {
	policy.SourceGroupID = sourceGroupID
	applyRoutePolicyDefaults(&policy)
	candidateErrors, policyErrors, err := m.validateAll(ctx, policy)
	if err != nil { return nil, err }
	if err := joinRoutePolicyValidationErrors(policyErrors, candidateErrors); err != nil { return nil, err }
	if err := m.repo.SaveRoutePolicy(ctx, policy); err != nil { return nil, err }
	if err := m.snapshots.Reload(ctx); err != nil {
		return nil, fmt.Errorf("policy saved but snapshot reload failed: %w", err)
	}
	return m.repo.GetRoutePolicy(ctx, sourceGroupID)
}
```

On reload failure, the API returns an explicit error but the loader retains its previous snapshot. Record policy before/after candidate IDs in an admin audit `Extra` map; never include account credentials or proxy secrets.

Add `RoutePolicyManager.Preview(ctx, sourceGroupID, unsavedPolicy)` beside `Save` using the same `validateAll` helper that `Save` converts into a joined validation error:

```go
type RouteModelDecision struct {
	RequestedModel string `json:"requested_model"`
	EffectiveModel string `json:"effective_model"`
}
type RoutePolicyPreviewCandidate struct {
	CandidateID int64 `json:"candidate_id"`
	Role RouteCandidateRole `json:"role"`
	TargetGroupID int64 `json:"target_group_id"`
	Valid bool `json:"valid"`
	Errors []string `json:"errors"`
	ModelDecisions []RouteModelDecision `json:"model_decisions"`
}
type RoutePolicyPreview struct {
	Valid bool `json:"valid"`
	Errors []string `json:"errors"`
	Candidates []RoutePolicyPreviewCandidate `json:"candidates"`
}

func (m *RoutePolicyManager) Preview(ctx context.Context, sourceGroupID int64, policy RoutePolicy) (RoutePolicyPreview, error) {
	policy.SourceGroupID = sourceGroupID
	applyRoutePolicyDefaults(&policy)
	errorsByCandidate, policyErrors, err := m.validateAll(ctx, policy)
	if err != nil { return RoutePolicyPreview{}, err }
	preview := RoutePolicyPreview{Valid: len(policyErrors) == 0, Errors: policyErrors}
	for index, candidate := range policy.Candidates {
		candidateErrors := errorsByCandidate[index]
		item := RoutePolicyPreviewCandidate{CandidateID: candidate.ID, Role: candidate.Role,
			TargetGroupID: candidate.TargetGroupID, Valid: len(candidateErrors) == 0,
			Errors: candidateErrors}
		keys := sortedMappingKeys(candidate.ModelMapping)
		for _, requested := range keys {
			item.ModelDecisions = append(item.ModelDecisions, RouteModelDecision{RequestedModel: requested,
				EffectiveModel: candidate.ModelMapping[requested]})
		}
		preview.Valid = preview.Valid && item.Valid
		preview.Candidates = append(preview.Candidates, item)
	}
	return preview, nil
}
```

Candidate validation errors are returned as a slice aligned with input order so multiple new `id=0` candidates cannot overwrite one another. Preview resolves candidate group metadata through the admin repository but never saves, reloads the snapshot, selects an account, or mutates Redis. This is deliberately separate from `RouteExecutor.Preview`, which previews the currently persisted snapshot for shadow traffic.

- [ ] **Step 5: Replace the admin service dependency and regenerate Wire**

Change `adminServiceImpl.routeFailoverManager` from `*RouteFailoverManager` to `*RoutePolicyManager`. Keep method names `GetRouteFailoverPolicy`, `SaveRouteFailoverPolicy`, and `DeleteRouteFailoverPolicy` for handler compatibility, but change their Go types to V2 `RoutePolicy`. Remove the V1 planner/manager provider functions from the active service Wire set at this point; keep their source files, V1 repository implementation, and migration 192 compatibility data for one release, but construct none of them in the running server.

Run: `cd backend; go generate ./cmd/server`

Expected: no dependency cycle.

- [ ] **Step 6: Run manager and admin service tests**

Run: `cd backend; go test ./internal/service ./cmd/server -run 'TestRoutePolicyManager|TestAdmin.*Route' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit V2 management**

```bash
git add backend/internal/service/route_policy_manager.go backend/internal/service/route_policy_manager_test.go backend/internal/service/admin_service.go backend/internal/service/admin_group.go backend/internal/service/wire.go backend/cmd/server/wire_gen.go
git commit -m "feat: manage stable route failover policies"
```

### Task 4: Expose admin CRUD, defaults, preview, and runtime state

**Files:**
- Modify: `backend/internal/handler/admin/group_handler.go`
- Create: `backend/internal/handler/admin/group_route_failover_v2_test.go`
- Modify: `backend/internal/server/routes/admin.go`
- Modify: `backend/internal/server/api_contract_test.go`

- [ ] **Step 1: Write failing API contract tests**

```go
func TestRouteFailoverV2AdminContract(t *testing.T) {
	router, serviceStub := newRouteFailoverAdminRouter(t)

	defaults := httptest.NewRecorder()
	router.ServeHTTP(defaults, httptest.NewRequest(http.MethodGet, "/admin/groups/1/route-failover/defaults", nil))
	require.Equal(t, http.StatusOK, defaults.Code)
	require.JSONEq(t, `{"max_route_attempts":3,"max_account_attempts_per_route":2,"max_total_attempts":4,"failover_timeout_ms":20000,"failure_threshold":5,"failure_window_seconds":60,"open_cooldown_seconds":60,"recovery_success_threshold":2,"half_open_concurrency":1,"route_sticky_ttl_seconds":3600}`, unwrapData(defaults.Body.Bytes()))

	body := `{"enabled":true,"max_route_attempts":3,"max_account_attempts_per_route":2,"max_total_attempts":4,"failover_timeout_ms":20000,"failure_threshold":5,"failure_window_seconds":60,"open_cooldown_seconds":60,"recovery_success_threshold":2,"candidates":[{"id":70,"role":"primary","target_group_id":1,"priority":0,"enabled":true,"model_mapping":{}},{"id":71,"role":"fallback","target_group_id":2,"priority":1,"enabled":true,"model_mapping":{"client":"provider"}}]}`
	save := httptest.NewRecorder()
	router.ServeHTTP(save, jsonRequest(http.MethodPut, "/admin/groups/1/route-failover", body))
	require.Equal(t, http.StatusOK, save.Code)
	require.Equal(t, int64(71), serviceStub.saved.Candidates[1].ID)

	preview := httptest.NewRecorder()
	router.ServeHTTP(preview, jsonRequest(http.MethodPost, "/admin/groups/1/route-failover/preview", body))
	require.Equal(t, http.StatusOK, preview.Code)
	require.Equal(t, 1, serviceStub.previewCalls)
	require.Equal(t, 1, serviceStub.saveCalls)

	runtime := httptest.NewRecorder()
	router.ServeHTTP(runtime, httptest.NewRequest(http.MethodGet, "/admin/groups/1/route-failover/runtime", nil))
	require.Equal(t, http.StatusOK, runtime.Code)
	require.NotContains(t, runtime.Body.String(), "proxy")
}
```

- [ ] **Step 2: Run the API test and verify the old DTO rejects V2 fields**

Run: `cd backend; go test ./internal/handler/admin -run TestRouteFailoverV2AdminContract -count=1`

Expected: FAIL.

- [ ] **Step 3: Replace request/response DTOs with the V2 contract**

```go
type RouteCandidateRequest struct {
	ID int64 `json:"id"`
	Role service.RouteCandidateRole `json:"role" binding:"required,oneof=primary fallback"`
	TargetGroupID int64 `json:"target_group_id" binding:"required,gt=0"`
	Priority int `json:"priority" binding:"gte=0"`
	Enabled bool `json:"enabled"`
	ModelMapping map[string]string `json:"model_mapping"`
}

type RoutePolicyRequest struct {
	Enabled bool `json:"enabled"`
	MaxRouteAttempts int `json:"max_route_attempts" binding:"required,min=1,max=10"`
	MaxAccountAttemptsPerRoute int `json:"max_account_attempts_per_route" binding:"required,min=1,max=10"`
	MaxTotalAttempts int `json:"max_total_attempts" binding:"required,min=1,max=20"`
	FailoverTimeoutMS int `json:"failover_timeout_ms" binding:"required,min=1"`
	FailureThreshold int `json:"failure_threshold" binding:"required,min=1"`
	FailureWindowSeconds int `json:"failure_window_seconds" binding:"required,min=1"`
	OpenCooldownSeconds int `json:"open_cooldown_seconds" binding:"required,min=1"`
	RecoverySuccessThreshold int `json:"recovery_success_threshold" binding:"required,min=1"`
	Candidates []RouteCandidateRequest `json:"candidates" binding:"required,min=1,max=10,dive"`
}
```

- [ ] **Step 4: Register admin-only supporting endpoints**

```go
groups.GET("/:id/route-failover/defaults", h.Admin.Group.GetRouteFailoverDefaults)
groups.POST("/:id/route-failover/preview", h.Admin.Group.PreviewRouteFailoverPolicy)
groups.GET("/:id/route-failover/runtime", h.Admin.Group.GetRouteFailoverRuntime)
```

All routes remain under the existing admin middleware. Preview calls `RoutePolicyManager.Preview` on the supplied unsaved policy and returns candidate/model decisions without saving, selecting an account, acquiring a lease, or writing Redis. Add a contract assertion that preview leaves the config repository and Redis test double untouched. Runtime returns no credentials and no proxy secrets.

- [ ] **Step 5: Run handler and server contract tests**

Run: `cd backend; go test ./internal/handler/admin ./internal/server -run 'TestRouteFailoverV2AdminContract|TestAPIContract' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the admin API**

```bash
git add backend/internal/handler/admin/group_handler.go backend/internal/handler/admin/group_route_failover_v2_test.go backend/internal/server/routes/admin.go backend/internal/server/api_contract_test.go
git commit -m "feat: expose route failover v2 admin api"
```

### Task 5: Add route metrics and runtime diagnostics

**Files:**
- Create: `backend/internal/service/route_metrics.go`
- Create: `backend/internal/service/route_metrics_test.go`
- Modify: `backend/internal/service/route_executor.go`
- Modify: `backend/internal/service/route_state_resilient.go`
- Modify: `backend/internal/service/route_policy_manager.go`

- [ ] **Step 1: Write failing metrics and runtime tests**

```go
func TestRouteMetricsRecordsFallbackFailureAndLatency(t *testing.T) {
	m := &RouteMetrics{}
	m.RecordRequest(RouteExecutionAudit{FallbackUsed: true, RouteAttemptCount: 2, AccountAttemptCount: 3}, 12*time.Millisecond)
	m.RecordFailure(RouteFailure{Class: RouteFailureProvider, Reason: "provider_503"})
	m.RecordCandidateAttempt(RouteResolvedCandidate{PolicyID: 7, CandidateID: 9, Role: RouteCandidateFallback}, true, 8*time.Millisecond, 4*time.Millisecond)
	stats := m.Stats()
	require.Equal(t, int64(1), stats.Requests)
	require.Equal(t, int64(1), stats.Fallbacks)
	require.Equal(t, int64(2), stats.RouteAttempts)
	require.Equal(t, int64(3), stats.AccountAttempts)
	require.Equal(t, int64(1), stats.Failures["provider:provider_503"])
	require.Equal(t, int64(12), stats.AttemptDurationTotalMS)
	require.Equal(t, float64(1), stats.Candidates[RouteCandidateMetricKey{PolicyID: 7, CandidateID: 9}].SuccessRate)
	require.Equal(t, int64(4), stats.Candidates[RouteCandidateMetricKey{PolicyID: 7, CandidateID: 9}].AddedLatencyMS)
}

func TestRouteRuntimeReportsSnapshotAndRedisHealth(t *testing.T) {
	runtime := BuildRouteRuntimeStatus(snapshotAt(time.Now().Add(-2*time.Minute)), RouteStateStats{Mode: RouteStateDegraded, StoreErrors: 1, LastErrorAt: time.Now()}, RouteMetricsStats{})
	require.GreaterOrEqual(t, runtime.SnapshotAgeSeconds, int64(120))
	require.Equal(t, "degraded", runtime.StateStoreStatus)
}
```

- [ ] **Step 2: Run metrics tests and verify the collector is undefined**

Run: `cd backend; go test ./internal/service -run 'TestRouteMetrics|TestRouteRuntime' -count=1`

Expected: FAIL.

- [ ] **Step 3: Implement atomic counters with labeled failure snapshots**

```go
type RouteMetrics struct {
	requests atomic.Int64
	fallbacks atomic.Int64
	routeAttempts atomic.Int64
	accountAttempts atomic.Int64
	attemptDurationTotalMS atomic.Int64
	stickyHits atomic.Int64
	circuitTransitions atomic.Int64
	mu sync.RWMutex
	failures map[string]int64
	candidates sync.Map // map[RouteCandidateMetricKey]*routeCandidateMetric
}

type RouteCandidateMetricKey struct { PolicyID, CandidateID int64 }
type routeCandidateMetric struct {
	attempts atomic.Int64
	successes atomic.Int64
	fallbackAttempts atomic.Int64
	attemptDurationTotalMS atomic.Int64
	addedLatencyTotalMS atomic.Int64
}

func (m *RouteMetrics) RecordFailure(f RouteFailure) {
	key := string(f.Class) + ":" + strings.TrimSpace(f.Reason)
	m.mu.Lock()
	if m.failures == nil { m.failures = make(map[string]int64) }
	m.failures[key]++
	m.mu.Unlock()
}

func (m *RouteMetrics) RecordCandidateAttempt(route RouteResolvedCandidate, success bool, attemptDuration, addedLatency time.Duration) {
	key := RouteCandidateMetricKey{PolicyID: route.PolicyID, CandidateID: route.CandidateID}
	value, _ := m.candidates.LoadOrStore(key, &routeCandidateMetric{})
	metric := value.(*routeCandidateMetric)
	metric.attempts.Add(1)
	if success { metric.successes.Add(1) }
	if route.Role == RouteCandidateFallback { metric.fallbackAttempts.Add(1) }
	metric.attemptDurationTotalMS.Add(attemptDuration.Milliseconds())
	metric.addedLatencyTotalMS.Add(addedLatency.Milliseconds())
}
```

Expose the approved metrics as JSON names: `route_requests_total`, `route_fallback_total`, `route_attempts_total`, `route_attempt_duration`, `route_circuit_state`, `route_circuit_transitions_total`, `route_failures_total`, `route_sticky_hits_total`, `route_policy_snapshot_age`, and `route_state_store_errors_total`. Per-candidate success rate is `successes / attempts`; fallback rate is that fallback candidate's attempts divided by total executor requests; added latency is the average elapsed time from executor entry until that candidate's real upstream attempt begins. Zero denominators return zero, never NaN.

- [ ] **Step 4: Record metrics at one executor boundary**

Use a deferred recorder in `RouteExecutor.Execute` so every exit path is counted once, and time each real `Forward` call for the per-candidate record. Increment circuit transitions only from `RouteCircuitTransition{Changed:true}` returned by permit/success/failure operations, which prevents multiple instances from double-counting a state change. Store last failure reason/time per candidate in Redis circuit hashes. For the runtime endpoint, inspect each configured candidate key directly and call bounded `ListCircuits("route:capability:{policy}:{candidate}:", 500)` for its capability states; this read-only scan never runs on the request path. Merge those global states with the local metric snapshot and return no raw sticky/session keys.

- [ ] **Step 5: Run metrics and executor tests with race detection**

Run: `cd backend; go test -race ./internal/service -run 'TestRouteMetrics|TestRouteRuntime|TestRouteExecutor' -count=1`

Expected: PASS with no races.

- [ ] **Step 6: Commit observability**

```bash
git add backend/internal/service/route_metrics.go backend/internal/service/route_metrics_test.go backend/internal/service/route_executor.go backend/internal/service/route_state_resilient.go backend/internal/service/route_policy_manager.go
git commit -m "feat: expose route failover runtime metrics"
```

### Task 6: Upgrade frontend types and API calls

**Files:**
- Modify: `frontend/src/types/index.ts`
- Modify: `frontend/src/api/admin/groups.ts`
- Modify: `frontend/src/api/__tests__/admin.groups.routeFailover.spec.ts`

- [ ] **Step 1: Replace the API test fixture with the V2 contract**

```ts
it('uses the V2 policy, defaults, preview, and runtime endpoints', async () => {
  mockedClient.get.mockResolvedValue({ data: { max_route_attempts: 3 } })
  await groups.getRouteFailoverDefaults(1)
  expect(mockedClient.get).toHaveBeenCalledWith('/admin/groups/1/route-failover/defaults')

  mockedClient.post.mockResolvedValue({ data: { valid: true, errors: [], candidates: [] } })
  await groups.previewRouteFailoverPolicy(1, policyInput)
  expect(mockedClient.post).toHaveBeenCalledWith('/admin/groups/1/route-failover/preview', policyInput)

  mockedClient.get.mockResolvedValue({ data: { snapshot_version: 1, snapshot_age_seconds: 0, state_store_status: 'healthy', route_state_store_errors_total: 0, candidates: [] } })
  await groups.getRouteFailoverRuntime(1)
  expect(mockedClient.get).toHaveBeenCalledWith('/admin/groups/1/route-failover/runtime')
})
```

- [ ] **Step 2: Run the API test and verify the new methods are missing**

Run: `cd frontend; pnpm exec vitest run src/api/__tests__/admin.groups.routeFailover.spec.ts`

Expected: FAIL with missing API exports/types.

- [ ] **Step 3: Define the V2 frontend contract exactly once**

```ts
export type RouteCandidateRole = 'primary' | 'fallback'

export interface RouteCandidateInput {
  id: number
  role: RouteCandidateRole
  target_group_id: number
  priority: number
  enabled: boolean
  model_mapping: Record<string, string>
}

export interface RoutePolicyInput {
  enabled: boolean
  max_route_attempts: number
  max_account_attempts_per_route: number
  max_total_attempts: number
  failover_timeout_ms: number
  failure_threshold: number
  failure_window_seconds: number
  open_cooldown_seconds: number
  recovery_success_threshold: number
  candidates: RouteCandidateInput[]
}

export interface RoutePolicy extends RoutePolicyInput {
  id: number
  source_group_id: number
}

export interface RouteRuntimeCandidate {
  candidate_id: number
  state: 'closed' | 'open' | 'half_open'
  capability_states: Record<string, 'closed' | 'open' | 'half_open'>
  last_failure_reason?: string
  last_failure_at?: string
  recovery_at?: string
  success_rate: number
  fallback_rate: number
  added_latency_ms: number
}

export type RoutePolicyDefaults = Omit<RoutePolicyInput, 'enabled' | 'candidates'> & {
  half_open_concurrency: number
  route_sticky_ttl_seconds: number
}

export interface RoutePolicyPreviewCandidate {
  candidate_id: number
  role: RouteCandidateRole
  target_group_id: number
  valid: boolean
  errors: string[]
  model_decisions: Array<{ requested_model: string; effective_model: string }>
}

export interface RoutePolicyPreview {
  valid: boolean
  errors: string[]
  candidates: RoutePolicyPreviewCandidate[]
}

export interface RouteRuntimeStatus {
  snapshot_version: number
  snapshot_age_seconds: number
  state_store_status: 'healthy' | 'degraded'
  route_state_store_errors_total: number
  candidates: RouteRuntimeCandidate[]
}
```

`half_open_concurrency` and `route_sticky_ttl_seconds` are read-only backend constants exposed by defaults/runtime, so they are intentionally absent from `RoutePolicyInput`. Remove old `max_attempts`, `success_threshold`, `window_seconds`, `half_open_lease_seconds`, and `targets` types so the modal cannot accidentally submit V1.

- [ ] **Step 4: Add typed API methods**

```ts
export async function getRouteFailoverDefaults(id: number): Promise<RoutePolicyDefaults> {
  const { data } = await apiClient.get<RoutePolicyDefaults>(`/admin/groups/${id}/route-failover/defaults`)
  return data
}
export async function previewRouteFailoverPolicy(id: number, policy: RoutePolicyInput): Promise<RoutePolicyPreview> {
  const { data } = await apiClient.post<RoutePolicyPreview>(`/admin/groups/${id}/route-failover/preview`, policy)
  return data
}
export async function getRouteFailoverRuntime(id: number): Promise<RouteRuntimeStatus> {
  const { data } = await apiClient.get<RouteRuntimeStatus>(`/admin/groups/${id}/route-failover/runtime`)
  return data
}
```

- [ ] **Step 5: Run API tests and typecheck**

Run: `cd frontend; pnpm exec vitest run src/api/__tests__/admin.groups.routeFailover.spec.ts; pnpm run typecheck`

Expected: PASS.

- [ ] **Step 6: Commit frontend contracts**

```bash
git add frontend/src/types/index.ts frontend/src/api/admin/groups.ts frontend/src/api/__tests__/admin.groups.routeFailover.spec.ts
git commit -m "feat: add route failover v2 frontend api"
```

### Task 7: Rebuild the admin modal around policy, candidates, and runtime

**Files:**
- Modify: `frontend/src/components/admin/group/RouteFailoverModal.vue`
- Modify: `frontend/src/components/admin/group/__tests__/RouteFailoverModal.spec.ts`
- Modify: `frontend/src/i18n/locales/zh/admin/overview.ts`
- Modify: `frontend/src/i18n/locales/en/admin/overview.ts`
- Modify: `frontend/src/views/admin/GroupsView.vue`

- [ ] **Step 1: Write failing UI behavior tests**

```ts
it('loads backend defaults and renders an immutable primary candidate', async () => {
  getDefaults.mockResolvedValue(defaults)
  getPolicy.mockRejectedValue({ status: 404, code: 'ROUTE_FAILOVER_POLICY_NOT_FOUND' })
  const wrapper = mountModal()
  await flushPromises()
  expect(wrapper.get('[data-testid="route-max-route-attempts"]').element).toHaveProperty('value', '3')
  expect(wrapper.get('[data-testid="route-failure-threshold"]').element).toHaveProperty('value', '5')
  expect(wrapper.get('[data-testid="route-primary-group"]').text()).toContain('Primary')
  expect(wrapper.find('[data-testid="route-remove-primary"]').exists()).toBe(false)
})

it('preserves candidate ids and previews before enabling', async () => {
  getDefaults.mockResolvedValue(defaults)
  getPolicy.mockResolvedValue(policyWithCandidateIds)
  previewPolicy.mockResolvedValue({ valid: true, errors: [], candidates: [{
    candidate_id: 71, role: 'fallback', target_group_id: 2, valid: true, errors: [],
    model_decisions: [{ requested_model: 'client-model', effective_model: 'provider-model' }]
  }] })
  const wrapper = mountModal()
  await flushPromises()
  await wrapper.get('[data-testid="route-preview"]').trigger('click')
  await flushPromises()
  expect(previewPolicy).toHaveBeenCalledWith(1, expect.objectContaining({
    candidates: expect.arrayContaining([expect.objectContaining({ id: 71 })])
  }))
})

it('shows runtime state without exposing it to ordinary group forms', async () => {
  const wrapper = mountModal()
  await flushPromises()
  await wrapper.get('[data-testid="route-tab-runtime"]').trigger('click')
  await flushPromises()
  expect(getRuntime).toHaveBeenCalledWith(1)
  expect(wrapper.get('[data-testid="route-state-71"]').text()).toContain('closed')
})
```

- [ ] **Step 2: Run the modal tests and verify they fail against V1 UI**

Run: `cd frontend; pnpm exec vitest run src/components/admin/group/__tests__/RouteFailoverModal.spec.ts`

Expected: FAIL.

- [ ] **Step 3: Build three compact tabs and backend-owned defaults**

Use a segmented tab control with `basic`, `candidates`, and `runtime`. Basic contains the enable switch, total timeout, the three accurately named attempt budgets, and a compact advanced circuit section for failure threshold, failure window, open cooldown, and recovery-success threshold. Candidates always renders the source group as a primary row with `role: primary`, `target_group_id: source.id`, and its server-provided ID when editing. Fallback rows support ordering, enable state, group selection, and model mappings; the selector contains only active, non-composite groups with the source platform and subscription type. Runtime is read-only and refreshes only when opened or when the refresh icon is clicked. Half-open concurrency and sticky TTL remain backend-owned read-only defaults in Phase 1.

```ts
const emptyPolicy = (d: RoutePolicyDefaults, group: AdminGroup): RoutePolicyInput => ({
  enabled: false,
  max_route_attempts: d.max_route_attempts,
  max_account_attempts_per_route: d.max_account_attempts_per_route,
  max_total_attempts: d.max_total_attempts,
  failover_timeout_ms: d.failover_timeout_ms,
  failure_threshold: d.failure_threshold,
  failure_window_seconds: d.failure_window_seconds,
  open_cooldown_seconds: d.open_cooldown_seconds,
  recovery_success_threshold: d.recovery_success_threshold,
  candidates: [{ id: 0, role: 'primary', target_group_id: group.id, priority: 0, enabled: true, model_mapping: {} }]
})
```

- [ ] **Step 4: Add validation and enable confirmation**

The save button remains enabled for disabled policies. Enabling requires a successful preview from the current form version; any edit invalidates the preview. Reject duplicate groups/mappings, non-concrete source groups, missing primary, disabled primary, invalid budgets, and `max_total_attempts < max_route_attempts` before the API call. Show inline errors; use existing app-store notifications only for request failures/success.

- [ ] **Step 5: Add runtime rows and translations**

Each candidate row shows circuit state, last failure, recovery time, success rate, fallback rate, and added latency using compact labels. Do not add internal IDs to ordinary user pages. Add equivalent Chinese and English strings for every visible label and error. Keep cards at or below 8px radius and do not nest cards.

- [ ] **Step 6: Run targeted frontend verification**

Run: `cd frontend; pnpm exec vitest run src/components/admin/group/__tests__/RouteFailoverModal.spec.ts src/api/__tests__/admin.groups.routeFailover.spec.ts src/views/admin/__tests__/GroupsView.duplicate.spec.ts`

Expected: PASS.

Run: `cd frontend; pnpm run typecheck; pnpm run build`

Expected: PASS.

- [ ] **Step 7: Visually verify desktop and mobile layouts**

Run: `cd frontend; pnpm run dev -- --host 127.0.0.1 --port 4178`

Expected: Vite reports `http://127.0.0.1:4178/`. At 1440x900 and 390x844, open the route failover modal and verify no overlapping labels, clipped candidate controls, nested cards, or horizontal page overflow. Verify Runtime refresh and all three tabs with both light and dark themes.

- [ ] **Step 8: Commit the admin UI**

```bash
git add frontend/src/components/admin/group/RouteFailoverModal.vue frontend/src/components/admin/group/__tests__/RouteFailoverModal.spec.ts frontend/src/i18n/locales/zh/admin/overview.ts frontend/src/i18n/locales/en/admin/overview.ts frontend/src/views/admin/GroupsView.vue
git commit -m "feat: add route failover v2 admin console"
```

### Task 8: Document configuration and add a read-only verifier

**Files:**
- Modify: `deploy/config.example.yaml`
- Modify: `deploy/.env.example`
- Create: `tools/route_failover_v2_verify.py`
- Create: `tools/test_route_failover_v2_verify.py`
- Create: `ops/route-failover-v2/README.md`

- [ ] **Step 1: Write failing verifier tests**

```python
import unittest

from tools.route_failover_v2_verify import build_parser, validate_environment

class RouteFailoverV2VerifyTest(unittest.TestCase):
    def test_verifier_rejects_production_mutation_flags(self):
        parser = build_parser()
        args = parser.parse_args(["--base-url", "http://47.119.114.46:8080", "--admin-token-env", "TOKEN", "--group-id", "1", "--environment-role", "production"])
        self.assertFalse(hasattr(args, "enable"))
        self.assertFalse(hasattr(args, "save"))

    def test_verifier_requires_expected_environment_role(self):
        with self.assertRaises(SystemExit):
            validate_environment("http://47.119.114.46:8080", "test")
        validate_environment("http://172.16.22.73:8080", "test")
        validate_environment("http://47.119.114.46:8080", "production")
```

- [ ] **Step 2: Run verifier tests and verify the tool is missing**

Run: `python -m unittest tools.test_route_failover_v2_verify -v`

Expected: FAIL because `route_failover_v2_verify` does not exist.

- [ ] **Step 3: Add documented disabled defaults**

```yaml
gateway:
  route_failover_v2:
    mode: off
    snapshot_refresh_seconds: 60
    redis_local_state_ttl_seconds: 30
    redis_degraded_max_total_attempts: 2
    debug_headers: false
```

Add matching environment examples with `GATEWAY_ROUTE_FAILOVER_V2_MODE=off`. State explicitly that 46 must keep debug headers false and may move from off only after 73 acceptance.

- [ ] **Step 4: Implement a read-only verifier**

```python
def verify(base_url: str, token: str, group_id: int) -> dict[str, object]:
    headers = {"Authorization": f"Bearer {token}"}
    paths = {
        "health": "/health",
        "policy": f"/api/v1/admin/groups/{group_id}/route-failover",
        "runtime": f"/api/v1/admin/groups/{group_id}/route-failover/runtime",
    }
    result: dict[str, object] = {}
    for name, path in paths.items():
        request = urllib.request.Request(base_url.rstrip("/") + path, headers=headers)
        with urllib.request.urlopen(request, timeout=10) as response:
            result[name] = json.load(response)
    return result
```

The CLI accepts `--base-url`, `--admin-token-env`, `--group-id`, and `--environment-role test|production`. It performs GET only, redacts the token, and exits nonzero when health is bad, snapshot age exceeds two refresh intervals, Redis is degraded unexpectedly, or the host does not match the declared role.

- [ ] **Step 5: Write the environment runbook**

The runbook fixes roles as:

```text
172.16.22.73 = test
47.119.114.46 = production
```

It includes migration backup checks, `off -> shadow -> enforce` gates, fault-injection ownership, abort thresholds, and the rule that no 46 change occurs from `tools/deploy_sub2api_local_73.py`. It also states WARP/DNS/Docker failover is infrastructure-level and must not be simulated by opening business route circuits.

- [ ] **Step 6: Run verifier tests**

Run: `python -m unittest tools.test_route_failover_v2_verify -v`

Expected: PASS.

- [ ] **Step 7: Commit configuration and runbook**

```bash
git add deploy/config.example.yaml deploy/.env.example tools/route_failover_v2_verify.py tools/test_route_failover_v2_verify.py ops/route-failover-v2/README.md
git commit -m "docs: add route failover rollout controls"
```

### Task 9: Validate on 73 before any production enablement

**Files:**
- Operational execution only; do not edit 46.

- [ ] **Step 1: Run all local release gates on Go 1.26.5**

Run: `cd backend; go test -race ./internal/service ./internal/repository ./internal/handler ./internal/handler/admin ./internal/server -count=1 -timeout=30m`

Expected: PASS.

Run: `cd frontend; pnpm exec vitest run src/components/admin/group/__tests__/RouteFailoverModal.spec.ts src/api/__tests__/admin.groups.routeFailover.spec.ts; pnpm run typecheck; pnpm run build`

Expected: PASS.

- [ ] **Step 2: Deploy the local build only to test server 73 with mode off**

Prerequisite: set `SUB2API_SSH_PASSWORD` in the secure shell environment without printing it.

Run: `python tools/deploy_sub2api_local_73.py --tag route-failover-v2-off`

Expected: the script reports a healthy `sub2api` container on 172.16.22.73 and does not connect to 47.119.114.46.

- [ ] **Step 3: Verify migrations and disabled behavior on 73**

Run: `python tools/route_failover_v2_verify.py --base-url http://172.16.22.73:8080 --admin-token-env SUB2API_ADMIN_TOKEN --group-id $env:ROUTE_V2_TEST_GROUP_ID --environment-role test`

Expected: health OK, snapshot loaded, mode off, no Redis degradation, and no enabled production-like policy.

- [ ] **Step 4: Run shadow mode on dedicated test groups**

Set `GATEWAY_ROUTE_FAILOVER_V2_MODE=shadow` in 73's Compose environment and recreate only the `sub2api` service. Generate representative Anthropic, OpenAI-compatible, Gemini, OpenAI native, and Grok text traffic. Expected: client/account behavior matches legacy, preview metrics populate, no half-open leases are created by shadow traffic, and P95 overhead is effectively zero relative to the off baseline.

- [ ] **Step 5: Run enforce fault injection on dedicated 73 groups**

Inject these cases one at a time: primary account 401/429, primary provider 503, primary DNS/TCP failure, malformed request, Redis unavailable, PostgreSQL unavailable after a valid snapshot, WARP outage, client disconnect, and semantic mid-stream failure. Expected results match the design matrix: only safe provider/route failures cross routes; account/request/infrastructure failures do not poison route circuits; Redis degradation emits an alert and caps total attempts at two; PostgreSQL uses the last snapshot; no streams concatenate.

- [ ] **Step 6: Check 73 acceptance thresholds**

Required results:

```text
enabled route decision overhead P95 <= 5 ms excluding upstream time
policy/candidate PostgreSQL queries on request path = 0
duplicate semantic output = 0
billing source-group mismatches = 0
missing effective route audit on successful fallback = 0
distributed half-open concurrent probes per health key <= 1
```

- [ ] **Step 7: Return 73 to off if any threshold fails**

Set `GATEWAY_ROUTE_FAILOVER_V2_MODE=off`, recreate only `sub2api`, and rerun the verifier. Expected: requests use legacy routing; additive data remains intact for diagnosis.

### Task 10: Gate production rollout on 46

**Files:**
- Operational execution only; requires a separate explicit production approval after Task 9 evidence is reviewed.

- [ ] **Step 1: Capture the exact approved image and database backup**

Record the image digest tested on 73. Back up 46 PostgreSQL before deploying. Expected: a restorable backup artifact and checksum are recorded in the change ticket; 46 remains production and 73 remains test.

- [ ] **Step 2: Deploy the same digest to 46 with mode off**

Deploy through the production Docker workflow, not `tools/deploy_sub2api_local_73.py`. Expected: migrations 193/194 apply, health passes, route mode remains off, and ordinary traffic is unchanged.

- [ ] **Step 3: Verify 46 read-only runtime state**

Run: `python tools/route_failover_v2_verify.py --base-url http://47.119.114.46:8080 --admin-token-env SUB2API_ADMIN_TOKEN --group-id $env:ROUTE_V2_PRODUCTION_GROUP_ID --environment-role production`

Expected: correct production host, healthy snapshot/Redis, debug headers false, and no customer policy enabled.

- [ ] **Step 4: Enable one internal group and observe one peak period**

Move 46 to enforce only after the internal policy has passed preview. Observe errors, latency, fallback rate, route failures, Redis errors, billing identity, and usage audit through one complete peak traffic period. Expected: all acceptance thresholds remain within the approved gate.

- [ ] **Step 5: Roll back by mode, not database rollback**

On any threshold breach, set mode to off and recreate only the application container. Expected: legacy routing resumes immediately; migrations and audit columns remain; no migration file is edited or reverted.

- [ ] **Step 6: Expand incrementally and retain compatibility for one release**

Enable low-risk groups in small batches. Keep V1 tables, additive V2 tables, and old compatibility code for at least one stable release. Do not add composite groups, media, asynchronous tasks, or WebSocket until their separate plans pass.
