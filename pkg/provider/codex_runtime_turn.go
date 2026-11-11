package provider

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CodexRuntimeTurnHooks are the agent turn helpers the runtime reaches through the agent like a getattr
type CodexRuntimeTurnHooks interface {
	ApplyLLMOutputTransform(finalResponse string, turnID any) (string, bool, any)
	EffectiveRequestOverrides() map[string]any
}

// Turn helpers of the agent, an error naming the agent type when it does not provide them
func codexTurnHooks(agent CodexRuntimeAgent) (CodexRuntimeTurnHooks, error) {
	hooks, ok := agent.(CodexRuntimeTurnHooks)
	if !ok {
		return nil, fmt.Errorf("codex runtime agent %T has no ApplyLLMOutputTransform and EffectiveRequestOverrides", agent)
	}
	return hooks, nil
}

// Home the agent's tools run against, the process home without a tool env
func codexAgentHome(agent CodexRuntimeAgent) string {
	if env := agent.ToolEnv(); env != nil && env.Home != "" {
		return env.Home
	}
	return home.Dir(context.Background())
}

// Codex model_providers id of a named custom provider, nil for codex's own provider
func codexModelProvider(agent CodexRuntimeAgent) *string {
	if strings.ToLower(strings.TrimSpace(agent.Provider())) != "custom" {
		return nil
	}
	id := modelsResolver(home.WithOverride(context.Background(), codexAgentHome(agent))).CodexModelProviderID(agent.RequestedProvider())
	if id == "" {
		return nil
	}
	return &id
}

// Model slug codex runs, the picker window suffix dropped and bare on codex's own provider
func codexWireModel(agent CodexRuntimeAgent, modelProvider *string) string {
	model := StripCodexContextVariantSuffix(agent.Model())
	if model != "" && modelProvider == nil {
		model = NormalizeModelForProvider(model, "openai-codex")
	}
	return model
}

// Effort levels the route's entry clamp accepts, ultra added on the app server where max is reachable
func routeSupportedEfforts(provider, model, apiMode string) []string {
	if strings.ToLower(strings.TrimSpace(provider)) == "openai-codex" {
		supported := codexSupportedEfforts(model)
		if apiMode == "codex_app_server" && slices.Contains(supported, "max") {
			return append(slices.Clone(supported), "ultra")
		}
		return supported
	}
	return openAICompatWireEfforts
}

// Picker label of a ladder level, the level itself when the route sends it verbatim, else what it sends
func effortDisplayLabel(effort, provider, model, apiMode string) string {
	requested := strings.ToLower(strings.TrimSpace(effort))
	clamped := clampEffort(requested, routeSupportedEfforts(provider, model, apiMode), nil)
	if requested == "" || clamped == requested {
		return requested
	}
	return fmt.Sprintf("%s (sends %s on this route)", requested, clamped)
}

// EffortDisplayLabel is the picker and reasoning status label for a level on a route
func EffortDisplayLabel(effort, provider, model, apiMode string) string {
	return effortDisplayLabel(effort, provider, model, apiMode)
}

// Turn effort from an explicit reasoning setting clamped to the route, nil leaves codex's default
func codexTurnEffort(agent CodexRuntimeAgent, model string) *string {
	rc := agent.ReasoningConfig()
	disabled := false
	if b, ok := rc["enabled"].(bool); ok && !b {
		disabled = true
	}
	if rc == nil || !(disabled || cxTruthy(rc["effort"])) {
		return nil
	}
	cfg := reasoningCfg{set: true, disabled: disabled}
	if !disabled {
		cfg.effort = strings.ToLower(strings.TrimSpace(pyrepr.Str(rc["effort"])))
	}
	route := classifyResponsesRoute(agent.Provider(), agent.BaseURL())
	effort, _ := resolveReasoning(model, codexParams{ctx: home.WithOverride(context.Background(), codexAgentHome(agent)), reasoning: cfg,
		provider: agent.Provider(), baseURL: agent.BaseURL(), isCodexBackend: route.isCodexBackend, isXAI: route.isXAI})
	if effort == "max" && rc["effort"] == "ultra" && slices.Contains(routeSupportedEfforts(agent.Provider(), model, "codex_app_server"), "ultra") {
		effort = "ultra"
	}
	if effort == "" {
		return nil
	}
	return &effort
}

