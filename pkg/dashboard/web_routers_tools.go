package dashboard

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Built in terminal backend picker rows of web_server_profiles.py as name, label and description
var terminalBackends = [][3]string{
	{"local", "Local", "Run commands directly on this machine. No isolation."},
	{"docker", "Docker / Podman", "Run commands in an isolated Docker or Podman container with a persistent workspace."},
	{"modal", "Modal", "Run commands in a Modal cloud sandbox."},
	{"daytona", "Daytona", "Run commands in a Daytona cloud sandbox."},
	{"ssh", "SSH", "Run commands on a remote host over SSH."},
}

// Toolsets whose backends carry a model catalog, mapped to the config section holding their model key
var modelCatalogToolsets = map[string]string{"image_gen": "image_gen", "video_gen": "video_gen"}

// Tool environment of the home in ctx reading the given config
func toolsRouteEnv(ctx context.Context, cfg map[string]any) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), Config: config.Tree(cfg), TerminalScope: tools.GetTerminalScope(ctx)}
}

// Env value of a name for the home in ctx, empty on any failure
func toolsEnvValue(ctx context.Context, name string) (value string) {
	defer func() {
		if recover() != nil {
			value = ""
		}
	}()
	v, _, err := config.GetEnvValue(ctx, home.Dir(ctx), name)
	if err != nil {
		return ""
	}
	return v
}

// Terminal setting from config.yaml, falling back to its env var
func terminalCfgValue(ctx context.Context, terminalCfg map[string]any, key, envVar string) string {
	if value := terminalCfg[key]; value != nil {
		if text := pystr.Strip(pyrepr.Str(value)); text != "" {
			return text
		}
	}
	return pystr.Strip(toolsEnvValue(ctx, envVar))
}

// Built in picker rows then the plugin registered backends of the home in ctx
func terminalBackendRows(ctx context.Context) []*pyjson.Object {
	rows := make([]*pyjson.Object, 0, len(terminalBackends))
	for _, row := range terminalBackends {
		rows = append(rows, pyjson.ObjectOf("name", row[0], "label", row[1], "description", row[2]))
	}
	return append(rows, pluginTerminalBackendRows(ctx)...)
}

// Captured CLI run of the docker probe, the remote_common function hermes tests replace
var terminalRunCapture = tools.RunCapture

// Health of the docker backend through the runtime the agent resolves, probed with its version command
func probeDockerBackend(ctx context.Context) (string, string) {
	dockerExe := tools.FindDocker(toolsRouteEnv(ctx, nil))
	if dockerExe == "" {
		return "needs_setup", "Docker CLI not found — install Docker Desktop, docker-ce, or Podman."
	}
	runtimeName := tools.DockerRuntimeName(dockerExe)
	result, err := terminalRunCapture(ctx, []string{dockerExe, "version"}, 2*time.Second)
	if errors.Is(err, tools.ErrCaptureTimeout) {
		return "needs_setup", runtimeName + " not responding (timed out)."
	}
	if err != nil {
		return "unavailable", fmt.Sprintf("%s probe failed: %s", runtimeName, pypath.OSErrorText(err))
	}
	if result.Code == 0 {
		return "ready", ""
	}
	return "needs_setup", fmt.Sprintf("%s not reachable — %s.", runtimeName, tools.DockerRuntimeStartHint(dockerExe))
}

// Health of the ssh backend, ready once host and user are set
func probeSSHBackend(ctx context.Context, terminalCfg map[string]any) (string, string) {
	host := terminalCfgValue(ctx, terminalCfg, "ssh_host", "TERMINAL_SSH_HOST")
	user := terminalCfgValue(ctx, terminalCfg, "ssh_user", "TERMINAL_SSH_USER")
	var missing []string
	if host == "" {
		missing = append(missing, "terminal.ssh_host")
	}
	if user == "" {
		missing = append(missing, "terminal.ssh_user")
	}
	if len(missing) > 0 {
		return "needs_setup", fmt.Sprintf("Set %s in config.yaml (or the matching TERMINAL_SSH_* env vars).", strings.Join(missing, " and "))
	}
	return "ready", user + "@" + host
}

// Health of the modal backend, ready with direct credentials or both token env vars
func probeModalBackend(ctx context.Context) (string, string) {
	direct := func() (ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		return tools.HasDirectModalCredentials(toolsRouteEnv(ctx, nil))
	}()
	if direct {
		return "ready", ""
	}
	if toolsEnvValue(ctx, "MODAL_TOKEN_ID") != "" && toolsEnvValue(ctx, "MODAL_TOKEN_SECRET") != "" {
		return "ready", ""
	}
	return "needs_setup", "Modal credentials not found — set MODAL_TOKEN_ID and MODAL_TOKEN_SECRET (or run `modal setup`)."
}

