package agent

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var _ provider.ChatCompletionAgent = (*AIAgent)(nil)

// Seconds an ollama thinking probe answer without a definitive value stays cached
const ollamaThinkingProbeTTL = 60 * time.Second

// Request state the agent shaped chat completion helpers read and write
type chatCompletionAgentState struct {
	retrySessionOnce              sync.Once
	retrySession                  *provider.RetrySession
	activeRequestAbort            atomic.Pointer[provider.RequestAbortHook]
	requestStateMu                sync.Mutex
	activeCodexStreamRequestToken any
	codexStreamedTextParts        []string
	codexOnFirstDelta             func()
	wireReasoningConfig           map[string]any
	reasoningEffortRejected       bool
	reasoningDisableRejected      bool
	reasoningFloorRequired        bool
	ollamaThinkingCache           map[[2]string]ollamaThinkingProbe
}

type ollamaThinkingProbe struct {
	supported, known bool
	at               time.Time
}

// Retry session holding the stream disable flags and the cross turn stale stream streak
func (a *AIAgent) RetrySession() *provider.RetrySession {
	a.retrySessionOnce.Do(func() {
		if a.retrySession == nil {
			a.retrySession = &provider.RetrySession{}
		}
	})
	return a.retrySession
}

func (a *AIAgent) Client() any { return a.client }

func (a *AIAgent) ComputeNonStreamStaleTimeout(apiKwargs map[string]any) (any, error) {
	return a.computeNonStreamStaleTimeout(a.homeContext(), apiKwargs), nil
}

func (a *AIAgent) ActiveRequestAbort() *provider.RequestAbortHook { return a.activeRequestAbort.Load() }

func (a *AIAgent) SetActiveRequestAbort(hook *provider.RequestAbortHook) {
	a.activeRequestAbort.Store(hook)
}

// Runs the published in flight request abort, false when none is registered
func (a *AIAgent) abortActiveRequest(reason string) bool {
	hook := a.activeRequestAbort.Load()
	if hook == nil || hook.Abort == nil {
		return false
	}
	return hook.Abort(reason)
}

func (a *AIAgent) RunCodexStream(ctx context.Context, apiKwargs map[string]any, client any, onFirstDelta func()) (any, error) {
	return provider.RunCodexStream(ctx, a, apiKwargs, client, onFirstDelta)
}

func (a *AIAgent) CodexOnFirstDelta() func() {
	a.requestStateMu.Lock()
	defer a.requestStateMu.Unlock()
	return a.codexOnFirstDelta
}

func (a *AIAgent) SetCodexOnFirstDelta(fn func()) {
	a.requestStateMu.Lock()
	defer a.requestStateMu.Unlock()
	a.codexOnFirstDelta = fn
}

// Messages create over the request local client or the shared one, refreshing anthropic credentials first
func (a *AIAgent) AnthropicMessagesCreate(ctx context.Context, apiKwargs map[string]any, client any) (any, error) {
	if client == nil && a.apiMode == "anthropic_messages" {
		if _, err := a.tryRefreshAnthropicClientCredentials(ctx); err != nil {
			return nil, err
		}
	}
	if client == nil {
		client = a.anthropicClient
	}
	return provider.CreateAnthropicMessage(ctx, client, apiKwargs, a.logPrefix, !a.RetrySession().DisableStreaming, a.captureAnthropicResponseHeaders)
}

func (a *AIAgent) ChatCompletionCapturingHeaders(ctx context.Context, client any, apiKwargs map[string]any) (any, error) {
	return provider.ChatCompletionCapturingHeaders(ctx, a, client, apiKwargs)
}

func (a *AIAgent) CodexSilentHangHint(model any) (string, error) {
	switch m := model.(type) {
	case nil:
		return a.codexSilentHangHint(nil), nil
	case string:
		return a.codexSilentHangHint(&m), nil
	default:
		s := pyrepr.Str(m)
		return a.codexSilentHangHint(&s), nil
	}
}

