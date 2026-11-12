package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// How long a turn waits for its settlement after an interrupt
var gatewayInterruptSettleSeconds = 10.0

// Poll interval of the turn pump
const gatewayAgentPoll = 100 * time.Millisecond

// Reply of steer and redirect when the owner consumed the text as a typed voice stop
const gatewayVoiceStopped = "voice_stopped"

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

// Context compressor figures as the status bar and banner read them
type gatewayCompressorView struct {
	lastPromptTokens     int
	lastRealPromptTokens *int
	contextLength        int
	compressionCount     int
	configContextLength  *int
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
	t.seen[pystr.Strip(t.shownText())] = true
	t.seen[pystr.Strip(t.spokenText())] = true
	t.shown, t.spoken = nil, nil
}

// Callbacks and launch values the classic CLI hands the gateway backed agent
type gatewayAgentOptions struct {
	SessionDB                *gatewaycli.GatewaySessionStore
	Connection               *gatewaycli.GatewayConnection
	SessionID                string
	Model                    string
	Provider                 string
	BaseURL                  string
	APIMode                  string
	MaxIterations            int
	QuietMode                bool
	ToolProgressMode         string
	ReasoningConfig          map[string]any
	ServiceTier              string
	PushReasoning            bool
	OneTurn                  bool
	OneTurnReasoning         string
	OneTurnAlias             string
	ClarifyCallback          func(questions []any) map[string]any
	ConnectionCallback       func(args ...any)
	ConnectionUpdateCallback func(snapshot map[string]any)
	ReasoningCallback        func(text string)
	ThinkingCallback         func(text any)
	ToolProgressCallback     func(eventType, functionName, preview string, functionArgs map[string]any, extra map[string]any)
	ToolCompleteCallback     func(toolCallID, functionName string, functionArgs map[string]any, functionResult string)
	StreamDeltaCallback      func(text any)
	ToolGenCallback          func(toolName string)
	NoticeCallback           func(notice provider.AgentNotice)
	NoticeClearCallback      func(key string)
	ReactionCallback         func(kind string)
	ModelConfirmCallback     func(refusal map[string]any) bool
	ModalWithdrawCallback    func(method string)
	ToolEnv                  *tools.Env
	ApprovalCallback         func(ctx context.Context, command, description string, allowPermanent, allowSession, smartDenied bool) (string, error)
	SudoPasswordCallback     func(ctx context.Context) string
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
	quietMode             bool
	serviceTier           string
	reasoningCallback     func(text string)
	showReasoningSent     *gatewayShowReasoning
	controlUpdateCallback func()
	suppressStatusOutput  bool
	printFn               func(string)
	finite                bool
	openRequest           *gatewayOpenRequest
	sessionMessages       []map[string]any
	pendingCLIUserMessage map[string]any
	voiceTurnPending      bool
	interruptRequested    bool
	turnAskIndex          *int
	usageStale            bool
	compressor            gatewayCompressorView
	counters              map[string]int
	contextTokens         int
	turnCallsBase         int
	turnBaseUsageAnchor   *provider.UsageAnchor
	turnAPICalls          *int
	apiLatencyHistory     []float64
	apiOutputHistory      []float64
	todoStore             *tools.TodoStore
	compressionSummary    map[string]any
	generation            any
	admission             any
	turn                  *gatewayTurn
	turnCtx               context.Context
	epoch                 int
	interruptSentAt       *time.Time
	rateLimitState        *provider.RateLimitState
	configContextLength   *int
	toolProgressMode      string
	reasoningConfig       map[string]any
	reasoningPushed       bool
	moaPreset             string
	turnRoute             map[string]any
	takenTurnRoute        *gatewayTakenRoute
	turnReasoning         string
	usageListener         *gatewaycli.SessionListener
	requestListener       *gatewaycli.SessionListener
	credits               *provider.CreditsCapture
	notificationConfig    map[string]any
	muteNotificationReply bool
}

// Session id, show flag and connection epoch of the last show_reasoning write
type gatewayShowReasoning struct {
	sessionID string
	show      bool
	epoch     int
}

// Request id and method of the server request a modal is answering
type gatewayOpenRequest struct {
	id     any
	method string
}

