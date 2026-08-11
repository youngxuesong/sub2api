# API Key Route Failover Design

Date: 2026-08-11
Status: Approved; implementation plan ready

## 1. Objective

Move route failover ownership from administrator-managed source-group policies to
each API key. A user chooses an ordered list of fallback groups while creating or
editing a key. When a request is executed by a fallback group, authorization,
subscription admission, quotas, limits, pricing, billing, and usage attribution
all use that effective group.

The feature must remain simple in the user interface: choose fallback groups and
their order. Users do not configure model mappings, attempt budgets, circuit
thresholds, or recovery parameters.

Environment identities are fixed:

- Production: `47.119.114.46`
- Testing: `172.16.22.73`

Implementation validation and fault injection happen only on the testing
environment. Test configuration or data must never overwrite production.

## 2. Superseded Decisions

This document supersedes the following decisions in
`2026-08-05-route-failover-v2-design.md`:

- Route failover is no longer configured by administrators per source group.
- Billing and entitlement no longer stay on the source group after failover.
- Users do not select administrator-defined profiles or target pools.
- Per-target model mappings are removed from the user-facing failover model.

The earlier design's failure classification, replay safety, streaming safety,
circuit-breaker principles, and observability requirements remain applicable
where they do not conflict with this document.

## 3. Confirmed Product Decisions

- `api_keys.group_id` remains the primary group.
- A key may have zero to five ordered fallback groups.
- No fallback configuration means primary-only behavior.
- Existing keys remain primary-only after migration.
- Users may edit the fallback chain at any time; changes affect new requests.
- A fallback uses the original requested model. Users cannot map models.
- An unsupported model causes that candidate to be skipped.
- No schedulable account or concurrency capacity, connection failures, timeouts,
  upstream `429`, and upstream `5xx` responses are eligible for cross-group
  failover.
- Request validation failures, authentication failures, policy failures, and
  other business `4xx` responses do not trigger failover.
- A successful fallback becomes sticky for the same session for a sliding one
  hour window.
- A fallback is charged using the effective group's prices and limits.
- Enabling or changing a non-empty fallback chain requires explicit risk
  acknowledgement.
- The administrator route-failover editor and its runtime policy are retired.

## 4. Architecture

The gateway keeps the existing protocol handlers and account schedulers, but
cross-group execution is owned by one route-execution boundary:

```text
Protocol handler
  -> API key authentication and route config
  -> Route executor
     -> effective-group billing admission
     -> account scheduler for that group
     -> protocol forwarder
  -> usage and route audit
```

Responsibilities are separated as follows:

- API key service: create, update, validate, and return the ordered fallback
  chain.
- Route configuration repository: persist targets transactionally and load them
  with the API key authentication payload.
- Route executor: order candidates, apply sticky routing and circuits, enforce
  replay safety, classify failures, and move between groups.
- Effective-group billing admission: resolve the user's entitlement,
  subscription, quota, concurrency, rate limits, pricing, and group multiplier
  for each attempted candidate.
- Account scheduler: select and retry accounts within one group using its
  existing account-level budget.
- Usage recorder: persist primary and effective group identities and charge once
  using the successful candidate's billing context.

Protocol handlers call the route executor once for cross-group routing. A
handler-owned retry loop must not independently restart the fallback chain.

## 5. Persistent Data

### 5.1 API Key Fields

Add these fields to `api_keys`:

```text
route_config_version              BIGINT NOT NULL DEFAULT 1
failover_risk_acknowledged_at     TIMESTAMPTZ NULL
```

`route_config_version` increments whenever the primary group or fallback chain
changes. It invalidates old route-sticky records without scanning Redis.

`failover_risk_acknowledged_at` records the last accepted non-empty chain. It is
cleared when failover is disabled and replaced with the current time when a new
or changed non-empty chain is acknowledged.

### 5.2 API Key Fallback Targets

Create `api_key_route_failover_targets`:

```text
id                 BIGSERIAL PRIMARY KEY
api_key_id         BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE
target_group_id    BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE
priority           SMALLINT NOT NULL CHECK (priority BETWEEN 1 AND 5)
created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()

UNIQUE (api_key_id, target_group_id)
UNIQUE (api_key_id, priority)
```

Service validation enforces constraints that cannot be expressed locally:

- zero to five targets;
- priorities are contiguous and match request-array order;
- target IDs are positive and unique;
- a target is not the key's primary group;
- the user may currently bind the target group;
- source and target groups are active, concrete, non-composite groups with the
  same protocol platform.

Subscription type does not need to match. A standard and subscription group may
participate in the same chain because each candidate uses its own billing rules.

