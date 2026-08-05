# Route Failover V2 Text Protocol Integration Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route all approved Phase 1 text protocols through one `RouteExecutor` call while preserving protocol responses, source-group billing, account scheduling, and streaming safety.

**Architecture:** Each handler builds a small protocol adapter from three callbacks: select an account inside the resolved candidate, acquire its wait slot, and perform exactly one upstream call. The executor owns retry and route switching; handlers retain authentication, user concurrency, request parsing, response rendering, and usage submission. `off` and disabled-policy requests run the byte-for-byte legacy path; `shadow` only previews and logs decisions.

**Tech Stack:** Go 1.26.5, Gin, existing gateway/OpenAI/Gemini services, Testify, `httptest`, Wire.

---

## File Map

- Create `backend/internal/handler/route_forwarder.go`: common callback-backed adapter, commit tracking, execution-mode decision, and audit context.
- Create `backend/internal/handler/route_forwarder_test.go`: adapter, semantic-commit, and mode tests.
- Modify `backend/internal/handler/gateway_handler.go`: inject `RouteExecutor`; preserve access-policy resolution before execution.
- Modify `backend/internal/handler/gateway_handler_chat_completions.go`: OpenAI-compatible chat on non-native groups.
- Modify `backend/internal/handler/gateway_handler_responses.go`: OpenAI-compatible responses on non-native groups.
- Modify `backend/internal/handler/gemini_v1beta_handler.go`: Gemini native generate and stream-generate.
- Modify `backend/internal/handler/openai_gateway_handler.go`: native OpenAI/Grok Responses and Anthropic Messages dispatch.
- Modify `backend/internal/handler/openai_chat_completions.go`: native OpenAI/Grok Chat Completions.
- Create focused integration tests per handler rather than rewriting large existing test files.
- Modify `backend/internal/service/gateway_scheduling.go`: expose source-resolved, one-group selection for the adapter and remove V1 planner ownership from account scheduling after all text handlers pass.
- Modify `backend/internal/service/openai_gateway_scheduling.go`: expose equivalent one-group native OpenAI selection.
- Modify `backend/internal/service/route_executor.go`: add non-mutating `EnabledFor` and `Preview` used by off/shadow gates.
- Modify `backend/cmd/server/wire_gen.go`: inject the executor into both handler constructors.
- Do not integrate Count Tokens, Embeddings, Images, Videos, asynchronous task endpoints, or WebSocket in this plan.

### Task 1: Add a handler adapter and non-mutating mode gate

**Files:**
- Create: `backend/internal/handler/route_forwarder.go`
- Create: `backend/internal/handler/route_forwarder_test.go`
- Modify: `backend/internal/service/route_executor.go`
- Modify: `backend/internal/service/route_executor_types.go`
- Modify: `backend/internal/service/route_state_resilient.go`

- [ ] **Step 1: Write failing tests for off, shadow, enforce, and semantic commit**

```go
func TestRouteExecutionDecision(t *testing.T) {
	tests := []struct {
		mode string
		policyEnabled bool
		eligible bool
		want routeExecutionDecision
	}{
		{"off", true, true, routeUseLegacy},
		{"shadow", true, true, routeUseShadow},
		{"enforce", false, true, routeUseLegacy},
		{"enforce", true, false, routeUseLegacy},
		{"enforce", true, true, routeUseExecutor},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, decideRouteExecution(tt.mode, tt.policyEnabled, tt.eligible))
	}
}

func TestRouteCommitTrackerIgnoresSSECommentsButCommitsData(t *testing.T) {
	tracker := newRouteCommitTracker(0)
	tracker.ObserveWrite([]byte(": ping\n\n"))
	require.Equal(t, service.SemanticUncommitted, tracker.State())
	tracker.ObserveWrite([]byte("data: {\"type\":\"content_block_delta\"}\n\n"))
	require.Equal(t, service.SemanticCommitted, tracker.State())
}

func TestRouteCommitTrackerCoversWriteStringAndJSON(t *testing.T) {
	tracker := newRouteCommitTracker(0)
	tracker.ObserveWrite([]byte(`{"type":"message"}`))
	require.Equal(t, service.SemanticCommitted, tracker.State())
	requireClosed(t, tracker.Committed())
}

func TestRouteCallbackForwarderSelectsOnlyResolvedGroup(t *testing.T) {
	var gotGroup int64
	forwarder := &routeCallbackForwarder{selectFn: func(_ context.Context, route service.RouteResolvedCandidate, _ map[int64]struct{}) (*service.AccountSelectionResult, error) {
		gotGroup = route.EffectiveGroupID
		return &service.AccountSelectionResult{Account: &service.Account{ID: 7}}, nil
	}}
	_, err := forwarder.SelectAccount(context.Background(), service.RouteResolvedCandidate{EffectiveGroupID: 22}, nil)
	require.NoError(t, err)
	require.Equal(t, int64(22), gotGroup)
}
```

- [ ] **Step 2: Run adapter tests and verify the helper is undefined**

Run: `cd backend; go test ./internal/handler -run 'TestRouteExecutionDecision|TestRouteCommitTracker|TestRouteCallbackForwarder' -count=1`

Expected: FAIL with undefined adapter types.

- [ ] **Step 3: Add preview support without acquiring a lease or account**

