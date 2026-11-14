package commands

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestDefaultAndUserListRoundTrip(t *testing.T) {
	if got := config.AsMap(config.Defaults()["terminal"])["client_env_passthrough"]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("default %#v", got)
	}
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	if known, suggestion := config.ValidateConfigKey("terminal.client_env_passthrough", configCommandSources(h)); !known || suggestion != "" {
		t.Fatalf("validate %v %q", known, suggestion)
	}
	load := func() config.Tree {
		t.Helper()
		cfg, err := config.Load(h, config.EnvRefLookup)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	if got := config.AsMap(load()["terminal"])["client_env_passthrough"]; !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("fresh home %#v", got)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("terminal:\n  client_env_passthrough: [GEM_HOME, POETRY_HOME]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	names := []any{"GEM_HOME", "POETRY_HOME"}
	cfg := load()
	if got := config.AsMap(cfg["terminal"])["client_env_passthrough"]; !reflect.DeepEqual(got, names) {
		t.Fatalf("user list %#v", got)
	}
	if got := state.ClientEnvConfiguredNames(map[string]any(cfg)); !reflect.DeepEqual(got, map[string]bool{"GEM_HOME": true, "POETRY_HOME": true}) {
		t.Fatalf("configured names %v", got)
	}
	if err := config.SaveConfig(context.Background(), h, map[string]any(cfg), config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := config.AsMap(load()["terminal"])["client_env_passthrough"]; !reflect.DeepEqual(got, names) {
		t.Fatalf("after save %#v", got)
	}
}
