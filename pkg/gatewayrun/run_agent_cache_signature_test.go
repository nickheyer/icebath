package gatewayrun

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

// test_agent_cache.py TestAgentConfigSignature test_model_change_different_signature
func TestModelChangeDifferentSignature(t *testing.T) {
	runtime := map[string]any{"api_key": "sk-test12345678", "base_url": "https://openrouter.ai/api/v1", "provider": "openrouter"}
	sig1 := agentConfigSignature("claude-sonnet-4", runtime, []string{"hermes-telegram"}, "", nil, "", "", false)
	sig2 := agentConfigSignature("claude-opus-4.6", runtime, []string{"hermes-telegram"}, "", nil, "", "", false)
	if sig1 == sig2 {
		t.Fatal("model change kept the signature")
	}
}

// test_agent_cache.py TestAgentConfigSignature test_same_token_prefix_different_full_token_changes_signature
func TestSameTokenPrefixDifferentFullTokenChangesSignature(t *testing.T) {
	rt1 := map[string]any{"api_key": "eyJhbGci.token-for-account-a", "base_url": "https://chatgpt.com/backend-api/codex",
		"provider": "openai-codex", "api_mode": "codex_responses"}
	rt2 := map[string]any{"api_key": "eyJhbGci.token-for-account-b", "base_url": "https://chatgpt.com/backend-api/codex",
		"provider": "openai-codex", "api_mode": "codex_responses"}
	if rt1["api_key"].(string)[:8] != rt2["api_key"].(string)[:8] {
		t.Fatal("fixtures must share a prefix")
	}
	sig1 := agentConfigSignature("gpt-5.3-codex", rt1, []string{"hermes-telegram"}, "", nil, "", "", false)
	sig2 := agentConfigSignature("gpt-5.3-codex", rt2, []string{"hermes-telegram"}, "", nil, "", "", false)
	if sig1 == sig2 {
		t.Fatal("tokens sharing a prefix collided")
	}
}

// test_agent_cache.py TestAgentConfigSignature test_provider_change_different_signature
func TestProviderChangeDifferentSignature(t *testing.T) {
	rt1 := map[string]any{"api_key": "sk-test12345678", "base_url": "https://openrouter.ai/api/v1", "provider": "openrouter"}
	rt2 := map[string]any{"api_key": "sk-test12345678", "base_url": "https://api.anthropic.com", "provider": "anthropic"}
	sig1 := agentConfigSignature("claude-sonnet-4", rt1, []string{"hermes-telegram"}, "", nil, "", "", false)
	sig2 := agentConfigSignature("claude-sonnet-4", rt2, []string{"hermes-telegram"}, "", nil, "", "", false)
	if sig1 == sig2 {
		t.Fatal("provider change kept the signature")
	}
}

// test_agent_cache.py TestAgentConfigSignature test_capability_change_different_signature
func TestCapabilityChangeDifferentSignature(t *testing.T) {
	native := map[string]any{"api_key": "sk-test12345678", "base_url": "https://proxy.example/v1", "provider": "custom",
		"capabilities": map[string]any{"openai_native_compaction": true}}
	plain := map[string]any{"api_key": "sk-test12345678", "base_url": "https://proxy.example/v1", "provider": "custom",
		"capabilities": map[string]any{"openai_native_compaction": false}}
	if agentConfigSignature("gpt-5.6", native, nil, "", nil, "", "", false) == agentConfigSignature("gpt-5.6", plain, nil, "", nil, "", "", false) {
		t.Fatal("capability change kept the signature")
	}
}

// test_agent_cache.py TestAgentConfigSignature test_compression_threshold_change_busts_cache
func TestCompressionThresholdChangeBustsCache(t *testing.T) {
	runtime := map[string]any{"api_key": "k", "base_url": "u", "provider": "p"}
	sig1 := agentConfigSignature("m", runtime, nil, "", map[string]any{"compression.threshold": 0.50}, "", "", false)
	sig2 := agentConfigSignature("m", runtime, nil, "", map[string]any{"compression.threshold": 0.75}, "", "", false)
	if sig1 == sig2 {
		t.Fatal("threshold change kept the signature")
	}
}