```go
type RoutePreview struct {
	PolicyID int64
	CandidateIDs []int64
	EffectiveGroupIDs []int64
	SnapshotVersion int64
	StateMode RouteStateMode
}

func (e *RouteExecutor) EnabledFor(sourceGroupID int64) bool {
	snapshot, ok := e.snapshots.Current()
	if !ok { return false }
	policy, ok := snapshot.Policies[sourceGroupID]
	return ok && policy.Enabled
}

func (e *RouteExecutor) Preview(ctx context.Context, req RouteRequest) RoutePreview {
	snapshot, ok := e.snapshots.Current()
	if !ok { return RoutePreview{} }
	policy, candidates := resolveRouteCandidates(snapshot, req, nil)
	preview := RoutePreview{PolicyID: policy.ID, SnapshotVersion: snapshot.Version, StateMode: RouteStateDistributed}
	if strings.TrimSpace(req.SessionHash) != "" {
		sticky, stickyOK, mode, _ := e.state.GetSticky(ctx, req.SourceGroupID, req.SessionHash)
		if mode == RouteStateDegraded { preview.StateMode = RouteStateDegraded }
		if stickyOK && sticky.PolicyID == policy.ID && stickyMatchesResolvedCandidate(sticky, candidates) {
			candidates = moveCandidateFirst(candidates, sticky.CandidateID)
		}
	}
	for _, candidate := range candidates {
		if len(preview.CandidateIDs) >= policy.MaxRouteAttempts { break }
		keys := NewRouteHealthKeys(policy.ID, candidate, req.Endpoint, req.RequestedModel)
		settings := circuitSettings(policy)
		candidateState, candidateMode, _ := e.state.Inspect(ctx, keys.Candidate, settings)
		capabilityState, capabilityMode, _ := e.state.Inspect(ctx, keys.Capability, settings)
		if candidateMode == RouteStateDegraded || capabilityMode == RouteStateDegraded { preview.StateMode = RouteStateDegraded }
		if candidateState.Allowed && capabilityState.Allowed {
			preview.CandidateIDs = append(preview.CandidateIDs, candidate.ID)
			preview.EffectiveGroupIDs = append(preview.EffectiveGroupIDs, candidate.TargetGroupID)
		}
	}
	return preview
}
```

Use the executor-facing `RouteRuntimeState.Inspect(context.Context, string, RouteCircuitSettings)` method defined in the core plan; its second return value is the distributed/degraded mode. Redis inspection may read hashes but must not run `SET`, create a lease, change a circuit state, or record a failure. The local fallback follows the same non-mutating rule.

- [ ] **Step 4: Add the callback adapter and mode gate**

```go
type routeExecutionDecision uint8
const (
	routeUseLegacy routeExecutionDecision = iota
	routeUseShadow
	routeUseExecutor
)

func decideRouteExecution(mode string, policyEnabled, eligible bool) routeExecutionDecision {
	if !eligible || !policyEnabled || mode == "off" { return routeUseLegacy }
	if mode == "shadow" { return routeUseShadow }
	if mode == "enforce" { return routeUseExecutor }
	return routeUseLegacy
}

func routeV2TextEligible(c *gin.Context, _ string, imageIntent bool) bool {
	if c == nil || imageIntent || strings.EqualFold(c.GetHeader("Upgrade"), "websocket") { return false }
	path := c.FullPath()
	switch {
	case strings.Contains(path, "/embeddings"), strings.Contains(path, "/images"),
		strings.Contains(path, "/videos"), strings.Contains(path, "/count_tokens"),
		strings.Contains(path, "/responses/compact"):
		return false
	default:
		return true
	}
}

type routeCallbackForwarder struct {
	selectFn func(context.Context, service.RouteResolvedCandidate, map[int64]struct{}) (*service.AccountSelectionResult, error)
	acquireFn func(context.Context, *service.AccountSelectionResult) (func(), *service.RouteFailure)
	forwardFn func(context.Context, service.RouteAttempt) service.RouteAttemptOutcome
	tracker *routeCommitTracker
}

func (f *routeCallbackForwarder) SelectAccount(ctx context.Context, route service.RouteResolvedCandidate, excluded map[int64]struct{}) (*service.AccountSelectionResult, error) {
	return f.selectFn(ctx, route, excluded)
}
func (f *routeCallbackForwarder) AcquireAccount(ctx context.Context, selection *service.AccountSelectionResult) (func(), *service.RouteFailure) {
	return f.acquireFn(ctx, selection)
}
func (f *routeCallbackForwarder) Forward(ctx context.Context, attempt service.RouteAttempt) service.RouteAttemptOutcome {
	return f.forwardFn(ctx, attempt)
}
func (f *routeCallbackForwarder) SemanticCommitted() <-chan struct{} {
	if f.tracker == nil { return nil }
	return f.tracker.Committed()
}

type routeCommitTracker struct {
	committed atomic.Bool
	commitOnce sync.Once
	committedCh chan struct{}
	mu sync.Mutex
	pending []byte
}

func newRouteCommitTracker(_ int) *routeCommitTracker {
	return &routeCommitTracker{committedCh: make(chan struct{})}
}
func (t *routeCommitTracker) State() service.SemanticCommitState {
	if t.committed.Load() { return service.SemanticCommitted }
	return service.SemanticUncommitted
}
func (t *routeCommitTracker) Committed() <-chan struct{} { return t.committedCh }
func (t *routeCommitTracker) MarkCommitted() {
	t.commitOnce.Do(func() {
		t.committed.Store(true)
		close(t.committedCh)
	})
}
func (t *routeCommitTracker) BeginAttempt() {
	if t.committed.Load() { return }
	t.mu.Lock()
	t.pending = t.pending[:0]
	t.mu.Unlock()
}
func (t *routeCommitTracker) ObserveWrite(p []byte) {
	if t.committed.Load() || len(bytes.TrimSpace(p)) == 0 { return }
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending = append(t.pending, p...)
	trimmed := bytes.TrimSpace(t.pending)
	if len(trimmed) > 0 && trimmed[0] == '{' { t.MarkCommitted(); return }
	for {
		end := bytes.Index(t.pending, []byte("\n\n"))
		if end < 0 { return }
		event := bytes.TrimSpace(t.pending[:end])
		t.pending = append(t.pending[:0], t.pending[end+2:]...)
		if len(event) == 0 || bytes.HasPrefix(event, []byte(":")) { continue }
		for _, line := range bytes.Split(event, []byte("\n")) {
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
				t.MarkCommitted()
				return
			}
		}
	}
}

type routeTrackingWriter struct {
	gin.ResponseWriter
	tracker *routeCommitTracker
}
func (w *routeTrackingWriter) Write(p []byte) (int, error) {
	w.tracker.ObserveWrite(p)
	return w.ResponseWriter.Write(p)
}
func (w *routeTrackingWriter) WriteString(s string) (int, error) {
	w.tracker.ObserveWrite([]byte(s))
	return w.ResponseWriter.WriteString(s)
}
func trackRouteCommit(c *gin.Context) (*routeCommitTracker, func()) {
	tracker := newRouteCommitTracker(0)
	original := c.Writer
	c.Writer = &routeTrackingWriter{ResponseWriter: original, tracker: tracker}
	return tracker, func() { c.Writer = original }
}

func setRouteDebugHeaders(c *gin.Context, enabled bool, attempt service.RouteAttempt) {
	if !enabled { return }
	c.Header("X-Sub2API-Route-Role", string(attempt.Route.Role))
	c.Header("X-Sub2API-Route-Attempt", strconv.Itoa(attempt.RouteAttempt))
	c.Header("X-Sub2API-Account-Attempt", strconv.Itoa(attempt.AccountAttempt))
}
```