// Health of the daytona backend, ready once its API key is set
func probeDaytonaBackend(ctx context.Context) (string, string) {
	if toolsEnvValue(ctx, "DAYTONA_API_KEY") != "" {
		return "ready", ""
	}
	return "needs_setup", "Set DAYTONA_API_KEY to use the Daytona backend."
}

// Status and detail of one terminal backend, a failing probe reported as unavailable
func probeTerminalBackend(ctx context.Context, name string, terminalCfg map[string]any) (status, detail string) {
	defer func() {
		if rec := recover(); rec != nil {
			status, detail = "unavailable", fmt.Sprintf("Probe failed: %v", rec)
		}
	}()
	switch name {
	case "local":
		return "ready", ""
	case "docker":
		return probeDockerBackend(ctx)
	case "ssh":
		return probeSSHBackend(ctx, terminalCfg)
	case "modal":
		return probeModalBackend(ctx)
	case "daytona":
		return probeDaytonaBackend(ctx)
	}
	probed := false
	func() {
		defer func() { _ = recover() }()
		if p := tools.PluginTerminalBackend(ctx, name, ""); p != nil {
			status, detail = p.Probe()
			probed = true
		}
	}()
	if probed {
		return status, detail
	}
	return "unavailable", "Unknown backend: " + name
}

// Visible provider rows of the category of a toolset under a fresh entitlement read
func categoryProviders(setup *tools.ToolsSetup, tsKey string, cfg map[string]any) ([]*pyjson.Object, error) {
	cat := tools.ToolCategory(tsKey)
	if cat == nil || cat.Len() == 0 {
		return nil, nil
	}
	return setup.VisibleProviders(cat, config.Tree(cfg), true, nil)
}

// Provider picker row of a name, the active row when no name is given, nil when none matches
func findToolsetProviderRow(setup *tools.ToolsSetup, tsKey string, cfg map[string]any, providerName string) (*pyjson.Object, error) {
	rows, err := categoryProviders(setup, tsKey, cfg)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		if providerName != "" {
			if p.GetOr("name", nil) == providerName {
				return p, nil
			}
			continue
		}
		active, err := setup.IsProviderActive(p, config.Tree(cfg), true)
		if err != nil {
			return nil, err
		}
		if active {
			return p, nil
		}
	}
	return nil, nil
}

// 400 for a toolset key outside the effective configurable set of the home in ctx
func requireKnownToolset(ctx context.Context, name string) error {
	for _, ts := range tools.EffectiveConfigurableToolsets(ctx) {
		if ts.Key == name {
			return nil
		}
	}
	return toolsBadRequest("Unknown toolset: " + name)
}

// A config section as a dict, a non dict value replaced in place
func toolsDictSection(cfg map[string]any, key string) map[string]any {
	section, ok := cfg[key].(map[string]any)
	if !ok {
		section = map[string]any{}
		cfg[key] = section
	}
	return section
}

func toolsBadRequest(detail string) error { return NewHTTPException(400, detail) }

// Payload of a toolset without a model catalog
func toolsNoModels(name string) *pyjson.Object {
	return pyjson.ObjectOf("name", name, "has_models", false, "models", []any{}, "current", nil, "default", nil)
}

// Display label of a platform, a built in one first then a plugin registered one, else the default
func toolsPlatformLabel(ctx context.Context, key, def string) (label string) {
	if info, ok := tools.PlatformInfoFor(key); ok {
		return info.Label
	}
	defer func() {
		if recover() != nil {
			label = def
		}
	}()
	if entry := gateway.Registry.Get(ctx, key); entry != nil {
		if entry.Emoji != "" {
			return entry.Emoji + "  " + entry.Label
		}
		return entry.Label
	}
	return def
}

// Body profile when set, else the query profile
func toolsScopeProfile(body *pyjson.Object, c *Call) string {
	if p, ok := body.GetOr("profile", nil).(string); ok && p != "" {
		return p
	}
	return profileQuery(c)
}