### 5.3 Usage Fields

Add route audit fields to `usage_logs`:

```text
source_group_id          BIGINT NULL
route_fallback_used      BOOLEAN NOT NULL DEFAULT FALSE
route_attempt_count      SMALLINT NOT NULL DEFAULT 1
route_fallback_reason    VARCHAR(64) NULL
route_sticky_hit         BOOLEAN NOT NULL DEFAULT FALSE
```

For new records, existing `usage_logs.group_id` means the effective group that
actually completed the request. `source_group_id` means the API key's primary
group. Historical rows with a null `source_group_id` treat `group_id` as both
source and effective group.

`usage_logs.subscription_id`, `rate_multiplier`, billing tier, total cost, and
related billing snapshots also come from the effective group.

## 6. API Contract

Create and update requests add:

```json
{
  "fallback_group_ids": [12, 18, 23],
  "failover_risk_acknowledged": true
}
```

Semantics are:

- On create, an omitted or empty `fallback_group_ids` creates a primary-only key.
- On update, an omitted `fallback_group_ids` leaves the chain unchanged unless
  the primary group changes.
- An empty update array disables failover.
- A non-empty update array replaces the entire ordered chain.
- Changing the primary group treats an omitted fallback array as empty, so stale
  targets cannot silently remain attached to a new primary.
- A changed non-empty chain requires `failover_risk_acknowledged: true`.
- Disabling failover does not require acknowledgement.

API key responses add:

```json
{
  "failover_enabled": true,
  "fallback_groups": [
    {
      "id": 12,
      "name": "Stable OpenAI",
      "platform": "openai",
      "subscription_type": "standard",
      "rate_multiplier": 1.5,
      "priority": 1
    }
  ]
}
```

Responses never expose another user's subscription or private group details.
The existing available-groups and user-group-rate APIs provide selector data;
the backend remains authoritative for all validation.

Validation errors use stable codes:

```text
FAILOVER_ACK_REQUIRED
FAILOVER_TOO_MANY_TARGETS
FAILOVER_DUPLICATE_TARGET
FAILOVER_PRIMARY_AS_TARGET
FAILOVER_TARGET_NOT_ALLOWED
FAILOVER_TARGET_INCOMPATIBLE
```

Create and update save the API key, targets, version, and acknowledgement in one
database transaction. An idempotent create replay returns the original key and
chain rather than inserting duplicate targets.

## 7. Authorization and Candidate Admission

Creation and editing use the same group-binding rules as a primary group:

- a subscription group requires an active subscription for that user;
- a standard group uses public, exclusive, and explicitly allowed group rules;
- inactive or deleted groups cannot be selected.

The route executor rechecks effective-group admission immediately before an
attempt. Cached authorization data is invalidated when a subscription, group,
user-group permission, primary group, or fallback chain changes. A target that
has become ineligible is skipped; it does not invalidate the API key.

The primary group's local authentication, user balance, API key quota, or local
policy failure does not trigger failover. A lack of schedulable accounts or
concurrency capacity is a routing-capacity failure and does trigger failover,
but does not affect the circuit breaker. Once a capacity or upstream-retryable
primary failure occurs, each fallback performs its own admission. An ineligible
fallback is skipped and execution continues to the next target.

## 8. Candidate and Session Flow

The normal candidate order is:

```text
primary -> fallback 1 -> fallback 2 -> ... -> fallback 5
```

Execution is:

1. Authenticate the key and load its primary group, targets, and route version.
2. Derive the existing stable session hash. Without a stable session hash, do
   not read or write route stickiness.
3. If a sticky record matches the key version and still references an eligible
   configured target that supports the requested model, attempt that target
   first.
4. Otherwise, start with the primary group.
5. Resolve the candidate's billing context, model capability, circuit state,
   account, concurrency, and limits.
6. Forward the unchanged requested model through that candidate's normal channel
   mapping. API-key route failover itself performs no model mapping.
7. On success, charge once using the effective billing context, record usage, and
   write or refresh fallback stickiness.
8. On an eligible safe failure, release reservations and slots, then attempt the
   next untried candidate.
9. On an ineligible or unsafe failure, return immediately.

If a sticky target fails with a retryable error, delete the binding and rebuild
the order as primary followed by untried fallbacks. A candidate is never called
twice in one request.

The route switch budget begins after the primary produces its first eligible
failover failure. It is 20 seconds and is also bounded by the request context and
client cancellation. The budget ends as soon as a candidate safely accepts the
request; it does not limit normal generation or streaming duration after that
point.

## 9. Route Stickiness

Redis stores:

```text
route:sticky:key:{api_key_id}:{session_hash}
```

