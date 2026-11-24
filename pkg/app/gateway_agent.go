package app

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"

	"github.com/nickheyer/icebath/pkg/internal/textutil"
)

// How long a turn waits for its settlement after an interrupt
var gatewayInterruptSettleSeconds = 10.0

// Poll interval of the turn pump
const gatewayAgentPoll = 100 * time.Millisecond

// Session usage keys mapped onto the counters the status bar, spinner and usage command read
var gatewayCounterFromUsage = [][2]string{
	{"session_input_tokens", "input_tokens"}, {"session_output_tokens", "output_tokens"},
	{"session_prompt_tokens", "prompt_tokens"}, {"session_completion_tokens", "completion_tokens"},
	{"session_total_tokens", "total_tokens"}, {"session_cache_read_tokens", "cache_read_tokens"},
	{"session_cache_write_tokens", "cache_write_tokens"}, {"session_reasoning_tokens", "reasoning_tokens"},
	{"session_api_calls", "api_calls"},
}

// Result keys every gateway turn result starts from
func gatewayResultDefaults() map[string]any {
	return map[string]any{
		"final_response": "", "failed": false, "partial": false, "error": nil, "failure_reason": nil,
		"completed": true, "interrupted": false, "interrupt_message": nil, "api_calls": 0,
		"pending_steer": nil, "last_reasoning": nil, "response_previewed": false,
		"response_transformed": false, "pre_transform_response": nil, "billing_block": nil,
		"auxiliary_usage": map[string]any{},
	}
}

// One run of a conversation and what each consumer got of the reply's current segment
type gatewayTurn struct {
	sessionID string
	admission any
	shown     []string
	spoken    []string
	seen      map[string]bool
	answered  map[any]bool
	gapped    bool
}

func newGatewayTurn(sessionID string) *gatewayTurn {
	return &gatewayTurn{sessionID: sessionID, seen: map[string]bool{}, answered: map[any]bool{}}
}

func (t *gatewayTurn) shownText() string { return strings.Join(t.shown, "") }

func (t *gatewayTurn) spokenText() string { return strings.Join(t.spoken, "") }

// Moves the current segment into the seen set and starts a fresh one
func (t *gatewayTurn) closeSegment() {
	t.seen[strings.TrimSpace(t.shownText())] = true
	t.seen[strings.TrimSpace(t.spokenText())] = true
	t.shown, t.spoken = nil, nil
}

// Callbacks and launch values the one shot chat hands the gateway backed agent
type gatewayAgentOptions struct {
	SessionDB                *gatewaycli.GatewaySessionStore
	Connection               *gatewaycli.GatewayConnection
	SessionID                string
	Model                    string
	Provider                 string
	BaseURL                  string
	APIMode                  string
	MaxIterations            int
	ToolProgressMode         string
	ReasoningConfig          map[string]any
	ServiceTier              string
	ClarifyCallback          func(questions []any) map[string]any
	ReasoningCallback        func(text string)
	ThinkingCallback         func(text any)
	ToolProgressCallback     func(eventType, functionName, preview string, functionArgs map[string]any, extra map[string]any)
	ToolCompleteCallback     func(toolCallID, functionName string, functionArgs map[string]any, functionResult string)
	StreamDeltaCallback      func(text any)
	ToolGenCallback          func(toolName string)
	NoticeCallback           func(notice provider.AgentNotice)
	NoticeClearCallback      func(key string)
	InterimAssistantCallback func(text any, alreadyStreamed bool)
	ToolEnv                  *tools.Env
}

// Gateway backed stand in for the in process agent, the turn runs in the gateway and this replays its events
type GatewayAgent struct {
	ctx        context.Context
	connection *gatewaycli.GatewayConnection
	sessionDB  *gatewaycli.GatewaySessionStore
	opts       gatewayAgentOptions

	mu                    sync.Mutex
	sessionID             string
	model                 string
	provider              string
	baseURL               string
	apiMode               string
	maxIterations         int
	serviceTier           string
	reasoningCallback     func(text string)
	showReasoningSent     *gatewayShowReasoning
	suppressStatusOutput  bool
	printFn               func(string)
	sessionMessages       []map[string]any
	pendingCLIUserMessage map[string]any
	interruptRequested    bool
	usageStale            bool
	counters              map[string]int
	todoStore             *tools.TodoStore
	generation            any
	admission             any
	turn                  *gatewayTurn
	epoch                 int
	interruptSentAt       *time.Time
	rateLimitState        *provider.RateLimitState
	toolProgressMode      string
	reasoningConfig       map[string]any
	moaPreset             string
	usageListener         *gatewaycli.SessionListener
	muteNotificationReply bool
}

