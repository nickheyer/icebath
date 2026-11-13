package agent

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

const voiceChatTask = "voice_chat"

func voiceTurnRouteLog() *slog.Logger {
	return slog.Default().With("logger", "agent.voice_turn_route")
}

// Agent fields a voice route swap saves beside the primary runtime snapshot
type voiceRouteExtras struct {
	fallbackActivated        bool
	fallbackIndex            int
	providerFallbackActive   bool
	providerFallbackRoute    [2]string
	rateLimitBackoffCount    int
	credentialPool           *provider.Pool
	credentialPoolEntryID    string
	configContextLength      int
	reasoningDisableRejected bool
	reasoningFloorRequired   bool
	reasoningEffortRejected  bool
}

// Session route a live voice turn replaced, snapshot nil when the turn changed only the effort
type voiceRouteState struct {
	reasoningConfig map[string]any
	snapshot        map[string]any
	extras          voiceRouteExtras
}

// Voice turn marker a surface sets, the live route state and the once per agent warning flag
type voiceTurnRouteFields struct {
	voiceTurnPending bool
	voiceRouteState  *voiceRouteState
	warnedVoiceRoute bool
}

// SetVoiceTurnPending marks the next turn as spoken so it runs on the auxiliary.voice_chat route
func (a *AIAgent) SetVoiceTurnPending(pending bool) { a.voiceTurnPending = pending }

// Entry the voice turn binds, nil when the slot follows the main model
func voiceRouteTarget(ctx context.Context, a *AIAgent, cfg map[string]any) map[string]any {
	providerName := pystr.Lower(pystr.Strip(pyrepr.Str(orEmpty(cfg["provider"]))))
	model := pystr.Strip(pyrepr.Str(orEmpty(cfg["model"])))
	baseURL := pystr.Strip(pyrepr.Str(orEmpty(cfg["base_url"])))
	switch providerName {
	case "", "auto", "main":
		switch {
		case baseURL != "":
			providerName = "custom"
		case model != "":
			providerName = pystr.Lower(pystr.Strip(a.provider))
		default:
			return nil
		}
	}
	if model == "" {
		model = provider.AuxModelForProvider(ctx, providerName, true)
	}
	if providerName == "" || model == "" {
		return nil
	}
	if baseURL == "" && providerName == pystr.Lower(a.provider) && model == a.model {
		return nil
	}
	providerName, baseURL = provider.ExpandDirectAPIAlias(ctx, providerName, baseURL)
	target := make(map[string]any, len(cfg)+3)
	maps.Copy(target, cfg)
	target["provider"], target["model"], target["base_url"] = providerName, model, baseURL
	return target
}

// Whether the request stays under the bound route's compression threshold, always true without one
func voiceRouteFits(ctx context.Context, a *AIAgent, messages []*state.Message, systemPrompt string) bool {
	limit := 0
	if a.contextCompressor != nil {
		limit = a.contextCompressor.Base().ThresholdTokens
	}
	if limit == 0 {
		return true
	}
	return provider.EstimateRequestTokens(ctx, messages, systemPrompt, agentToolMaps(a), true) < limit
}

// Weakest enabled effort the active route accepts when it cannot switch reasoning off, empty when off is sendable
func voiceLowestEffort(ctx context.Context, a *AIAgent) (string, error) {
	off := map[string]any{"enabled": false}
	floor, err := provider.KnownReasoningFloor(ctx, off, a.provider, a.baseURL, a.model, voiceChatTask)
	if err != nil {
		return "", err
	}
	if same, ok := floor.(map[string]any); !ok || reflect.ValueOf(same).UnsafePointer() != reflect.ValueOf(off).UnsafePointer() {
		return provider.ReasoningFloorEffort, nil
	}
	switch a.apiMode {
	case "codex_responses":
		route := provider.ClassifyResponsesRoute(a.provider, a.baseURL)
		effort, _ := provider.ResolveResponsesReasoning(ctx, a.model, a.provider, a.baseURL, route.IsCodexBackend(), route.IsXAI(), false, "none")
		if effort == "" || effort == "none" {
			return "", nil
		}
		return effort, nil
	case "anthropic_messages":
		if provider.IsMandatoryThinkingClaude(a.model) {
			return provider.ReasoningFloorEffort, nil
		}
	}
	return "", nil
}

// Effort for the active route with reasoning off lifted to the lowest valid level where off is not an option
func voiceReasoning(ctx context.Context, a *AIAgent, effort map[string]any) (map[string]any, error) {
	if enabled, isBool := effort["enabled"].(bool); !isBool || enabled {
		return effort, nil
	}
	lowest, err := voiceLowestEffort(ctx, a)
	if err != nil {
		return nil, err
	}
	if lowest != "" {
		return map[string]any{"enabled": true, "effort": lowest}, nil
	}
	return effort, nil
}

