package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysignal"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Toolset names that enable every toolset
var oneshotAllToolsets = map[string]bool{"all": true, "*": true}

// Logger of the one shot module
var oneshotLogger = slog.Default().With("logger", "hermes_cli.oneshot")

// Repeated or comma separated toolset flags as a clean list, nil when nothing remains
func normalizeOneshotToolsets(toolsets []string) []string {
	if out := splitCommaItems(toolsets); len(out) > 0 {
		return out
	}
	return nil
}

// Enabled and disabled MCP server names of the raw config, both empty when the config cannot be read
func oneshotConfiguredMCPServers(ctx context.Context) (enabled, disabled map[string]bool) {
	enabled, disabled = map[string]bool{}, map[string]bool{}
	cfg, err := config.ReadRawConfig(ctx, home.Dir(ctx))
	if err != nil {
		return map[string]bool{}, map[string]bool{}
	}
	servers, _ := oneshotMap(cfg["mcp_servers"])
	for name, serverCfg := range servers {
		entry, ok := oneshotMap(serverCfg)
		if !ok {
			continue
		}
		if tools.MCPServerEnabled(entry) {
			enabled[name] = true
		} else {
			disabled[name] = true
		}
	}
	return enabled, disabled
}

// Map form of a config section, false when the value is no mapping
func oneshotMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return map[string]any(m), true
	}
	return nil, false
}

// Plugin discovery the toolset validation runs before its second pass
var oneshotDiscoverPlugins = func(ctx context.Context) { plugins.DiscoverPlugins(ctx, false) }

// Valid explicit toolsets, warning on stderr about ignored entries and returning an error line when none are valid
func validateOneshotToolsets(ctx context.Context, toolsets []string, stderr io.Writer) ([]string, string) {
	normalized := normalizeOneshotToolsets(toolsets)
	if normalized == nil {
		return nil, ""
	}
	scope := tools.ScopeKey(home.Dir(ctx))
	validate := func(name string) bool { return tools.Default.ValidateToolset(ctx, scope, name) }
	var builtIn, unresolved []string
	for _, name := range normalized {
		if validate(name) {
			builtIn = append(builtIn, name)
		}
	}
	for _, name := range normalized {
		if !slices.Contains(builtIn, name) {
			unresolved = append(unresolved, name)
		}
	}
	if len(unresolved) > 0 {
		pluginValid := oneshotPluginToolsets(ctx, unresolved, validate)
		builtIn = append(builtIn, pluginValid...)
		var still []string
		for _, name := range unresolved {
			if !slices.Contains(pluginValid, name) {
				still = append(still, name)
			}
		}
		unresolved = still
	}
	for _, name := range builtIn {
		if !oneshotAllToolsets[name] {
			continue
		}
		var ignored []string
		for _, n := range normalized {
			if !oneshotAllToolsets[n] {
				ignored = append(ignored, n)
			}
		}
		if len(ignored) > 0 {
			fmt.Fprintf(stderr, "icebath -z: --toolsets all enables every toolset; ignoring additional entries: %s\n", strings.Join(ignored, ", "))
		}
		return nil, ""
	}
	mcpNames, mcpDisabled := map[string]bool{}, map[string]bool{}
	if len(unresolved) > 0 {
		mcpNames, mcpDisabled = oneshotConfiguredMCPServers(ctx)
	}
	var mcpValid, disabled, unknown []string
	for _, name := range unresolved {
		switch {
		case mcpNames[name]:
			mcpValid = append(mcpValid, name)
		case mcpDisabled[name]:
			disabled = append(disabled, name)
		default:
			unknown = append(unknown, name)
		}
	}
	valid := append(builtIn, mcpValid...)
	if len(unknown) > 0 {
		fmt.Fprintf(stderr, "icebath -z: ignoring unknown --toolsets entries: %s\n", strings.Join(unknown, ", "))
	}
	if len(disabled) > 0 {
		fmt.Fprintf(stderr, "icebath -z: ignoring disabled MCP servers (set enabled: true in config.yaml to use): %s\n", strings.Join(disabled, ", "))
	}
	if len(valid) == 0 {
		return nil, "icebath -z: --toolsets did not contain any valid toolsets.\n"
	}
	return valid, ""
}

