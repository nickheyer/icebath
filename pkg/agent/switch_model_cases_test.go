package agent

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

var switchCustomProviders = []map[string]any{
	{"name": "main-think", "base_url": "http://10.0.0.1:8000/v1", "model": "think-model", "extra_body": map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}},
	{"name": "main-plain", "base_url": "http://10.0.0.1:8000/v1", "model": "plain-model", "extra_body": map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}},
}

// Agent carrying the init time custom provider cache the switch helper reads
func switchOverridesAgent(model, baseURL string, overrides map[string]any) *AIAgent {
	a := &AIAgent{model: model, provider: "custom", requestOverrides: overrides, customProviderEntries: switchCustomProviders}
	a.customProviders = make([]any, len(switchCustomProviders))
	for i, e := range switchCustomProviders {
		a.customProviders[i] = e
	}
	a.setBaseURL(baseURL)
	return a
}

func TestSwitchAppliesMatchedProviderExtraBody(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := switchOverridesAgent("think-model", "http://10.0.0.1:8000/v1", map[string]any{"service_tier": "priority"})
	applySwitchedProviderRequestOverrides(context.Background(), a, "custom:main-think")
	if !reflect.DeepEqual(a.requestOverrides["extra_body"], map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}) || a.requestOverrides["service_tier"] != "priority" {
		t.Fatalf("overrides %v", a.requestOverrides)
	}
}

func TestSwitchToNoncustomClearsStaleExtraBody(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := switchOverridesAgent("claude-x", "https://api.anthropic.com", map[string]any{"extra_body": map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}, "service_tier": "priority"})
	applySwitchedProviderRequestOverrides(context.Background(), a, "anthropic")
	if _, ok := a.requestOverrides["extra_body"]; ok || a.requestOverrides["service_tier"] != "priority" {
		t.Fatalf("overrides %v", a.requestOverrides)
	}
}

func TestSwitchFromNoneOverrides(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := switchOverridesAgent("plain-model", "http://10.0.0.1:8000/v1", nil)
	applySwitchedProviderRequestOverrides(context.Background(), a, "custom:main-plain")
	if !reflect.DeepEqual(a.requestOverrides, map[string]any{"extra_body": map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}}) {
		t.Fatalf("overrides %v", a.requestOverrides)
	}
}

func TestSwitchToDifferentModelSameEndpointDoesNotInherit(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := switchOverridesAgent("plain-model", "http://10.0.0.1:8000/v1", map[string]any{"extra_body": map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}})
	applySwitchedProviderRequestOverrides(context.Background(), a, "custom:main-think")
	if _, ok := a.requestOverrides["extra_body"]; ok {
		t.Fatalf("overrides %v", a.requestOverrides)
	}
}

