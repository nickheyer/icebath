package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Offline model validation home with direct alias state reset and the given config
func switchValidateHome(t *testing.T, configYAML string) context.Context {
	t.Helper()
	validateTestHome(t)
	saved := providerModelIDsFn
	t.Cleanup(func() { providerModelIDsFn = saved })
	ctx, _ := switchAliasHome(t, configYAML)
	return ctx
}

func TestSwitchModelAllowsOpenAICodexModelMissingFromListing(t *testing.T) {
	ctx := switchValidateHome(t, "")
	fakeSwitchPipeline(t, false)
	providerModelIDsFn = catalogIDs("gpt-5.5", "gpt-5.4", "gpt-5.3-codex")
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "gpt-5.3-codex-spark", CurrentProvider: "openai-codex"})
	if !res.Success || res.NewModel != "gpt-5.3-codex-spark" || res.TargetProvider != "openai-codex" || res.WarningMessage == "" {
		t.Fatalf("result %+v", res)
	}
}

func TestKimiK26StaysOnOpencodeGo(t *testing.T) {
	ctx := switchValidateHome(t, "")
	fakeSwitchPipeline(t, false)
	live := []string{"minimax-m2.7", "minimax-m2.5", "kimi-k2.6", "kimi-k2.5", "glm-5.1", "glm-5", "deepseek-v4-pro",
		"deepseek-v4-flash", "qwen3.6-plus", "qwen3.5-plus", "mimo-v2-pro", "mimo-v2-omni", "mimo-v2.5-pro", "mimo-v2.5"}
	saved := listProviderModelsFn
	listProviderModelsFn = func(_ context.Context, provider string) []string {
		if provider == "opencode-go" {
			return append([]string{}, live...)
		}
		return nil
	}
	t.Cleanup(func() { listProviderModelsFn = saved })
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "kimi-k2.6", CurrentProvider: "opencode-go",
		CurrentBaseURL: "https://opencode.ai/zen/go/v1", CurrentAPIKey: "sk-test-opencode-go"})
	if res.TargetProvider != "opencode-go" || res.NewModel != "kimi-k2.6" {
		t.Fatalf("result %+v", res)
	}
}

// Explicit openai-codex switch with the given providers block and a codex runtime
func runSettingsOnlySwitch(t *testing.T, userProviders map[string]any) (ModelSwitchResult, *switchFakes) {
	t.Helper()
	ctx, _ := switchAliasHome(t, "")
	fakes := fakeSwitchPipeline(t, true)
	fakeSwitchRouting(t, []string{}, [2]string{})
	fakeSwitchRuntime(t, &Runtime{APIKey: "test-key", BaseURL: "https://chatgpt.com/backend-api/codex", APIMode: ModeCodexResponses}, nil)
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "gpt-5.6-luna", CurrentProvider: "openai-codex",
		ExplicitProvider: "openai-codex", UserProviders: userProviders, CustomProviders: []any{}})
	return res, fakes
}

func TestSettingsOnlyBlockKeepsBuiltinValidationRouting(t *testing.T) {
	res, fakes := runSettingsOnlySwitch(t, map[string]any{"openai-codex": map[string]any{"request_timeout_seconds": 3600, "stale_timeout_seconds": 300}})
	if !res.Success || len(fakes.validatedAs) != 1 || fakes.validatedAs[0] != "openai-codex" {
		t.Fatalf("result %+v validated as %v", res, fakes.validatedAs)
	}
}

func TestEndpointBlockStillRoutesCustomValidation(t *testing.T) {
	res, fakes := runSettingsOnlySwitch(t, map[string]any{"openai-codex": map[string]any{"base_url": "https://example.com/v1", "request_timeout_seconds": 3600}})
	if !res.Success || len(fakes.validatedAs) != 1 || fakes.validatedAs[0] != "custom:openai-codex" {
		t.Fatalf("result %+v validated as %v", res, fakes.validatedAs)
	}
}