func (a *AIAgent) EphemeralReasoningOff() bool      { return a.ephemeralReasoningOff }
func (a *AIAgent) SetEphemeralReasoningOff(on bool) { a.ephemeralReasoningOff = on }
func (a *AIAgent) ReasoningEffortRejected() bool    { return a.reasoningEffortRejected }
func (a *AIAgent) ReasoningDisableRejected() bool   { return a.reasoningDisableRejected }
func (a *AIAgent) ReasoningFloorRequired() bool     { return a.reasoningFloorRequired }
func (a *AIAgent) SetWireReasoningConfig(cfg map[string]any) {
	a.wireReasoningConfig = cfg
}
func (a *AIAgent) OllamaNumCtx() int { return a.ollamaNumCtx }

// Ollama thinking capability per model and base url, unknown answers rechecked after a minute
func (a *AIAgent) OllamaSupportsThinkingCached() bool {
	key := [2]string{a.model, a.baseURL}
	a.requestStateMu.Lock()
	cached, ok := a.ollamaThinkingCache[key]
	a.requestStateMu.Unlock()
	if ok && (cached.known || time.Since(cached.at) < ollamaThinkingProbeTTL) {
		return cached.supported
	}
	supported, known := provider.OllamaModelSupportsThinking(a.homeContext(), a.model, a.baseURL, a.apiKey, 5*time.Second)
	a.requestStateMu.Lock()
	if a.ollamaThinkingCache == nil {
		a.ollamaThinkingCache = map[[2]string]ollamaThinkingProbe{}
	}
	a.ollamaThinkingCache[key] = ollamaThinkingProbe{supported: supported && known, known: known, at: time.Now()}
	a.requestStateMu.Unlock()
	return supported && known
}

func (a *AIAgent) CredentialPool() provider.PoolProbe {
	if a.credentialPool == nil {
		return nil
	}
	return a.credentialPool
}

func (a *AIAgent) SetCredentialPool(pool provider.PoolProbe) {
	if pool == nil {
		a.credentialPool = nil
		return
	}
	a.credentialPool = pool.(*provider.Pool)
}

func (a *AIAgent) ClearCredentialPool() {
	a.credentialPool = nil
	a.credentialPoolEntryID = ""
}

func (a *AIAgent) CredentialPoolEnv() provider.PoolEnv { return agentPoolEnv(a.homeContext()) }

func (a *AIAgent) IsHeadedMode() (bool, error) { return tools.IsHeadedMode(a.env), nil }

func (a *AIAgent) CleanupBrowser(ctx context.Context, taskID string) error {
	return tools.CleanupBrowser(tools.WithEnvHome(ctx, a.env), a.env, taskID)
}

func (a *AIAgent) ActiveCodexStreamRequestToken() any {
	a.requestStateMu.Lock()
	defer a.requestStateMu.Unlock()
	return a.activeCodexStreamRequestToken
}

func (a *AIAgent) SetActiveCodexStreamRequestToken(token any) {
	a.requestStateMu.Lock()
	defer a.requestStateMu.Unlock()
	a.activeCodexStreamRequestToken = token
}

func (a *AIAgent) MaxIterations() int         { return a.maxIterations }
func (a *AIAgent) SuppressStatusOutput() bool { return a.suppressStatusOutput }

func (a *AIAgent) ShouldSanitizeToolCalls() bool { return a.shouldSanitizeToolCalls() }

func (a *AIAgent) CopyReasoningContentForAPI(src, dst map[string]any) {
	a.copyReasoningContentForAPI(state.MessageFromDict(src), dst)
}

func (a *AIAgent) SanitizeToolCallsForStrictAPI(msg map[string]any, model string) {
	sanitizeToolCallsForStrictAPI(msg, model, a.baseURL, a.provider)
}

// Copies of the configured prefill rows in order
func (a *AIAgent) PrefillMessages() []map[string]any {
	out := make([]map[string]any, 0, len(a.prefillMessages))
	for _, raw := range a.prefillMessages {
		out = append(out, raw.(map[string]any))
	}
	return out
}

func (a *AIAgent) SanitizeAPIMessages(msgs []map[string]any) []map[string]any {
	return sanitizeAPIMessages(a.homeContext(), a.sessionID, msgs)
}

func (a *AIAgent) EvictStaleOutboundToolImages(msgs []map[string]any) int {
	return EvictStaleOutboundToolImages(msgs, outboundRetiredImageIDs(a))
}