// Unresolved names a plugin toolset answers after discovery, none when discovery fails
func oneshotPluginToolsets(ctx context.Context, unresolved []string, validate func(string) bool) (valid []string) {
	defer func() {
		if recover() != nil {
			valid = nil
		}
	}()
	oneshotDiscoverPlugins(ctx)
	for _, name := range unresolved {
		if validate(name) {
			valid = append(valid, name)
		}
	}
	return valid
}

// Inputs of one one shot run, empty strings and nil slices standing for an absent flag
type OneshotOptions struct {
	Model, Provider, UsageFile, Resume, Reasoning string
	Toolsets, Skills                              []string
}

// Keywords of one in process agent turn
type oneshotRunOptions struct {
	Model, Provider, Resume, Reasoning string
	Toolsets, Skills                   []string
	UseConfigToolsets, Ledger          bool
}

// Python repr of a control flow failure that escapes the agent, empty when err is an ordinary failure
func oneshotControlFlowRepr(err error) string {
	var exit *pysignal.SystemExit
	switch {
	case errors.Is(err, pysignal.ErrKeyboardInterrupt), errors.Is(err, tools.ErrKeyboardInterrupt), errors.Is(err, errKeyboardInterrupt):
		return "KeyboardInterrupt()"
	case errors.As(err, &exit):
		return fmt.Sprintf("SystemExit(%d)", exit.Code)
	}
	return ""
}

// Reports whether a control flow failure RunOneshot re raises is a keyboard interrupt
func IsOneshotKeyboardInterrupt(err error) bool {
	return oneshotControlFlowRepr(err) == "KeyboardInterrupt()"
}

// Exit status a SystemExit failure RunOneshot re raises carries, not ok for any other failure
func OneshotSystemExitCode(err error) (int, bool) {
	var exit *pysignal.SystemExit
	if errors.As(err, &exit) {
		return exit.Code, true
	}
	return 0, false
}

// Runs one prompt and prints only the final reply, returning the exit code and any control flow failure to re raise
func RunOneshot(ctx context.Context, prompt string, o OneshotOptions) (int, error) {
	logging.Disable(logging.LevelCritical)
	realStdout, realStderr := os.Stdout, os.Stderr
	envModelEarly := pystr.Strip(os.Getenv("ICEBATH_INFERENCE_MODEL"))
	if o.Provider != "" && pystr.Strip(o.Model) == "" && envModelEarly == "" {
		fmt.Fprint(realStderr, "icebath -z: --provider requires --model (or ICEBATH_INFERENCE_MODEL). "+
			"Pass both explicitly, or neither to use your configured defaults.\n")
		return 2, nil
	}
	explicitToolsets, toolsetsError := validateOneshotToolsets(ctx, o.Toolsets, realStderr)
	if toolsetsError != "" {
		fmt.Fprint(realStderr, toolsetsError)
		return 2, nil
	}
	useConfigToolsets := normalizeOneshotToolsets(o.Toolsets) == nil
	_ = os.Setenv("ICEBATH_YOLO_MODE", "1")
	_ = os.Setenv("ICEBATH_ACCEPT_HOOKS", "1")
	_ = os.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	ctx, env := oneshotToolEnv(ctx)
	tools.DeclareStatelessChannel(env)
	response, result, failure := oneshotRedirected(func() (string, map[string]any, error) {
		return oneshotRunAgent(ctx, env, prompt, oneshotRunOptions{
			Model: o.Model, Provider: o.Provider, Toolsets: explicitToolsets, UseConfigToolsets: useConfigToolsets,
			Skills: o.Skills, Resume: o.Resume, Reasoning: o.Reasoning, Ledger: o.UsageFile != "",
		})
	})
	if result == nil {
		result = map[string]any{}
	}
	if failure != nil {
		if control := oneshotControlFlowRepr(failure); control != "" {
			oneshotWriteUsageFile(o.UsageFile, oneshotResultObject(result), control)
			return 0, failure
		}
		oneshotWriteUsageFile(o.UsageFile, oneshotResultObject(result), failure.Error())
		fmt.Fprintf(realStderr, "icebath -z: agent failed: %v\n", failure)
		return 1, nil
	}
	oneshotWriteUsageFile(o.UsageFile, oneshotResultObject(result), nil)
	if response != "" {
		response = state.SanitizeSurrogates(response)
		fmt.Fprint(realStdout, response)
		if !strings.HasSuffix(response, "\n") {
			fmt.Fprint(realStdout, "\n")
		}
	}
	exitCode := oneshotExitCode(response, result)
	if exitCode == 1 {
		fmt.Fprint(realStderr, "icebath -z: no final response was produced; treating the run as failed.\n")
	}
	return exitCode, nil
}

