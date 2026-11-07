package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Keyword options of a policy build, the zero value is a profile terminal launch without posture
type buildPolicyOptions struct {
	PrivateSecrets   *RecoveredSecrets
	DefaultTerminal  bool
	WorkspacePosture bool
	ClientLocal      bool
}

// Invalid params refusal naming the field and its detail text
func refuseText(field, detail string) error { return refusePolicy(field, &detail) }

// Launch flags that must arrive as JSON booleans
var launchBoolFlags = []string{"ignore_rules", "yolo", "checkpoints", "accept_hooks", "pass_session_id",
	"follow_profile_config", "cwd_explicit", "verbose"}

// Python number view of a JSON scalar, isInt true for ints and bools
func launchNumber(v any) (float64, bool, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true, true
		}
		return 0, true, true
	case int:
		return float64(n), true, true
	case int64:
		return float64(n), true, true
	case int32:
		return float64(n), true, true
	case float64:
		return n, false, true
	case float32:
		return float64(n), false, true
	}
	return 0, false, false
}

// Frozen launch policy of a create request over a deep copy of the profile config, refusing bad fields
func buildPolicy(ctx context.Context, params, cfg map[string]any, opts buildPolicyOptions) (*LocalSessionPolicy, error) {
	source, hasSource := params["source"]
	if !hasSource {
		source = "cli"
	}
	if source == "a2a" {
		return buildForwardPolicy(ctx, params, cfg, opts.PrivateSecrets)
	}
	var unknown []string
	for k := range params {
		if !CreateFields[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	label, isStr := source.(string)
	if _, surface := Surfaces[label]; len(unknown) > 0 || !isStr || !surface {
		if len(unknown) > 0 {
			return nil, refuseText("params."+unknown[0], "unknown create field")
		}
		return nil, refuseText("source", "not a surface: "+pyrepr.Repr(source))
	}
	for _, name := range BypassFields {
		if v, ok := params[name]; ok {
			if _, isBool := v.(bool); !isBool {
				return nil, refuseText(name, "must be a boolean")
			}
		}
	}
	safeMode := params["safe_mode"] == true
	ignoreUserConfig := safeMode || params["ignore_user_config"] == true
	if v, ok := params["api_key"]; ok {
		if s, isS := v.(string); !isS || strings.TrimSpace(s) == "" {
			return nil, refuseText("api_key", "must be a non-empty string")
		}
	}
	var model *string
	if v, ok := params["model"]; ok {
		s, isS := v.(string)
		if !isS || strings.TrimSpace(s) == "" {
			return nil, refuseText("model", "must be a non-empty string")
		}
		model = &s
	}
	h := home.Dir(ctx)
	fallback, err := tools.ResolveAgentCwd(&tools.Env{Home: h, TerminalScope: tools.GetTerminalScope(ctx)})
	if err != nil {
		return nil, err
	}
	rawCwd, hasCwd := params["cwd"]
	if !hasCwd {
		rawCwd = fallback
	}
	cwd, isS := rawCwd.(string)
	if !isS || !filepath.IsAbs(cwd) || !isDir(cwd) {
		return nil, refuseText("cwd", "must be an absolute path to an existing directory")
	}
	if rowCwd, ok := params["row_cwd"]; ok {
		if s, isS := rowCwd.(string); !isS || (s != "" && !filepath.IsAbs(s)) {
			return nil, refuseText("row_cwd", "must be an absolute path when set")
		}
	}
	if params["cwd_explicit"] == false && hasCwd {
		configured, err := profileWorkspaceCwd(ctx, h)
		if err != nil {
			return nil, err
		}
		if configured != "" {
			cwd = configured
		}
	}
	if err := validateEditor(label, params["editor"]); err != nil {
		return nil, err
	}
	if _, err := launchSkills(params); err != nil {
		return nil, err
	}
	text, err := pyjson.Dumps(cfg)
	if err != nil {
		return nil, err
	}
	copied, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	plain, _ := pyjson.Plain(copied).(map[string]any)
	config := presentSections(plain)
	picked := withoutAutoPick(params)
	_, pm := picked["model"]
	_, pp := picked["provider"]
	_, pb := picked["base_url"]
	if params["follow_profile_config"] == true && (pm || pp || pb) {
		block, err := setdefaultMap(config, "model")
		if err != nil {
			return nil, err
		}
		target, route := profileTarget(config)
		block["selection_profile"] = []any{target, route}
	}
	if err := applyLaunchOverrides(ctx, params, config); err != nil {
		return nil, err
	}
	_, hasToolsets := params["toolsets"]
	named := opts.WorkspacePosture && label != "tool" && hasCwd && !hasToolsets
	codingPosture := named && inCodingWorkspace(ctx, config, label, cwd)
	enabled, err := resolveToolsets(ctx, params, config, label, safeMode, codingPosture, ignoreUserConfig)
	if err != nil {
		return nil, err
	}
	var scope tools.TerminalPolicy
	if opts.DefaultTerminal {
		scope = tools.DefaultTerminalScope()
	} else if scope, err = tools.BuildProfileTerminalScope(ctx, h, nil); err != nil {
		return nil, err
	}
	terminal := make(map[string]any, len(scope))
	for k, v := range scope {
		terminal[k] = v
	}
	if params["cwd_explicit"] == true || terminalUsesHostCwd(terminal) {
		terminal["TERMINAL_CWD"] = cwd
	}
	var clientEnv, rulesJSON *string
	if raw, ok := params["client_env"]; ok {
		rules := state.ClientEnvRulesFor(config, opts.ClientLocal)
		env, err := state.NormaliseClientEnv(raw, rules)
		if err != nil {
			if typed, ok := err.(interface{ PyTypeName() string }); ok && typed.PyTypeName() == "ValueError" {
				return nil, refuseText("client_env", err.Error())
			}
			return nil, err
		}
		envText, err := pyjson.Dumps(env, pyjson.SortKeys(true))
		if err != nil {
			return nil, err
		}
		rulesText, err := pyjson.Dumps(rules.Object(), pyjson.SortKeys(true))
		if err != nil {
			return nil, err
		}
		clientEnv, rulesJSON = &envText, &rulesText
	}
	request := make(map[string]any, len(params)+1)
	for k, v := range params {
		if k != "request_id" && k != "api_key" && k != "client_env" {
			request[k] = v
		}
	}
	if _, ok := request["source"]; !ok {
		request["source"] = "cli"
	}
	if err := privateEditorRequest(request, opts.PrivateSecrets); err != nil {
		return nil, err
	}
	if err := extractConfigSecrets(config, opts.PrivateSecrets, nil); err != nil {
		return nil, err
	}
	if err := extractConfigSecrets(terminal, opts.PrivateSecrets, secretPath{nil}); err != nil {
		return nil, err
	}
	toolsets := make([]string, 0, len(enabled))
	for name := range enabled {
		toolsets = append(toolsets, name)
	}
	sort.Strings(toolsets)
	configJSON, err := pyjson.Dumps(config)
	if err != nil {
		return nil, err
	}
	requestJSON, err := pyjson.Dumps(request, pyjson.SortKeys(true))
	if err != nil {
		return nil, err
	}
	terminalJSON, err := pyjson.Dumps(terminal)
	if err != nil {
		return nil, err
	}
	return &LocalSessionPolicy{Source: label, Platform: Surfaces[label], Cwd: cwd, Model: model, Toolsets: toolsets,
		ConfigJSON: configJSON, RequestJSON: requestJSON, TerminalJSON: terminalJSON, SafeMode: safeMode,
		IgnoreUserConfig: ignoreUserConfig, CodingPosture: codingPosture, ClientEnvJSON: clientEnv,
		ClientEnvRemote: clientEnv != nil && !opts.ClientLocal, ClientEnvRulesJSON: rulesJSON,
		EditApprovalPolicy: "ask"}, nil
}

// Configured terminal cwd of a named profile, empty for the default profile, unset spellings and missing dirs
func profileWorkspaceCwd(ctx context.Context, h string) (string, error) {
	if name, ok := ProfileNameForHome(h); !ok || name == "default" {
		return "", nil
	}
	raw, err := config.ReadUserConfigRaw(ctx, filepath.Join(h, "config.yaml"))
	if err != nil {
		return "", err
	}
	terminal, _ := raw["terminal"].(map[string]any)
	text := ""
	if pyrepr.Truthy(terminal["cwd"]) {
		text = strings.TrimSpace(pyrepr.Str(terminal["cwd"]))
	}
	switch text {
	case "", ".", "auto", "cwd":
		return "", nil
	}
	configured, err := pypath.Abspath(pypath.Expanduser(text))
	if err != nil {
		return "", err
	}
	if configured != "" && isDir(configured) {
		return configured, nil
	}
	return "", nil
}

// Launch params with a bare pick of the held entry key or a bare custom pick spelled as the entry it serves
func withEntryPick(ctx context.Context, params, config map[string]any) (map[string]any, error) {
	picked, ok := params["provider"].(string)
	if !ok {
		return params, nil
	}
	identity, err := ProfileRouteIdentity(ctx, config)
	if err != nil {
		return nil, err
	}
	with := func(name string) map[string]any {
		out := make(map[string]any, len(params))
		for k, v := range params {
			out[k] = v
		}
		out["provider"] = name
		return out
	}
	if strings.ToLower(strings.TrimSpace(picked)) == "custom" {
		url, hasURL := params["base_url"]
		hasURL = hasURL && url != nil
		model := ""
		if pyrepr.Truthy(params["model"]) {
			model = pyrepr.Str(params["model"])
		}
		_, routeURL := modelRoute(config["model"])
		ownURL := provider.RouteKey(routeURL)
		if identity == "custom" && ownURL != "" {
			keep := false
			if hasURL {
				keep = provider.RouteKey(url) == ownURL
			} else {
				target, _ := profileTarget(config)
				keep = strings.ToLower(strings.TrimSpace(model)) == strings.ToLower(target) ||
					provider.RoutedCustomIdentity("", model, config, "") == ""
			}
			if keep {
				return params, nil
			}
		}
		held := ""
		if strings.HasPrefix(identity, "custom:") {
			held = identity
		}
		urlText := ""
		if hasURL {
			urlText = pyrepr.Str(url)
		}
		healed := provider.RoutedCustomIdentity(urlText, model, config, held)
		if healed == "" && strings.HasPrefix(identity, "custom:") && !hasURL {
			healed = identity
		}
		if _, hasKey := params["api_key"]; healed != "" && healed != identity && !hasURL && hasKey {
			return nil, refuseText("api_key", healed+" was chosen by model; name it with --provider or --base-url to use a launch key")
		}
		if healed != "" {
			return with(healed), nil
		}
		return params, nil
	}
	key := ConfiguredEntryKey(ctx, identity, config)
	if identity == "auto" || key == "" || strings.ToLower(strings.TrimSpace(picked)) != key {
		return params, nil
	}
	return with(identity), nil
}

// Userinfo of a split netloc reports a username or password
func urlHasCredentials(netloc string) bool {
	at := strings.LastIndex(netloc, "@")
	if at < 0 {
		return false
	}
	user, password, _ := strings.Cut(netloc[:at], ":")
	return user != "" || password != ""
}

// Validates the explicit model and agent launch overrides and folds them into the config
func applyLaunchOverrides(ctx context.Context, params, cfg map[string]any) error {
	params, err := withEntryPick(ctx, withoutAutoPick(params), cfg)
	if err != nil {
		return err
	}
	pickedProvider, hasProvider := params["provider"]
	keeps := false
	if hasProvider {
		if keeps, err = keepsProfileRoute(ctx, pickedProvider, cfg); err != nil {
			return err
		}
	}
	if hasProvider && !keeps {
		block, err := setdefaultMap(cfg, "model")
		if err != nil {
			return err
		}
		for _, stale := range RouteBoundKeys {
			delete(block, stale)
		}
	}
	for _, key := range []string{"provider", "base_url"} {
		value, ok := params[key]
		if !ok {
			continue
		}
		text, isS := value.(string)
		if !isS || strings.TrimSpace(text) == "" {
			return refuseText(key, "must be a non-empty string")
		}
		if key == "base_url" {
			url, err := pyurl.Split(text)
			if err != nil {
				return err
			}
			if (url.Scheme != "http" && url.Scheme != "https") || url.Hostname() == "" ||
				urlHasCredentials(url.Netloc) || url.Query != "" || url.Fragment != "" {
				return refuseText("base_url", "must be an http(s) URL without credentials, query or fragment")
			}
		}
		block, err := setdefaultMap(cfg, "model")
		if err != nil {
			return err
		}
		block[key] = value
	}
	for _, flag := range launchBoolFlags {
		if v, ok := params[flag]; ok {
			if _, isBool := v.(bool); !isBool {
				return refuseText(flag, "must be a boolean")
			}
		}
	}
	if raw, ok := params["service_tier"]; ok {
		tier, known := "", false
		if word, isS := raw.(string); isS {
			if parsed, err := provider.ParseExactServiceTier(word); err == nil {
				tier, known = parsed, true
			}
		}
		if !known {
			return refuseText("service_tier", "not a known tier: "+pyrepr.Repr(raw))
		}
		if tier == "" {
			tier = "normal"
		}
		agent, err := setdefaultMap(cfg, "agent")
		if err != nil {
			return err
		}
		agent["service_tier"] = tier
	}
	if raw, ok := params["run_budget"]; ok {
		_, isBool := raw.(bool)
		seconds, _, isNum := launchNumber(raw)
		if isBool || !isNum || !(seconds > 0) {
			return refuseText("run_budget", "must be a positive number of seconds")
		}
		agent, err := setdefaultMap(cfg, "agent")
		if err != nil {
			return err
		}
		agent["run_budget_seconds"] = seconds
	}
	if pyrepr.Truthy(params["checkpoints"]) {
		section := map[string]any{}
		switch held := cfg["checkpoints"].(type) {
		case map[string]any:
			for k, v := range held {
				section[k] = v
			}
		default:
			if pyrepr.Truthy(held) {
				section["enabled"] = true
			}
		}
		section["enabled"] = true
		cfg["checkpoints"] = section
	}
	if raw, ok := params["max_turns"]; ok {
		limit := 0
		_, isInt, _ := launchNumber(raw)
		if _, isS := raw.(string); isS || isInt {
			limit = config.ResolveTurnLimit(raw, 0)
		}
		if limit == 0 {
			return refuseText("max_turns", "unreadable cap: "+pyrepr.Repr(raw))
		}
		agent, err := setdefaultMap(cfg, "agent")
		if err != nil {
			return err
		}
		agent["max_turns"] = limit
	}
	if raw, ok := params["reasoning"]; ok {
		if s, isS := raw.(string); !isS || config.ParseReasoningEffort(s) == nil {
			return refuseText("reasoning", "not a known effort level: "+pyrepr.Repr(raw))
		}
		if err := pinReasoningEffort(cfg, raw, false); err != nil {
			return err
		}
	}
	return nil
}

// Session toolset set from an explicit pick or the profile, the coding posture and the client surface
func resolveToolsets(ctx context.Context, params, cfg map[string]any, source string, safeMode, codingPosture, bypass bool) (map[string]bool, error) {
	tree := config.Tree(cfg)
	surface := "cli"
	if source == "acp" {
		surface = "acp"
	}
	raw, hasExplicit := params["toolsets"]
	var explicit []any
	if hasExplicit {
		list, ok := raw.([]any)
		if !ok {
			return nil, refuseText("toolsets", "must be a list of toolset names, got "+pyrepr.TypeName(raw))
		}
		explicit = list
		known := func(name string) bool { return tools.Default.ValidateToolset(ctx, "", name) }
		if !bypass {
			known = tools.SavedToolsetResolver(ctx, tree)
		}
		configured, _ := cfg["mcp_servers"].(map[string]any)
		enabledServers := map[string]bool{}
		if len(configured) > 0 {
			enabledServers = tools.EnabledMCPServerNames(tree, tools.PortableMCPServerSet(ctx))
		}
		for _, item := range explicit {
			name, isS := item.(string)
			if !isS || !known(name) {
				return nil, refuseText("toolsets", "unknown or invalid toolset name: "+pyrepr.Repr(item))
			}
			server := name
			if _, own := configured[name]; strings.HasPrefix(name, "mcp-") && !own {
				server = name[4:]
			}
			if entry, listed := configured[server]; listed && !enabledServers[server] {
				why := "has no usable config entry"
				if _, isMap := entry.(map[string]any); isMap {
					why = "is disabled in config"
				}
				return nil, refuseText("toolsets", fmt.Sprintf("MCP server %s %s", pyrepr.Repr(server), why))
			}
		}
		for _, item := range explicit {
			if item == "desktop_ui" && source != "gui" {
				return nil, refuseText("toolsets", "desktop_ui is only available on the Desktop, not on "+source)
			}
		}
		section, err := setdefaultMap(cfg, "platform_toolsets")
		if err != nil {
			return nil, err
		}
		section[surface] = explicit
	}
	var posture []string
	if !hasExplicit && codingPosture {
		posture = tools.FocusToolsets(&tools.Env{Config: tree})
	}
	var enabled map[string]bool
	if posture != nil {
		enabled = make(map[string]bool, len(posture))
		for _, name := range posture {
			enabled[name] = true
		}
	} else {
		enabled = tools.Default.PlatformTools(ctx, "", tree, surface, tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
	}
	if hasExplicit {
		requested, available := map[string]bool{}, map[string]bool{}
		for _, item := range explicit {
			for _, t := range tools.Default.ResolveToolset(ctx, "", item.(string)) {
				requested[t] = true
			}
		}
		for name := range enabled {
			for _, t := range tools.Default.ResolveToolset(ctx, "", name) {
				available[t] = true
			}
		}
		var missing []string
		for t := range requested {
			if !available[t] {
				missing = append(missing, t)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, refuseText("toolsets", "not available on this surface: "+strings.Join(missing, ", "))
		}
	}
	if !hasExplicit && (source == "tui" || source == "gui") {
		agent, isMap := cfg["agent"].(map[string]any)
		if !isMap && pyrepr.Truthy(cfg["agent"]) {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(cfg["agent"]))
		}
		disabled := map[string]bool{}
		for _, name := range tools.ParseConfigStringList(agent["disabled_toolsets"]) {
			if name != "desktop_ui" {
				disabled[name] = true
			}
		}
		fold := map[string]bool{"project": true}
		if source == "gui" {
			fold = tools.ClientSurfaceToolsets
		}
		for name := range fold {
			if !disabled[name] {
				enabled[name] = true
			}
		}
	}
	if safeMode {
		for name := range tools.PluginToolsetKeys(ctx) {
			delete(enabled, name)
		}
	}
	return enabled, nil
}

// Whether the cwd selects the coding posture, a detection failure keeps the profile default
func inCodingWorkspace(ctx context.Context, cfg map[string]any, source, cwd string) (inFocus bool) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Debug(fmt.Sprintf("coding posture detection failed for %s; keeping the profile default", cwd), "error", rec)
			inFocus = false
		}
	}()
	return tools.InFocusWorkspace(&tools.Env{Home: home.Dir(ctx), Config: config.Tree(cfg)}, Surfaces[source], cwd)
}

