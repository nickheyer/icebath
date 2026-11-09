package provider

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ChatCompletionAgent is every agent field and method the agent shaped request helpers read or write
type ChatCompletionAgent interface {
	StreamDiagAgent
	Provider() string
	APIMode() string
	BaseURL() string
	Model() string
	Platform() string
	VerboseLogging() bool
	RetrySession() *RetrySession
	InterruptRequested() bool
	Client() any
	ComputeNonStreamStaleTimeout(apiKwargs map[string]any) (any, error)
	CreateRequestOpenAIClient(reason string, apiKwargs map[string]any) (any, error)
	AbortRequestOpenAIClient(client any, reason string) error
	CloseRequestOpenAIClient(client any, reason string) error
	ActiveRequestAbort() *RequestAbortHook
	SetActiveRequestAbort(hook *RequestAbortHook)
	RunCodexStream(ctx context.Context, apiKwargs map[string]any, client any, onFirstDelta func()) (any, error)
	CodexOnFirstDelta() func()
	AnthropicMessagesCreate(ctx context.Context, apiKwargs map[string]any, client any) (any, error)
	ChatCompletionCapturingHeaders(ctx context.Context, client any, apiKwargs map[string]any) (any, error)
	CodexSilentHangHint(model any) (string, error)
	ReasoningConfig() map[string]any
	EphemeralReasoningOff() bool
	SetEphemeralReasoningOff(on bool)
	ReasoningEffortRejected() bool
	ReasoningDisableRejected() bool
	ReasoningFloorRequired() bool
	SetWireReasoningConfig(cfg map[string]any)
	OllamaNumCtx() int
	OllamaSupportsThinkingCached() bool
	CredentialPool() PoolProbe
	SetCredentialPool(pool PoolProbe)
	ClearCredentialPool()
	CredentialPoolEnv() PoolEnv
	ToolEnv() *tools.Env
	IsHeadedMode() (bool, error)
	CleanupBrowser(taskID string) error
	CreateRequestAnthropicClient(reason string) (any, error)
	AbortRequestAnthropicClient(client any, reason string) error
	CloseRequestAnthropicClient(client any, reason string) error
	ActiveCodexStreamRequestToken() any
	SetActiveCodexStreamRequestToken(token any)
	EmitWaitNotice(text string)
	EmitDiagnosticWait(text string)
	ClientLogContext() string
	SessionID() string
	MaxIterations() int
	SuppressStatusOutput() bool
	SafePrint(text string, diagnostic bool)
	EmitWarning(text string)
	BufferStatus(text string)
	ShouldSanitizeToolCalls() bool
	CopyReasoningContentForAPI(src, dst map[string]any)
	SanitizeToolCallsForStrictAPI(msg map[string]any, model string)
	CachedSystemPrompt() string
	EphemeralSystemPrompt() string
	PrefillMessages() []map[string]any
	SanitizeAPIMessages(msgs []map[string]any) []map[string]any
	EvictStaleOutboundToolImages(msgs []map[string]any) int
	StripImagesForRejectingModel(msgs []map[string]any) bool
	DropThinkingOnlyAndMergeUsers(msgs []map[string]any) []map[string]any
	SubstituteAPIContent(msg map[string]any)
	CanonicalizeAPIToolCalls(msgs []map[string]any)
	CloneMessageForSend(msg map[string]any) map[string]any
	BuildAPIKwargs(apiMessages []map[string]any) (map[string]any, error)
	SanitizeOutboundKwargs(kwargs map[string]any)
	InterruptibleAPICall(ctx context.Context, apiKwargs map[string]any) (any, error)
	Transport() Transport
	MaxTokens() *int
	IsAnthropicOAuth() bool
	AnthropicPreserveDots() bool
	AnthropicBaseURL() string
	StreamEmitters
	SetCodexOnFirstDelta(fn func())
	FireStreamDelta(text string)
	FireToolGenStarted(name string)
	FireReasoningDelta(text string)
	HasStreamConsumers() bool
	WantsReasoningDeltas() bool
	DisableStreaming()
	CurrentAPIRequestID() string
	IsSubagent() bool
	FallbackIndex() int
	CreditsCapture() *CreditsCapture
	CurrentStreamedAssistantText() string
	RouteSuppressedStreamText(text string)
	ResetStreamDeliveryTracking()
	WarningPresentationEnabled() bool
	LogPrefix() string
	SetLastAPIFirstChunkAt(at float64)
}

