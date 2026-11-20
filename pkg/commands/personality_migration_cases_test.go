package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Writes cfg as the home config, runs the quiet migration and returns the raw config and its results
func runPersonalityMigration(t *testing.T, cfg map[string]any) (map[string]any, *config.MigrationResults) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := config.DefaultSafeDumpOptions()
	opts.AllowUnicode = true
	text, err := config.SafeDumpWith(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), text, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	ctx := context.Background()
	results, err := config.MigrateConfig(ctx, h, false, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := config.ReadRawConfig(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	return raw, results
}

// Reports whether any migration note mentions the personality
func mentionsPersonality(results *config.MigrationResults) bool {
	for _, item := range results.ConfigAdded {
		if strings.Contains(item, "personality") {
			return true
		}
	}
	return false
}

// Leaf of a two level section of the raw config
func rawLeaf(raw map[string]any, section, key string) any {
	m, _ := raw[section].(map[string]any)
	return m[key]
}

func TestMigrationResetsStalePersonalityName(t *testing.T) {
	raw, results := runPersonalityMigration(t, map[string]any{"_config_version": 33, "display": map[string]any{"personality": "kawaii"}})
	if got := rawLeaf(raw, "display", "personality"); got != "" {
		t.Fatalf("display.personality = %#v", got)
	}
	if got := ResolveEphemeralSystemPrompt(raw); got != "" {
		t.Fatalf("ephemeral prompt = %q", got)
	}
	if !mentionsPersonality(results) {
		t.Fatalf("config_added = %v", results.ConfigAdded)
	}
}

func TestMigrationScrubsPersonalityTextFromSystemPrompt(t *testing.T) {
	raw, _ := runPersonalityMigration(t, map[string]any{"_config_version": 33, "agent": map[string]any{"system_prompt": BuiltinPersonalities["kawaii"]}})
	if got := rawLeaf(raw, "agent", "system_prompt"); got != "" {
		t.Fatalf("agent.system_prompt = %#v", got)
	}
	if got := ResolveEphemeralSystemPrompt(raw); got != "" {
		t.Fatalf("ephemeral prompt = %q", got)
	}
}

func TestMigrationPreservesManualSystemPrompt(t *testing.T) {
	raw, _ := runPersonalityMigration(t, map[string]any{
		"_config_version": 33,
		"display":         map[string]any{"personality": "pirate"},
		"agent":           map[string]any{"system_prompt": "my manual prompt"},
	})
	if got := rawLeaf(raw, "display", "personality"); got != "" {
		t.Fatalf("display.personality = %#v", got)
	}
	if got := rawLeaf(raw, "agent", "system_prompt"); got != "my manual prompt" {
		t.Fatalf("agent.system_prompt = %#v", got)
	}
	if got := ResolveEphemeralSystemPrompt(raw); got != "my manual prompt" {
		t.Fatalf("ephemeral prompt = %q", got)
	}
}

func TestMigrationNoopWhenNothingStale(t *testing.T) {
	_, results := runPersonalityMigration(t, map[string]any{"_config_version": 33})
	if mentionsPersonality(results) {
		t.Fatalf("config_added = %v", results.ConfigAdded)
	}
}

func TestPostV34ChoiceIsNeverReset(t *testing.T) {
	raw, _ := runPersonalityMigration(t, map[string]any{"_config_version": 34, "display": map[string]any{"personality": "kawaii"}})
	if got := rawLeaf(raw, "display", "personality"); got != "kawaii" {
		t.Fatalf("display.personality = %#v", got)
	}
	if got := ResolveEphemeralSystemPrompt(raw); got != BuiltinPersonalities["kawaii"] {
		t.Fatalf("ephemeral prompt = %q", got)
	}
}
