package agent

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session tool surface with the disabled toolsets pinned, kanban guidance and the startup tool banner
func loadTools(ctx context.Context, a *AIAgent, enabled, disabled []string) {
	provider.RecoverLeftCoreAtStartup(ctx, nil, false)
	func() {
		defer func() {
			if r := recover(); r != nil {
				runAgentLog().Warn(fmt.Sprintf("Plugin discovery failed during agent setup: %v", r))
			}
		}()
		plugins.DiscoverPlugins(ctx, false)
	}()
	generation := tools.Default.Generation()
	a.toolSnapshotGeneration = &generation
	a.setDelegationBackend(tools.SavedDelegationBackend(ctx))
	a.configuredDisabledToolsets = disabled
	disabled = tools.SessionDisabledToolsets(disabled, a.platform, a.agentIsOneshot())
	a.disabledToolsets = disabled
	selection := tools.ToolSelectionKey(enabled, disabled)
	a.toolSnapshotSelection = &selection
	defs := a.toolSurface(ctx, tools.Default.ToolDefinitions(ctx, a.env, tools.DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: a.quietMode}))
	a.tools = make([]any, 0, len(defs))
	a.validToolNames = map[string]bool{}
	for _, d := range defs {
		a.tools = append(a.tools, d)
		a.validToolNames[snapshotToolName(d)] = true
	}
	guidance := ""
	if tools.OwnedKanbanTask(ctx) != "" && a.validToolNames["kanban_show"] {
		guidance = KanbanGuidance
	}
	a.kanbanWorkerGuidance = &guidance
	if a.quietMode {
		return
	}
	if len(a.tools) > 0 {
		names := make([]string, 0, len(a.validToolNames))
		for n := range a.validToolNames {
			names = append(names, n)
		}
		sort.Strings(names)
		_, _ = fmt.Fprintf(statusStdout, "🛠️  Loaded %d tools: %s\n", len(a.tools), strings.Join(names, ", "))
		if len(enabled) > 0 {
			_, _ = fmt.Fprintf(statusStdout, "   ✅ Enabled toolsets: %s\n", strings.Join(enabled, ", "))
		}
		if len(disabled) > 0 {
			_, _ = fmt.Fprintf(statusStdout, "   ❌ Disabled toolsets: %s\n", strings.Join(disabled, ", "))
		}
		var missing []string
		for name, available := range tools.Default.CheckToolsetRequirements(ctx, a.env) {
			if !available {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			a.safePrint("⚠️  Some tools may not work due to missing requirements: "+pyListRepr(missing), true)
		}
	} else {
		_, _ = fmt.Fprintln(statusStdout, "🛠️  No tools loaded (all tools filtered out or unavailable)")
	}
	if a.saveTrajectories {
		_, _ = fmt.Fprintln(statusStdout, "📝 Trajectory saving enabled")
	}
	if a.ephemeralSystemPrompt != "" {
		preview := a.ephemeralSystemPrompt
		if r := []rune(preview); len(r) > 60 {
			preview = string(r[:60]) + "..."
		}
		_, _ = fmt.Fprintf(statusStdout, "🔒 Ephemeral system prompt: '%s' (not saved to trajectories)\n", preview)
	}
	if a.usePromptCaching {
		source := "Claude via OpenRouter"
		if a.useNativeCacheLayout && a.provider == "anthropic" {
			source = "native Anthropic"
		} else if a.useNativeCacheLayout {
			source = "Anthropic-compatible endpoint"
		}
		_, _ = fmt.Fprintf(statusStdout, "💾 Prompt caching: ENABLED (%s, %s TTL)\n", source, a.cacheTTL)
	}
}

// Session id published to tools through the session context, the process env only for a root agent
func publishSessionID(ctx context.Context, a *AIAgent, sessionID string) {
	tools.SetCurrentSessionID(ctx, a.env, sessionID)
}