// RequestAbortHook is the abort callable an inline request publishes on the agent, compared by identity
type RequestAbortHook struct {
	Abort func(reason string) bool
}

// Implemented by a client exposing chat.completions.create over keyword arguments
type chatCompletionCreator interface {
	CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error)
}

// Implemented by the MoA facade whose chat.completions exposes prepare
type moaPrepareFacade interface {
	PrepareRequest(messages []map[string]any) (map[string]any, error)
}

// ModelDumper is an SDK model whose model_dump answers its plain dict
type ModelDumper interface {
	ModelDump(warnings bool) any
}

// Plain dict of an SDK model, any other value unchanged
func DumpIfModel(value any) any {
	if m, ok := value.(ModelDumper); ok {
		return m.ModelDump(false)
	}
	return value
}

// Runs one blocking request for the agent's api mode through the per request client the factory builds
func DispatchNonstreamingAPIRequest(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any, makeClient func(reason, kind string) (any, error)) (any, error) {
	switch agent.APIMode() {
	case string(ModeCodexResponses):
		client, err := makeClient("codex_stream_request", "openai")
		if err != nil {
			return nil, err
		}
		return agent.RunCodexStream(ctx, apiKwargs, client, agent.CodexOnFirstDelta())
	case string(ModeAnthropicMessages):
		client, err := makeClient("anthropic_messages_request", "anthropic_messages")
		if err != nil {
			return nil, err
		}
		return agent.AnthropicMessagesCreate(ctx, apiKwargs, client)
	case string(modeBedrockConverse):
		return bedrockConverseNonStream(ctx, apiKwargs)
	}
	if agent.Provider() == "moa" {
		client := agent.Client()
		if _, ok := client.(moaPrepareFacade); !ok {
			delete(apiKwargs, "_moa_prepared_request")
		}
		creator, ok := client.(chatCompletionCreator)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%T' object has no attribute 'chat'", client)
		}
		return creator.CreateChatCompletion(ctx, apiKwargs)
	}
	client, err := makeClient("chat_completion_request", "openai")
	if err != nil {
		return nil, err
	}
	apiKwargs = BypassChatSDKRequestTransform(apiKwargs, client)
	return agent.ChatCompletionCapturingHeaders(ctx, client, apiKwargs)
}

// Non streaming Converse call of agent kwargs with the routing keys popped
func bedrockConverseNonStream(ctx context.Context, apiKwargs map[string]any) (any, error) {
	region := "us-east-1"
	if r, ok := apiKwargs["__bedrock_region__"]; ok {
		region, _ = r.(string)
		delete(apiKwargs, "__bedrock_region__")
	}
	delete(apiKwargs, "__bedrock_converse__")
	result, _, err := bedrockConverseComplete(ctx, region, apiKwargs)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Inline stale budget from the agent resolver with the hosted openai-codex floor and cap, inf for a non numeric answer
func ResolveDirectStaleTimeout(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any) (float64, error) {
	raw, err := agent.ComputeNonStreamStaleTimeout(apiKwargs)
	if err != nil {
		return 0, err
	}
	var value float64
	switch v := raw.(type) {
	case float64:
		value = v
	case float32:
		value = float64(v)
	case int:
		value = float64(v)
	case int64:
		value = float64(v)
	default:
		return math.Inf(1), nil
	}
	baseURL := agent.BaseURL()
	if agent.APIMode() == string(ModeCodexResponses) && !(baseURL != "" && IsLocalEndpoint(baseURL)) {
		return boundCodexStaleTimeout(value, EstimateRequestContextTokens(ctx, apiKwargs)), nil
	}
	return value, nil
}

// Lifecycle of one agent inline request where done, cancelled and stale transition under one lock
type directInlineRequest struct {
	agent        ChatCompletionAgent
	apiKwargs    map[string]any
	staleTimeout float64
	callStart    time.Time
	mu           sync.Mutex
	client       any
	done         bool
	stale        bool
	cancelled    bool
	hook         *RequestAbortHook
	surface      func(StreamEvent)
	staleCtx     context.Context
}

// Aborts the registered client through the agent and logs a failed abort at debug
func (r *directInlineRequest) abortClient(client any, reason, logMsg string) {
	if err := r.agent.AbortRequestOpenAIClient(client, reason); err != nil {
		nonStreamLog.Debug(logMsg, "exc_info", err)
	}
}

// Aborts from a watchdog or interrupt, true only for the call that owns the stale transition
func (r *directInlineRequest) abort(reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return false
	}
	newlyStale := false
	if reason == "stale_call_kill" {
		if r.cancelled {
			return false
		}
		newlyStale = !r.stale
		if newlyStale {
			r.stale = true
			bumpStaleStreak(r.staleCtx)
		}
	} else {
		r.cancelled = true
	}
	if r.client != nil {
		r.abortClient(r.client, reason, fmt.Sprintf("Inline request abort failed (%s)", reason))
	}
	return newlyStale
}