// test_agent_cache.py TestAgentConfigSignature test_cache_keys_key_order_does_not_matter
func TestCacheKeysKeyOrderDoesNotMatter(t *testing.T) {
	runtime := map[string]any{"api_key": "k", "base_url": "u", "provider": "p"}
	sigA := agentConfigSignature("m", runtime, nil, "", map[string]any{"model.context_length": 200_000, "compression.threshold": 0.5}, "", "", false)
	sigB := agentConfigSignature("m", runtime, nil, "", map[string]any{"compression.threshold": 0.5, "model.context_length": 200_000}, "", "", false)
	if sigA != sigB {
		t.Fatal("key order changed the signature")
	}
}

// Shipped default of a section key
func shippedDefault(section, key string) any {
	return cfgGetDefault(map[string]any(config.Defaults()), nil, section, key)
}

// test_agent_cache.py TestExtractCacheBustingConfig test_missing_keys_yield_the_shipped_default
func TestMissingKeysYieldTheShippedDefault(t *testing.T) {
	out := extractCacheBustingConfig(context.Background(), map[string]any{})
	for _, pair := range cacheBustingConfigKeys {
		if got, want := out[pair[0]+"."+pair[1]], shippedDefault(pair[0], pair[1]); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s.%s = %#v, want %#v", pair[0], pair[1], got, want)
		}
	}
}

// test_agent_cache.py TestExtractCacheBustingConfig test_explicit_cap_differs_from_absent
func TestExplicitCapDiffersFromAbsent(t *testing.T) {
	ctx := context.Background()
	defaultCap := shippedDefault("compression", "threshold_tokens")
	base := 0
	if n, ok := defaultCap.(int); ok {
		base = n
	}
	otherCap := base + 100_000
	sig := func(cfg map[string]any) any {
		return extractCacheBustingConfig(ctx, cfg)["compression.threshold_tokens"]
	}
	if a, b := sig(map[string]any{}), sig(map[string]any{"compression": map[string]any{"threshold_tokens": defaultCap}}); !reflect.DeepEqual(a, b) || !reflect.DeepEqual(a, defaultCap) {
		t.Fatalf("absent %#v explicit default %#v shipped %#v", a, b, defaultCap)
	}
	got := sig(map[string]any{"compression": map[string]any{"threshold_tokens": otherCap}})
	if !reflect.DeepEqual(got, otherCap) || reflect.DeepEqual(got, sig(map[string]any{})) {
		t.Fatalf("explicit cap gave %#v", got)
	}
}

// Checkpoint prefixed entries of an extracted signature
func checkpointEntries(out map[string]any) map[string]any {
	kept := map[string]any{}
	for k, v := range out {
		if strings.HasPrefix(k, "checkpoints.") {
			kept[k] = v
		}
	}
	return kept
}

// test_agent_cache.py TestExtractCacheBustingConfig test_legacy_checkpoints_bool_carries_defaults_for_the_other_keys
func TestLegacyCheckpointsBoolCarriesDefaultsForTheOtherKeys(t *testing.T) {
	ctx := context.Background()
	legacy := extractCacheBustingConfig(ctx, map[string]any{"checkpoints": true})
	explicit := extractCacheBustingConfig(ctx, map[string]any{"checkpoints": map[string]any{"enabled": true}})
	if legacy["checkpoints.enabled"] != true {
		t.Fatalf("checkpoints.enabled = %#v", legacy["checkpoints.enabled"])
	}
	if !reflect.DeepEqual(checkpointEntries(legacy), checkpointEntries(explicit)) {
		t.Fatalf("legacy %#v explicit %#v", checkpointEntries(legacy), checkpointEntries(explicit))
	}
}

