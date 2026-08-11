# API Key Route Failover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let each API key own an ordered list of up to five fallback groups, route retry-safe failures through that list with one-hour session stickiness, and bill and audit the group that actually completes the request.

**Architecture:** Persist fallback targets as normalized API-key children and load their complete group snapshots into the existing API-key authentication cache. Replace the administrator-policy planner with an immutable per-request route plan that orders primary/sticky/fallback candidates, resolves each candidate's subscription and RPM context, and coordinates Redis stickiness and group/model circuits. Protocol handlers keep their existing group-local account retry loops but advance the cross-group plan only after a group is exhausted or returns a replay-safe retryable failure.

**Tech Stack:** Go 1.26, Ent, PostgreSQL, Redis/Lua, Gin, Wire, Vue 3, TypeScript, Vitest, Testify, sqlmock/miniredis.

---

## Constraints and Safety Boundary

- Production is `47.119.114.46`; testing is `172.16.22.73`.
- No deployment, schema change, data import, account mutation, or fault injection is performed on production.
- The production host is read only during the final acceptance import of accounts `2909`, `3448`, and `3449`.
- The three imported accounts, temporary user, temporary API key, and test groups live only on `172.16.22.73` and are cleaned up after verification.
- `new-api` at `172.16.22.73:3001` is a separate service and is not changed or queried by this work.
- Existing untracked files under `data/`, `ops/`, and `tools/` are user-owned. Do not add, remove, or commit them. The local acceptance scripts may be used as test harnesses but remain untracked.
- Legacy tables `route_failover_policies` and `route_failover_targets` remain in PostgreSQL for one stable release. Runtime code and HTTP routes stop reading and writing them.

## File Responsibility Map

### Persistent model and repository

- Create `backend/migrations/221_api_key_route_failover.sql`: additive API-key route fields, normalized target table, and usage audit fields.
- Create `backend/ent/schema/api_key_route_failover_target.go`: Ent definition for ordered fallback targets.
- Modify `backend/ent/schema/api_key.go`: route configuration version, risk acknowledgement, and target edge.
- Modify `backend/ent/schema/usage_log.go`: source/effective route audit fields.
- Regenerate `backend/ent/`: generated entities, builders, predicates, migration schema, client, mutation, runtime, and transaction types.
- Modify `backend/internal/repository/api_key_repo.go`: transactionally save/load ordered targets and invalidate keys that reference a group as either primary or fallback.
- Modify `backend/internal/repository/usage_log_repo_insert.go` and usage query mappers: dual-write and read route audit columns.

### Domain, routing, and billing

- Modify `backend/internal/service/api_key.go` and `backend/internal/service/api_key_service.go`: route domain types, request semantics, validation, acknowledgement, and version increments.
- Modify `backend/internal/service/api_key_auth_cache.go` and `backend/internal/service/api_key_auth_cache_impl.go`: cached target group snapshots and per-target RPM overrides.
- Rewrite `backend/internal/service/route_failover.go`: per-API-key candidate plan, fixed failure classification, replay guard, switch deadline, and route audit.
- Create `backend/internal/service/route_failover_sticky.go`: route-stickiness interface and fail-open helpers.
- Modify `backend/internal/repository/route_failover_circuit.go`: fixed circuit keyed by effective group plus requested model.
- Create `backend/internal/repository/route_failover_sticky.go`: Redis one-hour sliding route binding.
- Modify `backend/internal/service/billing_cache_service.go`: candidate-specific admission without counting user-global RPM more than once.
- Modify `backend/internal/service/gateway_scheduling.go` and `backend/internal/service/openai_account_scheduler.go`: group-local selection only; no administrator-policy traversal.
- Modify `backend/internal/service/gateway_usage_billing.go` and `backend/internal/service/openai_gateway_usage.go`: price and charge using the effective API key and subscription.

### Protocol boundaries and audit

- Create `backend/internal/handler/route_execution.go`: shared handler adapter for candidate admission, group-local retry state, semantic-output guard, and route outcome recording.
- Modify `backend/internal/handler/gateway_handler.go`, `gateway_handler_chat_completions.go`, `gateway_handler_responses.go`, `gateway_web_search.go`, and `gemini_v1beta_handler.go`: Anthropic/Gemini-compatible route plans.
- Modify `backend/internal/handler/openai_gateway_handler.go`, `openai_chat_completions.go`, and `openai_images.go`: OpenAI route plans.
- Modify DTO files and usage tables so `group_id` is the effective group and `source_group_id` is visible for failover audit.

### Retired administrator surface

- Modify `backend/internal/server/routes/admin.go`, `backend/internal/handler/admin/group_handler.go`, `backend/internal/service/admin_group.go`, `backend/internal/service/admin_service.go`, `backend/internal/service/wire.go`, and `backend/internal/repository/wire.go`: remove administrator policy endpoints and providers.
- Delete `backend/internal/service/route_failover_manager.go` and its manager test after replacement tests pass.
- Delete `frontend/src/components/admin/group/RouteFailoverModal.vue` and its test.
- Modify `frontend/src/api/admin/groups.ts`, `frontend/src/types/index.ts`, and `frontend/src/views/admin/GroupsView.vue`: remove legacy policy API and action.

### User surface

- Modify `frontend/src/api/keys.ts` and `frontend/src/types/index.ts`: API-key route request and response contract.
- Create `frontend/src/components/keys/RouteFailoverEditor.vue`: toggle, ordered selector, maximum-five enforcement, removal/reordering controls, and risk confirmation.
- Modify `frontend/src/views/user/KeysView.vue`: create/edit integration and `Primary + N fallbacks` display.
- Modify English and Chinese dashboard locale files.
- Modify user/admin usage views to display `Primary -> Effective` only when a fallback was used.

## Stable Domain Contract

Use these names consistently throughout all tasks:

```go
const MaxAPIKeyFallbackGroups = 5

type APIKeyFailoverTarget struct {
	ID                   int64
	APIKeyID             int64
	TargetGroupID        int64
	Priority             int
	Group                *Group
	UserGroupRPMOverride *int
	EffectiveRateMultiplier float64
}

type APIKeyRouteMutation struct {
	ReplaceTargets            bool
	TargetGroupIDs            []int64
	IncrementVersion          bool
	RiskAcknowledgedAt        *time.Time
	ClearRiskAcknowledgement  bool
}

type RouteUsageAudit struct {
	SourceGroupID       *int64
	FallbackUsed        bool
	AttemptCount        int
	FallbackReason      string
	StickyHit           bool
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
```

The API contract is:

```json
{
  "fallback_group_ids": [12, 18, 23],
  "failover_risk_acknowledged": true
}
```

`UpdateAPIKeyRequest.FallbackGroupIDs` is `*[]int64`; `nil` means omitted and a non-nil empty slice means disable. When `GroupID` changes and `FallbackGroupIDs` is nil, replace the chain with empty. Response targets use `fallback_groups`, never raw target-table rows.

### Task 1: Add the additive database and Ent model

**Files:**
- Create: `backend/migrations/221_api_key_route_failover.sql`
- Create: `backend/ent/schema/api_key_route_failover_target.go`
- Modify: `backend/ent/schema/api_key.go`
- Modify: `backend/ent/schema/usage_log.go`
- Create: `backend/ent/schema/api_key_route_failover_schema_test.go`
- Create: `backend/internal/repository/migrations_api_key_route_failover_test.go`
- Modify: `backend/internal/repository/migrations_schema_integration_test.go`
- Regenerate: `backend/ent/`

- [ ] **Step 1: Write the failing schema test**

```go
func fieldNames(fields []ent.Field) []string {
	names := make([]string, 0, len(fields))
	for _, item := range fields {
		names = append(names, item.Descriptor().Name)
	}
	return names
}

func TestAPIKeyRouteFailoverSchemaFields(t *testing.T) {
	apiKeyFields := fieldNames((schema.APIKey{}).Fields())
	require.Contains(t, apiKeyFields, "route_config_version")
	require.Contains(t, apiKeyFields, "failover_risk_acknowledged_at")

	usageFields := fieldNames((schema.UsageLog{}).Fields())
	for _, name := range []string{"source_group_id", "route_fallback_used", "route_attempt_count", "route_fallback_reason", "route_sticky_hit"} {
		require.Contains(t, usageFields, name)
	}

	targetFields := fieldNames((schema.APIKeyRouteFailoverTarget{}).Fields())
	require.Equal(t, []string{"api_key_id", "target_group_id", "priority"}, targetFields)
}

func TestMigration221DoesNotCopyLegacyPolicies(t *testing.T) {
	body, err := os.ReadFile("../../migrations/221_api_key_route_failover.sql")
	require.NoError(t, err)
	sqlText := string(body)
	require.NotContains(t, sqlText, "FROM route_failover_policies")
	require.NotContains(t, sqlText, "FROM route_failover_targets")
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run from `backend`: `go test ./ent/schema -run TestAPIKeyRouteFailoverSchemaFields -count=1`

Expected: FAIL because `APIKeyRouteFailoverTarget` and the new fields do not exist.

- [ ] **Step 3: Add the schema and migration**

The new Ent schema must contain exactly these persisted fields and constraints:

```go
func (APIKeyRouteFailoverTarget) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("api_key_id"),
		field.Int64("target_group_id"),
		field.Int("priority").Min(1).Max(5),
	}
}

