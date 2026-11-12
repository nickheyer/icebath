package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Executor thread body of the turn under the session policy, the refused result when the policy cannot bind
func (t *TurnRunner) runSync(ctx context.Context) (map[string]any, error) {
	policy, err := gateway.LocalPolicyForSource(ctx, t.runner, t.ctx.source)
	if err != nil {
		return nil, err
	}
	authority, err := gateway.ActiveAuthority(ctx, t.runner)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	entered := false
	scopeErr := gateway.PolicyScope(ctx, policy, authority, func(scoped context.Context) error {
		entered = true
		apiCtx, err := gateway.APIPolicyScope(scoped)
		if err != nil {
			return err
		}
		if err := gateway.BindTurnWorkspace(t.ctx.sessionEnv, policy, t.ctx.sessionID); err != nil {
			return err
		}
		result, err = t.runSyncScoped(apiCtx)
		return err
	})
	if scopeErr != nil {
		var storeErr *state.RuntimeStoreError
		if entered || !errors.As(scopeErr, &storeErr) {
			return nil, scopeErr
		}
		result = refusedTurnResult(ctx, policy, storeErr)
		if result == nil {
			return nil, scopeErr
		}
	}
	if captured := gateway.ExecutionResultOf(ctx); captured != nil {
		cliFields := t.cliResultFields
		t.cliResultFields = nil
		if len(cliFields) > 0 {
			merged := maps.Clone(result)
			maps.Copy(merged, cliFields)
			if pyrepr.Truthy(result["response_previewed"]) {
				merged["response_previewed"] = true
			}
			captured.Set("result", pyjson.Ordered(merged))
		} else {
			captured.Set("result", pyjson.Ordered(result))
		}
	}
	return result, nil
}

// Platform hint key of the turn, the policy platform else cli for the local platform
func turnPlatformKey(policy *gateway.LocalSessionPolicy, source *gateway.SessionSource) string {
	if policy != nil {
		return policy.Platform
	}
	if source.Platform == gateway.PlatformLocal {
		return "cli"
	}
	return string(source.Platform)
}

// Model and runtime of the turn with the one shot fallback notice, an error when resolution failed
func (t *TurnRunner) resolveTurnRuntime(ctx context.Context, policy *gateway.LocalSessionPolicy) (string, map[string]any, string, error) {
	tc, runner := t.ctx, t.runner
	model, runtimeKwargs, err := runner.resolveSessionAgentRuntime(ctx, tc.source, tc.sessionKey, tc.userConfig, true)
	if err != nil {
		return "", nil, "", err
	}
	pendingFallbackNotice := runner.takePreAgentFallbackNotice()
	model, runtimeKwargs, err = prepareAPIRuntime(ctx, model, runtimeKwargs)
	if err != nil {
		return "", nil, "", err
	}
	if policy != nil && policy.Model != nil && *policy.Model != "" && pendingFallbackNotice == "" {
		authority, err := gateway.ActiveAuthority(ctx, runner)
		if err != nil {
			return "", nil, "", err
		}
		current, err := policy.CurrentModel(ctx, authority)
		if err != nil {
			return "", nil, "", err
		}
		model = ""
		if current != nil {
			model = *current
		}
	}
	model, runtimeKwargs, err = oneTurnRoute(ctx, t, policy, model, runtimeKwargs)
	if err != nil {
		return "", nil, "", err
	}
	runLog().Debug(fmt.Sprintf("run_agent resolved: model=%s provider=%s session=%s", model, pyrepr.Str(runtimeKwargs["provider"]), tc.sessionKey))
	return model, runtimeKwargs, pendingFallbackNotice, nil
}