// Context and tool env of the run, a fresh env bound when the context carries none
func oneshotToolEnv(ctx context.Context) (context.Context, *tools.Env) {
	env := tools.ToolEnvFrom(ctx)
	if env == nil {
		cwd, _ := os.Getwd()
		env = &tools.Env{Home: home.Dir(ctx), Cwd: cwd}
		ctx = tools.WithToolEnv(ctx, env)
	}
	if env.Values == nil {
		env.Values = &sync.Map{}
	}
	return ctx, env
}

// Runs fn with stdout and stderr sent to the null device, a panic escaping fn returned as its failure
func oneshotRedirected(fn func() (string, map[string]any, error)) (response string, result map[string]any, failure error) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return "", nil, err
	}
	savedStdout, savedStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull
	defer func() {
		os.Stdout, os.Stderr = savedStdout, savedStderr
		_ = devnull.Close()
	}()
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				failure = e
			} else {
				failure = fmt.Errorf("%v", r)
			}
		}
	}()
	return fn()
}

// Ordered object form of a run result for the usage file
func oneshotResultObject(result map[string]any) *pyjson.Object {
	out := pyjson.NewObject()
	keys := make([]string, 0, len(result))
	for k := range result {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := result[k]
		if m, ok := v.(map[string]any); ok && k == "auxiliary_usage" {
			v = oneshotResultObject(m)
		}
		out.Set(k, v)
	}
	return out
}

// Shared session store the turn records into, nil when the store cannot open
var createOneshotSessionDB = func(ctx context.Context) *state.SessionDB {
	db, err := state.Acquire(ctx, "")
	if err != nil {
		oneshotLogger.Debug(fmt.Sprintf("SQLite session store not available for oneshot mode: %v", err))
		return nil
	}
	return db
}

// Effective model and route of a one shot turn, empty strings standing for None
type oneshotModelChoice struct {
	Model, Provider, BaseURL, APIKey, APIMode string
}

// Model name of the config model section, a mapping default split to its model
func oneshotConfiguredModel(modelCfg any) (string, error) {
	if s, ok := modelCfg.(string); ok {
		return s, nil
	}
	m, ok := oneshotMap(modelCfg)
	if !ok {
		return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(modelCfg))
	}
	raw := m["default"]
	if !pyrepr.Truthy(raw) {
		raw = m["model"]
	}
	if d, ok := oneshotMap(raw); ok {
		model := d["model"]
		if !pyrepr.Truthy(model) {
			model = d["default"]
		}
		if !pyrepr.Truthy(model) {
			return "", nil
		}
		return pystr.Strip(pyrepr.Str(model)), nil
	}
	if !pyrepr.Truthy(raw) {
		return "", nil
	}
	return pyrepr.Str(raw), nil
}

// Provider auto detection for a bare model name, the hermes_cli.models name hermes tests replace
var oneshotDetectProviderForModel = provider.DetectProviderForModel

