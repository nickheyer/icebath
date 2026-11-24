package provider

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

const seedURL = "http://127.0.0.1:11434/v1"

// Named endpoint the runtime resolves from cfg without reading credentials
func seedResolved(t *testing.T, cfg map[string]any, prov string) map[string]any {
	t.Helper()
	return seedResolver(context.Background()).getNamedCustomProvider(prov, config.AsMap(cfg["providers"]), false)
}

func seedHome(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
}

func TestModelSeedRetainsResolvableCustomEndpoint(t *testing.T) {
	for _, provider := range []string{"lab", "custom:lab", "custom:display-name"} {
		seedHome(t)
		endpoint := map[string]any{"name": "Display Name", "base_url": seedURL, "api_key": "${LAB_TOKEN}"}
		unrelated := map[string]any{"name": "Unrelated", "base_url": "http://127.0.0.1:11435/v1"}
		model := map[string]any{"provider": provider, "default": "synthetic-model"}
		seed := LaunchModelSeed(context.Background(), map[string]any{"model": model, "providers": map[string]any{"lab": endpoint, "other": unrelated}})
		if !reflect.DeepEqual(seed["model"], model) {
			t.Fatalf("%s: model = %v", provider, seed["model"])
		}
		copied := ProviderEntriesOf(config.AsMap(seed["providers"]))
		if len(copied) != 1 || copied[0]["base_url"] != seedURL || copied[0]["api_key"] != "${LAB_TOKEN}" {
			t.Fatalf("%s: copied = %v", provider, copied)
		}
		if resolved := seedResolved(t, seed, provider); resolved == nil || resolved["base_url"] != seedURL {
			t.Fatalf("%s: resolved = %v", provider, resolved)
		}
	}
}

func TestModelSeedBuiltinNameCollisionOnlyCopiesExplicitCustomSelection(t *testing.T) {
	seedHome(t)
	providers := map[string]any{"other-endpoint": map[string]any{"name": "OpenRouter", "base_url": seedURL}}
	for provider, copied := range map[string]bool{"openrouter": false, "custom:openrouter": true} {
		seed := LaunchModelSeed(context.Background(), map[string]any{
			"model": map[string]any{"provider": provider, "default": "synthetic-model"}, "providers": providers})
		if got := len(ProviderEntriesOf(config.AsMap(seed["providers"]))) > 0; got != copied {
			t.Fatalf("%s: copied = %v", provider, got)
		}
	}
}

func TestModelSeedKeepsExactKeyProviderTuning(t *testing.T) {
	seedHome(t)
	for _, model := range []map[string]any{
		{"provider": "openrouter", "default": "synthetic-model"},
		{"provider": "custom", "default": "synthetic-model", "base_url": seedURL},
	} {
		selected := map[string]any{model["provider"].(string): map[string]any{"request_timeout_seconds": 150}}
		providers := map[string]any{"neighbor": map[string]any{"request_timeout_seconds": 5}}
		for k, v := range selected {
			providers[k] = v
		}
		seed := LaunchModelSeed(context.Background(), map[string]any{"model": model, "providers": providers})
		if !reflect.DeepEqual(seed["providers"], selected) {
			t.Fatalf("%v: providers = %v", model["provider"], seed["providers"])
		}
	}
}

func TestModelSeedKeepsRuntimeEndpointSelection(t *testing.T) {
	seedHome(t)
	source := map[string]any{"model": map[string]any{"provider": "lab", "default": "synthetic-model"}, "providers": map[string]any{
		"other": map[string]any{"name": "Lab", "base_url": seedURL}, "lab": map[string]any{"base_url": "http://127.0.0.1:11435/v1"}}}
	selected := seedResolved(t, source, "lab")
	if selected == nil {
		t.Fatal("source does not resolve")
	}
	seed := LaunchModelSeed(context.Background(), source)
	if p, _ := seed["providers"].(map[string]any); len(p) != 1 {
		t.Fatalf("seed = %v", seed)
	}
	if inherited := seedResolved(t, seed, "lab"); inherited == nil || inherited["base_url"] != selected["base_url"] {
		t.Fatalf("inherited = %v, selected = %v", inherited, selected)
	}
}

