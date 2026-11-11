package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Resets the launch plugin cache before and after the case
func resetDashboardPluginCache(t *testing.T) {
	t.Helper()
	dashboardPluginsMu.Lock()
	dashboardPluginsCache = nil
	dashboardPluginsMu.Unlock()
	t.Cleanup(func() {
		dashboardPluginsMu.Lock()
		dashboardPluginsCache = nil
		dashboardPluginsMu.Unlock()
	})
}

// Writes a manifest under root name dashboard and returns that dashboard dir
func writePluginManifest(t *testing.T, root, name string, manifest map[string]any) string {
	t.Helper()
	dir := filepath.Join(root, name, "dashboard")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Project plugin planted under the working dir of an evil repo with an empty home
func plantProjectPlugin(t *testing.T) string {
	t.Helper()
	resetDashboardPluginCache(t)
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	if err := os.MkdirAll(filepath.Join(tmp, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(tmp, "evil-repo")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	writePluginManifest(t, filepath.Join(cwd, ".icebath", "plugins"), "evil",
		map[string]any{"name": "evil", "label": "Evil", "entry": "dist/index.js"})
	return cwd
}

func namedEntry(list []*pyjson.Object, name string) *pyjson.Object { return namedPlugin(list, name) }

func TestProjectPluginsEnvGateFalsyValuesKeepProjectPluginsDisabled(t *testing.T) {
	for _, value := range []string{"", "0", "false", "FALSE", "no", "off", "False"} {
		t.Run(value, func(t *testing.T) {
			plantProjectPlugin(t)
			if value == "" {
				t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "")
				os.Unsetenv("ICEBATH_ENABLE_PROJECT_PLUGINS")
			} else {
				t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", value)
			}
			if namedEntry(getDashboardPlugins(context.Background(), true), "evil") != nil {
				t.Fatalf("ICEBATH_ENABLE_PROJECT_PLUGINS=%q must NOT enable the project source", value)
			}
		})
	}
}

func TestProjectPluginsEnvGateTruthyValuesEnableProjectPlugins(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", "on", "YES"} {
		t.Run(value, func(t *testing.T) {
			plantProjectPlugin(t)
			t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", value)
			evil := namedEntry(getDashboardPlugins(context.Background(), true), "evil")
			if evil == nil {
				t.Fatal("project plugin not discovered")
			}
			if evil.GetOr("source", nil) != "project" {
				t.Fatalf("source %v", evil.GetOr("source", nil))
			}
		})
	}
}

// Dashboard dir of a plugin under the case temp dir
func sanitizerDashboardDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "plug", "dashboard")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestApiPathSanitizerSimpleRelativePathAccepted(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	d := sanitizerDashboardDir(t)
	if err := os.WriteFile(filepath.Join(d, "api.py"), []byte("router = None\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := safePluginAPIRelpath("api.py", d); got != "api.py" {
		t.Fatalf("got %q", got)
	}
}

func TestApiPathSanitizerTraversalRejected(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, payload := range []string{"../../../etc/passwd", "../neighbour/api.py", "../../../../tmp/evil.py", "subdir/../../../../etc/passwd"} {
		t.Run(payload, func(t *testing.T) {
			if got := safePluginAPIRelpath(payload, sanitizerDashboardDir(t)); got != "" {
				t.Fatalf("payload %q accepted as %q", payload, got)
			}
		})
	}
}

// User plugin home with the project source off, returning the plugin maker
func userPluginFactory(t *testing.T) func(name string, manifest map[string]any) {
	t.Helper()
	resetDashboardPluginCache(t)
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "")
	os.Unsetenv("ICEBATH_ENABLE_PROJECT_PLUGINS")
	return func(name string, manifest map[string]any) {
		writePluginManifest(t, filepath.Join(tmp, "plugins"), name, manifest)
	}
}

// Asserts the discovered entry carries no api file
func assertAPIScrubbed(t *testing.T, name string) {
	t.Helper()
	entry := namedEntry(getDashboardPlugins(context.Background(), true), name)
	if entry == nil {
		t.Fatalf("%s not discovered", name)
	}
	if v := entry.GetOr("_api_file", "missing"); v != nil {
		t.Fatalf("_api_file %v", v)
	}
	if entry.GetOr("has_api", nil) != false {
		t.Fatalf("has_api %v", entry.GetOr("has_api", nil))
	}
}