// Model from arg, env then config and provider from arg, direct alias, startup route, auto detection then config
func resolveOneshotModelAndProvider(ctx context.Context, cfg map[string]any, model, prov string) (*oneshotModelChoice, error) {
	modelCfg := cfg["model"]
	if !pyrepr.Truthy(modelCfg) {
		modelCfg = map[string]any{}
	}
	explicitModel := pystr.Strip(model)
	if explicitModel == "" {
		explicitModel = pystr.Strip(os.Getenv("ICEBATH_INFERENCE_MODEL"))
	}
	choice := &oneshotModelChoice{Model: explicitModel, Provider: pystr.Strip(prov)}
	if explicitModel == "" {
		configured, err := oneshotConfiguredModel(modelCfg)
		if err != nil {
			return nil, err
		}
		choice.Model = configured
	}
	if choice.Provider != "" || explicitModel == "" {
		return choice, nil
	}
	provider.EnsureDirectAliases(ctx)
	direct, isAlias := provider.DirectAliases.Get(pystr.Lower(pystr.Strip(explicitModel)))
	if !isAlias {
		cfgProvider := ""
		if m, ok := oneshotMap(modelCfg); ok && pyrepr.Truthy(m["provider"]) {
			cfgProvider = pystr.Lower(pystr.Strip(pyrepr.Str(m["provider"])))
		}
		currentProvider := cfgProvider
		if currentProvider == "" {
			currentProvider = pystr.Lower(pystr.Strip(os.Getenv("ICEBATH_INFERENCE_PROVIDER")))
		}
		if currentProvider == "" {
			currentProvider = "auto"
		}
		userProviders, _ := oneshotMap(cfg["providers"])
		customProviders, _ := cfg["custom_providers"].([]any)
		route, err := provider.ResolveStartupModelRoute(ctx, explicitModel, provider.StartupRouteOptions{
			CurrentProvider: currentProvider, UserProviders: userProviders, CustomProviders: customProviders})
		if err != nil {
			return nil, err
		}
		if route != nil {
			choice.Provider, choice.Model = route.Provider, route.Model
			return choice, nil
		}
		if detectedProvider, detectedModel, ok := oneshotDetectProviderForModel(ctx, explicitModel, currentProvider); ok {
			choice.Provider, choice.Model = detectedProvider, detectedModel
		}
		return choice, nil
	}
	choice.Model, choice.Provider = direct.Model, direct.Provider
	if aliasProvider, aliasKey, err := provider.DirectAliasRuntimeRequest(ctx, direct); err == nil {
		choice.Provider, choice.APIKey = aliasProvider, aliasKey
	} else {
		choice.APIKey = ""
	}
	if direct.BaseURL != "" {
		choice.BaseURL = strings.TrimRight(direct.BaseURL, "/")
	}
	return choice, nil
}

// Resolved session id, model projection history and session row of the session a one shot turn continues
func loadOneshotResumeTarget(ctx context.Context, db *state.SessionDB, resume string) (string, []map[string]any, map[string]any, error) {
	if resume == "" {
		return "", []map[string]any{}, nil, nil
	}
	if db == nil {
		return "", nil, nil, fmt.Errorf("cannot resume session %s: session store unavailable", resume)
	}
	resolved, err := db.ResolveResumeSessionID(resume)
	if err != nil {
		return "", nil, nil, err
	}
	if resolved == "" {
		resolved = resume
	}
	row, err := db.GetSession(ctx, resolved)
	if err != nil {
		return "", nil, nil, err
	}
	if row == nil || len(row.Keys) == 0 {
		return "", nil, nil, fmt.Errorf("session not found: %s", resume)
	}
	if refusal := kanban.KanbanResumeRefusal(ctx, db, resolved); refusal != "" {
		return "", nil, nil, fmt.Errorf("cannot resume session %s: %s", resume, refusal)
	}
	if _, err := db.AssertResumeSafe(ctx, resolved, nil, true); err != nil {
		return "", nil, nil, err
	}
	restored, _, err := db.GetResumeConversations(ctx, resolved)
	if err != nil {
		return "", nil, nil, err
	}
	history := []map[string]any{}
	for _, m := range restored {
		if !pyrepr.Equal(m["role"], "session_meta") {
			history = append(history, m)
		}
	}
	if err := db.ReopenSession(ctx, resolved); err != nil {
		oneshotLogger.Debug(fmt.Sprintf("reopen_session failed for resumed one-shot session %s: %v", resolved, err))
	}
	meta, _ := pyjson.Plain(row).(map[string]any)
	return resolved, history, meta, nil
}

