package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Plugin engine lookups of agent init, replaced by tests that hand in their own engine
var (
	loadContextEngineFn   = plugins.LoadContextEngine
	pluginContextEngineFn = plugins.GetPluginContextEngine
)

// Plugin context engine named by context.engine, nil for the built in compressor or when none loads
func selectContextEngine(ctx context.Context, agentCfg map[string]any) ContextEngine {
	if config.SafeWorkerEnabled() {
		return nil
	}
	name := "compressor"
	if section, ok := agentCfg["context"].(map[string]any); ok {
		if v := cfgText(firstTruthy(section["engine"], "compressor")); v != "" {
			name = v
		}
	}
	if name == "compressor" {
		return nil
	}
	var selected ContextEngine
	copyFailed := false
	if obj := loadContextEngineFn(ctx, name); obj != nil {
		if eng, err := newHostedContextEngine(ctx, obj); err == nil {
			selected = eng
		} else {
			runAgentLog().Debug(fmt.Sprintf("Context engine load from plugins/context_engine/: %v", err))
		}
	}
	if selected == nil {
		var candidate ContextEngine
		switch c := pluginContextEngineFn(ctx).(type) {
		case ContextEngine:
			candidate = c
		case *plugins.HostedObject:
			if eng, err := newHostedContextEngine(ctx, c); err == nil {
				candidate = eng
			}
		}
		if candidate != nil && candidate.Name() == name {
			func() {
				defer func() {
					if r := recover(); r != nil {
						copyFailed = true
						runAgentLog().Warn(fmt.Sprintf("Context engine '%s' could not be safely copied for this agent (%v) — falling back to built-in compressor. Plugin engines that hold uncopyable state (locks, DB connections) should override clone_for_agent() (or __deepcopy__) to copy only mutable budget state.", name, r))
					}
				}()
				selected = candidate.CloneForAgent()
			}()
		}
	}
	if selected == nil && !copyFailed {
		runAgentLog().Warn(fmt.Sprintf("Context engine '%s' not found — falling back to built-in compressor", name))
	}
	return selected
}

// Output reservation of the compressor, the native Gemini default when the agent sets none
func compressorMaxTokens(a *AIAgent) any {
	if a.maxTokens != 0 {
		return a.maxTokens
	}
	switch strings.ToLower(strings.TrimSpace(a.provider)) {
	case "gemini", "google", "google-gemini", "google-ai-studio":
		return provider.GeminiDefaultMaxOutputTokens
	}
	if provider.IsNativeGeminiBaseURL(a.baseURL) {
		return provider.GeminiDefaultMaxOutputTokens
	}
	return nil
}