// One turn model pick and its effort taken by the admission being sent
type gatewayTakenRoute struct {
	route     map[string]any
	reasoning string
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
		quietMode: o.QuietMode, counters: map[string]int{}, todoStore: tools.NewTodoStore(), toolProgressMode: "all"}
	a.resetSessionFigures()
	a.setTodoStore(a.todoStore)
	a.SetToolProgressMode(o.ToolProgressMode)
	if o.PushReasoning {
		a.reasoningPushed = a.PinReasoning(o.ReasoningConfig)
	} else {
		a.reasoningConfig = o.ReasoningConfig
	}
	if o.Provider == "moa" {
		a.moaPreset = o.Model
	}
	if o.OneTurn {
		a.turnRoute = a.routeOffSession(o.Model, o.Provider, o.OneTurnAlias)
		a.turnReasoning = o.OneTurnReasoning
	}
	a.applyInfo(a.stateInfo())
	if a.turnRoute != nil {
		a.mu.Lock()
		a.model = o.Model
		if p, _ := a.turnRoute["provider"].(string); p != "" {
			a.provider = p
		}
		a.mu.Unlock()
	}
	a.SetReasoningCallback(o.ReasoningCallback)
	a.SetServiceTier(o.ServiceTier)
	onUsage := gatewaycli.SessionListener(func(ctx context.Context, usage map[string]any) { a.applySessionUsage(ctx, usage) })
	onWithdrawn := gatewaycli.SessionListener(func(_ context.Context, payload map[string]any) { a.onRequestWithdrawn(payload) })
	a.usageListener, a.requestListener = &onUsage, &onWithdrawn
	state := a.state()
	state.Lock()
	state.UsageListeners = append(state.UsageListeners, a.usageListener)
	state.RequestListeners = append(state.RequestListeners, a.requestListener)
	state.Unlock()
	state.Lock()
	usage := state.Usage
	state.Unlock()
	if len(usage) > 0 {
		a.applySessionUsage(ctx, usage)
	}
	return a, nil
}

// Registers the usage and request listeners on a session's state unless they are already there
func (a *GatewayAgent) listen(sessionID string) {
	state := a.connection.State(sessionID)
	state.Lock()
	defer state.Unlock()
	if !slices.Contains(state.UsageListeners, a.usageListener) {
		state.UsageListeners = append(state.UsageListeners, a.usageListener)
	}
	if !slices.Contains(state.RequestListeners, a.requestListener) {
		state.RequestListeners = append(state.RequestListeners, a.requestListener)
	}
}

// Removes the listeners from the state of a session the agent left, so its later frames repaint nothing here
func (a *GatewayAgent) unlisten(sessionID string) {
	state := a.connection.State(sessionID)
	state.Lock()
	defer state.Unlock()
	state.UsageListeners = gatewaycli.WithoutListener(state.UsageListeners, a.usageListener)
	state.RequestListeners = gatewaycli.WithoutListener(state.RequestListeners, a.requestListener)
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
			slog.Debug(fmt.Sprintf("config.set verbose failed: %s", err.Error()))
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

// Sets the reasoning config, writing the session effort only when it differs from what the session runs
func (a *GatewayAgent) SetReasoningConfig(config map[string]any) { a.setReasoning(config, false) }

// Makes the effort the session's own choice, true when the owner committed it
func (a *GatewayAgent) PinReasoning(config map[string]any) bool { return a.setReasoning(config, true) }

// Whether a user picked effort was committed at build
func (a *GatewayAgent) ReasoningPushed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reasoningPushed
}

// Holds a one turn effort for the next admission only, sending nothing to the session
func (a *GatewayAgent) StageTurnReasoning(config map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasoningConfig = config
	a.turnReasoning = effortWord(config)
}

// Puts a reasoning level back on this agent after a one turn pick, the session never written
func (a *GatewayAgent) RestoreReasoning(config map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasoningConfig = config
	a.turnReasoning = ""
}

func (a *GatewayAgent) setReasoning(config map[string]any, explicit bool) bool {
	a.mu.Lock()
	a.reasoningConfig = config
	a.mu.Unlock()
	effort := effortWord(config)
	if effort == "" {
		return false
	}
	state := a.state()
	state.Lock()
	current := state.Info["reasoning_effort"]
	state.Unlock()
	if !explicit && pyjson.Equal(current, effort) {
		return true
	}
	sessionID := a.SessionID()
	snapshot, err := a.connection.Call(a.ctx, "session.resume", map[string]any{"session_id": sessionID})
	if err == nil {
		var revision, generation any
		revision, generation, err = snapshotFence(snapshot)
		if err == nil {
			_, err = a.connection.Call(a.ctx, "session.mutate", map[string]any{"session_id": sessionID, "request_id": uuidHex(),
				"expected_revision": revision, "expected_generation": generation, "operation": "reasoning",
				"payload": map[string]any{"effort": effort}})
		}
	}
	if err != nil {
		if isGatewayClientError(err) || errors.Is(err, errSnapshotFence) {
			slog.Debug(fmt.Sprintf("session.mutate reasoning failed: %s", err.Error()))
			return false
		}
		panic(err)
	}
	state.Lock()
	info := copyMap(state.Info)
	info["reasoning_effort"] = effort
	state.Info = info
	state.Unlock()
	return true
}