// Service tier the Responses path would request this turn in codex's words, nil when codex has none
func codexTurnServiceTier(agent CodexRuntimeAgent) (*string, error) {
	hooks, err := codexTurnHooks(agent)
	if err != nil {
		return nil, err
	}
	tier, _ := hooks.EffectiveRequestOverrides()["service_tier"].(string)
	word, ok := CodexTierWords[tier]
	if !ok {
		return nil, nil
	}
	return &word, nil
}

func codexStrPtrEqual(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// Spawns the agent's codex session once, retiring a live one whose prompt or provider changed
func ensureCodexSession(agent CodexRuntimeAgent, messages []map[string]any) error {
	developerInstructions := codexDeveloperInstructions(agent)
	modelProvider := codexModelProvider(agent)
	if agent.CodexSession() != nil {
		recorded := agent.CodexSessionPrompt()
		if recorded == nil || (*recorded == developerInstructions && codexStrPtrEqual(agent.CodexSessionModelProvider(), modelProvider)) {
			return nil
		}
		closeCodexSession(agent)
	}
	resumeThreadID := ""
	if agent.CodexSessionPrompt() == nil {
		stored, err := storedCodexThreadID(agent)
		if err != nil {
			return err
		}
		resumeThreadID = stored
	}
	env := agent.ToolEnv()
	var approval CodexApprovalCallback
	func() {
		defer func() { _ = recover() }()
		if !tools.NoUserCanAnswer(env) {
			if prompt := tools.CodexApprovalPrompt(env); prompt != nil {
				approval = prompt
			}
		}
	}()
	autoApprove := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Debug("codex app-server: approval-bypass lookup failed; keeping fail-closed default", "panic", r)
			}
		}()
		autoApprove = tools.IsApprovalBypassActive(env)
	}()
	prompt := developerInstructions
	agent.SetCodexSessionPrompt(&prompt)
	agent.SetCodexSessionModelProvider(modelProvider)
	historySeed := RenderHistorySeed(messages)
	cwd := agent.SessionCwd()
	if cwd == "" {
		resolved, err := tools.ResolveAgentCwd(env)
		if err != nil {
			return err
		}
		cwd = resolved
	}
	codexBin := GetConfiguredCodexBinary(loadLiveConfigSafe(codexAgentHome(agent)))
	providerID := ""
	if modelProvider != nil {
		providerID = *modelProvider
	}
	agent.SetCodexSession(NewCodexAppServerSession(CodexAppServerSessionOptions{
		Cwd: cwd, ApprovalCallback: approval, CodexBin: codexBin,
		RequestRouting: &CodexServerRequestRouting{AutoApproveExec: autoApprove, AutoApproveApplyPatch: autoApprove},
		OnEvent:        makeCodexAppServerEventBridge(agent), DeveloperInstructions: developerInstructions,
		Model: codexWireModel(agent, modelProvider), ModelProvider: providerID,
		ResumeThreadID: resumeThreadID, HistorySeed: historySeed, ToolEnv: env,
	}))
	return nil
}

// Splices the projected rows into messages and flushes them, true once they are durable in the session db
func persistProjectedMessages(agent CodexRuntimeAgent, turn *CodexTurnResult, messages *[]map[string]any) bool {
	if len(turn.ProjectedMessages) == 0 {
		return false
	}
	projected := turn.ProjectedMessages
	if first := projected[0]; turn.SubmittedUserText != nil && first["role"] == "user" && first["content"] == any(*turn.SubmittedUserText) {
		projected = projected[1:]
	}
	for _, message := range projected {
		state.AppendMessage(messages, message, nil)
	}
	if agent.CodexSessionDB() == nil {
		return false
	}
	flushOK := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("codex app-server projected-message flush failed", "panic", r)
			}
		}()
		ok, err := agent.FlushMessagesToSessionDB(*messages)
		if err != nil {
			slog.Warn("codex app-server projected-message flush failed", "error", err)
			return
		}
		flushOK = ok
	}()
	if !flushOK {
		slog.Warn(fmt.Sprintf("codex app-server turn was delivered but could NOT be persisted to the session DB (session=%s) — this turn will be missing after restart/resume", agent.SessionID()))
	}
	return flushOK
}

