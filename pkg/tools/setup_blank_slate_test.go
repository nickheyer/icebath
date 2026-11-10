package tools

import (
	"context"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestBlankSlateListExcludesSearchButKeepsKanban(t *testing.T) {
	cfg := config.Tree{}
	if err := BlankSlateMinimalToolsets(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var disabled []string
	for _, v := range cfg["agent"].(map[string]any)["disabled_toolsets"].([]any) {
		disabled = append(disabled, v.(string))
	}
	if slices.Contains(disabled, "search") {
		t.Fatalf("search disabled %v", disabled)
	}
	if !slices.Contains(disabled, "kanban") || !slices.Contains(disabled, "web") {
		t.Fatalf("kanban or web kept %v", disabled)
	}
	for _, kept := range []string{"file", "terminal", "vision", "skills"} {
		if slices.Contains(disabled, kept) {
			t.Fatalf("%s disabled %v", kept, disabled)
		}
	}
	if !slices.IsSorted(disabled) {
		t.Fatalf("unsorted %v", disabled)
	}
	cli := cfg["platform_toolsets"].(map[string]any)["cli"]
	if !slices.Equal(config.AsStrings(cli), []string{"file", "skills", "terminal", "vision"}) {
		t.Fatalf("cli %v", cli)
	}
}

func TestBlankSlateMinimalToolsetsOddSections(t *testing.T) {
	if err := BlankSlateMinimalToolsets(context.Background(), config.Tree{"platform_toolsets": []any{}}); err == nil || err.Error() != "TypeError: 'list' object does not support item assignment" {
		t.Fatalf("%v", err)
	}
	cfg := config.Tree{"agent": nil}
	if err := BlankSlateMinimalToolsets(context.Background(), cfg); err != nil || cfg["agent"] != nil {
		t.Fatalf("agent %v %v", cfg["agent"], err)
	}
	cfg = config.Tree{"agent": map[string]any{"max_turns": 9}, "platform_toolsets": map[string]any{"telegram": []any{"web"}}}
	if err := BlankSlateMinimalToolsets(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["agent"].(map[string]any)["max_turns"] != 9 || cfg["platform_toolsets"].(map[string]any)["telegram"] == nil {
		t.Fatalf("existing sections dropped %v", cfg)
	}
}