// Choice moved onto the resumed session's stored route unless the launch named an explicit model
func applyOneshotStoredSessionRuntime(ctx context.Context, choice *oneshotModelChoice, sessionMeta map[string]any, explicitModel bool) *oneshotModelChoice {
	if explicitModel {
		return choice
	}
	model, storedProvider, storedBaseURL, storedAPIMode, providerChanged, ok := storedSessionRoute(ctx, sessionMeta, choice.Model, choice.Provider)
	if !ok {
		return choice
	}
	choice.Model = model
	if providerChanged {
		choice.Provider, choice.BaseURL, choice.APIKey = storedProvider, storedBaseURL, ""
	}
	if storedAPIMode != "" {
		choice.APIMode = storedAPIMode
	}
	return choice
}

// Primary runtime through the CLI resolver, the fallback chain of cfg walked in order when the primary fails on credentials
func oneshotResolveRuntimeWithFallback(ctx context.Context, cfg map[string]any, requested, targetModel, explicitBaseURL, explicitAPIKey string) (*provider.Runtime, map[string]any, error) {
	h := home.Dir(ctx)
	rt, primaryErr := resolveRuntimeProviderFn(ctx, h, requested, targetModel, explicitBaseURL, explicitAPIKey)
	if primaryErr == nil {
		return rt, nil, nil
	}
	var primary *provider.AuthError
	if !errors.As(primaryErr, &primary) {
		return nil, nil, primaryErr
	}
	for _, entry := range provider.GetFallbackChain(cfg) {
		prov := pystr.Lower(pystr.Strip(cliStr(entry["provider"], "")))
		model := pystr.Strip(cliStr(entry["model"], ""))
		if prov == "" || model == "" {
			continue
		}
		baseURL := ""
		if pyrepr.Truthy(entry["base_url"]) {
			baseURL = cliStr(entry["base_url"], "")
		}
		runtime, err := resolveRuntimeProviderFn(ctx, h, prov, model, baseURL, provider.ResolveEntryAPIKey(ctx, entry))
		if err != nil {
			var authErr *provider.AuthError
			if errors.As(err, &authErr) {
				slog.Default().Debug(fmt.Sprintf("Fallback entry %s/%s failed: %v", prov, model, err))
			} else {
				slog.Default().Warn(fmt.Sprintf("Fallback entry %s/%s is misconfigured and was skipped: %v", prov, model, err))
			}
			continue
		}
		runtime.Provider = provider.EffectiveRuntimeProvider(entry, runtime)
		phrase, _ := provider.PrimaryFailureWording(primary)
		slog.Default().Warn(fmt.Sprintf("Primary provider %s (%v). Falling back to %s/%s", phrase, primary, prov, model))
		return runtime, entry, nil
	}
	return nil, nil, primaryErr
}

// Config the turn builds from, the hermes_cli.config.load_config hermes tests replace
var oneshotLoadConfig = func(ctx context.Context) (map[string]any, error) {
	cfg, err := config.Load(home.Dir(ctx), nil)
	return map[string]any(cfg), err
}

