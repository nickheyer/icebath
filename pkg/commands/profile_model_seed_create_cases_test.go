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
	doc := map[string]any{"_config_version": config.Defaults()["_config_version"]}
	for k, v := range source {
		doc[k] = v
	}
	if err := config.AtomicConfigWrite(ctx, h, filepath.Join(h, "config.yaml"), doc, ""); err != nil {
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
	for _, c := range []struct{ schema, provider, urlKey string }{
		{"providers", "lab", "base_url"}, {"providers", "custom:lab", "baseUrl"}, {"custom_providers", "custom:display-name", "base_url"},
	} {
		t.Run(c.schema+"-"+c.provider, func(t *testing.T) {
			endpoint := map[string]any{"name": "Display Name", c.urlKey: seedCaseURL, "api_key": "${LAB_TOKEN}"}
			unrelated := map[string]any{"name": "Unrelated", "base_url": "http://127.0.0.1:11435/v1"}
			var catalog any = []any{endpoint, unrelated}
			if c.schema == "providers" {
				catalog = map[string]any{"lab": endpoint, "other": unrelated}
			}
			model := map[string]any{"provider": c.provider, "default": "synthetic-model"}
			created := createFromLaunch(t, map[string]any{"model": model, c.schema: catalog})
			if !reflect.DeepEqual(created["model"], model) {
				t.Fatalf("model = %#v", created["model"])
			}
			copied := provider.CompatibleCustomProviders(created)
			if len(copied) != 1 || copied[0]["base_url"] != seedCaseURL || copied[0]["api_key"] != "${LAB_TOKEN}" {
				t.Fatalf("copied = %#v", copied)
			}
			resolved := provider.NamedCustomProviderEntry(context.Background(), c.provider, created)
			if resolved == nil || resolved["base_url"] != seedCaseURL {
				t.Fatalf("resolved = %#v", resolved)
			}
		})
	}
}

func TestBuiltinNameCollisionOnlyCopiesExplicitCustomSelection(t *testing.T) {
	endpoint := map[string]any{"name": "OpenRouter", "base_url": seedCaseURL}
	for _, c := range []struct {
		schema, provider string
		catalog          any
		copied           bool
	}{
		{"providers", "openrouter", map[string]any{"other-endpoint": endpoint}, false},
		{"custom_providers", "custom:openrouter", []any{endpoint}, true},
	} {
		t.Run(c.provider, func(t *testing.T) {
			created := createFromLaunch(t, map[string]any{
				"model": map[string]any{"provider": c.provider, "default": "synthetic-model"}, c.schema: c.catalog})
			if got := len(provider.CompatibleCustomProviders(created)) > 0; got != c.copied {
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