func TestSwitchEndpointMismatchDoesNotInherit(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := switchOverridesAgent("think-model", "http://10.9.9.9:8000/v1", map[string]any{"extra_body": map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": true}}})
	applySwitchedProviderRequestOverrides(context.Background(), a, "custom:main-think")
	if _, ok := a.requestOverrides["extra_body"]; ok {
		t.Fatalf("overrides %v", a.requestOverrides)
	}
}

// Agent on the given provider with the minimum state switch_model touches
func poolSwitchAgent(currentProvider, model string, pool *provider.Pool) *AIAgent {
	a := &AIAgent{provider: currentProvider, model: model, apiKey: currentProvider + "-key", apiMode: "chat_completions",
		clientKwargs:       map[string]any{"api_key": "***", "base_url": "https://" + currentProvider + ".example/v1"},
		cachedSystemPrompt: "cached-system-prompt", credentialPool: pool, primaryRuntime: map[string]any{}}
	a.setBaseURL("https://" + currentProvider + ".example/v1")
	return a
}

// Replaces the pool loader for the rest of the test, recording every provider it is asked for
func fakeSwitchPoolLoader(t *testing.T, load func(name string) *provider.Pool) *[]string {
	t.Helper()
	var calls []string
	prev := agentLoadPool
	agentLoadPool = func(_ provider.PoolEnv, name string) *provider.Pool {
		calls = append(calls, name)
		return load(name)
	}
	t.Cleanup(func() { agentLoadPool = prev })
	return &calls
}

func newTestPool(ctx context.Context, name string) *provider.Pool {
	return provider.NewPool(agentPoolEnv(ctx), name, nil)
}

func switchToGroq(t *testing.T, a *AIAgent) {
	t.Helper()
	if err := a.switchModel(context.Background(), "llama-3.3-70b", "groq", "groq-key-new", "https://api.groq.com/openai/v1", "chat_completions", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchModelReloadsCredentialPool(t *testing.T) {
	t.Run("test_switch_to_different_provider_reloads_pool", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		ctx := context.Background()
		oldPool, newPool := newTestPool(ctx, "opencode-go"), newTestPool(ctx, "groq")
		calls := fakeSwitchPoolLoader(t, func(string) *provider.Pool { return newPool })
		a := poolSwitchAgent("opencode-go", "qwen-coder", oldPool)
		switchToGroq(t, a)
		if a.credentialPool != newPool || a.credentialPool.Provider() != "groq" || !reflect.DeepEqual(*calls, []string{"groq"}) {
			t.Fatalf("pool %v calls %v", a.credentialPool, *calls)
		}
	})
	t.Run("test_switch_to_same_provider_does_not_reload_pool", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		ctx := context.Background()
		existing := newTestPool(ctx, "opencode-go")
		calls := fakeSwitchPoolLoader(t, func(string) *provider.Pool { return nil })
		a := poolSwitchAgent("opencode-go", "qwen-coder", existing)
		if err := a.switchModel(ctx, "qwen-coder", "opencode-go", "opencode-go-key-new", "https://opencode.example/v1", "chat_completions", nil); err != nil {
			t.Fatal(err)
		}
		if a.credentialPool != existing || len(*calls) != 0 {
			t.Fatalf("pool %v calls %v", a.credentialPool, *calls)
		}
	})
	t.Run("test_switch_creates_pool_when_agent_had_none", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		newPool := newTestPool(context.Background(), "groq")
		fakeSwitchPoolLoader(t, func(string) *provider.Pool { return newPool })
		a := poolSwitchAgent("opencode-go", "qwen-coder", nil)
		switchToGroq(t, a)
		if a.credentialPool != newPool || a.credentialPool.Provider() != "groq" {
			t.Fatalf("pool %v", a.credentialPool)
		}
	})
	t.Run("test_recover_pool_mismatch_guard_no_longer_trips_after_switch", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		ctx := context.Background()
		newPool := newTestPool(ctx, "groq")
		fakeSwitchPoolLoader(t, func(string) *provider.Pool { return newPool })
		a := poolSwitchAgent("opencode-go", "qwen-coder", newTestPool(ctx, "opencode-go"))
		switchToGroq(t, a)
		records := captureLogger(t, "run_agent", slog.LevelWarn)
		status := 429
		recovered, retried, err := a.recoverWithCredentialPool(ctx, &status, false, provider.ReasonRateLimit, nil, false)
		if err != nil || recovered || !retried {
			t.Fatalf("recovered %v retried %v err %v, the pool was never consulted", recovered, retried, err)
		}
		for _, r := range records() {
			if strings.Contains(r.msg, "Credential pool provider mismatch") {
				t.Fatalf("mismatch guard tripped %q", r.msg)
			}
		}
	})
	t.Run("test_pool_reload_failure_does_not_block_switch", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		fakeSwitchPoolLoader(t, func(string) *provider.Pool { panic(errors.New("simulated corrupt auth.json")) })
		a := poolSwitchAgent("opencode-go", "qwen-coder", newTestPool(context.Background(), "opencode-go"))
		switchToGroq(t, a)
		if a.provider != "groq" || a.model != "llama-3.3-70b" || a.credentialPool != nil {
			t.Fatalf("provider %q model %q pool %v", a.provider, a.model, a.credentialPool)
		}
	})
}

func TestSwitchModelReasoningOverride(t *testing.T) {
	t.Run("test_restore_primary_runtime_restores_reasoning", func(t *testing.T) {
		t.Setenv("ICEBATH_HOME", t.TempDir())
		cc := newStubEngine(0, 0.5)
		a := &AIAgent{fallbackActivated: true, model: "fallback-model", provider: "openai", contextCompressor: cc,
			reasoningConfig: map[string]any{"enabled": true, "effort": "medium"},
			primaryRuntime: map[string]any{"model": "claude-opus-4.5", "provider": "anthropic", "base_url": "https://api.anthropic.com",
				"api_mode": "anthropic_messages", "api_key": "key", "client_kwargs": map[string]any{}, "use_prompt_caching": true,
				"use_native_cache_layout": false, "reasoning_config": map[string]any{"enabled": true, "effort": "xhigh"},
				"compressor_model": "claude-opus-4.5", "compressor_base_url": "", "compressor_api_key": "", "compressor_provider": "",
				"compressor_context_length": 0, "compressor_api_mode": "", "compressor_threshold_tokens": 0,
				"anthropic_api_key": "key", "anthropic_base_url": "https://api.anthropic.com", "is_anthropic_oauth": false}}
		restored, err := a.restorePrimaryRuntime(context.Background())
		if err != nil || !restored {
			t.Fatalf("restored %v err %v", restored, err)
		}
		if !reflect.DeepEqual(a.reasoningConfig, map[string]any{"enabled": true, "effort": "xhigh"}) {
			t.Fatalf("reasoning %v", a.reasoningConfig)
		}
	})
}

func TestSwitchToMoaPinsChatCompletions(t *testing.T) {
	for _, incoming := range []string{"codex_responses", "anthropic_messages", "chat_completions", ""} {
		t.Run(incoming, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			fakeSwitchPoolLoader(t, func(string) *provider.Pool { return nil })
			a := &AIAgent{model: "minimax-m3", provider: "opencode-go", apiMode: "anthropic_messages", apiKey: "old-key", client: struct{}{},
				clientKwargs: map[string]any{"base_url": "https://old.example/v1"}, configContextLength: 123456, quietMode: true}
			a.setBaseURL("https://old.example/v1")
			_ = a.switchModel(context.Background(), "frontier", "moa", "moa-virtual-provider", "moa://local", incoming, nil)
			if a.provider != "moa" || a.baseURL != "moa://local" || a.apiMode != "chat_completions" {
				t.Fatalf("provider %q base %q api_mode %q", a.provider, a.baseURL, a.apiMode)
			}
			if _, ok := a.client.(*MoAClient); !ok {
				t.Fatalf("client %T", a.client)
			}
		})
	}
}
