package gatewayapi

import (
	"context"
	"crypto/subtle"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// APIServerAdapter serves OpenAI format requests through real agents on an HTTP listener
type APIServerAdapter struct {
	*gateway.BasePlatformAdapter
	// Launch scope the shutdown hooks write run state under, the gateway's own profile home
	lifecycleCtx        context.Context
	host                any
	port                int
	apiKey              string
	corsOrigins         []string
	modelName           string
	modelRoutes         *modelRouteTable
	directModelRequests bool
	toolProgressEvents  bool
	app                 *gateway.WebApplication
	runner              *gateway.WebAppRunner

	responseStore     *gateway.ResponseStore
	responseStoreHome string
	responseStores    map[string]*gateway.ResponseStore
	responseStoreMu   sync.Mutex

	runState

	sessionDB            *state.SessionDB
	sessionDBs           map[string]*state.SessionDB
	sessionDBCacheMu     sync.Mutex
	sessionDBCacheClosed bool
	sessionDBOpenMu      sync.Mutex

	lastResolvedModel           map[string]string
	maxConcurrentRuns           int
	historyToolOutputMaxChars   int
	inflightAgentRuns           int
	shutdownInterruptibleAgents map[*agent.AIAgent]struct{}
	memorySessions              *APIServerMemorySessions
	pendingAgentRequests        int

	metricsDay             string
	metricsRequestsToday   int
	metricsMessagesToday   int
	metricsTokensToday     int
	metricsLatencyMs       []float64
	metricsLastRequestAt   *float64
	metricsLastHeartbeatAt *float64

	backgroundTasks map[*agentTask]struct{}
	heartbeatStop   chan struct{}
	mu              sync.Mutex
}

// Validated model routes in configuration order
type modelRouteTable struct {
	keys   []string
	routes map[string]map[string]string
}

// Route of an alias, nil when unknown
func (t *modelRouteTable) get(alias string) map[string]string {
	if t == nil {
		return nil
	}
	return t.routes[alias]
}

// Class attributes of the API server adapter, stateless delivery with the profile prefix served
func apiServerAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.SupportsAsyncDelivery = false
	traits.ServesProfilePrefix = true
	traits.InteractiveResume = false
	return traits
}

// Adapter for an API server platform config
func NewAPIServerAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (*APIServerAdapter, error) {
	a := &APIServerAdapter{responseStores: map[string]*gateway.ResponseStore{}, sessionDBs: map[string]*state.SessionDB{},
		lastResolvedModel: map[string]string{}, shutdownInterruptibleAgents: map[*agent.AIAgent]struct{}{},
		backgroundTasks: map[*agentTask]struct{}{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformAPIServer, a)
	a.Traits = apiServerAdapterTraits()
	a.lifecycleCtx = context.WithoutCancel(ctx)
	extra := cfg.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	a.host, a.port = ListenAddress(extra)
	if v, ok := extra["key"]; ok {
		a.apiKey = pyrepr.Str(v)
	} else {
		key, err := gateway.GetScopedSecret(ctx, "API_SERVER_KEY", "", true)
		if err != nil {
			return nil, err
		}
		a.apiKey = key
	}
	var cors any = gateway.PlatformGateEnv(ctx, "API_SERVER_CORS_ORIGINS", "")
	if v, ok := extra["cors_origins"]; ok {
		cors = v
	}
	a.corsOrigins = parseCORSOrigins(cors)
	var explicitModel string
	if v, ok := extra["model_name"]; ok {
		explicitModel = pyrepr.Str(v)
	} else {
		name, err := gateway.GetScopedSecret(ctx, "API_SERVER_MODEL_NAME", "", true)
		if err != nil {
			return nil, err
		}
		explicitModel = name
	}
	a.modelName = resolveModelName(ctx, explicitModel)
	a.modelRoutes = parseModelRoutes(extra["model_routes"])
	a.directModelRequests = gateway.APICoerceRequestBool(extra["direct_model_requests"], false)
	a.toolProgressEvents = gateway.APICoerceRequestBool(extra["tool_progress_events"], true)
	store, err := gateway.NewResponseStore(ctx, gateway.APIMaxStoredResponses, "", gateway.APIMaxStoredResponses*gateway.APIIdentityRetentionFactor)
	if err != nil {
		return nil, err
	}
	a.responseStore = store
	a.responseStoreHome = home.Dir(ctx)
	if err := a.initializeRunState(ctx); err != nil {
		return nil, err
	}
	a.maxConcurrentRuns = resolveMaxConcurrentRuns(ctx)
	a.historyToolOutputMaxChars = resolveAPIServerInt(ctx, "history_tool_output_max_chars", 0)
	a.memorySessions = NewAPIServerMemorySessions(nil, nil)
	a.metricsDay = metricsDayKey(nil)
	return a, nil
}

// Pending admissions plus in flight turns plus live structured run tasks
func (a *APIServerAdapter) ActiveAgentWorkCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	live := 0
	for _, task := range a.activeRunTasks {
		if !task.Done() {
			live++
		}
	}
	return a.pendingAgentRequests + a.inflightAgentRuns + live
}