Create one tracker per executor call, assign it to the callback forwarder, wrap `c.Writer` with `routeTrackingWriter` for the duration of `RouteExecutor.Execute`, then restore the original writer. SSE comments, blank lines, and keepalive events remain uncommitted. A data event, `WriteString`, or JSON response body marks `SemanticCommitted`; adapters also explicitly call `MarkCommitted` for tool-call deltas and provider-native message events. The closed commit channel is what lets the core executor stop enforcing its pre-output timeout without canceling a long valid stream. This plan does not use the tracker for WebSocket.

A retryable attempt must return its classified error without rendering that upstream error to `c.Writer`; only the final `execution.Failure` is rendered. A synchronous 502/503/504 is marked `UpstreamNotAccepted` only when no task/response ID was accepted and the adapter has a complete upstream error response; ambiguous transport failures remain `UpstreamAcceptanceUnknown` and cannot cross routes. The tracker is a conservative backstop: if any JSON/error body was already written, the adapter reports committed/unsafe and the executor cannot append a fallback response. Add a handler test where a known-not-accepted primary 503 returns zero client bytes before fallback, plus regressions where unknown acceptance or an accidentally rendered primary error forbids the second route.

Every protocol callback invokes `setRouteDebugHeaders` immediately before its real upstream call. A later retry overwrites headers only while no semantic body has been sent. Tests prove the default emits none, the opt-in headers show only role/attempt counts, and group, candidate, and account IDs never appear in headers.

- [ ] **Step 5: Run adapter and preview tests**

Run: `cd backend; go test ./internal/service ./internal/handler -run 'TestRouteExecutionDecision|TestRouteCommitTracker|TestRouteCallbackForwarder|TestRouteExecutorPreview' -count=1`

Expected: PASS and no Redis keys are created by preview tests.

- [ ] **Step 6: Commit the adapter layer**

```bash
git add backend/internal/handler/route_forwarder.go backend/internal/handler/route_forwarder_test.go backend/internal/service/route_executor.go backend/internal/service/route_executor_types.go backend/internal/service/route_state_resilient.go
git commit -m "feat: add route executor handler adapter"
```

### Task 2: Expose one-candidate account scheduling

**Files:**
- Modify: `backend/internal/service/gateway_scheduling.go`
- Modify: `backend/internal/service/openai_gateway_scheduling.go`
- Create: `backend/internal/service/route_account_scheduler_test.go`

- [ ] **Step 1: Write failing tests that forbid nested route planning**

```go
func TestSelectAccountInResolvedGroupNeverInvokesV1Planner(t *testing.T) {
	groupID := int64(22)
	svc := newGatewayServiceForRouteSchedulerTest(t, groupID)
	svc.routeFailoverPlanner = &RouteFailoverPlanner{}
	selection, err := svc.SelectAccountInResolvedGroup(context.Background(), groupID, "session", "mapped-model", nil, "", 0)
	require.NoError(t, err)
	require.Equal(t, int64(22), selection.EffectiveGroupID)
}

func TestOpenAISelectAccountInResolvedGroupUsesEffectiveModel(t *testing.T) {
	groupID := int64(31)
	svc := newOpenAIGatewayServiceForRouteSchedulerTest(t, groupID, "gpt-mapped")
	selection, err := svc.SelectAccountInResolvedGroup(context.Background(), groupID, "session", "gpt-mapped", nil)
	require.NoError(t, err)
	require.Equal(t, "gpt-mapped", selection.EffectiveModel)
}
```

- [ ] **Step 2: Run scheduling tests and verify the methods are missing**

Run: `cd backend; go test ./internal/service -run 'TestSelectAccountInResolvedGroup|TestOpenAISelectAccountInResolvedGroup' -count=1`

