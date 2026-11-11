package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Manifest api field when it is a relative file staying inside the plugin dashboard dir, empty otherwise
func safePluginAPIRelpath(apiField any, dashboardDir string) string {
	s, ok := apiField.(string)
	if !ok || pystr.Strip(s) == "" {
		return ""
	}
	if filepath.IsAbs(s) || (len(s) > 0 && (s[0] == '/' || s[0] == '\\')) {
		return ""
	}
	resolved, err := pypath.Realpath(filepath.Join(dashboardDir, s), pypath.Lax)
	if err != nil {
		return ""
	}
	root, err := pypath.Realpath(dashboardDir, pypath.Lax)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return s
}

// Root and source pairs scanned for dashboard plugins in priority order, the first name winning
type pluginSearchDir struct {
	root     string
	source   string
	compiled bool
}

// User, bundled and opt in project plugin roots, the default root added when the launch home is a profile
func dashboardPluginSearchDirs() []pluginSearchDir {
	bundledRoot := plugins.GetBundledPluginsDir()
	userRoots := []string{filepath.Join(home.Process(), "plugins")}
	rootPlugins := filepath.Join(home.Root(), "plugins")
	a, errA := pypath.Realpath(rootPlugins, pypath.Lax)
	b, errB := pypath.Realpath(userRoots[0], pypath.Lax)
	if errA != nil || errB != nil || a != b {
		userRoots = append(userRoots, rootPlugins)
	}
	dirs := []pluginSearchDir{}
	for _, d := range userRoots {
		dirs = append(dirs, pluginSearchDir{d, "user", false})
	}
	dirs = append(dirs, pluginSearchDir{filepath.Join(bundledRoot, "memory"), "bundled", false}, pluginSearchDir{bundledRoot, "bundled", true})
	if config.EnvVarEnabled("ICEBATH_ENABLE_PROJECT_PLUGINS") {
		if cwd, err := os.Getwd(); err == nil {
			dirs = append(dirs, pluginSearchDir{filepath.Join(cwd, ".icebath", "plugins"), "project", false})
		}
	}
	return dirs
}

// Discovery entry of one manifest with its tab, slots and validated api file
func dashboardPluginEntry(data *pyjson.Object, name any, dashboardDir, source string) *pyjson.Object {
	rawTab, ok := data.GetOr("tab", nil).(*pyjson.Object)
	if !ok {
		rawTab = pyjson.NewObject()
	}
	tabInfo := pyjson.ObjectOf("path", rawTab.GetOr("path", "/"+pyrepr.Str(name)), "position", rawTab.GetOr("position", "end"))
	if override, ok := rawTab.GetOr("override", nil).(string); ok && strings.HasPrefix(override, "/") {
		tabInfo.Set("override", override)
	}
	if pyTruthy(rawTab.GetOr("hidden", nil)) {
		tabInfo.Set("hidden", true)
	}
	slots := []any{}
	if src, ok := data.GetOr("slots", nil).([]any); ok {
		for _, s := range src {
			if str, ok := s.(string); ok && str != "" {
				slots = append(slots, str)
			}
		}
	}
	rawAPI := data.GetOr("api", nil)
	safeAPI := safePluginAPIRelpath(rawAPI, dashboardDir)
	if pyTruthy(rawAPI) && safeAPI == "" {
		webServerLog().Warn(fmt.Sprintf("Plugin %s: refusing unsafe api path %s (must be a "+
			"relative file inside the plugin's dashboard/ "+
			"directory); backend routes from this plugin will "+
			"not be mounted", pyrepr.Str(name), pyrepr.Repr(rawAPI)))
	}
	var apiFile any
	if safeAPI != "" {
		apiFile = safeAPI
	}
	return pyjson.ObjectOf(
		"name", name,
		"label", data.GetOr("label", name),
		"description", data.GetOr("description", ""),
		"icon", data.GetOr("icon", "Puzzle"),
		"version", data.GetOr("version", "0.0.0"),
		"tab", tabInfo,
		"slots", slots,
		"entry", data.GetOr("entry", "dist/index.js"),
		"css", data.GetOr("css", nil),
		"has_api", safeAPI != "",
		"source", source,
		"_dir", dashboardDir,
		"_api_file", apiFile,
	)
}

