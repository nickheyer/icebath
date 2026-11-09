package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CLIOptions carries the HermesCLI constructor arguments, CLI args winning over config
type CLIOptions struct {
	Model         string
	Toolsets      []string
	Provider      string
	Reasoning     string
	APIKey        string
	BaseURL       string
	MaxTurns      *int
	RunBudget     *float64
	Verbose       bool
	Compact       *bool
	Resume        string
	Checkpoints   bool
	PassSessionID bool
	IgnoreRules   bool
}

// Reads display config for compact, tool progress, focus view, bells, streaming, previews and stream buffers
func (c *CLI) initDisplayOptions(verbose bool, compact *bool) {
	c.console = console()
	c.config = CLIConfig(c.ctx)
	display := config.LocalDisplayConfig(c.config["display"])
	if compact != nil {
		c.compact = *compact
	} else {
		c.compact = cliTruthy(display["compact"])
	}
	rawTP, ok := display["tool_progress"]
	if !ok {
		rawTP = "all"
	}
	if rawTP == false {
		c.toolProgressMode = "off"
	} else {
		c.toolProgressMode = cliStr(rawTP, "")
	}
	c.focusViewEnabled = cliTruthy(display["focus_view"])
	c.focusSavedToolProgress, c.focusLastCountedTool = "", ""
	c.focusHiddenLines = 0
	if c.focusViewEnabled {
		c.focusSavedToolProgress = normalizeToolProgressMode(c.toolProgressMode, "all")
		c.toolProgressMode = FocusToolProgressMode
	}
	c.resumeDisplay = cliStr(display["resume_display"], "full")
	c.bellOnComplete = cliTruthy(display["bell_on_complete"])
	c.bellOnPrompt = cliTruthy(display["bell_on_prompt"])
	c.showReasoning = displayFlag(display, "show_reasoning", true)
	c.reasoningFull = cliTruthy(display["reasoning_full"])
	maxLines, ok := display["persistent_output_max_lines"]
	if !ok {
		maxLines = 200
	}
	configureOutputHistory(displayFlag(display, "persistent_output", true), maxLines)
	c.modernLayout = layoutFromConfig(display)
	setLayoutActive(c.modernLayout)
	bim := busyInputModeFromConfig(display)
	if bim == "queue" || bim == "steer" {
		c.busyInputMode = bim
	} else {
		c.busyInputMode = "interrupt"
	}
	c.verbose = verbose
	c.streamingEnabled = cliTruthy(display["streaming"])
	c.showTimestamps = cliTruthy(display["timestamps"])
	c.timestampFormat = cliStr(display["timestamp_format"], "%H:%M")
	frm := strings.ToLower(strings.TrimSpace(cliStr(display["final_response_markdown"], "strip")))
	if frm == "render" || frm == "strip" || frm == "raw" {
		c.finalResponseMarkdown = frm
	} else {
		c.finalResponseMarkdown = "strip"
	}
	c.inlineDiffsEnabled = displayFlag(display, "inline_diffs", true)
	c.turnSummaryEnabled = displayFlag(display, "turn_summary", true)
	c.spinnerTokenFlowEnabled = displayFlag(display, "spinner_token_flow", true)
	c.turnSummaryCollector = nil
	c.turnSummaryStart = 0
	c.turnTokenBaseline = 0
	c.interactiveTurn = false
	ump, _ := display["user_message_preview"].(map[string]any)
	c.userMessagePreviewFirstLines = max(1, intOr(mapValueOr(ump, "first_lines", 2), 2))
	c.userMessagePreviewLastLines = max(0, intOr(mapValueOr(ump, "last_lines", 2), 2))
	c.streamBuf, c.reasoningPreviewBuf = "", ""
	c.streamStarted, c.streamBoxOpened, c.streamBoxLive = false, false, false
	c.streamedTextThisTurn = ""
	c.heldStatusLines = []string{}
	c.streamTableBuf = []string{}
	c.inStreamTable = false
	c.pendingEditSnapshots = map[string]any{}
	c.lastInputModeRecovery, c.lastTermiosDriftCheck = nil, nil
	c.inputModeRecoveryNoticeShown, c.termiosDriftNoticeShown = false, false
}