// Rewrites a codex reply through the output transform before its rows persist, hiding a held turn's commentary
func transformCodexReply(agent CodexRuntimeAgent, turn *CodexTurnResult, turnID any) (map[string]any, error) {
	held := agent.HoldStream()
	if turn.FinalText == "" || (turn.Interrupted && !held) {
		return map[string]any{"final_response": turn.FinalText}, nil
	}
	hooks, err := codexTurnHooks(agent)
	if err != nil {
		return nil, err
	}
	final, transformed, preTransform := hooks.ApplyLLMOutputTransform(turn.FinalText, turnID)
	var rows []map[string]any
	for _, m := range turn.ProjectedMessages {
		if m["role"] == "assistant" && cxTruthy(m["content"]) {
			rows = append(rows, m)
		}
	}
	finalIndex := -1
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i]["content"] == any(turn.FinalText) {
			finalIndex = i
			break
		}
	}
	if transformed && finalIndex >= 0 {
		rows[finalIndex]["content"] = final
	}
	if held {
		for i, message := range rows {
			if i != finalIndex && !cxTruthy(message["display_kind"]) {
				message["display_kind"] = "hidden"
			}
		}
	}
	if agent.HoldStream() {
		preTransform = nil
	}
	return map[string]any{"final_response": final, "response_transformed": transformed, "pre_transform_response": preTransform}, nil
}

func codexNoneIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Hands the turn to the codex app server and projects its events into messages in the chat completions shape
func RunCodexAppServerTurn(agent CodexRuntimeAgent, userMessage string, originalUserMessage any, messages []map[string]any, effectiveTaskID string, shouldReviewMemory bool, turnID any) (map[string]any, error) {
	if agent.CompressionCheckpointRequired() {
		return nil, CheckpointBlocked("codex_app_server owns the authoritative thread and compacts it without a truthful pre-compaction transcript boundary")
	}
	if err := ensureCodexSession(agent, messages); err != nil {
		return nil, err
	}
	turn, err := func() (turn *CodexTurnResult, err error) {
		defer func() {
			if r := recover(); r != nil {
				turn, err = nil, fmt.Errorf("%v", r)
			}
		}()
		if _, err := startCodexThread(agent); err != nil {
			return nil, err
		}
		wireModel := codexWireModel(agent, agent.CodexSessionModelProvider())
		effort := codexTurnEffort(agent, wireModel)
		tier, err := codexTurnServiceTier(agent)
		if err != nil {
			return nil, err
		}
		return agent.CodexSession().RunTurn(userMessage, CodexTurnOptions{Model: wireModel, ReasoningEffort: effort, ServiceTier: tier})
	}()
	if err != nil {
		slog.Error("codex app-server turn failed", "error", err)
		closeCodexSession(agent)
		interrupted, interruptMessage := consumeUserInterrupt(agent, true)
		return codexTurnResultOf(interrupted, interruptMessage, messages, 0, false, err.Error(),
			fmt.Sprintf("Codex app-server turn failed: %s. Fall back to default runtime with `/codex-runtime auto`.", err.Error()), nil), nil
	}
	interrupted, interruptMessage := consumeUserInterrupt(agent, turn.Interrupted)
	if turn.ShouldRetire {
		slog.Warn(fmt.Sprintf("codex app-server session retired (turn error: %s)", pyrepr.Str(codexNoneIfEmpty(turn.Error))))
		closeCodexSession(agent)
	}
	reply, err := transformCodexReply(agent, turn, turnID)
	if err != nil {
		return nil, err
	}
	if persistProjectedMessages(agent, turn, &messages) && !turn.ShouldRetire {
		storeCodexThreadID(agent, turn.ThreadID)
	}
	usage := finishCodexTurn(agent, turn, messages, originalUserMessage, shouldReviewMemory, reply["final_response"])
	extra := map[string]any{"agent_persisted": true, "codex_thread_id": codexNoneIfEmpty(turn.ThreadID), "codex_turn_id": codexNoneIfEmpty(turn.TurnID)}
	for k, v := range reply {
		if k != "final_response" {
			extra[k] = v
		}
	}
	for k, v := range usage {
		extra[k] = v
	}
	return codexTurnResultOf(interrupted, interruptMessage, messages, 1, !turn.Interrupted && turn.Error == "", codexNoneIfEmpty(turn.Error), reply["final_response"], extra), nil
}