// Session id, logs dir, the session state table, checkpoint manager, session model config and todo store
func initSessionState(ctx context.Context, a *AIAgent, o AIAgentOptions) error {
	a.sessionStart = time.Now()
	a.sessionID = o.SessionID
	if a.sessionID == "" {
		a.sessionID = state.NewSessionID()
	}
	publishSessionID(ctx, a, a.sessionID)
	a.logsDir = home.SessionsDir(home.Dir(ctx))
	if err := os.MkdirAll(a.logsDir, 0o755); err != nil {
		return err
	}
	setSessionStateDefaults(a)
	a.checkpointMgr = tools.NewCheckpointManager(a.env, tools.CheckpointManagerOptions{
		Enabled: o.CheckpointsEnabled, MaxSnapshots: o.CheckpointMaxSnapshots,
		MaxTotalSizeMB: o.CheckpointMaxTotalSizeMB, MaxFileSizeMB: o.CheckpointMaxFileSizeMB,
	})
	a.sessionDB = o.SessionDB
	a.parentSessionID = o.ParentSessionID
	var reasoning, maxTokens any
	if o.ReasoningConfig != nil {
		reasoning = o.ReasoningConfig
	}
	if o.MaxTokens != 0 {
		maxTokens = o.MaxTokens
	}
	a.sessionInitModelConfig = map[string]any{"max_iterations": a.maxIterations, "reasoning_config": reasoning, "max_tokens": maxTokens}
	if tools.YoloModeFrozen() {
		a.sessionInitModelConfig["yolo_mode"] = true
	}
	a.todoStore = tools.NewTodoStore()
	return nil
}

// The session state table, every factory entry a fresh value for this agent
func setSessionStateDefaults(a *AIAgent) {
	a.sessionMessages = []*state.Message{}
	a.codexReasoningReplayEnabled, a.codexReasoningReplayRejected = true, false
	a.memoryWriteOrigin, a.memoryWriteContext = "assistant_tool", "foreground"
	a.cachedSystemPrompt, a.cachedSystemPromptStatic = "", ""
	a.autoLoadSkillsResolved = false
	a.autoLoadSkillsResult = autoLoadSkillsResult{}
	a.frozenWorkspaceSnapshot = nil
	a.ownsSessionDB = false
	a.sessionPersistLock = &pythread.RLock{}
	a.pendingCLIUserMessage = nil
	a.lastFlushedDBIdx = 0
	a.sessionDBCreated = false
	a.endSessionOnClose = true
	a.persistDisabled = false
}

// Display, LM Studio load mode, streaming switch and tool loop guardrails from the config
func applyDisplayConfig(a *AIAgent, agentCfg map[string]any, platform string) {
	a.showCommentary = pyrepr.Truthy(cfgValue(cfgDict(agentCfg, "display"), "show_commentary", true))
	a.fastAutoSeconds = 60
	if section, ok := agentCfg["agent"].(map[string]any); ok {
		if v, present := section["fast_auto_seconds"]; present {
			a.fastAutoSeconds, _ = pyNumber(v)
		}
	}
	modelSection := cfgDict(agentCfg, "model")
	loadMode := strings.ToLower(strings.TrimSpace(cfgText(firstTruthy(modelSection["lmstudio_load_mode"], "explicit"))))
	a.lmstudioLoadMode = "explicit"
	if loadMode == "explicit" || loadMode == "jit" {
		a.lmstudioLoadMode = loadMode
	}
	if a.lmstudioLoadMode != loadMode {
		runAgentLog().Warn(fmt.Sprintf("Invalid model.lmstudio_load_mode=%s; expected 'explicit' or 'jit'. Using explicit.", pyrepr.Repr(modelSection["lmstudio_load_mode"])))
	}
	streaming := strings.ToLower(strings.TrimSpace(pyrepr.Str(cfgValue(modelSection, "streaming", "true"))))
	a.RetrySession().DisableStreaming = slices.Contains([]string{"false", "0", "no", "off"}, streaming)
	if !a.RetrySession().DisableStreaming && !slices.Contains([]string{"true", "1", "yes", "on"}, streaming) {
		runAgentLog().Warn(fmt.Sprintf("Invalid model.streaming=%s; expected a boolean. Using streaming (default).", pyrepr.Repr(modelSection["streaming"])))
	}
	a.stream5xxProbeTS = 0
	guardrails, _ := agentCfg["tool_loop_guardrails"].(map[string]any)
	cfg := tools.ToolCallGuardrailConfigFromMap(guardrails, platform)
	a.toolGuardrails = tools.NewToolCallGuardrailController(&cfg)
}