func (APIKeyRouteFailoverTarget) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("api_key_id", "target_group_id").Unique(),
		index.Fields("api_key_id", "priority").Unique(),
		index.Fields("target_group_id"),
	}
}
```

Use `mixins.TimeMixin{}` and table annotation `api_key_route_failover_targets`. Add the API-key edge with `ON DELETE CASCADE`; the target-group FK also uses `ON DELETE CASCADE`.

The migration body is:

```sql
ALTER TABLE api_keys
  ADD COLUMN IF NOT EXISTS route_config_version BIGINT NOT NULL DEFAULT 1,
  ADD COLUMN IF NOT EXISTS failover_risk_acknowledged_at TIMESTAMPTZ NULL;

CREATE TABLE IF NOT EXISTS api_key_route_failover_targets (
  id BIGSERIAL PRIMARY KEY,
  api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
  target_group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  priority SMALLINT NOT NULL CHECK (priority BETWEEN 1 AND 5),
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (api_key_id, target_group_id),
  UNIQUE (api_key_id, priority)
);

CREATE INDEX IF NOT EXISTS api_key_route_failover_targets_group_idx
  ON api_key_route_failover_targets (target_group_id);

ALTER TABLE usage_logs
  ADD COLUMN IF NOT EXISTS source_group_id BIGINT NULL,
  ADD COLUMN IF NOT EXISTS route_fallback_used BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS route_attempt_count SMALLINT NOT NULL DEFAULT 1,
  ADD COLUMN IF NOT EXISTS route_fallback_reason VARCHAR(64) NULL,
  ADD COLUMN IF NOT EXISTS route_sticky_hit BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS usage_logs_source_group_created_idx
  ON usage_logs (source_group_id, created_at DESC)
  WHERE source_group_id IS NOT NULL;
```

Do not copy rows from either legacy route table.

- [ ] **Step 4: Regenerate Ent and run schema tests**

Run from `backend`:

```bash
go generate ./ent
gofmt -w ent/schema/api_key.go ent/schema/api_key_route_failover_target.go ent/schema/usage_log.go ent/schema/api_key_route_failover_schema_test.go
go test ./ent/schema -run TestAPIKeyRouteFailoverSchemaFields -count=1
go test ./internal/repository -run TestMigration221DoesNotCopyLegacyPolicies -count=1
go test -tags=integration ./internal/repository -run TestMigrationsRunner_IsIdempotent_AndSchemaIsUpToDate -count=1
```

Expected: PASS. Extend the integration schema test to assert every new API-key/usage column, both unique target indexes, both cascading foreign keys, and that existing API keys have version `1` with zero target rows. Inspect generated migration schema and confirm both unique target indexes exist.

- [ ] **Step 5: Commit**

```bash
git add backend/migrations/221_api_key_route_failover.sql backend/ent backend/internal/repository/migrations_api_key_route_failover_test.go backend/internal/repository/migrations_schema_integration_test.go
git commit -m "feat: add API key route failover schema"
```

### Task 2: Add route domain fields and transactional repository operations

**Files:**
- Modify: `backend/internal/service/api_key.go`
- Modify: `backend/internal/service/api_key_service.go`
- Modify: `backend/internal/repository/api_key_repo.go`
- Create: `backend/internal/repository/api_key_route_repo_integration_test.go`

- [ ] **Step 1: Write failing repository tests**

Add integration tests with these exact cases:

```go
func TestAPIKeyRepositoryCreateWithRouteIsAtomic(t *testing.T) { /* one key plus priorities 1,2; duplicate target rolls back key */ }
func TestAPIKeyRepositoryUpdateWithRouteReplacesAndIncrementsVersion(t *testing.T) { /* [2,3] becomes [4], version increments once */ }
func TestAPIKeyRepositoryLoadsTargetsInPriorityOrder(t *testing.T) { /* GetByID, GetByKeyForAuth, ListByUserID */ }
func TestAPIKeyRepositoryListKeysByGroupIDIncludesFallbackReferences(t *testing.T) { /* primary and target references returned once */ }
```

Assertions for the first test must include:

```go
require.NoError(t, repo.CreateWithRoute(ctx, key, []int64{targetA, targetB}))
require.Equal(t, []int64{targetA, targetB}, fallbackGroupIDs(requireKey(t, repo.GetByID(ctx, key.ID))))

duplicate := newKey(userID, primaryID)
err := repo.CreateWithRoute(ctx, duplicate, []int64{targetA, targetA})
require.Error(t, err)
require.False(t, keyExists(t, duplicate.Key))
```

- [ ] **Step 2: Run and verify the tests fail**

Run from `backend`: `go test -tags=integration ./internal/repository -run 'TestAPIKeyRepository(CreateWithRoute|UpdateWithRoute|LoadsTargets|ListKeysByGroupID)' -count=1`

Expected: FAIL because route fields and repository methods do not exist.

- [ ] **Step 3: Add the stable repository contract**

Extend `APIKey` with:

```go
RouteConfigVersion         int64
FailoverRiskAcknowledgedAt *time.Time
FallbackTargets           []APIKeyFailoverTarget
```

Add a narrow optional repository capability so unrelated API-key test doubles do not acquire route methods:

```go
type APIKeyRouteRepository interface {
CreateWithRoute(ctx context.Context, key *APIKey, targetGroupIDs []int64) error
UpdateWithRoute(ctx context.Context, key *APIKey, fields APIKeyUpdateFields, route APIKeyRouteMutation) error
}
```

The production `apiKeyRepository` implements both `APIKeyRepository` and `APIKeyRouteRepository`. Add `RouteConfig` and `RiskAcknowledgement` booleans to `APIKeyUpdateFields`. `UpdateWithRoute` must reuse an Ent transaction from context when present, otherwise create and commit its own transaction. It must update the API-key row, delete the old child rows only when `ReplaceTargets` is true, insert contiguous priorities starting at one, and commit all changes together.

- [ ] **Step 4: Load route targets everywhere API keys are returned**

Use a shared query modifier:

```go
func withAPIKeyRouteTargets(q *dbent.APIKeyQuery) *dbent.APIKeyQuery {
	return q.WithRouteFailoverTargets(func(tq *dbent.APIKeyRouteFailoverTargetQuery) {
		tq.Order(dbent.Asc(apikeyroutefailovertarget.FieldPriority), dbent.Asc(apikeyroutefailovertarget.FieldID)).
			WithTargetGroup()
	})
}
```

Map each child row into `APIKeyFailoverTarget`. For `GetByKeyForAuth`, select every group field currently selected for the primary group on each target group as well.

Change `ListKeysByGroupID` to use an `OR` predicate between `api_keys.group_id = groupID` and `HasRouteFailoverTargetsWith(target_group_id = groupID)`, then deduplicate key strings.

- [ ] **Step 5: Run repository tests**

Run from `backend`:

```bash
gofmt -w internal/service/api_key.go internal/service/api_key_service.go internal/repository/api_key_repo.go internal/repository/api_key_route_repo_integration_test.go
go test -tags=integration ./internal/repository -run 'TestAPIKeyRepository(CreateWithRoute|UpdateWithRoute|LoadsTargets|ListKeysByGroupID)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/service/api_key.go backend/internal/service/api_key_service.go backend/internal/repository/api_key_repo.go backend/internal/repository/api_key_route_repo_integration_test.go
git commit -m "feat: persist API key fallback targets transactionally"
```

### Task 3: Enforce API-key route validation, update semantics, and risk acknowledgement

**Files:**
- Modify: `backend/internal/service/api_key_service.go`
- Create: `backend/internal/service/api_key_route_config_test.go`
- Modify: existing API-key service stubs that implement `APIKeyRepository`

- [ ] **Step 1: Write table-driven failing service tests**

Cover all stable error codes:

```go
var (
	ErrFailoverAckRequired        = infraerrors.BadRequest("FAILOVER_ACK_REQUIRED", "failover billing risk acknowledgement is required")
	ErrFailoverTooManyTargets     = infraerrors.BadRequest("FAILOVER_TOO_MANY_TARGETS", "at most five fallback groups are allowed")
	ErrFailoverDuplicateTarget    = infraerrors.BadRequest("FAILOVER_DUPLICATE_TARGET", "fallback groups must be unique")
	ErrFailoverPrimaryAsTarget    = infraerrors.BadRequest("FAILOVER_PRIMARY_AS_TARGET", "primary group cannot be a fallback group")
	ErrFailoverTargetNotAllowed   = infraerrors.Forbidden("FAILOVER_TARGET_NOT_ALLOWED", "fallback group is not available to this user")
	ErrFailoverTargetIncompatible = infraerrors.BadRequest("FAILOVER_TARGET_INCOMPATIBLE", "fallback group must be active, concrete, and use the primary platform")
)
```

Tests must cover zero through five targets, rejection of a sixth, duplicates, primary as target, unauthorized subscription/exclusive group, inactive group, composite group, cross-platform group, and different subscription types being accepted.

Add mutation tests for:

```go
func TestAPIKeyRouteCreateRequiresAckForNonEmptyChain(t *testing.T)
func TestAPIKeyRouteUpdateOmittedChainIsUnchanged(t *testing.T)
func TestAPIKeyRouteUpdateEmptyChainDisablesWithoutAck(t *testing.T)
func TestAPIKeyRouteUpdatePrimaryChangeClearsOmittedChain(t *testing.T)
func TestAPIKeyRouteUpdateUnchangedNonEmptyChainDoesNotRequireNewAck(t *testing.T)
func TestAPIKeyRouteUpdateChangedNonEmptyChainRequiresAckAndIncrementsVersion(t *testing.T)
```

- [ ] **Step 2: Run and verify failure**

Run from `backend`: `go test ./internal/service -run 'TestAPIKeyRoute(Create|Update|Validation)' -count=1`

Expected: FAIL because requests have no fallback fields and route validation is absent.

- [ ] **Step 3: Add request fields and deterministic validation**

```go
type CreateAPIKeyRequest struct {
	// existing fields
	FallbackGroupIDs          []int64 `json:"fallback_group_ids"`
	FailoverRiskAcknowledged bool    `json:"failover_risk_acknowledged"`
}