The value contains:

```text
route_config_version
effective_group_id
```

Rules are:

- write only after a fallback completes successfully;
- use a sliding 3,600-second TTL;
- refresh the TTL after every successful request on the sticky target;
- ignore and delete records with a stale version or removed target;
- delete the binding if the target becomes unavailable or fails retryably;
- never bind requests without a stable session hash;
- never expose the binding or internal group ID to the API client.

An API key edit increments the version, so all old sessions use the new chain on
their next request. Requests already executing keep the immutable configuration
snapshot with which they started.

## 10. Failure and Replay Rules

Cross-group failover is allowed only when all conditions are true:

```text
failure is routing capacity, connection failure, timeout, upstream 429, or upstream 5xx
AND replay is safe
AND no semantic output has been committed
AND the route switch budget allows another attempt
AND the request has not been canceled
```

Malformed input, unsupported request semantics, authentication failures, local
quota or balance failures, policy rejection, context-limit errors, and other
business `4xx` responses return immediately.

Account-local recovery may choose another account inside the same group using
the scheduler's existing budget. Cross-group failover starts when the group has
no schedulable account or concurrency capacity, or still ends in an eligible
route failure. Capacity failures permit failover but do not count as circuit
failures.

For streaming requests, an SSE comment or transport heartbeat is not semantic
output. A text token, tool call, business SSE event, irreversible WebSocket
frame, media task ID, or other client-visible result commits the response. Once
committed, no cross-group replay occurs and streams from different candidates
are never concatenated.

Asynchronous or non-idempotent endpoints may fail over only when their adapter
can prove the upstream did not accept the operation or supplies a stable
idempotency key. Otherwise they return the original failure.

## 11. Circuit Breaker and Store Failures

Users and administrators do not configure circuit parameters. Circuit health is
shared by effective target group and requested model:

```text
failure window:             60 seconds
failure threshold:          5 consecutive eligible failures
open cooldown:              60 seconds
half-open lease:            15 seconds
recovery success threshold: 2 consecutive successes
```

An open target is skipped without consuming an upstream attempt. Circuit state
records only replay-safe connection, timeout, `429`, and `5xx` failures. Request,
authorization, local billing, unsupported-model, and client-cancellation errors
do not affect it.

If Redis is unavailable, the request remains available. Distributed route
stickiness and shared circuit coordination are disabled, and candidates are
evaluated in configured order. The error is logged and metered. Redis failure
must never make an otherwise valid request fail.

If PostgreSQL is temporarily unavailable after authentication data has been
loaded, the gateway uses the last valid cached API key route configuration. If
no route configuration has ever been loaded, it fails safely to primary-only
routing. The hot route path does not query PostgreSQL per candidate.

## 12. Billing Semantics

The effective group owns all group-scoped rules after failover:

- group model pricing and rate multiplier;
- user-specific group rate override;
- active subscription and subscription quota;
- group/user RPM and concurrency limits;
- channel restrictions and profit-control rules;
- effective subscription ID and billing tier in usage records.

API key quota and API key rolling rate limits remain key-wide. They accumulate
the actual amount charged by whichever group succeeds.

Candidate admission creates only reversible reservations. A failed attempt must
release concurrency, quota, and rate-limit reservations before the next group is
tried. Exactly one successful billing operation is allowed per API key and
request ID. Existing usage idempotency remains the final duplicate-charge guard.

Example:

```text
primary group multiplier:   0.5x
effective fallback:         1.5x
charged multiplier:         1.5x
usage_logs.group_id:        fallback group
usage_logs.source_group_id: primary group
```

This rule prevents a user from selecting a cheap primary while consuming a more
expensive fallback at the primary price.

## 13. User Interface

The API key create and edit dialog adds a `Route failover` toggle below the
primary group selector. It is off by default.

When enabled:

- show only groups the user may bind and that match the primary protocol
  platform;
- exclude the primary group and already selected targets;
- show group name, platform, subscription type, and the user's effective rate
  multiplier;
- allow ordering and removal with existing icon controls;
- enforce a stable maximum of five rows;
- do not expose model mapping, circuit, timeout, or retry controls.

The warning states that a failed primary request may be executed by another
group and that the actual group's price, quota, rate limit, subscription, and
concurrency rules apply. The create action remains disabled until the user checks
`I understand that failover is billed by the actual group`.

Editing an unchanged chain does not require a new acknowledgement. Enabling or
changing a non-empty chain does. Disabling the chain does not.

Changing the primary group clears selected fallback rows in the UI. The key list
shows `Primary + N fallbacks`; expanding it shows the ordered group names and
multipliers. Usage details show the effective group by default and, on failover,
show `Primary -> Effective`.

