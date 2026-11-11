package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const pluginsHubCacheTTL = 5 * time.Second

// Plugins hub payloads by home key with their expiry
var (
	pluginsHubCacheMu        sync.Mutex
	pluginsHubCache          = map[string]*pyjson.Object{}
	pluginsHubCacheExpiresAt = map[string]time.Time{}
)

// Check functions with a background probe in flight
var (
	pluginsHubProbeMu       sync.Mutex
	pluginsHubProbeInflight = map[uintptr]bool{}
)

// Drops every memoized plugins hub payload
func invalidatePluginsHubCache() {
	pluginsHubCacheMu.Lock()
	defer pluginsHubCacheMu.Unlock()
	clear(pluginsHubCache)
	clear(pluginsHubCacheExpiresAt)
}

// Warms a cold check verdict on its own goroutine, nil when a probe of the function is already running
func scheduleCheckFnProbe(fn func(*tools.Env) bool) <-chan struct{} {
	id := tools.CheckFnID(fn)
	pluginsHubProbeMu.Lock()
	if pluginsHubProbeInflight[id] {
		pluginsHubProbeMu.Unlock()
		return nil
	}
	pluginsHubProbeInflight[id] = true
	pluginsHubProbeMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			_ = recover()
			pluginsHubProbeMu.Lock()
			delete(pluginsHubProbeInflight, id)
			pluginsHubProbeMu.Unlock()
		}()
		tools.Default.CheckCached(context.Background(), &tools.Env{}, fn)
	}()
	return done
}

// Background probe scheduler of the hub auth hint, the module attribute hermes tests wrap
var hubScheduleCheckFnProbe = scheduleCheckFnProbe

// Login command of a plugin that left core with its own CLI, else the auth command of its name
func pluginLoginCommand(name string) string {
	for _, f := range provider.LeftCore() {
		if f.Plugin == name && f.CLI != "" {
			return fmt.Sprintf("icebath %s login", f.CLI)
		}
	}
	return "icebath auth " + name
}