// Strips image parts for a model that rejected them, writing the rewritten rows back in place
func (a *AIAgent) StripImagesForRejectingModel(msgs []map[string]any) bool {
	rows := make([]any, len(msgs))
	for i, m := range msgs {
		rows[i] = m
	}
	changed := a.stripImagesForRejectingModel(&rows)
	for i := range msgs {
		if i < len(rows) {
			msgs[i] = rows[i].(map[string]any)
		}
	}
	return changed
}

func (a *AIAgent) DropThinkingOnlyAndMergeUsers(msgs []map[string]any) []map[string]any {
	return dropThinkingOnlyAndMergeUsers(msgs, true, nil)
}

func (a *AIAgent) SubstituteAPIContent(msg map[string]any) { substituteAPIContent(msg) }

func (a *AIAgent) CanonicalizeAPIToolCalls(msgs []map[string]any) {
	rows := make([]any, len(msgs))
	for i, m := range msgs {
		rows[i] = m
	}
	canonicalizeAPIToolCalls(rows)
}

func (a *AIAgent) CloneMessageForSend(msg map[string]any) map[string]any {
	return cloneMessageForSend(msg).(map[string]any)
}

func (a *AIAgent) BuildAPIKwargs(apiMessages []map[string]any) (map[string]any, error) {
	return a.buildAPIKwargs(apiMessages, nil, false)
}

func (a *AIAgent) SanitizeOutboundKwargs(kwargs map[string]any) { a.sanitizeOutboundKwargs(kwargs) }

func (a *AIAgent) CurrentStreamedAssistantText() string { return a.currentStreamedAssistantText() }
func (a *AIAgent) ResetStreamDeliveryTracking()         { a.resetStreamDeliveryTracking() }
func (a *AIAgent) WarningPresentationEnabled() bool     { return a.warningPresentationEnabled() }
func (a *AIAgent) SetLastAPIFirstChunkAt(at float64)    { a.lastAPIFirstChunkAt = &at }
func (a *AIAgent) LastAPIFirstChunkAt() *float64        { return a.lastAPIFirstChunkAt }
func (a *AIAgent) HasInterimAssistantCallback() bool    { return a.interimAssistantCallback != nil }

// Starts an empty streamed text list for the next Codex Responses request
func (a *AIAgent) ResetCodexStreamedTextParts() {
	a.requestStateMu.Lock()
	defer a.requestStateMu.Unlock()
	a.codexStreamedTextParts = []string{}
}

func (a *AIAgent) AppendCodexStreamedTextPart(text string) {
	a.requestStateMu.Lock()
	defer a.requestStateMu.Unlock()
	a.codexStreamedTextParts = append(a.codexStreamedTextParts, text)
}

// Copy of the text parts the current Codex Responses request streamed
func (a *AIAgent) CodexStreamedTextParts() []string {
	a.requestStateMu.Lock()
	defer a.requestStateMu.Unlock()
	return append([]string(nil), a.codexStreamedTextParts...)
}

// Hands tool call turn text to the display delta callback for reasoning tag extraction unless a plugin holds the reply
func (a *AIAgent) RouteSuppressedStreamText(text string) {
	if a.streamDeltaCallback == nil || a.holdStream {
		return
	}
	if callQuietly(a.streamDeltaCallback, text) {
		a.recordStreamedAssistantText(text)
	}
}

func (a *AIAgent) InterruptibleAPICall(ctx context.Context, apiKwargs map[string]any) (any, error) {
	return a.interruptibleAPICall(ctx, apiKwargs)
}

// Blocking provider call that an interrupt or the stale stream breaker can abandon
func (a *AIAgent) interruptibleAPICall(ctx context.Context, kwargs map[string]any) (any, error) {
	return provider.InterruptibleAPICall(ctx, a, kwargs)
}

// Streaming provider call firing the delta callbacks, the non streaming response shape on return
func (a *AIAgent) interruptibleStreamingAPICall(ctx context.Context, kwargs map[string]any, onFirstDelta func()) (any, error) {
	return provider.InterruptibleStreamingAPICall(ctx, a, kwargs, onFirstDelta)
}

// Per turn VM and browser cleanup leaving persistent terminals and headed browsers to the idle reapers
func (a *AIAgent) cleanupTaskResources(ctx context.Context, taskID string) {
	provider.CleanupTaskResources(tools.WithEnvHome(ctx, a.env), a, taskID)
}