// Validated skill identifiers of a create request, refused on a bypass launch that never reads the profile
func launchSkills(params map[string]any) ([]string, error) {
	raw, has := params["skills"]
	var list []any
	if has {
		items, isList := raw.([]any)
		bad := !isList || len(items) == 0
		for _, item := range items {
			if s, isS := item.(string); !isS || strings.TrimSpace(s) == "" {
				bad = true
			}
		}
		if bad {
			return nil, refuseText("skills", "must be a non-empty list of skill names")
		}
		list = items
	}
	if len(list) > 0 {
		for _, name := range BypassFields {
			if params[name] == true {
				return nil, refuseText("skills", "a bypass launch never reads the profile, so skills cannot be preloaded")
			}
		}
	}
	out := make([]string, len(list))
	for i, item := range list {
		out[i] = strings.TrimSpace(item.(string))
	}
	return out, nil
}

// Skill preload blocks rendered once at creation, refused when no named skill resolves
func renderLaunchSkills(ctx context.Context, params map[string]any, sessionID string) (*string, error) {
	names, err := launchSkills(params)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	scope := skills.NewScope(home.Dir(ctx), config.Tree(loadGatewayConfig(ctx, "")))
	prompt, loaded, missing := scope.BuildPreloadedSkillsPrompt(ctx, names, sessionID, nil)
	if len(loaded) == 0 {
		return nil, state.NewRuntimeStoreError("unknown_skill", nil, nil)
	}
	if len(missing) > 0 {
		slog.Warn(fmt.Sprintf("Skipping %s for session %s; continuing with: %s",
			skills.FormatMissingSkills(missing), sessionID, strings.Join(loaded, ", ")))
	}
	if prompt == "" {
		return nil, nil
	}
	return &prompt, nil
}