func TestModelSeedSelectsEnvReferencedProvider(t *testing.T) {
	seedHome(t)
	t.Setenv("LAB_PROVIDER", "custom:lab")
	endpoint := map[string]any{"base_url": seedURL}
	model := map[string]any{"provider": "${LAB_PROVIDER}", "default": "synthetic-model"}
	seed := LaunchModelSeed(context.Background(), map[string]any{"model": model,
		"providers": map[string]any{"lab": endpoint, "other": map[string]any{"base_url": "http://127.0.0.1:1/v1"}}})
	if want := map[string]any{"model": model, "providers": map[string]any{"lab": endpoint}}; !reflect.DeepEqual(seed, want) {
		t.Fatalf("seed = %v", seed)
	}
}

func TestModelSeedMatchesEnvReferencedEndpointNames(t *testing.T) {
	seedHome(t)
	t.Setenv("LAB_NAME", "Lab")
	t.Setenv("LAB_URL", seedURL)
	for _, endpoint := range []map[string]any{
		{"name": "${LAB_NAME}", "base_url": seedURL},
		{"name": "Lab", "base_url": "${LAB_URL}"},
	} {
		source := map[string]any{"model": map[string]any{"provider": "custom:lab", "default": "synthetic-model"},
			"providers": map[string]any{"gateway": endpoint}}
		seed := LaunchModelSeed(context.Background(), source)
		if !reflect.DeepEqual(seed, source) {
			t.Fatalf("seed = %v", seed)
		}
		dst := map[string]any{"providers": map[string]any{"stale": map[string]any{"name": "lab", "base_url": "http://127.0.0.1:1/v1"}}}
		if _, ok := MergeEndpointSeed(context.Background(), dst, seed, nil)["providers"].(map[string]any)["stale"]; ok {
			t.Fatalf("%v: stale entry kept", endpoint)
		}
	}
}

func TestModelSeedUsesSuppliedConfigWithoutReadingCredentials(t *testing.T) {
	seedHome(t)
	source := map[string]any{
		"model":     map[string]any{"provider": "custom:lab", "default": "synthetic-model"},
		"providers": map[string]any{"lab": map[string]any{"base_url": seedURL, "key_env": "LAB_TOKEN", "api_key": "${LAB_TOKEN}"}},
	}
	if seed := LaunchModelSeed(context.Background(), source); !reflect.DeepEqual(seed, source) {
		t.Fatalf("seed = %v", seed)
	}
}

func TestMergeEndpointSeedDropsSameIdentityEntries(t *testing.T) {
	seedHome(t)
	seeded := map[string]any{"name": "Lab", "base_url": seedURL}
	other := map[string]any{"base_url": seedURL + "x"}
	for i, c := range []struct{ dst, kept map[string]any }{
		{map[string]any{"stale": map[string]any{"name": "lab", "base_url": seedURL + "x"}}, map[string]any{}},
		{map[string]any{"lab": map[string]any{"base_url": "localhost:9999/v1"}}, map[string]any{}},
		{map[string]any{"lab": map[string]any{"request_timeout_seconds": 9},
			"old": map[string]any{"name": "lab", "base_url": seedURL + "x", "enabled": false}}, map[string]any{}},
		{map[string]any{"other": other}, map[string]any{"other": other}},
	} {
		seed := map[string]any{"model": map[string]any{"provider": "custom:lab", "default": "synthetic-model"},
			"providers": map[string]any{"gateway": seeded}}
		merged := MergeEndpointSeed(context.Background(), map[string]any{"providers": c.dst}, seed, nil)
		want := map[string]any{"gateway": seeded}
		for k, v := range c.kept {
			want[k] = v
		}
		if !reflect.DeepEqual(merged, map[string]any{"providers": want}) {
			t.Fatalf("case %d: merged = %v", i, merged)
		}
		if resolved := seedResolved(t, merged, "custom:lab"); resolved == nil || resolved["base_url"] != seedURL {
			t.Fatalf("case %d: resolved = %v", i, resolved)
		}
	}
}

func TestMergeEndpointSeedTuningSeedReplacesOnlyItsKey(t *testing.T) {
	seedHome(t)
	seed := map[string]any{
		"model":     map[string]any{"provider": "custom", "default": "synthetic-model", "base_url": seedURL},
		"providers": map[string]any{"custom": map[string]any{"request_timeout_seconds": 5}},
	}
	dst := map[string]any{"providers": map[string]any{"local": map[string]any{"name": "custom", "base_url": seedURL + "x"}}}
	want := map[string]any{"custom": map[string]any{"request_timeout_seconds": 5}, "local": map[string]any{"name": "custom", "base_url": seedURL + "x"}}
	if got := MergeEndpointSeed(context.Background(), dst, seed, nil)["providers"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("providers = %v", got)
	}
}