func TestSettingsOnlyBlockCodexSwitchSucceedsWithoutListing(t *testing.T) {
	ctx := switchValidateHome(t, "")
	fakeSwitchPipeline(t, false)
	fakeSwitchRouting(t, []string{}, [2]string{})
	fakeSwitchRuntime(t, &Runtime{APIKey: "test-key", BaseURL: "https://chatgpt.com/backend-api/codex", APIMode: ModeCodexResponses}, nil)
	providerModelIDsFn = catalogIDs("gpt-5.5", "gpt-5.4", "gpt-5.3-codex")
	validateProbeAPIModelsFn = func(context.Context, string, string, time.Duration, string, map[string]string) ProbeResult {
		return ProbeResult{ProbedURL: "https://chatgpt.com/backend-api/codex/models"}
	}
	res := mustSwitch(t, ctx, SwitchModelOptions{RawInput: "gpt-5.6-luna", CurrentProvider: "openai-codex",
		ExplicitProvider: "openai-codex", UserProviders: map[string]any{"openai-codex": map[string]any{"request_timeout_seconds": 3600, "stale_timeout_seconds": 300}},
		CustomProviders: []any{}})
	if !res.Success || res.NewModel != "gpt-5.6-luna" || res.TargetProvider != "openai-codex" {
		t.Fatalf("result %+v", res)
	}
}

func TestModelPickWithARaisingHookGetsTheDeclaredWire(t *testing.T) {
	RegisterProvider(Profile{Name: "respdefault", BaseURL: "https://api.respdefault.test/v1", Mode: ModeCodexResponses})
	RegisterPerModelAPIMode("respdefault", func(model, _ string) APIMode {
		if strings.HasPrefix(model, "broken") {
			panic("live catalog unavailable")
		}
		if strings.HasPrefix(model, "claude") {
			return ModeChatCompletions
		}
		return ""
	})
	t.Cleanup(func() { RegisterPerModelAPIMode("respdefault", nil) })
	ctx := context.Background()
	if mode, ok := ModelDerivedAPIMode(ctx, "respdefault", "broken-z", "sk-fake"); !ok || mode != string(ModeCodexResponses) {
		t.Fatalf("raising hook gave %q %v", mode, ok)
	}
	if mode, ok := ModelDerivedAPIMode(ctx, "respdefault", "claude-y", "sk-fake"); !ok || mode != string(ModeChatCompletions) {
		t.Fatalf("hook exception gave %q %v", mode, ok)
	}
	if mode, ok := ModelDerivedAPIMode(ctx, "respdefault", "gpt-x", "sk-fake"); ok {
		t.Fatalf("hook none gave %q", mode)
	}
}

const persistSeed = "model:\n  default: local-model\n  provider: custom\n  base_url: http://localhost:1234/v1\n" +
	"  api_mode: anthropic_messages\n  api_key: sk-stale\n  context_length: 32000\n  model_slots:\n    fast: gpt-5-mini\n" +
	"agent:\n  system_prompt: keepme\n"

var persistGlobalResult = ModelSwitchResult{Success: true, NewModel: "claude-haiku", TargetProvider: "anthropic",
	ProviderChanged: true, APIKey: "sk-new", IsGlobal: true}

// Writes the seed config and returns a context bound to its home with the config path
func persistHome(t *testing.T, seed string) (context.Context, string) {
	t.Helper()
	ctx, h := switchAliasHome(t, seed)
	return ctx, home.ConfigPath(h)
}

func persistedModelBlock(t *testing.T, path string) map[string]any {
	t.Helper()
	doc, err := config.ReadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := doc["model"].(map[string]any)
	return block
}

func persistOrFail(t *testing.T, ctx context.Context, result ModelSwitchResult) {
	t.Helper()
	if err := PersistModelSelection(ctx, result, ""); err != nil {
		t.Fatal(err)
	}
}

func TestPersistWritesTheOneModelBlockShape(t *testing.T) {
	ctx, path := persistHome(t, persistSeed)
	persistOrFail(t, ctx, persistGlobalResult)
	block := persistedModelBlock(t, path)
	if block["default"] != "claude-haiku" || block["provider"] != "anthropic" {
		t.Fatalf("block %v", block)
	}
	for _, gone := range []string{"base_url", "api_mode", "context_length", "api_key"} {
		if _, held := block[gone]; held {
			t.Fatalf("%s survived in %v", gone, block)
		}
	}
	if slots, _ := block["model_slots"].(map[string]any); slots["fast"] != "gpt-5-mini" {
		t.Fatalf("model_slots %v", block["model_slots"])
	}
	doc, _ := config.ReadDocument(path)
	if agent, _ := doc["agent"].(map[string]any); agent["system_prompt"] != "keepme" {
		t.Fatalf("agent %v", doc["agent"])
	}
}