type UpdateAPIKeyRequest struct {
	// existing fields
	FallbackGroupIDs          *[]int64 `json:"fallback_group_ids"`
	FailoverRiskAcknowledged bool     `json:"failover_risk_acknowledged"`
}
```

Implement:

```go
func (s *APIKeyService) validateFallbackGroups(
	ctx context.Context,
	user *User,
	primary *Group,
	targetIDs []int64,
) ([]APIKeyFailoverTarget, error)
```

Validation order is: count, positive/unique IDs, primary mismatch, load group, bind permission, active/non-composite, platform equality. Preserve request-array order and assign priorities `1..N`.

Resolve `EffectiveRateMultiplier` with `UserGroupRateRepository.GetByUserAndGroup`, falling back to the group's default multiplier. This value is for the user-facing response; runtime billing still resolves the same user/group value through its existing cache using the effective group ID.

- [ ] **Step 4: Apply create/update semantics**

Create uses `CreateWithRoute` when the chain is non-empty and the existing `Create` path when it is empty. Update uses `APIKeyRouteRepository.UpdateWithRoute` whenever the primary or chain changes, and the existing masked `Update` path otherwise. Production startup must fail provider validation if its API-key repository lacks `APIKeyRouteRepository`. Update computes `primaryChanged`, `replaceTargets`, `targetsChanged`, and `routeChanged` before mutating the key. Use this decision table:

```text
primary changed + fallback omitted    => replace with empty, routeChanged=true
primary unchanged + fallback omitted  => leave unchanged, routeChanged=false
fallback present and equal            => leave rows and acknowledgement unchanged
fallback present and empty            => replace empty, clear acknowledgement
fallback present and changed nonempty => require ack, replace rows, set acknowledgement now
```

Increment `route_config_version` exactly once when the primary or ordered fallback chain changes. Return the transactionally saved chain in create/update responses.

- [ ] **Step 5: Run service tests**

Run from `backend`: `go test ./internal/service -run 'TestAPIKeyRoute(Create|Update|Validation)' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/service/api_key_service.go backend/internal/service/api_key_route_config_test.go
git commit -m "feat: validate user-owned API key failover chains"
```

### Task 4: Expose the API contract and cache complete route snapshots

**Files:**
- Modify: `backend/internal/handler/api_key_handler.go`
- Modify: `backend/internal/handler/dto/types.go`
- Modify: `backend/internal/handler/dto/mappers.go`
- Modify: `backend/internal/service/api_key_auth_cache.go`
- Modify: `backend/internal/service/api_key_auth_cache_impl.go`
- Modify: `backend/internal/service/api_key_auth_cache_version_test.go`
- Create: `backend/internal/handler/api_key_route_contract_test.go`

- [ ] **Step 1: Write failing handler and cache tests**

Handler tests must assert create/update bind omitted versus empty arrays correctly and response JSON contains:

```json
{
  "route_config_version": 2,
  "failover_enabled": true,
  "fallback_groups": [
    {"id": 12, "name": "Stable OpenAI", "platform": "openai", "subscription_type": "standard", "rate_multiplier": 1.5, "priority": 1}
  ]
}
```

Add `TestAPIKeyRouteCreateIdempotentReplayReturnsOriginalChain`: send the same create request twice with the same idempotency key and assert the second response returns the original API-key ID and ordered chain while the database contains one key and one copy of each target.

Cache tests must round-trip two target groups, their priority, all pricing/feature fields already present in `APIKeyAuthGroupSnapshot`, and distinct `UserGroupRPMOverride` values.

Add invalidation tests proving a target-group edit invalidates keys that reference it as a fallback, and subscription/user-group permission invalidation clears every affected user's key snapshot. Add a cache-degradation test that loads a route snapshot once, makes the repository fail, and proves authentication still returns the last valid cached chain; a key with no successfully loaded route snapshot remains primary only.

- [ ] **Step 2: Run and verify failure**

Run from `backend`:

```bash
go test ./internal/handler -run TestAPIKeyRouteContract -count=1
go test ./internal/service -run 'TestAPIKeyAuthSnapshot.*Route' -count=1
```

Expected: FAIL because the contract and cache target snapshots do not exist.

- [ ] **Step 3: Add DTOs and handler binding**

```go
type APIKeyFallbackGroup struct {
	ID               int64   `json:"id"`
	Name             string  `json:"name"`
	Platform         string  `json:"platform"`
	SubscriptionType string  `json:"subscription_type"`
	RateMultiplier   float64 `json:"rate_multiplier"`
	Priority         int     `json:"priority"`
}
```

Add `FallbackGroupIDs` and `FailoverRiskAcknowledged` to handler request structs and service mapping. Add `RouteConfigVersion`, `FailoverEnabled`, and `FallbackGroups` to the response DTO. Do not return `failover_risk_acknowledged_at`, target-table IDs, RPM overrides, user subscription records, or private group fields.

Map `APIKeyFallbackGroup.RateMultiplier` from `APIKeyFailoverTarget.EffectiveRateMultiplier`, not directly from the group's default rate.

- [ ] **Step 4: Add auth-cache route snapshots**

```go
type APIKeyAuthRouteTargetSnapshot struct {
	Priority             int                     `json:"priority"`
	Group                APIKeyAuthGroupSnapshot `json:"group"`
	UserGroupRPMOverride *int                    `json:"user_group_rpm_override,omitempty"`
}
```

Add `RouteConfigVersion` and `FallbackTargets` to `APIKeyAuthSnapshot`. Extract one shared `groupToAuthSnapshot` and `authSnapshotToGroup` mapper so primary and fallback groups cannot drift. Query each target's RPM override only while building the cache snapshot, never during cache materialization. Bump `apiKeyAuthSnapshotVersion` from `19` to `20`.

Carry each target's already resolved `EffectiveRateMultiplier` in the response-side domain object, but do not trust it for charging. Charging continues to resolve the current multiplier by effective `(user_id, group_id)`.

When materializing a fallback target, attach its RPM override to `APIKeyFailoverTarget.UserGroupRPMOverride`; do not overwrite the primary user's override.

- [ ] **Step 5: Run contract/cache tests and auth middleware tests**

Run from `backend`:

```bash
gofmt -w internal/handler/api_key_handler.go internal/handler/dto/types.go internal/handler/dto/mappers.go internal/service/api_key_auth_cache.go internal/service/api_key_auth_cache_impl.go
go test ./internal/handler -run TestAPIKeyRouteContract -count=1
go test ./internal/service -run 'TestAPIKeyAuthSnapshot|TestAPIKeyAuthCacheVersion' -count=1
go test ./internal/server/middleware -run 'TestApiKeyAuthWithSubscription|TestAPIKeyAuthWithSubscriptionGoogle' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add backend/internal/handler/api_key_handler.go backend/internal/handler/api_key_route_contract_test.go backend/internal/handler/dto backend/internal/service/api_key_auth_cache.go backend/internal/service/api_key_auth_cache_impl.go backend/internal/service/api_key_auth_cache_version_test.go
git commit -m "feat: expose and cache API key failover routes"
```

### Task 5: Implement Redis route stickiness with a sliding one-hour TTL

**Files:**
- Create: `backend/internal/service/route_failover_sticky.go`
- Create: `backend/internal/repository/route_failover_sticky.go`
- Create: `backend/internal/repository/route_failover_sticky_test.go`
- Modify: `backend/internal/repository/wire.go`

- [ ] **Step 1: Write failing miniredis tests**

```go
func TestRouteStickyRoundTripAndSlidingTTL(t *testing.T)
func TestRouteStickyStaleVersionIsDeleted(t *testing.T)
func TestRouteStickyDeleteOnRetryableFailure(t *testing.T)
func TestRouteStickyStoreFailureIsFailOpen(t *testing.T)
```

Assert the Redis key is `route:sticky:key:{api_key_id}:{session_hash}`, the JSON value contains only `route_config_version` and `effective_group_id`, the initial TTL is `3600s`, and a successful sticky request refreshes it to `3600s`.

- [ ] **Step 2: Run and verify failure**

Run from `backend`: `go test ./internal/repository -run TestRouteSticky -count=1`

Expected: FAIL because the store does not exist.

- [ ] **Step 3: Implement the store**

```go
const APIKeyRouteStickyTTL = time.Hour

