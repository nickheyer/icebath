package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestToolsConfigPreselectedAndChecklist(t *testing.T) {
	names := []string{"create_issue", "team_member_add", "search"}
	if got := mcpPreselected(names, map[string]bool{"search": true}, nil); !reflect.DeepEqual(got, map[int]bool{2: true}) {
		t.Fatalf("include %v", got)
	}
	if got := mcpPreselected(names, nil, map[string]bool{"*team_member*": true}); !reflect.DeepEqual(got, map[int]bool{0: true, 2: true}) {
		t.Fatalf("glob exclude %v", got)
	}
	ui := &fakeUI{}
	tc := map[string]any{"include": []any{"x"}, "exclude": []any{"y"}}
	applyMCPChecklist(ui, "gh", tc, names, map[int]bool{0: true, 1: true, 2: true}, nil, nil)
	if len(tc) != 0 {
		t.Fatalf("all selected kept filters %v", tc)
	}
	tc = map[string]any{}
	applyMCPChecklist(ui, "gh", tc, names, map[int]bool{0: true}, nil, nil)
	if !reflect.DeepEqual(tc["include"], []any{"create_issue"}) {
		t.Fatalf("include mode %v", tc)
	}
	tc = map[string]any{"exclude": []any{"*team_member*", "search"}}
	applyMCPChecklist(ui, "gh", tc, names, map[int]bool{1: true, 2: true}, nil, map[string]bool{"*team_member*": true, "search": true})
	if !reflect.DeepEqual(tc["exclude"], []any{"*team_member*", "create_issue"}) || tc["include"] != nil {
		t.Fatalf("exclude mode %v", tc)
	}
	if !strings.Contains(ui.out.String(), "gh: 1 re-enabled tool(s) still match glob exclude pattern(s) ['*team_member*']") {
		t.Fatal(ui.out.String())
	}
}

func TestToolsConfigApplyMCPChange(t *testing.T) {
	cfg := config.Tree{"mcp_servers": map[string]any{"github": map[string]any{"command": "npx"}}}
	failed := applyMCPChange(cfg, []string{"github:create_issue", "github:create_issue", "ghost:x"}, "disable")
	tc := cfg["mcp_servers"].(map[string]any)["github"].(map[string]any)["tools"].(map[string]any)
	if !reflect.DeepEqual(tc["exclude"], []any{"create_issue"}) || !failed["ghost"] || len(failed) != 1 {
		t.Fatalf("disable %v %v", tc, failed)
	}
	applyMCPChange(cfg, []string{"github:create_issue"}, "enable")
	if !reflect.DeepEqual(tc["exclude"], []any{}) {
		t.Fatalf("enable %v", tc)
	}
}