func TestSameRouteRepickKeepsTheContextPin(t *testing.T) {
	ctx, path := persistHome(t, persistSeed)
	persistOrFail(t, ctx, ModelSwitchResult{Success: true, NewModel: "local-model", TargetProvider: "custom",
		BaseURL: "http://localhost:1234/v1", APIMode: "anthropic_messages", IsGlobal: true})
	block := persistedModelBlock(t, path)
	if pyStr(block["context_length"]) != "32000" || block["api_key"] != "sk-stale" {
		t.Fatalf("block %v", block)
	}
}

func TestCustomToOtherCustomEndpointDropsTheInlineKey(t *testing.T) {
	ctx, path := persistHome(t, persistSeed)
	persistOrFail(t, ctx, ModelSwitchResult{Success: true, NewModel: "other-model", TargetProvider: "custom:other-box",
		BaseURL: "http://other-box:8000/v1", APIMode: "chat_completions", IsGlobal: true})
	if _, held := persistedModelBlock(t, path)["api_key"]; held {
		t.Fatal("inline key survived a custom endpoint change")
	}
	writeConfigYAML(t, home.Dir(ctx), persistSeed)
	persistOrFail(t, ctx, ModelSwitchResult{Success: true, NewModel: "local-model", TargetProvider: "custom",
		BaseURL: "http://10.0.0.9:1234/v1", APIMode: "anthropic_messages", IsGlobal: true})
	if _, held := persistedModelBlock(t, path)["api_key"]; held {
		t.Fatal("inline key survived a same provider host change")
	}
}

func TestProviderSwitchDropsTheKeyEnvPointerButASameRouteRepickKeepsIt(t *testing.T) {
	seed := strings.Replace(persistSeed, "  api_key: sk-stale\n", "  key_env: CUSTOM_BOX_API_KEY\n", 1)
	ctx, path := persistHome(t, seed)
	persistOrFail(t, ctx, persistGlobalResult)
	if _, held := persistedModelBlock(t, path)["key_env"]; held {
		t.Fatal("key_env survived a provider switch")
	}
	writeConfigYAML(t, home.Dir(ctx), seed)
	persistOrFail(t, ctx, ModelSwitchResult{Success: true, NewModel: "local-model", TargetProvider: "custom",
		BaseURL: "http://localhost:1234/v1", APIMode: "anthropic_messages", IsGlobal: true})
	if got := persistedModelBlock(t, path)["key_env"]; got != "CUSTOM_BOX_API_KEY" {
		t.Fatalf("key_env %v", got)
	}
	writeConfigYAML(t, home.Dir(ctx), strings.NewReplacer("provider: custom\n", "provider: lmstudio\n",
		"CUSTOM_BOX_API_KEY", "ICEBATH_CUSTOM_LMSTUDIO_API_KEY").Replace(seed))
	persistOrFail(t, ctx, ModelSwitchResult{Success: true, NewModel: "qwen3-8b", TargetProvider: "lmstudio",
		BaseURL: "http://localhost:1234/v1", APIMode: "openai_chat", IsGlobal: true})
	block := persistedModelBlock(t, path)
	if block["default"] != "qwen3-8b" || block["key_env"] != "ICEBATH_CUSTOM_LMSTUDIO_API_KEY" {
		t.Fatalf("block %v", block)
	}
}

func TestApplyModelSelectionMasksClearedKeys(t *testing.T) {
	held := map[string]any{"default": "m", "provider": "custom", "base_url": "http://a/v1", "api_key": "sk", "key_env": "K"}
	removed := ApplyModelSelection(held, persistGlobalResult, false)
	masked := ApplyModelSelection(held, persistGlobalResult, true)
	for _, key := range []string{"api_key", "key_env", "base_url"} {
		if _, ok := removed[key]; ok {
			t.Fatalf("%s kept without masking %v", key, removed)
		}
		if v, ok := masked[key]; !ok || v != nil {
			t.Fatalf("%s not masked %v", key, masked)
		}
	}
	if _, ok := masked["api_mode"]; ok {
		t.Fatalf("absent key masked %v", masked)
	}
	if err := RemoteModelSelectionGuard(context.Background(), held, removed); err != nil {
		t.Fatalf("file backend guard refused %v", err)
	}
}
