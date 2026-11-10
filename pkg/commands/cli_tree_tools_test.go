package commands

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runs one icebath tools command line over a temp home holding cfg, returning the exit code and output
func runToolsMain(t *testing.T, dir string, cfg map[string]any, argv ...string) (int, string) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", dir)
	if cfg != nil {
		if err := config.SaveUser(context.Background(), dir, cfg); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	c := NewCLI()
	c.Root.SetArgs(argv)
	c.Root.SetOut(&out)
	c.Root.SetErr(&out)
	err := c.Root.ExecuteContext(home.WithOverride(context.Background(), dir))
	var exit *ExitError
	switch {
	case err == nil:
		return 0, out.String()
	case errors.As(err, &exit):
		return exit.Code, out.String()
	}
	t.Fatalf("%v: %v\n%s", argv, err, out.String())
	return 0, ""
}

func savedToolsets(t *testing.T, dir, platform string) []string {
	t.Helper()
	saved, err := config.ReadUser(dir)
	if err != nil {
		t.Fatal(err)
	}
	pt, _ := saved["platform_toolsets"].(map[string]any)
	return config.AsStrings(pt[platform])
}

func TestToolsDisableBuiltinDisableRemovesToolsetFromPlatform(t *testing.T) {
	dir := t.TempDir()
	runToolsMain(t, dir, map[string]any{"platform_toolsets": map[string]any{"cli": []any{"web", "memory", "terminal"}}}, "tools", "disable", "web", "--platform", "cli")
	if saved := savedToolsets(t, dir, "cli"); slices.Contains(saved, "web") || !slices.Contains(saved, "memory") {
		t.Fatalf("saved %v", saved)
	}
}

func TestToolsDisableMcpDisableUnknownServerPrintsError(t *testing.T) {
	_, out := runToolsMain(t, t.TempDir(), map[string]any{"mcp_servers": map[string]any{}}, "tools", "disable", "unknown:tool", "--platform", "cli")
	if !strings.Contains(out, "MCP server 'unknown' not found in config") {
		t.Fatalf("output %q", out)
	}
}

func TestToolsListListShowsMcpExcludedTools(t *testing.T) {
	cfg := map[string]any{"mcp_servers": map[string]any{"github": map[string]any{"tools": map[string]any{"exclude": []any{"create_issue"}}}}}
	_, out := runToolsMain(t, t.TempDir(), cfg, "tools", "list", "--platform", "cli")
	if !strings.Contains(out, "github") || !strings.Contains(out, "create_issue") {
		t.Fatalf("output %q", out)
	}
}

func TestToolsValidationMixedValidAndInvalidAppliesValidOnly(t *testing.T) {
	dir := t.TempDir()
	runToolsMain(t, dir, map[string]any{"platform_toolsets": map[string]any{"cli": []any{"web", "memory"}}}, "tools", "disable", "web", "bad_toolset", "--platform", "cli")
	if saved := savedToolsets(t, dir, "cli"); slices.Contains(saved, "web") || !slices.Contains(saved, "memory") {
		t.Fatalf("saved %v", saved)
	}
}

