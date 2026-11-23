package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
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
	c.toolProgressMode = settings.ToolProgress.Get(c.ctx)
	c.focusViewEnabled = false
	c.focusSavedToolProgress, c.focusLastCountedTool = "", ""
	c.focusHiddenLines = 0
	c.bellOnComplete = settings.BellOnComplete.Get(c.ctx)
	c.showReasoning = settings.ShowThinking.Get(settings.WithPlatform(c.ctx, settings.LocalPlatform))
	c.reasoningFull = settings.FullThinking.Get(c.ctx)
	c.modernLayout = layoutFromConfig(c.ctx)
	setLayoutActive(c.modernLayout)
	c.verbose = verbose
	c.streamingEnabled = settings.DisplayStreaming.Get(c.ctx)
	c.showTimestamps = settings.Timestamps.Get(c.ctx)
	c.timestampFormat = settings.DisplayTimestampFormat.Get(c.ctx)
	c.finalResponseMarkdown = settings.DisplayFinalResponseMarkdown.Get(c.ctx)
	c.inlineDiffsEnabled = settings.DisplayInlineDiffs.Get(c.ctx)
	c.streamBuf, c.reasoningPreviewBuf = "", ""
	c.streamStarted, c.streamBoxOpened, c.streamBoxLive = false, false, false
	c.streamedTextThisTurn = ""
	c.heldStatusLines = []string{}
	c.streamTableBuf = []string{}
	c.inStreamTable = false
	c.pendingEditSnapshots = map[string]*tools.LocalEditSnapshot{}
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
	configModel, nestedProvider := config.SplitModelDefault(settings.Model.Get(c.ctx))
	c.explicitModelOverride = model != ""
	c.model = model
	if c.model == "" {
		c.model = configModel
	}
	cfgProvider := settings.ModelProvider.Get(c.ctx)
	if cfgProvider == "" {
		cfgProvider = os.Getenv("ICEBATH_INFERENCE_PROVIDER")
	}
	startupProviderOverride, startupBaseURLOverride, startupAPIKeyOverride := "", "", ""
	if c.model != "" {
		current := firstNonEmpty(providerArg, nestedProvider, cfgProvider)
		userProviders := settings.Providers.Get(c.ctx)
		customProviders := provider.ProviderEntryList(c.config)
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
		localBase := settings.ModelBaseURL.Get(c.ctx)
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
	c.baseURL = firstNonEmpty(baseURL, startupBaseURLOverride, settings.ModelBaseURL.Get(c.ctx), os.Getenv("OPENROUTER_BASE_URL"))
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

// Resolves max turns and the run budget, each flag over its setting
func (c *CLI) initTurnLimits(maxTurns *int, runBudget *float64) {
	var raw any
	if maxTurns != nil {
		raw = *maxTurns
	} else if limit, set := settings.MaxTurns.Lookup(c.ctx); set {
		raw = limit
	}
	c.maxTurns = config.ResolveTurnLimit(raw, config.TurnLimitUnlimited)
	if runBudget != nil {
		v := *runBudget
		c.runBudgetSeconds = &v
	} else if budget := settings.AgentRunBudgetSeconds.Get(c.ctx); budget > 0 {
		seconds := budget.Seconds()
		c.runBudgetSeconds = &seconds
	}
	c.explicitRunBudget = runBudget
	c.explicitMaxTurns = maxTurns
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
	c.serviceTier = parseServiceTierConfig(settings.FastMode.Get(c.ctx))
	c.fallbackModel = provider.GetFallbackChain(c.config)
}

// Opens the session store and sets every per run mutable field, queues, overlays, pet, voice and status bar
func (c *CLI) initRuntimeState(resume string) {
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
