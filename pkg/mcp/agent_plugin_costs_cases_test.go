package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Installs the cost parser the top level wiring hands the plugin package
func installPortableCostParser(t *testing.T) {
	t.Helper()
	previous := plugins.InstalledServices()
	next := &plugins.Services{}
	if previous != nil {
		*next = *previous
	}
	next.ParseCostPolicy = func(costs, spendLimit, costUnit any, where string) error {
		_, err := ParseCostPolicy(costs, spendLimit, costUnit, where)
		return err
	}
	plugins.InstallServices(next)
	t.Cleanup(func() { plugins.InstallServices(previous) })
}

func writePortableCostPackage(t *testing.T, root string, decl map[string]any) {
	t.Helper()
	write := func(name string, doc any) {
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	write("plugin.json", map[string]any{"$schema": plugins.PluginSchemaV1, "name": "pkg",
		"extensions": map[string]any{"com.nousresearch.hermes": map[string]any{"servers": map[string]any{"srv": decl}}}})
	write("mcp.json", map[string]any{"$schema": plugins.MCPSchemaV1,
		"mcpServers": map[string]any{"srv": map[string]any{"type": "stdio", "command": "python3", "args": []any{"s.py"}}}})
}

func TestDeclarationsValidateTranslateAndSurviveANativeTrustOverride(t *testing.T) {
	h := isolateHome(t)
	logs := captureLogs(t)
	installPortableCostParser(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "pkg")
	writePortableCostPackage(t, root, map[string]any{"trust": "untrusted", "costs": map[string]any{"paid_render": []any{5, 20}},
		"spend_limit": 50, "future": 1})
	pkg, err := plugins.LoadAgentPlugin(root, filepath.Join(tmp, "data"))
	if err != nil {
		t.Fatal(err)
	}
	srv := pkg.MCPServers["srv"]
	if !reflect.DeepEqual(srv["tool_costs"], map[string]any{"paid_render": []any{int64(5), int64(20)}}) || srv["spend_limit"] != int64(50) {
		t.Fatalf("translated %v", srv)
	}
	warned := false
	for _, d := range pkg.Diagnostics {
		warned = warned || strings.Contains(d.Message, "future")
	}
	if !warned {
		t.Fatalf("unknown key not reported %v", pkg.Diagnostics)
	}
	writePortableCostPackage(t, root, map[string]any{"costs": map[string]any{"paid_render": -1}})
	_, err = plugins.LoadAgentPlugin(root, filepath.Join(tmp, "data"))
	var agentErr *plugins.AgentPluginError
	if !errors.As(err, &agentErr) || !strings.Contains(err.Error(), "cost of 'paid_render' must be a non-negative number") {
		t.Fatalf("negative cost %v", err)
	}
	kept := filterSuspiciousServers(map[string]any{"bad": map[string]any{"command": "x", "spend_limit": "lots"}, "ok": map[string]any{"command": "x"}})
	if len(kept) != 1 || kept["ok"] == nil || !strings.Contains(logs.text(), "spend_limit must be a non-negative number") {
		t.Fatalf("kept %v logs %q", kept, logs.text())
	}
	native := map[string]*Rendered{"srv": {Config: map[string]any{"command": "python3", "trust": "full", "spend_limit": 0}}}
	mergePortableServers(map[string]map[string]any{"srv": srv}, native, map[string]bool{})
	got := native["srv"].Config
	if got["trust"] != "full" || got["spend_limit"] != 0 || !reflect.DeepEqual(got["tool_costs"], srv["tool_costs"]) {
		t.Fatalf("native override %v", got)
	}
	installed := filepath.Join(h, "plugins", "pkg")
	writePortableCostPackage(t, installed, map[string]any{"trust": "untrusted", "costs": map[string]any{"paid_render": []any{5, 20}}, "spend_limit": 50})
	config.ResetCache()
	doc := "plugins:\n  enabled: [pkg]\nmcp_servers:\n  srv:\n    command: x\n    spend_limit: -5\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	portable, err := pluginPortableServers(h)
	if err != nil || portable["srv"] == nil {
		t.Fatalf("enabled package declares %v %v", portable, err)
	}
	if loaded := LoadServerConfigs(context.Background(), nil); loaded["srv"] != nil {
		t.Fatalf("the package revived a rejected native entry %v", loaded["srv"].Config)
	}
}