type APIKeyRouteStickyBinding struct {
	RouteConfigVersion int64 `json:"route_config_version"`
	EffectiveGroupID   int64 `json:"effective_group_id"`
}

type APIKeyRouteStickyStore interface {
	Get(ctx context.Context, apiKeyID int64, sessionHash string) (*APIKeyRouteStickyBinding, error)
	Set(ctx context.Context, apiKeyID int64, sessionHash string, binding APIKeyRouteStickyBinding, ttl time.Duration) error
	Refresh(ctx context.Context, apiKeyID int64, sessionHash string, ttl time.Duration) error
	Delete(ctx context.Context, apiKeyID int64, sessionHash string) error
}
```

Return `(nil, nil)` for missing keys. Empty session hashes bypass every Redis operation. Service callers log and meter store errors but continue with configured candidate order.

- [ ] **Step 4: Run tests**

Run from `backend`: `go test ./internal/repository -run TestRouteSticky -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/route_failover_sticky.go backend/internal/repository/route_failover_sticky.go backend/internal/repository/route_failover_sticky_test.go backend/internal/repository/wire.go
git commit -m "feat: add API key route stickiness store"
```

### Task 6: Re-key the circuit breaker by target group and requested model

**Files:**
- Modify: `backend/internal/service/route_failover.go`
- Modify: `backend/internal/repository/route_failover_circuit.go`
- Modify: `backend/internal/repository/route_failover_circuit_test.go`

- [ ] **Step 1: Replace policy-based circuit tests with fixed-parameter tests**

Tests must prove:

```text
key identity              effective group ID + hash(requested model)
window                    60 seconds
eligible failures to open 5 consecutive failures
open cooldown             60 seconds
half-open lease           15 seconds
successes to close        2 consecutive successes
```

Also prove capacity, unsupported model, billing/admission, request `4xx`, and client cancellation never call `RecordFailure`.

Prove a successful closed-state request clears the current failure sequence, so four failures, one success, then one failure does not open the circuit. The fifth consecutive eligible failure opens it.

- [ ] **Step 2: Run and verify failure**

Run from `backend`: `go test ./internal/repository -run TestRouteFailoverCircuit -count=1`

Expected: FAIL while the API still requires policy and target IDs.

- [ ] **Step 3: Replace the circuit interface**

```go
type RouteFailoverCircuit interface {
	Allow(ctx context.Context, effectiveGroupID int64, requestedModel string) (RouteFailoverPermit, error)
	RecordSuccess(ctx context.Context, effectiveGroupID int64, requestedModel, leaseID string) error
	RecordFailure(ctx context.Context, effectiveGroupID int64, requestedModel, leaseID string) error
}
```

Build Redis keys as `route_failover:group:{group_id}:{model_hash}:{state|failures|lease}`. Keep the existing Lua atomicity, replace policy parameters with constants, and treat every Redis error as allowed/no-record at the service boundary.

In the closed-state success branch, delete the failures sorted set. This is required for a consecutive-failure threshold rather than a count of unrelated failures inside the window.

- [ ] **Step 4: Run tests**

Run from `backend`: `go test ./internal/repository -run TestRouteFailoverCircuit -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/route_failover.go backend/internal/repository/route_failover_circuit.go backend/internal/repository/route_failover_circuit_test.go
git commit -m "refactor: key route circuits by group and model"
```

### Task 7: Build immutable per-request route plans and effective billing contexts

**Files:**
- Rewrite: `backend/internal/service/route_failover.go`
- Create: `backend/internal/service/route_failover_plan_test.go`
- Modify: `backend/internal/service/billing_cache_service.go`
- Create: `backend/internal/service/billing_cache_route_test.go`
- Modify: `backend/internal/service/subscription_service.go`

- [ ] **Step 1: Write failing route-plan tests**

Cover primary-only, normal ordering, sticky-first ordering, stale version deletion, removed target deletion, sticky retryable failure rebuilding primary-first order, no duplicate candidate per request, 20-second switch deadline, unsupported-model skip, ineligible-target skip, and Redis fail-open behavior.

Candidate eligibility uses the immutable authentication snapshot: standard targets re-run `User.CanBindGroup` against cached allowed groups and target exclusivity; subscription targets require `SubscriptionService.GetActiveSubscription`; every target must still be active, concrete, and platform-compatible. An ineligible target is skipped without invalidating the API key or opening its circuit.

Use these route types:

```go
type APIKeyRouteCandidate struct {
	EffectiveAPIKey     *APIKey
	Subscription        *UserSubscription
	SourceGroupID       int64
	EffectiveGroupID    int64
	RequestedModel      string
	IsFallback          bool
	StickyHit           bool
	CircuitLeaseID      string
	Priority            int
}

type APIKeyRouteAttemptFailure struct {
	Class             RouteFailureClass
	StatusCode        int
	ReplaySafe        bool
	SemanticCommitted bool
	Cause             error
}
```

- [ ] **Step 2: Run and verify failure**

Run from `backend`: `go test ./internal/service -run 'TestAPIKeyRoutePlan|TestBillingCacheRoute' -count=1`

Expected: FAIL because the per-key planner and candidate admission do not exist.

- [ ] **Step 3: Implement immutable candidate snapshots**

The planner constructor is:

```go
func NewAPIKeyRoutePlanner(
	sticky APIKeyRouteStickyStore,
	circuit RouteFailoverCircuit,
	subscriptions *SubscriptionService,
) *APIKeyRoutePlanner
```

The request API is:

```go
func (p *APIKeyRoutePlanner) NewPlan(
	ctx context.Context,
	apiKey *APIKey,
	primarySubscription *UserSubscription,
	sessionHash string,
	requestedModel string,
) *APIKeyRoutePlan

func (p *APIKeyRoutePlan) Next(ctx context.Context) (*APIKeyRouteCandidate, bool)
func (p *APIKeyRoutePlan) Skip(ctx context.Context, candidate *APIKeyRouteCandidate, class RouteFailureClass)
func (p *APIKeyRoutePlan) Fail(ctx context.Context, candidate *APIKeyRouteCandidate, failure APIKeyRouteAttemptFailure) bool
func (p *APIKeyRoutePlan) Succeed(ctx context.Context, candidate *APIKeyRouteCandidate)
func (p *APIKeyRoutePlan) Audit() RouteUsageAudit
```

`Fail` returns whether another candidate may be attempted. It returns false for business/local `4xx`, authentication, key quota/rate, unsafe replay, semantic commitment, cancellation, or expired deadline. Capacity failures advance without recording the circuit. Connection, timeout, upstream `429`, and upstream `5xx` advance and record the circuit when replay safe.

The 20-second deadline is created only when the primary records its first eligible cross-group failure. It is checked before starting another candidate and is not attached as the generation/streaming context deadline after an upstream safely accepts the request.

- [ ] **Step 4: Build an effective API key without mutating the auth snapshot**

```go
func effectiveAPIKeyForTarget(source *APIKey, target APIKeyFailoverTarget) *APIKey {
	copyKey := *source
	copyUser := *source.User
	copyUser.UserGroupRPMOverride = target.UserGroupRPMOverride
	groupID := target.TargetGroupID
	copyKey.GroupID = &groupID
	copyKey.Group = target.Group
	copyKey.User = &copyUser
	return &copyKey
}
```

The copied key retains ID, key-wide quota, key-wide rolling rate limits, user identity, and original route config. It changes only group, group ID, and group-scoped RPM override.

- [ ] **Step 5: Split candidate admission from key-global RPM accounting**

Refactor billing admission internally to accept:

```go
type BillingAdmissionOptions struct {
	CountUserRPM bool
}

