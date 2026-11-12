package dashboard

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

const dashboardFontDefaultID = "theme"

// Curated font override ids the picker may persist, in step with the frontend font choices
var dashboardFontChoices = map[string]bool{
	"system-sans": true, "system-serif": true, "system-mono": true,
	"inter": true, "ibm-plex-sans": true, "work-sans": true, "atkinson-hyperlegible": true, "dm-sans": true,
	"spectral": true, "fraunces": true, "source-serif": true,
	"jetbrains-mono": true, "ibm-plex-mono": true, "space-mono": true,
}

// Browser asset suffixes a dashboard plugin may serve, every other suffix answering 404
var pluginAssetContentTypes = map[string]string{
	".js": "application/javascript", ".mjs": "application/javascript", ".css": "text/css",
	".json": "application/json", ".html": "text/html", ".svg": "image/svg+xml",
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".ico": "image/x-icon", ".woff2": "font/woff2", ".woff": "font/woff",
	".ttf": "font/ttf", ".otf": "font/otf", ".map": "application/json",
}

// Install and update actions of the agent plugin routes, the plugins_cmd names hermes tests patch
var (
	uiDashboardInstallPlugin    = plugins.DashboardInstallPlugin
	uiDashboardUpdateUserPlugin = plugins.DashboardUpdateUserPlugin
)

// Python item assignment error of a config value that is no mapping
func notSubscriptable(v any) error {
	return &pyjson.TypeError{Msg: fmt.Sprintf("'%s' object does not support item assignment", pyrepr.TypeName(v))}
}

// Writes dashboard.key into the config of the profile under the config mutation lock
func setDashboardKey(ctx context.Context, key string, value any, profile string) error {
	sctx, _, err := configProfileScope(ctx, profile)
	if err != nil {
		return err
	}
	lctx, unlock := provider.LockConfigMutation(sctx)
	defer unlock()
	cfg, err := webLoadConfig(lctx)
	if err != nil {
		return err
	}
	if _, held := cfg["dashboard"]; !held {
		setConfigKey(cfg, "dashboard", map[string]any{})
	}
	section, ok := cfg["dashboard"].(map[string]any)
	if !ok {
		return notSubscriptable(cfg["dashboard"])
	}
	setConfigKey(section, key, value)
	return webSaveConfig(lctx, cfg, config.SaveOptions{})
}

// Config value at a key path, def when any key of the path is absent
func cfgGetDefault(cfg map[string]any, def any, keys ...string) any {
	if v, ok := config.CfgGet(cfg, keys...); ok {
		return v
	}
	return def
}

// Built in and user themes with the active theme name of the profile
func getDashboardThemes(ctx context.Context, profile string) (any, error) {
	return configScopedToThread(ctx, profile, func(sctx context.Context) (any, error) {
		cfg, err := webLoadConfig(sctx)
		if err != nil {
			return nil, err
		}
		active := cfgGetDefault(cfg, "default", "dashboard", "theme")
		themes := make([]any, 0, len(builtinDashboardThemes))
		seen := map[any]bool{}
		for _, t := range builtinDashboardThemes {
			themes = append(themes, t)
			seen[t.GetOr("name", nil)] = true
		}
		for _, t := range discoverUserThemes() {
			name := t.GetOr("name", nil)
			if seen[name] {
				continue
			}
			themes = append(themes, pyjson.ObjectOf("name", name, "label", t.GetOr("label", nil),
				"description", t.GetOr("description", nil), "definition", t))
			seen[name] = true
		}
		return pyjson.ObjectOf("themes", themes, "active", configJSON(active)), nil
	})
}

// Active font override of the profile, the theme font for an unset or unvetted id
func getDashboardFont(ctx context.Context, profile string) (any, error) {
	return configScopedToThread(ctx, profile, func(sctx context.Context) (any, error) {
		cfg, err := webLoadConfig(sctx)
		if err != nil {
			return nil, err
		}
		raw := cfgGetDefault(cfg, dashboardFontDefaultID, "dashboard", "font")
		switch raw.(type) {
		case map[string]any, []any:
			return nil, &pyjson.TypeError{Msg: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(raw))}
		}
		font, ok := raw.(string)
		if !ok || !dashboardFontChoices[font] {
			font = dashboardFontDefaultID
		}
		return pyjson.ObjectOf("font", font), nil
	})
}