// Toolset rows of the home in ctx with their enabled and configured state
func getToolsets(ctx context.Context, c *Call) (any, error) {
	type toolsetsRead struct {
		cfg               map[string]any
		rows              []tools.ConfigurableToolset
		enabledByPlatform map[string]map[string]bool
		configured        map[string]bool
	}
	read, err := func() (*toolsetsRead, error) {
		sctx, _, release, err := profileScope(ctx, profileQuery(c))
		if err != nil {
			return nil, err
		}
		defer release()
		cfg, err := webLoadConfig(sctx)
		if err != nil {
			return nil, err
		}
		setup := tools.NewToolsSetup(sctx, config.Tree(cfg))
		out := &toolsetsRead{cfg: cfg, rows: tools.EffectiveConfigurableToolsets(sctx), enabledByPlatform: map[string]map[string]bool{}, configured: map[string]bool{}}
		for _, ts := range out.rows {
			platform := tools.ToolsetConfigurationPlatform(ts.Key, "cli")
			if _, seen := out.enabledByPlatform[platform]; !seen {
				out.enabledByPlatform[platform] = setup.CurrentPlatformTools(config.Tree(cfg), platform)
			}
		}
		features := tools.ToolsConfigHooks.NousFeatures(sctx, home.Dir(sctx), cfg, false)
		for _, ts := range out.rows {
			configured, err := setup.ToolsetHasKeys(ts.Key, config.Tree(cfg), &features)
			if err != nil {
				return nil, err
			}
			out.configured[ts.Key] = configured
		}
		return out, nil
	}()
	if err != nil {
		return nil, err
	}
	result := []any{}
	for _, ts := range read.rows {
		toolNames := func() (names []any) {
			defer func() {
				if recover() != nil {
					names = []any{}
				}
			}()
			return stringsAny(slices.Compact(slices.Sorted(slices.Values(tools.Default.ResolveToolset(ctx, "", ts.Key)))))
		}()
		targetPlatform := tools.ToolsetConfigurationPlatform(ts.Key, "cli")
		isEnabled := false
		if tools.ConfigOnlyToolsets[ts.Key] {
			section, _ := read.cfg[ts.Key].(map[string]any)
			enabled, present := section["enabled"]
			isEnabled = !present || enabled == nil || provider.IsTruthyValue(enabled)
		} else {
			isEnabled = read.enabledByPlatform[targetPlatform][ts.Key]
		}
		result = append(result, pyjson.ObjectOf(
			"name", ts.Key, "label", tools.GUIToolsetLabel(ts.Label), "description", ts.Description,
			"platform", targetPlatform,
			"platform_label", tools.GUIToolsetLabel(toolsPlatformLabel(ctx, targetPlatform, targetPlatform)),
			"enabled", isEnabled, "available", isEnabled,
			"configured", read.configured[ts.Key], "tools", toolNames))
	}
	return result, nil
}

// Post setup key of the first visible toolset provider whose install has not run, empty when none is pending
func pendingInstallKey(ctx context.Context, name, scopeProfile string) (string, error) {
	cat := tools.ToolCategory(name)
	if cat == nil || cat.Len() == 0 {
		return "", nil
	}
	sctx, _, release, err := profileScope(ctx, scopeProfile)
	if err != nil {
		return "", err
	}
	defer release()
	cfg, err := webLoadConfig(sctx)
	if err != nil {
		return "", err
	}
	setup := tools.NewToolsSetup(sctx, config.Tree(cfg))
	rows, err := setup.VisibleProviders(cat, config.Tree(cfg), false, nil)
	if err != nil {
		return "", err
	}
	for _, prov := range rows {
		if key := prov.GetOr("post_setup", nil); pyrepr.Truthy(key) && !setup.PostSetupAlreadyInstalled(pyrepr.Str(key)) {
			return pyrepr.Str(key), nil
		}
	}
	return "", nil
}

// Enables or disables a toolset on its configuration platform and starts a pending provider install on enable
func toggleToolset(ctx context.Context, c *Call) (any, error) {
	name, body := c.Str("name"), c.Object("body")
	enabledFlag, _ := body.GetOr("enabled", nil).(bool)
	targetPlatform := tools.ToolsetConfigurationPlatform(name, "cli")
	scopeProfile := toolsScopeProfile(body, c)
	err := func() error {
		wctx, release, err := configWriteScope(ctx, scopeProfile)
		if err != nil {
			return err
		}
		defer release()
		if err := requireKnownToolset(wctx, name); err != nil {
			return err
		}
		cfg, err := webLoadConfig(wctx)
		if err != nil {
			return err
		}
		if tools.ConfigOnlyToolsets[name] {
			toolsDictSection(cfg, name)["enabled"] = enabledFlag
			return webSaveConfig(wctx, cfg, config.SaveOptions{})
		}
		setup := tools.NewToolsSetup(wctx, config.Tree(cfg))
		enabled := map[string]bool{}
		for k, on := range setup.CurrentPlatformTools(config.Tree(cfg), targetPlatform) {
			if on {
				enabled[k] = true
			}
		}
		if enabledFlag {
			enabled[name] = true
		} else {
			delete(enabled, name)
		}
		return setup.SavePlatformTools(config.Tree(cfg), targetPlatform, enabled)
	}()
	if err != nil {
		return nil, err
	}
	var postSetupStarted any
	if enabledFlag && !tools.ConfigOnlyToolsets[name] {
		err := func() (err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			pendingKey, err := pendingInstallKey(ctx, name, scopeProfile)
			if err != nil || pendingKey == "" {
				return err
			}
			args, err := profileCLIArgs(scopeProfile)
			if err != nil {
				return err
			}
			if _, err := spawnHermesAction(ctx, append(args, "tools", "post-setup", pendingKey), "tools-post-setup", nil); err != nil {
				return err
			}
			postSetupStarted = pendingKey
			return nil
		}()
		if err != nil {
			routersLog().Error(fmt.Sprintf("install-on-enable post-setup spawn failed for %s", name), "exc_info", err)
		}
	}
	return pyjson.ObjectOf("ok", true, "name", name, "platform", targetPlatform, "enabled", enabledFlag, "post_setup_started", postSetupStarted), nil
}

