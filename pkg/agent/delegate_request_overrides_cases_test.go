package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Config of test_delegate_request_overrides.py _cfg with the given keys laid over the direct endpoint
func requestOverridesCfg(overrides map[string]any) map[string]any {
	cfg := map[string]any{"model": "deepseek/deepseek-v4-flash-0731", "base_url": "https://openrouter.ai/api/v1", "api_key": "test-key-1234567890"}
	for k, v := range overrides {
		cfg[k] = v
	}
	return cfg
}

func resolveOverrides(t *testing.T, cfg map[string]any, parent *AIAgent) map[string]any {
	t.Helper()
	delegationTestEnv(t, 0, false)
	creds, err := resolveDelegationCredentials(context.Background(), cfg, parent)
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

// Scripts the runtime provider resolver the way the hermes resolve_runtime_provider patch does
func scriptRuntimeProvider(t *testing.T, rt *provider.Runtime) {
	t.Helper()
	prev := resolveRuntimeProviderFn
	resolveRuntimeProviderFn = func(context.Context, string, string, string, string, string) (*provider.Runtime, error) {
		return rt, nil
	}
	t.Cleanup(func() { resolveRuntimeProviderFn = prev })
}

func TestDirectBranchForwardsRequestOverrides(t *testing.T) {
	creds := resolveOverrides(t, requestOverridesCfg(map[string]any{
		"request_overrides": map[string]any{"extra_body": map[string]any{"provider": map[string]any{"sort": "throughput"}}},
	}), nil)
	want := map[string]any{"extra_body": map[string]any{"provider": map[string]any{"sort": "throughput"}}}
	if _, has := creds["max_output_tokens"]; has || !reflect.DeepEqual(creds["request_overrides"], want) {
		t.Fatalf("creds %v", creds)
	}
}

func TestDirectBranchAbsentRequestOverridesStaysNone(t *testing.T) {
	if creds := resolveOverrides(t, requestOverridesCfg(nil), nil); creds["request_overrides"] != nil {
		t.Fatalf("creds %v", creds)
	}
}

func TestDirectBranchNonDictRequestOverridesStaysNone(t *testing.T) {
	for _, bad := range []any{"throughput", []any{"extra_body"}, 42} {
		if creds := resolveOverrides(t, requestOverridesCfg(map[string]any{"request_overrides": bad}), nil); creds["request_overrides"] != nil {
			t.Fatalf("%v gave %v", bad, creds)
		}
	}
}

func TestDirectBranchDeepCopiesNestedExtraBody(t *testing.T) {
	source := map[string]any{"extra_body": map[string]any{"provider": map[string]any{"sort": "throughput"}}}
	creds := resolveOverrides(t, requestOverridesCfg(map[string]any{"request_overrides": source}), nil)
	extra := creds["request_overrides"].(map[string]any)["extra_body"].(map[string]any)
	extra["provider"].(map[string]any)["sort"] = "mutated"
	extra["injected"] = true
	if !reflect.DeepEqual(source, map[string]any{"extra_body": map[string]any{"provider": map[string]any{"sort": "throughput"}}}) {
		t.Fatalf("source mutated %v", source)
	}
}

func TestExplicitMergesOverRuntimeOnProviderAlongsideBaseUrl(t *testing.T) {
	scriptRuntimeProvider(t, &provider.Runtime{Provider: "custom", BaseURL: "https://provider-default.example/v1", APIKey: "provider-key",
		APIMode: "chat_completions", RequestOverrides: map[string]any{
			"service_tier": "default",
			"extra_body":   map[string]any{"thinking": map[string]any{"type": "disabled"}, "provider": map[string]any{"sort": "price"}},
		}})
	creds := resolveOverrides(t, requestOverridesCfg(map[string]any{
		"provider":          "mimo",
		"request_overrides": map[string]any{"service_tier": "flex", "extra_body": map[string]any{"provider": map[string]any{"sort": "throughput"}}},
	}), nil)
	want := map[string]any{
		"service_tier": "flex",
		"extra_body":   map[string]any{"thinking": map[string]any{"type": "disabled"}, "provider": map[string]any{"sort": "throughput"}},
	}
	if _, has := creds["max_output_tokens"]; has || !reflect.DeepEqual(creds["request_overrides"], want) {
		t.Fatalf("creds %v", creds)
	}
}

func TestNamedProviderBranchHonorsExplicitKey(t *testing.T) {
	scriptRuntimeProvider(t, &provider.Runtime{Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: "runtime-key",
		APIMode: "chat_completions", RequestOverrides: map[string]any{"extra_body": map[string]any{"reasoning": map[string]any{"enabled": true}}}})
	creds := resolveOverrides(t, map[string]any{
		"model": "deepseek/deepseek-v4-flash-0731", "provider": "openrouter",
		"request_overrides": map[string]any{"extra_body": map[string]any{"provider": map[string]any{"sort": "throughput"}}},
	}, nil)
	want := map[string]any{"extra_body": map[string]any{"reasoning": map[string]any{"enabled": true}, "provider": map[string]any{"sort": "throughput"}}}
	if !reflect.DeepEqual(creds["request_overrides"], want) {
		t.Fatalf("creds %v", creds)
	}
}

func TestNamedProviderBranchWithoutExplicitKeyUnchanged(t *testing.T) {
	scriptRuntimeProvider(t, &provider.Runtime{Provider: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: "runtime-key",
		APIMode: "chat_completions", RequestOverrides: map[string]any{"extra_body": map[string]any{"reasoning": map[string]any{"enabled": true}}}})
	creds := resolveOverrides(t, map[string]any{"model": "m", "provider": "openrouter"}, nil)
	want := map[string]any{"extra_body": map[string]any{"reasoning": map[string]any{"enabled": true}}}
	if !reflect.DeepEqual(creds["request_overrides"], want) {
		t.Fatalf("creds %v", creds)
	}
}

func TestInheritBranchHonorsExplicitKeyOverParent(t *testing.T) {
	parent := &AIAgent{requestOverrides: map[string]any{"service_tier": "default", "extra_body": map[string]any{"thinking": map[string]any{"type": "disabled"}}}}
	creds := resolveOverrides(t, map[string]any{
		"model": "", "provider": "",
		"request_overrides": map[string]any{"extra_body": map[string]any{"provider": map[string]any{"sort": "throughput"}}},
	}, parent)
	want := map[string]any{
		"service_tier": "default",
		"extra_body":   map[string]any{"thinking": map[string]any{"type": "disabled"}, "provider": map[string]any{"sort": "throughput"}},
	}
	if !reflect.DeepEqual(creds["request_overrides"], want) {
		t.Fatalf("creds %v", creds)
	}
}

func TestInheritBranchWithoutKeyStaysNone(t *testing.T) {
	if creds := resolveOverrides(t, map[string]any{"model": "", "provider": ""}, &AIAgent{}); creds["request_overrides"] != nil {
		t.Fatalf("creds %v", creds)
	}
}

func TestInheritBranchDeepCopiesParentOverrides(t *testing.T) {
	parentOverrides := map[string]any{"extra_body": map[string]any{"thinking": map[string]any{"type": "disabled"}}}
	parent := &AIAgent{requestOverrides: parentOverrides}
	creds := resolveOverrides(t, map[string]any{
		"model": "", "provider": "",
		"request_overrides": map[string]any{"extra_body": map[string]any{"provider": map[string]any{"sort": "throughput"}}},
	}, parent)
	creds["request_overrides"].(map[string]any)["extra_body"].(map[string]any)["thinking"].(map[string]any)["type"] = "mutated"
	if !reflect.DeepEqual(parentOverrides, map[string]any{"extra_body": map[string]any{"thinking": map[string]any{"type": "disabled"}}}) {
		t.Fatalf("parent overrides mutated %v", parentOverrides)
	}
}

func TestMergeHelperBothNone(t *testing.T) {
	if mergeRequestOverrides(nil, nil) != nil || mergeRequestOverrides(map[string]any{}, map[string]any{}) != nil || mergeRequestOverrides("junk", 42) != nil {
		t.Fatal("empty inputs must merge to nil")
	}
}

func TestMergeHelperNonDictRuntimeExtraBodyReplaced(t *testing.T) {
	merged := mergeRequestOverrides(map[string]any{"extra_body": "junk"}, map[string]any{"extra_body": map[string]any{"a": 1}})
	if !reflect.DeepEqual(merged, map[string]any{"extra_body": map[string]any{"a": 1}}) {
		t.Fatalf("merged %v", merged)
	}
}