// Returns a display flag's truthiness, the default when the key is absent
func displayFlag(display map[string]any, key string, def bool) bool {
	if v, ok := display[key]; ok {
		return cliTruthy(v)
	}
	return def
}

// Resolves model, provider, base URL, turn limits, toolsets, checkpoints, prompt and reasoning
func (c *CLI) initModelRouting(o CLIOptions) error {
	if err := c.initModelAndProvider(o.Model, o.Provider, o.APIKey, o.BaseURL); err != nil {
		return err
	}
	c.initTurnLimits(o.MaxTurns, o.RunBudget)
	c.initToolsets(o.Toolsets)
	c.initCheckpointsAndRules(o.Checkpoints, o.PassSessionID, o.IgnoreRules)
	c.initPromptAndReasoning(o.Reasoning)
	return nil
}

// Picks the model route with CLI args over env vars over the config file
func (c *CLI) initModelAndProvider(model, providerArg, apiKey, baseURL string) error {
	modelConfig := cliMap(c.config, "model")
	rawDefault := modelConfig["default"]
	if !cliTruthy(rawDefault) {
		rawDefault = modelConfig["model"]
	}
	if !cliTruthy(rawDefault) {
		rawDefault = ""
	}
	configModel, nestedProvider := config.SplitModelDefault(rawDefault)
	c.explicitModelOverride = model != ""
	c.model = model
	if c.model == "" {
		c.model = configModel
	}
	cfgProvider := cliStr(modelConfig["provider"], "")
	if cfgProvider == "" {
		cfgProvider = os.Getenv("ICEBATH_INFERENCE_PROVIDER")
	}
	startupProviderOverride, startupBaseURLOverride, startupAPIKeyOverride := "", "", ""
	if c.model != "" {
		current := firstNonEmpty(providerArg, nestedProvider, cfgProvider)
		userProviders, _ := c.config["providers"].(map[string]any)
		customProviders, _ := c.config["custom_providers"].([]any)
		route, err := provider.ResolveStartupModelRoute(c.ctx, c.model, provider.StartupRouteOptions{
			ExplicitProvider: providerArg, CurrentProvider: current, UserProviders: userProviders, CustomProviders: customProviders})
		if err != nil {
			return err
		}
		if route != nil {
			c.model = route.Model
			startupProviderOverride = route.Provider
			startupBaseURLOverride = route.BaseURL
			startupAPIKeyOverride = route.APIKey
		}
	}
	moaProviderOverride, normalized := normalizeMoaModel(c.model)
	c.model = normalized
	if c.model == "" {
		localBase := cliStr(modelConfig["base_url"], "")
		if host, err := pyurl.BaseURLHostname(localBase); err == nil && (host == "localhost" || host == "127.0.0.1") {
			if detected := provider.AutoDetectLocalModel(c.ctx, localBase); detected != "" {
				c.model = detected
			}
		}
	}
	c.modelIsDefault = model == "" && configModel == ""
	c.explicitAPIKey = firstNonEmpty(apiKey, startupAPIKeyOverride)
	c.typedAPIKey = apiKey
	c.explicitBaseURL = firstNonEmpty(baseURL, startupBaseURLOverride)
	c.requestedProvider = firstNonEmpty(moaProviderOverride, providerArg, startupProviderOverride, nestedProvider, cfgProvider, "auto")
	c.explicitProvider = firstNonEmpty(moaProviderOverride, providerArg, startupProviderOverride)
	if model == "" && providerArg != "" {
		namedCustom := provider.GetNamedCustomProvider(c.ctx, providerArg)
		if providerDefault := strings.TrimSpace(cliStr(namedCustom["model"], "")); providerDefault != "" {
			c.model = providerDefault
			c.modelIsDefault = false
		}
	}
	c.providerSource = ""
	c.provider = c.requestedProvider
	c.apiMode = "chat_completions"
	c.acpCommand = ""
	c.acpArgs = []string{}
	c.baseURL = firstNonEmpty(baseURL, startupBaseURLOverride, cliStr(modelConfig["base_url"], ""), os.Getenv("OPENROUTER_BASE_URL"))
	keys := []string{os.Getenv("OPENROUTER_API_KEY"), os.Getenv("OPENAI_API_KEY")}
	matches := false
	if c.baseURL != "" {
		matches, _ = pyurl.BaseURLHostMatches(c.baseURL, "openrouter.ai")
	}
	if !matches {
		keys[0], keys[1] = keys[1], keys[0]
	} else if !provider.LooksLikeOpenRouterKey(keys[1]) {
		keys[1] = ""
	}
	c.apiKey = firstNonEmpty(apiKey, keys[0], keys[1])
	return nil
}

