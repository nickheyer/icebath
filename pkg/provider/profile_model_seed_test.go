package provider

import (
	"context"
	"reflect"
	"testing"
)

const seedURL = "http://127.0.0.1:11434/v1"

// Named endpoint the runtime resolves from cfg without reading credentials
func seedResolved(t *testing.T, cfg map[string]any, prov string) map[string]any {
	t.Helper()
	return seedResolver(context.Background(), cfg).getNamedCustomProvider(prov, cfg, false)
}

func seedHome(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
}

func TestModelSeedRetainsResolvableCustomEndpoint(t *testing.T) {
	for _, c := range []struct{ schema, provider, urlKey string }{
		{"providers", "lab", "base_url"}, {"providers", "custom:lab", "baseUrl"}, {"custom_providers", "custom:display-name", "base_url"},
	} {
		seedHome(t)
		endpoint := map[string]any{"name": "Display Name", c.urlKey: seedURL, "api_key": "${LAB_TOKEN}"}
		unrelated := map[string]any{"name": "Unrelated", "base_url": "http://127.0.0.1:11435/v1"}
		var catalog any = []any{endpoint, unrelated}
		if c.schema == "providers" {
			catalog = map[string]any{"lab": endpoint, "other": unrelated}
		}
		model := map[string]any{"provider": c.provider, "default": "synthetic-model"}
		seed := LaunchModelSeed(context.Background(), map[string]any{"model": model, c.schema: catalog})
		if !reflect.DeepEqual(seed["model"], model) {
			t.Fatalf("%s: model = %v", c.provider, seed["model"])
		}
		copied := CompatibleCustomProviders(seed)
		if len(copied) != 1 || copied[0]["base_url"] != seedURL || copied[0]["api_key"] != "${LAB_TOKEN}" {
			t.Fatalf("%s: copied = %v", c.provider, copied)
		}
		if resolved := seedResolved(t, seed, c.provider); resolved == nil || resolved["base_url"] != seedURL {
			t.Fatalf("%s: resolved = %v", c.provider, resolved)
		}
	}
}