func (a *AIAgent) Transport() provider.Transport { return a.getTransport("") }

// Configured output cap, nil when unset
func (a *AIAgent) MaxTokens() *int {
	if a.maxTokens == nil {
		return nil
	}
	n := *a.maxTokens
	return &n
}

// Configured output cap read the Python max_tokens or 0 way
func (a *AIAgent) maxTokensOrZero() int {
	if a.maxTokens == nil {
		return 0
	}
	return *a.maxTokens
}

func (a *AIAgent) IsAnthropicOAuth() bool         { return a.isAnthropicOAuth }
func (a *AIAgent) AnthropicPreserveDots() bool    { return a.anthropicPreserveDots() }
func (a *AIAgent) AnthropicBaseURL() string       { return a.anthropicBaseURL }
func (a *AIAgent) EmitStreamStart()               { a.enqueueStreamHook("on_stream_start", nil) }
func (a *AIAgent) FireStreamDelta(text string)    { a.fireStreamDelta(text) }
func (a *AIAgent) FireReasoningDelta(text string) { a.fireReasoningDelta(text) }

func (a *AIAgent) EmitStreamEnd(finalText string, finished bool, errText *string) {
	var errValue any
	if errText != nil {
		errValue = *errText
	}
	a.enqueueStreamHook("on_stream_end", map[string]any{"final_text": finalText, "finished": finished, "error": errValue})
}

// Tells the display layer the model is generating tool call arguments, a failing callback swallowed
func (a *AIAgent) FireToolGenStarted(name string) {
	fn, ok := a.toolGenCallback.(func(string))
	if !ok || fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("tool_gen_callback failed", "error", r)
		}
	}()
	fn(name)
}

func (a *AIAgent) HasStreamConsumers() bool { return a.hasStreamConsumers() }

// True when a plugin stream observer, the stream delta callback or the stream callback is registered
func (a *AIAgent) hasStreamConsumers() bool {
	if plugins.HasStreamObserverHooks(a.homeContext()) {
		return true
	}
	return a.streamDeltaCallback != nil || a.streamCallback != nil
}

func (a *AIAgent) WantsReasoningDeltas() bool {
	return a.reasoningCallback != nil || a.streamDeltaCallback != nil
}

func (a *AIAgent) DisableStreaming() { a.RetrySession().DisableStreaming = true }

func (a *AIAgent) EmitWarning(text string) { a.emitWarning(text) }

func (a *AIAgent) BufferStatus(text string)    { a.bufferStatus(text) }
func (a *AIAgent) CurrentAPIRequestID() string { return a.currentAPIRequestID }
func (a *AIAgent) IsSubagent() bool            { return a.isSubagent }
func (a *AIAgent) FallbackIndex() int          { return a.fallbackIndex }

// Releases the task's processes, VM, browser, computer use session and file read state, each step quietly
func (a *AIAgent) closeTaskResources(taskID string) {
	ctx := a.homeContext()
	killProcesses := func() error {
		reg := tools.DefaultProcessRegistry()
		for _, process := range reg.ListSessions(tools.WithEnvHome(ctx, a.env), a.env, "", "", false) {
			owner, _ := process["owner_task_id"].(string)
			if !a.processOwnerTaskIDs[owner] || process["status"] != "running" {
				continue
			}
			if persist, _ := process["persist_on_release"].(bool); persist {
				continue
			}
			sessionID, _ := process["session_id"].(string)
			reg.KillProcess(tools.WithEnvHome(ctx, a.env), a.env, sessionID, "agent_close", true)
		}
		return nil
	}
	forgetFileState := func() error {
		for owner := range a.processOwnerTaskIDs {
			if owner != "" && owner != taskID {
				tools.ClearFileOpsCache(owner)
			}
		}
		return nil
	}
	steps := []func() error{
		killProcesses,
		func() error { tools.CleanupVM(a.env, taskID, false); return nil },
		func() error { return tools.CleanupBrowser(tools.WithEnvHome(ctx, a.env), a.env, taskID) },
		func() error { tools.ReleaseComputerUseSession(ctx, taskID); return nil },
		forgetFileState,
	}
	for _, step := range steps {
		quietly(step)
	}
}
