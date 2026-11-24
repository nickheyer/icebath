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
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool definitions of the default registry, the model_tools.get_tool_definitions the tool snapshot reads
var loadToolDefinitions = func(ctx context.Context, env *tools.Env, o tools.DefinitionOptions) []map[string]any {
	return tools.Default.ToolDefinitions(ctx, env, o)
}

// Plugin discovery the tool snapshot runs first
var discoverPluginsAtLoad = func(ctx context.Context) { plugins.DiscoverPlugins(ctx, false) }

// Availability of every toolset's requirements, the model_tools.check_toolset_requirements the banner reads
var loadToolsetRequirements = func(ctx context.Context, env *tools.Env) map[string]bool {
	return tools.Default.CheckToolsetRequirements(ctx, env)
}

// Session tool surface with the disabled toolsets pinned, kanban guidance and the startup tool banner
func loadTools(ctx context.Context, a *AIAgent, enabled, disabled []string) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				pkgLog().Warn(fmt.Sprintf("Plugin discovery failed during agent setup: %v", r))
			}
		}()
		discoverPluginsAtLoad(ctx)
	}()
	generation := tools.Default.Generation()
	a.toolSnapshotGeneration = &generation
	a.setDelegationBackend(tools.SavedDelegationBackend(ctx))
	a.configuredDisabledToolsets = disabled
	disabled = tools.SessionDisabledToolsets(disabled, a.agentIsOneshot(ctx))
	a.disabledToolsets = disabled
	selection := tools.ToolSelectionKey(enabled, disabled)
	a.toolSnapshotSelection = &selection
	defs := a.toolSurface(ctx, loadToolDefinitions(ctx, a.env, tools.DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: a.quietMode}))
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
		for name, available := range loadToolsetRequirements(ctx, a.env) {
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
		Enabled: o.CheckpointsEnabled, MaxSnapshots: *o.CheckpointMaxSnapshots,
		MaxTotalSizeMB: *o.CheckpointMaxTotalSizeMB, MaxFileSizeMB: *o.CheckpointMaxFileSizeMB,
	})
	a.sessionDB = sessionStoreOrNil(o.SessionDB)
	a.parentSessionID = o.ParentSessionID
	var reasoning, maxTokens any
	if o.ReasoningConfig != nil {
		reasoning = o.ReasoningConfig
	}
	if o.MaxTokens != nil {
		maxTokens = *o.MaxTokens
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
	a.setSessionMessages([]*state.Message{})
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

// Commentary display, fast window, LM Studio load mode, streaming switch and tool loop guardrails in effect for ctx
func applyDisplayConfig(ctx context.Context, a *AIAgent, platform string) {
	a.showCommentary = settings.DisplayShowCommentary.Get(ctx)
	a.fastAutoSeconds = settings.AgentFastAutoSeconds.Get(ctx).Seconds()
	a.lmstudioLoadMode = settings.ModelLMStudioLoadMode.Get(ctx)
	a.RetrySession().DisableStreaming = !settings.ModelStreaming.Get(ctx)
	a.stream5xxProbeTS = 0
	cfg := tools.ToolCallGuardrailConfigFor(ctx, platform)
	a.toolGuardrails = tools.NewToolCallGuardrailController(&cfg)
}

// Memory manager scoping options, the CLI warn and status callbacks, title and gateway identity
func memoryProviderInitOptions(ctx context.Context, a *AIAgent, platform string) memory.InitOptions {
	if platform == "" {
		platform = "cli"
	}
	o := memory.InitOptions{Home: home.Dir(ctx), Platform: platform, AgentContext: "primary"}
	if a.memoryAgentContext != "" {
		o.AgentContext = a.memoryAgentContext
	} else if platform == "cron" || platform == "subagent" {
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
func initMemory(ctx context.Context, a *AIAgent, skipMemory bool, platform string, manager *memory.MemoryManager) {
	a.memoryStore = nil
	a.memoryEnabled, a.userProfileEnabled = false, false
	a.memoryNudgeInterval, a.turnsSinceMemory, a.itersSinceSkill = settings.MemoryNudgeInterval.Default, 0, 0
	if config.SafeWorkerEnabled() {
		a.memoryManager = nil
		return
	}
	toolsetRequested := slices.Contains(a.enabledToolsets, "memory") && !slices.Contains(a.disabledToolsets, "memory")
	if !skipMemory || toolsetRequested {
		a.memoryEnabled, a.userProfileEnabled = settings.Memory.Get(ctx), settings.LearnAboutMe.Get(ctx)
		if a.memoryAgentContext == "subagent" && !toolsetRequested {
			a.memoryEnabled, a.userProfileEnabled = false, false
		}
		a.memoryNudgeInterval = settings.MemoryNudgeInterval.Get(ctx)
		if a.memoryEnabled || a.userProfileEnabled {
			store := memory.NewStore(home.MemoriesDir(home.Dir(ctx)), settings.MemoryMemoryCharLimit.Get(ctx), settings.MemoryUserCharLimit.Get(ctx), a.memoryEnabled, a.userProfileEnabled)
			a.memoryStore = store
			if err := store.LoadFromDisk(tools.WithEnvHome(ctx, a.env)); err != nil {
				pkgLog().Warn(fmt.Sprintf("Built-in memory could not be read from disk: %v", err))
			}
		}
	}
	a.memoryManager = nil
	if manager != nil && !skipMemory {
		a.memoryManager = manager
	} else if !skipMemory {
		name := strings.TrimSpace(settings.MemoryProvider.Get(ctx))
		if !memory.IsCoreMemoryProvider(name) {
			mm := memory.NewMemoryManager(ctx, a.env, a.memoryStore)
			p, err := resolveMemoryProvider(ctx, a.env, name, a.emitStartupWarning)
			if err == nil && p != nil {
				err = mm.AddProvider(ctx, p)
			}
			switch {
			case err != nil:
				pkgLog().Warn(fmt.Sprintf("Memory provider plugin init failed: %v", err))
			case len(mm.Providers()) > 0 && mm.InitializeForAgent(ctx, a.sessionID, memoryProviderInitOptions(ctx, a, platform), a.emitStartupWarning):
				a.memoryManager = mm
				pkgLog().Info(fmt.Sprintf("Memory provider '%s' activated", name))
			default:
				pkgLog().Debug(fmt.Sprintf("Memory provider '%s' not found or not available", name))
			}
		}
	}
	surface := make([]map[string]any, 0, len(a.tools))
	for _, t := range a.tools {
		if m, ok := t.(map[string]any); ok {
			surface = append(surface, m)
		}
	}
	injected, valid, _ := memory.InjectMemoryProviderTools(ctx, a.toolsetScopedEnv(), a.memoryManager, surface, a.validToolNames)
	if injected != nil {
		a.tools = make([]any, 0, len(injected))
		for _, t := range injected {
			a.tools = append(a.tools, t)
		}
		a.validToolNames = valid
	}
}

// Layered value of a setting that takes a word or a list of model name parts, which its readers decode themselves
func wordOrModelList(ctx context.Context, e settings.Entry) any {
	v, err := settings.Resolve(ctx, e.Info().Key)
	if err != nil || v.Value == nil {
		return e.DefaultValue()
	}
	return v.Value
}

// Agent section knobs, guard settings, retry and recovery budgets in effect for ctx
func applyAgentSection(ctx context.Context, a *AIAgent) {
	a.skillNudgeInterval = settings.SkillsCreationNudgeInterval.Get(ctx)
	a.budgetWarningRatio = 0
	if ratio, ok := settings.AgentBudgetWarningRatio.Lookup(ctx); ok {
		a.budgetWarningRatio, _ = NormalizeBudgetWarningRatio(ratio)
	}
	a.toolUseEnforcement = wordOrModelList(ctx, settings.AgentToolUseEnforcement)
	a.executionGuidance = wordOrModelList(ctx, settings.AgentExecutionGuidance)
	if a.runBudgetSeconds == 0 {
		if budget, ok := settings.AgentRunBudgetSeconds.Lookup(ctx); ok && budget > 0 {
			a.runBudgetSeconds = budget.Seconds()
		}
	}
	guardEnabled, guardThreshold := resolveGuardSettings(ctx)
	a.emptyGuardEnabled, a.emptyGuardCostThresholdUSD = &guardEnabled, &guardThreshold
	a.intentAckContinuation = wordOrModelList(ctx, settings.AgentIntentAckContinuation)
	a.textVerbosity = settings.AgentTextVerbosity.Get(ctx)
	a.stallGuards, a.taskCompletionGuidance = settings.AgentStallGuards.Get(ctx), settings.AgentTaskCompletionGuidance.Get(ctx)
	a.parallelToolCallGuidance, a.environmentProbe = settings.AgentParallelToolCallGuidance.Get(ctx), settings.AgentEnvironmentProbe.Get(ctx)
	a.botModeProtocol = settings.AgentBotModeProtocol.Get(ctx)
	if a.environmentProbe {
		tools.WarmEnvironmentProbeAsync()
	}
	a.sessionTitleHint = ""
	a.platformHintOverrides = settings.PlatformHints.Get(ctx)
	a.apiMaxRetries = max(settings.AgentApiMaxRetries.Get(ctx), 1)
	a.autoRecoveryCycles = max(settings.AgentAutoRecoveryCycles.Get(ctx), 0)
}