func TestToolsActionAcceptsDeferredPluginWithoutMaterializing(t *testing.T) {
	const platform = "deferred-tools-test"
	for _, action := range []string{"list", "enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			configured := []any{"memory"}
			if action == "disable" {
				configured = []any{"memory", "web"}
			}
			loads, discovers := 0, 0
			loader := &plugins.DeferredPlatformLoader{Load: func(context.Context) { loads++ }}
			prev := tools.PluginToolsetCatalog.Discover
			tools.PluginToolsetCatalog.Discover = func(context.Context) {
				discovers++
				gateway.Registry.RegisterDeferred(platform, loader, "")
			}
			t.Cleanup(func() {
				tools.PluginToolsetCatalog.Discover = prev
				gateway.Registry.Unregister(context.Background(), platform, "")
			})
			dir := t.TempDir()
			argv := []string{"tools", action, "--platform", platform}
			if action != "list" {
				argv = []string{"tools", action, "web", "--platform", platform}
			}
			if err := config.SaveUser(context.Background(), dir, map[string]any{"platform_toolsets": map[string]any{platform: configured}}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			_, out := runToolsMain(t, dir, nil, argv...)
			if strings.Contains(out, "Unknown platform") || discovers == 0 || loads != 0 {
				t.Fatalf("output %q discovers %d loads %d", out, discovers, loads)
			}
			after, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if action == "list" {
				if !strings.Contains(out, "Built-in toolsets ("+platform+"):") || !bytes.Equal(before, after) {
					t.Fatalf("output %q config rewritten %v", out, !bytes.Equal(before, after))
				}
				return
			}
			if bytes.Equal(before, after) {
				t.Fatal("config not saved")
			}
			if saved := savedToolsets(t, dir, platform); slices.Contains(saved, "web") != (action == "enable") {
				t.Fatalf("%s saved %v", action, saved)
			}
		})
	}
}

func TestEnableOnStringPlatformToolsetsKeepsListedEntries(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cfg := config.Tree{"platform_toolsets": map[string]any{"telegram": `["browser", "terminal", "video", "video_gen"]`}}
	if err := mcp.ApplyToolsetChange(context.Background(), cfg, "telegram", []string{"computer_use"}, "enable"); err != nil {
		t.Fatal(err)
	}
	saved, ok := cfg["platform_toolsets"].(map[string]any)["telegram"].([]any)
	if !ok {
		t.Fatalf("saved %T", cfg["platform_toolsets"].(map[string]any)["telegram"])
	}
	for _, want := range []string{"browser", "terminal", "video", "video_gen", "computer_use"} {
		if !slices.Contains(config.AsStrings(saved), want) {
			t.Fatalf("%s missing from %v", want, saved)
		}
	}
}

// Runs the tools menu MCP configurator over a temp home with the probe and checklist swapped
func runMCPConfigurator(t *testing.T, cfg config.Tree, found map[string][]mcp.ToolSummary, checklist func(title string, items []string, pre map[int]bool) map[int]bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	prevProbe := mcp.SwapToolsConfigProbe(func(context.Context, *tools.Env) map[string][]mcp.ToolSummary { return found })
	t.Cleanup(func() { mcp.SwapToolsConfigProbe(prevProbe) })
	prevChecklist := terminalChecklist
	terminalChecklist = func(_ *terminalUI, title string, items []string, selected, _ map[int]bool) (map[int]bool, error) {
		return checklist(title, items, selected), nil
	}
	t.Cleanup(func() { terminalChecklist = prevChecklist })
	var out bytes.Buffer
	con := &cliui.Console{In: strings.NewReader(""), Out: &out}
	if err := tools.ToolsCommandHooks.ConfigureMCPTools(home.WithOverride(context.Background(), dir), con, cfg); err != nil {
		t.Fatal(err)
	}
	return out.String(), dir
}

func TestDisablingToolWritesIncludeList(t *testing.T) {
	cfg := config.Tree{"mcp_servers": map[string]any{"github": map[string]any{"command": "npx"}}}
	found := map[string][]mcp.ToolSummary{"github": {{Name: "create_issue", Description: "Create an issue"}, {Name: "delete_repo", Description: "Delete a repo"}, {Name: "search_repos", Description: "Search repos"}}}
	_, dir := runMCPConfigurator(t, cfg, found, func(string, []string, map[int]bool) map[int]bool { return map[int]bool{0: true, 2: true} })
	toolsCfg := cfg["mcp_servers"].(map[string]any)["github"].(map[string]any)["tools"].(map[string]any)
	if !reflect.DeepEqual(toolsCfg["include"], []any{"create_issue", "search_repos"}) {
		t.Fatalf("include %v", toolsCfg)
	}
	if _, has := toolsCfg["exclude"]; has {
		t.Fatalf("exclude written %v", toolsCfg)
	}
	saved, err := config.ReadUser(dir)
	if err != nil || !reflect.DeepEqual(saved["mcp_servers"].(map[string]any)["github"].(map[string]any)["tools"], map[string]any{"include": []any{"create_issue", "search_repos"}}) {
		t.Fatalf("saved %v %v", saved, err)
	}
}

