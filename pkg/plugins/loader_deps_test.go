package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

const depDist = "icebath-depprobe-fixture"

var depReq = depDist + ">=1.0,<2"

// Writes a dist-info directory for the fixture distribution at a version into a site directory
func writeDistInfo(t *testing.T, site, version string) {
	t.Helper()
	info := filepath.Join(site, strings.ReplaceAll(depDist, "-", "_")+"-"+version+".dist-info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "Metadata-Version: 2.1\nName: " + depDist + "\nVersion: " + version + "\n"
	if err := os.WriteFile(filepath.Join(info, "METADATA"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Commits a PM selection for the running tree under a temp home the way a sync leaves it
func selectGeneration(t *testing.T, withDist bool, version string) string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	venv := filepath.Join(pm.InstallStateDir(ctx, pm.RepoRoot()), "environments", "gen1", "venv")
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte("home = /nonexistent\nversion = 3.12.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	site, err := pm.SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if withDist {
		writeDistInfo(t, site, version)
	}
	facts, _ := json.Marshal(map[string]any{"schema": 1, "packages": map[string]any{"venv": map[string]any{"environment": venv}}})
	path := pm.EnvironmentRuntimeFactsPath(ctx, pm.RepoRoot())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, facts, 0o644); err != nil {
		t.Fatal(err)
	}
	return site
}

func TestKeyboardInterruptFromCallerThreadHookPropagates(t *testing.T) {
	m := timeoutManager(t, "1.0")
	var mu sync.Mutex
	later := 0
	addHook(m, "subagent_stop", func(ctx context.Context, kw map[string]any) (any, error) { return nil, ErrUserInterrupt })
	addHook(m, "subagent_stop", func(ctx context.Context, kw map[string]any) (any, error) {
		mu.Lock()
		later++
		mu.Unlock()
		return nil, nil
	})
	buf := captureLogs(t)
	func() {
		defer func() {
			r := recover()
			u, ok := r.(*UserInterrupt)
			if !ok || !errors.Is(u, ErrUserInterrupt) {
				t.Fatalf("recovered %v, want a UserInterrupt", r)
			}
		}()
		m.InvokeHook(context.Background(), "subagent_stop", map[string]any{"parent_session_id": "p1"})
		t.Fatal("InvokeHook returned instead of raising the interrupt")
	}()
	mu.Lock()
	defer mu.Unlock()
	if later != 0 {
		t.Fatal("a callback after the interrupt ran")
	}
	if strings.Contains(buf.String(), "subagent_stop") {
		t.Fatalf("the interrupt was reported as a hook failure: %s", buf.String())
	}
}

func TestInterruptEscapesUnboundedHookMiddlewareAndSections(t *testing.T) {
	m := timeoutManager(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	addHook(m, "on_session_start", func(ctx context.Context, kw map[string]any) (any, error) { return nil, ctx.Err() })
	expectInterrupt(t, func() { m.InvokeHook(ctx, "on_session_start", nil) })
	expectInterrupt(t, func() { m.AInvokeHook(ctx, "on_session_start", nil) })
	m.mu.Lock()
	m.middleware["llm_request"] = []*middlewareEntry{{name: "mw", fn: func(map[string]any) (any, error) { panic(ErrUserInterrupt) }}}
	m.mu.Unlock()
	expectInterrupt(t, func() { m.InvokeMiddleware("llm_request", "request", map[string]any{"request": map[string]any{}}) })
	section := &SystemPromptSection{ID: "s", Plugin: "p", ContentFunc: func(map[string]any) (any, error) { return nil, ErrUserInterrupt }}
	expectInterrupt(t, func() { renderPromptSectionText(section, map[string]any{}) })
}

// Fails unless the function unwinds with a UserInterrupt panic
func expectInterrupt(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if _, ok := recover().(*UserInterrupt); !ok {
			t.Fatal("expected a UserInterrupt to escape")
		}
	}()
	fn()
}

func TestPluginDebugHandlerTeesToStderr(t *testing.T) {
	savedLogger, savedOut := Logger, debugStderr
	pluginsDebugState.Lock()
	savedState := pluginsDebugState.enabled
	savedInstalled := pluginsDebugState.installed
	pluginsDebugState.installed = false
	pluginsDebugState.Unlock()
	var out bytes.Buffer
	debugStderr = &syncWriter{w: &out}
	t.Cleanup(func() {
		Logger, debugStderr = savedLogger, savedOut
		pluginsDebugState.Lock()
		pluginsDebugState.enabled, pluginsDebugState.installed = savedState, savedInstalled
		pluginsDebugState.Unlock()
	})
	t.Setenv(EnvPluginsDebug, "")
	installPluginDebugHandler(true)
	if out.Len() != 0 || excInfo(errors.New("x")) != nil {
		t.Fatal("debug handler installed while ICEBATH_PLUGINS_DEBUG is off")
	}
	t.Setenv(EnvPluginsDebug, "1")
	installPluginDebugHandler(true)
	logWarn("Failed to load plugin 'x': boom")
	text := out.String()
	if !strings.Contains(text, "[plugins] DEBUG ICEBATH_PLUGINS_DEBUG=1") || !strings.Contains(text, "[plugins] WARNING Failed to load plugin 'x': boom") {
		t.Fatalf("stderr tee = %q", text)
	}
	if info := excInfo(&PluginPanicError{Value: "v", Stack: []byte("stack")}); len(info) != 2 || !strings.Contains(info[1].(string), "stack") {
		t.Fatalf("exc_info = %v", info)
	}
	installPluginDebugHandler(true)
	if strings.Count(out.String(), "verbose plugin discovery logging enabled") != 1 {
		t.Fatal("handler installed twice")
	}
}

func TestBundledPluginsDirHonoursOverride(t *testing.T) {
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", "/opt/bundled")
	if GetBundledPluginsDir() != "/opt/bundled" {
		t.Fatal("override ignored")
	}
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", "")
	if GetBundledPluginsDir() != filepath.Join(pm.RepoRoot(), "plugins") {
		t.Fatalf("default = %s", GetBundledPluginsDir())
	}
}
