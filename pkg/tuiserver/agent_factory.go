package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session overlay prompt of the personality commands package, assigned by the command wiring
var Personality struct {
	EphemeralSystemPrompt func(cfg map[string]any) string
}

// Toolset validation and plugin discovery an explicit toolset pin resolves through
var (
	validateToolset = tools.Default.ValidateToolset
	discoverPlugins = plugins.DiscoverPlugins
	// Coding posture selection, configured platform toolsets and disabled toolsets every session build reads
	codingSelection      = tools.CodingSelection
	platformTools        = tools.Default.PlatformTools
	loadDisabledToolsets = loadDisabledToolsetsConfig
	// Effective config of the active home with defaults merged, the hermes load_config read
	loadConfig = func(ctx context.Context) (config.Tree, error) { return config.LoadContext(ctx, home.Dir(ctx), nil) }
)

// Approval modes a session reports, anything else read as manual
var approvalModes = map[string]bool{"manual": true, "smart": true, "off": true}

// Session platform, desktop for the chat panel of the desktop app else tui
func ResolveSessionPlatform() string { return resolveSessionPlatform() }

func resolveSessionPlatform() string {
	desktop := provider.IsTruthyValue(os.Getenv("ICEBATH_DESKTOP"))
	if desktop && !provider.IsTruthyValue(os.Getenv("ICEBATH_DESKTOP_TERMINAL")) {
		return "desktop"
	}
	return "tui"
}

// Session db source, an explicit caller value kept and only an empty one falling back to the platform
func resolveSessionSource(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return resolveSessionPlatform()
}

// Platform an agent of a session source is built for
func resolveAgentPlatform(source string) string { return resolveSessionSource(source) }

// Custom adapter entry an agent runs, resolved in the session's own profile
func scopedSessionProvider(ctx context.Context, a *agent.AIAgent, session *tuigateway.Session) string {
	profileHome := strOr(session.Get("profile_home"))
	if profileHome == "" {
		profileHome = importedHome()
	}
	ctx = profileRuntimeScope(ctx, profileHome, true)
	if name := strOr(runtimeModelConfig(ctx, a, nil)["provider"]); name != "" {
		return name
	}
	return "custom"
}

// Persisted model config with the agent current runtime identity merged on, falsy attributes deleting their key
func runtimeModelConfig(ctx context.Context, a *agent.AIAgent, existing map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range existing {
		out[key] = value
	}
	view := agent.SessionRuntimeView(a)
	model, baseURL := strings.TrimSpace(view.Model()), strings.TrimSpace(view.BaseURL())
	identity := provider.SessionProviderIdentity(ctx, strings.TrimSpace(view.Provider()), strings.TrimSpace(view.RequestedProvider()), baseURL, model, nil)
	var reasoning any
	if rc := view.ReasoningConfig(); rc != nil {
		reasoning = rc
	}
	var tier any
	if a.ServiceTier() != "" {
		tier = a.ServiceTier()
	}
	live := []struct {
		key   string
		value any
	}{{"model", model}, {"provider", identity}, {"base_url", baseURL}, {"api_mode", strings.TrimSpace(view.APIMode())},
		{"reasoning_config", reasoning}, {"service_tier", tier}}
	for _, field := range live {
		_, isDict := field.value.(map[string]any)
		if truthy(field.value) || isDict {
			out[field.key] = field.value
		} else {
			delete(out, field.key)
		}
	}
	return out
}

// Config env of the active home the approval and toolset gates read
func configEnv(ctx context.Context) *tools.Env {
	tree, err := loadConfig(ctx)
	if err != nil {
		panic(err)
	}
	return &tools.Env{Home: home.Dir(ctx), Config: tree, TerminalScope: tools.GetTerminalScope(ctx)}
}

// Effective approvals mode through the approval gate itself, manual for anything unknown
func loadApprovalMode(ctx context.Context) string {
	mode := tools.GetApprovalMode(configEnv(ctx))
	if approvalModes[mode] {
		return mode
	}
	return "manual"
}