// Selected plugin engine or the built in compressor, with the compression switches and native compaction caps
func buildContextEngine(ctx context.Context, a *AIAgent, agentCfg map[string]any, cs CompressionSettings, customProviders []map[string]any, effectiveContextLength int) {
	if selected := selectContextEngine(ctx, agentCfg); selected != nil {
		a.contextCompressor = selected
		a.compressionThresholdAutoraised = nil
		pluginCtxLen := provider.GetModelContextLength(ctx, provider.ContextLengthRequest{
			Model: a.model, BaseURL: a.baseURL, APIKey: a.apiKey, Provider: a.provider,
			ConfigContextLength: effectiveContextLength, CustomProviders: customProviders,
		})
		if len(cs.ModelThresholds) > 0 {
			selected.Base().ModelThresholds = cs.ModelThresholds
			if hosted, ok := selected.(*hostedContextEngine); ok {
				_ = hosted.obj.Set(ctx, "model_thresholds", cs.ModelThresholds)
			}
		}
		selected.UpdateModel(a.model, pluginCtxLen, a.baseURL, a.apiKey, a.provider, a.apiMode)
		if !a.quietMode {
			runAgentLog().Info("Using context engine: " + selected.Name())
		}
	} else {
		providers := make([]any, len(customProviders))
		for i, p := range customProviders {
			providers[i] = p
		}
		var configured *int
		if effectiveContextLength != 0 {
			configured = &effectiveContextLength
		}
		var tokensCap any
		if cs.ThresholdTokens != nil {
			tokensCap = *cs.ThresholdTokens
		}
		a.contextCompressor = NewContextCompressor(ctx, ContextCompressorOptions{
			Model: a.model, ThresholdPercent: cs.Threshold, ProtectFirstN: cs.ProtectFirst, ProtectLastN: cs.ProtectLast,
			SummaryTargetRatio: cs.TargetRatio, QuietMode: a.quietMode, BaseURL: a.baseURL, APIKey: a.apiKey,
			ConfigContextLength: configured, Provider: a.provider, APIMode: a.apiMode,
			AbortOnSummaryFailure: cs.AbortOnSummaryFailure, MaxTokens: compressorMaxTokens(a), ModelThresholds: cs.ModelThresholds,
			ThresholdTokensCap: tokensCap, ProactivePruneTokens: cs.ProactivePruneTokens,
			ProactivePruneMinResultChars: cs.ProactivePruneMinChars, ProactivePruneMinReclaimTokens: cs.ProactivePruneMinReclaim,
			MinTailUserMessages: cs.MinTailUsers, TailMode: cs.TailMode, CustomProviders: providers, Env: a.env,
		})
	}
	cc, native := a.contextCompressor.(*ContextCompressor)
	if native {
		func() {
			defer func() { _ = recover() }()
			_ = cc.BindSessionState(a.sessionDB, a.sessionID)
		}()
	}
	a.compressionEnabled, a.compressionInPlace = cs.Enabled, cs.InPlace
	if cs.CheckpointRequired && cs.MicroCompact {
		compressionLog().Warn("compression.checkpoint_required is enabled: post-turn micro-compaction is disabled for this agent so every lossy rewrite passes through the checkpoint-gated compressor.")
		cs.MicroCompact = false
	}
	if native {
		cc.microCompactEnabled, cc.microCompactEveryNTurns, cc.microCompactDefragThresholdTokens = cs.MicroCompact, cs.MicroCompactEveryNTurns, cs.MicroCompactDefragTokens
		if cs.PrepareAhead {
			cc.SetPreparedCompaction(NewPreparedCompaction())
		}
	} else if hosted, ok := a.contextCompressor.(*hostedContextEngine); ok {
		for name, value := range map[string]any{"_micro_compact_enabled": cs.MicroCompact, "_micro_compact_every_n_turns": cs.MicroCompactEveryNTurns, "_micro_compact_defrag_threshold_tokens": cs.MicroCompactDefragTokens} {
			if _, err := hosted.obj.Get(ctx, name); err == nil {
				_ = hosted.obj.Set(ctx, name, value)
			}
		}
	}
	a.compressionCheckpointRequired = cs.CheckpointRequired
	warnCheckpointRequiredWithoutCapableProvider(a.compressionCheckpointRequired, a.memoryManager)
	a.codexAppServerAutoCompaction = cs.CodexAppServerAuto
	a.codexResponsesNativeCompaction = cs.CodexResponsesNative
	a.codexResponsesCompactThreshold = 0
	if cs.CodexResponsesCompactThreshold != nil {
		a.codexResponsesCompactThreshold = float64(*cs.CodexResponsesCompactThreshold)
	}
	isCodex := strings.ToLower(strings.TrimSpace(a.provider)) == "openai-codex"
	a.runtimeCapabilities = map[string]bool{"native_compaction": provider.ResolveNativeCompactionCapability(a.model, a.baseURL, a.provider, isCodex)}
	a.maxCompressionAttempts = cs.MaxAttempts
	a.compressionIdleCompactAfterSeconds = float64(cs.IdleCompactAfterSeconds)
}

// Refuses a context window under the floor unless LM Studio carries an explicit configured length
func enforceMinimumContext(a *AIAgent) error {
	window := 0
	if a.contextCompressor != nil {
		window = a.contextCompressor.Base().ContextLength
	}
	local := a.baseURL != "" && provider.IsLocalEndpoint(a.baseURL)
	if a.ollamaNumCtx != 0 && local {
		window = max(window, a.ollamaNumCtx)
	}
	lmstudioExplicit := strings.ToLower(strings.TrimSpace(a.provider)) == "lmstudio" && a.configContextLength > 0
	if window == 0 || window >= provider.MinimumContextLength || lmstudioExplicit {
		return nil
	}
	floorK := provider.MinimumContextLength / 1000
	var remedy string
	if local {
		remedy = fmt.Sprintf("Your local server is serving a %s-token window.  Start it with at least %dK context (llama.cpp: -c %d; vLLM: --max-model-len; Ollama: OLLAMA_CONTEXT_LENGTH=%d or a Modelfile num_ctx), or set model.ollama_num_ctx in config.yaml to the window it really serves (at least %dK).",
			commaInt(window), floorK, provider.MinimumContextLength, provider.MinimumContextLength, floorK)
	} else {
		remedy = fmt.Sprintf("Choose a model with at least %dK context.  If your server reports a window smaller than the model's true window, set model.context_length in config.yaml to the real value (this must be at least %dK).", floorK, floorK)
	}
	return errors.New(fmt.Sprintf("Model %s has a context window of %s tokens, which is below the minimum %s required by Icebath Agent.  %s",
		a.model, commaInt(window), commaInt(provider.MinimumContextLength), remedy))
}