// Enabled toolsets of a platform, the hermes_cli.tools_config._get_platform_tools hermes tests replace
var oneshotPlatformTools = func(ctx context.Context, cfg map[string]any, platform string) []string {
	enabled := tools.Default.PlatformTools(ctx, tools.ScopeKey(home.Dir(ctx)), config.Tree(cfg), platform, tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
	out := make([]string, 0, len(enabled))
	for name, on := range enabled {
		if on {
			out = append(out, name)
		}
	}
	return out
}

// Bounded single query wait for MCP discovery before the agent snapshots its tools, the name hermes tests replace
var oneshotEnsureMCPDiscovery = func(ctx context.Context, env *tools.Env) {
	mcp.EnsureDiscoveryBeforeAgentBuild(ctx, env, oneshotLogger, nil, true, "cli-mcp-discovery")
}

// Agent surface a one shot turn drives, the AIAgent methods the run and its teardown call
type oneshotAgent interface {
	SetSuppressStatusOutput(on bool)
	SetStreamDeltaCallback(cb any)
	SetToolGenCallback(cb any)
	SessionID() string
	Platform() string
	RunConversation(ctx context.Context, userMessage string, o agent.RunConversationOptions) (map[string]any, error)
	SessionMessages() []*state.Message
	ShutdownMemoryProvider(ctx context.Context, messages []map[string]any)
	Close() error
}

// Agent constructor of a one shot turn, the run_agent.AIAgent hermes tests replace
var newOneshotAgent = func(ctx context.Context, o agent.AIAgentOptions) (oneshotAgent, error) {
	a, err := agent.NewAIAgent(ctx, o)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// Preloaded skills prompt of the requested skills, failing only when every requested skill is missing
func oneshotPreloadedSkillsPrompt(ctx context.Context, requested []string) (string, error) {
	var parsed []string
	for _, name := range normalizeOneshotToolsets(requested) {
		if !slices.Contains(parsed, name) {
			parsed = append(parsed, name)
		}
	}
	if len(parsed) == 0 {
		return "", nil
	}
	prompt, loaded, missing := skills.NewScope(home.Dir(ctx), nil).BuildPreloadedSkillsPrompt(ctx, parsed, "", nil)
	if len(missing) > 0 {
		if len(loaded) == 0 {
			return "", errors.New(skills.FormatMissingSkills(missing))
		}
		slog.Default().Warn(fmt.Sprintf("Skipping %s. Continuing with: %s. List available skills with `icebath skills list`.",
			skills.FormatMissingSkills(missing), strings.Join(loaded, ", ")))
	}
	return prompt, nil
}

// One in process turn built like a CLI chat turn, the _run_agent hermes tests replace
var oneshotRunAgent = runOneshotAgentTurn

// Builds the agent like a normal chat turn, runs one conversation and returns the reply and run result
func runOneshotAgentTurn(ctx context.Context, env *tools.Env, prompt string, o oneshotRunOptions) (string, map[string]any, error) {
	cfg, err := oneshotLoadConfig(ctx)
	if err != nil {
		return "", nil, err
	}
	choice, err := resolveOneshotModelAndProvider(ctx, cfg, o.Model, o.Provider)
	if err != nil {
		return "", nil, err
	}
	db := createOneshotSessionDB(ctx)
	resumeSID, history, resumeMeta, err := loadOneshotResumeTarget(ctx, db, o.Resume)
	if err != nil {
		return "", nil, err
	}
	choice = applyOneshotStoredSessionRuntime(ctx, choice, resumeMeta, pystr.Strip(o.Model) != "")
	runtime, fallbackEntry, err := oneshotResolveRuntimeWithFallback(ctx, cfg, choice.Provider, choice.Model, choice.BaseURL, choice.APIKey)
	if err != nil {
		return "", nil, err
	}
	if fallbackEntry != nil {
		choice = &oneshotModelChoice{Model: cliStr(fallbackEntry["model"], ""), Provider: runtime.Provider,
			BaseURL: choice.BaseURL, APIKey: choice.APIKey}
	}
	if choice.APIMode != "" {
		runtime.APIMode = provider.APIMode(choice.APIMode)
	}
	reasoningConfig := config.ResolveReasoningConfig(cfg, choice.Model)
	if pystr.Strip(o.Reasoning) != "" {
		if parsed := config.ParseReasoningEffort(o.Reasoning); parsed == nil {
			slog.Default().Warn(fmt.Sprintf("Unknown --reasoning '%s', keeping the configured level", o.Reasoning))
		} else {
			reasoningConfig = parsed
		}
	}
	toolsetsList := normalizeOneshotToolsets(o.Toolsets)
	if toolsetsList == nil && o.UseConfigToolsets {
		toolsetsList = oneshotPlatformTools(ctx, cfg, "cli")
		sort.Strings(toolsetsList)
	}
	env.Config = config.Tree(cfg)
	oneshotEnsureMCPDiscovery(ctx, env)
	skillsPrompt, err := oneshotPreloadedSkillsPrompt(ctx, o.Skills)
	if err != nil {
		return "", nil, err
	}
	var a oneshotAgent
	relaySessionID := ""
	defer func() { closeOneshotAgent(ctx, env, a, db, relaySessionID) }()
	var store agent.SessionStore
	if db != nil {
		store = db
	}
	a, err = newOneshotAgent(ctx, agent.AIAgentOptions{
		APIKey: runtime.APIKeyValue(), BaseURL: runtime.BaseURL, Provider: runtime.Provider,
		RequestedProvider: runtime.RequestedProvider, APIMode: string(runtime.APIMode), Model: choice.Model,
		EnabledToolsets: toolsetsList, QuietMode: true, Platform: "cli", SessionDB: store, SessionID: resumeSID,
		CredentialPool: runtime.CredentialPool, FallbackModel: provider.GetFallbackChain(cfg),
		RequestOverrides: runtime.RequestOverrides, EphemeralSystemPrompt: skillsPrompt,
		ReasoningConfig: reasoningConfig, ClarifyCallback: oneshotClarifyCallback, Env: env, Cwd: env.Cwd,
	})
	if err != nil {
		return "", nil, err
	}
	a.SetSuppressStatusOutput(true)
	a.SetStreamDeltaCallback(nil)
	a.SetToolGenCallback(nil)
	auxBefore := pyjson.NewObject()
	if o.Ledger {
		auxBefore = agent.OneshotAuxiliaryUsage(ctx, db, resumeSID)
	}
	relaySessionID = a.SessionID()
	var replay []map[string]any
	if len(history) > 0 {
		replay = history
	}
	result, err := a.RunConversation(ctx, prompt, agent.RunConversationOptions{ConversationHistory: replay})
	if err != nil {
		return "", nil, err
	}
	if result == nil {
		result = map[string]any{}
	}
	if o.Ledger {
		fallback := a.SessionID()
		if fallback == "" {
			fallback = resumeSID
		}
		agent.AttachAuxiliaryUsage(ctx, result, db, auxBefore, fallback)
	}
	return cliStr(result["final_response"], ""), result, nil
}

// Runs a cleanup step, logging a failure or panic at debug and never raising
func oneshotQuietly(what string, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			oneshotLogger.Debug(fmt.Sprintf("oneshot %s failed: %v", what, r))
		}
	}()
	if err := fn(); err != nil {
		oneshotLogger.Debug(fmt.Sprintf("oneshot %s failed: %v", what, err))
	}
}

