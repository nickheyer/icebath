package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func TestV38MigrationRemovesOnlyLegacyRelayPluginKeys(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	path := filepath.Join(h, "config.yaml")
	opts := config.DefaultSafeDumpOptions()
	opts.SortKeys = false
	text, err := config.SafeDumpWith(map[string]any{
		"_config_version": 37,
		"plugins":         map[string]any{"enabled": []any{"keep-me", "observability/nemo_relay", "nemo_relay"}},
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	results, err := config.MigrateConfig(context.Background(), h, false, true)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.SafeLoad(string(after))
	if err != nil {
		t.Fatal(err)
	}
	raw := parsed.(map[string]any)
	if v, ok := raw["_config_version"].(int); !ok || v < 38 {
		t.Fatalf("_config_version = %#v", raw["_config_version"])
	}
	if got := raw["plugins"].(map[string]any)["enabled"]; !reflect.DeepEqual(got, []any{"keep-me"}) {
		t.Fatalf("plugins.enabled = %#v", got)
	}
	found := false
	for _, w := range results.Warnings {
		found = found || (strings.Contains(w, "observability/nemo_relay") && strings.Contains(w, RelayPluginsConfigEnv))
	}
	if !found {
		t.Fatalf("warnings = %v", results.Warnings)
	}
}