// Session id, show flag and connection epoch of the last show_reasoning write
type gatewayShowReasoning struct {
	sessionID string
	show      bool
	epoch     int
}

// Builds the agent over the session store's connection, refusing when there is no gateway session
func newGatewayAgent(ctx context.Context, o gatewayAgentOptions) (*GatewayAgent, error) {
	conn := o.Connection
	if conn == nil && o.SessionDB != nil {
		conn = o.SessionDB.Connection
	}
	if conn == nil {
		return nil, &gateway.GatewayClientError{Msg: "Gateway session unavailable; no local fallback"}
	}
	a := &GatewayAgent{ctx: ctx, connection: conn, sessionDB: o.SessionDB, opts: o, sessionID: o.SessionID,
		model: o.Model, provider: o.Provider, baseURL: o.BaseURL, apiMode: o.APIMode, maxIterations: o.MaxIterations,
		counters: map[string]int{}, todoStore: tools.NewTodoStore(), toolProgressMode: "all"}
	a.resetSessionFigures()
	a.setTodoStore(a.todoStore)
	a.SetToolProgressMode(o.ToolProgressMode)
	a.reasoningConfig = o.ReasoningConfig
	if o.Provider == "moa" {
		a.moaPreset = o.Model
	}
	a.applyInfo(a.stateInfo())
	a.SetReasoningCallback(o.ReasoningCallback)
	a.SetServiceTier(o.ServiceTier)
	onUsage := gatewaycli.SessionListener(func(ctx context.Context, usage map[string]any) { a.applySessionUsage(ctx, usage) })
	a.usageListener = &onUsage
	state := a.state()
	state.Lock()
	state.UsageListeners = append(state.UsageListeners, a.usageListener)
	state.Unlock()
	state.Lock()
	usage := state.Usage
	state.Unlock()
	if len(usage) > 0 {
		a.applySessionUsage(ctx, usage)
	}
	return a, nil
}

// Current tool progress display mode
func (a *GatewayAgent) ToolProgressMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.toolProgressMode
}

// Sets the tool progress mode and sends it to the session as its verbose setting when it changed
func (a *GatewayAgent) SetToolProgressMode(mode string) {
	a.mu.Lock()
	unchanged := mode == a.toolProgressMode
	a.toolProgressMode = mode
	a.mu.Unlock()
	if unchanged || !slices.Contains([]string{"off", "new", "all", "verbose"}, mode) {
		return
	}
	if _, err := a.connection.Call(a.ctx, "config.set", map[string]any{"session_id": a.SessionID(), "key": "verbose", "value": mode}); err != nil {
		if isGatewayClientError(err) {
			pkgLog().Debug(fmt.Sprintf("config.set verbose failed: %s", err.Error()))
			return
		}
		panic(err)
	}
}

// Reasoning config this agent holds
func (a *GatewayAgent) ReasoningConfig() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reasoningConfig
}

// Raised when a resume snapshot lacks its revision or generation fence
var errSnapshotFence = errors.New("snapshot carries no revision or execution_generation")

// Revision and execution generation of a resume snapshot
func snapshotFence(snapshot any) (any, any, error) {
	obj, ok := pyjson.AsObject(snapshot)
	if !ok {
		return nil, nil, fmt.Errorf("%w: the snapshot is %s", errSnapshotFence, errchain.Kind(snapshot))
	}
	revision, hasRevision := obj.Get("revision")
	generation, hasGeneration := obj.Get("execution_generation")
	if !hasRevision || !hasGeneration {
		return nil, nil, errSnapshotFence
	}
	return revision, generation, nil
}

// Whether the session runs a route other than the given model and provider
func (a *GatewayAgent) routeOffSession(model, provider string) bool {
	state := a.state()
	state.Lock()
	sessionModel := pyrepr.Truthy(state.Info["model"])
	state.Unlock()
	return model != "" && sessionModel && !state.RunsRoute(model, provider)
}

