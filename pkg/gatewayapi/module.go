package gatewayapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	// Host the adapter binds when neither config nor env names one
	DefaultHost = "127.0.0.1"
	// Port the adapter binds when neither config nor env names one
	DefaultPort = 8642
	// Bind retries while a restarting predecessor still holds the port
	bindAttempts = 5
	// Seconds between keepalive comments on a chat completions stream
	chatCompletionsSSEKeepaliveSeconds = 10.0
	// Seconds between runtime status heartbeats
	apiServerHeartbeatSeconds = 30.0
	// Latency samples kept for the daily p95
	apiServerLatencySampleLimit = 512
	// Messages a responses history keeps when auto truncation applies
	responsesAutoTruncationHistoryLimit = 100
	// Largest inlined media file in bytes
	mediaDataURLMaxBytes = 5 * 1024 * 1024
	// English failure label a gateway auth error sniffer keys on
	ProviderAuthFailedLabel = "Provider authentication failed"
	// English failure label a gateway rate limit sniffer keys on
	ProviderRateLimitedLabel = "Provider rate-limited"
)

// Logger of the API server platform module
func apiLog() *slog.Logger {
	return slog.Default().With("logger", "gateway.platforms.api_server")
}

// Static feature flags of the capabilities payload in wire order
var staticFeatureFlags = pyjson.ObjectOf(
	"run_status", true, "run_events_sse", true, "run_stop", true, "run_steer", true,
	"run_approval_response", true, "tool_progress_events", true, "approval_events", true,
	"session_resources", true, "model_options", true, "session_chat", true,
	"session_chat_streaming", true, "session_fork", true, "session_model_lock", true,
	"reasoning_streaming", true,
	"admin_config_rw", false, "jobs_admin", false, "memory_write_api", false,
	"skills_api", true, "audio_api", false, "realtime_voice", false,
	"session_continuity_header", "X-Hermes-Session-Id",
	"session_key_header", "X-Hermes-Session-Key")

// Endpoint advertised by the capabilities payload, gated on a feature flag when one is named
type capabilityEndpoint struct {
	name, method, path, feature string
}

var capabilityEndpoints = []capabilityEndpoint{
	{"health", "GET", "/health", ""}, {"health_detailed", "GET", "/health/detailed", ""},
	{"models", "GET", "/v1/models", ""}, {"model_options", "GET", "/api/model/options", ""},
	{"chat_completions", "POST", "/v1/chat/completions", ""},
	{"responses", "POST", "/v1/responses", ""}, {"runs", "POST", "/v1/runs", ""},
	{"run_status", "GET", "/v1/runs/{run_id}", ""},
	{"run_events", "GET", "/v1/runs/{run_id}/events", ""},
	{"run_approval", "POST", "/v1/runs/{run_id}/approval", ""},
	{"run_steer", "POST", "/v1/runs/{run_id}/steer", ""},
	{"run_unknown_resolution", "POST", "/v1/runs/{run_id}/resolve-unknown", "run_unknown_resolution"},
	{"run_stop", "POST", "/v1/runs/{run_id}/stop", ""}, {"skills", "GET", "/v1/skills", ""},
	{"toolsets", "GET", "/v1/toolsets", ""}, {"sessions", "GET", "/api/sessions", ""},
	{"session_create", "POST", "/api/sessions", ""},
	{"session", "GET", "/api/sessions/{session_id}", ""},
	{"session_update", "PATCH", "/api/sessions/{session_id}", ""},
	{"session_delete", "DELETE", "/api/sessions/{session_id}", ""},
	{"session_messages", "GET", "/api/sessions/{session_id}/messages", ""},
	{"session_fork", "POST", "/api/sessions/{session_id}/fork", ""},
	{"session_chat", "POST", "/api/sessions/{session_id}/chat", ""},
	{"session_chat_stream", "POST", "/api/sessions/{session_id}/chat/stream", ""},
	{"session_model_lock", "POST", "/api/sessions/{session_id}/model", ""},
}

// Choices an approval request offers, once and deny only for a smart denied or session less prompt
func approvalEventChoices(smartDenied, allowSession, allowPermanent bool) []any {
	if smartDenied || !allowSession {
		return []any{"once", "deny"}
	}
	if allowPermanent {
		return []any{"once", "session", "always", "deny"}
	}
	return []any{"once", "session", "deny"}
}

