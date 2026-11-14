package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func showSources(cfg map[string]any, env map[string]string) ShowSources {
	return ShowSources{
		Config: cfg, ConfigPath: "/h/config.yaml", EnvPath: "/h/.env", ProjectRoot: "/opt/icebath",
		EnvValue:          func(n string) string { return env[n] },
		EnvFile:           func() (map[string]string, error) { return env, nil },
		AnthropicKey:      func() string { return "" },
		ActivePersonality: func(map[string]any) (string, error) { return "", errors.New("no personality module") },
		SkillSettings: func() ([]SkillConfigVar, map[string]any, error) {
			return []SkillConfigVar{{Key: "wiki.root", Skill: "wiki"}, {Key: "wiki.token", Skill: "wiki"}}, map[string]any{"wiki.root": "/docs"}, nil
		},
		MaskSecret: func(v, empty string) string {
			if v == "" {
				return empty
			}
			return v[:4] + "..." + v[len(v)-4:]
		},
		RedactConfigValue: func(v any) any { return v },
	}
}

func TestShowConfigNoManagedScopeSilent(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(t.TempDir(), "nope"))
	InvalidateManagedCache()
	var out strings.Builder
	cfg := map[string]any{"model": map[string]any{"default": "user/model"}, "display": map[string]any{"personality": "kawaii"},
		"terminal": map[string]any{"backend": "ssh"}, "compression": map[string]any{"threshold_tokens": 120000},
		"auxiliary": map[string]any{"vision": map[string]any{"model": "gpt-4o"}}}
	env := map[string]string{"OPENROUTER_API_KEY": "sk-or-1234567890", "ICEBATH_MAX_ITERATIONS": "60", "TELEGRAM_BOT_TOKEN": "t"}
	if err := ShowConfig(&out, showSources(cfg, env)); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(strings.ToLower(text), "managed by your administrator") {
		t.Fatal("managed banner without a managed scope")
	}
	for _, want := range []string{"  Model:        {'default': 'user/model'}", "  Max turns:    None",
		"stale ICEBATH_MAX_ITERATIONS=60", "  Personality:  kawaii", "  SSH host:     (not set)", "  Threshold:    50%",
		"  Token cap:    120,000 tokens", "  Target ratio: 20% of threshold preserved", "  Vision        provider=auto, model=gpt-4o",
		"  OpenRouter     sk-o...7890", "  Anthropic      (not set)", "  Telegram:     configured", "  Discord:      not configured",
		"  wiki.root            /docs  [wiki]", "  wiki.token           (not set)  [wiki]", "  Timezone:     (server-local)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if err := ShowConfig(&out, showSources(map[string]any{"compression": map[string]any{"threshold": "half"}}, nil)); err == nil {
		t.Fatal("non numeric threshold accepted")
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
	if err := ShowConfig(&out, showSources(map[string]any{}, nil)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"managed by your administrator (" + managed + ")", "Managed config keys: model.default", "Managed env keys: OPENROUTER_API_KEY"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in\n%s", want, out.String())
		}
	}
}

func TestShowConfigModelKeepsFileOrder(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("ICEBATH_MANAGED_DIR", filepath.Join(t.TempDir(), "nope"))
	InvalidateManagedCache()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	text := "model:\n  provider: custom\n  base_url: http://127.0.0.1:9/v1\n  default: fake-model\n  context_length: 128000\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(h, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	src := showSources(cfg, nil)
	src.RedactConfigValue = RedactConfigValue
	var out strings.Builder
	if err := ShowConfig(&out, src); err != nil {
		t.Fatal(err)
	}
	want := "  Model:        {'provider': 'custom', 'base_url': 'http://127.0.0.1:9/v1', 'default': 'fake-model', 'context_length': 128000}"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("missing %q in\n%s", want, out.String())
	}
}