// Toolsets that exist because of the client, every client surface toolset on desktop else project
func guiSurfaceToolsets(platform string) map[string]bool {
	if platform == "desktop" {
		out := map[string]bool{}
		for name := range tools.ClientSurfaceToolsets {
			out[name] = true
		}
		return out
	}
	return map[string]bool{"project": true}
}

// Selection plus the client surface toolsets the session carries, configured disables applied except desktop_ui
func withSessionToolsets(ctx context.Context, selection []string, platform string) []string {
	foldIn := guiSurfaceToolsets(platform)
	for _, name := range selection {
		delete(foldIn, name)
	}
	for _, name := range loadDisabledToolsets(ctx) {
		if name != "desktop_ui" {
			delete(foldIn, name)
		}
	}
	extra := make([]string, 0, len(foldIn))
	for name := range foldIn {
		extra = append(extra, name)
	}
	sort.Strings(extra)
	return append(append([]string{}, selection...), extra...)
}

// Writes a TUI notice line to stderr
func tuiNotice(text string) {
	fmt.Fprintln(os.Stderr, text)
}

// Resolves an ICEBATH_TUI_TOOLSETS pin, nil with all for every toolset, ok false when nothing was valid
func resolveExplicitToolsets(ctx context.Context, explicit []string, validate func(string) bool) (resolved []string, all bool, ok bool) {
	builtIn := []string{}
	unresolved := []string{}
	for _, name := range explicit {
		if validate(name) {
			builtIn = append(builtIn, name)
		}
	}
	for _, name := range explicit {
		if !slices.Contains(builtIn, name) {
			unresolved = append(unresolved, name)
		}
	}
	if len(unresolved) > 0 {
		pluginValid := func() (valid []string) {
			defer func() {
				if recover() != nil {
					valid = nil
				}
			}()
			discoverPlugins(ctx, false)
			for _, name := range unresolved {
				if validate(name) {
					valid = append(valid, name)
				}
			}
			return valid
		}()
		builtIn = append(builtIn, pluginValid...)
		unresolved = slices.DeleteFunc(unresolved, func(name string) bool { return slices.Contains(pluginValid, name) })
	}
	if slices.ContainsFunc(builtIn, func(name string) bool { return name == "all" || name == "*" }) {
		var ignored []string
		for _, name := range explicit {
			if name != "all" && name != "*" {
				ignored = append(ignored, name)
			}
		}
		if len(ignored) > 0 {
			tuiNotice("[tui] ICEBATH_TUI_TOOLSETS=all enables every toolset; ignoring additional entries: " + strings.Join(ignored, ", "))
		}
		return nil, true, true
	}
	if len(unresolved) == 0 {
		return builtIn, false, true
	}
	mcpNames, mcpDisabled := map[string]bool{}, map[string]bool{}
	func() {
		defer func() {
			if recover() != nil {
				mcpNames, mcpDisabled = map[string]bool{}, map[string]bool{}
			}
		}()
		raw, err := config.ReadRawConfig(ctx, home.Dir(ctx))
		if err != nil {
			panic(err)
		}
		servers, _ := raw["mcp_servers"].(map[string]any)
		for name, serverCfg := range servers {
			if cfg, isDict := serverCfg.(map[string]any); isDict {
				if tools.MCPServerEnabled(cfg) {
					mcpNames[name] = true
				} else {
					mcpDisabled[name] = true
				}
			}
		}
	}()
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
	if len(unknown) > 0 {
		tuiNotice("[tui] ignoring unknown ICEBATH_TUI_TOOLSETS entries: " + strings.Join(unknown, ", "))
	}
	if len(disabled) > 0 {
		tuiNotice("[tui] ignoring disabled MCP servers in ICEBATH_TUI_TOOLSETS " +
			"(set enabled: true in config.yaml to use): " + strings.Join(disabled, ", "))
	}
	resolved = append(builtIn, mcpValid...)
	if len(resolved) == 0 {
		return nil, false, false
	}
	return resolved, false, true
}