// Approval request payload with the command and description redacted and the run event envelope and choices
func approvalRequestEvent(ctx context.Context, runID string, approvalData *pyjson.Object, fields ...any) *pyjson.Object {
	event := pyjson.NewObject()
	if approvalData != nil {
		event = approvalData.Copy()
	}
	for _, key := range []string{"command", "description"} {
		if v, ok := event.Get(key); ok {
			event.Set(key, gateway.APIRedactApprovalCommand(ctx, v))
		}
	}
	smart, _ := event.Get("smart_denied")
	allowSession, _ := event.Get("allow_session")
	allowPermanent, _ := event.Get("allow_permanent")
	fields = append(fields, "choices", approvalEventChoices(pyrepr.Truthy(smart), allowSession != false, allowPermanent != false))
	event.Update(runEvent(runID, "approval.request", fields...))
	return event
}

// Canonical base version reported by protocol and compatibility payloads
func hermesVersion() string {
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		panic(err)
	}
	return info.BaseVersion
}

// Host and port the adapter binds, config winning over the env fallbacks
func ListenAddress(extra map[string]any) (any, int) {
	return gateway.APIServerListenAddress(extra)
}

// Runtime agent keywords for an explicit provider and model resolved from the authenticated catalog
func resolveRequestRuntimeAgentKwargs(ctx context.Context, providerName, targetModel string) (map[string]any, error) {
	rt, err := provider.ResolveRuntimeProvider(ctx, home.Dir(ctx), providerName, targetModel, "", "")
	if err != nil {
		return nil, errors.New(provider.FormatRuntimeProviderError(err))
	}
	var pool any
	if rt.CredentialPool != nil {
		pool = rt.CredentialPool
	}
	args := []any{}
	for _, a := range rt.Args {
		args = append(args, a)
	}
	return map[string]any{"api_key": rt.APIKey, "base_url": rt.BaseURL, "provider": rt.Provider,
		"api_mode": string(rt.APIMode), "command": rt.Command, "args": args, "credential_pool": pool}, nil
}

// Per request model, provider and options, a bare model honored only when allowed and never the virtual one
func requestAgentOverrides(body any, virtualModel string, allowBareModel bool) map[string]any {
	obj, ok := body.(*pyjson.Object)
	if !ok {
		return map[string]any{}
	}
	overrides := map[string]any{}
	providerName := gateway.APICleanRequestString(obj.GetOr("provider", nil))
	if providerName != nil {
		overrides["requested_provider"] = *providerName
	}
	model := gateway.APICleanRequestString(obj.GetOr("model", nil))
	if model != nil && *model != virtualModel && (providerName != nil || allowBareModel) {
		overrides["requested_model"] = *model
	}
	if options, ok := obj.GetOr("model_options", nil).(*pyjson.Object); ok {
		overrides["model_options"] = options.Copy()
	}
	return overrides
}

// Relay metadata object of an OpenAI request body, empty when absent or malformed
func requestRelayMetadata(body any) *pyjson.Object {
	obj, ok := body.(*pyjson.Object)
	if !ok {
		return pyjson.NewObject()
	}
	metadata, ok := obj.GetOr("metadata", nil).(*pyjson.Object)
	if !ok {
		return pyjson.NewObject()
	}
	return metadata.Copy()
}

// Whether a message is any compaction carrier shape the compressor classifies
func isCompressedSummaryMessage(message any) bool {
	if _, ok := message.(*pyjson.Object); !ok {
		if _, isMap := message.(map[string]any); !isMap {
			return false
		}
	}
	return state.IsCompactionSummaryMessage(pyjson.Plain(message))
}