// Returns the first non empty string of its arguments
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Resolves max turns as CLI arg over config over env var and the run budget as flag over config
func (c *CLI) initTurnLimits(maxTurns *int, runBudget *float64) {
	agentCfg := cliMap(c.config, "agent")
	var raw any
	switch {
	case maxTurns != nil:
		raw = *maxTurns
	case agentCfg["max_turns"] != nil:
		raw = agentCfg["max_turns"]
	case c.config["max_turns"] != nil:
		raw = c.config["max_turns"]
	default:
		raw = envOrNil("ICEBATH_MAX_ITERATIONS")
	}
	c.maxTurns = config.ResolveTurnLimit(raw, config.TurnLimitUnlimited)
	if runBudget != nil {
		v := *runBudget
		c.runBudgetSeconds = &v
	} else {
		c.runBudgetSeconds = floatPtrOf(agentCfg["run_budget_seconds"])
	}
	c.explicitRunBudget = runBudget
	c.explicitMaxTurns = maxTurns
}

// Returns an environment value, nil when the variable is unset
func envOrNil(name string) any {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return nil
}

// Converts a numeric config value to a float pointer, nil for anything else
func floatPtrOf(v any) *float64 {
	var f float64
	switch t := v.(type) {
	case int:
		f = float64(t)
	case int64:
		f = float64(t)
	case float64:
		f = t
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return nil
		}
		f = parsed
	default:
		return nil
	}
	return &f
}

// Records the typed toolsets and the configured disabled toolsets
func (c *CLI) initToolsets(toolsets []string) {
	c.enabledToolsets = toolsets
	c.explicitToolsets = toolsets != nil
	c.disabledToolsets = tools.ParseConfigStringList(cliMap(c.config, "agent")["disabled_toolsets"])
}

// Reads checkpoint settings, the pass session id flag and the ignore rules opt in
func (c *CLI) initCheckpointsAndRules(checkpoints, passSessionID, ignoreRules bool) {
	cpCfg := map[string]any{}
	switch t := c.config["checkpoints"].(type) {
	case bool:
		cpCfg = map[string]any{"enabled": t}
	case map[string]any:
		cpCfg = t
	}
	c.checkpointsEnabled = checkpoints || cliTruthy(cpCfg["enabled"])
	c.explicitCheckpoints = checkpoints
	c.checkpointMaxSnapshots = intOr(mapValueOr(cpCfg, "max_snapshots", 20), 20)
	c.checkpointMaxTotalSizeMB = intOr(mapValueOr(cpCfg, "max_total_size_mb", 500), 500)
	c.checkpointMaxFileSizeMB = intOr(mapValueOr(cpCfg, "max_file_size_mb", 10), 10)
	c.passSessionID = passSessionID
	c.ignoreRules = ignoreRules || provider.IsTruthyValue(os.Getenv("ICEBATH_IGNORE_RULES"))
	c.explicitIgnoreRules = ignoreRules
}