// Raised when a resume snapshot lacks its revision or generation fence
var errSnapshotFence = errors.New("snapshot carries no revision or execution_generation")

// Revision and execution generation of a resume snapshot
func snapshotFence(snapshot any) (any, any, error) {
	obj, ok := pyjson.AsObject(snapshot)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", errSnapshotFence, pyrepr.TypeName(snapshot))
	}
	revision, hasRevision := obj.Get("revision")
	generation, hasGeneration := obj.Get("execution_generation")
	if !hasRevision || !hasGeneration {
		return nil, nil, errSnapshotFence
	}
	return revision, generation, nil
}

// Model and provider a turn sends when the session runs another route, nil when it runs this one
func (a *GatewayAgent) routeOffSession(model, provider, alias string) map[string]any {
	state := a.state()
	state.Lock()
	sessionModel := pyrepr.Truthy(state.Info["model"])
	state.Unlock()
	if model == "" || !sessionModel || (state.RunsRoute(model, provider) && alias == "") {
		return nil
	}
	route := map[string]any{"model": firstNonEmpty(alias, model)}
	if provider != "" {
		route["provider"] = provider
	}
	return route
}

// Commits a session scoped model switch, or stages and restores a one turn pick without writing the session
func (a *GatewayAgent) SwitchModel(newModel, newProvider string, once, restore bool, alias string) (bool, error) {
	if once || restore {
		var route map[string]any
		if !restore {
			route = a.routeOffSession(newModel, newProvider, alias)
		}
		a.mu.Lock()
		a.turnRoute = route
		if restore {
			a.turnReasoning = ""
		}
		if newModel != "" {
			a.model = newModel
		}
		if newProvider != "" {
			a.provider = newProvider
		}
		a.mu.Unlock()
		return true, nil
	}
	if newModel == a.Model() && (newProvider == "" || newProvider == a.Provider()) && a.routeOffSession(newModel, newProvider, "") == nil {
		a.mu.Lock()
		a.turnRoute = nil
		a.mu.Unlock()
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
		if a.opts.ModelConfirmCallback == nil {
			return false, &gateway.GatewayClientError{Msg: modelRefusalText(result, false)}
		}
		if !a.opts.ModelConfirmCallback(result) {
			return false, nil
		}
		confirmed := copyMap(payload)
		confirmed["confirm"] = requireKey(result, "confirm")
		if result, err = mutate(confirmed); err != nil {
			return false, err
		}
		if result["status"] == "confirmation_required" {
			return false, &gateway.GatewayClientError{Msg: modelRefusalText(result, true)}
		}
	}
	state := a.state()
	if after, err := a.connection.Call(a.ctx, "session.resume", map[string]any{"session_id": sessionID}); err != nil {
		if !isGatewayClientError(err) {
			return false, err
		}
		slog.Warn(fmt.Sprintf("session.resume after the committed model switch failed: %s", err.Error()))
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
	a.mu.Lock()
	a.turnRoute = nil
	a.mu.Unlock()
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

// Repoints the agent at another gateway session, attaching this viewer and reading that session's state
func (a *GatewayAgent) SetSessionID(value string) {
	a.mu.Lock()
	previous := a.sessionID
	a.sessionID = value
	a.mu.Unlock()
	if previous == "" || value == previous || value == "" {
		return
	}
	a.unlisten(previous)
	if a.sessionDB != nil {
		if _, err := a.sessionDB.Attach(a.ctx, value, false); err != nil {
			if !isGatewayClientError(err) {
				panic(err)
			}
			slog.Debug(fmt.Sprintf("session.resume %s failed: %s", value, err.Error()))
		}
	} else if _, err := a.connection.Call(a.ctx, "session.resume", map[string]any{"session_id": value}); err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		slog.Debug(fmt.Sprintf("session.resume %s failed: %s", value, err.Error()))
	}
	a.listen(value)
	a.applyInfo(a.stateInfo())
	state := a.state()
	state.Lock()
	usage := state.Usage
	state.Unlock()
	if len(usage) > 0 {
		a.applySessionUsage(a.ctx, usage)
	}
	a.syncShowReasoning()
	if notice := a.lostTurnNotice(); notice != "" {
		a.emitPrint(notice)
	}
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
		slog.Debug(fmt.Sprintf("config.set show_reasoning failed: %s", err.Error()))
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
		slog.Debug(fmt.Sprintf("config.set service_tier failed: %s", err.Error()))
		return
	}
	state.Lock()
	state.ServiceTierSent = &wanted
	state.Unlock()
}

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
	if n, ok := pyIntValue(info["context_length"]); ok && n != 0 {
		a.compressor.contextLength = n
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

// Whether status output stays quiet
func (a *GatewayAgent) QuietMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.quietMode
}