// Client view of a stored message with compaction scaffolding hidden and diagnostic rows emptied
func projectClientMessage(message *pyjson.Object) *pyjson.Object {
	kind, _ := message.Get("display_kind")
	meta, _ := message.GetOr("display_metadata", nil).(*pyjson.Object)
	var category any
	if meta != nil {
		category, _ = meta.Get("notification_category")
	}
	if kind == "hidden" && category == "diagnostic" {
		kept := pyjson.NewObject()
		for _, k := range message.Keys {
			switch k {
			case "id", "session_id", "role", "timestamp", "display_kind", "platform_message_id":
				v, _ := message.Get(k)
				kept.Set(k, v)
			}
		}
		kept.Set("content", "")
		return kept
	}
	projected := state.ProjectCompactionMessageForDisplay(pyjson.Plain(message))
	if projected == nil {
		out := pyjson.NewObject()
		internal := map[string]bool{}
		for _, k := range state.CompactionInternalFields() {
			internal[k] = true
		}
		for _, k := range message.Keys {
			if !internal[k] {
				v, _ := message.Get(k)
				out.Set(k, v)
			}
		}
		out.Set("content", "")
		out.Set("display_kind", "hidden")
		return out
	}
	out, _ := pyjson.AsObject(projected)
	ordered := pyjson.NewObject()
	for _, k := range message.Keys {
		if v, ok := out.Get(k); ok {
			ordered.Set(k, v)
		}
	}
	keys := append([]string(nil), out.Keys...)
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := ordered.Get(k); !ok {
			v, _ := out.Get(k)
			ordered.Set(k, v)
		}
	}
	return ordered
}

// Most recent messages up to the limit, every compaction summary kept wherever it sits
func autoTruncateResponseHistory(history []any, limit int) []any {
	if limit <= 0 || len(history) <= limit {
		return history
	}
	var summaries []int
	for i, m := range history {
		if isCompressedSummaryMessage(m) {
			summaries = append(summaries, i)
		}
	}
	if len(summaries) == 0 {
		return history[len(history)-limit:]
	}
	kept := map[int]bool{}
	for _, i := range summaries[:min(limit, len(summaries))] {
		kept[i] = true
	}
	remaining := limit - len(kept)
	if remaining > 0 {
		summarySet := map[int]bool{}
		for _, i := range summaries {
			summarySet[i] = true
		}
		for index := len(history) - 1; index >= 0; index-- {
			if summarySet[index] {
				continue
			}
			kept[index] = true
			remaining--
			if remaining <= 0 {
				break
			}
		}
	}
	indices := make([]int, 0, len(kept))
	for i := range kept {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	out := make([]any, 0, len(indices))
	for _, i := range indices {
		out = append(out, history[i])
	}
	return out
}

// Bad request for a multimodal normalization error, its code before the first colon
func multimodalValidationError(err error, param string) *gateway.WebResponse {
	raw := err.Error()
	code, message, found := strings.Cut(raw, ":")
	if !found || message == "" {
		code, message = "invalid_content_part", raw
	}
	return errorResponse(message, http.StatusBadRequest, errOpts{Code: code, Param: param})
}

// Turn process ownership a gateway reaper reads, keyed by agent the way hermes sets agent attributes
type turnProcessOwnership struct {
	taskID   string
	baseline map[string]bool
	epoch    *int64
}

var (
	turnProcessEpochMu      sync.Mutex
	turnProcessEpochs       = map[string]int64{}
	turnProcessEpochCounter int64
	turnProcessOwners       = map[*agent.AIAgent]*turnProcessOwnership{}
)

// Ownership record of an agent, created empty on first read
func turnOwnershipOf(a *agent.AIAgent) *turnProcessOwnership {
	turnProcessEpochMu.Lock()
	defer turnProcessEpochMu.Unlock()
	own := turnProcessOwners[a]
	if own == nil {
		own = &turnProcessOwnership{}
		turnProcessOwners[a] = own
	}
	return own
}

// Reaps background processes an abandoned API turn created unless a newer run claimed its task
func reapDisconnectedAgentProcesses(ctx context.Context, a *agent.AIAgent, source string) {
	turnProcessEpochMu.Lock()
	own := turnProcessOwners[a]
	turnProcessEpochMu.Unlock()
	if own == nil || own.taskID == "" || own.baseline == nil {
		return
	}
	taskID, baseline := own.taskID, own.baseline
	var isStillCurrent func() bool
	if own.epoch != nil {
		epoch := *own.epoch
		isStillCurrent = func() bool {
			turnProcessEpochMu.Lock()
			current, ok := turnProcessEpochs[taskID]
			turnProcessEpochMu.Unlock()
			return !ok || current == epoch
		}
	}
	go gateway.APIAgentRuntime.ReapGatewayTurnProcesses(context.WithoutCancel(ctx), taskID, baseline, source, isStillCurrent)
}

// Snapshots the process baseline and claims the task epoch for an API agent turn
func publishTurnProcessOwnership(a *agent.AIAgent, taskID string) {
	turnProcessEpochMu.Lock()
	turnProcessEpochCounter++
	epoch := turnProcessEpochCounter
	turnProcessEpochs[taskID] = epoch
	turnProcessEpochMu.Unlock()
	baseline := tools.DefaultProcessRegistry().SnapshotRunningIDs(taskID)
	own := turnOwnershipOf(a)
	turnProcessEpochMu.Lock()
	own.taskID, own.baseline, own.epoch = taskID, baseline, &epoch
	turnProcessEpochMu.Unlock()
}

// Clears turn ownership at turn end so a later disconnect never reaps work the turn left running
func clearTurnProcessOwnership(a *agent.AIAgent) {
	turnProcessEpochMu.Lock()
	defer turnProcessEpochMu.Unlock()
	own := turnProcessOwners[a]
	if own == nil {
		return
	}
	if own.taskID != "" && own.epoch != nil {
		if turnProcessEpochs[own.taskID] == *own.epoch {
			delete(turnProcessEpochs, own.taskID)
		}
	}
	delete(turnProcessOwners, a)
}

// Normalized session chat message or input, a 400 when missing or malformed
func sessionChatUserMessage(body *pyjson.Object, param string) (any, *gateway.WebResponse) {
	userMessage := body.GetOr("message", nil)
	if !pyrepr.Truthy(userMessage) {
		userMessage = body.GetOr("input", nil)
	}
	if !gateway.APIContentHasVisiblePayload(userMessage) {
		return nil, errorResponse("Missing 'message' field", http.StatusBadRequest, errOpts{Code: "missing_message"})
	}
	normalized, err := gateway.APINormalizeMultimodalContent(userMessage)
	if err != nil {
		return nil, multimodalValidationError(err, param)
	}
	return normalized, nil
}

// Parsed body author, nil when absent or null, a ValueError when not an object
func requestTurnAuthor(body *pyjson.Object) (*pyjson.Object, error) {
	raw := body.GetOr("author", nil)
	if raw == nil {
		return nil, nil
	}
	if _, ok := raw.(*pyjson.Object); !ok {
		return nil, &gateway.PyError{Kind: "ValueError", Msg: "author must be an object"}
	}
	return provider.ParseTurnAuthor(raw), nil
}

var usageTokenKeys = []string{"input_tokens", "output_tokens", "total_tokens"}

// Chat completions usage block from the agent usage
func chatUsagePayload(usage map[string]any) *pyjson.Object {
	out := pyjson.NewObject()
	for i, name := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		out.Set(name, usageValue(usage, usageTokenKeys[i]))
	}
	return out
}