// Next turn toolsets, the profile selection for a following session without an explicit pick
func (p *LocalSessionPolicy) currentToolsets(ctx context.Context, cfg map[string]any) ([]string, error) {
	_, surface := Surfaces[p.Source]
	_, picked := policyObject(p.RequestJSON)["toolsets"]
	if !p.FollowsProfile() || !surface || p.EditorMCPJSON != nil || picked {
		return p.Toolsets, nil
	}
	enabled, err := resolveToolsets(ctx, map[string]any{}, cfg, p.Source, p.SafeMode, p.CodingPosture, false)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(enabled))
	for name := range enabled {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Agent run budget normalizer cmd/icebath installs from pkg/agent, nil panics on the first read
var NormalizeRunBudgetSeconds func(value any) *float64

// Frozen run budget seconds of the policy, nil for no budget
func (p *LocalSessionPolicy) RunBudgetSeconds(ctx context.Context) (*float64, error) {
	cfg, err := p.config(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	agent, _ := cfg["agent"].(map[string]any)
	return NormalizeRunBudgetSeconds(agent["run_budget_seconds"]), nil
}

// Frozen launch policy of the turn executing in ctx, nil for standalone serve and messaging turns
func activeLocalPolicy(ctx context.Context) *LocalSessionPolicy {
	policy, _ := ActivePolicy(ctx).(*LocalSessionPolicy)
	return policy
}