Expected: FAIL with undefined methods.

- [ ] **Step 3: Add explicit single-group entry points**

```go
func (s *GatewayService) SelectAccountInResolvedGroup(ctx context.Context, groupID int64, sessionHash, effectiveModel string, excludedIDs map[int64]struct{}, metadataUserID string, sub2apiUserID int64) (*AccountSelectionResult, error) {
	selection, err := s.selectAccountWithLoadAwarenessInExactGroup(ctx, groupID, sessionHash, effectiveModel, excludedIDs, metadataUserID, sub2apiUserID)
	if err != nil { return nil, err }
	selection.EffectiveGroupID = groupID
	selection.EffectiveModel = effectiveModel
	return selection, nil
}

func (s *GatewayService) ResolveRouteSourceGroup(ctx context.Context, groupID int64) (*Group, int64, error) {
	group, resolvedID, err := s.checkClaudeCodeRestriction(ctx, &groupID)
	if err != nil { return nil, 0, err }
	if resolvedID == nil || *resolvedID <= 0 { return nil, 0, ErrGroupNotFound }
	return group, *resolvedID, nil
}

func (s *OpenAIGatewayService) SelectAccountInResolvedGroup(ctx context.Context, groupID int64, sessionHash, effectiveModel string, excludedIDs map[int64]struct{}) (*AccountSelectionResult, error) {
	selection, err := s.selectAccountWithLoadAwareness(ctx, &groupID, PlatformOpenAI, sessionHash, effectiveModel, excludedIDs, false, OpenAIEndpointCapabilityAny, false)
	if err != nil { return nil, err }
	selection.EffectiveGroupID = groupID
	selection.EffectiveModel = effectiveModel
	return selection, nil
}
```

Extract `selectAccountWithLoadAwarenessInExactGroup` from the body of the current in-group scheduler after its `checkClaudeCodeRestriction` block. It loads `groupID` directly with `resolveGroupByID`, rejects composite groups, attaches that exact group to context, and then runs the existing channel-pricing, platform, model, quota, affinity, exclusion, and load filters. It must never call `resolveGatewayGroup`, `checkClaudeCodeRestriction`, `routeFailoverPlanner`, or substitute another group ID. Keep the current legacy method unchanged until Task 6.

Add a native OpenAI/Grok exact-group variant that accepts `platform`, `requireCompact`, `requiredCapability`, and `useUpstreamTokenCost`, because those text routes must retain their existing filters. The executor never calls the public legacy `SelectAccountWithLoadAwareness` method. Extend `TestSelectAccountInResolvedGroupNeverInvokesV1Planner` with a group whose `FallbackGroupID` points elsewhere and assert the selected/effective group remains the exact ID passed by the executor.

- [ ] **Step 4: Run scheduling regression tests**

Run: `cd backend; go test ./internal/service -run 'TestSelectAccountInResolvedGroup|TestOpenAISelectAccountInResolvedGroup|TestGatewayService_SelectAccountWithLoadAwareness|TestOpenAISelectAccountWithLoadAwareness' -count=1`

Expected: PASS; legacy scheduling behavior remains unchanged.

- [ ] **Step 5: Commit the scheduler adapters**

```bash
git add backend/internal/service/gateway_scheduling.go backend/internal/service/openai_gateway_scheduling.go backend/internal/service/route_account_scheduler_test.go
git commit -m "feat: expose resolved-group account scheduling"
```

### Task 3: Integrate Anthropic Messages and non-native OpenAI-compatible text

**Files:**
- Modify: `backend/internal/handler/gateway_handler.go`
- Modify: `backend/internal/handler/gateway_handler_chat_completions.go`
- Modify: `backend/internal/handler/gateway_handler_responses.go`
- Create: `backend/internal/handler/gateway_route_executor_test.go`
- Modify: `backend/cmd/server/wire_gen.go`

- [ ] **Step 1: Write failing handler tests for immediate route switch and source identity**

```go
func TestGatewayMessagesRouteExecutorSwitchesProviderFailureToFallback(t *testing.T) {
	h, recorder := newGatewayRouteExecutorHandler(t, routeHandlerScenario{PrimaryStatus: 503, FallbackStatus: 200})
	router := newGatewayMessagesTestRouter(h, groupAPIKey(10))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, jsonRequest(http.MethodPost, "/v1/messages", `{"model":"claude-sonnet","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []int64{10, 20}, recorder.ForwardedGroupIDs())
	require.Equal(t, int64(10), recorder.BilledGroupID())
	require.Equal(t, int64(20), recorder.EffectiveGroupID())
}