// Interrupts every adapter owned agent during shutdown and returns how many were interrupted
func (a *APIServerAdapter) InterruptActiveRuns(reason string) int {
	ctx := a.lifecycleCtx
	a.mu.Lock()
	runIDs := map[string]bool{}
	for runID, task := range a.activeRunTasks {
		if !task.Done() {
			runIDs[runID] = true
		}
	}
	for runID := range a.activeRunAgents {
		runIDs[runID] = true
	}
	agents := []*agent.AIAgent{}
	seen := map[*agent.AIAgent]bool{}
	for _, runID := range a.activeRunAgentOrder {
		if ag := a.activeRunAgents[runID]; ag != nil && !seen[ag] {
			seen[ag] = true
			agents = append(agents, ag)
		}
	}
	for ag := range a.shutdownInterruptibleAgents {
		if ag != nil && !seen[ag] {
			seen[ag] = true
			agents = append(agents, ag)
		}
	}
	a.mu.Unlock()
	ids := make([]string, 0, len(runIDs))
	for id := range runIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	a.markShutdownInterruptedRuns(ctx, ids)
	interrupted := 0
	for _, ag := range agents {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					apiLog().Debug(fmt.Sprintf("[api_server] failed interrupting active agent: %v", rec))
				}
			}()
			if agent.RequestHardInterrupt(ag, reason, "gateway shutdown") {
				interrupted++
			}
		}()
	}
	return interrupted
}

// Persists the gateway drain start on every nonterminal API run
func (a *APIServerAdapter) MarkShutdownRequested() int {
	return a.markShutdownRequested(a.lifecycleCtx)
}

// Whether the owning gateway refuses new agent turns
func gatewayIsDraining() (draining bool) {
	defer func() {
		if recover() != nil {
			draining = false
		}
	}()
	runner := gateway.CurrentGatewayRunner()
	return runner != nil && gateway.APIRunnerIsDraining(runner)
}

// Retryable 503 while the gateway drains existing work, nil otherwise
func (a *APIServerAdapter) drainingResponse() *gateway.WebResponse {
	if !gatewayIsDraining() {
		return nil
	}
	return errorResponse("Gateway is draining existing work; retry shortly.", http.StatusServiceUnavailable,
		errOpts{Code: "gateway_draining", Headers: map[string]string{"Retry-After": "1"}})
}

// Transfers this request's drain reservation to agent bookkeeping
func (a *APIServerAdapter) activateAdmittedRequest(ctx context.Context) {
	if reservation := requestReservation(ctx); reservation != nil {
		a.releasePendingAPIWork(reservation)
	}
}

// Structured runs, queued process completions and active async delegations
func (a *APIServerAdapter) readinessWorkCounts() (int, int, int) {
	activeAPIRuns := a.activeStructuredRunCount()
	processDepth, activeDelegations := 0, 0
	func() {
		defer func() { _ = recover() }()
		processDepth = tools.DefaultProcessRegistry().CompletionQueue.Len()
	}()
	func() {
		defer func() { _ = recover() }()
		activeDelegations = tools.AsyncDelegationActiveCount()
	}()
	return activeAPIRuns, processDepth, activeDelegations
}

// Structured runs that still have executable work
func (a *APIServerAdapter) activeStructuredRunCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, status := range a.runStatuses {
		switch s, _ := status.Get("status"); s {
		case "queued", "running", "waiting_for_approval", "stopping":
			n++
		}
	}
	return n
}