func (s *BillingCacheService) CheckRouteCandidateEligibility(
	ctx context.Context,
	user *User,
	apiKey *APIKey,
	group *Group,
	subscription *UserSubscription,
	platform string,
	opts BillingAdmissionOptions,
) error
```

`CheckBillingEligibility` calls it with `CountUserRPM: true`. Fallback attempts call it with false because the request already counted against the user-global RPM at primary admission; group/override RPM still counts once for each group actually attempted. API-key quota/rate checks remain key-wide and do not reserve or deduct during admission.

- [ ] **Step 6: Run route and billing tests**

Run from `backend`: `go test ./internal/service -run 'TestAPIKeyRoutePlan|TestBillingCacheRoute|TestBillingCache.*RPM' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add backend/internal/service/route_failover.go backend/internal/service/route_failover_plan_test.go backend/internal/service/billing_cache_service.go backend/internal/service/billing_cache_route_test.go backend/internal/service/subscription_service.go
git commit -m "feat: plan API key routes with effective billing contexts"
```

### Task 8: Remove cross-group traversal from account schedulers

**Files:**
- Modify: `backend/internal/service/gateway_scheduling.go`
- Modify: `backend/internal/service/openai_account_scheduler.go`
- Modify: `backend/internal/service/route_failover_gateway_test.go`
- Modify: `backend/internal/service/gateway_multiplatform_test.go`
- Modify: `backend/internal/service/openai_gateway_service_test.go`

- [ ] **Step 1: Write failing scheduler-boundary tests**

```go
func TestGatewaySelectionNeverChangesRequestedGroup(t *testing.T)
func TestOpenAISelectionNeverChangesRequestedGroup(t *testing.T)
func TestGroupLocalExhaustionReturnsNoAvailableAccounts(t *testing.T)
```

Each test creates accounts in two groups, selects with group A, and proves group B is never returned even when A is exhausted.

- [ ] **Step 2: Run and verify failure against the administrator-policy planner**

Run from `backend`: `go test ./internal/service -run 'Test(Gateway|OpenAI)SelectionNeverChangesRequestedGroup|TestGroupLocalExhaustion' -count=1`

Expected: FAIL while selection methods can traverse a global route policy.

- [ ] **Step 3: Make selection strictly group local**

Delete the route-planner branches from `SelectAccountWithLoadAwareness`, `SelectAccountForModelWithRouteFailover`, and `selectAccountWithSchedulerAndRouteFailover`. Rename public methods only where call sites can be migrated atomically; the retained method signatures must return `EffectiveGroupID` equal to their input group and must not set `RouteTargetID`.

Keep account-level retry budgets, model/channel mapping, concurrency acquisition, sticky account selection, profit gates, and stream safety unchanged.

- [ ] **Step 4: Run scheduler suites**

Run from `backend`:

```bash
go test ./internal/service -run 'Test(Gateway|OpenAI)SelectionNeverChangesRequestedGroup|TestGroupLocalExhaustion' -count=1
go test ./internal/service -run 'TestGatewayService_SelectAccountWithLoadAwareness|TestOpenAI.*Account|TestFailoverState' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/service/gateway_scheduling.go backend/internal/service/openai_account_scheduler.go backend/internal/service/route_failover_gateway_test.go backend/internal/service/gateway_multiplatform_test.go backend/internal/service/openai_gateway_service_test.go
git commit -m "refactor: keep account scheduling within one group"
```

### Task 9: Persist effective-group usage and route audit exactly once

**Files:**
- Modify: `backend/internal/service/usage_log.go`
- Modify: `backend/internal/service/gateway_usage_billing.go`
- Modify: `backend/internal/service/openai_gateway_usage.go`
- Modify: `backend/internal/repository/usage_log_repo_insert.go`
- Modify: `backend/internal/repository/usage_log_repo_query.go`
- Modify: `backend/internal/handler/dto/types.go`
- Modify: `backend/internal/handler/dto/mappers.go`
- Create: `backend/internal/service/route_failover_billing_test.go`
- Modify: `backend/internal/repository/usage_log_repo_unit_test.go`

- [ ] **Step 1: Write failing billing and persistence tests**

Test a `0.5x` standard primary and `1.5x` subscription fallback. Assert:

```go
require.Equal(t, fallbackGroupID, *usage.GroupID)
require.Equal(t, primaryGroupID, *usage.SourceGroupID)
require.True(t, usage.RouteFallbackUsed)
require.Equal(t, 2, usage.RouteAttemptCount)
require.Equal(t, "upstream_5xx", *usage.RouteFallbackReason)
require.Equal(t, fallbackSubscription.ID, *usage.SubscriptionID)
require.Equal(t, 1.5, usage.RateMultiplier)
```

Also prove duplicate request IDs create one usage row and charge once, and historical rows with null `source_group_id` are mapped as source equals effective.

- [ ] **Step 2: Run and verify failure**

Run from `backend`:

```bash
go test ./internal/service -run TestRouteFailoverBilling -count=1
go test ./internal/repository -run TestUsageLogRouteAudit -count=1
```

Expected: FAIL because usage route fields are not carried or persisted.

- [ ] **Step 3: Add route audit fields and inputs**

Add these fields to `UsageLog` and both normal usage input types:

```go
SourceGroupID      *int64
RouteFallbackUsed bool
RouteAttemptCount int
RouteFallbackReason *string
RouteStickyHit    bool
SourceGroup       *Group
```

Add `RouteAudit RouteUsageAudit` to `RecordUsageInput`, `RecordUsageLongContextInput`, `OpenAIRecordUsageInput`, and cyber usage input. Default attempt count to one. If source is absent on a newly created log, set it to the effective `GroupID`.

- [ ] **Step 4: Extend every insert and scan position together**

In `usage_log_repo_insert.go`, add the five SQL types, columns, placeholders, and prepared arguments in the same order immediately after `group_id`. Update batch CTEs, no-result inserts, `usageLogSelectColumns`, scans, and integration fixtures together. Retain `ON CONFLICT (request_id, api_key_id) DO NOTHING` as the final duplicate-charge guard.

Usage query hydration must collect both effective `group_id` and `source_group_id`, batch-load their public group records, and attach `Group` plus `SourceGroup`. For historical rows with null `source_group_id`, assign `SourceGroupID = GroupID` and `SourceGroup = Group` in the service mapper.

Expose `source_group_id`, `route_fallback_used`, `route_attempt_count`, `route_fallback_reason`, `route_sticky_hit`, and the public `source_group` summary in both user and admin usage DTOs.

- [ ] **Step 5: Make billing consume the effective objects**

Every successful handler passes `candidate.EffectiveAPIKey` and `candidate.Subscription` into usage recording. Cost resolution, user group rate lookup, subscription deduction, platform quota, group limits, and rate multiplier therefore use the actual group. The original API key ID and key-wide quota remain unchanged.

- [ ] **Step 6: Run usage and billing suites**

Run from `backend`:

```bash
gofmt -w internal/service/usage_log.go internal/service/gateway_usage_billing.go internal/service/openai_gateway_usage.go internal/repository/usage_log_repo_insert.go internal/repository/usage_log_repo_query.go
go test ./internal/service -run 'TestRouteFailoverBilling|TestGatewayUsageBilling|TestOpenAI.*Usage' -count=1
go test ./internal/repository -run 'TestUsageLog(RouteAudit|Repo)' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add backend/internal/service/usage_log.go backend/internal/service/gateway_usage_billing.go backend/internal/service/openai_gateway_usage.go backend/internal/service/route_failover_billing_test.go backend/internal/repository/usage_log_repo_insert.go backend/internal/repository/usage_log_repo_query.go backend/internal/repository/usage_log_repo_unit_test.go backend/internal/handler/dto
git commit -m "feat: bill and audit the effective route group"
```

### Task 10: Add the shared handler route-execution boundary

**Files:**
- Create: `backend/internal/handler/route_execution.go`
- Create: `backend/internal/handler/route_execution_test.go`
- Modify: `backend/internal/handler/failover_loop.go`
- Create: `backend/internal/service/route_failover_metrics.go`
- Create: `backend/internal/service/route_failover_metrics_test.go`

- [ ] **Step 1: Write failing execution-boundary tests**

Tests must prove that one request creates one route plan, every candidate creates a fresh group-local `FailoverState`, group-local account retries do not advance the route, exhausted capacity does advance it, every acquired account slot/reservation is released before advancement, a semantic write blocks advancement, an SSE comment alone does not, cancellation stops immediately, and a candidate is never attempted twice.

Metrics tests must cover route requests, fallback successes, candidate skips by reason, circuit transitions, sticky hits, invalid sticky records, configuration validation failures, state-store errors, route-switch duration, and billing idempotency conflicts.

- [ ] **Step 2: Run and verify failure**

Run from `backend`: `go test ./internal/handler -run TestRouteExecution -count=1`

Expected: FAIL because the shared execution state does not exist.

- [ ] **Step 3: Implement the adapter**

```go
type routeExecution struct {
	plan              *service.APIKeyRoutePlan
	candidate         *service.APIKeyRouteCandidate
	local             *FailoverState
	semanticCommitted bool
}