// Agent toolsets of a session, nil for all, an explicit pin, the coding posture or the configured CLI set
func loadEnabledToolsets(ctx context.Context, platform string) []string {
	sessionPlatform := platform
	if sessionPlatform == "" {
		sessionPlatform = resolveSessionPlatform()
	}
	var explicit []string
	for _, item := range strings.Split(os.Getenv("ICEBATH_TUI_TOOLSETS"), ",") {
		if item = strings.TrimSpace(item); item != "" {
			explicit = append(explicit, item)
		}
	}
	fallbackNotice := ""
	if len(explicit) == 0 {
		var selection []string
		suppress(func() {
			selection = codingSelection(configEnv(ctx), sessionPlatform, "")
		})
		if selection != nil {
			out := withSessionToolsets(ctx, selection, sessionPlatform)
			sort.Strings(out)
			return out
		}
	}
	scope := tools.ScopeKey(home.Dir(ctx))
	if len(explicit) > 0 {
		resolved, all, ok := resolveExplicitToolsets(ctx, explicit, func(name string) bool {
			return validateToolset(ctx, scope, name)
		})
		if ok {
			if all {
				return nil
			}
			return resolved
		}
		fallbackNotice = "[tui] no valid ICEBATH_TUI_TOOLSETS entries; using configured CLI toolsets"
	}
	var result []string
	loaded := func() (done bool) {
		defer func() {
			if recover() != nil {
				done = false
			}
		}()
		cfg, err := loadConfig(ctx)
		if err != nil {
			panic(err)
		}
		enabled := platformTools(ctx, scope, cfg, "cli", tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
		if fallbackNotice != "" {
			tuiNotice(fallbackNotice)
		}
		if len(enabled) > 0 {
			names := make([]string, 0, len(enabled))
			for name := range enabled {
				names = append(names, name)
			}
			result = withSessionToolsets(ctx, names, sessionPlatform)
			sort.Strings(result)
			return true
		}
		if tools.PlatformToolsetsExplicitlySaved(cfg, "cli") {
			result = []string{}
			return true
		}
		result = nil
		return true
	}()
	if !loaded {
		if fallbackNotice != "" {
			tuiNotice("[tui] no valid ICEBATH_TUI_TOOLSETS entries and configured CLI toolsets could not be loaded; enabling all toolsets")
		}
		return nil
	}
	return result
}

// Enabled and configured disabled toolsets a desktop or TUI session of the platform is built with
func GatewayToolsets(ctx context.Context, platform string) (enabled, disabled []string) {
	return loadEnabledToolsets(ctx, platform), loadDisabledToolsetsConfig(ctx)
}

// Configured agent disabled toolsets, nil when none or unreadable
func loadDisabledToolsetsConfig(ctx context.Context) (disabled []string) {
	defer func() {
		if recover() != nil {
			disabled = nil
		}
	}()
	cfg, err := loadConfig(ctx)
	if err != nil {
		return nil
	}
	agentCfg, _ := cfg["agent"].(map[string]any)
	for _, name := range tools.ParseConfigStringList(agentCfg["disabled_toolsets"]) {
		disabled = append(disabled, name)
	}
	if len(disabled) == 0 {
		return nil
	}
	return disabled
}

// Status bar placements a session may report
var statusbarModes = map[string]bool{"off": true, "top": true, "bottom": true}

// Tool progress modes a session may run
var toolProgressModes = map[string]bool{"off": true, "new": true, "all": true, "verbose": true}

// Tools whose lifecycle is the UI card or diff the user reviews, never optional chrome
var toolLifecycleUITools = map[string]bool{
	"clarify": true, "manage_connections": true, "setup_mcp": true,
	"image_generate": true, "manage_catalog": true, "delegate_task": true, "setup_beat": true, "set_theme": true,
	"edit_file": true, "patch": true, "write_file": true,
}

// Launch scoped model seed, empty when unset
func envModelSeed() string {
	if seed := os.Getenv("ICEBATH_MODEL"); seed != "" {
		return strings.TrimSpace(seed)
	}
	return strings.TrimSpace(os.Getenv("ICEBATH_INFERENCE_MODEL"))
}

// Model of a new session, the launch seed, the configured default, else the cached silent default
func resolveModel(ctx context.Context) (model string) {
	if seed := envModelSeed(); seed != "" {
		return seed
	}
	switch m := loadCfg(ctx)["model"].(type) {
	case map[string]any:
		return strings.TrimSpace(strOr(m["default"]))
	case string:
		if m != "" {
			return strings.TrimSpace(m)
		}
	}
	defer func() {
		if recover() != nil {
			model = provider.PreferredSilentDefaultModel
		}
	}()
	return provider.GetPreferredSilentDefaultModel(ctx, "openrouter")
}

// Model and provider config.yaml selects, auto read as no provider
func configModelTarget(ctx context.Context) (string, string) {
	switch m := loadCfg(ctx)["model"].(type) {
	case map[string]any:
		name := strings.TrimSpace(strOr(m["provider"]))
		if strings.ToLower(name) == "auto" {
			name = ""
		}
		return strings.TrimSpace(strOr(m["default"])), name
	case string:
		return strings.TrimSpace(m), ""
	}
	return "", ""
}

// Startup model and explicit provider, the env pin, the routed launch seed, else the model with no provider
func resolveStartupRuntime(ctx context.Context) (string, string) {
	model := resolveModel(ctx)
	if explicit := strings.TrimSpace(os.Getenv("ICEBATH_TUI_PROVIDER")); explicit != "" {
		return model, explicit
	}
	explicitModel := envModelSeed()
	if explicitModel == "" {
		return model, ""
	}
	routedModel, routedProvider, found := "", "", false
	suppress(func() {
		fullCfg := loadCfg(ctx)
		currentProvider := ""
		if cfg, ok := fullCfg["model"].(map[string]any); ok {
			currentProvider = strings.ToLower(strings.TrimSpace(strOr(cfg["provider"])))
		}
		if currentProvider == "" {
			currentProvider = strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_INFERENCE_PROVIDER")))
		}
		if currentProvider == "" {
			currentProvider = "auto"
		}
		userProviders, _ := fullCfg["providers"].(map[string]any)
		customProviders, _ := fullCfg["custom_providers"].([]any)
		route, err := provider.ResolveStartupModelRoute(ctx, explicitModel, provider.StartupRouteOptions{
			CurrentProvider: currentProvider, UserProviders: userProviders, CustomProviders: customProviders})
		if err != nil {
			panic(err)
		}
		if route != nil {
			routedModel, routedProvider, found = route.Model, route.Provider, true
			return
		}
		if detectedProvider, detectedModel, ok := provider.DetectStaticProviderForModel(ctx, explicitModel, currentProvider); ok {
			routedModel, routedProvider, found = detectedModel, detectedProvider, true
		}
	})
	if found {
		return routedModel, routedProvider
	}
	return model, ""
}

// Whether a provider name resolves to a routable route, false on any failure
func isRoutableProvider(ctx context.Context, name string) (routable bool) {
	defer func() {
		if recover() != nil {
			routable = false
		}
	}()
	return provider.IsRoutableProvider(ctx, name)
}

// Whether persisted runtime overrides still name a routable provider, empty counting as not routable
func overridesHaveRoutableProvider(ctx context.Context, overrides map[string]any) bool {
	name := strings.TrimSpace(strOr(overrides["provider_override"]))
	if name == "" {
		modelOverride, _ := overrides["model_override"].(map[string]any)
		name = strings.TrimSpace(strOr(modelOverride["provider"]))
	}
	return name != "" && isRoutableProvider(ctx, name)
}

// Row model_config as a dict, empty when absent or invalid, a parse error raised unless quiet
func parseModelConfig(raw any, quiet bool) (map[string]any, error) {
	switch v := raw.(type) {
	case map[string]any:
		return v, nil
	case *pyjson.Object:
		return objectMap(v), nil
	case string:
		if strings.TrimSpace(v) == "" {
			return map[string]any{}, nil
		}
		parsed, err := pyjson.Loads(v)
		if err != nil {
			if !quiet {
				return nil, err
			}
			logger().Debug("failed to parse stored session model_config", "error", err)
			return map[string]any{}, nil
		}
		switch p := parsed.(type) {
		case map[string]any:
			return p, nil
		case *pyjson.Object:
			return objectMap(p), nil
		}
	}
	return map[string]any{}, nil
}

// Whether a stored row is a canonical Bot Chat whose runtime follows the member profile config
func rowFollowsProfile(row *pyjson.Object) bool {
	if row == nil || row.Len() == 0 {
		return false
	}
	modelConfig, _ := parseModelConfig(row.GetOr("model_config", nil), true)
	return truthy(modelConfig["follow_profile_config"]) || strings.TrimSpace(strOr(row.GetOr("title", nil))) == "Bot Chat"
}

// Runtime overrides persisted with a stored session so a resume restores the route that chat used
func storedSessionRuntimeOverrides(ctx context.Context, row *pyjson.Object) map[string]any {
	if row == nil || row.Len() == 0 {
		return map[string]any{}
	}
	modelConfig, _ := parseModelConfig(row.GetOr("model_config", nil), true)
	rowTitle := strings.TrimSpace(strOr(row.GetOr("title", nil)))
	roomPlumbing := truthy(modelConfig["room_plumbing"]) || (truthy(row.GetOr("hidden", nil)) && strings.HasPrefix(rowTitle, "Group:"))
	composerMatches := false
	if composer, ok := modelConfig["composer_override_profile"].(map[string]any); ok {
		targetModel, targetProvider := configModelTarget(ctx)
		composerMatches = strings.TrimSpace(strOr(composer["model"])) == targetModel && strings.TrimSpace(strOr(composer["provider"])) == targetProvider
	}
	if roomPlumbing || (rowFollowsProfile(row) && !composerMatches) {
		return map[string]any{}
	}
	overrides := map[string]any{}
	model := strOr(row.GetOr("model", nil))
	if model == "" {
		model = strOr(modelConfig["model"])
	}
	model = strings.TrimSpace(model)
	route := state.SessionGatewayRuntime(row)
	routeField := func(key string) string { return strings.TrimSpace(strOr(route.GetOr(key, nil))) }
	providerName, baseURL, apiMode := routeField("provider"), routeField("base_url"), routeField("api_mode")
	serviceTier := strings.TrimSpace(strOr(modelConfig["service_tier"]))
	reasoningConfig := modelConfig["reasoning_config"]
	if provider.IsForeignProviderEndpoint(providerName, baseURL) {
		baseURL, apiMode = "", ""
	}
	if providerName != "" && !isRoutableProvider(ctx, providerName) {
		healed := ""
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger().Debug("custom provider identity recovery failed", "error", r)
				}
			}()
			healed = provider.CanonicalCustomIdentity(ctx, baseURL, "", model, "")
		}()
		if healed != "" {
			logger().Info(fmt.Sprintf("healed stale session provider %s to %s", pyrepr.Repr(providerName), pyrepr.Repr(healed)))
			providerName, baseURL = healed, ""
		} else {
			providerName, baseURL, apiMode = "", "", ""
		}
	}
	orNone := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	if model != "" {
		overrides["model_override"] = map[string]any{
			"model": model, "provider": orNone(providerName), "base_url": orNone(baseURL), "api_mode": orNone(apiMode)}
	}
	if providerName != "" {
		overrides["provider_override"] = providerName
	}
	if rc, ok := reasoningConfig.(map[string]any); ok {
		overrides["reasoning_config_override"] = rc
	}
	if serviceTier != "" {
		if strings.ToLower(serviceTier) == "normal" {
			overrides["service_tier_override"] = ""
		} else {
			overrides["service_tier_override"] = serviceTier
		}
	}
	return overrides
}

