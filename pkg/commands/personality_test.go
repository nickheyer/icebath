package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"gopkg.in/yaml.v3"
)

var kawaiiPrompt = BuiltinPersonalities["kawaii"]

func TestBuiltinsAvailableWithoutAnyConfig(t *testing.T) {
	merged := AvailablePersonalities(nil)
	if len(merged) < 1 {
		t.Fatal("no builtins")
	}
	for name, def := range merged {
		if name != strings.ToLower(name) {
			t.Errorf("name %q not lower case", name)
		}
		if RenderPersonalityPrompt(def) == "" {
			t.Errorf("builtin %q renders empty", name)
		}
	}
}

func TestUserEntriesOverlayBuiltinsByName(t *testing.T) {
	cfg := config.Tree{"agent": map[string]any{"personalities": map[string]any{"kawaii": "toned down", "custom": "hi"}}}
	merged := AvailablePersonalities(cfg)
	if merged["kawaii"] != "toned down" || merged["custom"] != "hi" {
		t.Fatalf("overlay wrong: %v", merged)
	}
}

func TestRootLevelPersonalitiesAreHonouredBelowAgentBlock(t *testing.T) {
	cfg := config.Tree{"personalities": map[string]any{"robot": "root", "shared": "root"},
		"agent": map[string]any{"personalities": map[string]any{"shared": "agent"}}}
	merged := AvailablePersonalities(cfg)
	if merged["robot"] != "root" || merged["shared"] != "agent" {
		t.Fatalf("merge wrong: %v", merged)
	}
	name, _, err := ResolvePersonality("robot", cfg)
	if err != nil || name != "robot" {
		t.Fatalf("resolve robot: %q %v", name, err)
	}
}

func TestNeutralNamesNormalizeToEmpty(t *testing.T) {
	for _, raw := range []any{"", "none", "None", " DEFAULT ", "neutral", nil} {
		if got := NormalizePersonalityName(raw); got != "" {
			t.Errorf("%v -> %q", raw, got)
		}
	}
}

func TestResolvePersonalityNeutralAndCaseInsensitive(t *testing.T) {
	name, prompt, err := ResolvePersonality("none", config.Tree{})
	if err != nil || name != "" || prompt != "" {
		t.Fatalf("none: %q %q %v", name, prompt, err)
	}
	name, prompt, err = ResolvePersonality("  KAWAII ", config.Tree{})
	if err != nil || name != "kawaii" || prompt != kawaiiPrompt {
		t.Fatalf("kawaii: %q %v", name, err)
	}
}

func TestResolvePersonalityUnknownRaises(t *testing.T) {
	_, _, err := ResolvePersonality("doesnotexist", config.Tree{})
	var typed interface{ PyTypeName() string }
	if !errors.As(err, &typed) || typed.PyTypeName() != "ValueError" {
		t.Fatalf("want ValueError, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "Unknown personality: `doesnotexist`.\n\nAvailable: `none`, `catgirl`, ") {
		t.Fatalf("message: %q", err.Error())
	}
}

func TestResolveOverlayPersonalityWinsOverManualPrompt(t *testing.T) {
	cfg := map[string]any{"display": map[string]any{"personality": "kawaii"}, "agent": map[string]any{"system_prompt": "manual forever"}}
	if got := ResolveEphemeralSystemPrompt(cfg); got != kawaiiPrompt {
		t.Fatalf("got %q", got)
	}
}

func TestResolveOverlayFallsBackToManualPrompt(t *testing.T) {
	for _, neutral := range []string{"", "none", "default", "neutral"} {
		cfg := map[string]any{"display": map[string]any{"personality": neutral}, "agent": map[string]any{"system_prompt": "manual forever"}}
		if got := ResolveEphemeralSystemPrompt(cfg); got != "manual forever" {
			t.Errorf("%q -> %q", neutral, got)
		}
	}
}

func TestResolveOverlayIgnoresUnknownName(t *testing.T) {
	cfg := map[string]any{"display": map[string]any{"personality": "ghost"}, "agent": map[string]any{"system_prompt": "manual forever"}}
	if got := ResolveEphemeralSystemPrompt(cfg); got != "manual forever" {
		t.Fatalf("got %q", got)
	}
	if ActivePersonalityName(cfg) != "" {
		t.Fatal("ghost active")
	}
}

func TestRenderDictPersonality(t *testing.T) {
	rendered := RenderPersonalityPrompt(map[string]any{"system_prompt": "You are X.", "tone": "warm", "style": "brief"})
	for _, want := range []string{"You are X.", "Tone: warm", "Style: brief"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("missing %q in %q", want, rendered)
		}
	}
}

func TestPromptTextNormalizesNoneStrList(t *testing.T) {
	if PromptText(nil) != "" || PromptText("  hi  ") != "hi" || PromptText([]any{"a", " b ", ""}) != "a\nb" {
		t.Fatal("prompt text normalization wrong")
	}
}

func readPersonalityConfig(t *testing.T, h string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPersistPersonalityRoundtrip(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, h)
	ctx := context.Background()
	if !PersistPersonality(ctx, "KAWAII ") {
		t.Fatal("persist failed")
	}
	if got := readPersonalityConfig(t, h)["display"].(map[string]any)["personality"]; got != "kawaii" {
		t.Fatalf("got %v", got)
	}
	if !PersistPersonality(ctx, "none") {
		t.Fatal("persist none failed")
	}
	if got := readPersonalityConfig(t, h)["display"].(map[string]any)["personality"]; got != "" {
		t.Fatalf("got %v", got)
	}
}

func TestPersistPersonalityNeverTouchesSystemPrompt(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("agent:\n  system_prompt: manual forever\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, h)
	if !PersistPersonality(context.Background(), "kawaii") {
		t.Fatal("persist failed")
	}
	raw := readPersonalityConfig(t, h)
	if raw["agent"].(map[string]any)["system_prompt"] != "manual forever" {
		t.Fatalf("system prompt changed: %v", raw)
	}
	if raw["display"].(map[string]any)["personality"] != "kawaii" {
		t.Fatalf("personality not written: %v", raw)
	}
}
