# Route Failover V2 Design

Date: 2026-08-05
Status: Approved design, pending implementation plan

## 1. Objective

Build production-grade, user-transparent route failover for sub2api. The design must distinguish request, account, route, provider, and local infrastructure failures; keep authorization and billing tied to the source group; preserve streaming correctness; and provide observable, recoverable failover across upstream account pools.

The architecture covers all protocols, but implementation is phased. Phase 1 is configured only by administrators, supports concrete same-platform groups, and covers the core text protocols. Ordinary users do not configure target groups or circuit thresholds.

Environment roles are fixed:

- `47.119.114.46`: production
- `172.16.22.73`: testing

All validation and fault injection happen on 73 before any policy is enabled on 46.

## 2. Current Implementation

The uncommitted implementation currently provides:

- PostgreSQL policy and target persistence.
- An in-memory policy snapshot.
- Redis circuit counters and half-open leases.
- Admin CRUD and a route failover modal.
- Source API key and source group billing identity.
- A guard that stops failover after semantic streaming output is committed.

Its runtime flow is:

```text
Protocol Handler
  -> SelectAccountWithLoadAwareness
     -> primary group accounts
     -> fallback group accounts after primary exhaustion
  -> Handler-owned retry loop
```

This is an account-pool fallback MVP, not route high availability. Important limitations are:

- OpenAI and Grok native handlers do not use the planner.
- The primary route has no circuit identity or health state.
- Provider failures generally retry or exhaust primary accounts instead of immediately changing routes.
- Route and Handler attempt budgets are independent.
- Account- and request-scoped failures can poison route circuits.
- Circuit checks use the requested model while recordings can use the mapped model.
- Half-open leases are acquired while planning candidates, even if the candidate is never attempted.
- Channel and composite routing are resolved against the source group rather than every effective candidate.
- Legacy Claude Code fallback can make the reported effective group differ from the account's actual group.
- Route stickiness and route audit fields are absent.
- Candidate validation performs PostgreSQL reads on the request path.

The current implementation must not be enabled in production.

## 3. Design Decision

### 3.1 Selected Architecture

Introduce one deep `RouteExecutor` module at the seam between protocol handlers and account scheduling:

```text
Protocol Handler
  -> RouteExecutor
     -> AccountScheduler
        -> Upstream Adapter
           -> Egress Infrastructure
```

Responsibilities are deliberately separated:

- Protocol Handler: parse requests, authenticate, maintain the downstream connection, and render protocol responses.
- RouteExecutor: candidate selection, budgets, circuit state, failure classification, model mapping, route stickiness, and route audit.
- AccountScheduler: account health, account selection, concurrency, RPM, account affinity, and account-local retries.
- Upstream Adapter: provider protocol conversion and forwarding.
- Egress Infrastructure: Docker networking, WARP, DNS, and HTTP/SOCKS proxy availability.

Business route failover stays in sub2api because it requires group authorization, model mapping, account scheduling, billing, and streaming state. Shared WARP/DNS/proxy failover remains outside the business circuit.

### 3.2 Rejected Alternatives

Extending the current account selector was rejected because it keeps account and route retry semantics coupled and duplicates behavior across handlers.

Moving all failover to Nginx, Mihomo, or an upstream aggregator was rejected because those layers cannot correctly evaluate API key permissions, model compatibility, billing identity, account health, or semantic stream commitment.

## 4. Domain Invariants

The source group and effective candidate group have different meanings:

```text
Source group
  authorization, entitlement, quota, and user billing identity

Candidate group
  account pool, upstream capability, channel mapping, and actual resource cost
```

The following invariants apply:

- User billing always remains attached to the source group.
- Actual group, account, model, and resource cost are audited separately.
- Phase 1 candidates must be active concrete groups on the same platform as the source group.
- An unchanged model name may pass through.
- A changed model requires an explicit administrator-approved mapping.
- Unsupported mappings skip the candidate as a configuration problem and do not count as runtime route failures.
- A primary candidate is explicit and has the same health semantics as fallback candidates.
- Candidate IDs are stable across edits so circuit and audit identities remain valid.