// test_agent_cache.py TestExtractCacheBustingConfig test_non_dict_section_treated_as_missing
func TestNonDictSectionTreatedAsMissing(t *testing.T) {
	out := extractCacheBustingConfig(context.Background(), map[string]any{"compression": "broken", "model": map[string]any{"context_length": 100_000}})
	if !reflect.DeepEqual(out["compression.enabled"], shippedDefault("compression", "enabled")) {
		t.Fatalf("compression.enabled = %#v", out["compression.enabled"])
	}
	if !reflect.DeepEqual(out["compression.threshold"], shippedDefault("compression", "threshold")) {
		t.Fatalf("compression.threshold = %#v", out["compression.threshold"])
	}
	if out["model.context_length"] != 100_000 {
		t.Fatalf("model.context_length = %#v", out["model.context_length"])
	}
}

// test_agent_cache.py TestExtractCacheBustingConfig test_none_config_is_safe
func TestNoneConfigIsSafe(t *testing.T) {
	out := extractCacheBustingConfig(context.Background(), nil)
	for _, pair := range cacheBustingConfigKeys {
		if got, want := out[pair[0]+"."+pair[1]], shippedDefault(pair[0], pair[1]); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s.%s = %#v, want %#v", pair[0], pair[1], got, want)
		}
	}
	if _, ok := out["tools.registry_generation"]; !ok {
		t.Fatal("registry generation missing")
	}
}

// test_agent_cache.py TestExtractCacheBustingConfig test_extract_includes_live_tool_registry_generation
func TestExtractIncludesLiveToolRegistryGeneration(t *testing.T) {
	before := extractCacheBustingConfig(context.Background(), map[string]any{})["tools.registry_generation"]
	if before != tools.Default.Generation() {
		t.Fatalf("generation %#v, registry %d", before, tools.Default.Generation())
	}
	name := "gatewayrun_generation_probe"
	tools.Default.Register(tools.Def{Name: name, Toolset: "gatewayrun_probe", Schema: map[string]any{"name": name},
		Handler: func(context.Context, *tools.Call) string { return "" }})
	t.Cleanup(func() { _ = tools.Default.Deregister(name, "", "") })
	after := extractCacheBustingConfig(context.Background(), map[string]any{})["tools.registry_generation"]
	if after != tools.Default.Generation() || after == before {
		t.Fatalf("generation %#v after a registration, before %#v", after, before)
	}
}

// Memory provider whose identity hook returns, panics or is absent by mode
type identityProvider struct {
	memory.BaseProvider
	name   string
	writer string
	mode   string
}

func (p *identityProvider) Name() string                                                 { return p.name }
func (p *identityProvider) Available(context.Context) bool                               { return true }
func (p *identityProvider) Initialize(context.Context, string, memory.InitOptions) error { return nil }
func (p *identityProvider) ToolSchemas(context.Context) ([]map[string]any, error) {
	return []map[string]any{}, nil
}
func (p *identityProvider) IdentitySignature(ctx context.Context) map[string]any {
	switch p.mode {
	case "identity":
		return map[string]any{"fakeprov.writer": p.writer, "fakeprov.aliases": []any{[]any{"a", "b"}}}
	case "raising hook":
		panic("boom")
	}
	return p.BaseProvider.IdentitySignature(ctx)
}

type loadCall struct {
	name           string
	registerSkills bool
}