func newRouteExecution(plan *service.APIKeyRoutePlan, localSwitchBudget int) *routeExecution
func (r *routeExecution) next(ctx context.Context) (*service.APIKeyRouteCandidate, bool)
func (r *routeExecution) capacityFailed(ctx context.Context, err error) bool
func (r *routeExecution) upstreamFailed(ctx context.Context, err error, replaySafe bool) bool
func (r *routeExecution) markSemanticCommitted()
func (r *routeExecution) succeed(ctx context.Context) service.RouteUsageAudit
```

`next` resets account exclusions and same-account retry counters for the new group. `upstreamFailed` translates only connection/timeout/429/5xx `UpstreamFailoverError` values to eligible route failures. It must not use `c.Writer.Size() != before` as the sole stream guard; protocol forwarders explicitly call `markSemanticCommitted` when a token, tool call, business SSE event, irreversible WebSocket frame, or media task ID is emitted.

Add an atomic `APIKeyRouteMetrics` snapshot following the existing gateway metrics style. Emit structured logs containing request ID, API key ID, source group, configured/attempted group IDs, failure class/status, effective group, route version, sticky state, attempt count, effective subscription ID, effective multiplier, and billing result. Never log API-key plaintext, request bodies, upstream credentials, or Redis binding values.

- [ ] **Step 4: Run handler unit tests**

Run from `backend`:

```bash
go test ./internal/handler -run 'TestRouteExecution|TestFailoverState|TestGatewayHandlerStreamFailover' -count=1
go test ./internal/service -run TestAPIKeyRouteMetrics -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/handler/route_execution.go backend/internal/handler/route_execution_test.go backend/internal/handler/failover_loop.go backend/internal/service/route_failover_metrics.go backend/internal/service/route_failover_metrics_test.go
git commit -m "feat: centralize cross-group route execution"
```

### Task 11: Integrate Anthropic and Gemini-compatible handlers

**Files:**
- Modify: `backend/internal/handler/gateway_handler.go`
- Modify: `backend/internal/handler/gateway_handler_chat_completions.go`
- Modify: `backend/internal/handler/gateway_handler_responses.go`
- Modify: `backend/internal/handler/gateway_web_search.go`
- Modify: `backend/internal/handler/gemini_v1beta_handler.go`
- Modify: `backend/internal/handler/gateway_handler_route_failover_test.go`
- Modify: `backend/internal/handler/gateway_handler_stream_failover_test.go`

- [ ] **Step 1: Add failing protocol tests**

For messages, chat completions, responses, web search, native Gemini, count-tokens, sync, and streaming variants, assert:

```text
primary capacity exhausted        fallback selected
primary connection/timeout/429/5xx fallback selected when replay safe
primary business 400/401/403      no fallback
fallback admission rejected       next configured fallback tried
fallback model unsupported        skipped without circuit failure
semantic stream output committed  no cross-group replay
successful fallback               effective key/subscription passed to RecordUsage
```

- [ ] **Step 2: Run and verify failure**

Run from `backend`: `go test ./internal/handler -run 'TestGatewayHandler.*APIKeyRoute|TestGemini.*APIKeyRoute' -count=1`

Expected: FAIL before handlers create route plans.

- [ ] **Step 3: Wrap the existing group-local loops**

For each entry point:

```go
plan := h.routePlanner.NewPlan(ctx, apiKey, primarySubscription, sessionHash, requestedModel)
route := newRouteExecution(plan, localAccountSwitchBudget)
for candidate, ok := route.next(ctx); ok; candidate, ok = route.next(ctx) {
	if err := h.billingCacheService.CheckRouteCandidateEligibility(
		ctx, candidate.EffectiveAPIKey.User, candidate.EffectiveAPIKey,
		candidate.EffectiveAPIKey.Group, candidate.Subscription,
		service.QuotaPlatform(ctx, candidate.EffectiveAPIKey),
		service.BillingAdmissionOptions{CountUserRPM: false},
	); err != nil {
		plan.Skip(ctx, candidate, service.RouteFailureAdmission)
		continue
	}
	// Run the existing account selection and forwarding loop only for candidate.EffectiveGroupID.
}
```

Primary billing remains checked once before route planning with user RPM counting enabled. For the primary candidate, do not repeat billing admission. Forward the unchanged requested model through normal channel mapping for the candidate group; API-key routing adds no model mapping.

On success pass `candidate.EffectiveAPIKey`, `candidate.Subscription`, and `route.succeed(ctx)` to usage recording. Refresh/write stickiness only after the successful usage path is accepted.

- [ ] **Step 4: Run protocol and cancellation tests**

Run from `backend`:

```bash
go test ./internal/handler -run 'TestGatewayHandler.*APIKeyRoute|TestGemini.*APIKeyRoute|TestGatewayHandler.*Cancellation|TestGatewayHandlerStreamFailover' -count=1
go test ./internal/service -run 'TestGatewayService_SelectAccountWithLoadAwareness|TestUsage' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/handler/gateway_handler.go backend/internal/handler/gateway_handler_chat_completions.go backend/internal/handler/gateway_handler_responses.go backend/internal/handler/gateway_web_search.go backend/internal/handler/gemini_v1beta_handler.go backend/internal/handler/gateway_handler_route_failover_test.go backend/internal/handler/gateway_handler_stream_failover_test.go
git commit -m "feat: route Anthropic and Gemini requests by API key"
```

### Task 12: Integrate OpenAI handlers, including streaming and non-idempotent endpoints

**Files:**
- Modify: `backend/internal/handler/openai_gateway_handler.go`
- Modify: `backend/internal/handler/openai_chat_completions.go`
- Modify: `backend/internal/handler/openai_images.go`
- Modify: `backend/internal/handler/openai_gateway_handler_test.go`
- Create: `backend/internal/handler/openai_route_failover_test.go`

- [ ] **Step 1: Write failing OpenAI route tests**

Cover responses, chat completions, messages dispatch, images, websocket/live, and compact/count endpoints. Prove the same failure matrix as Task 11 plus:

```text
pool-mode same-account retries stay within candidate
OAuth 429 account retry budget exhausts before route advancement
SSE/WebSocket semantic output blocks route replay
image/media task ID blocks route replay
non-idempotent endpoint without proof of non-acceptance does not fail over
explicit idempotency key permits replay before semantic acceptance
```

- [ ] **Step 2: Run and verify failure**

Run from `backend`: `go test ./internal/handler -run 'TestOpenAI.*APIKeyRoute' -count=1`

Expected: FAIL before OpenAI handlers own a route execution.

- [ ] **Step 3: Integrate one route plan per request**

Apply the shared boundary around, not inside, existing OpenAI account retry loops. Reset `failedAccountIDs`, `sameAccountRetryCount`, `switchCount`, profit-veto count, and OAuth-429 state only when moving to another group. Resolve request pricing context and channel mapping for `candidate.EffectiveAPIKey.GroupID`.

Use the successful candidate for:

```go
APIKey:       candidate.EffectiveAPIKey,
Subscription: candidate.Subscription,
RouteAudit:   route.succeed(ctx),
```

Never rewrite the requested model because of API-key route selection. Retain normal channel/account mapping after the group is chosen.

- [ ] **Step 4: Run OpenAI handler and service suites**

Run from `backend`:

```bash
go test ./internal/handler -run 'TestOpenAI.*APIKeyRoute|TestOpenAIGatewayHandler|TestOpenAIChatCompletions' -count=1
go test ./internal/service -run 'TestOpenAI.*(Scheduler|Usage|Sticky|Billing)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add backend/internal/handler/openai_gateway_handler.go backend/internal/handler/openai_chat_completions.go backend/internal/handler/openai_images.go backend/internal/handler/openai_gateway_handler_test.go backend/internal/handler/openai_route_failover_test.go
git commit -m "feat: route OpenAI requests by API key"
```

### Task 13: Retire the administrator route-policy runtime and HTTP surface

**Files:**
- Modify: `backend/internal/server/routes/admin.go`
- Modify: `backend/internal/handler/admin/group_handler.go`
- Modify: `backend/internal/service/admin_group.go`
- Modify: `backend/internal/service/admin_service.go`
- Modify: `backend/internal/service/wire.go`
- Modify: `backend/internal/repository/wire.go`
- Delete: `backend/internal/service/route_failover_manager.go`
- Delete: `backend/internal/service/route_failover_manager_test.go`
- Modify: `backend/internal/service/wire_test.go`
- Create: `backend/internal/server/routes/admin_route_registration_test.go`

- [ ] **Step 1: Write the failing route-registration test**

```go
func TestAdminRouteFailoverEndpointsAreRetired(t *testing.T) {
	router := buildAdminTestRouter(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		resp := performRequest(router, method, "/api/v1/admin/groups/1/route-failover", nil)
		require.Equal(t, http.StatusNotFound, resp.Code)
	}
}
```

Add a Wire test that constructs the application without `RouteFailoverManager` or `RouteFailoverConfigRepository`.

- [ ] **Step 2: Run and verify failure**

Run from `backend`: `go test ./internal/server/routes ./internal/service -run 'TestAdminRouteFailoverEndpointsAreRetired|TestWire' -count=1`

Expected: FAIL because routes/providers are still registered.

- [ ] **Step 3: Remove the legacy runtime surface**

Delete the three routes, request/response types, admin service methods, manager fields/constructor parameters, policy repository provider, startup snapshot reload, and manager code. Keep the circuit provider and new sticky/planner providers. Keep legacy repository source files only if a rolling binary still imports their concrete symbols; otherwise remove provider registration so they are unreachable.

Run Wire generation after constructor changes:

```bash
go generate ./cmd/server
```

- [ ] **Step 4: Run route and dependency tests**

Run from `backend`:

```bash
go test ./internal/server/routes ./internal/service -run 'TestAdminRouteFailoverEndpointsAreRetired|TestWire' -count=1
go test ./cmd/server ./internal/handler/admin -count=1
```

Expected: PASS. `rg -n 'GetRouteFailoverPolicy|SaveRouteFailoverPolicy|DeleteRouteFailoverPolicy|ProvideRouteFailoverManager' backend` returns no runtime matches.

- [ ] **Step 5: Commit**

```bash
git add -A backend/internal/server/routes/admin.go backend/internal/handler/admin/group_handler.go backend/internal/service backend/internal/repository/wire.go backend/cmd/server
git commit -m "refactor: retire administrator route failover policies"
```

### Task 14: Add frontend API-key route types and editor behavior

**Files:**
- Modify: `frontend/src/types/index.ts`
- Modify: `frontend/src/api/keys.ts`
- Create: `frontend/src/api/__tests__/keys.routeFailover.spec.ts`
- Create: `frontend/src/components/keys/RouteFailoverEditor.vue`
- Create: `frontend/src/components/keys/__tests__/RouteFailoverEditor.spec.ts`

- [ ] **Step 1: Write failing API and component tests**

API tests assert create sends empty/ordered fallback IDs and acknowledgement, update preserves omission versus empty arrays, and responses parse ordered fallback groups.

Component tests assert: toggle is off by default; primary and cross-platform groups are excluded; already selected groups are excluded; add/remove/reorder works; a sixth row cannot be added; changing primary clears selections; non-empty changed chain requires the checkbox; unchanged edit does not; disabling does not.

- [ ] **Step 2: Run and verify failure**

Run from `frontend`: `pnpm exec vitest run src/api/__tests__/keys.routeFailover.spec.ts src/components/keys/__tests__/RouteFailoverEditor.spec.ts`

Expected: FAIL because types, payloads, and component are absent.

- [ ] **Step 3: Add frontend contract types**

```ts
export interface ApiKeyFallbackGroup {
  id: number
  name: string
  platform: GroupPlatform
  subscription_type: SubscriptionType
  rate_multiplier: number
  priority: number
}