// Responses usage block from the agent usage
func responsesUsagePayload(usage map[string]any) *pyjson.Object {
	out := pyjson.NewObject()
	for _, key := range usageTokenKeys {
		out.Set(key, usageValue(usage, key))
	}
	return out
}

func usageValue(usage map[string]any, key string) any {
	if v, ok := usage[key]; ok {
		return v
	}
	return int64(0)
}

// Interrupts and reaps an abandoned agent run, then cancels its task and waits unless told not to
func abandonAgentTask(ctx context.Context, a *agent.AIAgent, task *agentTask, reason, reapSource string, awaitCancel bool) {
	if a != nil {
		func() {
			defer func() { _ = recover() }()
			agent.RequestHardInterrupt(a, reason, strings.ToLower(reason))
		}()
		reapDisconnectedAgentProcesses(ctx, a, reapSource)
	}
	if task != nil && !task.Done() {
		task.Cancel()
		if awaitCancel {
			task.Wait()
		}
	}
}

// Whether the API server can run, the HTTP server being built in
func CheckAPIServerRequirements() bool { return true }

var corsHeaders = [][2]string{
	{"Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS"},
	{"Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Hermes-Session-Id"},
}

var securityHeaders = [][2]string{
	{"Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'"},
	{"Permissions-Policy", "camera=(), microphone=(), geolocation=()"},
	{"Strict-Transport-Security", "max-age=31536000; includeSubDomains"},
	{"X-Content-Type-Options", "nosniff"},
	{"X-Frame-Options", "DENY"},
	{"X-XSS-Protection", "0"},
	{"Referrer-Policy", "no-referrer"},
}