// Discovered dashboard plugins of the profile without hidden or consent withheld ones, internal fields stripped
func listDashboardPlugins(ctx context.Context, profile string) (any, error) {
	visible, err := configScopedToThread(ctx, profile, func(sctx context.Context) ([]*pyjson.Object, error) {
		cfg, err := webLoadConfig(sctx)
		if err != nil {
			return nil, err
		}
		var hidden any = cfgGetDefault(cfg, []any{}, "dashboard", "hidden_plugins")
		if !pyrepr.Truthy(hidden) {
			hidden = []any{}
		}
		out := []*pyjson.Object{}
		for _, p := range getDashboardPlugins(sctx, false) {
			name, _ := p.GetOr("name", "").(string)
			isHidden, err := pyNameIn(name, hidden)
			if err != nil {
				return nil, err
			}
			if !isHidden && !pluginConsentWithheld(sctx, p) {
				out = append(out, p)
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	rows := make([]any, len(visible))
	for i, p := range visible {
		row := pyjson.NewObject()
		for _, k := range p.Keys {
			if !strings.HasPrefix(k, "_") {
				row.Set(k, p.Vals[k])
			}
		}
		rows[i] = row
	}
	return rows, nil
}

// Forces a rescan of the dashboard plugins of the profile
func rescanDashboardPlugins(ctx context.Context, profile string) (any, error) {
	found, err := configScopedToThread(ctx, profile, func(sctx context.Context) ([]*pyjson.Object, error) {
		return getDashboardPlugins(sctx, true), nil
	})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "count", int64(len(found))), nil
}

// Agent plugins merged with dashboard extension metadata for the profile, session protected
func getPluginsHub(ctx context.Context, req *Request, profile string) (any, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	var out any
	err := raised(func() error {
		hub, err := configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
			return mergedPluginsHub(sctx, false)
		})
		out = hub
		return err
	})
	if err == nil {
		return out, nil
	}
	if h, ok := asHTTPException(err); ok {
		return nil, h
	}
	routersLog().Warn(fmt.Sprintf("plugins/hub failed: %s", err))
	return nil, NewHTTPException(500, "Failed to build plugins hub.")
}

// Tail of an agent plugin mutation, 400 on a failed result, then caches dropped, a client decided answer returned unchanged
func pluginAction(ctx context.Context, result map[string]any, fallbackError string, rescan bool, clientDecides ...string) (map[string]any, error) {
	if len(clientDecides) == 0 {
		clientDecides = []string{"consent_required"}
	}
	for _, flag := range clientDecides {
		if pyrepr.Truthy(result[flag]) {
			return result, nil
		}
	}
	if !pyrepr.Truthy(result["ok"]) {
		var detail any = fallbackError
		if pyrepr.Truthy(result["error"]) {
			detail = configJSON(result["error"])
		}
		return nil, NewHTTPException(400, detail)
	}
	if rescan {
		getDashboardPlugins(ctx, true)
	}
	invalidatePluginsHubCache()
	return result, nil
}

// Curated plugin catalog merged with the installed state of this backend, session protected
func getPluginsCatalog(ctx context.Context, req *Request) (any, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	var out any
	err := raised(func() error {
		enabled, disabled := plugins.EnabledPluginSet(ctx), plugins.DisabledPluginSet(ctx)
		entries, err := plugins.DiscoverAllPlugins(home.Dir(ctx))
		if err != nil {
			return err
		}
		installed := map[string]map[string]any{}
		order := []string{}
		for _, e := range entries {
			aliases := map[string]bool{}
			for _, alias := range []string{e.Name, e.Key} {
				if alias != "" {
					aliases[alias] = true
				}
			}
			info := map[string]any{"dir": e.Dir, "runtime_status": pluginRuntimeStatus(aliases, enabled, disabled)}
			for _, alias := range []string{e.Name, e.Key} {
				if !aliases[alias] {
					continue
				}
				if _, held := installed[alias]; !held {
					order = append(order, alias)
				}
				installed[alias] = info
			}
		}
		state, err := plugins.InstalledCatalogState(ctx, installed, order)
		if err != nil {
			return err
		}
		out = configJSON(state)
		return nil
	})
	if err != nil {
		routersLog().Warn(fmt.Sprintf("plugins/catalog failed: %s", err))
		return nil, NewHTTPException(500, "Failed to build plugins catalog.")
	}
	return out, nil
}

// Result of a plugin mutation run inside the home and secret scope of the dashboard's own profile
func pluginMutation(ctx context.Context, fn func(ctx context.Context) (map[string]any, error)) (map[string]any, error) {
	sctx, _, err := configProfileScope(ctx, "")
	if err != nil {
		return nil, err
	}
	return fn(sctx)
}

// Installs an agent plugin from an identifier or a catalog name, session protected
func postAgentPluginInstall(ctx context.Context, req *Request, body *pyjson.Object) (any, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	catalogName := pystr.Strip(bodyStr(body, "catalog_name"))
	identifier := pystr.Strip(bodyStr(body, "identifier"))
	if identifier == "" && catalogName == "" {
		return nil, NewHTTPException(400, "Provide an identifier or a catalog_name.")
	}
	force, _ := body.GetOr("force", nil).(bool)
	enable, _ := body.GetOr("enable", nil).(bool)
	result, err := pluginMutation(ctx, func(sctx context.Context) (map[string]any, error) {
		installed, err := uiDashboardInstallPlugin(sctx, plugins.DashboardInstallOptions{Identifier: identifier, Force: force, Enable: enable,
			CatalogName: catalogName, Ref: bodyStr(body, "ref")})
		if err != nil {
			return nil, err
		}
		return pluginAction(sctx, installed, "Install failed.", true)
	})
	if err != nil {
		return nil, err
	}
	delete(result, "after_install_path")
	return configJSON(result), nil
}

// Plugin name of a URL parameter, a 400 for an empty name or a path traversal attempt
func validatePluginName(name string) (string, error) {
	name = strings.Trim(name, "/")
	if name == "" || strings.Contains(name, "..") || strings.Contains(name, "\\") {
		return "", NewHTTPException(400, "Invalid plugin name.")
	}
	return name, nil
}

// Runs a named plugin mutation behind the session token and the plugin name check
func namedPluginAction(ctx context.Context, req *Request, name string, action func(ctx context.Context, name string) (map[string]any, error),
	fallbackError string, rescan bool, clientDecides ...string) (any, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	result, err := pluginMutation(ctx, func(sctx context.Context) (map[string]any, error) {
		valid, err := validatePluginName(name)
		if err != nil {
			return nil, err
		}
		acted, err := action(sctx, valid)
		if err != nil {
			return nil, err
		}
		return pluginAction(sctx, acted, fallbackError, rescan, clientDecides...)
	})
	if err != nil {
		return nil, err
	}
	return configJSON(result), nil
}

// JSON body of a request as a mapping, empty for an unreadable or non mapping body
func optionalJSONBody(ctx context.Context, req *Request) *pyjson.Object {
	body, err := req.JSON(ctx)
	if err != nil {
		return pyjson.NewObject()
	}
	if obj, ok := body.(*pyjson.Object); ok && obj != nil {
		return obj
	}
	return pyjson.NewObject()
}

// Updates a user plugin, the optional body carrying the accepted capability delta or the forced scan
func postAgentPluginUpdate(ctx context.Context, req *Request, name string) (any, error) {
	body := optionalJSONBody(ctx, req)
	accept, force := body.GetOr("accept_capabilities", nil) == true, body.GetOr("force", nil) == true
	return namedPluginAction(ctx, req, name, func(sctx context.Context, n string) (map[string]any, error) {
		return uiDashboardUpdateUserPlugin(sctx, n, accept, force)
	}, "Update failed.", true, "consent_required", "scan_blocked")
}

// Loads a plugin for the profile home a sibling process names and hands it to that profile's open chats
func postAgentPluginActivate(ctx context.Context, req *Request) (any, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	var body any
	if decoded, err := req.JSON(ctx); err == nil {
		body = decoded
	} else {
		body = pyjson.NewObject()
	}
	fields := pyjson.NewObject()
	if pyrepr.Truthy(body) {
		obj, ok := body.(*pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(body))
		}
		fields = obj
	}
	rawName := ""
	if v := fields.GetOr("name", nil); pyrepr.Truthy(v) {
		rawName = pyrepr.Str(v)
	}
	name, err := validatePluginName(rawName)
	if err != nil {
		return nil, err
	}
	rawHome := ""
	if v := fields.GetOr("home", nil); pyrepr.Truthy(v) {
		rawHome = pyrepr.Str(v)
	}
	target, err := pypath.PathExpanduser(pypath.PathStr(rawHome))
	if err != nil {
		return nil, err
	}
	profile, known := "", false
	if target != "" && target != "." {
		profile, known = gateway.ProfileNameForHome(target)
	}
	if !known {
		return nil, NewHTTPException(400, "Not a Hermes profile home.")
	}
	scope := profile
	if profile == "default" {
		scope = ""
	}
	sctx, _, err := configProfileScope(ctx, scope)
	if err != nil {
		return nil, err
	}
	if home.Key(home.Dir(sctx)) != home.Key(target) {
		return nil, NewHTTPException(400, "Home is not served by this backend.")
	}
	activation, err := plugins.LoadAndGoLive(sctx, name)
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true, "activation", configJSON(activation)), nil
}