// Bounded wait for notify on complete background processes of this turn before the agent kills its children
func lingerForBackgroundCompletions(ctx context.Context, env *tools.Env) {
	tools.DefaultProcessRegistry().WaitForPendingCompletions(ctx, env, "", nil, oneshotPendingCompletionsPollIntervalSec)
}

// Teardown mirroring the gateway agent cleanup, finalizing the relay root and closing the session store
func closeOneshotAgent(ctx context.Context, env *tools.Env, a oneshotAgent, db *state.SessionDB, relaySessionID string) {
	if a != nil {
		oneshotQuietly("background completion wait", func() error {
			lingerForBackgroundCompletions(ctx, env)
			return nil
		})
		if relaySessionID != "" {
			platform := a.Platform()
			if platform == "" {
				platform = "cli"
			}
			oneshotQuietly("session finalize", func() error {
				agent.FinalizeSession(ctx, map[string]any{"session_id": relaySessionID, "platform": platform, "reason": "shutdown"})
				return nil
			})
		}
		oneshotQuietly("memory/context cleanup", func() error {
			a.ShutdownMemoryProvider(ctx, agent.SessionMessageDicts(a.SessionMessages()))
			return nil
		})
		oneshotQuietly("agent cleanup", a.Close)
	}
	if db != nil {
		oneshotQuietly("session store cleanup", db.Close)
	}
}

// Notice telling the agent no user can answer a clarify question
const oneshotClarifyNotice = "oneshot mode: no user available. Pick the best choices using your own judgment, " +
	"or make the most reasonable assumption you can, and continue."

// Clarify answer of a one shot turn, undelivered so the agent picks defaults and proceeds
func oneshotClarifyCallback(_ context.Context, _ []any) (map[string]any, error) {
	return map[string]any{"answers": map[string]any{}, "outcome": "undelivered", "notice": oneshotClarifyNotice}, nil
}