// Tool schemas the gateway session loads, one per name of its info tools groups in toolset order
func (a *GatewayAgent) Tools() []map[string]any {
	groups, _ := a.stateInfo()["tools"].(map[string]any)
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []map[string]any
	for _, key := range keys {
		members, _ := groups[key].([]any)
		for _, name := range members {
			out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": name}})
		}
	}
	return out
}

// Names of the tools the gateway session loads
func (a *GatewayAgent) ValidToolNames() map[string]bool {
	names := map[string]bool{}
	for _, tool := range a.Tools() {
		names[pyrepr.Str(tool["function"].(map[string]any)["name"])] = true
	}
	return names
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
	if n, ok := pyIntValue(usage["last_prompt_tokens"]); ok {
		a.compressor.lastPromptTokens = n
	}
	if n, ok := pyIntValue(usage["context_length"]); ok && n != 0 {
		a.compressor.contextLength = n
	}
	if pin, has := usage["config_context_length"]; has {
		var held *int
		if n, ok := pyIntValue(pin); ok {
			held = &n
		}
		a.compressor.configContextLength, a.configContextLength = held, held
	}
	if n, ok := pyIntValue(usage["compression_count"]); ok {
		a.compressor.compressionCount = n
	}
	if source, has := usage["context_source"]; has && source != nil {
		if source == "provider_usage" {
			a.compressor.lastRealPromptTokens = nil
		} else {
			shownPlusOne := a.compressor.lastPromptTokens + 1
			a.compressor.lastRealPromptTokens = &shownPlusOne
		}
	}
	anchor := false
	if n, ok := pyIntValue(usage["context_tokens"]); ok {
		a.contextTokens = n
		anchor = true
	}
	latency, latencyOK := pyNumber(usage["avg_latency_s"])
	tps, tpsOK := pyNumber(usage["tokens_per_s"])
	if latencyOK && latency > 0 && tpsOK {
		a.apiLatencyHistory = []float64{latency}
		a.apiOutputHistory = []float64{tps * latency}
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
	if anchor {
		a.anchorDisplay(ctx)
	}
}

// Zeroes the per session figures the bar, spinner and usage command read, the context length staying
func (a *GatewayAgent) resetSessionFigures() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, pair := range gatewayCounterFromUsage {
		a.counters[pair[0]] = 0
	}
	a.compressor.lastPromptTokens, a.compressor.lastRealPromptTokens, a.compressor.compressionCount = 0, nil, 0
	a.contextTokens, a.turnCallsBase = 0, 0
	a.turnBaseUsageAnchor, a.turnAPICalls = nil, nil
	a.apiLatencyHistory, a.apiOutputHistory = nil, nil
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
func (a *GatewayAgent) SummarizeAPIError(err error) string { return pystr.Slice(err.Error(), 0, 300) }

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
		slog.Debug(fmt.Sprintf("session.interrupt failed: %s", err.Error()))
	}
}

// Context of the goroutine running the turn, where the CLI registered its prompt callbacks
func (a *GatewayAgent) threadCtx() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.turnCtx != nil {
		return a.turnCtx
	}
	return a.ctx
}

// Explicit stop of the running generation, the same session.interrupt
func (a *GatewayAgent) HardInterrupt() { a.Interrupt() }

// Sends a steer or redirect for the running generation, the voice stop word when the owner took it as one
func (a *GatewayAgent) correct(verb, text string) any {
	a.mu.Lock()
	generation := a.generation
	a.mu.Unlock()
	if generation == nil {
		return false
	}
	raw, err := a.connection.Call(a.ctx, "session."+verb, map[string]any{"session_id": a.SessionID(), "text": text, "execution_generation": generation})
	if err != nil {
		if isGatewayClientError(err) {
			return false
		}
		panic(err)
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	if result["voice_stopped"] == true {
		a.emitPrint(i18n.T(a.ctx, "cli.voice.stop_phrase_typed", "", nil))
		return gatewayVoiceStopped
	}
	return result["status"] == "queued" || result["status"] == "redirected"
}

// Steers the running turn, true when queued, the voice stop word when consumed as one
func (a *GatewayAgent) Steer(text string) any { return a.correct("steer", text) }

// Redirects the running turn, the redirect row becoming the turn's ask when accepted
func (a *GatewayAgent) Redirect(text string) any {
	accepted := a.correct("redirect", text)
	if accepted == true {
		a.mu.Lock()
		a.turnAskIndex = nil
		a.mu.Unlock()
	}
	return accepted
}

// Whether a steer or redirect answer means the text was taken
func gatewayCorrectionTaken(answer any) bool { return answer == true || answer == gatewayVoiceStopped }

// Whether the owner took the text as a typed voice stop
func gatewayCorrectionVoiceStopped(answer any) bool { return answer == gatewayVoiceStopped }

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