func TestModelSeedBuiltinNameCollisionOnlyCopiesExplicitCustomSelection(t *testing.T) {
	seedHome(t)
	endpoint := map[string]any{"name": "OpenRouter", "base_url": seedURL}
	for _, c := range []struct {
		schema, provider string
		catalog          any
		copied           bool
	}{
		{"providers", "openrouter", map[string]any{"other-endpoint": endpoint}, false},
		{"custom_providers", "custom:openrouter", []any{endpoint}, true},
	} {
		seed := LaunchModelSeed(context.Background(), map[string]any{
			"model": map[string]any{"provider": c.provider, "default": "synthetic-model"}, c.schema: c.catalog})
		if got := len(CompatibleCustomProviders(seed)) > 0; got != c.copied {
			t.Fatalf("%s: copied = %v", c.provider, got)
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
	for _, c := range []struct {
		providers map[string]any
		legacy    []any
	}{
		{map[string]any{"lab": map[string]any{"request_timeout_seconds": 150}}, []any{map[string]any{"name": "Lab", "base_url": seedURL}}},
		{map[string]any{"other": map[string]any{"name": "Lab", "base_url": seedURL}, "lab": map[string]any{"base_url": "http://127.0.0.1:11435/v1"}}, []any{}},
	} {
		source := map[string]any{"model": map[string]any{"provider": "lab", "default": "synthetic-model"},
			"providers": c.providers, "custom_providers": c.legacy}
		selected := seedResolved(t, source, "lab")
		if selected == nil {
			t.Fatal("source does not resolve")
		}
		seed := LaunchModelSeed(context.Background(), source)
		p, _ := seed["providers"].(map[string]any)
		l, _ := seed["custom_providers"].([]any)
		if len(p)+len(l) != 1 {
			t.Fatalf("seed = %v", seed)
		}
		if inherited := seedResolved(t, seed, "lab"); inherited == nil || inherited["base_url"] != selected["base_url"] {
			t.Fatalf("inherited = %v, selected = %v", inherited, selected)
		}
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
	for _, c := range []struct {
		schema   string
		endpoint map[string]any
	}{
		{"providers", map[string]any{"name": "${LAB_NAME}", "base_url": seedURL}},
		{"custom_providers", map[string]any{"name": "${LAB_NAME}", "base_url": seedURL}},
		{"custom_providers", map[string]any{"name": "Lab", "base_url": "${LAB_URL}"}},
	} {
		var catalog any = []any{c.endpoint}
		if c.schema == "providers" {
			catalog = map[string]any{"gateway": c.endpoint}
		}
		source := map[string]any{"model": map[string]any{"provider": "custom:lab", "default": "synthetic-model"}, c.schema: catalog}
		seed := LaunchModelSeed(context.Background(), source)
		if !reflect.DeepEqual(seed, source) {
			t.Fatalf("seed = %v", seed)
		}
		dst := map[string]any{"providers": map[string]any{"stale": map[string]any{"name": "lab", "base_url": "http://127.0.0.1:1/v1"}}}
		if _, ok := MergeEndpointSeed(context.Background(), dst, seed)["providers"].(map[string]any)["stale"]; ok {
			t.Fatalf("%v: stale entry kept", c.endpoint)
		}
	}
}

func TestModelSeedUsesSuppliedConfigWithoutReadingCredentials(t *testing.T) {
	seedHome(t)
	source := map[string]any{
		"model":     map[string]any{"provider": "custom:lab", "default": "synthetic-model"},
		"providers": map[string]any{"lab": map[string]any{"api": seedURL, "key_env": "LAB_TOKEN", "api_key": "${LAB_TOKEN}"}},
	}
	if seed := LaunchModelSeed(context.Background(), source); !reflect.DeepEqual(seed, source) {
		t.Fatalf("seed = %v", seed)
	}
}

func TestMergeEndpointSeedDropsSameIdentityEntries(t *testing.T) {
	seedHome(t)
	legacy := func(extra ...any) []any {
		e := map[string]any{"name": "Lab", "base_url": seedURL}
		for i := 0; i+1 < len(extra); i += 2 {
			e[extra[i].(string)] = extra[i+1]
		}
		return []any{e}
	}
	camel := []any{map[string]any{"name": "Lab", "baseUrl": seedURL}}
	for i, c := range []struct {
		seed     []any
		dst      map[string]any
		expected map[string]any
	}{
		{legacy(), map[string]any{"stale": map[string]any{"name": "lab", "base_url": seedURL + "x"}}, map[string]any{}},
		{legacy(), map[string]any{"lab": map[string]any{"base_url": "localhost:9999/v1"}}, map[string]any{}},
		{camel, map[string]any{"lab": map[string]any{"base_url": seedURL + "x"}}, map[string]any{}},
		{legacy("enabled", false), map[string]any{"lab": map[string]any{"base_url": seedURL + "x"}}, map[string]any{}},
		{legacy(), map[string]any{"lab": map[string]any{"request_timeout_seconds": 9},
			"old": map[string]any{"name": "lab", "base_url": seedURL + "x", "enabled": false}}, map[string]any{}},
		{legacy(), map[string]any{"other": map[string]any{"base_url": seedURL + "x"}}, map[string]any{"other": map[string]any{"base_url": seedURL + "x"}}},
	} {
		seed := map[string]any{"model": map[string]any{"provider": "custom:lab", "default": "synthetic-model"}, "custom_providers": c.seed}
		merged := MergeEndpointSeed(context.Background(), map[string]any{"providers": c.dst}, seed)
		if want := map[string]any{"providers": c.expected, "custom_providers": c.seed}; !reflect.DeepEqual(merged, want) {
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
	if got := MergeEndpointSeed(context.Background(), dst, seed)["providers"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("providers = %v", got)
	}
}