// Reasoning config and service tier of the turn, the API admission's request options winning
func (t *TurnRunner) resolveTurnEffort(ctx context.Context, policy *gateway.LocalSessionPolicy, model string) (map[string]any, error) {
	tc, runner := t.ctx, t.runner
	var reasoningConfig map[string]any
	var err error
	if policy != nil {
		reasoningConfig, err = policy.CurrentReasoningConfig(ctx, runner, tc.sessionKey)
	} else {
		reasoningConfig, err = runner.resolveSessionReasoningConfig(ctx, tc.source, tc.sessionKey, model)
	}
	if err != nil {
		return nil, err
	}
	reasoningConfig = oneTurnReasoning(ctx, t, reasoningConfig)
	var tier *string
	if policy != nil {
		tier, err = policy.CurrentServiceTier(ctx, runner, tc.sessionKey)
	} else {
		tier, err = runner.resolveSessionServiceTier(ctx, tc.source, tc.sessionKey)
	}
	if err != nil {
		return nil, err
	}
	runner.serviceTier = tier
	if api := gateway.APIExecution(ctx); api != nil {
		settings, _ := api.GetOr("settings", nil).(map[string]any)
		var modelOptions any
		if settings != nil {
			modelOptions = settings["model_options"]
		} else if obj, ok := api.GetOr("settings", nil).(interface{ GetOr(string, any) any }); ok {
			modelOptions = obj.GetOr("model_options", nil)
		}
		if requested := gateway.APIRequestReasoningConfig(modelOptions); requested != nil {
			reasoningConfig = requested
		}
		if requestedTier := gateway.APIRequestServiceTier(modelOptions); !gateway.IsAPIRequestOptionMissing(requestedTier) {
			if s, ok := requestedTier.(string); ok {
				runner.serviceTier = &s
			} else {
				runner.serviceTier = nil
			}
		}
	}
	runner.reasoningConfig = reasoningConfig
	return reasoningConfig, nil
}

// Executor thread body of the turn, the gateway result of one agent run
func (t *TurnRunner) runSyncScoped(ctx context.Context) (map[string]any, error) {
	tc, runner := t.ctx, t.runner
	policy, err := gateway.LocalPolicyForSource(ctx, runner, tc.source)
	if err != nil {
		return nil, err
	}
	if policy != nil && policy.Yolo() && tc.sessionKey != "" {
		tools.ApplyLaunchYolo(tc.sessionKey)
	}
	platformKey := turnPlatformKey(policy, tc.source)
	combinedEphemeral := t.combinedEphemeralPrompt(ctx)
	if policy != nil && policy.SkillsPrompt != nil && *policy.SkillsPrompt != "" {
		if combinedEphemeral != "" {
			combinedEphemeral += "\n\n" + *policy.SkillsPrompt
		} else {
			combinedEphemeral = *policy.SkillsPrompt
		}
	}
	var maxIterations int
	if policy != nil {
		maxIterations = policy.MaxTurns()
	} else {
		maxIterations = gateway.CurrentMaxIterations(ctx)
	}
	if room := gateway.CurrentRoomExecutionPolicy(ctx); room != nil {
		maxIterations = int(room.MaxIterations)
		tc.enabledToolsets = slices.Clone(room.EnabledToolsets)
	}
	model, runtimeKwargs, pendingFallbackNotice, err := t.resolveTurnRuntime(ctx, policy)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Model resolution failed for session %s: %s", tc.sessionKey, err), "exc_info", err)
		text, reason, retryable := resolutionFailureReply(ctx, err, tc.source.Platform)
		return agent.StampFailure(map[string]any{"final_response": text, "messages": []any{}, "api_calls": 0, "tools": []any{},
			"failed": true, "completed": false, "error": err.Error()}, reason, retryable), nil
	}
	var pr map[string]any
	if policy != nil {
		authority, err := gateway.ActiveAuthority(ctx, runner)
		if err != nil {
			return nil, err
		}
		if pr, err = gateway.SessionRouting(ctx, authority, policy); err != nil {
			return nil, err
		}
	} else {
		pr = runner.providerRouting
	}
	reasoningConfig, err := t.resolveTurnEffort(ctx, policy, model)
	if err != nil {
		return nil, err
	}
	streams := t.setupStreamConsumer(ctx, platformKey)
	route := runner.resolveTurnAgentConfig(model, runtimeKwargs, tierFromRunner)
	a, reusedCachedAgent, err := t.resolveTurnAgent(ctx, route, platformKey, combinedEphemeral, maxIterations, reasoningConfig, pr)
	if err != nil {
		return nil, err
	}
	if pendingFallbackNotice != "" {
		a.SetPendingFallbackNotice([]string{pendingFallbackNotice})
	}
	if err := t.wireTurnAgentCallbacks(ctx, a, route, reasoningConfig, streams); err != nil {
		return nil, err
	}
	var localSinks *localViewerPrevious
	if localViewerTurn(t) && !tc.muteNotificationReply {
		if localSinks, err = wireLocalViewerEvents(ctx, t, a); err != nil {
			return nil, err
		}
		a.MarkViewerTurnOutputBase()
	}
	run, err := t.runTurnConversation(ctx, a, reusedCachedAgent, localSinks)
	if err != nil {
		return nil, err
	}
	return t.projectTurnResult(ctx, a, run, streams, localSinks)
}