// Provider matrix and key status of the config panel of a toolset
func getToolsetConfig(ctx context.Context, c *Call) (any, error) {
	name := c.Str("name")
	sctx, _, release, err := profileScope(ctx, profileQuery(c))
	if err != nil {
		return nil, err
	}
	defer release()
	if err := requireKnownToolset(sctx, name); err != nil {
		return nil, err
	}
	cfg, err := webLoadConfig(sctx)
	if err != nil {
		return nil, err
	}
	h := home.Dir(sctx)
	setup := tools.NewToolsSetup(sctx, config.Tree(cfg))
	cat := tools.ToolCategory(name)
	providers := []any{}
	var activeProvider any
	if cat != nil && cat.Len() > 0 {
		features := tools.ToolsConfigHooks.NousFeatures(sctx, h, cfg, true)
		rows, err := setup.VisibleProviders(cat, config.Tree(cfg), true, nil)
		if err != nil {
			return nil, err
		}
		for _, prov := range rows {
			envVars := []any{}
			declared, _ := prov.GetOr("env_vars", nil).([]any)
			for _, raw := range declared {
				e, ok := raw.(*pyjson.Object)
				if !ok {
					return nil, &routePyError{class: "TypeError", msg: fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(raw))}
				}
				key, held := e.Get("key")
				if !held {
					return nil, &routePyError{class: "KeyError", msg: "'key'"}
				}
				value, _, err := config.GetEnvValue(sctx, h, pyrepr.Str(key))
				if err != nil {
					return nil, err
				}
				envVars = append(envVars, pyjson.ObjectOf("key", key, "prompt", e.GetOr("prompt", key),
					"url", e.GetOr("url", nil), "default", e.GetOr("default", nil), "is_set", value != ""))
			}
			isActive, err := setup.IsProviderActive(prov, config.Tree(cfg), true)
			if err != nil {
				return nil, err
			}
			provName, held := prov.Get("name")
			if !held {
				return nil, &routePyError{class: "KeyError", msg: "'name'"}
			}
			if isActive && activeProvider == nil {
				activeProvider = provName
			}
			status, err := setup.ProviderReadinessStatus(prov, config.Tree(cfg), &features, &isActive)
			if err != nil {
				return nil, err
			}
			row := pyjson.ObjectOf(
				"name", provName,
				"badge", prov.GetOr("badge", ""),
				"tag", prov.GetOr("tag", ""),
				"env_vars", envVars,
				"post_setup", prov.GetOr("post_setup", nil),
				"requires_nous_auth", pyrepr.Truthy(prov.GetOr("requires_nous_auth", nil)),
				"managed_nous_feature", prov.GetOr("managed_nous_feature", nil),
				"is_active", isActive,
				"status", status)
			if backend := prov.GetOr("web_backend", nil); name == "web" && pyrepr.Truthy(backend) {
				row.Set("web_backend", backend)
				row.Set("capabilities", stringsAny(setup.WebProviderCapabilities(pyrepr.Str(backend))))
			}
			if ttsProvider := prov.GetOr("tts_provider", nil); name == "tts" && pyrepr.Truthy(ttsProvider) {
				row.Set("tts_provider", ttsProvider)
			}
			providers = append(providers, row)
		}
	}
	payload := pyjson.ObjectOf("name", name, "has_category", cat != nil, "providers", providers, "active_provider", activeProvider)
	if name == "web" {
		var searchBackend, extractBackend any
		searchManaged, extractManaged := false, false
		func() {
			defer func() {
				if recover() != nil {
					searchBackend, extractBackend, searchManaged, extractManaged = nil, nil, false, false
				}
			}()
			env := toolsRouteEnv(sctx, cfg)
			search, extract := tools.GetSearchBackend(env), tools.GetExtractBackend(env)
			managedSearch := tools.ManagedWebSearch(env) || (search == "firecrawl" && tools.FirecrawlIsManagedRoute(env, "search"))
			managedExtract := extract == "firecrawl" && tools.FirecrawlIsManagedRoute(env, "extract")
			searchBackend, extractBackend, searchManaged, extractManaged = search, extract, managedSearch, managedExtract
		}()
		payload.Set("active_search_backend", searchBackend)
		payload.Set("active_extract_backend", extractBackend)
		payload.Set("search_via_nous", searchManaged)
		payload.Set("extract_via_nous", extractManaged)
	}
	return payload, nil
}