// Middleware adding CORS headers for allowed origins and answering the preflight, reading the adapter the app holds
func corsMiddlewareFor(app *gateway.WebApplication) gateway.WebMiddleware {
	return func(ctx context.Context, request *gateway.WebRequest, handler gateway.WebHandler) (*gateway.WebResponse, error) {
		return corsMiddleware(ctx, app, request, handler)
	}
}

// CORS headers for allowed origins and the OPTIONS preflight, 403 for a disallowed browser origin
func corsMiddleware(ctx context.Context, app *gateway.WebApplication, request *gateway.WebRequest, handler gateway.WebHandler) (*gateway.WebResponse, error) {
	adapter, _ := app.State["api_server_adapter"].(*APIServerAdapter)
	origin := request.Header.Get("Origin")
	var headers [][2]string
	if adapter != nil {
		if !adapter.originAllowed(origin) {
			return gateway.NewWebResponse(http.StatusForbidden, nil), nil
		}
		headers = adapter.corsHeadersForOrigin(origin)
	}
	if request.Method == http.MethodOptions {
		if headers == nil {
			return gateway.NewWebResponse(http.StatusForbidden, nil), nil
		}
		resp := gateway.NewWebResponse(http.StatusOK, nil)
		for _, h := range headers {
			resp.Headers.Set(h[0], h[1])
		}
		return resp, nil
	}
	resp, err := handler(ctx, request)
	if err != nil {
		return nil, err
	}
	for _, h := range headers {
		resp.Headers.Set(h[0], h[1])
	}
	return resp, nil
}

// Early 413 for an oversized declared body and for a body that trips the read cap mid stream
func bodyLimitMiddleware(ctx context.Context, request *gateway.WebRequest, handler gateway.WebHandler) (*gateway.WebResponse, error) {
	switch request.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		if values, ok := request.Header["Content-Length"]; ok && len(values) > 0 {
			n, ok := pyint.Parse(values[0])
			if !ok {
				return errorResponse("Invalid Content-Length header.", http.StatusBadRequest, errOpts{Code: "invalid_content_length"}), nil
			}
			if n.Cmp(big.NewInt(gateway.APIMaxRequestBytes)) > 0 {
				return errorResponse("Request body too large.", http.StatusRequestEntityTooLarge, errOpts{Code: "body_too_large"}), nil
			}
		}
	}
	resp, err := handler(ctx, request)
	var httpErr *gateway.WebHTTPError
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusRequestEntityTooLarge {
		return errorResponse("Request body too large.", http.StatusRequestEntityTooLarge, errOpts{Code: "body_too_large"}), nil
	}
	return resp, err
}

// Security headers on every response including errors, never replacing one the handler set
func securityHeadersMiddleware(ctx context.Context, request *gateway.WebRequest, handler gateway.WebHandler) (*gateway.WebResponse, error) {
	resp, err := handler(ctx, request)
	if err != nil {
		var httpErr *gateway.WebHTTPError
		if !errors.As(err, &httpErr) {
			return nil, err
		}
		resp = httpErr.Response()
	}
	for _, h := range securityHeaders {
		if resp.Headers.Get(h[0]) == "" {
			resp.Headers.Set(h[0], h[1])
		}
	}
	return resp, nil
}

// Error text redacted before it crosses the HTTP boundary, cut to the limit when one is given
func redactAPIErrorText(value any, limit int) string {
	redacted := tools.RedactSensitiveTextWith(nil, pyrepr.Str(value), tools.RedactOptions{Force: true})
	if limit >= 0 {
		runes := []rune(redacted)
		if len(runes) > limit {
			return string(runes[:limit])
		}
	}
	return redacted
}

// OpenAI style error envelope with the message redacted
func openAIError(message, errType string, param, code any) *pyjson.Object {
	if errType == "" {
		errType = "invalid_request_error"
	}
	return pyjson.ObjectOf("error", pyjson.ObjectOf(
		"message", redactAPIErrorText(message, -1), "type", errType, "param", param, "code", code))
}

// Optional fields of an error response, nil param and code encoding as null
type errOpts struct {
	Type    string
	Param   any
	Code    any
	Headers map[string]string
}