## 5. Persistent Configuration

### 5.1 Route Policies

`route_policies` contains:

```text
id
source_group_id                 unique
enabled
max_route_attempts
max_account_attempts_per_route
max_total_attempts
failover_timeout_ms
failure_threshold
failure_window_seconds
open_cooldown_seconds
recovery_success_threshold
created_at
updated_at
```

The three attempt limits have distinct meanings:

- `max_route_attempts`: maximum distinct candidates considered for execution.
- `max_account_attempts_per_route`: maximum accounts actually called within one candidate.
- `max_total_attempts`: maximum upstream calls across the entire request.

`failover_timeout_ms` limits time spent before the first semantic output. The earliest budget or cancellation condition always wins.

### 5.2 Route Candidates

`route_candidates` contains:

```text
id
policy_id
role                 primary | fallback
target_group_id
priority
enabled
model_mapping
created_at
updated_at
```

Each enabled policy has exactly one primary candidate pointing to the source group. Candidate updates preserve IDs; repositories must not replace all candidates by deleting and reinserting them.

### 5.3 Initial Defaults

Initial defaults are:

```text
max_route_attempts: 3
max_account_attempts_per_route: 2
max_total_attempts: 4
failover_timeout_ms: 20000
failure_threshold: 5
failure_window_seconds: 60
open_cooldown_seconds: 60
recovery_success_threshold: 2
half_open_concurrency: 1
route_sticky_ttl_seconds: 3600
```

Defaults must have one backend source of truth. Migrations and the frontend display backend-provided values instead of maintaining independent copies.

## 6. Runtime State

Redis stores disposable distributed state:

```text
route:circuit:{policy}:{candidate}
route:capability:{policy}:{candidate}:{endpoint}:{requested_model}
route:lease:{health_key}
route:sticky:{source_group}:{session_hash}
```

There are two health levels:

- Candidate route health for DNS, TCP, TLS, route-specific proxy, and provider-wide failures.
- Capability health for failures isolated to an endpoint and requested model.

A candidate is executable only when both levels allow it.

The sticky value records the policy, candidate, effective group, and effective model. Existing sessions stay on a successful fallback until the one-hour TTL expires. Recovered primary routes receive new sessions; existing fallback sessions do not oscillate back mid-conversation.

## 7. RouteExecutor Interface

Conceptually, the external interface is:

```go
Execute(ctx, RouteRequest, RouteForwarder) RouteExecutionResult
```

`RouteRequest` contains only caller-known facts:

```text
source_group_id
requested_model
endpoint
session_hash
stream
request_id or idempotency_key
```

Each protocol supplies a `RouteForwarder` adapter. It receives a fully resolved route attempt, including candidate, effective group, effective model, and selected account, and returns a standardized outcome containing the native result, failure classification, replay safety, upstream acceptance, and semantic commit state.

Handlers call `Execute` once and do not own a second account or route retry loop.

## 8. Execution Flow

1. The Handler authenticates and parses the request.
2. Access policies such as Claude Code-only fallback are resolved before health routing.
3. The Handler constructs `RouteRequest` and calls `RouteExecutor`.
4. The executor loads the last valid policy snapshot.
5. It reads route stickiness and builds the ordered candidate list.
6. For each candidate, it validates the cached configuration and current capability.
7. It checks candidate and capability circuits.
8. A half-open lease is acquired only immediately before a real upstream attempt.
9. Channel and model mapping are resolved for the effective candidate.
10. The account scheduler selects an account in that candidate group.
11. The executor acquires the account slot and calls the protocol forwarder.
12. The standardized outcome is classified.
13. Success updates circuits, audit data, usage, and route stickiness.
14. Failure either returns, selects another account, or selects another route according to its classification and replay safety.

Without stickiness, candidate order is primary followed by configured fallbacks. With stickiness, the sticky candidate is first. Invalid or unhealthy sticky candidates are skipped; a successful replacement updates the sticky value.

## 9. Failure Classification