// Resolves the ephemeral prompt, prefill, reasoning, service tier, routing knobs and fallback chain
func (c *CLI) initPromptAndReasoning(reasoning string) {
	c.systemPrompt = os.Getenv("ICEBATH_EPHEMERAL_SYSTEM_PROMPT")
	if c.systemPrompt == "" {
		c.systemPrompt = commands.ResolveEphemeralSystemPrompt(c.config)
	}
	c.personalities = commands.AvailablePersonalities(config.Tree(c.config))
	c.prefillMessages = loadPrefillMessages(c.ctx, resolvePrefillMessagesFile(c.ctx, c.config), "")
	c.reasoningConfig = config.ResolveReasoningConfig(c.config, c.model)
	c.explicitReasoningConfig = nil
	c.explicitReasoning = ""
	if strings.TrimSpace(reasoning) != "" {
		cliReasoning := parseReasoningConfig(reasoning)
		if cliReasoning == nil {
			cliLogger.Warn(fmt.Sprintf("Unknown --reasoning '%s', keeping the configured level", reasoning))
		} else {
			c.reasoningConfig = cliReasoning
			c.explicitReasoningConfig = cliReasoning
			c.explicitReasoning = strings.TrimSpace(reasoning)
		}
	}
	agentCfg := cliMap(c.config, "agent")
	c.serviceTier = parseServiceTierConfig(cliStr(agentCfg["service_tier"], ""))
	pr := cliMap(c.config, "provider_routing")
	c.providerSort = pr["sort"]
	c.providersOnly = pr["only"]
	c.providersIgnore = pr["ignore"]
	c.providersOrder = pr["order"]
	c.providerRequireParams = cliTruthy(pr["require_parameters"])
	c.providerDataCollection = pr["data_collection"]
	c.openRouterMinCodingScore = nil
	if rawScore := cliMap(c.config, "openrouter")["min_coding_score"]; rawScore != nil && rawScore != "" {
		if f := floatPtrOf(rawScore); f != nil && *f >= 0 && *f <= 1 {
			c.openRouterMinCodingScore = f
		}
	}
	c.fallbackModel = provider.GetFallbackChain(c.config)
}

// Opens the session store and sets every per run mutable field, queues, overlays, pet, voice and status bar
func (c *CLI) initRuntimeState(resume string) {
	provider.ServeNoticesOnStderr()
	c.activeAgentRouteSignature = ""
	c.agent = nil
	c.toolCallbacksInstalled = false
	c.app = nil
	c.conversationHistory = []map[string]any{}
	c.sessionStart = time.Now()
	c.promptStartTime = nil
	c.promptDuration = 0
	c.lastTurnFinishedAt = nil
	c.initSessionStore()
	c.pendingTitle = ""
	c.resumed = resume != ""
	c.sessionID = resume
	if c.sessionID == "" {
		c.sessionID = newSessionID(c.sessionStart)
	}
	c.writeTerminalBreadcrumb()
	c.historyFile = filepath.Join(home.Dir(c.ctx), ".icebath_history")
	c.lastInvalidate = nil
	c.initUIState()
}

// Opens state.db early so /title works before the first message, surfacing a failed store prominently
func (c *CLI) initSessionStore() {
	c.sessionDB = nil
	c.sessionDBUnavailable = false
	db, err := state.NewSessionDB(c.ctx, filepath.Join(home.Dir(c.ctx), "state.db"), false)
	if err == nil {
		c.sessionDB = db
		return
	}
	c.sessionDBUnavailable = true
	cliLogger.Warn(fmt.Sprintf("Failed to initialize SessionDB — session will NOT be indexed for search: %v", err))
	failure := state.DescribeStorageFailure(c.ctx, err)
	present := func() error {
		rich := i18n.T(c.ctx, "cli.session_store.unavailable_rich", "", map[string]any{
			"reason": failure.Gloss, "action": failure.Action, "details": state.StorageFailureDetails(err, 200)})
		if _, werr := fmt.Fprintln(stderrWriter(), pyrich.Render(rich, c.console.ShouldUseColor())); werr != nil {
			fmt.Fprintln(c.console.Out, i18n.T(c.ctx, "cli.session_store.unavailable_plain", "", map[string]any{
				"reason": failure.Gloss, "action": failure.Action}))
		}
		return nil
	}
	_, _ = config.RenderNotification(c.ctx, present, "cli", true, nil)
}