// Commits a session scoped model switch
func (a *GatewayAgent) SwitchModel(newModel, newProvider string) (bool, error) {
	if newModel == a.Model() && (newProvider == "" || newProvider == a.Provider()) && !a.routeOffSession(newModel, newProvider) {
		return true, nil
	}
	sessionID := a.SessionID()
	payload := map[string]any{"model": newModel}
	if newProvider != "" {
		payload["provider"] = newProvider
	}
	snapshot, err := a.connection.Call(a.ctx, "session.resume", map[string]any{"session_id": sessionID})
	if err != nil {
		return false, err
	}
	revision, generation, err := snapshotFence(snapshot)
	if err != nil {
		return false, err
	}
	mutate := func(payload map[string]any) (map[string]any, error) {
		raw, err := a.connection.Call(a.ctx, "session.mutate", map[string]any{"session_id": sessionID, "operation": "model",
			"expected_revision": revision, "expected_generation": generation, "request_id": uuidHex(), "payload": payload})
		if err != nil {
			return nil, err
		}
		result, _ := pyjson.Plain(raw).(map[string]any)
		if result == nil {
			result = map[string]any{}
		}
		return result, nil
	}
	result, err := mutate(payload)
	if err != nil {
		return false, err
	}
	if result["status"] == "confirmation_required" {
		return false, &gateway.GatewayClientError{Msg: modelRefusalText(result, false)}
	}
	state := a.state()
	if after, err := a.connection.Call(a.ctx, "session.resume", map[string]any{"session_id": sessionID}); err != nil {
		if !isGatewayClientError(err) {
			return false, err
		}
		pkgLog().Warn(fmt.Sprintf("session.resume after the committed model switch failed: %s", err.Error()))
	} else {
		state.AdoptSnapshot(after)
	}
	committedProvider := newProvider
	if p, _ := result["provider"].(string); p != "" {
		committedProvider = p
	}
	committedModel := newModel
	if m, _ := result["model"].(string); m != "" {
		committedModel = m
	}
	state.Lock()
	info := copyMap(state.Info)
	info["model"] = committedModel
	if committedProvider != "" {
		info["provider"] = committedProvider
	}
	state.Info = info
	state.Unlock()
	a.applyInfo(info)
	return true, nil
}

// Todo store mirroring the gateway's plan for this session
func (a *GatewayAgent) TodoStore() *tools.TodoStore {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.todoStore
}

// Swaps in a fresh todo store seeded with the gateway's snapshot of the current session
func (a *GatewayAgent) setTodoStore(store *tools.TodoStore) {
	a.mu.Lock()
	a.todoStore = store
	a.mu.Unlock()
	state := a.state()
	state.Lock()
	snapshot := state.PlanState
	state.Unlock()
	if len(snapshot) > 0 {
		a.setTodos(snapshot)
	}
}

// Gateway session this agent replays
func (a *GatewayAgent) SessionID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionID
}

// Reasoning sink the CLI installed, nil when reasoning is not shown
func (a *GatewayAgent) ReasoningCallback() func(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reasoningCallback
}

// Installs the reasoning sink and sends the session's show_reasoning flag to match
func (a *GatewayAgent) SetReasoningCallback(callback func(text string)) {
	a.mu.Lock()
	a.reasoningCallback = callback
	a.mu.Unlock()
	a.syncShowReasoning()
}

// Sends show_reasoning unless this viewer already set the same value on this session and gateway life
func (a *GatewayAgent) syncShowReasoning() {
	sessionID, epoch := a.SessionID(), a.connection.Epoch()
	a.mu.Lock()
	show := a.reasoningCallback != nil
	sent := a.showReasoningSent
	a.mu.Unlock()
	if sent != nil && sent.sessionID == sessionID && sent.show == show && (!show || sent.epoch == epoch) {
		return
	}
	value := "0"
	if show {
		value = "1"
	}
	if _, err := a.connection.CallWith(a.ctx, "config.set", map[string]any{"session_id": sessionID, "key": "show_reasoning", "value": value},
		gatewaycli.CallOptions{Timeout: 10 * time.Second}); err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		pkgLog().Debug(fmt.Sprintf("config.set show_reasoning failed: %s", err.Error()))
		return
	}
	a.mu.Lock()
	a.showReasoningSent = &gatewayShowReasoning{sessionID: sessionID, show: show, epoch: a.connection.Epoch()}
	a.mu.Unlock()
}

// Service tier this agent was built with
func (a *GatewayAgent) ServiceTier() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serviceTier
}

// Sets the service tier and sends it as the session override when it differs from the last one sent
func (a *GatewayAgent) SetServiceTier(tier string) {
	a.mu.Lock()
	a.serviceTier = tier
	a.mu.Unlock()
	state := a.state()
	wanted := firstNonEmpty(tier, "normal")
	state.Lock()
	sent := state.ServiceTierSent
	state.Unlock()
	if (sent != nil && wanted == *sent) || (sent == nil && tier == "") {
		return
	}
	if _, err := a.connection.CallWith(a.ctx, "config.set", map[string]any{"session_id": a.SessionID(), "key": "service_tier", "value": wanted},
		gatewaycli.CallOptions{Timeout: 10 * time.Second}); err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		pkgLog().Debug(fmt.Sprintf("config.set service_tier failed: %s", err.Error()))
		return
	}
	state.Lock()
	state.ServiceTierSent = &wanted
	state.Unlock()
}

// Writes nothing, the gateway owns the session log
func (a *GatewayAgent) persistSession() {}

// Releases nothing, the LLM clients live in the gateway
func (a *GatewayAgent) releaseClients() {}