| Class | Examples | Action | Route circuit |
| --- | --- | --- | --- |
| request | malformed input, unsupported model, context limit, policy rejection | return immediately | no |
| account | expired token, account quota, account-level 401/429 | select another account in the route | no |
| capacity | no schedulable accounts or no concurrency slot | select another route for this request | no |
| route | destination unreachable, route proxy failure, candidate channel invalid | select another route | yes |
| provider | provider outage, provider-wide rate limit, classified 502/503/504 | select another route | yes |
| infrastructure | shared WARP, DNS, Redis, PostgreSQL, or host network failure | infrastructure degradation and alert | no |
| canceled | client disconnect or request deadline | stop | no |
| unknown | failure cannot be safely classified | stop cross-route replay | no |

The existing `GatewayFailureScope` is extended or mapped into this taxonomy. An `UpstreamFailoverError` alone is not evidence of route failure.

## 10. Replay and Streaming Safety

Every attempt outcome includes:

```text
replay_safety             safe | unsafe | unknown
upstream_accepted         true | false | unknown
semantic_output_committed true | false
```

Cross-route replay is permitted only when:

```text
failure class permits failover
AND replay_safety is safe
AND semantic output is not committed
AND all budgets allow another attempt
```

Connection, TLS, and pre-acceptance failures are typical safe cases. Provider responses are replayed only when their adapter explicitly classifies them as safe.

Once a text token, tool call, business SSE event, irreversible WebSocket frame, or upstream task ID is committed, no cross-route replay occurs. SSE comments and non-semantic heartbeats do not commit the response. Streams from different upstreams are never concatenated.

Media and asynchronous task creation require an upstream idempotency key or task reconciliation before automatic failover is enabled.

## 11. Circuit State Machine

Circuits use `CLOSED`, `OPEN`, and `HALF_OPEN` states.

`CLOSED` records only safe, route- or provider-scoped failures within a sliding window. Reaching the threshold transitions to `OPEN`.

`OPEN` skips the candidate without opening an upstream connection or consuming an upstream attempt. After cooldown, one request may acquire the distributed half-open lease.

`HALF_OPEN` allows one real probe at a time. A failed probe immediately reopens the circuit. Consecutive successful probes close it. Leases are released when an acquired attempt is abandoned before forwarding.

Passive production traffic is the Phase 1 health signal. Optional provider-specific active probes may later assist recovery, but a models-list response cannot prove that generation is healthy.

## 12. Store Failure Behavior

If PostgreSQL is unavailable, the executor continues using the last valid snapshot and exposes snapshot age and staleness. If no snapshot has ever loaded, only the source group is used.

If Redis is unavailable, requests remain available using short-lived process-local circuit state. Distributed stickiness and half-open coordination are disabled, total attempts are reduced, and an alert is emitted. Redis failures are never silently discarded.

Shared WARP, DNS, or host egress failures are infrastructure incidents. They do not open business candidate circuits. Egress health and reconnection remain managed by the Docker infrastructure layer.

## 13. Audit and Observability

The source `usage_logs.group_id` remains the billing group. Usage records add:

```text
effective_group_id
route_policy_id
route_candidate_id
fallback_used
route_attempt_count
account_attempt_count
fallback_reason
requested_model
effective_model
```

Required metrics are:

```text
route_requests_total
route_fallback_total
route_attempts_total
route_attempt_duration
route_circuit_state
route_circuit_transitions_total
route_failures_total{scope,reason}
route_sticky_hits_total
route_policy_snapshot_age
route_state_store_errors_total
```

Operators can trace a request ID to its source group, effective candidate, actual account, mappings, attempts, fallback reason, user charge, and actual resource cost.

Successful failover does not expose internal group or account IDs to ordinary clients. Optional debugging headers are disabled in production by default.

## 14. Administration

Phase 1 configuration is administrator-only and attached to one source group. The UI has:

- Basic policy: enabled state, total timeout, and accurately named attempt budgets.
- Candidates: explicit primary, ordered fallbacks, enable state, and model mappings.
- Runtime state: circuit state, last failure, recovery time, success rate, fallback rate, and added latency.

