package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func showSources(env map[string]string) ShowSources {
	return ShowSources{
		ConfigPath: "/h/config.yaml", EnvPath: "/h/.env", ProjectRoot: "/opt/icebath",
		EnvValue:     func(n string) string { return env[n] },
		AnthropicKey: func() string { return "" },
		SkillSettings: func() ([]SkillConfigVar, map[string]any, error) {
			return []SkillConfigVar{{Key: "wiki.root", Skill: "wiki"}, {Key: "wiki.token", Skill: "wiki"}}, map[string]any{"wiki.root": "/docs"}, nil
		},
		MaskSecret: func(v, empty string) string {
			if v == "" {
				return empty
			}
			return v[:4] + "..." + v[len(v)-4:]
		},
	}
}

func TestShowConfigNoManagedScopeSilent(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(t.TempDir(), "nope"))
	InvalidateManagedCache()
	var out strings.Builder
	env := map[string]string{"OPENROUTER_API_KEY": "sk-or-1234567890", "TELEGRAM_BOT_TOKEN": "t"}
	if err := ShowConfig(context.Background(), &out, showSources(env)); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(strings.ToLower(text), "managed by your administrator") {
		t.Fatal("managed banner without a managed scope")
	}
	for _, want := range []string{"  OpenRouter     sk-o...7890", "  Anthropic      (not set)", "  Telegram:     configured", "  Discord:      not configured",
		"  wiki.root            /docs  [wiki]", "  wiki.token           (not set)  [wiki]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
}

func TestShowConfigManagedBannerListsPinnedKeys(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	managed := t.TempDir()
	t.Setenv("ICEBATH_MANAGED_DIR", managed)
	os.WriteFile(filepath.Join(managed, "config.yaml"), []byte("model:\n  default: managed/model\n"), 0o600)
	os.WriteFile(filepath.Join(managed, ".env"), []byte("OPENROUTER_API_KEY=sk-x\n"), 0o600)
	InvalidateManagedCache()
	t.Cleanup(InvalidateManagedCache)
	var out strings.Builder
	if err := ShowConfig(context.Background(), &out, showSources(nil)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"managed by your administrator (" + managed + ")", "Managed config keys: model.default", "Managed env keys: OPENROUTER_API_KEY"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in\n%s", want, out.String())
		}
	}
}