// Builds and registers the per request client, failing the dispatch when the timer already fired
func (r *directInlineRequest) makeClient(reason, kind string) (any, error) {
	client, err := r.agent.CreateRequestOpenAIClient(reason, r.apiKwargs)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.client = client
	staleBeforeDispatch := r.stale
	if staleBeforeDispatch {
		r.abortClient(client, "stale_call_kill", "Inline abort after late client registration failed")
	}
	r.mu.Unlock()
	if staleBeforeDispatch {
		return nil, &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Non-streaming API call timed out before request dispatch (threshold: %ds)", int(r.staleTimeout))}
	}
	r.agent.SetActiveRequestAbort(r.hook)
	return client, nil
}

func (r *directInlineRequest) markDone() {
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
}

// Takes the registered client out of the request so the caller closes it once
func (r *directInlineRequest) popClient() any {
	r.mu.Lock()
	defer r.mu.Unlock()
	client := r.client
	r.client = nil
	return client
}

func (r *directInlineRequest) wasStale() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stale
}

// Timer callback that reports and records the kill only when it owns the stale transition
func (r *directInlineRequest) onStale() {
	if !r.abort("stale_call_kill") {
		return
	}
	elapsed := time.Since(r.callStart).Seconds()
	model, _ := r.apiKwargs["model"].(string)
	reportStaleNonstreamKill(r.surface, model, EstimateRequestContextTokens(r.staleCtx, r.apiKwargs), elapsed, r.staleTimeout, true, "")
	touchStaleKillActivity(r.surface, elapsed)
}

// Status lines go to the diagnostic buffer, activity lines to the activity clock, wait notices to the agent notice
func agentRequestSurface(agent ChatCompletionAgent) func(StreamEvent) {
	return func(ev StreamEvent) {
		switch ev.Kind {
		case EventStatus:
			agent.BufferDiagnosticStatus(ev.Text)
		case EventActivity:
			agent.TouchActivity(ev.Text)
		case EventWaitNotice:
			agent.EmitWaitNotice(ev.Text)
		}
	}
}

// Stale watchdog timer seam the inline call tests swap the way hermes tests patch threading.Timer
var directWatchdogArmFn = time.AfterFunc

// Runs the agent's blocking call on the calling goroutine with a heartbeat, one shot stale kill and socket backstop
func DirectAPICall(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any) (resp any, err error) {
	if err := checkStaleGiveup(agent.RetrySession()); err != nil {
		return nil, err
	}
	agent.TouchActivity("waiting for non-streaming API response")
	callStart := time.Now()
	staleTimeout, err := ResolveDirectStaleTimeout(ctx, agent, apiKwargs)
	if err != nil {
		return nil, err
	}
	if hard := inlineNonstreamHardTimeout(staleTimeout); hard > 0 {
		if _, ok := apiKwargs["timeout"]; !ok {
			next := make(map[string]any, len(apiKwargs)+1)
			for k, v := range apiKwargs {
				next[k] = v
			}
			next["timeout"] = staleTimeout
			apiKwargs = next
		}
	}
	r := &directInlineRequest{agent: agent, apiKwargs: apiKwargs, staleTimeout: staleTimeout, callStart: callStart,
		surface: agentRequestSurface(agent), staleCtx: withStaleStreak(ctx, agent.RetrySession())}
	r.hook = &RequestAbortHook{Abort: r.abort}
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(directAPIActivityHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-t.C:
				func() {
					defer func() { _ = recover() }()
					agent.TouchActivity("waiting for non-streaming API response")
				}()
			}
		}
	}()
	var watchdog *time.Timer
	fired := make(chan struct{})
	if !math.IsInf(staleTimeout, 0) && !math.IsNaN(staleTimeout) && staleTimeout > 0 {
		watchdog = directWatchdogArmFn(secondsDuration(staleTimeout), func() {
			defer close(fired)
			r.onStale()
		})
	}
	succeeded := false
	defer func() {
		if watchdog != nil && !watchdog.Stop() {
			<-fired
		}
		r.markDone()
		close(hbStop)
		select {
		case <-hbDone:
		case <-time.After(2 * time.Second):
		}
		if agent.ActiveRequestAbort() == r.hook {
			agent.SetActiveRequestAbort(nil)
		}
		if client := r.popClient(); client != nil {
			reason := "request_error_cleanup"
			if succeeded {
				reason = "request_complete"
			}
			if cerr := agent.CloseRequestOpenAIClient(client, reason); cerr != nil {
				resp, err = nil, cerr
			}
		}
	}()
	resp, err = DispatchNonstreamingAPIRequest(ctx, agent, apiKwargs, r.makeClient)
	if err != nil {
		if agent.InterruptRequested() {
			return nil, &InterruptedError{Message: "Agent interrupted during API call"}
		}
		if r.wasStale() {
			return nil, &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Non-streaming API call timed out after %ds with no response (threshold: %ds)",
				int(time.Since(callStart).Seconds()), int(staleTimeout))}
		}
		return nil, err
	}
	if agent.InterruptRequested() {
		return nil, &InterruptedError{Message: "Agent interrupted during API call"}
	}
	r.markDone()
	agent.RetrySession().ResetStaleStreak()
	succeeded = true
	return resp, nil
}