// Live agent, session key and owning session store of a record, ok false when any is missing
func liveSessionAgentDB(ctx context.Context, session *tuigateway.Session) (*agent.AIAgent, string, *state.DB, bool) {
	a := sessionAgent(session)
	sessionKey := strings.TrimSpace(strOr(session.Get("session_key")))
	if a == nil || sessionKey == "" {
		return nil, "", nil, false
	}
	db := a.SessionDB()
	if db == nil {
		db = getDB(ctx)
	}
	if db == nil {
		return nil, "", nil, false
	}
	return a, sessionKey, db, true
}

// Persists the active session runtime so a later resume restores the same footer
func persistLiveSessionRuntime(ctx context.Context, session *tuigateway.Session) {
	a, sessionKey, db, ok := liveSessionAgentDB(ctx, session)
	if !ok {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logger().Debug("failed to persist live session runtime", "error", r)
		}
	}()
	row, err := db.GetSession(ctx, sessionKey)
	if err != nil {
		panic(err)
	}
	if row == nil {
		row = pyjson.NewObject()
	}
	existing, err := parseModelConfig(row.GetOr("model_config", nil), false)
	if err != nil {
		panic(err)
	}
	modelConfig := runtimeModelConfig(ctx, a, existing)
	if composer, isDict := session.Get("composer_override_profile").(map[string]any); isDict {
		modelConfig["composer_override_profile"] = composer
	} else if _, present := session.Lookup("composer_override_profile"); present {
		delete(modelConfig, "composer_override_profile")
	}
	if tierOverride, present := session.Lookup("create_service_tier_override"); present && tierOverride != nil {
		if truthy(tierOverride) {
			modelConfig["service_tier"] = tierOverride
		} else {
			modelConfig["service_tier"] = "normal"
		}
	}
	encoded, err := pyjson.Dumps(modelConfig)
	if err != nil {
		panic(err)
	}
	var model *string
	if m := strings.TrimSpace(strOr(modelConfig["model"])); m != "" {
		model = &m
	}
	if err := db.UpdateSessionMeta(ctx, sessionKey, encoded, model); err != nil {
		panic(err)
	}
}

