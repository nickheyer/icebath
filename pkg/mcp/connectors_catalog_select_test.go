package mcp

import (
	"context"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestOnlyADesktopSessionCarriesTheCatalogTool(t *testing.T) {
	ctx := context.Background()
	env := &tools.Env{Home: t.TempDir()}
	if names, _ := tools.Default.SelectToolNames(ctx, env, []string{"catalog", "web"}, nil, tools.SelectionContext{}); !names["manage_catalog"] {
		t.Fatalf("catalog selection %v", names)
	}
	if names, _ := tools.Default.SelectToolNames(ctx, env, nil, nil, tools.SelectionContext{}); names["manage_catalog"] {
		t.Fatal("an unrestricted selection carries manage_catalog")
	}
	if slices.Contains(tools.Default.ResolveToolset(ctx, "", "all"), "manage_catalog") {
		t.Fatal("all resolves to manage_catalog")
	}
	for _, platform := range []string{"cli", "tui", "telegram", "cron", ""} {
		if !tools.SessionPlatformToolDrops(platform)["manage_catalog"] {
			t.Fatalf("%q keeps manage_catalog", platform)
		}
	}
	if tools.SessionPlatformToolDrops("desktop")["manage_catalog"] {
		t.Fatal("desktop drops manage_catalog")
	}
	deferred := map[string]bool{}
	for _, name := range config.Defaults().Sub("tools").Sub("tool_search")["defer"].([]any) {
		deferred[name.(string)] = true
	}
	if !tools.IsDeferrableToolName(ctx, env, "manage_catalog", deferred) {
		t.Fatal("manage_catalog is not behind tool_search by default")
	}
}
