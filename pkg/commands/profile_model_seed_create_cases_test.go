package commands

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
)

const seedCaseURL = "http://127.0.0.1:11434/v1"

// Writes source as the launch config, creates a fresh profile and returns its raw config
func createFromLaunch(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	ctx := context.Background()
	if err := config.AtomicConfigWrite(ctx, h, filepath.Join(h, "config.yaml"), source, ""); err != nil {
		t.Fatal(err)
	}
	profile, err := manage.CreateProfile(ctx, "sample-bot", manage.CreateOptions{NoSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := config.ReadUserConfigRaw(ctx, filepath.Join(profile, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestFreshProfileRetainsResolvableCustomEndpoint(t *testing.T) {
	for _, selected := range []string{"lab", "custom:lab", "custom:display-name"} {
		t.Run(selected, func(t *testing.T) {
			endpoint := map[string]any{"name": "Display Name", "base_url": seedCaseURL, "api_key": "${LAB_TOKEN}"}
			unrelated := map[string]any{"name": "Unrelated", "base_url": "http://127.0.0.1:11435/v1"}
			model := map[string]any{"provider": selected, "default": "synthetic-model"}
			created := createFromLaunch(t, map[string]any{"model": model, "providers": map[string]any{"lab": endpoint, "other": unrelated}})
			if !reflect.DeepEqual(created["model"], model) {
				t.Fatalf("model = %#v", created["model"])
			}
			copied := provider.ProviderEntries(created)
			if len(copied) != 1 || copied[0]["base_url"] != seedCaseURL || copied[0]["api_key"] != "${LAB_TOKEN}" {
				t.Fatalf("copied = %#v", copied)
			}
			resolved := provider.NamedCustomProviderEntry(context.Background(), selected, created)
			if resolved == nil || resolved["base_url"] != seedCaseURL {
				t.Fatalf("resolved = %#v", resolved)
			}
		})
	}
}

func TestBuiltinNameCollisionOnlyCopiesExplicitCustomSelection(t *testing.T) {
	endpoint := map[string]any{"name": "OpenRouter", "base_url": seedCaseURL}
	for _, c := range []struct {
		provider string
		copied   bool
	}{{"openrouter", false}, {"custom:openrouter", true}} {
		t.Run(c.provider, func(t *testing.T) {
			created := createFromLaunch(t, map[string]any{
				"model": map[string]any{"provider": c.provider, "default": "synthetic-model"}, "providers": map[string]any{"other-endpoint": endpoint}})
			if got := len(provider.ProviderEntries(created)) > 0; got != c.copied {
				t.Fatalf("copied = %v", got)
			}
		})
	}
}

func TestFreshProfileKeepsExactKeyProviderTuning(t *testing.T) {
	for _, model := range []map[string]any{
		{"provider": "openrouter", "default": "synthetic-model"},
		{"provider": "custom", "default": "synthetic-model", "base_url": seedCaseURL},
	} {
		t.Run(model["provider"].(string), func(t *testing.T) {
			selected := map[string]any{model["provider"].(string): map[string]any{"request_timeout_seconds": 150}}
			providers := map[string]any{"neighbor": map[string]any{"request_timeout_seconds": 5}}
			for k, v := range selected {
				providers[k] = v
			}
			created := createFromLaunch(t, map[string]any{"model": model, "providers": providers})
			if !reflect.DeepEqual(created["providers"], selected) {
				t.Fatalf("providers = %#v", created["providers"])
			}
		})
	}
}