export interface ApiKey {
  // existing fields
  route_config_version: number
  failover_enabled: boolean
  fallback_groups: ApiKeyFallbackGroup[]
}
```

Add `fallback_group_ids?: number[]` and `failover_risk_acknowledged?: boolean` to create/update request types. Change `keysAPI.create` to accept one `CreateApiKeyRequest` object instead of adding more positional parameters; migrate its existing call site in Task 15.

- [ ] **Step 4: Build the focused editor**

Component props and emits:

```ts
const props = defineProps<{
  modelValue: number[]
  primaryGroupId: number | null
  groups: Group[]
  userGroupRates: Record<number, number>
  enabled: boolean
  riskAcknowledged: boolean
  acknowledgementRequired: boolean
}>()

const emit = defineEmits<{
  'update:modelValue': [number[]]
  'update:enabled': [boolean]
  'update:riskAcknowledged': [boolean]
}>()
```

Use existing `Icon`, `Select`, `GroupOptionItem`, and icon buttons for move up/down/remove with tooltips. Render the approved warning that the actual group's price, quota, rate limit, subscription, and concurrency rules apply. Do not render model mapping, circuit, retry, or timeout settings.

- [ ] **Step 5: Run API/component tests**

Run from `frontend`: `pnpm exec vitest run src/api/__tests__/keys.routeFailover.spec.ts src/components/keys/__tests__/RouteFailoverEditor.spec.ts`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/types/index.ts frontend/src/api/keys.ts frontend/src/api/__tests__/keys.routeFailover.spec.ts frontend/src/components/keys/RouteFailoverEditor.vue frontend/src/components/keys/__tests__/RouteFailoverEditor.spec.ts
git commit -m "feat: add API key failover editor contract"
```

### Task 15: Integrate the editor, key-list summary, locales, and usage audit display

**Files:**
- Modify: `frontend/src/views/user/KeysView.vue`
- Modify: `frontend/src/views/user/__tests__/KeysView.spec.ts`
- Modify: `frontend/src/views/user/UsageView.vue`
- Modify: `frontend/src/views/user/__tests__/UsageView.spec.ts`
- Modify: `frontend/src/components/admin/usage/UsageTable.vue`
- Modify: `frontend/src/components/admin/usage/__tests__/UsageTable.spec.ts`
- Modify: `frontend/src/i18n/locales/en/dashboard.ts`
- Modify: `frontend/src/i18n/locales/zh/dashboard.ts`
- Modify: `frontend/src/types/index.ts`

- [ ] **Step 1: Write failing view tests**

Key tests assert create/edit payloads, risk-gated submit, primary change clearing the chain, and list summary `Primary + 2 fallbacks` with ordered names/multipliers.

Usage tests assert normal rows show only the effective group and fallback rows show `Primary -> Effective`; both user and admin views use `source_group_id ?? group_id` for historical rows.

- [ ] **Step 2: Run and verify failure**

Run from `frontend`:

```bash
pnpm exec vitest run src/views/user/__tests__/KeysView.spec.ts src/views/user/__tests__/UsageView.spec.ts src/components/admin/usage/__tests__/UsageTable.spec.ts
```

Expected: FAIL because view state and route audit fields are absent.

- [ ] **Step 3: Integrate form state and payloads**

Add to `formData`:

```ts
enable_route_failover: false,
fallback_group_ids: [] as number[],
failover_risk_acknowledged: false,
original_fallback_group_ids: [] as number[],
```

Compute acknowledgement requirement by ordered array equality. Create sends the route fields. Edit omits `fallback_group_ids` when both primary and chain are unchanged; sends `[]` when disabled; sends the ordered list and acknowledgement when changed. Watch primary group changes and clear fallback IDs plus acknowledgement.

- [ ] **Step 4: Add route summary and usage fields**

Extend `UsageLog` with:

```ts
source_group_id: number | null
route_fallback_used: boolean
route_attempt_count: number
route_fallback_reason: string | null
route_sticky_hit: boolean
source_group?: Group
```

In list rows show a compact `Primary + N fallbacks` control; expansion is an unframed ordered list, not nested cards. Usage group cell renders the effective group normally and `source -> effective` only when `route_fallback_used` is true.

- [ ] **Step 5: Add English and Chinese strings**

Add keys for route toggle, selector, maximum, add/remove/reorder tooltips, summary, acknowledgement label, and billing warning under `keys.routeFailover`. Add usage route labels under `usage.route`.

- [ ] **Step 6: Run view, locale, type, and lint checks**

Run from `frontend`:

```bash
pnpm exec vitest run src/views/user/__tests__/KeysView.spec.ts src/views/user/__tests__/UsageView.spec.ts src/components/admin/usage/__tests__/UsageTable.spec.ts src/i18n/__tests__/localesMessageCompile.spec.ts
pnpm run typecheck
pnpm run lint:check
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add frontend/src/views/user/KeysView.vue frontend/src/views/user/__tests__/KeysView.spec.ts frontend/src/views/user/UsageView.vue frontend/src/views/user/__tests__/UsageView.spec.ts frontend/src/components/admin/usage/UsageTable.vue frontend/src/components/admin/usage/__tests__/UsageTable.spec.ts frontend/src/i18n/locales/en/dashboard.ts frontend/src/i18n/locales/zh/dashboard.ts frontend/src/types/index.ts
git commit -m "feat: configure and inspect API key failover"
```

### Task 16: Remove the frontend administrator failover editor

**Files:**
- Delete: `frontend/src/components/admin/group/RouteFailoverModal.vue`
- Delete: `frontend/src/components/admin/group/__tests__/RouteFailoverModal.spec.ts`
- Delete: `frontend/src/api/__tests__/admin.groups.routeFailover.spec.ts`
- Modify: `frontend/src/api/admin/groups.ts`
- Modify: `frontend/src/views/admin/GroupsView.vue`
- Modify: `frontend/src/types/index.ts`
- Modify: `frontend/src/i18n/locales/en/admin/overview.ts`
- Modify: `frontend/src/i18n/locales/zh/admin/overview.ts`
- Create: `frontend/src/views/admin/__tests__/GroupsView.routeFailoverRetired.spec.ts`

