package tools

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// A legacy subset entry is reported, cleared by enabling its toolset, and the tools come back
func TestLegacySearchEntryIsReportedThenClearedByEnablingWeb(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("EXA_API_KEY", "exa-test")
	InvalidateCheckCache()
	ClearToolDefinitionsCache()
	t.Cleanup(func() { InvalidateCheckCache(); ClearToolDefinitionsCache() })
	ctx := context.Background()
	cfg := config.Tree{"platform_toolsets": map[string]any{"cli": []any{"file", "terminal", "web"}},
		"agent": map[string]any{"disabled_toolsets": []any{"search", "terminal_tools", "browser", "memory"}}}
	enabled := platformTools(cfg, "cli", false)
	want := map[string]PartialStrip{
		"terminal": {Stripped: []string{"terminal"}, Entries: []string{"terminal_tools"}, Hint: "run `icebath tools enable terminal` to restore"},
		"web":      {Stripped: []string{"web_search"}, Entries: []string{"search"}, Hint: "run `icebath tools enable web` to restore"},
	}
	if got := Default.ToolsetsPartiallyStripped(ctx, "", cfg, enabled); !reflect.DeepEqual(got, want) {
		t.Fatalf("stripped %+v", got)
	}
	if err := Default.SavePlatformTools(ctx, "", cfg, "cli", enabled, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	disabled := ParseConfigStringList(cfg.Get("agent.disabled_toolsets"))
	if !reflect.DeepEqual(disabled, []string{"browser", "memory"}) {
		t.Fatalf("disabled %v", disabled)
	}
	enabled = platformTools(cfg, "cli", false)
	if got := Default.ToolsetsPartiallyStripped(ctx, "", cfg, enabled); len(got) != 0 {
		t.Fatalf("still stripped %+v", got)
	}
	names := map[string]bool{}
	for _, td := range Default.ToolDefinitions(ctx, &Env{Home: t.TempDir()}, DefinitionOptions{Enabled: sortedSet(enabled), Disabled: disabled, Quiet: true}) {
		names[toolDefName(td)] = true
	}
	for _, n := range []string{"web_search", "web_extract", "terminal"} {
		if !names[n] {
			t.Fatalf("%s missing from %v", n, names)
		}
	}
}