func TestToolsConfigDisableEnableCommand(t *testing.T) {
	seedServers(t, map[string]any{"github": map[string]any{"command": "npx"}})
	ctx := context.Background()
	ui := &fakeUI{}
	if err := ToolsDisableEnableCommand(ctx, ui, "disable", "cli", []string{"github:create_issue", "ghost:x", "not_a_toolset"}); err != nil {
		t.Fatal(err)
	}
	out := ui.out.String()
	for _, want := range []string{"MCP server 'ghost' not found in config", "Unknown toolset 'not_a_toolset'", "Disabled: github:create_issue"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	tc := readServers(t)["github"].(map[string]any)["tools"].(map[string]any)
	if !reflect.DeepEqual(tc["exclude"], []any{"create_issue"}) {
		t.Fatalf("exclude %v", tc)
	}
	ui = &fakeUI{}
	ToolsDisableEnableCommand(ctx, ui, "enable", "cli", []string{"github:create_issue"})
	if !strings.Contains(ui.out.String(), "Enabled: github:create_issue") {
		t.Fatal(ui.out.String())
	}
	ui = &fakeUI{}
	ToolsDisableEnableCommand(ctx, ui, "list", "nowhere", nil)
	if !strings.Contains(ui.out.String(), "Unknown platform 'nowhere'. Valid: ") {
		t.Fatal(ui.out.String())
	}
	ui = &fakeUI{}
	ToolsDisableEnableCommand(ctx, ui, "disable", "cli", []string{"discord"})
	if !strings.Contains(ui.out.String(), "Toolset 'discord' is not available on platform 'cli' (only: discord)") {
		t.Fatal(ui.out.String())
	}
	ui = &fakeUI{}
	ToolsDisableEnableCommand(ctx, ui, "list", "cli", nil)
	if !strings.Contains(ui.out.String(), "Built-in toolsets (cli):") || !strings.Contains(ui.out.String(), "github  all tools enabled") {
		t.Fatal(ui.out.String())
	}
}

func TestToolsConfigIncludeModeConfigureWritesIncludeNotExclude(t *testing.T) {
	h := seedServers(t, map[string]any{"srv": map[string]any{"command": "npx"}, "off": map[string]any{"command": "npx", "enabled": false}})
	prev := toolsConfigProbe
	toolsConfigProbe = func(ctx context.Context, env *tools.Env) map[string][]ToolSummary {
		return map[string][]ToolSummary{"srv": {{"a", "first"}, {"b", ""}, {"c", strings.Repeat("d", 80)}}}
	}
	t.Cleanup(func() { toolsConfigProbe = prev })
	ui := &fakeUI{checklist: func(title string, items []string, selected map[int]bool) map[int]bool {
		if title != "MCP Server: srv  (3 tools)" || items[0] != "a  (first)" || items[1] != "b" || !strings.HasSuffix(items[2], "...)") {
			t.Fatalf("checklist %q %v", title, items)
		}
		return map[int]bool{0: true}
	}}
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	if err := configureMCPToolsInteractive(context.Background(), &tools.Env{}, ui, cfg); err != nil {
		t.Fatal(err)
	}
	tc := readServers(t)["srv"].(map[string]any)["tools"].(map[string]any)
	if !reflect.DeepEqual(tc["include"], []any{"a"}) || tc["exclude"] != nil {
		t.Fatalf("tools %v", tc)
	}
	if !strings.Contains(ui.out.String(), "srv: 1 enabled, 2 disabled") || !strings.Contains(ui.out.String(), "MCP tool configuration saved") {
		t.Fatal(ui.out.String())
	}
}

func setToolsConfigProbe(t *testing.T, found map[string][]ToolSummary) {
	t.Helper()
	prev := toolsConfigProbe
	toolsConfigProbe = func(context.Context, *tools.Env) map[string][]ToolSummary { return found }
	t.Cleanup(func() { toolsConfigProbe = prev })
}

func TestToolsConfigDisablingToolWritesIncludeList(t *testing.T) {
	h := isolateHome(t)
	setToolsConfigProbe(t, map[string][]ToolSummary{"github": {{"create_issue", "Create an issue"}, {"delete_repo", "Delete a repo"}, {"search_repos", "Search repos"}}})
	cfg := config.Tree{"mcp_servers": map[string]any{"github": map[string]any{"command": "npx"}}}
	ui := &fakeUI{checklist: func(string, []string, map[int]bool) map[int]bool { return map[int]bool{0: true, 2: true} }}
	if err := configureMCPToolsInteractive(context.Background(), &tools.Env{}, ui, cfg); err != nil {
		t.Fatal(err)
	}
	tc := cfg["mcp_servers"].(map[string]any)["github"].(map[string]any)["tools"].(map[string]any)
	if _, has := tc["exclude"]; has || !reflect.DeepEqual(tc["include"], []any{"create_issue", "search_repos"}) {
		t.Fatalf("tools %v", tc)
	}
	saved, err := config.ReadUser(h)
	if err != nil || !reflect.DeepEqual(saved["mcp_servers"].(map[string]any)["github"].(map[string]any)["tools"], map[string]any{"include": []any{"create_issue", "search_repos"}}) {
		t.Fatalf("saved %v %v", saved, err)
	}
}

func TestToolsConfigEmptyToolsServerSkipped(t *testing.T) {
	isolateHome(t)
	setToolsConfigProbe(t, map[string][]ToolSummary{"empty": {}})
	var titles []string
	ui := &fakeUI{checklist: func(title string, _ []string, pre map[int]bool) map[int]bool {
		titles = append(titles, title)
		return pre
	}}
	cfg := config.Tree{"mcp_servers": map[string]any{"empty": map[string]any{"command": "npx"}}}
	if err := configureMCPToolsInteractive(context.Background(), &tools.Env{}, ui, cfg); err != nil {
		t.Fatal(err)
	}
	if len(titles) != 0 || !strings.Contains(ui.out.String(), "no tools found") {
		t.Fatalf("checklists %v output %s", titles, ui.out.String())
	}
}

func TestToolsConfigEmptyIncludeReopensWithNothingPreselected(t *testing.T) {
	h := isolateHome(t)
	setToolsConfigProbe(t, map[string][]ToolSummary{"github": {{"create_issue", "Create an issue"}, {"search_repos", "Search repos"}}})
	cfg := config.Tree{"mcp_servers": map[string]any{"github": map[string]any{"command": "npx", "tools": map[string]any{"include": []any{}}}}}
	var preseen map[int]bool
	ui := &fakeUI{checklist: func(_ string, _ []string, pre map[int]bool) map[int]bool { preseen = pre; return pre }}
	if err := configureMCPToolsInteractive(context.Background(), &tools.Env{}, ui, cfg); err != nil {
		t.Fatal(err)
	}
	if preseen == nil || len(preseen) != 0 {
		t.Fatalf("preselected %v", preseen)
	}
	if _, err := os.Stat(filepath.Join(h, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config saved %v", err)
	}
	if tc := cfg["mcp_servers"].(map[string]any)["github"].(map[string]any)["tools"]; !reflect.DeepEqual(tc, map[string]any{"include": []any{}}) {
		t.Fatalf("tools %v", tc)
	}
}

func savedPlatformToolsets(t *testing.T, h, platform string) []any {
	t.Helper()
	saved, err := config.ReadUser(h)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := saved["platform_toolsets"].(map[string]any)[platform].([]any)
	return list
}

func TestToolsConfigDisableBuiltinAndMixedTargets(t *testing.T) {
	for _, names := range [][]string{{"web"}, {"web", "bad_toolset"}} {
		h := isolateHome(t)
		if err := config.SaveUser(h, map[string]any{"platform_toolsets": map[string]any{"cli": []any{"web", "memory", "terminal"}}}); err != nil {
			t.Fatal(err)
		}
		ui := &fakeUI{}
		if err := ToolsDisableEnableCommand(context.Background(), ui, "disable", "cli", names); err != nil {
			t.Fatal(err)
		}
		saved := savedPlatformToolsets(t, h, "cli")
		has := func(k string) bool {
			for _, v := range saved {
				if v == k {
					return true
				}
			}
			return false
		}
		if has("web") || !has("memory") {
			t.Fatalf("targets %v saved %v output %s", names, saved, ui.out.String())
		}
		if len(names) == 2 && !strings.Contains(ui.out.String(), "Unknown toolset 'bad_toolset'") {
			t.Fatal(ui.out.String())
		}
	}
}

func TestToolsConfigDisableUnknownMCPServerAndListExcluded(t *testing.T) {
	seedServers(t, map[string]any{})
	ui := &fakeUI{}
	if err := ToolsDisableEnableCommand(context.Background(), ui, "disable", "cli", []string{"unknown:tool"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ui.out.String(), "MCP server 'unknown' not found in config") {
		t.Fatal(ui.out.String())
	}
	seedServers(t, map[string]any{"github": map[string]any{"tools": map[string]any{"exclude": []any{"create_issue"}}}})
	ui = &fakeUI{}
	if err := ToolsDisableEnableCommand(context.Background(), ui, "list", "cli", nil); err != nil {
		t.Fatal(err)
	}
	if out := ui.out.String(); !strings.Contains(out, "github") || !strings.Contains(out, "create_issue") {
		t.Fatal(out)
	}
}