// Persists the memory provider and context engine selection of the profile
func putPluginProviders(ctx context.Context, req *Request, body *pyjson.Object, profile string) (any, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	err := func() error {
		sctx, _, err := configProfileScope(ctx, profile)
		if err != nil {
			return err
		}
		lctx, unlock := provider.LockConfigMutation(sctx)
		defer unlock()
		if raw := body.GetOr("memory_provider", nil); raw != nil {
			memoryProvider := normalizeMemoryProviderName(raw)
			if err := requireMemoryProviderReady(lctx, memoryProvider); err != nil {
				return err
			}
			if err := plugins.SaveMemoryProvider(lctx, memoryProvider); err != nil {
				return err
			}
		}
		if engine, ok := body.GetOr("context_engine", nil).(string); ok {
			return plugins.SaveContextEngine(lctx, engine)
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	invalidatePluginsHubCache()
	return pyjson.ObjectOf("ok", true), nil
}

// Shows or hides a plugin in the sidebar of the profile, kept in dashboard.hidden_plugins
func postPluginVisibility(ctx context.Context, req *Request, name string, body *pyjson.Object, profile string) (any, error) {
	if err := requireToken(req); err != nil {
		return nil, err
	}
	name, err := validatePluginName(name)
	if err != nil {
		return nil, err
	}
	hidden, _ := body.GetOr("hidden", nil).(bool)
	err = func() error {
		sctx, _, err := configProfileScope(ctx, profile)
		if err != nil {
			return err
		}
		lctx, unlock := provider.LockConfigMutation(sctx)
		defer unlock()
		cfg, err := webLoadConfig(lctx)
		if err != nil {
			return err
		}
		section, ok := cfg["dashboard"].(map[string]any)
		if !ok {
			section = map[string]any{}
			setConfigKey(cfg, "dashboard", section)
		}
		hiddenList, _ := section["hidden_plugins"].([]any)
		if hiddenList == nil {
			hiddenList = []any{}
		}
		at := -1
		for i, item := range hiddenList {
			if item == name {
				at = i
				break
			}
		}
		if hidden && at < 0 {
			hiddenList = append(hiddenList, name)
		} else if !hidden && at >= 0 {
			hiddenList = append(hiddenList[:at:at], hiddenList[at+1:]...)
		}
		setConfigKey(section, "hidden_plugins", hiddenList)
		return webSaveConfig(lctx, cfg, config.SaveOptions{})
	}()
	if err != nil {
		return nil, err
	}
	invalidatePluginsHubCache()
	return pyjson.ObjectOf("ok", true, "name", name, "hidden", hidden), nil
}

// Whether a resolved path is the base or lies below it
func pathWithin(target, base string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Static asset of a consented dashboard plugin of the profile, limited to the browser asset suffixes
func servePluginAsset(ctx context.Context, pluginName, filePath, profile string) (any, error) {
	plugin, err := configScopedToThread(ctx, profile, func(sctx context.Context) (*pyjson.Object, error) {
		for _, p := range getDashboardPlugins(sctx, false) {
			if p.GetOr("name", nil) == pluginName {
				if pluginConsentWithheld(sctx, p) {
					return nil, nil
				}
				return p, nil
			}
		}
		return nil, nil
	})
	if _, refused := asHTTPException(err); refused {
		plugin, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	if plugin == nil {
		return nil, NewHTTPException(404, "Plugin not found")
	}
	base, _ := plugin.GetOr("_dir", "").(string)
	noStore := []string{"Cache-Control", "no-store, no-cache, must-revalidate"}
	if compiled, ok := compiledDashboard(plugin); ok {
		root := filepath.Clean(base)
		target := filepath.Clean(pypath.PathJoin(root, filePath))
		if !pathWithin(target, root) {
			return nil, NewHTTPException(403, "Path traversal blocked")
		}
		rel, err := filepath.Rel(root, target)
		if err != nil {
			return nil, err
		}
		name := filepath.ToSlash(rel)
		if info, err := fs.Stat(compiled.Assets, name); err != nil || !info.Mode().IsRegular() {
			return nil, NewHTTPException(404, "File not found")
		}
		mediaType, known := pluginAssetContentTypes[pystr.Lower(pypath.PathSuffix(target))]
		if !known {
			return nil, NewHTTPException(404, "File not found")
		}
		return NewFSFileResponse(compiled.Assets, name, target, 200, mediaType, noStore...)
	}
	target, err := pypath.Resolve(pypath.PathJoin(base, filePath))
	if err != nil {
		return nil, err
	}
	resolvedBase, err := pypath.Resolve(base)
	if err != nil {
		return nil, err
	}
	if !pathWithin(target, resolvedBase) {
		return nil, NewHTTPException(403, "Path traversal blocked")
	}
	st, err := pypath.PathlibStat(target)
	if err != nil {
		return nil, err
	}
	if st == nil || !st.Mode().IsRegular() {
		return nil, NewHTTPException(404, "File not found")
	}
	mediaType, known := pluginAssetContentTypes[pystr.Lower(pypath.PathSuffix(target))]
	if !known {
		return nil, NewHTTPException(404, "File not found")
	}
	return NewFileResponse(target, 200, mediaType, nil, "", noStore...), nil
}

// Dashboard theme, font and plugin routes in their declaration order
func dashboardUiRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", Optional(Str), nil)
	pluginPath := PathParam("name", Str)
	r.Get("/api/dashboard/themes", &Endpoint{
		Name:   "get_dashboard_themes",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getDashboardThemes(ctx, profileQuery(c)) },
	})
	r.Put("/api/dashboard/theme", &Endpoint{
		Name:   "set_dashboard_theme",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", themeSetBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			name := bodyStr(c.Object("body"), "name")
			if err := setDashboardKey(ctx, "theme", name, profileQuery(c)); err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "theme", name), nil
		},
	})
	r.Get("/api/dashboard/font", &Endpoint{
		Name:   "get_dashboard_font",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getDashboardFont(ctx, profileQuery(c)) },
	})
	r.Put("/api/dashboard/font", &Endpoint{
		Name:   "set_dashboard_font",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", fontSetBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			font := bodyStr(c.Object("body"), "font")
			if !dashboardFontChoices[font] {
				font = dashboardFontDefaultID
			}
			if err := setDashboardKey(ctx, "font", font, profileQuery(c)); err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("ok", true, "font", font), nil
		},
	})
	r.Get("/api/dashboard/plugins", &Endpoint{
		Name:   "get_dashboard_plugins",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return listDashboardPlugins(ctx, profileQuery(c)) },
	})
	r.Get("/api/dashboard/plugins/rescan", &Endpoint{
		Name:   "rescan_dashboard_plugins",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return rescanDashboardPlugins(ctx, profileQuery(c)) },
	})
	r.Get("/api/dashboard/plugins/hub", &Endpoint{
		Name:   "get_plugins_hub",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getPluginsHub(ctx, c.Request, profileQuery(c)) },
	})
	r.Get("/api/dashboard/plugins/catalog", &Endpoint{
		Name:   "get_plugins_catalog",
		Handle: func(ctx context.Context, c *Call) (any, error) { return getPluginsCatalog(ctx, c.Request) },
	})
	r.Post("/api/dashboard/agent-plugins/install", &Endpoint{
		Name: "post_agent_plugin_install",
		Body: []BodyParam{Body("body", agentPluginInstallBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return postAgentPluginInstall(ctx, c.Request, c.Object("body"))
		},
	})
	for _, toggle := range []struct {
		suffix, name, fallback string
		enabled                bool
	}{
		{"enable", "post_agent_plugin_enable", "Enable failed.", true},
		{"disable", "post_agent_plugin_disable", "Disable failed.", false},
	} {
		r.Post("/api/dashboard/agent-plugins/{name:path}/"+toggle.suffix, &Endpoint{
			Name:   toggle.name,
			Params: []Param{pluginPath},
			Handle: func(ctx context.Context, c *Call) (any, error) {
				return namedPluginAction(ctx, c.Request, c.Str("name"), func(sctx context.Context, n string) (map[string]any, error) {
					return plugins.DashboardSetAgentPluginEnabled(sctx, n, toggle.enabled)
				}, toggle.fallback, false)
			},
		})
	}
	r.Post("/api/dashboard/agent-plugins/{name:path}/update", &Endpoint{
		Name:   "post_agent_plugin_update",
		Params: []Param{pluginPath},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return postAgentPluginUpdate(ctx, c.Request, c.Str("name"))
		},
	})
	r.Delete("/api/dashboard/agent-plugins/{name:path}", &Endpoint{
		Name:   "delete_agent_plugin",
		Params: []Param{pluginPath},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return namedPluginAction(ctx, c.Request, c.Str("name"), func(sctx context.Context, n string) (map[string]any, error) {
				return plugins.DashboardRemoveUserPlugin(sctx, n)
			}, "Remove failed.", true)
		},
	})
	r.Post("/api/dashboard/agent-plugins/activate", &Endpoint{
		Name:   "post_agent_plugin_activate",
		Handle: func(ctx context.Context, c *Call) (any, error) { return postAgentPluginActivate(ctx, c.Request) },
	})
	r.Put("/api/dashboard/plugin-providers", &Endpoint{
		Name:   "put_plugin_providers",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", pluginProvidersPutBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return putPluginProviders(ctx, c.Request, c.Object("body"), profileQuery(c))
		},
	})
	r.Post("/api/dashboard/plugins/{name:path}/visibility", &Endpoint{
		Name:   "post_plugin_visibility",
		Params: []Param{pluginPath, optProfile},
		Body:   []BodyParam{Body("body", pluginVisibilityBodyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return postPluginVisibility(ctx, c.Request, c.Str("name"), c.Object("body"), profileQuery(c))
		},
	})
	r.Get("/dashboard-plugins/@{profile}/{plugin_name}/{file_path:path}", &Endpoint{
		Name:   "serve_profile_plugin_asset",
		Params: []Param{PathParam("profile", Str), PathParam("plugin_name", Str), PathParam("file_path", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return servePluginAsset(ctx, c.Str("plugin_name"), c.Str("file_path"), c.Str("profile"))
		},
	})
	r.Get("/dashboard-plugins/{plugin_name}/{file_path:path}", &Endpoint{
		Name:   "serve_plugin_asset",
		Params: []Param{PathParam("plugin_name", Str), PathParam("file_path", Str), optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return servePluginAsset(ctx, c.Str("plugin_name"), c.Str("file_path"), profileQuery(c))
		},
	})
	return r
}
