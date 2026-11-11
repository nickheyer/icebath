package tuiserver

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Captures what a body writes to stderr
func captureStderr(t *testing.T, body func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	body()
	os.Stderr = saved
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// Fresh launch home holding the given config.yaml body
func toolsetHome(t *testing.T, body string) {
	t.Helper()
	h := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", h)
	setLaunchHome(t, h)
}

// Plugin discovery that does nothing, the hermes_cli.plugins stub the cases install
func noDiscovery(t *testing.T, fn func(ctx context.Context, force bool)) {
	t.Helper()
	saved := discoverPlugins
	discoverPlugins = fn
	t.Cleanup(func() { discoverPlugins = saved })
}

func TestLoadEnabledToolsetsPrefersTuiEnv(t *testing.T) {
	toolsetHome(t, "")
	t.Setenv("ICEBATH_TUI_TOOLSETS", "web, terminal, ,memory")
	if got := loadEnabledToolsets(context.Background(), ""); !reflect.DeepEqual(got, []string{"web", "terminal", "memory"}) {
		t.Fatalf("toolsets = %v", got)
	}
}

func TestLoadEnabledToolsetsFiltersInvalidTuiEnv(t *testing.T) {
	toolsetHome(t, "")
	t.Setenv("ICEBATH_TUI_TOOLSETS", "web, nope")
	noDiscovery(t, func(context.Context, bool) {})
	var got []string
	stderr := captureStderr(t, func() { got = loadEnabledToolsets(context.Background(), "") })
	if !reflect.DeepEqual(got, []string{"web"}) || !strings.Contains(stderr, "nope") {
		t.Fatalf("toolsets = %v stderr = %q", got, stderr)
	}
}

func TestLoadEnabledToolsetsAcceptsPluginEnvAfterDiscovery(t *testing.T) {
	toolsetHome(t, "")
	t.Setenv("ICEBATH_TUI_TOOLSETS", "plugin_demo")
	ready := false
	original := validateToolset
	validateToolset = func(ctx context.Context, scope, name string) bool {
		return (name == "plugin_demo" && ready) || original(ctx, scope, name)
	}
	t.Cleanup(func() { validateToolset = original })
	noDiscovery(t, func(context.Context, bool) { ready = true })
	if got := loadEnabledToolsets(context.Background(), ""); !reflect.DeepEqual(got, []string{"plugin_demo"}) {
		t.Fatalf("toolsets = %v", got)
	}
}

func TestLoadEnabledToolsetsAllEnvMeansAll(t *testing.T) {
	toolsetHome(t, "")
	t.Setenv("ICEBATH_TUI_TOOLSETS", "all")
	if got := loadEnabledToolsets(context.Background(), ""); got != nil {
		t.Fatalf("toolsets = %v", got)
	}
}

func TestLoadDisabledToolsetsReadsAgentConfig(t *testing.T) {
	toolsetHome(t, "agent:\n  disabled_toolsets: [browser]\n")
	if got := loadDisabledToolsets(context.Background()); !reflect.DeepEqual(got, []string{"browser"}) {
		t.Fatalf("disabled = %v", got)
	}
}

func TestLoadDisabledToolsetsNoneWhenUnsetOrConfigFails(t *testing.T) {
	toolsetHome(t, "agent:\n  disabled_toolsets: []\n")
	if got := loadDisabledToolsets(context.Background()); got != nil {
		t.Fatalf("empty list = %v", got)
	}
	toolsetHome(t, "{}\n")
	if got := loadDisabledToolsets(context.Background()); got != nil {
		t.Fatalf("unset = %v", got)
	}
	failConfigLoad(t)
	if got := loadDisabledToolsets(context.Background()); got != nil {
		t.Fatalf("failed load = %v", got)
	}
}

// Swaps the coding posture selection for a fixed answer
func fakeCodingSelection(t *testing.T, selection []string) {
	t.Helper()
	saved := codingSelection
	codingSelection = func(*tools.Env, string, string) []string { return selection }
	t.Cleanup(func() { codingSelection = saved })
}

// Swaps the configured disabled toolsets for a fixed answer
func fakeDisabled(t *testing.T, disabled []string) {
	t.Helper()
	saved := loadDisabledToolsets
	loadDisabledToolsets = func(context.Context) []string { return disabled }
	t.Cleanup(func() { loadDisabledToolsets = saved })
}

// Asserts the fallback result holds memory and project, never kanban, and only recently shipped extras
func assertConfiguredFallback(t *testing.T, result []string) {
	t.Helper()
	if result == nil {
		t.Fatal("result is None")
	}
	set := map[string]bool{}
	for _, name := range result {
		set[name] = true
	}
	if !set["memory"] || !set["project"] || set["kanban"] {
		t.Fatalf("result = %v", result)
	}
	for name := range set {
		if name != "memory" && name != "project" && !tools.RecentlyShippedToolsets[name] {
			t.Fatalf("unexpected toolset %s in %v", name, result)
		}
	}
}

func TestLoadEnabledToolsetsFoldsProjectIntoFocusPosture(t *testing.T) {
	toolsetHome(t, "")
	os.Unsetenv("ICEBATH_TUI_TOOLSETS")
	fakeCodingSelection(t, []string{"coding", "figma"})
	if got := loadEnabledToolsets(context.Background(), "tui"); !reflect.DeepEqual(got, []string{"coding", "figma", "project"}) {
		t.Fatalf("toolsets = %v", got)
	}
}

func TestLoadEnabledToolsetsHonorsDisabledProjectOnFocusPath(t *testing.T) {
	toolsetHome(t, "")
	os.Unsetenv("ICEBATH_TUI_TOOLSETS")
	fakeCodingSelection(t, []string{"coding", "figma"})
	fakeDisabled(t, []string{"project"})
	if got := loadEnabledToolsets(context.Background(), "tui"); !reflect.DeepEqual(got, []string{"coding", "figma"}) {
		t.Fatalf("toolsets = %v", got)
	}
}

func TestLoadEnabledToolsetsHonorsDisabledProjectOnConfiguredFallback(t *testing.T) {
	toolsetHome(t, "")
	os.Unsetenv("ICEBATH_TUI_TOOLSETS")
	fakeCodingSelection(t, nil)
	saved := platformTools
	platformTools = func(context.Context, string, config.Tree, string, tools.PlatformToolsOptions) map[string]bool {
		return map[string]bool{"memory": true, "web": true}
	}
	t.Cleanup(func() { platformTools = saved })
	fakeDisabled(t, []string{"project"})
	if got := loadEnabledToolsets(context.Background(), "tui"); !reflect.DeepEqual(got, []string{"memory", "web"}) {
		t.Fatalf("toolsets = %v", got)
	}
}

func TestWithSessionToolsetsKeepsDesktopUiWhenProjectDisabled(t *testing.T) {
	fakeDisabled(t, []string{"project"})
	if got := withSessionToolsets(context.Background(), []string{"memory"}, "desktop"); !reflect.DeepEqual(got, []string{"memory", "catalog", "desktop_ui"}) {
		t.Fatalf("disabled project = %v", got)
	}
	loadDisabledToolsets = func(context.Context) []string { return nil }
	if got := withSessionToolsets(context.Background(), []string{"memory"}, "desktop"); !reflect.DeepEqual(got, []string{"memory", "catalog", "desktop_ui", "project"}) {
		t.Fatalf("nothing disabled = %v", got)
	}
}

func TestLoadEnabledToolsetsRejectsDisabledMcpEnv(t *testing.T) {
	toolsetHome(t, "mcp_servers:\n  mcp-off:\n    enabled: false\nplatform_toolsets:\n  cli: [memory]\n")
	t.Setenv("ICEBATH_TUI_TOOLSETS", "mcp-off")
	noDiscovery(t, func(context.Context, bool) {})
	var result []string
	stderr := captureStderr(t, func() { result = loadEnabledToolsets(context.Background(), "") })
	assertConfiguredFallback(t, result)
	if !strings.Contains(stderr, "mcp-off") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestLoadEnabledToolsetsFallsBackWhenTuiEnvInvalid(t *testing.T) {
	toolsetHome(t, "platform_toolsets:\n  cli: [memory]\n")
	t.Setenv("ICEBATH_TUI_TOOLSETS", "nope")
	noDiscovery(t, func(context.Context, bool) {})
	var result []string
	stderr := captureStderr(t, func() { result = loadEnabledToolsets(context.Background(), "") })
	assertConfiguredFallback(t, result)
	if strings.TrimSpace(stderr) == "" {
		t.Fatal("no fallback warning")
	}
}

func TestLoadEnabledToolsetsWarnsWhenConfigFallbackFails(t *testing.T) {
	toolsetHome(t, "")
	failConfigLoad(t)
	t.Setenv("ICEBATH_TUI_TOOLSETS", "nope")
	noDiscovery(t, func(context.Context, bool) {})
	var result []string
	stderr := captureStderr(t, func() { result = loadEnabledToolsets(context.Background(), "") })
	if result != nil || strings.TrimSpace(stderr) == "" {
		t.Fatalf("result = %v stderr = %q", result, stderr)
	}
}

func TestLoadEnabledToolsetsHonorsBuiltinEnvIfConfigFails(t *testing.T) {
	toolsetHome(t, "")
	failConfigLoad(t)
	t.Setenv("ICEBATH_TUI_TOOLSETS", "web")
	if got := loadEnabledToolsets(context.Background(), ""); !reflect.DeepEqual(got, []string{"web"}) {
		t.Fatalf("toolsets = %v", got)
	}
}

// Makes the config load raise, the boom load_config of the cases
func failConfigLoad(t *testing.T) {
	t.Helper()
	saved := loadConfig
	loadConfig = func(context.Context) (config.Tree, error) { return nil, errors.New("boom") }
	t.Cleanup(func() { loadConfig = saved })
}

func TestLoadEnabledToolsetsReportsDisabledMcpSeparately(t *testing.T) {
	toolsetHome(t, "mcp_servers:\n  mcp-off:\n    enabled: false\n")
	t.Setenv("ICEBATH_TUI_TOOLSETS", "web,mcp-off,nope")
	noDiscovery(t, func(context.Context, bool) {})
	var result []string
	stderr := captureStderr(t, func() { result = loadEnabledToolsets(context.Background(), "") })
	if !reflect.DeepEqual(result, []string{"web"}) || !strings.Contains(stderr, "nope") || !strings.Contains(stderr, "mcp-off") {
		t.Fatalf("result = %v stderr = %q", result, stderr)
	}
}
