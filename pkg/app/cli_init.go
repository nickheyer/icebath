package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Constructor arguments of the CLI, command line args winning over config
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
	c.bellOnComplete = cliTruthy(display["bell_on_complete"])
	c.showReasoning = displayFlag(display, "show_reasoning", true)
	c.reasoningFull = cliTruthy(display["reasoning_full"])
	c.modernLayout = layoutFromConfig(display)
	setLayoutActive(c.modernLayout)
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
	c.streamBuf, c.reasoningPreviewBuf = "", ""
	c.streamStarted, c.streamBoxOpened, c.streamBoxLive = false, false, false
	c.streamedTextThisTurn = ""
	c.heldStatusLines = []string{}
	c.streamTableBuf = []string{}
	c.inStreamTable = false
	c.pendingEditSnapshots = map[string]*tools.LocalEditSnapshot{}
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
}

// Records the checkpoints and pass session id flags and the ignore rules opt in
func (c *CLI) initCheckpointsAndRules(checkpoints, passSessionID, ignoreRules bool) {
	c.explicitCheckpoints = checkpoints
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
	c.prefillMessages = loadPrefillMessages(c.ctx, resolvePrefillMessagesFile(c.ctx, c.config), "")
	c.reasoningConfig = config.ResolveReasoningConfig(c.config, c.model)
	c.explicitReasoningConfig = nil
	c.explicitReasoning = ""
	if strings.TrimSpace(reasoning) != "" {
		cliReasoning := parseReasoningConfig(reasoning)
		if cliReasoning == nil {
			pkgLog().Warn(fmt.Sprintf("Unknown --reasoning '%s', keeping the configured level", reasoning))
		} else {
			c.reasoningConfig = cliReasoning
			c.explicitReasoningConfig = cliReasoning
			c.explicitReasoning = strings.TrimSpace(reasoning)
		}
	}
	agentCfg := cliMap(c.config, "agent")
	c.serviceTier = parseServiceTierConfig(cliStr(agentCfg["service_tier"], ""))
	c.fallbackModel = provider.GetFallbackChain(c.config)
}

// Opens the session store and sets every per run mutable field, queues, overlays, pet, voice and status bar
func (c *CLI) initRuntimeState(resume string) {
	provider.ServeNoticesOnStderr()
	c.activeAgentRouteSignature = ""
	c.setAgent(nil)
	c.conversationHistory = []map[string]any{}
	c.sessionStart = cliNow()
	c.initSessionStore()
	c.resumed = resume != ""
	c.sessionID = resume
	if c.sessionID == "" {
		c.sessionID = newSessionID(c.sessionStart)
	}
	c.writeTerminalBreadcrumb()
	c.initUIState()
}

// Opens the gateway session store early so a title works before the first message, the gateway the single writer
func (c *CLI) initSessionStore() {
	c.sessionDB = gatewaycli.SessionStore()
}

// Mints main's local session id, replaced by the gateway's once the CLI opens its gateway session
func newSessionID(now time.Time) string { return state.NewSessionIDAt(now, 6) }

// Sets the per run UI state every chat call needs, since -q never goes through run
func (c *CLI) initUIState() {
	c.pendingInput = &inputQueue{}
	c.interruptQueue = &inputQueue{}
	c.lastTurnInterrupted = false
	c.pendingToolInfo = map[string][]map[string]any{}
	c.spinnerText = ""
	c.lastScrollbackTool = ""
	c.preloadedSkills = []string{}
	c.autoLoadSkillsResult = nil
	c.preloadSkillsDone = nil
	c.preloadSkillsResult = nil
	c.preloadSkillsError = nil
	c.preloadSkillsRequested = []string{}
	c.preloadSkillsFinalized = false
	c.activeSessionLease = nil
	c.statusBarSuppressedAfterResize.Store(false)
}