// Routes provider loading for a name to the given instance under a fresh home and an empty memo
func installFakeProvider(t *testing.T, name string, provider memory.Provider) *[]loadCall {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	memoryIdentityProviderMu.Lock()
	saved := memoryIdentityProviderMemo
	memoryIdentityProviderMemo = map[string]memory.Provider{}
	memoryIdentityProviderMu.Unlock()
	calls := &[]loadCall{}
	if provider != nil {
		memory.RegisterProvider(memory.ProviderRegistration{Name: name, Source: memory.SourceUser, Home: dir,
			New: func(_ context.Context, _ *tools.Env, opts memory.LoadOptions) (memory.Provider, error) {
				*calls = append(*calls, loadCall{name, opts.RegisterSkills})
				return provider, nil
			}})
	}
	t.Cleanup(func() {
		memory.UnregisterProviders(memory.SourceUser, dir)
		memoryIdentityProviderMu.Lock()
		memoryIdentityProviderMemo = saved
		memoryIdentityProviderMu.Unlock()
	})
	if home.Dir(context.Background()) != dir {
		t.Fatalf("home %s, want %s", home.Dir(context.Background()), dir)
	}
	return calls
}

// Memory keys a provider added beyond the documented config keys
func providerDeclaredKeys(out map[string]any) []string {
	documented := map[string]bool{}
	for _, pair := range cacheBustingConfigKeys {
		if pair[0] == "memory" {
			documented[pair[0]+"."+pair[1]] = true
		}
	}
	keys := []string{}
	for k := range out {
		if strings.HasPrefix(k, "memory.") && !documented[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// test_agent_cache.py TestExtractCacheBustingConfig test_provider_identity_signature_enters_under_memory_prefix_and_is_re_read_from_one_instance
func TestProviderIdentitySignatureEntersUnderMemoryPrefixAndIsReReadFromOneInstance(t *testing.T) {
	provider := &identityProvider{name: "fakeprov", writer: "alice", mode: "identity"}
	calls := installFakeProvider(t, "fakeprov", provider)
	cfg := map[string]any{"memory": map[string]any{"provider": "fakeprov"}}
	first := extractCacheBustingConfig(context.Background(), cfg)
	provider.writer = "bob"
	second := extractCacheBustingConfig(context.Background(), cfg)
	if got := providerDeclaredKeys(first); !reflect.DeepEqual(got, []string{"memory.fakeprov.aliases", "memory.fakeprov.writer"}) {
		t.Fatalf("declared keys %v", got)
	}
	if !reflect.DeepEqual(first["memory.fakeprov.aliases"], []any{[]any{"a", "b"}}) {
		t.Fatalf("aliases %#v", first["memory.fakeprov.aliases"])
	}
	if first["memory.fakeprov.writer"] != "alice" || second["memory.fakeprov.writer"] != "bob" {
		t.Fatalf("writers %#v %#v", first["memory.fakeprov.writer"], second["memory.fakeprov.writer"])
	}
	if !reflect.DeepEqual(*calls, []loadCall{{"fakeprov", false}}) {
		t.Fatalf("calls %#v", *calls)
	}
}

// test_agent_cache.py TestExtractCacheBustingConfig test_provider_contributes_nothing_without_a_working_identity_hook
func TestProviderContributesNothingWithoutAWorkingIdentityHook(t *testing.T) {
	for _, kind := range []string{"no hook", "no provider", "raising hook"} {
		t.Run(kind, func(t *testing.T) {
			var provider memory.Provider
			switch kind {
			case "no hook":
				provider = &identityProvider{name: "p"}
			case "raising hook":
				provider = &identityProvider{name: "p", mode: "raising hook"}
			}
			calls := installFakeProvider(t, "p", provider)
			cfg := map[string]any{}
			if provider != nil {
				cfg = map[string]any{"memory": map[string]any{"provider": "p"}}
			}
			out := extractCacheBustingConfig(context.Background(), cfg)
			if got := providerDeclaredKeys(out); len(got) != 0 {
				t.Fatalf("declared keys %v", got)
			}
			if _, ok := out["tools.registry_generation"]; !ok {
				t.Fatal("registry generation missing")
			}
			want := []loadCall{}
			if provider != nil {
				want = []loadCall{{"p", false}}
			}
			if !reflect.DeepEqual(*calls, want) {
				t.Fatalf("calls %#v", *calls)
			}
		})
	}
}