// Configured CORS origins as a stable list
func parseCORSOrigins(value any) []string {
	if !pyrepr.Truthy(value) {
		return []string{}
	}
	var items []any
	switch v := value.(type) {
	case string:
		for _, part := range strings.Split(v, ",") {
			items = append(items, part)
		}
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	default:
		items = []any{pyrepr.Str(v)}
	}
	out := []string{}
	for _, item := range items {
		if s := strings.TrimSpace(pyrepr.Str(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Concurrent agent run cap, zero disabling it
func resolveMaxConcurrentRuns(ctx context.Context) int {
	return resolveAPIServerInt(ctx, "max_concurrent_runs", 10)
}

// Integer setting under gateway.api_server, the default when unreadable and negatives clamped to zero
func resolveAPIServerInt(ctx context.Context, key string, def int) (value int) {
	defer func() {
		if recover() != nil {
			value = def
		}
	}()
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		return def
	}
	raw, ok := config.CfgGet(tree, "gateway", "api_server", key)
	if !ok || raw == nil {
		raw = int64(def)
	}
	n, err := pyint.Of(raw)
	if err != nil || !n.IsInt64() {
		return def
	}
	return max(0, int(n.Int64()))
}

// UTC day bucket of the daily metrics
func metricsDayKey(timestamp *float64) string {
	t := apiNow()
	if timestamp != nil && *timestamp != 0 {
		t = *timestamp
	}
	sec, frac := math.Modf(t)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC().Format("2006-01-02")
}

// ISO timestamp in UTC, nil for nil
func isoTimestamp(timestamp *float64) any {
	if timestamp == nil {
		return nil
	}
	sec, frac := math.Modf(*timestamp)
	t := time.Unix(int64(sec), int64(math.Round(frac*1e6))*1000).UTC()
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05+00:00")
	}
	return t.Format("2006-01-02T15:04:05.000000+00:00")
}

// Resets the daily counters when the UTC day rolled over, caller holds the lock
func (a *APIServerAdapter) resetMetricsIfNeededLocked() {
	current := metricsDayKey(nil)
	if a.metricsDay == current {
		return
	}
	a.metricsDay = current
	a.metricsRequestsToday, a.metricsMessagesToday, a.metricsTokensToday = 0, 0, 0
	a.metricsLatencyMs = a.metricsLatencyMs[:0]
}

// Non negative total tokens of a usage mapping
func totalTokensFromUsage(usage map[string]any) int {
	if usage == nil {
		return 0
	}
	raw := usage["total_tokens"]
	if !pyrepr.Truthy(raw) {
		return 0
	}
	n, err := pyint.Of(raw)
	if err != nil || !n.IsInt64() {
		return 0
	}
	return max(0, int(n.Int64()))
}

// Nearest rank p95 of the latency samples rounded to two places, nil without samples
func latencyP95Ms(samples []float64) any {
	if len(samples) == 0 {
		return nil
	}
	ordered := append([]float64(nil), samples...)
	sort.Float64s(ordered)
	index := max(0, min(len(ordered)-1, int(float64(len(ordered))*0.95+0.999999)-1))
	return pyint.Round(ordered[index], 2)
}

// Runtime status metrics payload of this listener
func (a *APIServerAdapter) apiServerStatusPayload(heartbeatAt *float64) *pyjson.Object {
	structured := a.activeStructuredRunCount()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resetMetricsIfNeededLocked()
	heartbeat := a.metricsLastHeartbeatAt
	if heartbeatAt != nil {
		heartbeat = heartbeatAt
	}
	return pyjson.ObjectOf(
		"host", a.host, "port", int64(a.port),
		"active_runs", int64(structured+a.inflightAgentRuns),
		"stored_runs", int64(len(a.runStatuses)),
		"last_request_at", isoTimestamp(a.metricsLastRequestAt),
		"last_heartbeat", isoTimestamp(heartbeat),
		"metrics_today", pyjson.ObjectOf(
			"day", a.metricsDay, "requests", int64(a.metricsRequestsToday),
			"messages", int64(a.metricsMessagesToday), "tokens", int64(a.metricsTokensToday),
			"latency_p95_ms", latencyP95Ms(a.metricsLatencyMs)))
}

// Publishes a metrics bearing runtime status snapshot stamped with this heartbeat
func (a *APIServerAdapter) publishRuntimeStatus(ctx context.Context) {
	now := apiNow()
	a.mu.Lock()
	a.metricsLastHeartbeatAt = &now
	a.mu.Unlock()
	platformState := "disconnected"
	if a.IsConnected() {
		platformState = "connected"
	}
	gateway.APIWriteRuntimeStatusSafe(ctx, a.BasePlatformAdapter, "api_server_heartbeat",
		map[string]any{"platform_state": platformState, "platform_metrics": a.apiServerStatusPayload(nil)})
}

// Counts one served request with its tokens and latency, then republishes runtime status
func (a *APIServerAdapter) recordAPIMetrics(ctx context.Context, usage map[string]any, latencySeconds float64) {
	a.mu.Lock()
	a.resetMetricsIfNeededLocked()
	a.metricsRequestsToday++
	a.metricsMessagesToday++
	a.metricsTokensToday += totalTokensFromUsage(usage)
	now := apiNow()
	a.metricsLastRequestAt = &now
	a.metricsLatencyMs = append(a.metricsLatencyMs, max(0.0, latencySeconds*1000.0))
	if len(a.metricsLatencyMs) > apiServerLatencySampleLimit {
		a.metricsLatencyMs = append([]float64(nil), a.metricsLatencyMs[len(a.metricsLatencyMs)-apiServerLatencySampleLimit:]...)
	}
	a.mu.Unlock()
	a.publishRuntimeStatus(ctx)
}

// Publishes runtime status every heartbeat interval while connected
func (a *APIServerAdapter) heartbeatLoop(ctx context.Context) {
	for a.IsConnected() {
		a.publishRuntimeStatus(ctx)
		if !a.IsConnected() {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(apiServerHeartbeatSeconds * float64(time.Second))):
		}
	}
}

// Advertised model name, the explicit override then the active profile name then icebath-agent
func resolveModelName(ctx context.Context, explicit string) string {
	profileName := ""
	func() {
		defer func() { _ = recover() }()
		profile := profiles.GetActiveProfileName(ctx)
		if profile != "" && profile != "default" && profile != "custom" {
			profileName = profile
		}
	}()
	return provider.ResolveEffectiveModel(explicit, profileName, "icebath-agent")
}

// CORS headers for an allowed browser origin, nil when none apply
func (a *APIServerAdapter) corsHeadersForOrigin(origin string) [][2]string {
	if origin == "" || len(a.corsOrigins) == 0 {
		return nil
	}
	out := append([][2]string(nil), corsHeaders...)
	for _, o := range a.corsOrigins {
		if o == "*" {
			return append(out, [2]string{"Access-Control-Allow-Origin", "*"}, [2]string{"Access-Control-Max-Age", "600"})
		}
	}
	for _, o := range a.corsOrigins {
		if o == origin {
			return append(out, [2]string{"Access-Control-Allow-Origin", origin}, [2]string{"Vary", "Origin"},
				[2]string{"Access-Control-Max-Age", "600"})
		}
	}
	return nil
}

// Headers of an SSE stream prepared inside a handler with CORS resolved up front
func (a *APIServerAdapter) sseHeaders(request *gateway.WebRequest, extra map[string]string) map[string]string {
	headers := map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"}
	for _, h := range a.corsHeadersForOrigin(request.Header.Get("Origin")) {
		headers[h[0]] = h[1]
	}
	for k, v := range extra {
		headers[k] = v
	}
	return headers
}

// Whether a non browser client or a configured browser origin is calling
func (a *APIServerAdapter) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	for _, o := range a.corsOrigins {
		if o == "*" || o == origin {
			return true
		}
	}
	return false
}