// Warns gateway and cron surfaces once that a Nous Hermes 3 or 4 model is not agentic
func warnNonagenticHermesModel(a *AIAgent) {
	if a.quietMode || a.platform == "" || a.platform == "cli" {
		return
	}
	if warning := provider.CheckHermesModelWarning(a.model); warning != "" {
		a.emitWarning("⚠ Nous Research Hermes 3 & 4 models are NOT agentic — they lack reliable tool-calling for agent workflows (delegation, cron, proactive tools). Consider an agentic model instead (Claude, GPT, Gemini, Qwen-Coder, etc.).")
		runAgentLog().Warn(warning)
	}
}

// Context engine tool schemas appended to the surface, then the engine session start hook
func injectContextEngineTools(ctx context.Context, a *AIAgent) {
	a.contextEngineToolNames = map[string]bool{}
	if a.contextCompressor != nil && a.tools != nil && (a.enabledToolsets == nil || slices.Contains(a.enabledToolsets, "context_engine")) {
		existing := map[string]bool{}
		for _, t := range a.tools {
			if m, ok := t.(map[string]any); ok {
				existing[snapshotToolName(m)] = true
			}
		}
		for _, raw := range a.contextCompressor.GetToolSchemas() {
			schema, ok := memory.NormalizeToolSchema(raw)
			if !ok {
				runAgentLog().Warn("Context engine returned a tool schema with no resolvable name; skipping to avoid poisoning the request (" + pyrepr.Repr(raw) + ")")
				continue
			}
			name, _ := schema["name"].(string)
			if existing[name] {
				continue
			}
			a.tools = append(a.tools, map[string]any{"type": "function", "function": schema})
			if a.validToolNames == nil {
				a.validToolNames = map[string]bool{}
			}
			a.validToolNames[name], a.contextEngineToolNames[name], existing[name] = true, true, true
		}
	}
	if a.contextCompressor != nil {
		platform := a.platform
		if platform == "" {
			platform = "cli"
		}
		var conversationID any
		if a.gatewaySessionKey != "" {
			conversationID = a.gatewaySessionKey
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					runAgentLog().Debug(fmt.Sprintf("Context engine on_session_start: %v", r))
				}
			}()
			a.contextCompressor.OnSessionStart(a.sessionID, map[string]any{
				"hermes_home": home.Dir(ctx), "platform": platform, "model": a.model,
				"context_length": a.contextCompressor.Base().ContextLength, "conversation_id": conversationID,
			})
		}()
	}
}

// Subdirectory hint tracker over the session or scoped terminal cwd and the fresh usage counters
func initUsageState(a *AIAgent) {
	workingDir := a.sessionCwd
	if workingDir == "" {
		workingDir, _ = tools.ScopeTerminalCwd(a.env)
	}
	a.subdirectoryHints = tools.NewSubdirectoryHintTrackerFor(a.env, workingDir, !a.skipContextFiles)
	a.userTurnCount, a.isUserInitiatedTurn = 0, false
	a.usageAnchor, a.turnBaseUsageAnchor, a.requestPressureAnchored = nil, nil, false
	a.sessionPromptTokens, a.sessionCompletionTokens, a.sessionTotalTokens, a.sessionAPICalls = 0, 0, 0, 0
	a.sessionInputTokens, a.sessionOutputTokens = 0, 0
	a.sessionCacheReadTokens, a.sessionCacheWriteTokens, a.sessionReasoningTokens = 0, 0, 0
	a.sessionEstimatedCostUSD, a.sessionCostStatus, a.sessionCostSource = 0, "unknown", "none"
	a.apiLatencyHistory, a.apiOutputHistory = make([]float64, 0, 10), make([]int, 0, 10)
}

// Agent config read once for memory, skills and compression, empty with a class only warning when it fails
func loadAgentConfig(ctx context.Context) map[string]any {
	tree, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		runAgentLog().Warn(fmt.Sprintf("Agent configuration could not be loaded (%T); using defaults for memory, skills, compression and related settings. Check the active Icebath environment and dependencies; run 'icebath config check' after configuration imports work again.", err))
		return map[string]any{}
	}
	return map[string]any(tree)
}

