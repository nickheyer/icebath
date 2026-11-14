package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Home holding one enabled portable Agent Plugin whose mcp.json declares a stdio worker, args added when given
func portablePluginHome(t *testing.T, args []any) (string, string) {
	t.Helper()
	h := isolateHome(t)
	t.Setenv("ICEBATH_BUNDLED_PLUGINS", t.TempDir())
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	plugin := filepath.Join(h, "plugins", "portable")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON := func(name string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plugin, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	worker := map[string]any{"type": "stdio", "command": "python"}
	if args != nil {
		worker["args"] = args
	}
	writeJSON("plugin.json", map[string]any{"$schema": plugins.PluginSchemaV1, "name": "portable.test"})
	writeJSON("mcp.json", map[string]any{"$schema": plugins.MCPSchemaV1, "mcpServers": map[string]any{"worker": worker}})
	return h, plugin
}

// Writes config.yaml enabling the portable plugin above the given mcp_servers block
func writePortableConfig(t *testing.T, h, servers string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("plugins:\n  enabled:\n    - portable.test\n"+servers), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

// The portable worker among the loaded servers, the one entry whose command is python
func portableWorker(t *testing.T, got map[string]*Rendered) *Rendered {
	t.Helper()
	var found *Rendered
	for _, r := range got {
		if r.Config["command"] == "python" {
			if found != nil {
				t.Fatalf("two portable workers in %v", got)
			}
			found = r
		}
	}
	if found == nil {
		t.Fatalf("portable worker missing from %v", got)
	}
	return found
}

// test_mcp_tool.py TestLoadMCPConfig test_portable_servers_merge_after_native_interpolation
func TestLoadMCPConfigPortableServersMergeAfterNativeInterpolation(t *testing.T) {
	h, _ := portablePluginHome(t, []any{"${UNKNOWN}"})
	t.Setenv("PORT", "3000")
	writePortableConfig(t, h, "mcp_servers:\n  native:\n    command: node\n    args: [\"${PORT}\"]\n")
	got := LoadServerConfigs(context.Background(), nil)
	if native := got["native"]; native == nil || !reflect.DeepEqual(native.Config["args"], []any{"3000"}) {
		t.Fatalf("native %v", got)
	}
	if worker := portableWorker(t, got); !reflect.DeepEqual(worker.Config["args"], []any{"${UNKNOWN}"}) {
		t.Fatalf("portable args %v", worker.Config["args"])
	}
}

// test_mcp_tool.py TestLoadMCPConfig test_portable_server_resolves_through_real_plugin_discovery
func TestLoadMCPConfigPortableServerResolvesThroughRealPluginDiscovery(t *testing.T) {
	h, plugin := portablePluginHome(t, nil)
	writePortableConfig(t, h, "")
	got := LoadServerConfigs(context.Background(), nil)
	if len(got) != 1 {
		t.Fatalf("servers %v", got)
	}
	server := portableWorker(t, got)
	raw, err := json.Marshal(server.Config["env"])
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	cwd := pyStr(server.Config["cwd"])
	if server.Config["command"] != "python" || realDir(t, cwd) != realDir(t, plugin) || env["PLUGIN_ROOT"] != cwd || !strings.HasPrefix(env["PLUGIN_DATA"], filepath.Join(h, "plugin-data")) {
		t.Fatalf("server %v", server.Config)
	}
	if _, ok := server.Config["agent_plugin"]; ok {
		t.Fatal("agent_plugin leaked into the server config")
	}
}