// Request metadata with line breaks flattened, trimmed and cut for security logs
func cleanLogValue(value any, maxLen int) string {
	if value == nil {
		return ""
	}
	text := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(pyrepr.Str(value)))
	runes := []rune(text)
	if len(runes) > maxLen {
		return string(runes[:maxLen])
	}
	return text
}

// Ordered non secret source metadata of a request for audit warnings
func (a *APIServerAdapter) requestAuditContext(request *gateway.WebRequest) [][2]string {
	peerIP := request.Remote
	remote := request.Remote
	if remote == "" {
		remote = peerIP
	}
	return [][2]string{
		{"remote", cleanLogValue(remote, 200)},
		{"peer_ip", cleanLogValue(peerIP, 200)},
		{"forwarded_for", cleanLogValue(request.Header.Get("X-Forwarded-For"), 200)},
		{"real_ip", cleanLogValue(request.Header.Get("X-Real-IP"), 200)},
		{"method", cleanLogValue(request.Method, 16)},
		{"path", cleanLogValue(request.RelURL.RequestURI(), 500)},
		{"user_agent", cleanLogValue(request.Header.Get("User-Agent"), 300)},
	}
}

// Audit context as key repr pairs, source unknown when empty
func (a *APIServerAdapter) requestAuditLogSuffix(request *gateway.WebRequest) string {
	var fields []string
	for _, kv := range a.requestAuditContext(request) {
		if kv[1] != "" {
			fields = append(fields, kv[0]+"="+pyrepr.Repr(kv[1]))
		}
	}
	if len(fields) == 0 {
		return "source='unknown'"
	}
	return strings.Join(fields, " ")
}