// Names a for loop over a manifest provides_tools value yields, false for a value Python cannot iterate
func iterManifestNames(value any) ([]any, bool) {
	switch t := value.(type) {
	case []any:
		return t, true
	case string:
		out := make([]any, 0, len(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, true
	case map[string]any:
		out := make([]any, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		return out, true
	case *pyjson.Object:
		out := make([]any, 0, len(t.Keys))
		for _, k := range t.Keys {
			out = append(out, k)
		}
		return out, true
	}
	return nil, false
}

// Whether a provided tool last probed unavailable and the login command, cold verdicts probed in the background
func pluginAuthHint(ctx context.Context, name string, providesTools any) (authRequired bool, authCommand string) {
	defer func() {
		if recover() != nil {
			authRequired, authCommand = false, ""
		}
	}()
	names, ok := iterManifestNames(providesTools)
	if !ok {
		return false, ""
	}
	env := memoryToolsEnv(ctx)
	for _, raw := range names {
		tname, isStr := raw.(string)
		if !isStr {
			continue
		}
		entry := tools.Default.Lookup(ctx, env.Home, tname)
		if entry == nil || entry.Check == nil {
			continue
		}
		value, cached := tools.CachedCheckResult(&tools.Env{Home: home.Override(ctx)}, entry.Check)
		if !cached {
			hubScheduleCheckFnProbe(entry.Check)
		} else if !value {
			return true, pluginLoginCommand(name)
		}
	}
	return false, ""
}

// Runtime state of a plugin alias set, disabled winning over enabled
func pluginRuntimeStatus(aliases, enabledSet, disabledSet map[string]bool) string {
	for a := range aliases {
		if disabledSet[a] {
			return "disabled"
		}
	}
	for a := range aliases {
		if enabledSet[a] {
			return "enabled"
		}
	}
	return "inactive"
}

// Python membership test of a name in a config value, an error for a value that is no container
func pyNameIn(name string, container any) (bool, error) {
	switch t := container.(type) {
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s == name {
				return true, nil
			}
		}
		return false, nil
	case string:
		return strings.Contains(t, name), nil
	case map[string]any:
		_, ok := t[name]
		return ok, nil
	case *pyjson.Object:
		_, ok := t.Get(name)
		return ok, nil
	}
	return false, &pyjson.TypeError{Msg: fmt.Sprintf("argument of type '%s' is not iterable", pyrepr.TypeName(container))}
}

// Agent discovery, dashboard manifests and provider picker metadata of the home in ctx, memoized briefly
func mergedPluginsHub(ctx context.Context, forceRefresh bool) (*pyjson.Object, error) {
	h := home.Dir(ctx)
	cacheKey := home.Key(h)
	now := time.Now()
	if !forceRefresh {
		pluginsHubCacheMu.Lock()
		cached := pluginsHubCache[cacheKey]
		expires := pluginsHubCacheExpiresAt[cacheKey]
		pluginsHubCacheMu.Unlock()
		if cached != nil && now.Before(expires) {
			return cached, nil
		}
	}
	startedAt := time.Now()
	dashboardList := getDashboardPlugins(ctx, false)
	dashByName := map[string]*pyjson.Object{}
	for _, p := range dashboardList {
		dashByName[pyrepr.Str(p.GetOr("name", nil))] = p
	}
	disabledSet := plugins.DisabledPluginSet(ctx)
	enabledSet := plugins.EnabledPluginSet(ctx)
	cfg, err := webLoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	var hiddenPlugins any = []any{}
	if v, ok := config.CfgGet(cfg, "dashboard", "hidden_plugins"); ok && pyrepr.Truthy(v) {
		hiddenPlugins = v
	}
	pluginsRootResolved, err := pypath.Resolve(filepath.Join(h, "plugins"))
	if err != nil {
		return nil, err
	}
	rows := []any{}
	removedEntries := plugins.ResolvedRemovedEntries(ctx)
	active := plugins.CategoryActiveNames(ctx)
	entries, err := plugins.DiscoverAllPlugins(h)
	if err != nil {
		return nil, err
	}
	agentNames := map[string]bool{}
	for _, e := range entries {
		runtimeStatus, err := plugins.ListedPluginStatus(ctx, e, enabledSet, disabledSet, active)
		if err != nil {
			return nil, err
		}
		if runtimeStatus == "not enabled" {
			runtimeStatus = "inactive"
		}
		dm := dashByName[e.Name]
		underUserTree, isDir, hasManifestFile, hasGit := false, false, false, false
		if e.Dir != "" {
			resolved, err := pypath.Resolve(e.Dir)
			if err != nil {
				return nil, err
			}
			_, underUserTree = pypath.PathRelativeTo(resolved, pluginsRootResolved)
			if st, err := os.Stat(e.Dir); err == nil {
				isDir = st.IsDir()
			}
			_, err = os.Stat(filepath.Join(e.Dir, "dashboard", "manifest.json"))
			hasManifestFile = err == nil
			_, err = os.Stat(filepath.Join(e.Dir, ".git"))
			hasGit = err == nil
		}
		canRemoveUpdate := (e.Source == "user" || e.Source == "git") && underUserTree && isDir
		var providesTools any = []any{}
		if v := plugins.ListedPluginManifest(e)["provides_tools"]; pyrepr.Truthy(v) {
			providesTools = v
		}
		authRequired, authCommand := false, ""
		if pyrepr.Truthy(providesTools) {
			authRequired, authCommand = pluginAuthHint(ctx, e.Name, providesTools)
		}
		var manifest any
		if dm != nil && dm.Len() > 0 {
			manifest = stripDashboardManifest(dm)
		}
		userHidden, err := pyNameIn(e.Name, hiddenPlugins)
		if err != nil {
			return nil, err
		}
		var removedReason any
		if reason, removed := plugins.RemovedAnnotation(ctx, e.Name, e.Dir, removedEntries); removed {
			removedReason = reason
		}
		rows = append(rows, pyjson.ObjectOf(
			"name", e.Name,
			"version", e.Version,
			"description", e.Description,
			"source", e.Source,
			"runtime_status", runtimeStatus,
			"has_dashboard_manifest", dm != nil || hasManifestFile,
			"dashboard_manifest", manifest,
			"path", e.Dir,
			"can_remove", canRemoveUpdate,
			"can_update_git", canRemoveUpdate && hasGit,
			"auth_required", authRequired,
			"auth_command", authCommand,
			"user_hidden", userHidden,
			"removed_reason", removedReason,
		))
		agentNames[e.Name] = true
	}
	orphanDashboard := []any{}
	for _, p := range dashboardList {
		if !agentNames[pyrepr.Str(p.GetOr("name", nil))] {
			orphanDashboard = append(orphanDashboard, stripDashboardManifest(p))
		}
	}
	memoryProviders, err := discoverMemoryProviderStatuses(ctx)
	if err != nil {
		return nil, err
	}
	memoryOptions := make([]any, len(memoryProviders))
	for i, row := range memoryProviders {
		memoryOptions[i] = row
	}
	contextEngines := []any{}
	if engines, err := plugins.DiscoverContextEngines(ctx); err == nil {
		for _, engine := range engines {
			contextEngines = append(contextEngines, pyjson.ObjectOf("name", engine.Name, "description", engine.Description))
		}
	}
	payload := pyjson.ObjectOf(
		"plugins", rows,
		"orphan_dashboard_plugins", orphanDashboard,
		"providers", pyjson.ObjectOf(
			"memory_provider", normalizeMemoryProviderName(plugins.CurrentMemoryProvider(ctx)),
			"memory_options", memoryOptions,
			"context_engine", plugins.CurrentContextEngine(ctx),
			"context_options", contextEngines,
		),
	)
	if duration := time.Since(startedAt); duration >= 250*time.Millisecond {
		webServerLog().Info(fmt.Sprintf("plugins/hub rebuilt in %.3fs (plugins=%d memory_options=%d)", duration.Seconds(), len(rows), len(memoryProviders)))
	}
	pluginsHubCacheMu.Lock()
	pluginsHubCache[cacheKey] = payload
	pluginsHubCacheExpiresAt[cacheKey] = time.Now().Add(pluginsHubCacheTTL)
	pluginsHubCacheMu.Unlock()
	return payload, nil
}