// Mints the local session id the Go CLI owns, its store having no gateway facade
func newSessionID(now time.Time) string { return state.NewSessionIDAt(now, 6) }

// Mints the id for /new and /branch, main's local mint on this CLI's own store
func (c *CLI) newSessionID(now time.Time) string { return newSessionID(now) }

// Sets the per run UI state every chat call needs, since -q never goes through run
func (c *CLI) initUIState() {
	c.pendingInput = &inputQueue{}
	c.interruptQueue = &inputQueue{}
	c.agentRunning, c.shouldExit = false, false
	c.detachOnExit = false
	c.lastTurnInterrupted = false
	c.terminalIOBroken = false
	c.deleteSessionOnExit = false
	c.pendingRelaunch = nil
	c.lastCtrlCTime = 0
	c.clarifyState, c.clarifyMultiBase = nil, nil
	c.clarifyFreetext = false
	c.clarifyPrefill = ""
	c.sudoState, c.modalInputSnapshot, c.approvalState = nil, nil, nil
	c.slashConfirmState, c.modelPickerState = nil, nil
	c.clarifyDeadline, c.sudoDeadline, c.approvalDeadline, c.slashConfirmDeadline = 0, 0, 0, 0
	c.composerPlaceholder = getRandomComposerHint(c.ctx)
	c.commandPaletteState, c.secretState = nil, nil
	c.pendingResumeSessions = nil
	c.pendingAgentSeed = nil
	c.secretDeadline = 0
	c.toolStartTime = 0
	c.pendingToolInfo = map[string][][2]string{}
	c.spinnerText, c.commandStatus = "", ""
	c.lastScrollbackTool = ""
	c.commandRunning, c.commandBlocksInput = false, false
	c.petRenderer = nil
	c.petAnimStop = nil
	c.petSlug, c.petKittyPending = "", ""
	c.petEnabled, c.petAnimRunning = false, false
	c.petCols = 18
	c.petScale = 0.7
	c.petFramesCache = map[string]any{}
	c.petKittyCache = map[string]any{}
	c.petKittyImageID, c.petFrameIdx = 0, 0
	c.petCfgChecked, c.petEventUntil = 0, 0
	c.petEvent = ""
	c.petReasoning, c.petTurnError = false, false
	c.attachedImages = []string{}
	c.imageCounter = 0
	c.promptStash = config.NewPromptStash()
	c.preloadedSkills = []string{}
	c.startupSkillsLineShown = false
	c.autoLoadSkillsResult = nil
	c.preloadSkillsDone = nil
	c.preloadSkillsResult = nil
	c.preloadSkillsError = nil
	c.preloadSkillsRequested = []string{}
	c.preloadSkillsFinalized = false
	c.activeSessionLease = nil
	c.voiceMode, c.voiceTTS, c.voiceRecording = false, false, false
	c.voiceProcessing, c.voiceContinuous = false, false
	c.voiceRecorder, c.voiceTTSStop = nil, nil
	c.voiceLiveText = ""
	c.voiceTTSDone = newSyncEvent()
	c.voiceTTSDone.Set()
	c.voiceBargeCapture = newSyncEvent()
	c.voiceLastTTSText = ""
	c.voiceBargePhase = ""
	c.statusBarVisible = statusBarVisibleFromDisplayConfig(c.config["display"])
	display := cliMap(c.config, "display")
	c.batteryVisible = cliTruthy(display["battery"])
	c.vimMode = cliTruthy(display["vim_mode"])
	c.statusBarSuppressedAfterResize, c.resizeRecoveryPending = false, false
	c.resizeRecoveryTimer, c.statusBarUnsuppressTimer = nil, nil
	c.lastResizeWidth = nil
	c.backgroundTasks = map[string]chan struct{}{}
	c.backgroundTaskCounter = 0
	c.cacheHitBaselinePrompt, c.cacheHitBaselineRead, c.cacheHitBaselineCompressions = 0, 0, 0
	c.cacheHitBaselineModel = ""
}