// Safe API source metadata persisted on cron jobs created over HTTP
func (a *APIServerAdapter) cronOriginFromRequest(request *gateway.WebRequest) map[string]any {
	ctx := map[string]string{}
	for _, kv := range a.requestAuditContext(request) {
		ctx[kv[0]] = kv[1]
	}
	origin := map[string]any{"platform": "api_server", "chat_id": "api"}
	for _, pair := range [][2]string{{"remote", "source_ip"}, {"peer_ip", "peer_ip"}, {"forwarded_for", "forwarded_for"},
		{"real_ip", "real_ip"}, {"user_agent", "user_agent"}} {
		if ctx[pair[0]] != "" {
			origin[pair[1]] = ctx[pair[0]]
		}
	}
	return origin
}

// API key authorized for the URL selected profile, empty when a named profile has no usable key
func (a *APIServerAdapter) expectedAPIKey(ctx context.Context) string {
	profile := requestProfile(ctx)
	if profile == "" || profile == "default" {
		return a.apiKey
	}
	key, _, err := provider.GetSecret(ctx, "API_SERVER_KEY")
	if err != nil {
		apiLog().Warn(fmt.Sprintf("Failed to resolve a usable profile-scoped API_SERVER_KEY for %s: %s", pyrepr.Repr(profile), pyTypeName(err)))
		return ""
	}
	if provider.HasUsableSecret(key, 16) {
		return key
	}
	return ""
}

// Gateway auth failure 401
func authFailedResponse() *gateway.WebResponse {
	return jsonResponse(pyjson.ObjectOf("error", pyjson.ObjectOf("message", "Invalid gateway API key (API_SERVER_KEY)",
		"type", "gateway_auth_error", "code", "gateway_auth_failed")), http.StatusUnauthorized, nil)
}

// Nil when the bearer token matches, else the 401, a named profile without a key failing closed
func (a *APIServerAdapter) checkAuth(ctx context.Context, request *gateway.WebRequest) *gateway.WebResponse {
	profile := requestProfile(ctx)
	expected := a.expectedAPIKey(ctx)
	if expected == "" {
		if !(profile != "" && profile != "default") {
			return nil
		}
		apiLog().Warn(fmt.Sprintf("API server rejected request for profile %s: no profile-scoped API_SERVER_KEY is configured; %s",
			pyrepr.Repr(profile), a.requestAuditLogSuffix(request)))
		return authFailedResponse()
	}
	header := request.Header.Get("Authorization")
	if strings.HasPrefix(header, "Bearer ") {
		token := strings.TrimSpace(header[7:])
		if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
			return nil
		}
	}
	apiLog().Warn("API server rejected invalid API key: " + a.requestAuditLogSuffix(request))
	return authFailedResponse()
}

var callbackPlatformRE = regexp.MustCompile(`^[a-z0-9_]+$`)

// Lowercased platform name with dashes as underscores, empty when not a plain identifier
func normalizeCallbackPlatform(value string) string {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "-", "_")
	if callbackPlatformRE.MatchString(normalized) {
		return normalized
	}
	return ""
}

// Python type name of an error for logs that must not carry its text
func pyTypeName(err error) string {
	if typed, ok := err.(interface{ PyTypeName() string }); ok {
		return typed.PyTypeName()
	}
	if pyErr, ok := err.(*gateway.PyError); ok {
		return pyErr.Kind
	}
	return fmt.Sprintf("%T", err)
}