- [ ] **Step 1: Write the failing retirement test**

```ts
it('does not expose administrator route failover actions', async () => {
  const wrapper = await mountGroupsView()
  expect(wrapper.text()).not.toContain('Route failover')
  expect(wrapper.findComponent({ name: 'RouteFailoverModal' }).exists()).toBe(false)
})
```

- [ ] **Step 2: Run and verify failure**

Run from `frontend`: `pnpm exec vitest run src/views/admin/__tests__/GroupsView.routeFailoverRetired.spec.ts`

Expected: FAIL while the action and modal remain.

- [ ] **Step 3: Remove the API, types, modal, state, action, and locale block**

Delete `RouteFailoverPolicy`, `RouteFailoverTarget`, their input types, the three admin API methods, modal import/render/state/handlers, and the administrator locale block. Do not remove unrelated group fallback settings such as Claude Code invalid-request fallback.

- [ ] **Step 4: Run frontend checks**

Run from `frontend`:

```bash
pnpm exec vitest run src/views/admin/__tests__/GroupsView.routeFailoverRetired.spec.ts
pnpm run typecheck
pnpm run lint:check
```

Expected: PASS. `rg -n 'RouteFailoverModal|getRouteFailoverPolicy|saveRouteFailoverPolicy|deleteRouteFailoverPolicy' frontend/src` returns no matches.

- [ ] **Step 5: Commit**

```bash
git add -A frontend/src/components/admin/group frontend/src/api frontend/src/views/admin/GroupsView.vue frontend/src/views/admin/__tests__/GroupsView.routeFailoverRetired.spec.ts frontend/src/types/index.ts frontend/src/i18n/locales/en/admin/overview.ts frontend/src/i18n/locales/zh/admin/overview.ts
git commit -m "refactor: retire admin route failover editor"
```

### Task 17: Run full automated regression and inspect generated changes

**Files:**
- Modify only files required by failures attributable to this feature.

- [ ] **Step 1: Run targeted backend suites**

Run from `backend`:

```bash
go test ./internal/service ./internal/repository ./internal/handler ./internal/server/middleware ./internal/server/routes -count=1
```

Expected: PASS.

- [ ] **Step 2: Run full backend tests and build**

Run from `backend`:

```bash
go test ./... -count=1
go build ./cmd/server
```

Expected: PASS.

- [ ] **Step 3: Run full frontend verification**

Run from `frontend`:

```bash
pnpm run typecheck
pnpm run lint:check
pnpm exec vitest run
pnpm run build
```

Expected: PASS.

- [ ] **Step 4: Inspect scope and stale runtime references**

Run from repository root:

```bash
git status --short
git diff --check
rg -n 'route_failover_policies|route_failover_targets' backend/internal frontend/src
rg -n 'source_group_id|route_fallback_used|route_attempt_count|route_fallback_reason|route_sticky_hit' backend frontend/src
```

Expected: legacy table names appear only in migration/compatibility tests or inert repository code with no provider; every new usage field appears in schema, insert, scan, service, DTO, and frontend type paths.

- [ ] **Step 5: Commit verification-only fixes if any were required**

```bash
git add backend frontend
git commit -m "test: cover API key route failover regression"
```

Skip this commit when the worktree has no verification fix.

### Task 18: Deploy and perform isolated real acceptance on `172.16.22.73`

**Files:**
- Use without committing: `tools/deploy_sub2api_hybrid_73.py`
- Use without committing: `tools/test_route_failover_73.py`
- Use without committing: `tools/test_sticky_session_73.py`

- [ ] **Step 1: Confirm the target identity before any mutation**

Run from repository root with the existing credential mechanism:

```powershell
python tools/deploy_sub2api_hybrid_73.py --help
python tools/test_route_failover_73.py --help
```

The scripts must print or enforce `TEST_HOST = "172.16.22.73"` and `PRODUCTION_HOST = "47.119.114.46"`. Abort if deployment targets production, port `3001`, or the `new-api` Docker stack.

- [ ] **Step 2: Update the local acceptance harness for the new user-owned contract**

Keep the scripts untracked. Replace the administrator policy creation with API-key creation:

```json
{
  "name": "route-failover-live-test",
  "group_id": "<group-1>",
  "fallback_group_ids": ["<group-2>", "<group-3>"],
  "failover_risk_acknowledged": true
}
```

Update usage assertions to require `source_group_id = group-1`, `group_id = actual group`, correct route attempt metadata, and the actual group's multiplier/subscription ID. Update sticky assertions to scan `route:sticky:key:{api_key_id}:*`, decode route version/effective group, and require a TTL within `3540..3600` seconds.

- [ ] **Step 3: Deploy only to 73 with rollback capture**

Run from repository root after setting the existing test SSH password environment variable:

```powershell
python tools/deploy_sub2api_hybrid_73.py --tag api-key-route-failover
```

Expected: the `sub2api` container on `172.16.22.73:8080` becomes healthy, migration 221 is applied, and the script prints deployed image, rollback image, and compose backup. The retired administrator endpoint returns `404`.

- [ ] **Step 4: Import the three production AiHub accounts into three isolated test groups**

Run:

```powershell
python tools/test_route_failover_73.py
```

The script may read credentials/proxy configuration for production account IDs `2909`, `3448`, and `3449`, but all writes go to `172.16.22.73`. It creates one isolated group per account and an API key whose primary/fallback order matches those groups.

- [ ] **Step 5: Verify primary, first fallback, second fallback, and no-fallback behavior**

Acceptance sequence:

```text
all accounts schedulable             => group 1/account 1 succeeds
group 1 account unschedulable         => group 2/account 2 succeeds
groups 1 and 2 unschedulable          => group 3/account 3 succeeds
all three unschedulable               => request fails after exactly 3 group attempts
business 400 against primary          => no fallback usage row and no sticky write
```

For each success, require one charge and one usage row. Confirm the row's effective group, source group, attempt count, fallback reason, sticky flag, multiplier, and subscription ID.

- [ ] **Step 6: Verify one-hour sliding stickiness and version invalidation**

Run:

```powershell
python tools/test_sticky_session_73.py
```

Use the same explicit session across multiple turns. After fallback succeeds, re-enable the primary and prove the next request remains on the fallback while refreshing TTL. Edit the API key chain, prove `route_config_version` increments, and prove the next request ignores/deletes the old binding and follows the new chain.

- [ ] **Step 7: Verify Redis failure degradation**

Temporarily block only route-sticky/circuit access in the isolated test flow or use a test process with a deliberately unavailable Redis route store. Prove the request still evaluates `primary -> fallback 1 -> fallback 2`, succeeds, logs the state-store error, and writes no sticky state. Do not stop the shared Redis container if that would disrupt unrelated test services.

- [ ] **Step 8: Restore and clean test-only state**

The harness must restore account schedulability and credentials in `finally`, delete the temporary user/key/groups/accounts/proxy that it created, and delete matching route-sticky keys. Keep the deployed test build for user inspection unless health checks fail; use the captured rollback image only on failure.

- [ ] **Step 9: Record acceptance evidence without secrets**

Capture IDs, HTTP statuses, request IDs, source/effective group IDs, route attempt counts, multipliers, TTL ranges, image/version, and cleanup result. Never include API-key plaintext, upstream credentials, proxy passwords, SSH passwords, or Redis values containing sensitive material.

## Final Acceptance Matrix

| Requirement | Automated proof | Live proof |
|---|---|---|
| 0-5 ordered fallbacks; sixth rejected | Tasks 2-4 | create/edit on 73 |
| Risk acknowledgement | Tasks 3, 14, 15 | create key on 73 |
| No config means primary only | Tasks 3, 7 | no-fallback case |
| Old keys do not inherit policies | Tasks 1, 3, 13 | inspect migrated old key |
| Capacity/connection/timeout/429/5xx fail over | Tasks 7, 10-12 | fault sequence |
| Business/local errors do not fail over | Tasks 7, 10-12 | primary 400 case |
| Unsupported model is skipped | Tasks 7, 11, 12 | optional group capability fault |
| Actual group owns admission and billing | Tasks 7, 9, 11, 12 | usage/billing assertions |
| One successful charge | Task 9 | one usage row per request ID |
| Source/effective group audit | Tasks 9, 15 | PostgreSQL usage row |
| One-hour sliding route stickiness | Task 5, 7 | sticky script TTL |
| Route edit invalidates old binding | Tasks 3, 5, 7 | version edit case |
| Stream semantic output blocks replay | Tasks 10-12 | targeted protocol tests |
| Redis failure remains available | Tasks 5-7 | isolated fail-open case |
| Admin policy surface retired | Tasks 13, 16 | endpoint 404 |
| Production untouched; new-api untouched | safety checks | target evidence |
