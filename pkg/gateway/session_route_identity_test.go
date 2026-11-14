package gateway

import (
	"context"
	"reflect"
	"testing"
)

const routeA, routeB = "http://127.0.0.1:8001/v1", "http://127.0.0.1:8002/v1"

func routeTeamConfig() map[string]any {
	return map[string]any{"providers": map[string]any{"team": map[string]any{"base_url": routeA, "api_key": "sk-team"}}}
}

func TestSameRoute(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	m := func(kv ...string) map[string]any {
		out := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	for i, tc := range []struct {
		a, b, cfg map[string]any
		want      bool
	}{
		{m("provider", "vllm", "base_url", routeA), m("provider", "custom", "base_url", routeA+"/"), map[string]any{}, true},
		{m("provider", "ollama", "base_url", routeA), m("provider", "custom", "base_url", routeA), map[string]any{}, true},
		{m("provider", "team"), m("provider", "custom:team"), routeTeamConfig(), true},
		{m("provider", "team"), m("provider", "custom:team", "base_url", routeA), routeTeamConfig(), true},
		{m("provider", "team"), m("provider", "custom:team", "base_url", routeB), routeTeamConfig(), false},
		{m("provider", "team"), m("provider", "team"), map[string]any{"providers": map[string]any{"team": map[string]any{"base_url": routeB}}}, true},
		{m("provider", "auto"), m("provider", "custom", "base_url", routeB), map[string]any{}, true},
		{m("provider", "deepinfra"), m("provider", "deep-infra"), map[string]any{}, true},
		{m("provider", "openrouter"), m("provider", "anthropic"), map[string]any{}, false},
		{m("provider", "custom", "base_url", routeA), m("provider", "custom", "base_url", routeB), map[string]any{}, false},
	} {
		for _, pair := range [][2]map[string]any{{tc.a, tc.b}, {tc.b, tc.a}} {
			got, err := SameRoute(ctx, pair[0], pair[1], tc.cfg)
			if err != nil || got != tc.want {
				t.Errorf("case %d %v vs %v: %v %v", i, pair[0], pair[1], got, err)
			}
		}
	}
}

func TestResolvedRouteIdentityAndURL(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	for i, tc := range []struct {
		block, cfg map[string]any
		id, url    string
	}{
		{map[string]any{"provider": "custom:team"}, routeTeamConfig(), "custom:team", routeA},
		{map[string]any{"provider": "TEAM", "base_url": routeB + "/"}, routeTeamConfig(), "custom:team", routeB},
		{map[string]any{"provider": "ollama", "base_url": routeA}, map[string]any{}, "custom", routeA},
		{map[string]any{"provider": ""}, routeTeamConfig(), "auto", ""},
	} {
		id, url, err := ResolvedRoute(ctx, tc.block, tc.cfg)
		if err != nil || id != tc.id || url != tc.url {
			t.Errorf("case %d: %q %q %v", i, id, url, err)
		}
	}
}

func TestConfiguredEntryKeyIsTheKeyOrALegacyEntrysDisplayName(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	heldKey := func(entries map[string]any, providerName string) string {
		cfg := map[string]any{"model": map[string]any{"provider": providerName}}
		for k, v := range entries {
			cfg[k] = v
		}
		identity, err := ProfileRouteIdentity(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return ConfiguredEntryKey(ctx, identity, cfg)
	}
	keyed := map[string]any{"providers": map[string]any{"work": map[string]any{"name": "OpenRouter", "base_url": routeA}}}
	legacy := map[string]any{"custom_providers": []any{map[string]any{"name": "OpenRouter", "base_url": routeA}}}
	for i, tc := range []struct {
		entries  map[string]any
		provider string
		want     string
	}{
		{keyed, "custom:work", "work"},
		{keyed, "custom:openrouter", "work"},
		{legacy, "custom:openrouter", "openrouter"},
		{map[string]any{}, "openrouter", ""},
	} {
		if got := heldKey(tc.entries, tc.provider); got != tc.want {
			t.Errorf("case %d: %q", i, got)
		}
	}
}

func TestRouteBoundKeysCoverRouteWireWindowAndCredentials(t *testing.T) {
	want := []string{"provider", "base_url", "api_mode", "context_length", "api_key", "api", "api_key_from", "key_env", "api_key_env"}
	if !reflect.DeepEqual(RouteBoundKeys, want) {
		t.Fatalf("keys %v", RouteBoundKeys)
	}
}