// Model catalog of a toolset backend, the named provider row or the active one
func getToolsetModels(ctx context.Context, c *Call) (any, error) {
	name := c.Str("name")
	section, ok := modelCatalogToolsets[name]
	if !ok {
		return toolsNoModels(name), nil
	}
	providerName, _ := c.Value("provider").(string)
	sctx, _, release, err := profileScope(ctx, profileQuery(c))
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, err := webLoadConfig(sctx)
	if err != nil {
		return nil, err
	}
	setup := tools.NewToolsSetup(sctx, config.Tree(cfg))
	row, err := findToolsetProviderRow(setup, name, cfg, providerName)
	if err != nil {
		return nil, err
	}
	plugin := ""
	if row != nil {
		plugin = tools.ResolveToolsetModelPlugin(name, row)
	}
	if plugin == "" {
		return toolsNoModels(name), nil
	}
	ids, meta, defaultModel := setup.ToolsetModelCatalog(name, plugin, config.Tree(cfg))
	var current any
	if sectionCfg, ok := cfg[section].(map[string]any); ok {
		if raw, ok := sectionCfg["model"].(string); ok && pystr.Strip(raw) != "" {
			current = pystr.Strip(raw)
		}
	}
	inCatalog := func(id any) bool {
		s, ok := id.(string)
		if !ok {
			return false
		}
		_, held := meta[s]
		return held
	}
	var defaultValue any
	if defaultModel != "" {
		defaultValue = defaultModel
	}
	if !inCatalog(current) {
		current = nil
		if inCatalog(defaultValue) {
			current = defaultValue
		}
	}
	models := []any{}
	for _, id := range ids {
		m := meta[id]
		field := func(key string, def any) any {
			if v, ok := m[key]; ok {
				return v
			}
			return def
		}
		models = append(models, pyjson.ObjectOf("id", id, "display", field("display", id), "speed", field("speed", ""),
			"strengths", field("strengths", ""), "price", field("price", "")))
	}
	return pyjson.ObjectOf("name", name, "has_models", len(models) > 0, "provider", row.GetOr("name", nil), "plugin", plugin,
		"models", models, "current", current, "default", defaultValue), nil
}

// Persists the model selection of a toolset backend after checking it against the backend catalog
func selectToolsetModel(ctx context.Context, c *Call) (any, error) {
	name, body := c.Str("name"), c.Object("body")
	section, ok := modelCatalogToolsets[name]
	if !ok {
		return nil, toolsBadRequest("Toolset has no model catalog: " + name)
	}
	modelID := pystr.Strip(pyrepr.Str(pyrepr.Or(body.GetOr("model", nil), "")))
	if modelID == "" {
		return nil, toolsBadRequest("model is required")
	}
	providerName, _ := body.GetOr("provider", nil).(string)
	wctx, release, err := configWriteScope(ctx, toolsScopeProfile(body, c))
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, err := webLoadConfig(wctx)
	if err != nil {
		return nil, err
	}
	setup := tools.NewToolsSetup(wctx, config.Tree(cfg))
	row, err := findToolsetProviderRow(setup, name, cfg, providerName)
	if err != nil {
		return nil, err
	}
	plugin := ""
	if row != nil {
		plugin = tools.ResolveToolsetModelPlugin(name, row)
	}
	if plugin == "" {
		return nil, toolsBadRequest("No model-capable backend is active for " + name)
	}
	_, meta, _ := setup.ToolsetModelCatalog(name, plugin, config.Tree(cfg))
	if _, held := meta[modelID]; !held {
		return nil, toolsBadRequest(fmt.Sprintf("Unknown model %s for backend %s", pyrepr.Repr(modelID), pyrepr.Repr(plugin)))
	}
	toolsDictSection(cfg, section)["model"] = modelID
	if err := webSaveConfig(wctx, cfg, config.SaveOptions{}); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "name", name, "model", modelID, "plugin", plugin), nil
}