The administrator group failover modal, routes, and navigation entry are
removed. Administrators may inspect usage and route audit data but do not edit a
user's fallback chain through the retired group-policy interface.

## 14. Migration and Compatibility

Migration is additive for API keys and usage logs:

1. Add API key route-version and acknowledgement fields.
2. Create the API-key target table and indexes.
3. Add usage route-audit fields and indexes required by route queries.
4. Deploy with API-key failover disabled.
5. Stop registering the administrator route-policy API and remove its UI.
6. Enable API-key failover only for dedicated testing on `172.16.22.73`.

Existing API keys receive version `1` and no target rows. No administrator policy
is copied to a key, and no fallback is inferred.

The legacy `route_failover_policies` and `route_failover_targets` tables remain
for one stable release but are not loaded, written, or exposed by the new
runtime. Their rows are inert. Physical deletion is a separate, explicitly
approved cleanup after rollback confidence is established.

Legacy usage rows remain readable through the null `source_group_id` fallback
rule. Existing group dashboards must be updated so new records aggregate by the
effective `group_id` and can optionally filter by `source_group_id`.

## 15. Observability

Every route execution emits structured data sufficient to reconstruct:

```text
request_id
api_key_id
source_group_id
ordered candidate group IDs
attempted candidate group IDs
failure class and status per attempt
effective_group_id
route_config_version
sticky hit or miss
route attempt count
effective subscription and rate multiplier
billing result
```

API key plaintext and upstream credentials are never logged. Ordinary client
responses do not reveal fallback topology or internal group IDs.

Required counters include route requests, fallback successes, candidate skips,
circuit transitions, sticky hits, invalid sticky records, configuration
validation failures, state-store errors, route-switch duration, and billing
idempotency conflicts.

## 16. Test Strategy

### 16.1 Automated Tests

Repository and service tests cover:

- zero through five targets and rejection of a sixth;
- duplicate, primary-as-target, unauthorized, inactive, composite, and
  cross-platform targets;
- transactional create, update, disable, primary change, and idempotent replay;
- unchanged versus changed-chain risk acknowledgement;
- no migration of administrator policies or old API keys;
- ordered candidates and unsupported-model skipping;
- exact failover error classification and circuit recording;
- safe and unsafe replay before and after semantic stream commitment;
- effective-group subscription, quota, multiplier, limits, and billing;
- failed-attempt reservation release and request-ID charge idempotency;
- sliding sticky TTL, retryable sticky escape, and route-version invalidation;
- Redis and PostgreSQL degradation behavior;
- API response redaction and ownership checks;
- frontend selection, ordering, maximum, warning, acknowledgement, and edit
  behavior.

### 16.2 Testing Environment Verification

On `172.16.22.73`, use three dedicated AiHub groups and temporary users/keys to
verify:

1. A healthy primary never calls a fallback.
2. Primary capacity exhaustion, connection failure, timeout, `429`, and `5xx`
   select fallbacks in the configured order.
3. A business `4xx` does not leave the primary group.
4. A fallback lacking the requested model is skipped.
5. A successful fallback remains selected for the same session and refreshes
   the one-hour TTL.
6. Changing target order invalidates the old sticky version on the next request.
7. Distinct group multipliers prove the final charge, subscription usage, and
   usage row belong to the effective group.
8. Usage details preserve both primary and effective group identities.
9. A partial streaming response is never replayed or concatenated.
10. Redis loss removes stickiness without removing request availability.

Restore account schedulability and remove temporary users, keys, subscriptions,
route targets, sticky keys, and test-only pricing after verification. Do not
write any testing configuration or data to `47.119.114.46`.

## 17. Acceptance Criteria

The feature is accepted when all of the following are demonstrated:

- A user can create and edit an API key with zero to five ordered fallback
  groups and cannot select a group they may not use.
- Risk acknowledgement is enforced by both frontend and backend.
- Existing keys remain primary-only without user action.
- Eligible failures move through the configured groups in order while business
  `4xx` failures do not.
- The same session stays on a successful fallback for a sliding hour and a
  configuration edit invalidates that binding.
- The successful effective group supplies entitlement, quota, rate limits,
  concurrency, subscription, pricing, multiplier, and usage attribution.
- No failed attempt causes duplicate billing or leaks a reservation.
- No cross-group replay occurs after semantic output is committed.
- Redis failure preserves request availability.
- The retired administrator policy has no runtime effect.
- Automated tests pass and the dedicated three-group verification succeeds on
  `172.16.22.73` without any production mutation.