func TestGatewayMessagesDoesNotSwitchAfterSemanticStreamOutput(t *testing.T) {
	h, recorder := newGatewayRouteExecutorHandler(t, routeHandlerScenario{PrimaryWritesSemanticEventThenFails: true})
	router := newGatewayMessagesTestRouter(h, groupAPIKey(10))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, jsonRequest(http.MethodPost, "/v1/messages", `{"model":"claude-sonnet","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	require.Equal(t, []int64{10}, recorder.ForwardedGroupIDs())
	require.NotContains(t, response.Body.String(), "fallback-token")
}
```

- [ ] **Step 2: Run the gateway integration tests and verify they fail on the legacy loop**

Run: `cd backend; go test ./internal/handler -run 'TestGatewayMessagesRouteExecutor' -count=1`

Expected: FAIL because `GatewayHandler` has no V2 executor path.

- [ ] **Step 3: Inject the executor and resolve access policy before health routing**

Add `routeExecutor *service.RouteExecutor` to `GatewayHandler` and its constructor. Before building `RouteRequest`, call `ResolveRouteSourceGroup` after Claude Code client context has been established. The resolved source group becomes `RouteRequest.SourceGroupID`; retain the original API key group in a separate audit field and as `usage_logs.group_id`. `FallbackGroupIDOnInvalidRequest` remains in its existing error path and is never passed as a health candidate.

```go
func (h *GatewayHandler) routeRequest(groupID int64, model, endpoint, sessionHash string, stream bool, requestID string) service.RouteRequest {
	return service.RouteRequest{SourceGroupID: groupID, RequestedModel: model, Endpoint: endpoint,
		SessionHash: sessionHash, Stream: stream, RequestID: requestID}
}
```

- [ ] **Step 4: Replace the Messages retry loop with one executor call**

```go
tracker, restoreWriter := trackRouteCommit(c)
defer restoreWriter()
forwarder := &routeCallbackForwarder{
	tracker: tracker,
	selectFn: func(ctx context.Context, route service.RouteResolvedCandidate, excluded map[int64]struct{}) (*service.AccountSelectionResult, error) {
		return h.gatewayService.SelectAccountInResolvedGroup(ctx, route.EffectiveGroupID, sessionKey, route.EffectiveModel, excluded, parsedReq.MetadataUserID, subject.UserID)
	},
	acquireFn: func(ctx context.Context, selection *service.AccountSelectionResult) (func(), *service.RouteFailure) {
		return acquireGatewayRouteAccount(ctx, c, h.concurrencyHelper, selection, reqStream, &streamStarted)
	},
	forwardFn: func(ctx context.Context, attempt service.RouteAttempt) service.RouteAttemptOutcome {
		tracker.BeginAttempt()
		setRouteDebugHeaders(c, h.cfg.Gateway.RouteFailoverV2.DebugHeaders, attempt)
		bodyForAttempt := h.gatewayService.ReplaceModelInBody(body, attempt.Route.EffectiveModel)
		before := c.Writer.Size()
		result, err := h.forwardMessagesAttempt(ctx, c, attempt.Account, bodyForAttempt, parsedReq)
		return classifyGatewayRouteOutcome(err, before, c.Writer.Size(), tracker.State(), result)
	},
}
execution := h.routeExecutor.Execute(c.Request.Context(), h.routeRequest(sourceGroupID, reqModel, EndpointAnthropicMessages, sessionKey, reqStream, requestIDFromContext(c)), forwarder)
if execution.Failure != nil { h.writeMessagesRouteFailure(c, execution.Failure, streamStarted); return }
result := execution.Value.(*service.ForwardResult)
```

The usage callback keeps `APIKey` unchanged, uses `attempt.Account`, and attaches `execution.Audit`. Do not call `NewFailoverState`, `HandleFailoverError`, `RecordRouteFailoverSuccess`, or `RecordRouteFailoverFailure` inside this execution branch.

- [ ] **Step 5: Integrate non-native Chat Completions and Responses with their native response writers**

In `gateway_handler_chat_completions.go`, build the same callback adapter but call the existing Gemini/Antigravity/Anthropic compatibility forward method selected by `attempt.Account.Platform`. In `gateway_handler_responses.go`, call the existing Responses compatibility forward method. Both adapters replace the model with `attempt.Route.EffectiveModel`, classify semantic output from writer deltas, and pass `execution.Audit` to usage.

```go
decision := decideRouteExecution(h.cfg.Gateway.RouteFailoverV2.Mode,
	h.routeExecutor.EnabledFor(sourceGroupID), routeV2TextEligible(c, reqModel, false))
if decision == routeUseShadow {
	preview := h.routeExecutor.Preview(c.Request.Context(), h.routeRequest(sourceGroupID, reqModel, inboundEndpoint, sessionHash, reqStream, requestIDFromContext(c)))
	defer logRouteShadowComparison(c, preview, selectedLegacyGroupID)
}
if decision != routeUseExecutor { h.runLegacyChatCompletions(c); return }
```

Extract the current loop into `runLegacyChatCompletions`/`runLegacyResponses` before adding the branch so `off`, disabled policies, and shadow execute exactly the prior implementation.

- [ ] **Step 6: Regenerate Wire and run gateway tests**

Run: `cd backend; go generate ./cmd/server`

Expected: `wire_gen.go` passes `*service.RouteExecutor` to `NewGatewayHandler`.

Run: `cd backend; go test ./internal/handler -run 'TestGateway(Messages|ChatCompletions|Responses)RouteExecutor|TestGatewayHandler' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit gateway text integration**

```bash
git add backend/internal/handler/gateway_handler.go backend/internal/handler/gateway_handler_chat_completions.go backend/internal/handler/gateway_handler_responses.go backend/internal/handler/gateway_route_executor_test.go backend/cmd/server/wire_gen.go
git commit -m "feat: route compatible text protocols through v2"
```

### Task 4: Integrate Gemini native generation

**Files:**
- Modify: `backend/internal/handler/gemini_v1beta_handler.go`
- Create: `backend/internal/handler/gemini_route_executor_test.go`

- [ ] **Step 1: Write failing Gemini failover and stream tests**

```go
func TestGeminiGenerateContentUsesFallbackCandidate(t *testing.T) {
	h, recorder := newGeminiRouteExecutorHandler(t, routeHandlerScenario{PrimaryStatus: 503, FallbackStatus: 200})
	router := newGeminiGenerateRouter(h, groupAPIKey(10))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, jsonRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []int64{10, 20}, recorder.ForwardedGroupIDs())
}

func TestGeminiStreamDoesNotConcatenateCandidates(t *testing.T) {
	h, recorder := newGeminiRouteExecutorHandler(t, routeHandlerScenario{PrimaryWritesSemanticEventThenFails: true})
	router := newGeminiGenerateRouter(h, groupAPIKey(10))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, jsonRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	require.Equal(t, []int64{10}, recorder.ForwardedGroupIDs())
	require.NotContains(t, response.Body.String(), "fallback-text")
}
```

- [ ] **Step 2: Run Gemini tests and verify the old failover loop is still used**

Run: `cd backend; go test ./internal/handler -run 'TestGeminiGenerateContentUsesFallbackCandidate|TestGeminiStreamDoesNotConcatenateCandidates' -count=1`

Expected: FAIL.

- [ ] **Step 3: Replace Gemini generation's loop with the adapter**

```go
tracker, restoreWriter := trackRouteCommit(c)
defer restoreWriter()
forwarder := &routeCallbackForwarder{
	tracker: tracker,
	selectFn: func(ctx context.Context, route service.RouteResolvedCandidate, excluded map[int64]struct{}) (*service.AccountSelectionResult, error) {
		return h.gatewayService.SelectAccountInResolvedGroup(ctx, route.EffectiveGroupID, sessionKey, route.EffectiveModel, excluded, "", 0)
	},
	acquireFn: func(ctx context.Context, selection *service.AccountSelectionResult) (func(), *service.RouteFailure) {
		return acquireGeminiRouteAccount(ctx, c, geminiConcurrency, selection, stream, &streamStarted)
	},
	forwardFn: func(ctx context.Context, attempt service.RouteAttempt) service.RouteAttemptOutcome {
		tracker.BeginAttempt()
		setRouteDebugHeaders(c, h.cfg.Gateway.RouteFailoverV2.DebugHeaders, attempt)
		before := c.Writer.Size()
		result, err := h.forwardGeminiAttempt(ctx, c, attempt.Account, requestBody, attempt.Route.EffectiveModel, action, stream)
		return classifyGeminiRouteOutcome(err, before, c.Writer.Size(), tracker.State(), result)
	},
}
execution := h.routeExecutor.Execute(c.Request.Context(), service.RouteRequest{
	SourceGroupID: sourceGroupID, RequestedModel: modelName, Endpoint: service.CompositeRouteEndpointGemini,
	SessionHash: sessionKey, Stream: stream, RequestID: requestIDFromContext(c),
}, forwarder)
```

Keep the action (`generateContent` or `streamGenerateContent`) fixed across candidates. Apply route model mapping before the existing account-level Gemini mapping and persist the entire mapping chain. Count Tokens remains on the legacy path.

- [ ] **Step 4: Run Gemini and cancellation regressions**

Run: `cd backend; go test ./internal/handler -run 'TestGemini(GenerateContentUsesFallbackCandidate|StreamDoesNotConcatenateCandidates)|Cancellation' -count=1`

Expected: PASS; cancellation produces no second candidate attempt.

- [ ] **Step 5: Commit Gemini integration**

```bash
git add backend/internal/handler/gemini_v1beta_handler.go backend/internal/handler/gemini_route_executor_test.go
git commit -m "feat: route gemini generation through v2"
```

### Task 5: Integrate native OpenAI and Grok text routes

**Files:**
- Modify: `backend/internal/handler/openai_gateway_handler.go`
- Modify: `backend/internal/handler/openai_chat_completions.go`
- Create: `backend/internal/handler/openai_route_executor_test.go`
- Modify: `backend/cmd/server/wire_gen.go`

- [ ] **Step 1: Write failing native route coverage tests**

```go
func TestNativeOpenAIResponsesUsesRouteExecutor(t *testing.T) {
	h, recorder := newOpenAIRouteExecutorHandler(t, service.PlatformOpenAI, routeHandlerScenario{PrimaryStatus: 503, FallbackStatus: 200})
	router := newOpenAIResponsesRouter(h, groupAPIKey(10))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, jsonRequest(http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.4","input":"hi"}`))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []int64{10, 20}, recorder.ForwardedGroupIDs())
}

func TestNativeGrokChatUsesRouteExecutor(t *testing.T) {
	h, recorder := newOpenAIRouteExecutorHandler(t, service.PlatformGrok, routeHandlerScenario{PrimaryStatus: 503, FallbackStatus: 200})
	router := newOpenAIChatRouter(h, groupAPIKey(10))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, jsonRequest(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4","messages":[{"role":"user","content":"hi"}]}`))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []int64{10, 20}, recorder.ForwardedGroupIDs())
}

func TestNativeOpenAIImageIntentStaysLegacy(t *testing.T) {
	h, recorder := newOpenAIRouteExecutorHandler(t, service.PlatformOpenAI, routeHandlerScenario{})
	router := newOpenAIResponsesRouter(h, groupAPIKey(10))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, jsonRequest(http.MethodPost, "/openai/v1/responses", `{"model":"gpt-5.4","input":"draw","tools":[{"type":"image_generation"}]}`))
	require.Zero(t, recorder.RouteExecutorCalls())
}
```

- [ ] **Step 2: Run native route tests and verify the executor is bypassed**

Run: `cd backend; go test ./internal/handler -run 'TestNative(OpenAI|Grok)' -count=1`

Expected: FAIL for the two text routing cases.

- [ ] **Step 3: Inject `RouteExecutor` into `OpenAIGatewayHandler`**

Add the field and constructor parameter, regenerate Wire, and gate execution with:

```go
eligible := routeV2TextEligible(c, reqModel, imageIntent)
decision := decideRouteExecution(h.cfg.Gateway.RouteFailoverV2.Mode,
	h.routeExecutor.EnabledFor(sourceGroupID), eligible)
```

The gate must return legacy for Responses image generation, Images, Videos, Embeddings, Count Tokens, `/responses/compact`, and all WebSocket turns.

- [ ] **Step 4: Replace native Responses, Chat, and Anthropic Messages dispatch loops**

```go
tracker, restoreWriter := trackRouteCommit(c)
defer restoreWriter()
forwarder := &routeCallbackForwarder{
	tracker: tracker,
	selectFn: func(ctx context.Context, route service.RouteResolvedCandidate, excluded map[int64]struct{}) (*service.AccountSelectionResult, error) {
		return h.gatewayService.SelectAccountInResolvedGroupForCapability(ctx,
			route.EffectiveGroupID, platform, sessionHash, route.EffectiveModel, excluded,
			requireCompact, requiredCapability, useUpstreamTokenCost)
	},
	acquireFn: func(ctx context.Context, selection *service.AccountSelectionResult) (func(), *service.RouteFailure) {
		return acquireOpenAIRouteAccount(ctx, c, h, selection, reqStream, &streamStarted, reqLog)
	},
	forwardFn: func(ctx context.Context, attempt service.RouteAttempt) service.RouteAttemptOutcome {
		tracker.BeginAttempt()
		setRouteDebugHeaders(c, h.cfg.Gateway.RouteFailoverV2.DebugHeaders, attempt)
		attemptBody := mappedBody(true, attempt.Route.EffectiveModel)
		before := service.OpenAICompactKeepaliveAdjustedWrittenSize(c)
		result, err := h.forwardOpenAITextAttempt(ctx, c, attempt.Account, attemptBody, parsedReq, endpoint)
		return classifyOpenAIRouteOutcome(c, err, before, tracker.State(), result)
	},
}
```

Use this adapter in `Responses`, `ChatCompletions`, and the OpenAI/Grok Anthropic Messages dispatch. Each function gets its own `forwardOpenAITextAttempt` wrapper around the current single-attempt code so it retains endpoint capability checks, silent-refusal handling, proxy marks, channel mapping, and native error rendering.

- [ ] **Step 5: Preserve primary/fallback platform and model invariants**

Before each forward, assert `attempt.Account.Platform` equals the source concrete platform (`openai` or `grok`). A mismatch returns a request/configuration failure, never a route-health failure. Apply administrator route mapping first, then existing channel/account mapping; pass requested and effective models to usage audit separately.

- [ ] **Step 6: Run native and existing scheduling tests**

Run: `cd backend; go test ./internal/handler ./internal/service -run 'TestNative(OpenAI|Grok)|TestOpenAI.*Failover|TestOpenAI.*Scheduler' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit native text integration**

```bash
git add backend/internal/handler/openai_gateway_handler.go backend/internal/handler/openai_chat_completions.go backend/internal/handler/openai_route_executor_test.go backend/cmd/server/wire_gen.go
git commit -m "feat: route native openai and grok text through v2"
```

### Task 6: Remove the V1 planner from account scheduling and prove compatibility

**Files:**
- Modify: `backend/internal/service/gateway_scheduling.go`
- Modify: `backend/internal/service/gateway_service.go`
- Modify: `backend/internal/handler/gateway_handler.go`
- Modify: `backend/cmd/server/wire_gen.go`
- Modify: `backend/internal/service/route_failover_gateway_test.go`

- [ ] **Step 1: Add an AST contract test forbidding nested route failover**

```go
func TestLegacyAccountSelectorDoesNotOwnRouteFailover(t *testing.T) {
	content, err := os.ReadFile("gateway_scheduling.go")
	require.NoError(t, err)
	source := string(content)
	start := strings.Index(source, "func (s *GatewayService) SelectAccountWithLoadAwareness")
	require.NotEqual(t, -1, start)
	end := strings.Index(source[start:], "func (s *GatewayService) selectAccountWithLoadAwarenessInGroup")
	require.NotEqual(t, -1, end)
	body := source[start : start+end]
	require.NotContains(t, body, "routeFailoverPlanner")
	require.NotContains(t, body, "RecordRouteFailover")
}
```

- [ ] **Step 2: Run the contract test and verify it fails against the MVP**

Run: `cd backend; go test ./internal/service -run TestLegacyAccountSelectorDoesNotOwnRouteFailover -count=1`

Expected: FAIL because V1 planning is currently inside `SelectAccountWithLoadAwareness`.

- [ ] **Step 3: Restore the legacy selector to account-only behavior**

```go
func (s *GatewayService) SelectAccountWithLoadAwareness(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, metadataUserID string, sub2apiUserID int64) (*AccountSelectionResult, error) {
	return s.selectAccountWithLoadAwarenessInGroup(ctx, groupID, sessionHash, requestedModel, excludedIDs, metadataUserID, sub2apiUserID)
}
```

Remove `routeFailoverPlanner` from `GatewayService` and `NewGatewayService`. Delete `recordRouteFailoverOutcome`, `RecordRouteFailoverSuccess`, and `RecordRouteFailoverFailure` call sites. Keep the V1 planner provider, repository, circuit, and `RouteFailoverManager` wired temporarily because the old admin API still calls `planner.Reload()` until the administration plan replaces it. The planner remains outside every gateway/account-selection hot path; remove its admin construction only when Task 3 of the administration plan passes.

- [ ] **Step 4: Verify off and disabled policies are byte-compatible**

```go
func TestRouteV2OffMatchesLegacyResponse(t *testing.T) {
	legacy := runGatewayFixture(t, routeFixtureConfig{Mode: "off"})
	disabled := runGatewayFixture(t, routeFixtureConfig{Mode: "enforce", PolicyEnabled: false})
	require.Equal(t, legacy.Status, disabled.Status)
	require.JSONEq(t, legacy.Body, disabled.Body)
	require.Equal(t, legacy.SelectedAccountIDs, disabled.SelectedAccountIDs)
}
```

- [ ] **Step 5: Regenerate Wire and run compatibility tests**

Run: `cd backend; go generate ./cmd/server`

Run: `cd backend; go test ./internal/handler ./internal/service ./cmd/server -run 'TestLegacyAccountSelector|TestRouteV2Off|TestGateway|TestOpenAI|TestGemini' -count=1`

Expected: PASS without a Wire dependency cycle.

- [ ] **Step 6: Commit removal of nested planning**

```bash
git add backend/internal/service/gateway_scheduling.go backend/internal/service/gateway_service.go backend/internal/handler/gateway_handler.go backend/cmd/server/wire_gen.go backend/internal/service/route_failover_gateway_test.go
git commit -m "refactor: make account scheduling route agnostic"
```

### Task 7: Run the protocol safety matrix

**Files:**
- Create: `backend/internal/handler/route_executor_protocol_matrix_test.go`

- [ ] **Step 1: Add a shared protocol matrix test**

```go
func TestRouteExecutorProtocolMatrix(t *testing.T) {
	protocols := []routeProtocolFixture{
		anthropicMessagesFixture(),
		anthropicCompatibleChatFixture(),
		geminiCompatibleChatFixture(),
		antigravityCompatibleChatFixture(),
		compatibleResponsesFixture(),
		geminiGenerateFixture(),
		geminiStreamFixture(),
		nativeOpenAIResponsesFixture(),
		nativeOpenAIChatFixture(),
		nativeOpenAIAnthropicMessagesFixture(),
		nativeGrokResponsesFixture(),
		nativeGrokChatFixture(),
		nativeGrokAnthropicMessagesFixture(),
	}
	for _, protocol := range protocols {
		t.Run(protocol.Name+" disabled compatibility", func(t *testing.T) {
			off := protocol.Run(t, routeHandlerScenario{Mode: "off"})
			disabled := protocol.Run(t, routeHandlerScenario{Mode: "enforce", PolicyEnabled: false})
			require.Equal(t, off.Status, disabled.Status)
			require.Equal(t, off.Body, disabled.Body)
			require.Equal(t, off.SelectedAccountIDs, disabled.SelectedAccountIDs)
		})
		t.Run(protocol.Name+" provider failure", func(t *testing.T) {
			result := protocol.Run(t, routeHandlerScenario{PrimaryStatus: 503, FallbackStatus: 200})
			require.Equal(t, []int64{10, 20}, result.ForwardedGroupIDs)
			require.Equal(t, int64(10), result.BilledGroupID)
		})
		t.Run(protocol.Name+" account failure", func(t *testing.T) {
			result := protocol.Run(t, routeHandlerScenario{PrimaryFirstAccountStatus: 401, PrimarySecondAccountStatus: 200})
			require.Equal(t, []int64{10, 10}, result.ForwardedGroupIDs)
			require.Zero(t, result.RouteCircuitFailures)
		})
		t.Run(protocol.Name+" committed stream", func(t *testing.T) {
			result := protocol.Run(t, routeHandlerScenario{PrimaryWritesSemanticEventThenFails: true})
			require.Equal(t, []int64{10}, result.ForwardedGroupIDs)
			require.False(t, result.StreamsConcatenated)
		})
		t.Run(protocol.Name+" malformed request", func(t *testing.T) {
			result := protocol.Run(t, routeHandlerScenario{MalformedRequest: true})
			require.Empty(t, result.ForwardedGroupIDs)
			require.Zero(t, result.RouteCircuitFailures)
		})
		t.Run(protocol.Name+" total budget", func(t *testing.T) {
			result := protocol.Run(t, routeHandlerScenario{AllProvidersFail: true, MaxTotalAttempts: 2})
			require.Len(t, result.ForwardedGroupIDs, 2)
			require.Equal(t, 2, result.Audit.AccountAttemptCount)
		})
		t.Run(protocol.Name+" client cancellation", func(t *testing.T) {
			result := protocol.Run(t, routeHandlerScenario{CancelPrimaryAttempt: true})
			require.LessOrEqual(t, len(result.ForwardedGroupIDs), 1)
			require.Zero(t, result.RouteCircuitFailures)
		})
	}
}
```

- [ ] **Step 2: Run the matrix with race detection**

Run: `cd backend; go test -race ./internal/handler -run TestRouteExecutorProtocolMatrix -count=1 -timeout=10m`

Expected: PASS with no races, stream concatenation, or cross-route replay after semantic output.

- [ ] **Step 3: Run all touched backend packages**

Run: `cd backend; go test ./internal/handler ./internal/service ./internal/server/routes ./cmd/server -count=1 -timeout=20m`

Expected: PASS on Go 1.26.5.

- [ ] **Step 4: Verify excluded protocols still contain no executor calls**

Run: `rg -n -g 'openai_embeddings.go' -g 'openai_images.go' -g 'grok_media.go' -g '*ws*.go' "routeExecutor\.Execute" backend/internal/handler`

Expected: no matches.

- [ ] **Step 5: Commit the protocol matrix**

```bash
git add backend/internal/handler/route_executor_protocol_matrix_test.go
git commit -m "test: cover route failover text protocols"
```
