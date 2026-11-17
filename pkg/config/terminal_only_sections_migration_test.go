package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMigration54DropsTheRetiredInterfaceSections(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	path := filepath.Join(h, "config.yaml")
	text, err := SafeDump(map[string]any{
		"_config_version": 53,
		"dashboard":       map[string]any{"theme": "midnight"},
		"webapp":          map[string]any{"enabled": true},
		"desktop":         map[string]any{"font_family": "Mono"},
		"display":         map[string]any{"skin": "slate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
	ResetCache()
	t.Cleanup(ResetCache)
	res, err := MigrateConfig(context.Background(), h, false, true)
	if err != nil {
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
	doc, _ := parsed.(map[string]any)
	for _, section := range []string{"dashboard", "webapp", "desktop"} {
		if _, kept := doc[section]; kept {
			t.Fatalf("%s survived the migration: %v", section, doc)
		}
	}
	if display, _ := doc["display"].(map[string]any); display["skin"] != "slate" {
		t.Fatalf("display touched: %v", doc["display"])
	}
	found := false
	for _, added := range res.ConfigAdded {
		found = found || added == "removed dashboard, webapp, desktop"
	}
	if !found {
		t.Fatalf("config added %v", res.ConfigAdded)
	}
}