Ordinary users do not configure group IDs, topology, or circuit thresholds. A later phase may let API keys choose among administrator-approved stable, low-cost, and no-fallback profiles.

## 15. Compatibility Rules

Claude Code `FallbackGroupID` remains an access-policy mechanism. It is resolved before `RouteExecutor`; the resulting source identity and actual group must be explicit and auditable.

`FallbackGroupIDOnInvalidRequest` remains separate from health failover and never affects circuit health.

Phase 1 rejects route policies for `composite` groups. Composite support is enabled only after every candidate can independently resolve its concrete platform, endpoint route, model, channel mapping, quota attribution, and audit identity. The resolved concrete platforms must still satisfy the same-platform rule.

## 16. Phased Delivery

### Phase 1: Core Module

Implement the policy snapshot, stable candidates, `RouteExecutor`, failure classifier, circuit store, sticky store, scheduler adapter, and audit recorder. Do not integrate production handlers yet.

### Phase 2: Core Text Protocols

Integrate Anthropic Messages, OpenAI Responses, OpenAI Chat Completions, Gemini Native, Gemini/Antigravity compatibility, and Grok text. Remove each Handler's old retry loop as it is integrated.

### Phase 3: Administration and Operations

Add capability validation, policy preview, circuit status, failure reasons, fallback rates, latency, snapshot health, Redis health, and policy change audit.

### Phase 4: Remaining Protocols

Integrate Count Tokens and Embeddings first. Add Images, Videos, asynchronous tasks, and WebSocket only with their replay-safety rules. Models-list aggregation remains a separate concern. Add composite groups last.

## 17. Test Matrix

Tests must prove:

- Provider failure on primary immediately selects a fallback route.
- Account 401/429 selects another account without changing route health.
- Malformed requests never change circuit state.
- Requested and mapped models use one consistent health identity.
- Primary and fallback circuits open, half-open, and recover correctly.
- One distributed half-open probe executes at a time.
- Unused half-open leases are released.
- All protocols obey the same total budgets.
- No failover or stream concatenation occurs after semantic output.
- Fallback route stickiness lasts one hour.
- Source billing, actual cost, actual group, and model audit are correct.
- Legacy Claude Code fallback cannot corrupt effective-group identity.
- PostgreSQL, Redis, WARP, provider, and client-disconnect fault cases behave as designed.
- Multi-instance circuit and lease behavior is atomic.
- Disabled policies have no observable routing behavior change.
- The request path performs zero PostgreSQL queries for policy or candidate validation.

## 18. Test Environment Rollout

On `172.16.22.73`:

1. Apply additive compatibility migrations with the feature disabled.
2. Run shadow mode that computes but does not execute new route decisions.
3. Compare old scheduler choices with RouteExecutor decisions.
4. Enable real failover only for dedicated test groups.
5. Inject account, provider, Redis, PostgreSQL, WARP, and disconnect failures.
6. Validate streaming, billing, stickiness, recovery, and audit.
7. Run concurrency and latency load tests.

Acceptance criteria are:

- Disabled-policy P95 overhead is effectively zero relative to baseline.
- Enabled-policy routing adds no more than 5 ms P95 excluding upstream time.
- Policy and candidate validation cause zero request-path PostgreSQL queries.
- A safe provider failure can succeed on the next candidate within the same client request.
- No duplicate semantic output, cross-stream concatenation, or billing identity error occurs.
- Redis failure preserves core request availability and emits an alert.

## 19. Production Rollout

On `47.119.114.46`:

1. Back up PostgreSQL and apply additive migrations.
2. Deploy code with the feature disabled.
3. Enable one internal test group.
4. Observe at least one complete peak traffic period.
5. Enable low-risk groups incrementally.
6. Expand only after error rate, latency, billing, and audit gates pass.
7. Roll back by disabling the feature flag; database rollback is not required.
8. Retain old fields and behavior for at least one stable release before cleanup.

The current MVP is never enabled on production as an intermediate step.