// Codex silent hang hint of the agent for the request model, empty when the agent answers none or fails
func codexSilentHangHintFor(agent ChatCompletionAgent, apiKwargs map[string]any) (hint string) {
	defer func() {
		if recover() != nil {
			hint = ""
		}
	}()
	h, err := agent.CodexSilentHangHint(apiKwargs["model"])
	if err != nil {
		return ""
	}
	return h
}

// Runs the agent's blocking call inline for nested pool routes, else on a worker the interrupt and stale watchdogs poll
func InterruptibleAPICall(ctx context.Context, agent ChatCompletionAgent, apiKwargs map[string]any) (any, error) {
	if directAPICallFor(ctx, agent.Provider(), agent.Platform(), APIMode(agent.APIMode())) {
		return DirectAPICall(ctx, agent, apiKwargs)
	}
	if err := checkStaleGiveup(agent.RetrySession()); err != nil {
		return nil, err
	}
	return agentNonStreamRequest(ctx, agent, apiKwargs)
}

// Consumes the one shot answer without thinking flag of a length continuation
func consumeEphemeralReasoningOff(agent ChatCompletionAgent) bool {
	consumed := agent.EphemeralReasoningOff()
	if consumed {
		agent.SetEphemeralReasoningOff(false)
	}
	return consumed
}

// Agent reasoning config with the one shot off override and the route's rejection rules, recorded as what went out
func ReasoningConfigForWire(ctx context.Context, agent ChatCompletionAgent) map[string]any {
	cfg := agent.ReasoningConfig()
	ephemeralOff := consumeEphemeralReasoningOff(agent)
	if agent.ReasoningEffortRejected() {
		agent.SetWireReasoningConfig(nil)
		return nil
	}
	if cfg == nil {
		cfg = UnsetReasoningDefault(ctx, agent)
	}
	if agent.ReasoningDisableRejected() {
		if cfg != nil && (cfg["enabled"] == false || cfg["effort"] == "none") {
			if agent.ReasoningFloorRequired() {
				floored := copyDict(cfg)
				floored["enabled"] = true
				floored["effort"] = ReasoningFloorEffort
				agent.SetWireReasoningConfig(floored)
				return floored
			}
			agent.SetWireReasoningConfig(nil)
			return nil
		}
		agent.SetWireReasoningConfig(cfg)
		return cfg
	}
	if ephemeralOff {
		next := map[string]any{}
		for k, v := range cfg {
			next[k] = v
		}
		next["enabled"] = false
		next["effort"] = "none"
		cfg = next
	}
	agent.SetWireReasoningConfig(cfg)
	return cfg
}

// Model capability seam the unset effort tests swap the way hermes tests patch get_model_capabilities
var modelsDevCapabilitiesFn = ModelsDevCapabilities