// Memory manager scoping options, the CLI warn and status callbacks, title and gateway identity
func memoryProviderInitOptions(ctx context.Context, a *AIAgent, platform string) memory.InitOptions {
	if platform == "" {
		platform = "cli"
	}
	o := memory.InitOptions{Home: home.Dir(ctx), Platform: platform, AgentContext: "primary"}
	if platform == "cron" || platform == "subagent" {
		o.AgentContext = platform
	}
	if platform == "cli" {
		o.Warn, o.Status = a.emitWarning, a.emitStatus
	}
	if a.sessionDB != nil {
		if title, ok, err := a.sessionDB.GetSessionTitle(ctx, a.sessionID); err == nil && ok && title != "" {
			o.SessionTitle = title
			if source, sok, serr := a.sessionDB.GetSessionTitleSource(ctx, a.sessionID); serr == nil && sok && source != "" {
				o.SessionTitleSource = source
			}
		}
	}
	o.UserID, o.UserIDAlt, o.UserName = a.userID, a.userIDAlt, a.userName
	o.ChatID, o.ChatName, o.ChatType = a.chatID, a.chatName, a.chatType
	o.ThreadID, o.GatewaySessionKey = a.threadID, a.gatewaySessionKey
	o.Cwd = a.sessionCwd
	o.AgentIdentity, o.AgentWorkspace = profiles.GetActiveProfileName(ctx), "icebath"
	return o
}

// Built in memory store and the external memory provider manager, then the provider tools on the surface
func initMemory(ctx context.Context, a *AIAgent, agentCfg map[string]any, skipMemory bool, platform string, manager *memory.MemoryManager) {
	a.memoryStore = nil
	a.memoryEnabled, a.userProfileEnabled = false, false
	a.memoryNudgeInterval, a.turnsSinceMemory, a.itersSinceSkill = 10, 0, 0
	if config.SafeWorkerEnabled() {
		a.memoryManager = nil
		return
	}
	toolsetRequested := slices.Contains(a.enabledToolsets, "memory") && !slices.Contains(a.disabledToolsets, "memory")
	var memConfig map[string]any
	if !skipMemory || toolsetRequested {
		memConfig = cfgDict(agentCfg, "memory")
		a.memoryEnabled = isTruthyValue(memConfig["memory_enabled"], true)
		a.userProfileEnabled = isTruthyValue(memConfig["user_profile_enabled"], true)
		if nudge, ok := configIntOf(cfgValue(memConfig, "nudge_interval", 10)); ok {
			a.memoryNudgeInterval = nudge
			if a.memoryEnabled || a.userProfileEnabled {
				memLimit, mok := configIntOf(cfgValue(memConfig, "memory_char_limit", 2200))
				userLimit, uok := configIntOf(cfgValue(memConfig, "user_char_limit", 1375))
				if mok && uok {
					store := memory.NewStore(home.MemoriesDir(home.Dir(ctx)), memLimit, userLimit, a.memoryEnabled, a.userProfileEnabled)
					a.memoryStore = store
					_ = store.LoadFromDisk(tools.WithEnvHome(ctx, a.env))
				}
			}
		}
	}
	a.memoryManager = nil
	if manager != nil && !skipMemory {
		a.memoryManager = manager
	} else if !skipMemory {
		name := cfgText(memConfig["provider"])
		if !memory.IsCoreMemoryProvider(name) {
			mm := memory.NewMemoryManager(ctx, a.env, a.memoryStore)
			p, err := resolveMemoryProvider(ctx, a.env, name, a.emitStartupWarning)
			if err == nil && p != nil {
				err = mm.AddProvider(ctx, p)
			}
			switch {
			case err != nil:
				runAgentLog().Warn(fmt.Sprintf("Memory provider plugin init failed: %v", err))
			case len(mm.Providers()) > 0 && mm.InitializeForAgent(ctx, a.sessionID, memoryProviderInitOptions(ctx, a, platform), a.emitStartupWarning):
				a.memoryManager = mm
				runAgentLog().Info(fmt.Sprintf("Memory provider '%s' activated", name))
			default:
				runAgentLog().Debug(fmt.Sprintf("Memory provider '%s' not found or not available", name))
			}
		}
	}
	surface := make([]map[string]any, 0, len(a.tools))
	for _, t := range a.tools {
		if m, ok := t.(map[string]any); ok {
			surface = append(surface, m)
		}
	}
	injected, valid, _ := memory.InjectMemoryProviderTools(ctx, a.env, a.memoryManager, surface, a.validToolNames)
	if injected != nil {
		a.tools = make([]any, 0, len(injected))
		for _, t := range injected {
			a.tools = append(a.tools, t)
		}
		a.validToolNames = valid
	}
}