// Status bar placement, off for a literal false and top for anything unknown
func coerceStatusbar(raw any) string {
	if raw == false {
		return "off"
	}
	if s, ok := raw.(string); ok {
		if mode := strings.ToLower(strings.TrimSpace(s)); statusbarModes[mode] {
			return mode
		}
	}
	return "top"
}

// Mouse tracking mode of the display config, legacy tui_mouse only when mouse_tracking is absent
func displayMouseTracking(display any) string {
	return tuigateway.DisplayMouseTracking(display)
}

// Reasoning config of a model through the shared resolver, per model override over the global effort
func loadReasoningConfig(ctx context.Context, model string) map[string]any {
	return config.ResolveReasoningConfig(loadCfg(ctx), model)
}

// Configured agent service tier, empty for the normal tier
func loadServiceTier(ctx context.Context) string {
	agentCfg, _ := loadCfg(ctx)["agent"].(map[string]any)
	raw, present := agentCfg["service_tier"]
	if !present {
		raw = ""
	}
	return provider.ParseServiceTier(strOr(raw))
}

// OpenRouter provider routing preferences of the config, empty when unset
func loadProviderRouting(ctx context.Context) (routing any) {
	defer func() {
		if recover() != nil {
			routing = map[string]any{}
		}
	}()
	raw, present := loadCfg(ctx)["provider_routing"]
	if !present || !truthy(raw) {
		return map[string]any{}
	}
	return raw
}