// Profile default reasoning for an unset effort on chat completions, nil when the model or local server cannot reason
func UnsetReasoningDefault(ctx context.Context, agent ChatCompletionAgent) (out map[string]any) {
	if agent.APIMode() != string(ModeChatCompletions) {
		return nil
	}
	provider, model := agent.Provider(), agent.Model()
	var def map[string]any
	func() {
		defer func() {
			if recover() != nil {
				def = nil
			}
		}()
		if profile, ok := GetProviderProfile(ctx, provider); ok {
			def = reasoningConfigMap(profile.DefaultReasoningConfig(model))
		}
	}()
	if len(def) == 0 {
		return nil
	}
	var supports *bool
	func() {
		defer func() { _ = recover() }()
		if caps, ok := modelsDevCapabilitiesFn(ctx, provider, model, false); ok {
			supports = caps.SupportsReasoning
		}
	}()
	if supports != nil && !*supports {
		return nil
	}
	if agent.OllamaNumCtx() != 0 && !agent.OllamaSupportsThinkingCached() {
		return nil
	}
	return copyDict(def)
}

// Swaps a pool of another provider for the fallback provider's own pool when it holds credentials
func RebindFallbackCredentialPool(agent ChatCompletionAgent, fbProvider, fbModel string) {
	if existing := agent.CredentialPool(); existing != nil {
		poolProvider := strings.ToLower(strings.TrimSpace(existing.Provider()))
		if poolProvider != "" && poolProvider != fbProvider {
			nonStreamLog.Info(fmt.Sprintf("Fallback to %s/%s: clearing primary credential pool (pool_provider=%s) to prevent cross-provider contamination", fbProvider, fbModel, poolProvider))
			agent.ClearCredentialPool()
		}
	}
	if agent.CredentialPool() != nil {
		return
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				nonStreamLog.Debug(fmt.Sprintf("Fallback to %s/%s: could not attach credential pool: %v", fbProvider, fbModel, rec))
			}
		}()
		if pool := LoadPool(agent.CredentialPoolEnv(), fbProvider); pool != nil && pool.HasCredentials() {
			agent.SetCredentialPool(pool)
			nonStreamLog.Info(fmt.Sprintf("Fallback to %s/%s: attached fallback credential pool", fbProvider, fbModel))
		}
	}()
}

// Per turn VM and browser cleanup that leaves persistent terminals and headed browsers to their idle reapers
func CleanupTaskResources(agent ChatCompletionAgent, taskID string) {
	type step struct {
		label, skipWhat string
		skip            func() (bool, error)
		cleanup         func() error
	}
	steps := []step{
		{"VM", "cleanup_vm for persistent env", func() (bool, error) { return tools.IsPersistentEnv(agent.ToolEnv(), taskID) },
			func() error { tools.CleanupVM(agent.ToolEnv(), taskID, false); return nil }},
		{"browser", "cleanup_browser for headed session", agent.IsHeadedMode, func() error { return agent.CleanupBrowser(taskID) }},
	}
	for _, s := range steps {
		err := func() (err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			skip, err := s.skip()
			if err != nil {
				return err
			}
			if skip {
				if agent.VerboseLogging() {
					slog.Debug(fmt.Sprintf("Skipping per-turn %s %s; idle reaper will handle it.", s.skipWhat, taskID))
				}
				return nil
			}
			return s.cleanup()
		}()
		if err != nil && agent.VerboseLogging() {
			nonStreamLog.Warn(fmt.Sprintf("Failed to cleanup %s for task %s: %s", s.label, taskID, err))
		}
	}
}

// Merges the nous profile extra_body into Messages wire kwargs of a nous agent, never provider_preferences
func MergeNousPortalMessagesExtraBody(ctx context.Context, agent ChatCompletionAgent, anthropicKwargs map[string]any, apiMessages []map[string]any) map[string]any {
	switch agent.Provider() {
	case "nous", "nous-portal", "nousresearch":
	default:
		return anthropicKwargs
	}
	func() {
		defer func() {
			if p := recover(); p != nil {
				nonStreamLog.Debug(fmt.Sprintf("Nous Portal extra_body merge failed: %v", p))
			}
		}()
		profile, ok := GetProviderProfile(ctx, "nous")
		if !ok {
			return
		}
		existing, present := anthropicKwargs["extra_body"]
		extra, isMap := existing.(map[string]any)
		if present && !isMap {
			nonStreamLog.Debug(fmt.Sprintf("Nous Portal extra_body merge failed: '%s' object has no attribute 'update'", pyrepr.TypeName(existing)))
			return
		}
		if !present {
			extra = map[string]any{}
			anthropicKwargs["extra_body"] = extra
		}
		body := profile.BuildExtraBody(agent.SessionID(), map[string]any{hookCtxKey: ctx, "data_use": state.DataUseRequirement(apiMessages)})
		for k, v := range body {
			extra[k] = v
		}
	}()
	return anthropicKwargs
}
