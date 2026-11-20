package plugins

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, body)
	}
}

// test_context_engine_host_contract.py test_engine_collector_forwards_register_command_to_plugin_manager plus its refusals
func TestEngineCollectorForwardsRegisterCommandToPluginManager(t *testing.T) {
	m := NewManager(t.TempDir())
	handler := PluginCommandHandler(func(_ context.Context, raw string) (any, error) { return "echo: " + raw, nil })
	collector := &engineCollector{name: "my-lcm"}
	collector.registerCommand(m, "my-lcm-test-cmd", handler, "test command from a context engine", "<msg>")
	entry := m.PluginCommands()["my-lcm-test-cmd"]
	if entry == nil || entry.ArgsHint != "<msg>" || entry.Plugin != "context-engine:my-lcm" || entry.Description != "test command from a context engine" {
		t.Fatalf("entry %+v", entry)
	}
	if out, _ := entry.Handler(context.Background(), "hi"); out != "echo: hi" {
		t.Fatalf("handler %v", out)
	}
	previous := installedServices()
	t.Cleanup(func() { InstallServices(previous) })
	InstallServices(&Services{ResolveBuiltinCommand: func(name string) bool { return name == "help" }})
	collector.registerCommand(m, "  ", handler, "", "")
	collector.registerCommand(m, "/Help", handler, "", "")
	collector.registerCommand(m, "my-lcm-test-cmd", func(context.Context, string) (any, error) { return "second", nil }, "", "")
	commands := m.PluginCommands()
	if len(commands) != 1 {
		t.Fatalf("refused registrations landed %v", commands)
	}
	if out, _ := commands["my-lcm-test-cmd"].Handler(context.Background(), ""); out != "echo: " {
		t.Fatal("a plugin command was replaced")
	}
	if c := newEngineCollectorContext(m, ""); c.engineCollector.name != "context_engine" {
		t.Fatalf("default engine name %q", c.engineCollector.name)
	}
}

// LoadNamed warns and answers nil for a failing load or a missing instance
func TestLoadNamedWarnsOnFailureOrNoInstance(t *testing.T) {
	if LoadNamed("x", "/d", func(string) (any, error) { return nil, os.ErrNotExist }, "Context engine", "engine") != nil {
		t.Fatal("error load")
	}
	if LoadNamed("x", "/d", func(string) (any, error) { return nil, nil }, "Context engine", "engine") != nil {
		t.Fatal("empty load")
	}
	if LoadNamed("x", "/d", func(string) (any, error) { return "", nil }, "Context engine", "engine") != nil {
		t.Fatal("falsy load")
	}
	if got := LoadNamed("x", "/d", func(dir string) (any, error) { return dir, nil }, "Context engine", "engine"); got != "/d" {
		t.Fatalf("instance %v", got)
	}
}