// History, media paths, raw result and pre run compression count of one agent run
type turnConversation struct {
	agentHistory      []map[string]any
	historyMediaPaths map[string]struct{}
	result            map[string]any
	compressions      *int
}

// Loads the history and runs the conversation under the prompt scopes, the local viewer sinks restored after
func (t *TurnRunner) runTurnConversation(ctx context.Context, a *agent.AIAgent, reusedCachedAgent bool, localSinks *localViewerPrevious) (*turnConversation, error) {
	defer func() {
		if localSinks != nil {
			restoreLocalViewerEvents(a, localSinks)
			publishUsage(ctx, t, a, true)
			publishSettledInfo(ctx, t, a)
		}
	}()
	agentHistory, observed, historyMediaPaths, err := t.loadTurnHistory(ctx, a, reusedCachedAgent)
	if err != nil {
		return nil, err
	}
	persistMsg, persistTS := t.prepareTurnMessage(ctx, agentHistory)
	compressions := agentCompressionCount(a)
	restoreSudo := sudoPromptScope(ctx, t, a)
	defer restoreSudo()
	scoped := secretPromptScope(ctx, t, a)
	restoreVault := vaultPromptScope(scoped, t, a)
	defer restoreVault()
	result, err := runClassicCLITurn(scoped, t, func() (map[string]any, error) {
		return t.runConversationWithApproval(scoped, a, agentHistory, observed, persistMsg, persistTS)
	})
	if err != nil {
		return nil, err
	}
	return &turnConversation{agentHistory: agentHistory, historyMediaPaths: historyMediaPaths, result: result, compressions: compressions}, nil
}