// Primary runtime snapshot of the session route plus the fallback, pool and rejection fields a swap touches
func captureVoiceRoute(ctx context.Context, a *AIAgent) (map[string]any, voiceRouteExtras) {
	return buildPrimaryRuntimeSnapshot(ctx, a, a.apiMode), voiceRouteExtras{
		fallbackActivated: a.fallbackActivated, fallbackIndex: a.fallbackIndex,
		providerFallbackActive: a.providerFallbackActive, providerFallbackRoute: a.providerFallbackRoute,
		rateLimitBackoffCount: a.rateLimitBackoffCount, credentialPool: a.credentialPool,
		credentialPoolEntryID: a.credentialPoolEntryID, configContextLength: a.configContextLength,
		reasoningDisableRejected: a.reasoningDisableRejected, reasoningFloorRequired: a.reasoningFloorRequired,
		reasoningEffortRejected: a.reasoningEffortRejected,
	}
}

// Puts the session route back from the captured snapshot and fields
func reinstallVoiceRoute(ctx context.Context, a *AIAgent, st *voiceRouteState) error {
	if err := reinstallRuntimeSnapshot(ctx, a, st.snapshot); err != nil {
		return err
	}
	x := st.extras
	a.fallbackActivated, a.fallbackIndex = x.fallbackActivated, x.fallbackIndex
	a.providerFallbackActive, a.providerFallbackRoute = x.providerFallbackActive, x.providerFallbackRoute
	a.rateLimitBackoffCount, a.credentialPool, a.credentialPoolEntryID = x.rateLimitBackoffCount, x.credentialPool, x.credentialPoolEntryID
	a.configContextLength = x.configContextLength
	a.reasoningDisableRejected, a.reasoningFloorRequired, a.reasoningEffortRejected = x.reasoningDisableRejected, x.reasoningFloorRequired, x.reasoningEffortRejected
	return nil
}

// Logs every unused voice route and shows the user one notice per agent
func warnVoiceRouteOnce(a *AIAgent, target map[string]any, reason string) {
	voiceTurnRouteLog().Warn(fmt.Sprintf("auxiliary.%s route %s/%s not used: %s; the voice turn runs on %s/%s",
		voiceChatTask, pyrepr.Str(target["provider"]), pyrepr.Str(target["model"]), reason, a.provider, a.model))
	if a.warnedVoiceRoute {
		return
	}
	a.warnedVoiceRoute = true
	a.emitWarning(fmt.Sprintf("⚠ Voice chat model %s via %s unavailable (%s); using %s.",
		pyrepr.Str(target["model"]), pyrepr.Str(target["provider"]), reason, a.model))
}

// Binds the voice entry, the reason it was not used or empty when the agent now runs on it
func bindVoiceRoute(ctx context.Context, a *AIAgent, target map[string]any) (reason string) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			reason = voiceBindFailureReason(fmt.Sprint(r), "Exception")
		}
	}()
	_, _, bound, err := a.bindRouteEntry(ctx, target, pyrepr.Str(target["provider"]), pyrepr.Str(target["model"]))
	if err != nil {
		return voiceBindFailureReason(err.Error(), surfaceExceptionName(err))
	}
	if !bound {
		return "provider not configured"
	}
	return ""
}

// First line of a bind failure, the exception class name when the message is empty
func voiceBindFailureReason(message, className string) string {
	if message == "" {
		return className
	}
	return pystr.Splitlines(message, false)[0]
}

// Binds the voice route of a marked turn and returns the system prompt the turn sends
func beginVoiceTurnRoute(ctx context.Context, a *AIAgent, messages []*state.Message, systemPrompt string) (string, error) {
	if !a.voiceTurnPending {
		return systemPrompt, nil
	}
	a.voiceTurnPending = false
	cfg, err := provider.AuxiliaryTaskConfig(ctx, voiceChatTask)
	if err != nil {
		return "", err
	}
	effort := config.ParseReasoningEffort(cfg["reasoning_effort"])
	target := voiceRouteTarget(ctx, a, cfg)
	if target == nil && effort == nil {
		return systemPrompt, nil
	}
	st := &voiceRouteState{reasoningConfig: pycopy.Of(a.reasoningConfig)}
	a.voiceRouteState = st
	if target != nil {
		st.snapshot, st.extras = captureVoiceRoute(ctx, a)
		reason := bindVoiceRoute(ctx, a, target)
		if reason == "" && !voiceRouteFits(ctx, a, messages, systemPrompt) {
			reason = "the conversation is larger than its context window"
		}
		if reason != "" {
			if err := reinstallVoiceRoute(ctx, a, st); err != nil {
				return "", err
			}
			warnVoiceRouteOnce(a, target, reason)
			a.voiceRouteState = &voiceRouteState{reasoningConfig: st.reasoningConfig}
			return voiceTurnSystemPrompt(a, systemPrompt), nil
		}
		a.rewritePromptModelIdentity(a.model, a.provider)
		a.reasoningDisableRejected, a.reasoningFloorRequired, a.reasoningEffortRejected = false, false, false
		a.turnRouteTask = voiceChatTask
		voiceTurnRouteLog().Info(fmt.Sprintf("Voice turn routed to %s (%s)", a.model, a.provider))
	}
	if effort != nil {
		reasoning, err := voiceReasoning(ctx, a, effort)
		if err != nil {
			return "", err
		}
		a.reasoningConfig = reasoning
	}
	return voiceTurnSystemPrompt(a, systemPrompt), nil
}