// Whether reasoning shows by default, true unless display show_reasoning says otherwise
func loadShowReasoning(ctx context.Context) bool {
	raw, present := displayCfg(ctx)["show_reasoning"]
	if !present {
		return true
	}
	return truthy(raw)
}

// Memory notification level, on by default with booleans normalized
func loadMemoryNotifications(ctx context.Context) string {
	raw := displayCfg(ctx)["memory_notifications"]
	if b, isBool := raw.(bool); isBool {
		if b {
			return "on"
		}
		return "off"
	}
	if truthy(raw) {
		return strings.ToLower(pyrepr.Str(raw))
	}
	return "on"
}

// Tool progress mode, the env pin first then display tool_progress, all by default
func loadToolProgressMode(ctx context.Context) string {
	if env := strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_TUI_TOOL_PROGRESS"))); toolProgressModes[env] {
		return env
	}
	raw, present := displayCfg(ctx)["tool_progress"]
	if !present {
		raw = "all"
	}
	if b, isBool := raw.(bool); isBool {
		if b {
			return "all"
		}
		return "off"
	}
	mode := "all"
	if truthy(raw) {
		mode = strings.ToLower(strings.TrimSpace(pyrepr.Str(raw)))
	}
	if toolProgressModes[mode] {
		return mode
	}
	return "all"
}