// Gateway result of the finished run with its usage, transcript sync and viewer fields
func (t *TurnRunner) projectTurnResult(ctx context.Context, a *agent.AIAgent, run *turnConversation, streams turnStreamCallbacks,
	localSinks *localViewerPrevious) (map[string]any, error) {
	tc := t.ctx
	agentHistory, historyMediaPaths, result := run.agentHistory, run.historyMediaPaths, run.result
	persisted := persistedTurn(a, agentHistory, result, run.compressions)
	t.finishStreamConsumer(ctx, result, agentHistory, streams.consumer)
	if finiteViewerTurn(ctx) && a.CodexSession() != nil {
		a.CloseCodexSession()
		a.SetCodexSessionPrompt(nil)
	}
	finalResponse, _ := result["final_response"].(string)
	usage := turnUsage(tc.currentAgent(), result)
	compactedInPlace, effectiveSessionID, historyOffset, err := t.syncSessionAfterRun(ctx, agentHistory)
	if err != nil {
		return nil, err
	}
	common := map[string]any{
		"messages": resultGet(result, "messages", []any{}), "api_calls": resultGet(result, "api_calls", 0),
		"failed": resultGet(result, "failed", false), "failure_reason": result["failure_reason"],
		"usage_limit": result["usage_limit"],
		"partial":     resultGet(result, "partial", false), "completed": result["completed"],
		"interrupted": resultGet(result, "interrupted", false), "interrupt_message": result["interrupt_message"],
		"error": result["error"],
	}
	for _, key := range failureDescriptorKeys {
		if v, ok := result[key]; ok {
			common[key] = v
		}
	}
	turnTools := any(tc.toolsHolder)
	if len(tc.toolsHolder) == 0 {
		turnTools = []any{}
	}
	maps.Copy(common, map[string]any{
		"compression_exhausted": resultGet(result, "compression_exhausted", false),
		"compression_deferred":  resultGet(result, "compression_deferred", false),
		"tools":                 turnTools,
		"history_offset":        historyOffset, "compacted_in_place": compactedInPlace, "session_id": effectiveSessionID,
	})
	if compactedInPlace || effectiveSessionID != tc.sessionID {
		common["_compressed"] = true
	}
	if idx, ok := result["current_turn_user_idx"]; ok {
		common["current_turn_user_idx"] = idx
	}
	maps.Copy(common, usage)
	if len(persisted) > 0 {
		common["persisted_turn"] = persisted
	}
	previewed := resultGet(result, "response_previewed", false)
	viewerMeta := map[string]any{}
	if localSinks != nil {
		previewed = pyrepr.Truthy(previewed) || (finalResponse != "" && a.InterimTextWasDelivered(finalResponse))
		viewerMeta["local_viewer"] = true
		if block, ok := result["billing_block"].(map[string]any); ok {
			viewerMeta["billing_block"] = block
		}
		if warning, ok := result["warning"].(string); ok && warning != "" {
			viewerMeta["warning"] = warning
		}
	}
	if finalResponse == "" {
		finalResponse = normalizeEmptyAgentResponse(ctx, result, "", len(agentHistory))
		finalResponse = sanitizeGatewayFinalResponse(ctx, tc.source.Platform, finalResponse)
		if finalResponse == "" && pyrepr.Truthy(result["error"]) {
			finalResponse = i18n.T(ctx, "gateway.shared.warn_passthrough", "", map[string]any{"error": result["error"]})
		}
		out := map[string]any{"final_response": finalResponse}
		maps.Copy(out, common)
		if localSinks != nil {
			maps.Copy(out, viewerMeta)
			out["last_reasoning"] = result["last_reasoning"]
			out["response_previewed"] = previewed
			out["response_transformed"] = pyrepr.Truthy(result["response_transformed"])
		}
		return out, nil
	}
	finalResponse, err = appendAutoMediaTags(finalResponse, result, agentHistory, historyMediaPaths)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"final_response": finalResponse, "last_reasoning": result["last_reasoning"]}
	maps.Copy(out, common)
	maps.Copy(out, viewerMeta)
	out["response_previewed"] = previewed
	out["response_transformed"] = resultGet(result, "response_transformed", false)
	out["response_reused"] = resultGet(result, "response_reused", false)
	out["agent_persisted"] = resultGet(result, "agent_persisted", true)
	return out, nil
}

// Token counts of the agent the run used plus the one shot ledger keys the result carries
func turnUsage(a *agent.AIAgent, result map[string]any) map[string]any {
	usage := map[string]any{"last_prompt_tokens": 0, "input_tokens": 0, "output_tokens": 0, "model": nil, "provider": nil, "context_length": 0}
	if a != nil {
		usage["model"] = a.Model()
		usage["provider"] = a.Provider()
		usage["input_tokens"] = a.SessionPromptTokens()
		usage["output_tokens"] = a.SessionCompletionTokens()
		if comp := a.ContextCompressor(); comp != nil {
			usage["last_prompt_tokens"] = comp.Base().LastPromptTokens
			usage["context_length"] = comp.Base().ContextLength
		}
	}
	for _, key := range ledgerPassthroughKeys {
		if v, ok := result[key]; ok {
			usage[key] = v
		}
	}
	return usage
}