// Persists a provider selection of a toolset, one web capability when named, and reports a missing Nous entitlement
func selectToolsetProvider(ctx context.Context, c *Call) (any, error) {
	name, body := c.Str("name"), c.Object("body")
	providerName := pyrepr.Str(body.GetOr("provider", nil))
	capability, hasCapability := body.GetOr("capability", nil).(string)
	if hasCapability {
		if name != "web" {
			return nil, toolsBadRequest("capability selection is only supported for the web toolset")
		}
		if capability != "search" && capability != "extract" {
			return nil, toolsBadRequest(fmt.Sprintf("Unknown capability: %s (expected 'search' or 'extract')", pyrepr.Repr(capability)))
		}
	}
	sctx, _, release, err := profileScope(ctx, toolsScopeProfile(body, c))
	if err != nil {
		return nil, err
	}
	defer release()
	if err := requireKnownToolset(sctx, name); err != nil {
		return nil, err
	}
	providerRow := func(setup *tools.ToolsSetup, cfg map[string]any) (*pyjson.Object, error) {
		rows, err := categoryProviders(setup, name, cfg)
		if err != nil {
			return nil, err
		}
		for _, p := range rows {
			if p.GetOr("name", nil) == providerName {
				return p, nil
			}
		}
		return nil, nil
	}
	var cfg map[string]any
	var setup *tools.ToolsSetup
	response := pyjson.NewObject()
	err = func() error {
		lctx, unlock := provider.LockConfigMutation(sctx)
		defer unlock()
		var err error
		if cfg, err = webLoadConfig(lctx); err != nil {
			return err
		}
		setup = tools.NewToolsSetup(sctx, config.Tree(cfg))
		if hasCapability {
			prov, err := providerRow(setup, cfg)
			if err != nil {
				return err
			}
			if prov == nil {
				return toolsBadRequest(fmt.Sprintf("Unknown provider %s for toolset %s", pyrepr.Repr(providerName), pyrepr.Repr(name)))
			}
			backend := prov.GetOr("web_backend", nil)
			if !pyrepr.Truthy(backend) {
				return toolsBadRequest(fmt.Sprintf("Provider %s has no web backend key", pyrepr.Repr(providerName)))
			}
			if !slices.Contains(setup.WebProviderCapabilities(pyrepr.Str(backend)), capability) {
				return toolsBadRequest(fmt.Sprintf("%s does not support %s", providerName, capability))
			}
			if pyrepr.Truthy(prov.GetOr("managed_nous_feature", nil)) {
				backend = tools.NousManagedProvider
			}
			toolsDictSection(cfg, "web")[capability+"_backend"] = backend
		} else if err := setup.ApplyProviderSelection(name, providerName, config.Tree(cfg)); err != nil {
			var selection *tools.ProviderSelectionError
			if errors.As(err, &selection) {
				return toolsBadRequest(strings.Trim(selection.Error(), `"`))
			}
			return err
		}
		if err := webSaveConfig(lctx, cfg, config.SaveOptions{}); err != nil {
			return err
		}
		response.Set("ok", true)
		response.Set("name", name)
		response.Set("provider", providerName)
		if hasCapability {
			response.Set("capability", capability)
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	row, err := providerRow(setup, cfg)
	if err != nil {
		return nil, err
	}
	var managedFeature any
	if row != nil {
		managedFeature = row.GetOr("managed_nous_feature", nil)
	}
	if pyrepr.Truthy(managedFeature) {
		features := tools.ToolsConfigHooks.NousFeatures(sctx, home.Dir(sctx), cfg, true)
		entitled := false
		if acct := features.Account; acct != nil && acct.LoggedIn {
			if category, _ := tools.ToolsConfigHooks.ManagedCoverageCategory(pyrepr.Str(managedFeature)); category != "" {
				entitled = acct.EntitledFor(category)
			} else {
				entitled = acct.ToolGatewayEntitled
			}
		}
		if !entitled {
			response.Set("needs_nous_auth", true)
			response.Set("feature", managedFeature)
		}
	}
	return response, nil
}

// Persists API keys of a toolset's provider rows to the env store, a blank value leaving its key unchanged
func saveToolsetEnv(ctx context.Context, c *Call) (any, error) {
	name, body := c.Str("name"), c.Object("body")
	env, _ := body.GetOr("env", nil).(*pyjson.Object)
	sctx, _, release, err := profileScope(ctx, toolsScopeProfile(body, c))
	if err != nil {
		return nil, err
	}
	defer release()
	if err := requireKnownToolset(sctx, name); err != nil {
		return nil, err
	}
	cfg, err := webLoadConfig(sctx)
	if err != nil {
		return nil, err
	}
	rows, err := categoryProviders(tools.NewToolsSetup(sctx, config.Tree(cfg)), name, cfg)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, prov := range rows {
		declared, _ := prov.GetOr("env_vars", nil).([]any)
		for _, raw := range declared {
			if e, ok := raw.(*pyjson.Object); ok {
				allowed[pyrepr.Str(e.GetOr("key", nil))] = true
			}
		}
	}
	var unknown []string
	for _, k := range env.Keys {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return nil, toolsBadRequest(fmt.Sprintf("Unknown env var(s) for toolset %s: %s", name, strings.Join(unknown, ", ")))
	}
	h := home.Dir(sctx)
	saved, skipped := []any{}, []any{}
	for _, key := range env.Keys {
		value, _ := env.Vals[key].(string)
		if pystr.Strip(value) == "" {
			skipped = append(skipped, key)
			continue
		}
		if err := config.SaveEnvValueContext(sctx, h, key, pystr.Strip(value)); err != nil {
			if memoryIsValueError(err) {
				return nil, toolsBadRequest(memoryExcStr(err))
			}
			return nil, err
		}
		saved = append(saved, key)
	}
	status := pyjson.NewObject()
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for k := range allowed {
			if !yield(k) {
				return
			}
		}
	}) {
		value, _, err := config.GetEnvValue(sctx, h, key)
		if err != nil {
			return nil, err
		}
		status.Set(key, value != "")
	}
	return pyjson.ObjectOf("ok", true, "name", name, "saved", saved, "skipped", skipped, "is_set", status), nil
}

