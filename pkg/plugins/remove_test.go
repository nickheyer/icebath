package plugins

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Temp home with a plugins dir and recording oauth grant services
func rmHome(t *testing.T) (string, *[]string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath-home")
	if err := os.MkdirAll(filepath.Join(h, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, h)
	config.ResetCache()
	forgotten := &[]string{}
	previous := installedServices()
	InstallServices(&Services{
		PluginOAuthKey:     func(dir string) string { return "grant:" + filepath.Base(dir) },
		ForgetPluginGrants: func(key string) error { *forgotten = append(*forgotten, key); return nil },
	})
	t.Cleanup(func() { InstallServices(previous) })
	return h, forgotten
}

// Plugin tree with a manifest line set and a register function
func rmPlugin(t *testing.T, root, rel, name, extra string) string {
	t.Helper()
	d := filepath.Join(root, rel)
	vtWrite(t, filepath.Join(d, "plugin.yaml"), "name: "+name+"\nversion: '1.0'\n"+extra)
	vtWrite(t, filepath.Join(d, "__init__.py"), "def register(ctx):\n    pass\n")
	return d
}

// Parsed config.yaml of a home
func rmConfig(t *testing.T, h string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestRemoveForgetsEveryConfigTraceAndResetsTheMemoryProvider(t *testing.T) {
	h, forgotten := rmHome(t)
	rmPlugin(t, filepath.Join(h, "plugins"), "fakemem", "fakemem", "kind: exclusive\n")
	vtWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [fakemem, keep-me]\n  disabled: [other]\n"+
		"  entries:\n    fakemem: {allow_tool_override: true}\n    keep-me: {}\nmemory:\n  provider: fakemem\n")
	result, err := DashboardRemoveUserPlugin(context.Background(), "fakemem")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"ok": true, "name": "fakemem", "cleared_memory_provider": true}; !reflect.DeepEqual(result, want) {
		t.Fatalf("result = %v", result)
	}
	if _, err := os.Stat(filepath.Join(h, "plugins", "fakemem")); !os.IsNotExist(err) {
		t.Fatalf("tree still present: %v", err)
	}
	cfg := rmConfig(t, h)
	plugins := cfg["plugins"].(map[string]any)
	if !reflect.DeepEqual(plugins["enabled"], []any{"keep-me"}) || !reflect.DeepEqual(plugins["disabled"], []any{"other"}) {
		t.Fatalf("lists = %v", plugins)
	}
	if !reflect.DeepEqual(plugins["entries"], map[string]any{"keep-me": map[string]any{}}) {
		t.Fatalf("entries = %v", plugins["entries"])
	}
	if provider := cfg["memory"].(map[string]any)["provider"]; provider != "" {
		t.Fatalf("memory provider = %v", provider)
	}
	if strings.Join(*forgotten, ",") != "grant:fakemem" {
		t.Fatalf("grants forgotten = %v", *forgotten)
	}
}

func TestRemoveOfASymlinkInsideThePluginsDirUnlinksOnlyTheLink(t *testing.T) {
	h, _ := rmHome(t)
	victim := rmPlugin(t, filepath.Join(h, "plugins"), "victim", "victim", "")
	if err := os.Symlink(victim, filepath.Join(h, "plugins", "alias")); err != nil {
		t.Fatal(err)
	}
	vtWrite(t, filepath.Join(h, "plugins", ".install-metadata.json"), `{"victim": {"pinned_sha": "`+strings.Repeat("a", 40)+`"}}`)
	vtWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [victim]\n")
	result, err := DashboardRemoveUserPlugin(context.Background(), "alias")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"ok": true, "name": "alias"}; !reflect.DeepEqual(result, want) {
		t.Fatalf("result = %v", result)
	}
	if _, err := os.Lstat(filepath.Join(h, "plugins", "alias")); !os.IsNotExist(err) {
		t.Fatal("alias link still present")
	}
	if _, err := os.Stat(filepath.Join(victim, "plugin.yaml")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(h, "plugins", ".install-metadata.json"))
	if !strings.Contains(string(raw), "victim") {
		t.Fatal("install metadata lost the victim")
	}
	if got := rmConfig(t, h)["plugins"].(map[string]any)["enabled"]; !reflect.DeepEqual(got, []any{"victim"}) {
		t.Fatalf("enabled = %v", got)
	}
}

func TestRemoveClearsTheToolsetOfAPluginWithoutProvidesTools(t *testing.T) {
	h, _ := rmHome(t)
	installHostEnvHooks()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	ctx := homeCtx(h)
	t.Cleanup(func() {
		if m := GetPluginManager(ctx); m.pluginHostInstance != nil {
			m.pluginHostInstance.Shutdown()
		}
	})
	d := rmPlugin(t, filepath.Join(h, "plugins"), "zznodecl", "zznodecl", "")
	vtWrite(t, filepath.Join(d, "__init__.py"), "def register(ctx):\n"+
		"    ctx.register_tool(name='zznodecl_tool', toolset='zznodecl_ts', handler=lambda args, **kw: '{}',\n"+
		"                      schema={'name': 'zznodecl_tool', 'description': 'probe',\n"+
		"                              'parameters': {'type': 'object', 'properties': {}}})\n")
	vtWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [zznodecl]\nplatform_toolsets:\n  cli: [icebath-cli, zznodecl_ts]\n  telegram: [icebath-telegram]\n")
	result, err := DashboardRemoveUserPlugin(ctx, "zznodecl")
	if err != nil || result["ok"] != true {
		t.Fatalf("%v %v", result, err)
	}
	want := map[string]any{"cli": []any{"icebath-cli"}, "telegram": []any{"icebath-telegram"}}
	if got := rmConfig(t, h)["platform_toolsets"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("platform_toolsets %v", got)
	}
}