// Tool progress mode a live session runs, all by default
func sessionToolProgressMode(sid string) string {
	raw, present := tuigateway.SessionRecord(sid).Lookup("tool_progress_mode")
	if !present {
		raw = "all"
	}
	if mode := strOr(raw); mode != "" {
		return mode
	}
	return "all"
}

// Whether a session reports verbose tool progress
func sessionVerbose(sid string) bool { return sessionToolProgressMode(sid) == "verbose" }

// Session reasoning display flag, the config default when the session sets none
func sessionShowReasoning(ctx context.Context, sid string) bool {
	if raw, present := tuigateway.SessionRecord(sid).Lookup("show_reasoning"); present {
		return truthy(raw)
	}
	return loadShowReasoning(ctx)
}

// Whether a session shows tool progress at all
func toolProgressEnabled(sid string) bool { return sessionToolProgressMode(sid) != "off" }

// Whether a tool's lifecycle is an interactive card or diff the client must always receive
func toolLifecycleRequiredForUI(name string) bool { return toolLifecycleUITools[name] }

// Arguments of one runtime provider resolution
type runtimeResolveArgs struct {
	requested, targetModel, explicitBaseURL, explicitAPIKey string
}

// Primary runtime or one complete provider and model fallback, provider only fallback entries skipped
func resolveRuntimeWithFallback(ctx context.Context, args runtimeResolveArgs) (*provider.Runtime, string, bool, error) {
	h := home.Dir(ctx)
	rt, primaryErr := provider.ResolveRuntimeProvider(ctx, h, args.requested, args.targetModel, args.explicitBaseURL, args.explicitAPIKey)
	if primaryErr == nil {
		return rt, "", false, nil
	}
	var primary *provider.AuthError
	if !errors.As(primaryErr, &primary) {
		return nil, "", false, primaryErr
	}
	for _, entry := range loadFallbackModel(ctx) {
		fbProvider, fbModel := strings.TrimSpace(strOr(entry["provider"])), strings.TrimSpace(strOr(entry["model"]))
		if fbProvider == "" || fbModel == "" {
			continue
		}
		runtime, ok := func() (runtime *provider.Runtime, ok bool) {
			defer func() {
				if recover() != nil {
					runtime, ok = nil, false
				}
			}()
			baseURL := ""
			if truthy(entry["base_url"]) {
				baseURL = strOr(entry["base_url"])
			}
			resolved, err := provider.ResolveRuntimeProvider(ctx, h, fbProvider, fbModel, baseURL, provider.ResolveEntryAPIKey(ctx, entry))
			if err != nil {
				return nil, false
			}
			resolved.Provider = provider.EffectiveRuntimeProvider(entry, resolved)
			return resolved, true
		}()
		if !ok {
			continue
		}
		wording, _ := provider.PrimaryFailureWording(primary)
		logger().Warn(fmt.Sprintf("Primary %s (%s), falling back to %s model %s", wording, primary, fbProvider, fbModel))
		return runtime, fbModel, true, nil
	}
	return nil, "", false, primaryErr
}