// Agent section knobs, guard settings, retry and recovery budgets
func applyAgentSection(a *AIAgent, agentCfg map[string]any) {
	a.skillNudgeInterval = 10
	if skills, ok := cfgValue(agentCfg, "skills", map[string]any{}).(map[string]any); ok {
		if n, nok := configIntOf(cfgValue(skills, "creation_nudge_interval", 10)); nok {
			a.skillNudgeInterval = n
		}
	}
	section := cfgDict(agentCfg, "agent")
	a.budgetWarningRatio, _ = NormalizeBudgetWarningRatio(section["budget_warning_ratio"])
	a.toolUseEnforcement = cfgValue(section, "tool_use_enforcement", "auto")
	a.executionGuidance = cfgValue(section, "execution_guidance", "auto")
	if a.runBudgetSeconds == 0 {
		if v := normalizeRunBudgetSeconds(section["run_budget_seconds"]); v != nil {
			a.runBudgetSeconds = *v
		}
	}
	guardEnabled, guardThreshold := resolveGuardSettings(section["empty_response_guard"])
	a.emptyGuardEnabled, a.emptyGuardCostThresholdUSD = &guardEnabled, &guardThreshold
	a.intentAckContinuation = cfgValue(section, "intent_ack_continuation", "auto")
	verbosity := strings.ToLower(strings.TrimSpace(cfgText(section["text_verbosity"])))
	if verbosity != "" && verbosity != "low" && verbosity != "medium" && verbosity != "high" {
		runAgentLog().Warn(fmt.Sprintf("Unknown agent.text_verbosity %s; expected low, medium or high — ignoring", pyrepr.Repr(verbosity)))
		verbosity = ""
	}
	a.textVerbosity = verbosity
	flag := func(key string) bool { return pyrepr.Truthy(cfgValue(section, key, true)) }
	a.stallGuards, a.taskCompletionGuidance = flag("stall_guards"), flag("task_completion_guidance")
	a.parallelToolCallGuidance, a.environmentProbe = flag("parallel_tool_call_guidance"), flag("environment_probe")
	a.botModeProtocol = flag("bot_mode_protocol")
	if a.environmentProbe {
		tools.WarmEnvironmentProbeAsync()
	}
	a.sessionTitleHint = ""
	a.platformHintOverrides = cfgDict(agentCfg, "platform_hints")
	a.apiMaxRetries = 3
	if n, ok := configIntOf(cfgValue(section, "api_max_retries", 3)); ok {
		a.apiMaxRetries = max(n, 1)
	}
	a.autoRecoveryCycles = 5
	if n, ok := configIntOf(cfgValue(section, "auto_recovery_cycles", 5)); ok {
		a.autoRecoveryCycles = max(n, 0)
	}
}
