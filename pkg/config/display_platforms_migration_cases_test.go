package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationCreatesPlatformsEntries(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	path := filepath.Join(h, "config.yaml")
	text, err := SafeDump(map[string]any{
		"_config_version": 15,
		"display": map[string]any{"tool_progress_overrides": map[string]any{"signal": "off", "telegram": "all"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
	ResetCache()
	t.Cleanup(ResetCache)
	if _, err := MigrateConfig(context.Background(), h, false, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := SafeLoad(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	display, _ := parsed.(map[string]any)["display"].(map[string]any)
	platforms, _ := display["platforms"].(map[string]any)
	leaf := func(platform string) any {
		p, _ := platforms[platform].(map[string]any)
		return p["tool_progress"]
	}
	if leaf("signal") != "off" || leaf("telegram") != "all" {
		t.Fatalf("display.platforms = %#v", platforms)
	}
}
