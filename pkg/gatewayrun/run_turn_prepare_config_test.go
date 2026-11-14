package gatewayrun

import (
	"reflect"
	"testing"
)

// Provider extra body a custom endpoint carries as request overrides
func providerOverrides() map[string]any {
	return map[string]any{"extra_body": map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}}
}

// Runtime kwargs of a local custom endpoint with extra keys layered on
func customRuntimeKwargs(extra map[string]any) map[string]any {
	base := map[string]any{"api_key": "no-key-required", "base_url": "http://10.0.0.1:8000/v1", "provider": "custom",
		"api_mode": "chat_completions", "command": nil, "args": []any{}, "credential_pool": nil, "max_tokens": nil}
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// Bare runner with the given per turn service tier
func tierRunner(tier *string) *GatewayRunner {
	r := allocGatewayRunner()
	r.serviceTier = tier
	return r
}

func TestProviderRequestOverridesPreservedWithoutServiceTier(t *testing.T) {
	rk := customRuntimeKwargs(map[string]any{"request_overrides": providerOverrides()})
	route := tierRunner(nil).resolveTurnAgentConfig("main", rk, tierFromRunner)
	if !reflect.DeepEqual(route.requestOverrides, providerOverrides()) {
		t.Fatalf("overrides %v", route.requestOverrides)
	}
	route.requestOverrides["probe"] = true
	if _, aliased := rk["request_overrides"].(map[string]any)["probe"]; aliased {
		t.Fatal("route overrides alias the runtime kwargs")
	}
}

func TestProviderRequestOverridesMergedUnderFastMode(t *testing.T) {
	original := resolveFastModeOverrides
	resolveFastModeOverrides = func(string, string, string, string) map[string]any { return map[string]any{"service_tier": "priority"} }
	t.Cleanup(func() { resolveFastModeOverrides = original })
	route := tierRunner(strp("priority")).resolveTurnAgentConfig("main", customRuntimeKwargs(map[string]any{"request_overrides": providerOverrides()}), tierFromRunner)
	if !reflect.DeepEqual(route.requestOverrides["extra_body"], providerOverrides()["extra_body"]) || route.requestOverrides["service_tier"] != "priority" {
		t.Fatalf("overrides %v", route.requestOverrides)
	}
}

func TestNoProviderOverridesYieldsEmpty(t *testing.T) {
	route := tierRunner(nil).resolveTurnAgentConfig("main", customRuntimeKwargs(nil), tierFromRunner)
	if len(route.requestOverrides) != 0 || route.requestOverrides == nil {
		t.Fatalf("overrides %v", route.requestOverrides)
	}
}