// Cached prompt carrying the bound identity, the turn's own prompt when nothing is cached
func voiceTurnSystemPrompt(a *AIAgent, systemPrompt string) string {
	if a.cachedSystemPrompt != "" {
		return a.cachedSystemPrompt
	}
	return systemPrompt
}

// SessionRuntime is the model route and effort a surface reports or persists as the session's own
type SessionRuntime interface {
	Model() string
	Provider() string
	RequestedProvider() string
	BaseURL() string
	APIMode() string
	AnthropicBaseURL() string
	ReasoningConfig() map[string]any
}

// Agent with the session's own route and effort in place of a live voice turn's
type sessionRuntimeView struct {
	*AIAgent
	own map[string]any
}

// Session value of a route field, the agent's live value when the snapshot lacks it
func (v *sessionRuntimeView) ownString(name, live string) string {
	if value, ok := v.own[name]; ok {
		text, _ := value.(string)
		return text
	}
	return live
}

func (v *sessionRuntimeView) Model() string    { return v.ownString("model", v.AIAgent.Model()) }
func (v *sessionRuntimeView) Provider() string { return v.ownString("provider", v.AIAgent.Provider()) }
func (v *sessionRuntimeView) BaseURL() string  { return v.ownString("base_url", v.AIAgent.BaseURL()) }
func (v *sessionRuntimeView) APIMode() string  { return v.ownString("api_mode", v.AIAgent.APIMode()) }

func (v *sessionRuntimeView) RequestedProvider() string {
	return v.ownString("requested_provider", v.AIAgent.RequestedProvider())
}

func (v *sessionRuntimeView) AnthropicBaseURL() string {
	return v.ownString("_anthropic_base_url", v.AIAgent.AnthropicBaseURL())
}

func (v *sessionRuntimeView) ReasoningConfig() map[string]any {
	reasoning, _ := v.own["reasoning_config"].(map[string]any)
	return reasoning
}

// SessionRuntimeView is what a surface reports or persists as the session's model and effort, mid voice turn included
func SessionRuntimeView(a *AIAgent) SessionRuntime {
	if a == nil {
		return nil
	}
	st := a.voiceRouteState
	if st == nil {
		return a
	}
	return newSessionRuntimeView(a, st)
}

// View of an agent mid voice turn holding the route fields its snapshot captured and the session's own effort
func newSessionRuntimeView(a *AIAgent, st *voiceRouteState) *sessionRuntimeView {
	own := map[string]any{}
	for _, name := range []string{"model", "provider", "requested_provider", "base_url", "api_mode"} {
		if value, ok := st.snapshot[name]; ok {
			own[name] = value
		}
	}
	if value, ok := st.snapshot["anthropic_base_url"]; ok {
		own["_anthropic_base_url"] = value
	}
	own["reasoning_config"] = st.reasoningConfig
	return &sessionRuntimeView{AIAgent: a, own: own}
}

// Undoes the voice route of the turn, idempotent because every turn exit calls it
func endVoiceTurnRoute(ctx context.Context, a *AIAgent) {
	st := a.voiceRouteState
	if st == nil {
		return
	}
	a.voiceRouteState = nil
	a.turnRouteTask = ""
	if err := restoreVoiceRoute(ctx, a, st); err != nil {
		voiceTurnRouteLog().Warn("Voice turn route restore failed; the next turn re-resolves the main runtime", "exc_info", err.Error())
		a.fallbackActivated = true
	}
	a.reasoningConfig = st.reasoningConfig
}

// Remembers a reasoning floor the voice route learned and reinstalls the session route
func restoreVoiceRoute(ctx context.Context, a *AIAgent, st *voiceRouteState) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	if st.snapshot == nil {
		return nil
	}
	if a.reasoningFloorRequired {
		if err := provider.RememberReasoningFloor(a.provider, a.baseURL, map[string]any{"model": a.model}, nil); err != nil {
			return err
		}
	}
	return reinstallVoiceRoute(ctx, a, st)
}