// Model and runtime of a new agent, a per session override winning over config, with the pre agent fallback notice
func resolveAgentModelRuntime(ctx context.Context, modelOverride any, providerOverride string) (string, *provider.Runtime, string, error) {
	var model, requested string
	var args runtimeResolveArgs
	overrides := map[string]string{}
	if mo, isDict := modelOverride.(map[string]any); isDict && truthy(mo["model"]) {
		model = strOr(mo["model"])
		requested = strOr(mo["provider"])
		if requested == "" {
			requested = providerOverride
		}
		overrideBaseURL := strOr(mo["base_url"])
		if strings.ToLower(strings.TrimSpace(requested)) == "custom" {
			if recovered := provider.CanonicalCustomIdentity(ctx, overrideBaseURL, "", model, ""); recovered != "" {
				requested = recovered
			}
			if overrideBaseURL != "" {
				args.explicitBaseURL = overrideBaseURL
			}
		}
		args.requested, args.targetModel = requested, model
		for _, key := range []string{"base_url", "api_key", "api_mode"} {
			overrides[key] = strOr(mo[key])
		}
	} else {
		model, requested = resolveStartupRuntime(ctx)
		if text, isText := modelOverride.(string); isText && text != "" {
			model = text
		}
		if providerOverride != "" {
			requested = providerOverride
		}
		args = runtimeResolveArgs{requested: requested, targetModel: model}
	}
	rt, selectedModel, usedFallback, err := resolveRuntimeWithFallback(ctx, args)
	if err != nil {
		return "", nil, "", err
	}
	if usedFallback {
		if selectedModel == "" {
			return "", nil, "", errors.New("Auth fallback resolved without a model")
		}
		primaryProvider := requested
		if primaryProvider == "" {
			if cfgModel, ok := loadCfg(ctx)["model"].(map[string]any); ok {
				primaryProvider = strOr(cfgModel["provider"])
			}
		}
		return selectedModel, rt, provider.PreAgentFallbackNotice(primaryProvider, model, rt.Provider, selectedModel), nil
	}
	if rt.Source == "local-runtime" {
		delete(overrides, "base_url")
	}
	anySet := false
	for key, value := range overrides {
		if value == "" {
			continue
		}
		anySet = true
		switch key {
		case "base_url":
			rt.BaseURL = value
		case "api_key":
			rt.APIKey = value
		case "api_mode":
			rt.APIMode = provider.APIMode(value)
		}
	}
	if anySet {
		rederivePerModelRoute(ctx, model, rt)
	}
	return model, rt, "", nil
}

// Re-derives api mode and base URL from the target model for providers that pick the wire per model
func rederivePerModelRoute(ctx context.Context, model string, rt *provider.Runtime) {
	name := rt.RequestedProvider
	if name == "" {
		name = rt.Provider
	}
	apiMode, ok := provider.ModelDerivedAPIMode(ctx, name, model, "")
	if !ok {
		return
	}
	rt.APIMode = provider.APIMode(apiMode)
	rt.BaseURL = provider.NormalizeOpenCodeBaseURL(name, provider.APIMode(apiMode), rt.BaseURL)
}

// Config ephemeral system prompt plus the ICEBATH_TUI_SKILLS preload block, failing only when every skill is missing
func startupSystemPrompt(ctx context.Context, cfg map[string]any, taskID string) (string, error) {
	systemPrompt := Personality.EphemeralSystemPrompt(cfg)
	startupSkills := parseTUISkillsEnv()
	if len(startupSkills) == 0 {
		return systemPrompt, nil
	}
	skillsPrompt, loadedSkills, missingSkills := skills.NewScope(home.Dir(ctx), nil).BuildPreloadedSkillsPrompt(ctx, startupSkills, taskID, nil)
	if len(missingSkills) > 0 {
		if len(loadedSkills) == 0 {
			return "", &pypath.ValueError{Msg: skills.FormatMissingSkills(missingSkills)}
		}
		logger().Warn(fmt.Sprintf("Skipping %s. Continuing with: %s. List available skills with `icebath skills list`.",
			skills.FormatMissingSkills(missingSkills), strings.Join(loadedSkills, ", ")))
	}
	if skillsPrompt != "" {
		var parts []string
		for _, part := range []string{systemPrompt, skillsPrompt} {
			if part != "" {
				parts = append(parts, part)
			}
		}
		systemPrompt = strings.TrimSpace(strings.Join(parts, "\n\n"))
	}
	return systemPrompt, nil
}

// Plain map of an ordered JSON object
func objectMap(o *pyjson.Object) map[string]any {
	out := map[string]any{}
	if o == nil {
		return out
	}
	for _, key := range o.Keys {
		out[key], _ = o.Get(key)
	}
	return out
}