func TestDiscoveryScrubsApiFieldAbsoluteApiPathInManifestIsScrubbed(t *testing.T) {
	make := userPluginFactory(t)
	make("evil", map[string]any{"name": "evil", "label": "Evil", "api": "/tmp/payload.py", "entry": "dist/index.js"})
	assertAPIScrubbed(t, "evil")
}

func TestDiscoveryScrubsApiFieldTraversalApiPathInManifestIsScrubbed(t *testing.T) {
	make := userPluginFactory(t)
	make("traverse", map[string]any{"name": "traverse", "label": "Traverse", "api": "../../../../tmp/evil.py", "entry": "dist/index.js"})
	assertAPIScrubbed(t, "traverse")
}

// Synthetic cache entry whose api file is a benign router under the case temp dir
func payloadPlugin(t *testing.T, source, apiFile string) *pyjson.Object {
	t.Helper()
	dash := filepath.Join(t.TempDir(), "plug", "dashboard")
	if err := os.MkdirAll(dash, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dash, "api.py"), []byte("from fastapi import APIRouter\nrouter = APIRouter()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return pyjson.ObjectOf("name", "synthetic", "label", "Synthetic",
		"tab", pyjson.ObjectOf("path", "/synthetic", "position", "end"), "slots", []any{}, "entry", "dist/index.js",
		"css", nil, "has_api", true, "source", source, "_dir", dash, "_api_file", apiFile)
}

// Plugin api routes the app carries, mounted at startup or hot mounted
func mountedPluginRoutes(app *FastAPI) []*pluginAPIRoute {
	out := []*pluginAPIRoute{}
	for _, r := range app.Router.Routes {
		if pr, ok := r.(*pluginAPIRoute); ok {
			out = append(out, pr)
		}
	}
	if hot := hotMountOf(app); hot != nil {
		out = append(out, hot.mounted()...)
	}
	return out
}

// Runs the startup mount over a cache holding only the plugin and returns the app
func mountOverCache(t *testing.T, plugin *pyjson.Object) *FastAPI {
	t.Helper()
	resetDashboardPluginCache(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dashboardPluginsMu.Lock()
	dashboardPluginsCache = []*pyjson.Object{plugin}
	dashboardPluginsMu.Unlock()
	app := NewFastAPI("t", "0", nil, nil)
	mountPluginAPIRoutes(app)
	return app
}

func TestMountApiRoutesRefusesUntrustedProjectSourceApiIsNotImported(t *testing.T) {
	app := mountOverCache(t, payloadPlugin(t, "project", "api.py"))
	if got := mountedPluginRoutes(app); len(got) != 0 {
		t.Fatalf("project-source plugin's api file was mounted: %v", got[0].Path)
	}
}

func TestMountApiRoutesRefusesUntrustedTraversalApiCaughtAtMountTime(t *testing.T) {
	app := mountOverCache(t, payloadPlugin(t, "user", "../../../tmp/evil.py"))
	if got := mountedPluginRoutes(app); len(got) != 0 {
		t.Fatalf("traversal api file was mounted: %v", got[0].Path)
	}
}

func TestEndToEndPocBlockedFullChainBlocked(t *testing.T) {
	resetDashboardPluginCache(t)
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	if err := os.MkdirAll(filepath.Join(tmp, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(tmp, "evil-repo")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "0")
	payload := filepath.Join(tmp, "payload.py")
	if err := os.WriteFile(payload, []byte("OWNED = True\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writePluginManifest(t, filepath.Join(cwd, ".icebath", "plugins"), "evil",
		map[string]any{"name": "evil", "label": "Evil", "api": payload, "entry": "dist/index.js"})
	list := getDashboardPlugins(context.Background(), true)
	app := NewFastAPI("t", "0", nil, nil)
	mountPluginAPIRoutes(app)
	if namedEntry(list, "evil") != nil {
		t.Fatal("evil discovered")
	}
	if pluginAPIMounted(app, "evil") {
		t.Fatal("evil api mounted")
	}
	for _, r := range mountedPluginRoutes(app) {
		if r.apiPath == payload || filepath.Base(filepath.Dir(r.apiPath)) == "evil-repo" {
			t.Fatalf("payload mounted at %s", r.Path)
		}
	}
}