// JSON error response with the OpenAI envelope
func errorResponse(message string, status int, o errOpts) *gateway.WebResponse {
	return jsonResponse(openAIError(message, o.Type, o.Param, o.Code), status, o.Headers)
}

// Bad request with the bare message and type envelope OpenAI validators use
func invalidRequest(message string) *gateway.WebResponse {
	return jsonResponse(pyjson.ObjectOf("error", pyjson.ObjectOf("message", message, "type", "invalid_request_error")), http.StatusBadRequest, nil)
}

// JSON response of a value the encoder always accepts
func jsonResponse(data any, status int, headers map[string]string) *gateway.WebResponse {
	resp, err := gateway.WebJSONResponse(data, status, headers)
	if err != nil {
		panic(err)
	}
	return resp
}

// Reservation of an admitted request not yet in agent bookkeeping, released once
type pendingReservation struct {
	active   bool
	detached bool
}

type reservationKey struct{}

// Reservation the admission wrapper bound to this request, nil outside one
func requestReservation(ctx context.Context) *pendingReservation {
	r, _ := ctx.Value(reservationKey{}).(*pendingReservation)
	return r
}

// Authenticated drain checked admission that reserves pending work for the handler's lifetime
func (a *APIServerAdapter) admitAPIAgentRequest(handler gateway.WebHandler) gateway.WebHandler {
	return func(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
		var authErr *gateway.WebResponse
		if a.usesRoomRunAuth(request) {
			authErr = a.checkRunAuth(ctx, request, "dispatch")
		} else {
			authErr = a.checkAuth(ctx, request)
		}
		if authErr != nil {
			return authErr, nil
		}
		if draining := a.drainingResponse(); draining != nil {
			return draining, nil
		}
		reservation := &pendingReservation{active: true}
		a.mu.Lock()
		a.pendingAgentRequests++
		a.mu.Unlock()
		defer a.releasePendingAPIWork(reservation)
		return handler(context.WithValue(ctx, reservationKey{}, reservation), request)
	}
}

// Releases a pending work reservation exactly once
func (a *APIServerAdapter) releasePendingAPIWork(reservation *pendingReservation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if reservation.active {
		reservation.active = false
		a.pendingAgentRequests = max(0, a.pendingAgentRequests-1)
	}
}

// Bearer token check ahead of the handler, its 401 returned instead
func (a *APIServerAdapter) requireAuth(handler gateway.WebHandler) gateway.WebHandler {
	return func(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
		if authErr := a.checkAuth(ctx, request); authErr != nil {
			return authErr, nil
		}
		return handler(ctx, request)
	}
}

// Pending work reservation for background work started outside an admitted request, released by done unless detached
func (a *APIServerAdapter) reservePendingAPIWork() (*pendingReservation, func()) {
	reservation := &pendingReservation{active: true}
	a.mu.Lock()
	a.pendingAgentRequests++
	a.mu.Unlock()
	return reservation, func() {
		if !reservation.detached {
			a.releasePendingAPIWork(reservation)
		}
	}
}

// Idempotency cache entry with its body fingerprint and store time
type idemEntry struct {
	resp any
	fp   string
	ts   float64
}

// In memory idempotency cache with a TTL and least recently stored eviction
type idempotencyCache struct {
	mu       sync.Mutex
	order    []string
	store    map[string]idemEntry
	inflight map[[2]string]*idemCall
	ttl      float64
	max      int
}

type idemCall struct {
	done chan struct{}
	resp any
	err  error
}

func newIdempotencyCache(maxItems int, ttlSeconds float64) *idempotencyCache {
	return &idempotencyCache{store: map[string]idemEntry{}, inflight: map[[2]string]*idemCall{}, ttl: ttlSeconds, max: maxItems}
}

// Drops expired entries then the oldest past the size cap, caller holds the lock
func (c *idempotencyCache) purgeLocked() {
	now := apiNow()
	kept := c.order[:0]
	for _, k := range c.order {
		if now-c.store[k].ts > c.ttl {
			delete(c.store, k)
			continue
		}
		kept = append(kept, k)
	}
	c.order = kept
	for len(c.order) > c.max {
		delete(c.store, c.order[0])
		c.order = c.order[1:]
	}
}