// Post setup keys any visible provider declares for the home in ctx, the tools_config function hermes tests replace
var validPostSetupKeys = func(ctx context.Context) map[string]bool {
	return tools.NewToolsSetup(ctx, nil).ValidPostSetupKeys()
}

// Spawns the post setup install of a provider key as a background action under the profile
func runToolsetPostSetup(ctx context.Context, c *Call) (any, error) {
	name, body := c.Str("name"), c.Object("body")
	key := pyrepr.Str(body.GetOr("key", nil))
	scopeProfile := toolsScopeProfile(body, c)
	_, err := scopedToThread(ctx, scopeProfile, func(sctx context.Context) (any, error) {
		if err := requireKnownToolset(sctx, name); err != nil {
			return nil, err
		}
		if !validPostSetupKeys(sctx)[key] {
			return nil, toolsBadRequest("Unknown post-setup key: " + key)
		}
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	result, err := spawnProfileAction(ctx, scopeProfile, []string{"tools", "post-setup", key}, "tools-post-setup",
		"Failed to spawn tools post-setup", "Failed to run post-setup")
	if err != nil {
		return nil, err
	}
	result.Set("key", key)
	return result, nil
}

// Terminal backend rows of the home in ctx with the health of each one
func getTerminalBackends(ctx context.Context, c *Call) (any, error) {
	sctx, _, release, err := profileScope(ctx, profileQuery(c))
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, err := webLoadConfig(sctx)
	if err != nil {
		return nil, err
	}
	terminalCfg, ok := cfg["terminal"].(map[string]any)
	if !ok {
		terminalCfg = map[string]any{}
	}
	rows := terminalBackendRows(sctx)
	active := pystr.Lower(pystr.Strip(pyrepr.Str(pyrepr.Or(terminalCfg["backend"], "local"))))
	known := false
	for _, row := range rows {
		known = known || row.GetOr("name", nil) == active
	}
	if !known {
		active = "local"
	}
	backends := []any{}
	for _, row := range rows {
		rowName := pyrepr.Str(row.GetOr("name", nil))
		status, detail := probeTerminalBackend(sctx, rowName, terminalCfg)
		backends = append(backends, pyjson.ObjectOf("name", rowName, "label", row.GetOr("label", nil), "description", row.GetOr("description", nil),
			"active", rowName == active, "status", status, "detail", detail))
	}
	return pyjson.ObjectOf("active", active, "backends", backends), nil
}

// Persists the terminal backend of the profile after checking it against the rows of that profile
func selectTerminalBackend(ctx context.Context, c *Call) (any, error) {
	body := c.Object("body")
	submitted := pyrepr.Str(pyrepr.Or(body.GetOr("backend", nil), ""))
	backend := pystr.Lower(pystr.Strip(submitted))
	err := func() error {
		wctx, release, err := configWriteScope(ctx, toolsScopeProfile(body, c))
		if err != nil {
			return err
		}
		defer release()
		var names []string
		for _, row := range terminalBackendRows(wctx) {
			if n := pyrepr.Str(row.GetOr("name", nil)); !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
		if !slices.Contains(names, backend) {
			slices.Sort(names)
			return toolsBadRequest(fmt.Sprintf("Unknown terminal backend: %s. Use one of: %s", pyrepr.Repr(submitted), strings.Join(names, ", ")))
		}
		cfg, err := webLoadConfig(wctx)
		if err != nil {
			return err
		}
		toolsDictSection(cfg, "terminal")["backend"] = backend
		return webSaveConfig(wctx, cfg, config.SaveOptions{})
	}()
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "backend", backend), nil
}

// Routes of hermes_cli/web_routers/tools.py in declaration order
func toolsRouter() *APIRouter {
	r := NewAPIRouter("")
	namePath := PathParam("name", Str)
	optProfile := QueryParam("profile", Optional(Str), nil)
	r.Get("/api/tools/toolsets", &Endpoint{Name: "get_toolsets", Params: []Param{optProfile}, Handle: getToolsets})
	r.Put("/api/tools/toolsets/{name}", &Endpoint{Name: "toggle_toolset",
		Doc:    "Enable/disable a configurable toolset for its configuration platform\n(``platform_toolsets.cli`` for most; platform-restricted toolsets target\ntheir own platform) via the same ``_save_platform_tools`` the CLI uses.",
		Params: []Param{namePath, optProfile}, Body: []BodyParam{Body("body", toolsetToggleModel)}, Handle: toggleToolset})
	r.Get("/api/tools/toolsets/{name}/config", &Endpoint{Name: "get_toolset_config",
		Doc:    "Provider matrix + key status for a toolset's config panel (the CLI picker\nrows, each env var annotated ``is_set``); no category -> ``has_category: false``.",
		Params: []Param{namePath, optProfile}, Handle: getToolsetConfig})
	r.Get("/api/tools/toolsets/{name}/models", &Endpoint{Name: "get_toolset_models",
		Doc:    "Model catalog for a toolset backend (image/video gen) — the GUI\ncounterpart of the CLI model picker.  ``provider`` names a picker row\n(default: the active provider); no catalog -> ``has_models: false``.",
		Params: []Param{namePath, QueryParam("provider", Optional(Str), nil), optProfile}, Handle: getToolsetModels})
	r.Put("/api/tools/toolsets/{name}/model", &Endpoint{Name: "select_toolset_model",
		Doc:    "Persist a backend model selection (``image_gen.model`` /\n``video_gen.model``), validated against the resolved backend's catalog.",
		Params: []Param{namePath, optProfile}, Body: []BodyParam{Body("body", toolsetModelSelectModel)}, Handle: selectToolsetModel})
	r.Put("/api/tools/toolsets/{name}/provider", &Endpoint{Name: "select_toolset_provider",
		Doc: "Persist a provider selection via ``apply_provider_selection`` (shared with\n``hermes tools``, so both write identical keys).\n\n" +
			"``web`` only: ``capability`` ('search' | 'extract') writes\n``web.<capability>_backend`` (the override the dispatchers resolve first);\n" +
			"omitted -> legacy ``web.backend``.  Managed Nous rows report Portal\nentitlement (``needs_nous_auth`` + ``feature``): the GUI has no inline\n" +
			"login, so an unentitled selection would write config and never activate.",
		Params: []Param{namePath, optProfile}, Body: []BodyParam{Body("body", toolsetProviderSelectModel)}, Handle: selectToolsetProvider})
	r.Put("/api/tools/toolsets/{name}/env", &Endpoint{Name: "save_toolset_env",
		Doc:    "Persist API keys to ``.env`` via ``save_env_value``.  Keys are validated\nagainst the union of the category's visible-provider ``env_vars`` so this\ncan't write arbitrary env vars; a blank value means \"leave unchanged\".",
		Params: []Param{namePath, optProfile}, Body: []BodyParam{Body("body", toolsetEnvUpdateModel)}, Handle: saveToolsetEnv})
	r.Post("/api/tools/toolsets/{name}/post-setup", &Endpoint{Name: "run_toolset_post_setup",
		Doc:    "Spawn ``hermes tools post-setup <key>`` (long-running installs) as a\nbackground action tailed via ``GET /api/actions/tools-post-setup/status``;\n``profile`` is threaded so hooks see the drawer's HERMES_HOME.",
		Params: []Param{namePath, optProfile}, Body: []BodyParam{Body("body", toolsetPostSetupModel)}, Handle: runToolsetPostSetup})
	r.Get("/api/tools/terminal/backends", &Endpoint{Name: "get_terminal_backends",
		Doc:    "Terminal backend rows with health probes: ``status`` is ``ready`` /\n``needs_setup`` / ``unavailable``; a probe failure is a status, never an\nerror response.",
		Params: []Param{optProfile}, Handle: getTerminalBackends})
	r.Put("/api/tools/terminal/backend", &Endpoint{Name: "select_terminal_backend",
		Doc:    "Persist ``terminal.backend``.  A backend that still needs setup is\nallowed — the picker shows guidance instead of blocking, like the CLI.\nValidated inside the target profile's scope, like the GET: plugin backends\nare registered per profile.",
		Params: []Param{optProfile}, Body: []BodyParam{Body("body", terminalBackendSelectModel)}, Handle: selectTerminalBackend})
	r.Get("/api/tools/computer-use/status", &Endpoint{Name: "get_computer_use_status",
		Doc:    "Computer Use readiness for the desktop card (payload shape: see\n``tools.computer_use.permissions.computer_use_status``).",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return scopedToThread(ctx, profileQuery(c), func(sctx context.Context) (any, error) {
				return codeexec.ComputerUseStatus(toolsRouteEnv(sctx, nil))
			})
		}})
	r.Post("/api/tools/computer-use/permissions/grant", &Endpoint{Name: "grant_computer_use_permissions",
		Doc:    "Spawn ``hermes computer-use permissions grant`` (macOS-only: launches\nCuaDriver via LaunchServices so the TCC dialog is attributed correctly).\nThe frontend polls ``GET /api/actions/computer-use-grant/status``.",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			if runtime.GOOS != "darwin" {
				return nil, toolsBadRequest("Computer Use permission grants are a macOS concept.")
			}
			return spawnProfileAction(ctx, profileQuery(c), []string{"computer-use", "permissions", "grant"}, "computer-use-grant",
				"Failed to spawn computer-use permissions grant", "Failed to request permissions")
		}})
	return r
}