// Stops nothing, the gateway runs the memory providers of its session
func (a *GatewayAgent) shutdownMemoryProvider() {}

// Cached gateway state of the current session
func (a *GatewayAgent) state() *gatewaycli.SessionState { return a.connection.State(a.SessionID()) }

// Copy of the current session's info dict
func (a *GatewayAgent) stateInfo() map[string]any {
	state := a.state()
	state.Lock()
	defer state.Unlock()
	return copyMap(state.Info)
}

// Adopts the model, provider and context length the session's info names
func (a *GatewayAgent) applyInfo(info map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m, _ := info["model"].(string); m != "" {
		a.model = m
	}
	if p, _ := info["provider"].(string); p != "" {
		a.provider = p
	}
}

// Model the session runs, or the one turn pick this agent staged
func (a *GatewayAgent) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

// Provider the session runs
func (a *GatewayAgent) Provider() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.provider
}

// Base URL the CLI resolved for this agent
func (a *GatewayAgent) BaseURL() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.baseURL
}

// API mode the CLI resolved for this agent
func (a *GatewayAgent) APIMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.apiMode
}

// Turn limit this agent was built with
func (a *GatewayAgent) MaxIterations() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.maxIterations
}

// Rate limit state the gateway last reported for this session
func (a *GatewayAgent) RateLimitState() *provider.RateLimitState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rateLimitState
}

// Installs one session cumulative usage dict on the figures the status bar reads
func (a *GatewayAgent) applySessionUsage(ctx context.Context, usage map[string]any) {
	if usage == nil {
		return
	}
	a.mu.Lock()
	for _, pair := range gatewayCounterFromUsage {
		if n, ok := pyIntValue(usage[pair[1]]); ok {
			a.counters[pair[0]] = n
		}
	}
	if raw, has := usage["rate_limit"]; has {
		a.rateLimitState = rateLimitStateOf(raw)
	}
	if m, _ := usage["model"].(string); m != "" {
		a.model = m
	}
	if p, _ := usage["provider"].(string); p != "" {
		a.provider = p
	}
	a.mu.Unlock()
}

// Zeroes the per session figures the bar, spinner and usage command read, the context length staying
func (a *GatewayAgent) resetSessionFigures() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, pair := range gatewayCounterFromUsage {
		a.counters[pair[0]] = 0
	}
}

// Drops the previous session's figures and shows the gateway's for the current one
func (a *GatewayAgent) ResetSessionState() {
	a.resetSessionFigures()
	state := a.state()
	state.Lock()
	hasMessages, usage := len(state.Messages) > 0, state.Usage
	state.Unlock()
	if !hasMessages && len(usage) == 0 {
		return
	}
	if !a.refreshStats(nil) && len(usage) > 0 {
		a.applySessionUsage(a.ctx, usage)
	}
}

// Clears the interrupt flag this agent holds
func (a *GatewayAgent) ClearInterrupt() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.interruptRequested = false
}

// Whether an interrupt was requested for the running turn
func (a *GatewayAgent) InterruptRequested() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.interruptRequested
}

// First 300 characters of an error, the one line a failed side call shows
func (a *GatewayAgent) SummarizeAPIError(err error) string { return textutil.First(err.Error(), 300) }

// Sends session.interrupt for the running generation, the message staying with the CLI
func (a *GatewayAgent) Interrupt() {
	a.mu.Lock()
	a.interruptRequested = true
	if a.interruptSentAt == nil {
		now := time.Now()
		a.interruptSentAt = &now
	}
	generation := a.generation
	a.mu.Unlock()
	if generation == nil {
		state := a.state()
		state.Lock()
		generation = state.ExecutionGeneration
		state.Unlock()
	}
	if _, err := a.connection.CallWith(a.ctx, "session.interrupt", map[string]any{"session_id": a.SessionID(), "execution_generation": generation},
		gatewaycli.CallOptions{Timeout: 5 * time.Second}); err != nil {
		pkgLog().Debug(fmt.Sprintf("session.interrupt failed: %s", err.Error()))
	}
}

// Integer value of a decoded JSON number that is not a bool
func pyIntValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case *big.Int:
		if n.IsInt64() {
			return int(n.Int64()), true
		}
	}
	return 0, false
}

// Numeric value of a decoded JSON int or float that is not a bool
func pyNumber(v any) (float64, bool) {
	if n, ok := pyIntValue(v); ok {
		return float64(n), true
	}
	f, ok := v.(float64)
	return f, ok
}

// Whether err is the Python GatewayClientError or one of its subclasses
func isGatewayClientError(err error) bool {
	var ce *gateway.GatewayClientError
	return errors.As(err, &ce)
}

// Shallow copy of a map, an empty map for nil
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
