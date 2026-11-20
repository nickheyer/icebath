package tools

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func TestSeededConfigRowsNeedNoPlugin(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	ctx := context.Background()
	seeded, err := config.SeedConfigFile(ctx, h, home.ConfigPath(h), "")
	if err != nil || !seeded {
		t.Fatalf("seed_config_file = %v %v", seeded, err)
	}
	raw, err := config.ReadRawConfig(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := raw["platform_toolsets"].(map[string]any)
	unresolved := map[string]any{}
	for platform, names := range rows {
		list, _ := names.([]any)
		for _, n := range list {
			name, _ := n.(string)
			if !Default.ValidateToolset(ctx, "", name) {
				unresolved[platform] = names
			}
		}
	}
	if len(unresolved) > 0 {
		t.Fatalf("unresolved = %#v", unresolved)
	}
}