func TestEmptyToolsServerSkipped(t *testing.T) {
	var titles []string
	cfg := config.Tree{"mcp_servers": map[string]any{"empty": map[string]any{"command": "npx"}}}
	out, _ := runMCPConfigurator(t, cfg, map[string][]mcp.ToolSummary{"empty": {}}, func(title string, _ []string, pre map[int]bool) map[int]bool {
		titles = append(titles, title)
		return pre
	})
	if len(titles) != 0 || !strings.Contains(out, "no tools found") {
		t.Fatalf("checklists %v output %q", titles, out)
	}
}

func TestEmptyIncludeReopensWithNothingPreselected(t *testing.T) {
	cfg := config.Tree{"mcp_servers": map[string]any{"github": map[string]any{"command": "npx", "tools": map[string]any{"include": []any{}}}}}
	found := map[string][]mcp.ToolSummary{"github": {{Name: "create_issue", Description: "Create an issue"}, {Name: "search_repos", Description: "Search repos"}}}
	var preseen map[int]bool
	calls := 0
	_, dir := runMCPConfigurator(t, cfg, found, func(_ string, _ []string, pre map[int]bool) map[int]bool {
		calls++
		preseen = pre
		return pre
	})
	if calls != 1 || preseen == nil || len(preseen) != 0 {
		t.Fatalf("calls %d preselected %v", calls, preseen)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config saved %v", err)
	}
	if got := cfg["mcp_servers"].(map[string]any)["github"].(map[string]any)["tools"]; !reflect.DeepEqual(got, map[string]any{"include": []any{}}) {
		t.Fatalf("tools %v", got)
	}
}

func TestToolsTreeMatchesTheArgparseParser(t *testing.T) {
	c := NewCLI()
	for _, tc := range []struct {
		argv []string
		key  string
		want any
	}{
		{[]string{"tools", "post-setup", "chromium"}, "post_setup_key", "chromium"},
		{[]string{"tools", "disable", "web", "github:create_issue"}, "names", []string{"web", "github:create_issue"}},
		{[]string{"tools", "enable", "memory"}, "names", []string{"memory"}},
		{[]string{"tools", "list"}, "platform", "cli"},
		{[]string{"tools", "--summary"}, "summary", true},
	} {
		cmd, rest, err := c.Root.Find(tc.argv)
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.ParseFlags(rest); err != nil {
			t.Fatal(err)
		}
		if got := c.Namespace(cmd, cmd.Flags().Args())[tc.key]; !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%v: %s = %#v", tc.argv, tc.key, got)
		}
	}
	for _, tc := range []struct {
		argv []string
		want []string
	}{
		{[]string{"tools", "post-setup", "--help"}, []string{"KEY", "Post-setup hook key (e.g. chromium, cua_driver, kittentts)"}},
		{[]string{"tools", "disable", "--help"}, []string{"NAME", "Toolset name (e.g. web) or MCP tool in server:tool form"}},
		{[]string{"tools", "enable", "--help"}, []string{"NAME", "Toolset name or MCP tool in server:tool form"}},
	} {
		var out bytes.Buffer
		h := NewCLI()
		h.Root.SetArgs(tc.argv)
		h.Root.SetOut(&out)
		h.Root.SetErr(&out)
		_ = h.Root.Execute()
		for _, w := range tc.want {
			if !strings.Contains(out.String(), w) {
				t.Fatalf("%v help lacks %q:\n%s", tc.argv, w, out.String())
			}
		}
	}
}