// Constructs the agent in hermes order from routing through the primary runtime snapshot
func initAgent(ctx context.Context, a *AIAgent, o AIAgentOptions) error {
	if config.SafeWorkerEnabled() {
		o.SkipContextFiles, o.SkipMemory, o.SkipBackgroundReview = true, true, true
		o.LoadSoulIdentity = false
		o.PrefillMessages = []any{}
		o.FallbackModel = nil
	}
	provider.InstallSafeStdio()
	copyPassthroughParams(a, o)
	copyGatewayIdentityParams(a, o)
	a.env = o.Env
	if a.env == nil {
		a.env = tools.ToolEnvFrom(ctx)
	}
	a.env = withToolMetricSink(ctx, a.env)
	a.sessionCwd = o.Cwd
	a.iterationBudget = o.IterationBudget
	if a.iterationBudget == nil {
		a.iterationBudget = NewIterationBudget(o.MaxIterations)
	}
	a.printFn, a.backgroundReviewCallback = nil, nil
	a.memoryNotifications = "on"
	a.skipBackgroundReview = o.SkipBackgroundReview
	a.logPrefix = ""
	if o.LogPrefix != "" {
		a.logPrefix = o.LogPrefix + " "
	}
	baseURL := o.BaseURL
	if provider.IsActualRoute(o.Provider, baseURL) {
		baseURL = provider.NormalizeActualBaseURL(baseURL)
	}
	a.setBaseURL(baseURL)
	var providerName *string
	if p := strings.ToLower(strings.TrimSpace(o.Provider)); p != "" {
		providerName = &p
		a.provider = p
	} else {
		a.provider = ""
	}
	a.requestedProvider = a.provider
	if rp := strings.ToLower(strings.TrimSpace(o.RequestedProvider)); rp != "" {
		a.requestedProvider = rp
	}
	a.capabilities = map[string]bool{}
	for k, v := range o.Capabilities {
		a.capabilities[k] = v
	}
	a.credentialPool = o.CredentialPool
	a.acpCommand = o.ACPCommand
	if a.acpCommand == "" {
		a.acpCommand = o.Command
	}
	a.acpArgs = append([]string{}, o.ACPArgs...)
	if len(o.ACPArgs) == 0 {
		a.acpArgs = append([]string{}, o.Args...)
	}
	a.resolveAPIMode(ctx, o.APIMode, providerName, baseURL)
	a.finalizeRouting(ctx, o.APIMode)
	copyCallbackParams(a, o)
	a.suppressStatusOutput = false
	setControlStateDefaults(a)
	if err := setupAgentLogging(ctx, a); err != nil {
		return err
	}
	a.reasoningEchoFlag = readReasoningEchoFromConfig(ctx)
	a.requestOverrides = map[string]any{}
	for k, v := range o.RequestOverrides {
		a.requestOverrides[k] = v
	}
	a.prefillMessages = o.PrefillMessages
	if a.prefillMessages == nil {
		a.prefillMessages = []any{}
	}
	a.forceASCIIPayload = false
	a.imageRejectingModels = map[[2]string]bool{}
	a.reasoningRejectingRoutes = map[[3]string]map[string]bool{}
	a.fastWindow = nil
	initPromptCacheConfig(ctx, a)
	initTurnState(a, o.RunBudgetSeconds)
	setStreamStateDefaults(a)
	if err := buildClient(ctx, a, o.APIKey, baseURL, o.FallbackModel); err != nil {
		return err
	}
	initFallbackChain(a, o.FallbackModel)
	loadTools(ctx, a, o.EnabledToolsets, o.DisabledToolsets)
	if err := initSessionState(ctx, a, o); err != nil {
		return err
	}
	agentCfg := loadAgentConfig(ctx)
	applyDisplayConfig(a, agentCfg, o.Platform)
	initMemory(ctx, a, agentCfg, o.SkipMemory, o.Platform, o.MemoryManager)
	applyAgentSection(a, agentCfg)
	cs, err := parseCompressionConfig(a, agentCfg)
	if err != nil {
		return err
	}
	configContextLength, customProviders, effectiveContextLength, modelCfg := resolveContextLength(ctx, a, agentCfg, baseURL)
	buildContextEngine(ctx, a, agentCfg, cs, customProviders, effectiveContextLength)
	pinned := 0
	if configContextLength != nil {
		pinned = *configContextLength
	}
	configureOllamaNumCtx(ctx, a, modelCfg, pinned)
	if err := enforceMinimumContext(a); err != nil {
		return err
	}
	warnNonagenticHermesModel(a)
	injectContextEngineTools(ctx, a)
	initUsageState(a)
	clampCompressorToOllamaNumCtx(a)
	emitCompressionSummary(ctx, a, cs)
	snapshotPrimaryRuntime(ctx, a)
	return nil
}
