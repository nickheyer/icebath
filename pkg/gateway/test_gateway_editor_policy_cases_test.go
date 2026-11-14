package gateway

import (
	"context"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestAcpPolicyUsesItsOwnToolsAndRestoresFrozenCwd(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	cfg := map[string]any{"platform_toolsets": map[string]any{"cli": []any{"terminal"}, "acp": []any{"file"}}}
	params := map[string]any{"source": "acp", "cwd": t.TempDir(), "model": "fixture",
		"editor": map[string]any{"mcp_servers": []any{}, "edit_approval_policy": "ask"}}
	before := os.Environ()
	policy, err := buildPolicy(ctx, params, cfg, buildPolicyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Platform != "acp" {
		t.Fatalf("platform %q", policy.Platform)
	}
	want := tools.Default.PlatformTools(ctx, "", config.Tree(cfg), "acp", tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
	got := map[string]bool{}
	for _, name := range policy.Toolsets {
		got[name] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("toolsets %v, want %v", got, want)
	}
	if slices.Contains(policy.Toolsets, "terminal") || slices.Contains(policy.Toolsets, "project") {
		t.Fatalf("toolsets %v", policy.Toolsets)
	}
	restored, err := restorePolicy(policy.asDict())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, policy) {
		t.Fatalf("restored %+v, want %+v", restored, policy)
	}
	cfg["platform_toolsets"].(map[string]any)["acp"] = []any{"terminal"}
	if slices.Contains(restored.Toolsets, "terminal") {
		t.Fatalf("restored toolsets %v", restored.Toolsets)
	}
	if after := os.Environ(); !reflect.DeepEqual(after, before) {
		t.Fatalf("environment changed")
	}
}