// Cached response for a key and fingerprint, computing it once while concurrent callers share the flight
func (c *idempotencyCache) getOrSet(key, fingerprint string, compute func() (any, error)) (any, error) {
	c.mu.Lock()
	c.purgeLocked()
	if item, ok := c.store[key]; ok && item.fp == fingerprint {
		c.mu.Unlock()
		return item.resp, nil
	}
	flightKey := [2]string{key, fingerprint}
	call := c.inflight[flightKey]
	if call == nil {
		call = &idemCall{done: make(chan struct{})}
		c.inflight[flightKey] = call
		go func() {
			defer close(call.done)
			defer func() {
				c.mu.Lock()
				if c.inflight[flightKey] == call {
					delete(c.inflight, flightKey)
				}
				c.mu.Unlock()
			}()
			call.resp, call.err = compute()
			if call.err != nil {
				return
			}
			c.mu.Lock()
			if _, exists := c.store[key]; exists {
				for i, k := range c.order {
					if k == key {
						c.order = append(c.order[:i], c.order[i+1:]...)
						break
					}
				}
			}
			c.store[key] = idemEntry{resp: call.resp, fp: fingerprint, ts: apiNow()}
			c.order = append(c.order, key)
			c.purgeLocked()
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	<-call.done
	return call.resp, call.err
}

var idemCache = newIdempotencyCache(1000, 300)

// SHA256 of the repr of the body subset under the given keys
func makeRequestFingerprint(body *pyjson.Object, keys []string) string {
	subset := pyjson.NewObject()
	for _, k := range keys {
		subset.Set(k, body.GetOr(k, nil))
	}
	sum := sha256.Sum256([]byte(pyrepr.Repr(subset)))
	return hex.EncodeToString(sum[:])
}

// Whether a prefix names the profile this process was launched as
func namesLaunchProfile(ctx context.Context, profile string) (matches bool) {
	defer func() {
		if recover() != nil {
			matches = false
		}
	}()
	return profiles.ProfileMatchesHome(ctx, profile, home.RoutingProcess())
}

// Stable session id from the system prompt and first user message, namespaced by a routed profile
func deriveChatSessionID(ctx context.Context, systemPrompt, firstUserMessage, profile string) string {
	seed := systemPrompt + "\n" + firstUserMessage
	if profile != "" && profile != "default" && !namesLaunchProfile(ctx, profile) {
		seed = profile + "\x00" + seed
	}
	sum := sha256.Sum256([]byte(seed))
	return "api-" + hex.EncodeToString(sum[:])[:16]
}

// Provider credential resolution failure, typed so other run errors are never labelled auth failures
type providerAuthResolutionError struct {
	msg   string
	cause error
}

func (e *providerAuthResolutionError) Error() string { return e.msg }
func (e *providerAuthResolutionError) Unwrap() error { return e.cause }

// Whether the cause is a quota cap with valid credentials rather than an auth failure
func (e *providerAuthResolutionError) isRateLimited() bool {
	cause := e.cause
	var runtimeErr interface{ Unwrap() error }
	if errors.As(cause, &runtimeErr) && isRuntimeError(cause) {
		cause = runtimeErr.Unwrap()
	}
	return cause != nil && provider.IsRateLimitedAuthError(cause)
}

// Whether an error is a plain runtime error wrapping its cause
func isRuntimeError(err error) bool {
	var wrapped *runtimeCauseError
	return errors.As(err, &wrapped)
}

// Runtime error raised from a cause, the RuntimeError from exc chain
type runtimeCauseError struct {
	msg   string
	cause error
}

func (e *runtimeCauseError) Error() string { return e.msg }
func (e *runtimeCauseError) Unwrap() error { return e.cause }

// Failure line shown as the assistant reply in API backed chat UIs
func (e *providerAuthResolutionError) userText(ctx context.Context) string {
	key := "platform.api_server.provider_auth_failed"
	if e.isRateLimited() {
		key = "platform.api_server.provider_rate_limited"
	}
	label := i18n.T(ctx, key, "", nil)
	return i18n.T(ctx, "platform.api_server.provider_error_line", "", map[string]any{"label": label, "error": e.Error()})
}

// Ordered unbounded SSE event queue of one session chat stream run, safe to enqueue from any goroutine
type sessionEventQueue struct {
	mu        sync.Mutex
	pending   []sessionEvent
	ready     chan struct{}
	sessionID string
	runID     string
	seq       int64
}

type sessionEvent struct {
	name    string
	payload *pyjson.Object
	end     bool
}

func newSessionEventQueue(sessionID, runID string) *sessionEventQueue {
	return &sessionEventQueue{ready: make(chan struct{}, 1), sessionID: sessionID, runID: runID}
}

// Stamps session, run, sequence and time onto a payload
func (q *sessionEventQueue) payload(name string, payload *pyjson.Object) sessionEvent {
	q.mu.Lock()
	q.seq++
	seq := q.seq
	q.mu.Unlock()
	setDefault(payload, "session_id", q.sessionID)
	setDefault(payload, "run_id", q.runID)
	setDefault(payload, "seq", seq)
	setDefault(payload, "ts", apiNow())
	return sessionEvent{name: name, payload: payload}
}

// Appends an event and wakes the reader
func (q *sessionEventQueue) put(ev sessionEvent) {
	q.mu.Lock()
	q.pending = append(q.pending, ev)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// Queues a named event in order
func (q *sessionEventQueue) enqueue(name string, payload *pyjson.Object) {
	q.put(q.payload(name, payload))
}

// Queues the end of stream marker
func (q *sessionEventQueue) close() { q.put(sessionEvent{end: true}) }

// Returns once every event handed over before the call is queued, enqueue appending synchronously
func (q *sessionEventQueue) flush() {}

// Next queued event, ok false when the timeout or the context ends first
func (q *sessionEventQueue) next(ctx context.Context, timeout time.Duration) (sessionEvent, bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			ev := q.pending[0]
			q.pending = q.pending[1:]
			q.mu.Unlock()
			return ev, true, nil
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return sessionEvent{}, false, ctx.Err()
		case <-deadline.C:
			return sessionEvent{}, false, nil
		case <-q.ready:
		}
	}
}

// Sets a key only when absent, the dict setdefault
func setDefault(o *pyjson.Object, key string, value any) any {
	if v, ok := o.Get(key); ok {
		return v
	}
	o.Set(key, value)
	return value
}

// Wall clock seconds, swappable through the shared API clock
func apiNow() float64 { return gateway.APINow() }

// Agent task of a handler, done once its goroutine returns and cancelled through its context
type agentTask struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// Starts fn on its own goroutine under a cancellable context
func startAgentTask(ctx context.Context, fn func(ctx context.Context)) *agentTask {
	taskCtx, cancel := context.WithCancel(ctx)
	t := &agentTask{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		defer cancel()
		fn(taskCtx)
	}()
	return t
}

// Whether the task finished
func (t *agentTask) Done() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Requests cancellation of the task
func (t *agentTask) Cancel() { t.once.Do(t.cancel) }

// Blocks until the task finishes
func (t *agentTask) Wait() { <-t.done }

// Wait channel closed when the task finishes
func (t *agentTask) DoneCh() <-chan struct{} { return t.done }

// Elapsed wall seconds since a start time
func since(start time.Time) float64 { return time.Since(start).Seconds() }

// Outcome of a counted worker, a recovered panic carried for the caller to raise again
type workerResult[T any] struct {
	Value T
	Err   error
	Panic any
}

// Runs fn on a goroutine with the API worker lifetime count held from submission until fn returns
func submitAPIWorker[T any](fn func() (T, error)) <-chan workerResult[T] {
	out := make(chan workerResult[T], 1)
	err := gateway.SubmitAPIWorker(func(counted func() (any, error)) error {
		go func() {
			var res workerResult[T]
			defer func() {
				if rec := recover(); rec != nil {
					res.Panic = rec
				}
				out <- res
			}()
			v, err := counted()
			res.Err = err
			if typed, ok := v.(T); ok {
				res.Value = typed
			}
		}()
		return nil
	}, func() (any, error) { return fn() })
	if err != nil {
		out <- workerResult[T]{Err: err}
	}
	return out
}

// First truthy value, else the last one, the a or b or c chain
func firstTruthy(values ...any) any {
	for _, v := range values {
		if pyrepr.Truthy(v) {
			return v
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values[len(values)-1]
}

// String value or empty for nil and non strings
func textOf(v any) string {
	s, _ := v.(string)
	return s
}