// Plugins of every plugins root dashboard manifest, the default search dirs when none are given
func discoverDashboardPlugins(searchDirs []pluginSearchDir) []*pyjson.Object {
	if searchDirs == nil {
		searchDirs = dashboardPluginSearchDirs()
	}
	out := []*pyjson.Object{}
	seen := map[string]bool{}
	for _, sd := range searchDirs {
		names, compiled, ok := pluginRootChildren(sd)
		if !ok {
			continue
		}
		for _, childName := range names {
			child := filepath.Join(sd.root, childName)
			manifestFile := filepath.Join(child, "dashboard", "manifest.json")
			var entry *pyjson.Object
			var err error
			if d, isCompiled := compiled[childName]; isCompiled {
				entry, err = compiledDashboardEntry(d, child, sd.source, seen)
			} else {
				entry, err = readDashboardPluginManifest(child, manifestFile, sd.source, seen)
			}
			if err != nil {
				if _, isOS := err.(*os.PathError); isOS {
					webServerLog().Warn(fmt.Sprintf("Skipping unreadable dashboard plugin %s: %s", manifestFile, err))
				} else {
					webServerLog().Warn(fmt.Sprintf("Bad dashboard plugin manifest %s: %s", manifestFile, err))
				}
				continue
			}
			if entry != nil {
				out = append(out, entry)
			}
		}
	}
	return out
}

// Sorted child names of a plugins root with the compiled dashboards a bundled root lists, false when skipped
func pluginRootChildren(sd pluginSearchDir) ([]string, map[string]plugins.BundledDashboard, bool) {
	compiled := map[string]plugins.BundledDashboard{}
	if sd.compiled {
		for _, d := range plugins.BundledDashboards() {
			compiled[d.Name] = d
		}
	}
	names := []string{}
	if st, err := os.Stat(sd.root); err != nil || !st.IsDir() {
		if len(compiled) == 0 {
			return nil, nil, false
		}
	} else {
		entries, err := os.ReadDir(sd.root)
		if err != nil {
			webServerLog().Warn(fmt.Sprintf("Skipping unreadable dashboard plugin root %s: %s", sd.root, err))
			return nil, nil, false
		}
		for _, e := range entries {
			if _, dup := compiled[e.Name()]; !dup {
				names = append(names, e.Name())
			}
		}
	}
	for name := range compiled {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, compiled, true
}

// Entry of a dashboard compiled into the binary, listed under its directory name in the bundled root
func compiledDashboardEntry(d plugins.BundledDashboard, child, source string, seen map[string]bool) (*pyjson.Object, error) {
	entry, err := dashboardManifestEntry(d.Manifest, child, source, seen)
	if entry != nil {
		entry.Set("_bundled_dashboard", d.Name)
	}
	return entry, err
}

// Compiled in dashboard behind a discovery entry, false for a plugin read from disk
func compiledDashboard(plugin *pyjson.Object) (plugins.BundledDashboard, bool) {
	if plugin == nil {
		return plugins.BundledDashboard{}, false
	}
	name, ok := plugin.GetOr("_bundled_dashboard", nil).(string)
	if !ok {
		return plugins.BundledDashboard{}, false
	}
	for _, d := range plugins.BundledDashboards() {
		if d.Name == name {
			return d, true
		}
	}
	return plugins.BundledDashboard{}, false
}

// Entry of one plugin dir, nil when it has no manifest or its name was already seen
func readDashboardPluginManifest(child, manifestFile, source string, seen map[string]bool) (*pyjson.Object, error) {
	if st, err := os.Stat(child); err != nil || !st.IsDir() {
		return nil, nil
	}
	if _, err := os.Stat(manifestFile); err != nil {
		return nil, nil
	}
	raw, err := os.ReadFile(manifestFile)
	if err != nil {
		return nil, err
	}
	return dashboardManifestEntry(raw, child, source, seen)
}

// Entry of manifest bytes of the plugin dir child, nil when its name was already seen
func dashboardManifestEntry(raw []byte, child, source string, seen map[string]bool) (*pyjson.Object, error) {
	text, err := pycodecs.Decode(raw, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	data, ok := decoded.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(decoded))
	}
	name := data.GetOr("name", filepath.Base(child))
	switch name.(type) {
	case []any, *pyjson.Object:
		return nil, fmt.Errorf("unhashable type: '%s'", pyTypeName(name))
	}
	key := fmt.Sprintf("%T:%s", name, pyrepr.Repr(name))
	if seen[key] {
		return nil, nil
	}
	seen[key] = true
	return dashboardPluginEntry(data, name, filepath.Join(child, "dashboard"), source), nil
}

// Python type name of a decoded JSON value
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case []any:
		return "list"
	case *pyjson.Object:
		return "dict"
	case float64:
		return "float"
	}
	return "int"
}

// Plugin entry without its private underscore keys
func stripDashboardManifest(p *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range p.Keys {
		if !strings.HasPrefix(k, "_") {
			out.Set(k, p.Vals[k])
		}
	}
	return out
}
