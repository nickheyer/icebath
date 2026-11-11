package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

func pluginsCasesWrite(t *testing.T, path string, v any) {
	t.Helper()
	body, isText := v.(string)
	if !isText {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		body = string(raw)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPluginDiscoveryEnabledPortablePluginRegistersComponents(t *testing.T) {
	root := t.TempDir()
	h := filepath.Join(root, "home")
	plugin := filepath.Join(h, "plugins", "portable")
	app := filepath.Join(root, "example-app")
	pluginsCasesWrite(t, app, "")
	pluginsCasesWrite(t, filepath.Join(plugin, "plugin.json"), map[string]any{
		"$schema": plugins.PluginSchemaV1, "name": "portable.test",
		"extensions": map[string]any{"com.nousresearch.hermes": map[string]any{"servers": map[string]any{"worker": map[string]any{
			"app":      map[string]any{"darwin": map[string]any{"presence": "executable", "location": app}},
			"requires": map[string]any{"app": true}, "liveness": map[string]any{"kind": "static"}}}}}})
	pluginsCasesWrite(t, filepath.Join(plugin, "skills", "summarize", "SKILL.md"), "---\nname: summarize\ndescription: Summarize reports.\n---\nBody.\n")
	pluginsCasesWrite(t, filepath.Join(plugin, "mcp.json"), map[string]any{"$schema": plugins.MCPSchemaV1,
		"mcpServers": map[string]any{"worker": map[string]any{"type": "stdio", "command": "python"}}})
	pluginsCasesWrite(t, filepath.Join(h, "plugins", "native", "plugin.yaml"), "name: native\nversion: 1.0.0\n")
	pluginsCasesWrite(t, filepath.Join(h, "plugins", "native", "__init__.py"), "def register(ctx):\n    pass\n")
	pluginsCasesWrite(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [portable.test, native]\n")
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", pluginsCasesDir(t, filepath.Join(root, "bundled")))
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	restoreInterpreter := plugins.SwapHostInterpreter(func(context.Context) (string, error) { return python, nil })
	previous := plugins.InstalledServices()
	plugins.InstallServices(&plugins.Services{PlatformDeclarations: PlatformDeclarationsService()})
	plugins.ResetPluginManagersForTests()
	tools.ClearPlatformDeclarations()
	t.Cleanup(func() {
		plugins.ResetPluginManagersForTests()
		tools.ClearPlatformDeclarations()
		plugins.InstallServices(previous)
		restoreInterpreter()
	})
	ctx := home.WithOverride(context.Background(), h)
	manager := plugins.GetPluginManager(ctx)
	manager.DiscoverAndLoad(ctx, false)

	skills := manager.ListPluginSkillMetadata()
	if len(skills) != 1 || !strings.HasSuffix(skills[0]["name"].(string), ":summarize") {
		t.Fatalf("skills %v", skills)
	}
	servers := manager.GetPortableMCPServers()
	if len(servers) != 1 || servers["worker"] == nil {
		t.Fatalf("portable servers %v", servers)
	}
	wire := mcp.PrefixedToolName("worker", "nvapp_client_get_driver_status")
	if !strings.HasSuffix(wire, "__nvapp_client_get_driver_status") || len(wire) > 64 {
		t.Fatalf("wire name %q", wire)
	}
	rows := map[string]map[string]any{}
	for _, row := range manager.ListPlugins() {
		rows[row["key"].(string)] = row
	}
	if rows["portable.test"]["enabled"] != true || rows["native"]["enabled"] != true {
		t.Fatalf("plugins %v", rows)
	}
	host, err := manager.PluginHost()
	if err != nil || !host.Alive() {
		t.Fatalf("the native plugin module was never imported: host %v err %v", host, err)
	}
	if tools.LookupPlatformDeclaration("worker") == nil {
		t.Fatal("no platform declaration for worker")
	}
	if got := plugins.PortableLiveness("worker"); !reflect.DeepEqual(got, map[string]any{"kind": "static"}) {
		t.Fatalf("liveness %v", got)
	}
	manager.Unload("portable.test")
	if tools.LookupPlatformDeclaration("worker") != nil {
		t.Fatal("unload left the platform declaration")
	}
	if got := plugins.PortableLiveness("worker"); got != nil {
		t.Fatalf("unload left liveness %v", got)
	}
}

func pluginsCasesDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestForceReloadSymmetryPreToolCallTimeoutDoesNotReachToolHandler(t *testing.T) {
	c := bridgePluginContext(t)
	pluginsCasesWrite(t, filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), "plugins:\n  hook_callback_timeout: 0.1\n")
	hold := make(chan struct{})
	defer close(hold)
	c.RegisterHook("pre_tool_call", func(context.Context, map[string]any) (any, error) {
		<-hold
		return nil, nil
	}, false)
	var dispatchCalls [][]any
	d, env := bridgedDispatcher(t, func(_ context.Context, call *tools.Call) string {
		dispatchCalls = append(dispatchCalls, []any{call.Name, call.Args})
		return `{"ok": true}`
	})
	result := d.Dispatch(context.Background(), &tools.Call{Name: "web_search", Args: map[string]any{"query": "test"}, TaskID: "t1", SessionID: "s1", Env: env})
	if len(dispatchCalls) != 0 {
		t.Fatalf("the tool handler ran: %v", dispatchCalls)
	}
	if !strings.Contains(result, "pre_tool_call plugin callback timed out or is still running") {
		t.Fatalf("result %q", result)
	}
}
