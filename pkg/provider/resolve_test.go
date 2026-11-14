package provider

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// fakeCreds is a login store for tests that answers from fixed maps
type fakeCreds struct {
	creds   map[string]Credential
	byModel map[string]map[string]Credential
	benched map[string]bool
	active  string
	free    bool
	errs    map[string]*AuthError
	scoped  func(poolKey string, entry map[string]any) (Credential, bool)
	calls   []string
}

func (f *fakeCreds) Credential(provider string) (Credential, bool) {
	f.calls = append(f.calls, provider)
	if f.creds == nil {
		return Credential{}, false
	}
	c, ok := f.creds[provider]
	return c, ok
}

func (f *fakeCreds) CredentialForModel(provider, model string) (Credential, bool) {
	if f.byModel != nil {
		if m, ok := f.byModel[provider]; ok {
			c, ok := m[model]
			return c, ok
		}
	}
	return f.Credential(provider)
}

func (f *fakeCreds) TokenBenchedForModel(provider, token, model string) bool {
	return f.benched[token+"|"+model]
}

func (f *fakeCreds) ActiveLoggedInProvider(bool) string { return f.active }

func (f *fakeCreds) FreeInferenceActive() bool { return f.free }

func (f *fakeCreds) RuntimeCredential(provider string) (Credential, error) {
	if cred, ok := f.Credential(provider); ok && (cred.APIKey != "" || cred.TokenSource != nil) {
		return cred, nil
	}
	if err := f.errs[provider]; err != nil {
		return Credential{}, err
	}
	return Credential{}, oauthMissingLoginError(provider)
}

func (f *fakeCreds) CredentialForEntry(poolKey string, entry map[string]any) (Credential, bool) {
	if f.scoped != nil {
		return f.scoped(poolKey, entry)
	}
	return f.Credential(poolKey)
}

// Blanks every provider key env var and the routing env vars so the host machine never leaks in
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, p := range Profiles() {
		for _, v := range p.KeyEnv {
			t.Setenv(v, "")
		}
		if p.BaseURLEnv != "" {
			t.Setenv(p.BaseURLEnv, "")
		}
	}
	for _, v := range []string{"OPENAI_API_KEY", "OPENROUTER_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"OPENAI_BASE_URL", "OPENROUTER_BASE_URL", "CUSTOM_BASE_URL", "ICEBATH_INFERENCE_PROVIDER", "ICEBATH_CODEX_BASE_URL",
		"ICEBATH_QWEN_BASE_URL", "AZURE_FOUNDRY_API_KEY", "AZURE_FOUNDRY_BASE_URL", "AZURE_ANTHROPIC_KEY", "NOUS_INFERENCE_BASE_URL",
		"AWS_BEARER_TOKEN_BEDROCK", "DEEPSEEK_API_KEY", "OLLAMA_API_KEY", "LM_API_KEY", "LM_BASE_URL", "XAI_API_KEY", "XAI_BASE_URL"} {
		t.Setenv(v, "")
	}
}

// Resolver over a temporary home whose config and .env are given
func testResolver(t *testing.T, cfg map[string]any, creds CredentialSource, dotenv string) *resolver {
	t.Helper()
	clearProviderEnv(t)
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	if cfg == nil {
		cfg = map[string]any{}
	}
	return newResolver(context.Background(), config.Tree(cfg), config.NewSecrets(h), h, creds)
}

func mustResolve(t *testing.T, r *resolver, requested, apiKey, baseURL, model string) *Runtime {
	t.Helper()
	rt, err := r.ResolveRuntime(requested, apiKey, baseURL, nil, model)
	if err != nil {
		t.Fatalf("resolve %q: %v", requested, err)
	}
	return rt
}

func authErrOf(t *testing.T, err error) *AuthError {
	t.Helper()
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected AuthError, got %T %v", err, err)
	}
	return authErr
}

// ---- hasUsableSecret ----

func TestHasUsableSecret(t *testing.T) {
	for _, bad := range []string{"", "   ", "123", "sk-xxxxxxxx", "your_api_key_here", "changeme", "x-x_x x", "ghp_xxx"} {
		if hasUsableSecret(bad) {
			t.Errorf("%q should not be usable", bad)
		}
	}
	for _, good := range []string{"sk-genuinely-long-real-key-12345", "test-key", "or-key", "sk-or-v1-legacy"} {
		if !hasUsableSecret(good) {
			t.Errorf("%q should be usable", good)
		}
	}
	if !looksLikeOpenRouterKey(" sk-or-v1-abc") || looksLikeOpenRouterKey("sk-proj-abc") {
		t.Fatal("openrouter key shape")
	}
}

// ---- api key providers ----

func TestConfiguredAPIKeyProviderWithoutKeyFailsClosed(t *testing.T) {
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "deepseek", "default": "deepseek-v4-pro"}}, &fakeCreds{}, "")
	_, err := r.ResolveRuntime("", "", "", nil, "")
	authErr := authErrOf(t, err)
	if !strings.Contains(authErr.Message, "No usable credentials") || !strings.Contains(authErr.Message, "deepseek") || authErr.Code != "missing_api_key" {
		t.Fatalf("got %q %q", authErr.Message, authErr.Code)
	}
	if !strings.Contains(authErr.Message, "Set DEEPSEEK_API_KEY.") {
		t.Fatalf("env hint: %q", authErr.Message)
	}
}

func TestNoAuthLMStudioStillResolves(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "")
	rt := mustResolve(t, r, "lmstudio", "", "", "")
	if rt.Provider != "lmstudio" || rt.APIKey != LMStudioNoAuthPlaceholder || rt.APIMode != ModeChatCompletions {
		t.Fatalf("got %+v", rt)
	}
}

func TestLMStudioUsesTokenAndSavedBaseURL(t *testing.T) {
	t.Run("token", func(t *testing.T) {
		r := testResolver(t, map[string]any{"model": map[string]any{"provider": "lmstudio", "base_url": "http://127.0.0.1:1234/v1", "default": "publisher/model-a"}}, &fakeCreds{}, "LM_API_KEY=lm-token\n")
		rt := mustResolve(t, r, "lmstudio", "", "", "")
		if rt.APIKey != "lm-token" || rt.BaseURL != "http://127.0.0.1:1234/v1" || rt.APIMode != ModeChatCompletions {
			t.Fatalf("got %+v", rt)
		}
	})
	t.Run("saved base url wins over env", func(t *testing.T) {
		r := testResolver(t, map[string]any{"model": map[string]any{"provider": "lmstudio", "base_url": "http://192.168.1.10:1234/v1", "default": "qwen/qwen3-coder-30b"}}, &fakeCreds{}, "")
		t.Setenv("LM_BASE_URL", "http://override.local:9999/v1")
		rt := mustResolve(t, r, "lmstudio", "", "", "")
		if rt.BaseURL != "http://192.168.1.10:1234/v1" || rt.APIKey != LMStudioNoAuthPlaceholder {
			t.Fatalf("got %+v", rt)
		}
	})
}

func TestAIGatewayFromEnvAndExplicitOverrideSkipsPool(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "AI_GATEWAY_API_KEY=test-ai-gw-key\n")
	rt := mustResolve(t, r, "ai-gateway", "", "", "")
	if rt.Provider != "ai-gateway" || rt.APIMode != ModeChatCompletions || rt.BaseURL != "https://ai-gateway.vercel.sh/v1" || rt.APIKey != "test-ai-gw-key" || rt.RequestedProvider != "ai-gateway" {
		t.Fatalf("got %+v", rt)
	}
	creds := &fakeCreds{creds: map[string]Credential{"ai-gateway": {APIKey: "pooled-should-not-be-used", Source: "manual"}}}
	r = testResolver(t, nil, creds, "")
	rt = mustResolve(t, r, "ai-gateway", "ai-gateway-explicit-token", "https://proxy.example.com/v1/", "")
	if rt.APIKey != "ai-gateway-explicit-token" || rt.BaseURL != "https://proxy.example.com/v1" || rt.Source != "explicit" || rt.APIMode != ModeChatCompletions {
		t.Fatalf("got %+v", rt)
	}
	if len(creds.calls) != 0 {
		t.Fatalf("pool consulted: %v", creds.calls)
	}
}

func TestGeminiExplicitAPIModeProviderGuard(t *testing.T) {
	cases := []struct {
		name     string
		modelCfg map[string]any
		want     APIMode
	}{
		{"stale anthropic api mode ignored on switch", map[string]any{"provider": "anthropic", "api_mode": "anthropic_messages", "default": "claude-3-5-sonnet"}, ModeChatCompletions},
		{"matching provider honours persisted", map[string]any{"provider": "gemini", "api_mode": "anthropic_messages", "default": "gemini-2.5-pro"}, ModeAnthropicMessages},
		{"no persisted provider honours", map[string]any{"api_mode": "anthropic_messages", "default": "gemini-2.5-pro"}, ModeAnthropicMessages},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := testResolver(t, map[string]any{"model": c.modelCfg}, &fakeCreds{}, "")
			rt := mustResolve(t, r, "gemini", "fake-gemini-key", "https://fake-gemini-endpoint.example.com/v1beta/", "")
			if rt.Provider != "gemini" || rt.APIMode != c.want || rt.APIKey != "fake-gemini-key" || rt.BaseURL != "https://fake-gemini-endpoint.example.com/v1beta" || rt.Source != "explicit" {
				t.Fatalf("got %+v", rt)
			}
		})
	}
}

func TestMinimaxConfigBaseURLOverridesDefault(t *testing.T) {
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "minimax", "base_url": "https://api.minimaxi.com/anthropic"}}, &fakeCreds{}, "MINIMAX_API_KEY=test-minimax-key\n")
	rt := mustResolve(t, r, "minimax", "", "", "")
	if rt.Provider != "minimax" || rt.BaseURL != "https://api.minimaxi.com/anthropic" || rt.APIMode != ModeAnthropicMessages {
		t.Fatalf("got %+v", rt)
	}
}

func TestOpenCodeGoModelDerivationAndHealing(t *testing.T) {
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "opencode-go", "default": "minimax-m2.5", "api_mode": "chat_completions"}}, &fakeCreds{}, "OPENCODE_GO_API_KEY=test-opencode-go-key\n")
	rt := mustResolve(t, r, "opencode-go", "", "", "")
	if rt.Provider != "opencode-go" || rt.APIMode != ModeAnthropicMessages {
		t.Fatalf("got %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "opencode-go", "default": "mimo-v2.5", "base_url": "https://opencode.ai/zen/v1"}}, &fakeCreds{}, "OPENCODE_GO_API_KEY=test-opencode-go-key\n")
	rt = mustResolve(t, r, "opencode-go", "", "", "")
	if rt.APIMode != ModeChatCompletions || rt.BaseURL != "https://opencode.ai/zen/go/v1" {
		t.Fatalf("heal: %+v", rt)
	}
	for _, model := range []string{"qwen3.8-flash", "glm-5.3-flash"} {
		r = testResolver(t, map[string]any{"model": map[string]any{"provider": "opencode-go", "default": "glm-5.3-flash"}}, &fakeCreds{}, "")
		explicit := mustResolve(t, r, "opencode-go", "test-opencode-go-key", "", model)
		r2 := testResolver(t, map[string]any{"model": map[string]any{"provider": "opencode-go", "default": "glm-5.3-flash"}}, &fakeCreds{}, "OPENCODE_GO_API_KEY=test-opencode-go-key\n")
		env := mustResolve(t, r2, "opencode-go", "", "", model)
		if explicit.Source != "explicit" || explicit.APIMode != env.APIMode || explicit.BaseURL != env.BaseURL || explicit.APIMode != OpenCodeModelMode("opencode-go", model) {
			t.Fatalf("%s: explicit %+v env %+v", model, explicit, env)
		}
	}
}

// ---- credential pool entries ----

func TestResolveRuntimeUsesCredentialPool(t *testing.T) {
	creds := &fakeCreds{creds: map[string]Credential{"openai-codex": {APIKey: "pool-token", Source: "manual", BaseURL: "https://chatgpt.com/backend-api/codex"}}}
	r := testResolver(t, nil, creds, "")
	rt := mustResolve(t, r, "openai-codex", "", "", "")
	if rt.Provider != "openai-codex" || rt.APIKey != "pool-token" || rt.Source != "manual" || rt.APIMode != ModeCodexResponses {
		t.Fatalf("got %+v", rt)
	}
	t.Setenv("ICEBATH_CODEX_BASE_URL", "http://127.0.0.1:8787/v1")
	rt = mustResolve(t, r, "openai-codex", "", "", "")
	if rt.BaseURL != "http://127.0.0.1:8787/v1" {
		t.Fatalf("codex override: %+v", rt)
	}
	t.Setenv("ICEBATH_CODEX_BASE_URL", "")
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "openai-codex", "default": "gpt-5.3-codex", "base_url": "http://127.0.0.1:8400/backend-api/codex/"}}, creds, "")
	rt = mustResolve(t, r, "openai-codex", "", "", "")
	if rt.BaseURL != "http://127.0.0.1:8400/backend-api/codex" || rt.APIMode != ModeCodexResponses {
		t.Fatalf("model base url: %+v", rt)
	}
}

func TestXaiPoolBaseURLRules(t *testing.T) {
	pool := func(url string) *fakeCreds {
		return &fakeCreds{creds: map[string]Credential{"xai": {APIKey: "pool-token", Source: "env:XAI_API_KEY", BaseURL: url}}}
	}
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "xai", "default": "grok-4", "base_url": "http://127.0.0.1:8765/v1/"}}, pool("https://api.x.ai/v1"), "")
	rt := mustResolve(t, r, "xai", "", "", "")
	if rt.APIKey != "pool-token" || rt.BaseURL != "http://127.0.0.1:8765/v1" || rt.APIMode != ModeCodexResponses {
		t.Fatalf("registry host row: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "xai", "default": "grok-4", "base_url": "http://127.0.0.1:8765/v1"}}, pool("https://relay.example/v1"), "")
	rt = mustResolve(t, r, "xai", "", "", "")
	if rt.BaseURL != "https://relay.example/v1" {
		t.Fatalf("explicit credential endpoint: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "deepseek", "default": "deepseek-v4-pro", "base_url": "http://127.0.0.1:8765/v1"}}, pool("https://api.x.ai/v1"), "")
	rt = mustResolve(t, r, "xai", "", "", "")
	if rt.BaseURL != "https://api.x.ai/v1" {
		t.Fatalf("another provider's base url: %+v", rt)
	}
}

func TestCustomPoolLoopbackNoKeyExemption(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{"local-ollama": map[string]any{"api": "http://localhost:11434/v1"}, "remote": map[string]any{"api": "https://api.remote-vendor.example/v1"}}}
	pool := func(key string) *fakeCreds {
		return &fakeCreds{creds: map[string]Credential{"local-ollama": {APIKey: key}, "remote": {APIKey: key}}}
	}
	r := testResolver(t, cfg, pool("123"), "")
	if rt := r.tryResolveFromCustomPool("http://localhost:11434/v1", "custom", "", ""); rt == nil || rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("short key: %+v", rt)
	}
	r = testResolver(t, cfg, pool("m"), "")
	if rt := r.tryResolveFromCustomPool("http://localhost:11434/v1", "custom", "", ""); rt == nil || rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("single char: %+v", rt)
	}
	r = testResolver(t, cfg, pool("xy"), "")
	if rt := r.tryResolveFromCustomPool("https://api.remote-vendor.example/v1", "custom", "", ""); rt == nil || rt.APIKey != "xy" {
		t.Fatalf("remote short key passes through: %+v", rt)
	}
	r = testResolver(t, cfg, pool("sk-genuinely-long-real-key-12345"), "")
	if rt := r.tryResolveFromCustomPool("http://localhost:11434/v1", "custom", "", ""); rt == nil || rt.APIKey != "sk-genuinely-long-real-key-12345" || rt.Source != "pool:local-ollama" {
		t.Fatalf("usable key: %+v", rt)
	}
}

// ---- oauth providers ----

func TestOAuthAutoFallthroughOnAuthFailure(t *testing.T) {
	creds := &fakeCreds{active: "qwen-oauth", errs: map[string]*AuthError{"qwen-oauth": qwenErr("stale", "qwen_auth_missing", true)}}
	r := testResolver(t, nil, creds, "")
	rt := mustResolve(t, r, "auto", "", "", "")
	if rt.Provider == "qwen-oauth" || rt.AuthError == nil || rt.AuthError.Code != "qwen_auth_missing" {
		t.Fatalf("got %+v", rt)
	}
	r = testResolver(t, nil, creds, "")
	if _, err := r.ResolveRuntime("qwen-oauth", "", "", nil, ""); err == nil || authErrOf(t, err).Code != "qwen_auth_missing" {
		t.Fatalf("explicit request raises: %v", err)
	}
}

func TestAutoDetectedNousAuthFailureFallsThroughToOpenRouter(t *testing.T) {
	creds := &fakeCreds{active: "nous", errs: map[string]*AuthError{"nous": {Message: "Refresh session has been revoked", Provider: "nous", Code: "invalid_grant", ReloginRequired: true}}}
	r := testResolver(t, nil, creds, "OPENROUTER_API_KEY=test-or-key\n")
	rt := mustResolve(t, r, "auto", "", "", "")
	if rt.Provider != "openrouter" || rt.APIKey != "test-or-key" {
		t.Fatalf("got %+v", rt)
	}
}

func TestQwenOAuthSuppliedTokenKeepsBaseURLOverride(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "")
	t.Setenv("ICEBATH_QWEN_BASE_URL", "https://qwen-proxy.example.test/v1/")
	rt := mustResolve(t, r, "qwen-oauth", "fake-qwen-token", "", "")
	if rt.APIKey != "fake-qwen-token" || rt.BaseURL != "https://qwen-proxy.example.test/v1" {
		t.Fatalf("got %+v", rt)
	}
}

func TestMinimaxOAuthRuntime(t *testing.T) {
	creds := &fakeCreds{creds: map[string]Credential{"minimax-oauth": {APIKey: "mock-access-token", BaseURL: MiniMaxOAuthBaseURL, Source: "oauth"}}}
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "minimax-oauth"}}, creds, "")
	rt := mustResolve(t, r, "minimax-oauth", "", "", "")
	if rt.Provider != "minimax-oauth" || rt.APIMode != ModeAnthropicMessages || rt.APIKey != "mock-access-token" {
		t.Fatalf("got %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "minimax-oauth", "default": "MiniMax-M2.7", "api_mode": "chat_completions"}},
		&fakeCreds{creds: map[string]Credential{"minimax-oauth": {APIKey: "oauth-token", Source: "manual:minimax_oauth", BaseURL: "https://api.minimax.io/anthropic"}}}, "")
	rt = mustResolve(t, r, "minimax-oauth", "", "", "")
	if rt.APIMode != ModeAnthropicMessages || rt.BaseURL != "https://api.minimax.io/anthropic" {
		t.Fatalf("stale config: %+v", rt)
	}
}

func TestCodexAppServerOptInOnEveryRung(t *testing.T) {
	modelCfg := map[string]any{"provider": "openai-codex", "default": "gpt-5.5", "openai_runtime": "codex_app_server"}
	pool := &fakeCreds{creds: map[string]Credential{"openai-codex": {APIKey: "tok", Source: "pool"}}}
	for _, rung := range []string{"pool", "explicit"} {
		creds := pool
		apiKey := ""
		if rung == "explicit" {
			creds = &fakeCreds{}
			apiKey = "sk-explicit"
		}
		r := testResolver(t, map[string]any{"model": modelCfg}, creds, "")
		rt := mustResolve(t, r, "openai-codex", apiKey, "", "")
		if rt.Provider != "openai-codex" || rt.APIMode != modeCodexAppServer {
			t.Fatalf("%s: got %+v", rung, rt)
		}
		for _, openaiRuntime := range []any{nil, "auto"} {
			cfg := map[string]any{"provider": "openai-codex", "default": "gpt-5.5"}
			if openaiRuntime != nil {
				cfg["openai_runtime"] = openaiRuntime
			}
			r := testResolver(t, map[string]any{"model": cfg}, creds, "")
			if rt := mustResolve(t, r, "openai-codex", apiKey, "", ""); rt.APIMode != ModeCodexResponses {
				t.Fatalf("%s unset keeps wire: %+v", rung, rt)
			}
		}
	}
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "openai", "default": "gpt-5.5-codex", "openai_runtime": "codex_app_server"}}, &fakeCreds{}, "")
	rt := mustResolve(t, r, "openai", "sk-explicit", "", "")
	if rt.Provider != "custom" || rt.APIMode != modeCodexAppServer {
		t.Fatalf("openai alias: %+v", rt)
	}
}

func TestOpenAIAliasPairsKeyWithOpenAIBaseURL(t *testing.T) {
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "custom:mylocal", "default": "local-main"}}, &fakeCreds{}, "OPENAI_BASE_URL=https://llm-proxy.corp.example/v1\nOPENAI_API_KEY=sk-proxy-issued\n")
	rt := mustResolve(t, r, "openai", "", "", "gpt-x")
	if rt.Provider != "custom" || rt.BaseURL != "https://llm-proxy.corp.example/v1" || rt.APIKey != "sk-proxy-issued" {
		t.Fatalf("got %+v", rt)
	}
	block := map[string]any{"provider": "openai", "model": "review-model", "base_url": "https://gateway.example/v1", "api_key": "gw-key"}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "custom:mylocal", "default": "local-main"}, "auxiliary": map[string]any{"background_review": block}}, &fakeCreds{}, "")
	ar := r.resolveTaskProviderModel("background_review", "", "", "", "")
	if ar.provider != "custom" || ar.baseURL != "https://gateway.example/v1" || ar.apiKey != "gw-key" {
		t.Fatalf("aux: %+v", ar)
	}
	rt = mustResolve(t, r, "openai", "gw-key", "https://gateway.example/v1", "review-model")
	if rt.Provider != ar.provider || rt.BaseURL != ar.baseURL || rt.APIKey != ar.apiKey {
		t.Fatalf("runtime %+v differs from aux %+v", rt, ar)
	}
}

// ---- openrouter and bare custom fallback ----

func TestOpenRouterExplicitAndPoolRules(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "")
	rt := mustResolve(t, r, "openrouter", "test-key", "https://example.com/v1/", "")
	if rt.Provider != "openrouter" || rt.APIMode != ModeChatCompletions || rt.APIKey != "test-key" || rt.BaseURL != "https://example.com/v1" || rt.Source != "explicit" {
		t.Fatalf("explicit: %+v", rt)
	}
	pool := &fakeCreds{creds: map[string]Credential{"openrouter": {APIKey: "pool-key", Source: "manual", BaseURL: OpenRouterBaseURL}}}
	r = testResolver(t, nil, pool, "")
	rt = mustResolve(t, r, "auto", "", "", "")
	if rt.Provider != "openrouter" || rt.APIKey != "pool-key" || rt.BaseURL != OpenRouterBaseURL || rt.Source != "manual" {
		t.Fatalf("auto uses pool: %+v", rt)
	}
	r = testResolver(t, nil, pool, "")
	rt = mustResolve(t, r, "openrouter", "explicit-key", "", "")
	if rt.APIKey != "explicit-key" || rt.BaseURL != OpenRouterBaseURL || rt.Source != "explicit" {
		t.Fatalf("explicit key skips pool: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "openai-codex", "base_url": "https://chatgpt.com/backend-api/codex"}}, &fakeCreds{}, "")
	if rt := mustResolve(t, r, "openrouter", "", "", ""); rt.BaseURL != OpenRouterBaseURL {
		t.Fatalf("codex config base url ignored: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "auto", "base_url": "https://custom.example/v1/"}}, &fakeCreds{}, "")
	if rt := mustResolve(t, r, "auto", "", "", ""); rt.Provider != "openrouter" || rt.BaseURL != "https://custom.example/v1" {
		t.Fatalf("auto custom config url: %+v", rt)
	}
}

func TestOpenRouterKeySelection(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "OPENAI_API_KEY=sk-openai-should-lose\nOPENROUTER_API_KEY=sk-or-should-win\n")
	if rt := mustResolve(t, r, "openrouter", "", "", ""); rt.APIKey != "sk-or-should-win" {
		t.Fatalf("openrouter key priority: %+v", rt)
	}
	r = testResolver(t, nil, &fakeCreds{}, "OPENAI_API_KEY=sk-or-v1-legacy-fallback\n")
	if rt := mustResolve(t, r, "openrouter", "", "", ""); rt.APIKey != "sk-or-v1-legacy-fallback" {
		t.Fatalf("legacy sk-or key: %+v", rt)
	}
	r = testResolver(t, nil, &fakeCreds{}, "OPENAI_API_KEY=sk-proj-real-openai-key\n")
	if p, err := r.resolveProvider("auto", "", "", false); err != nil || p != "openai-api" {
		t.Fatalf("real openai key auto detects openai-api: %q %v", p, err)
	}
	rt := r.resolveOpenRouterRuntime("openrouter", "", "")
	if rt.BaseURL != OpenRouterBaseURL || rt.APIKey != "" {
		t.Fatalf("real openai key never sent to openrouter: %+v", rt)
	}
	cases := []struct{ base, want string }{
		{"https://proxy.corp.example/v1", ""},
		{"proxy.corp.example:8080/v1", ""},
		{"https://openrouter.ai/api/v1", "sk-openai-fallback"},
		{"http://openrouter.ai/api/v1", ""},
		{"https://openrouter.ai:8443/api/v1", ""},
		{"https://openrouter.ai:443/api/v1", "sk-openai-fallback"},
	}
	for _, c := range cases {
		r := testResolver(t, nil, &fakeCreds{}, "OPENAI_API_KEY=sk-openai-fallback\n")
		t.Setenv("OPENAI_BASE_URL", c.base)
		rt := r.resolveOpenRouterRuntime("openrouter", "", "")
		if rt.BaseURL != OpenRouterBaseURL || rt.APIKey != c.want {
			t.Errorf("%s: got %+v want key %q", c.base, rt, c.want)
		}
	}
}

func TestOpenRouterDotenvKeyRules(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "OPENROUTER_API_KEY=dotenv-or-key\n")
	if rt := mustResolve(t, r, "openrouter", "", "", ""); rt.APIKey != "dotenv-or-key" {
		t.Fatalf("dotenv key survives exhausted pool: %+v", rt)
	}
	r = testResolver(t, nil, &fakeCreds{}, "OPENROUTER_API_KEY=op://Vault/Item/field\n")
	t.Setenv("OPENROUTER_API_KEY", "resolved-or-key")
	if rt := mustResolve(t, r, "openrouter", "", "", ""); rt.APIKey != "resolved-or-key" {
		t.Fatalf("op ref loses to resolved env: %+v", rt)
	}
	r = testResolver(t, nil, &fakeCreds{}, "OPENAI_API_KEY=sk-or-dotenv-openai-key\n")
	if rt := mustResolve(t, r, "openrouter", "", "", ""); rt.APIKey != "sk-or-dotenv-openai-key" {
		t.Fatalf("openai dotenv fallback: %+v", rt)
	}
}

func TestCustomEndpointKeyLeakRules(t *testing.T) {
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "custom", "base_url": "http://127.0.0.1:1234/v1"}}, &fakeCreds{}, "OPENAI_API_KEY=local-key\nOPENROUTER_API_KEY=or-key\n")
	rt := mustResolve(t, r, "custom", "", "", "")
	if rt.BaseURL != "http://127.0.0.1:1234/v1" || rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("saved config url, no leak: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "custom", "base_url": "https://my-vllm-server.example.com/v1", "api_key": "sk-vllm-key"}}, &fakeCreds{}, "OPENROUTER_API_KEY=sk-or-...leak\n")
	rt = mustResolve(t, r, "custom", "", "", "")
	if rt.BaseURL != "https://my-vllm-server.example.com/v1" || rt.APIKey != "sk-vllm-key" {
		t.Fatalf("config key: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "openrouter", "base_url": "http://127.0.0.1:8082/v1", "default": "my-local-model"}}, &fakeCreds{}, "OPENROUTER_API_KEY=or-key\nOPENAI_API_KEY=openai-key\n")
	rt = mustResolve(t, r, "custom", "", "", "")
	if rt.Provider != "custom" || rt.BaseURL != "http://127.0.0.1:8082/v1" || rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("loopback under another provider: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "custom", "base_url": "https://api.deepseek.com/v1"}}, &fakeCreds{}, "OPENAI_API_KEY=sk-openai-secret\nOPENROUTER_API_KEY=or-secret\n")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.BaseURL != "https://api.deepseek.com/v1" || rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("openai key only to openai host: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "custom", "base_url": "https://api.openai.com/v1"}}, &fakeCreds{}, "OPENAI_API_KEY=sk-openai-secret\n")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.APIKey != "sk-openai-secret" {
		t.Fatalf("openai key reaches openai host: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "custom", "base_url": "https://api.deepseek.com/v1"}}, &fakeCreds{}, "DEEPSEEK_API_KEY=sk-deepseek-secret\n")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.APIKey != "sk-deepseek-secret" {
		t.Fatalf("host derived key: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "custom", "base_url": "https://api.deepseek.com.attacker.test/v1"}}, &fakeCreds{}, "DEEPSEEK_API_KEY=sk-deepseek-secret\n")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("lookalike host: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "custom", "base_url": "https://api.example.com/v1"}}, &fakeCreds{}, "OPENAI_API_KEY=sk-openai-secret\nOPENROUTER_API_KEY=or-secret\n")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("already handled vendors skipped: %+v", rt)
	}
}

func TestOllamaURLSubstringLeak(t *testing.T) {
	mk := func(baseURL string) map[string]any {
		return map[string]any{"model": map[string]any{"base_url": baseURL, "api_key": "", "provider": "custom"}}
	}
	r := testResolver(t, mk("http://127.0.0.1:9000/ollama.com/v1"), &fakeCreds{}, "OPENAI_API_KEY=oa-secret\nOPENROUTER_API_KEY=or-secret\nOLLAMA_API_KEY=ol-SECRET-should-not-leak\n")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("path injection: %+v", rt)
	}
	r = testResolver(t, mk("http://ollama.com.attacker.test:9000/v1"), &fakeCreds{}, "OPENAI_API_KEY=oa-secret\nOLLAMA_API_KEY=ol-SECRET-should-not-leak\n")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("lookalike host: %+v", rt)
	}
	r = testResolver(t, mk("https://ollama.com/v1"), &fakeCreds{}, "OPENAI_API_KEY=oa-secret\nOLLAMA_API_KEY=ol-legit-key\n")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.APIKey != "ol-legit-key" {
		t.Fatalf("genuine ollama: %+v", rt)
	}
}

func TestExplicitOpenRouterMirrorRules(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "OPENAI_BASE_URL=https://my-custom-llm.example.com/v1\nOPENROUTER_API_KEY=or-test-key\n")
	rt := mustResolve(t, r, "openrouter", "", "", "")
	if !strings.Contains(rt.BaseURL, "openrouter.ai") || strings.Contains(rt.BaseURL, "my-custom-llm") || rt.APIKey != "or-test-key" {
		t.Fatalf("openai base url skipped: %+v", rt)
	}
	mirrorCfg := map[string]any{"model": map[string]any{"provider": "openrouter", "base_url": "https://openrouter-mirror.example.com/api/v1"}}
	r = testResolver(t, mirrorCfg, &fakeCreds{}, "OPENROUTER_API_KEY=router-key\n")
	rt = mustResolve(t, r, "openrouter", "", "", "")
	if rt.BaseURL != "https://openrouter-mirror.example.com/api/v1" || rt.APIKey != "router-key" {
		t.Fatalf("config mirror: %+v", rt)
	}
	pool := &fakeCreds{creds: map[string]Credential{"openrouter": {APIKey: "pool-key", Source: "manual", BaseURL: OpenRouterBaseURL}}}
	r = testResolver(t, mirrorCfg, pool, "OPENROUTER_API_KEY=router-key\n")
	rt = mustResolve(t, r, "openrouter", "", "", "")
	if rt.BaseURL != "https://openrouter-mirror.example.com/api/v1" || rt.APIKey != "router-key" {
		t.Fatalf("mirror bypasses pool: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "openrouter", "base_url": OpenRouterBaseURL}}, pool, "OPENROUTER_API_KEY=router-key\n")
	if rt := mustResolve(t, r, "openrouter", "", "", ""); rt.APIKey != "pool-key" {
		t.Fatalf("canonical url keeps the pool: %+v", rt)
	}
	r = testResolver(t, mirrorCfg, &fakeCreds{}, "OPENROUTER_API_KEY=router-key\n")
	t.Setenv("CUSTOM_BASE_URL", "http://localhost:11434/v1")
	rt = mustResolve(t, r, "openrouter", "", "", "")
	if rt.BaseURL != "http://localhost:11434/v1" || rt.APIKey == "router-key" {
		t.Fatalf("custom base url outranks mirror: %+v", rt)
	}
}

func TestBareCustomExplicitLaunchKeyWinsOverPool(t *testing.T) {
	cfg := map[string]any{"model": map[string]any{"provider": "custom"}, "providers": map[string]any{"nine": map[string]any{"api": "http://127.0.0.1:9/v1"}}}
	r := testResolver(t, cfg, &fakeCreds{creds: map[string]Credential{"nine": {APIKey: "sk-pooled", Source: "pool"}}}, "")
	rt := r.resolveOpenRouterRuntime("custom", "sk-launch-explicit", "http://127.0.0.1:9/v1")
	if rt.APIKey != "sk-launch-explicit" {
		t.Fatalf("got %+v", rt)
	}
}

// ---- named custom providers ----

func TestCodexAppServerOptInRoutesOnlyNamedCustomProviders(t *testing.T) {
	cfg := map[string]any{
		"model":     map[string]any{"provider": "custom:my-gateway", "default": "gpt-5.4", "openai_runtime": "codex_app_server"},
		"providers": map[string]any{"my-gateway": map[string]any{"api": "https://gateway.example.com/v1", "api_key": "test-key", "default_model": "gpt-5.4"}},
	}
	r := testResolver(t, cfg, &fakeCreds{}, "")
	rt := mustResolve(t, r, "custom:my-gateway", "", "", "")
	if rt.Provider != "custom" || rt.RequestedProvider != "custom:my-gateway" || rt.APIMode != modeCodexAppServer || rt.APIKey != "test-key" {
		t.Fatalf("named: %+v", rt)
	}
	anonymous := mustResolve(t, r, "custom", "k", "https://gateway.example.com/v1", "")
	if anonymous.APIMode != ModeChatCompletions {
		t.Fatalf("anonymous: %+v", anonymous)
	}
	delete(cfg["model"].(map[string]any), "openai_runtime")
	r = testResolver(t, cfg, &fakeCreds{}, "")
	if rt := mustResolve(t, r, "custom:my-gateway", "", "", ""); rt.APIMode != ModeChatCompletions {
		t.Fatalf("without opt in: %+v", rt)
	}
}

func TestNamedCustomProviderUsesSavedCredentials(t *testing.T) {
	cfg := map[string]any{"custom_providers": []any{map[string]any{"name": "Local", "base_url": "http://1.2.3.4:1234/v1", "api_key": "local-provider-key", "model": "gpt-5.6", "capabilities": map[string]any{"openai_native_compaction": true}}}}
	r := testResolver(t, cfg, &fakeCreds{}, "")
	rt := mustResolve(t, r, "local", "", "", "")
	if rt.Provider != "custom" || rt.APIMode != ModeChatCompletions || rt.BaseURL != "http://1.2.3.4:1234/v1" || rt.APIKey != "local-provider-key" || rt.RequestedProvider != "local" || rt.Source != "custom_provider:Local" {
		t.Fatalf("got %+v", rt)
	}
	if !reflect.DeepEqual(rt.Capabilities, map[string]bool{"openai_native_compaction": true}) || rt.Model != "gpt-5.6" {
		t.Fatalf("extras: %+v", rt)
	}
	cfg = map[string]any{"providers": map[string]any{"local": map[string]any{"name": "Local", "base_url": "http://1.2.3.4:1234/v1", "capabilities": map[string]any{"openai_native_compaction": true, "invalid-value": "yes", "42": true}}}}
	r = testResolver(t, cfg, &fakeCreds{}, "")
	entry := r.getNamedCustomProvider("local", nil, true)
	if entry == nil || !reflect.DeepEqual(entry["capabilities"], map[string]bool{"openai_native_compaction": true, "42": true}) {
		t.Fatalf("capabilities filtered: %v", entry)
	}
}

func TestBareCustomResolvesProvidersEntryNamedCustom(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{"custom": map[string]any{"api": "https://cliproxy.example.com/v1", "api_key": "cliproxy-key", "default_model": "gpt-5.4", "name": "CLIProxy"}}}
	r := testResolver(t, cfg, &fakeCreds{}, "")
	rt := mustResolve(t, r, "custom", "", "", "")
	if rt.Provider != "custom" || rt.BaseURL != "https://cliproxy.example.com/v1" || rt.APIKey != "cliproxy-key" || rt.RequestedProvider != "custom" {
		t.Fatalf("got %+v", rt)
	}
	r = testResolver(t, map[string]any{"providers": map[string]any{"custom": map[string]any{"api": "http://localhost:11434/v1"}}}, &fakeCreds{}, "")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.BaseURL != "http://localhost:11434/v1" || rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("loopback noauth: %+v", rt)
	}
}

func TestBareCustomWithoutCredentialsForRemoteEndpointFailsFast(t *testing.T) {
	cfg := map[string]any{"model": map[string]any{"provider": "volcano"}, "providers": map[string]any{"volcano": map[string]any{"api": "https://ark.example.com/v1", "key_env": "ARK_API_KEY"}}}
	r := testResolver(t, cfg, &fakeCreds{}, "")
	_, err := r.ResolveRuntime("custom", "", "", nil, "")
	authErr := authErrOf(t, err)
	if authErr.Provider != "custom" || authErr.Code != "missing_api_key" || !strings.Contains(authErr.Message, "provider 'custom'") || !strings.Contains(authErr.Message, "real name") {
		t.Fatalf("got %+v", authErr)
	}
	_, err = r.ResolveRuntime("ollama", "", "", nil, "")
	authErr = authErrOf(t, err)
	if authErr.Provider != "ollama" || authErr.Code != "missing_base_url" || !strings.Contains(authErr.Message, "provider 'ollama' has no endpoint") {
		t.Fatalf("alias: %+v", authErr)
	}
	r = testResolver(t, cfg, &fakeCreds{}, "OPENROUTER_API_KEY=sk-or-v1-usable-key\n")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.Provider != "custom" || rt.APIKey != "sk-or-v1-usable-key" {
		t.Fatalf("with openrouter key: %+v", rt)
	}
}

func TestLocalAliasWithoutAnyEndpointNeverReachesOpenRouter(t *testing.T) {
	for _, alias := range []string{"ollama", "vllm"} {
		r := testResolver(t, map[string]any{"model": map[string]any{"provider": alias}}, &fakeCreds{}, "OPENROUTER_API_KEY=sk-or-v1-cloud-key\n")
		_, err := r.ResolveRuntime(alias, "", "", nil, "")
		authErr := authErrOf(t, err)
		if authErr.Provider != alias || authErr.Code != "missing_base_url" || !strings.Contains(authErr.Message, "providers."+alias+".base_url") {
			t.Fatalf("%s: %+v", alias, authErr)
		}
		if bare := mustResolve(t, r, "custom", "", "", ""); bare.Provider != "custom" || bare.APIKey != "sk-or-v1-cloud-key" {
			t.Fatalf("%s bare custom: %+v", alias, bare)
		}
		t.Setenv("OPENROUTER_BASE_URL", "https://mirror.example/api/v1")
		if _, err := r.ResolveRuntime(alias, "key-for-my-local-server", "", nil, ""); err == nil || !strings.Contains(err.Error(), "has no endpoint") {
			t.Fatalf("%s explicit key does not lift the guard: %v", alias, err)
		}
	}
}

func TestLocalAliasWithAnEndpointAnywhereResolvesToIt(t *testing.T) {
	url := "http://192.168.1.5:11434/v1"
	configs := map[string]map[string]any{
		"providers": {"providers": map[string]any{"ollama": map[string]any{"base_url": url}}},
		"model":     {"model": map[string]any{"provider": "ollama", "base_url": url}},
		"explicit":  {"model": map[string]any{"provider": "ollama"}},
	}
	for name, cfg := range configs {
		r := testResolver(t, cfg, &fakeCreds{}, "OPENROUTER_API_KEY=sk-or-v1-cloud-key\n")
		explicit := ""
		if name == "explicit" {
			explicit = url
		}
		rt := mustResolve(t, r, "ollama", "", explicit, "")
		if rt.Provider != "custom" || rt.BaseURL != url || rt.APIKey != NoKeyPlaceholder {
			t.Fatalf("%s: %+v", name, rt)
		}
	}
}

func TestNamedCustomProvidersOnOneGatewayKeepOwnCredentials(t *testing.T) {
	cfg := map[string]any{"custom_providers": []any{
		map[string]any{"name": "gpt", "base_url": "https://gateway.example.com", "key_env": "GPT_KEY", "api_mode": "codex_responses", "model": "gpt-5.5"},
		map[string]any{"name": "claude", "base_url": "https://gateway.example.com", "key_env": "CLAUDE_KEY", "api_mode": "anthropic_messages", "model": "claude-opus-4-8"},
	}}
	r := testResolver(t, cfg, &fakeCreds{}, "GPT_KEY=gpt-secret\nCLAUDE_KEY=claude-secret\n")
	rt := mustResolve(t, r, "custom:claude", "", "", "")
	if rt.Provider != "custom" || rt.BaseURL != "https://gateway.example.com" || rt.APIKey != "claude-secret" || rt.APIMode != ModeAnthropicMessages || rt.RequestedProvider != "custom:claude" || rt.Model != "claude-opus-4-8" {
		t.Fatalf("got %+v", rt)
	}
	r = testResolver(t, map[string]any{"custom_providers": []any{map[string]any{"name": "Local LLM", "base_url": "http://localhost:1234/v1"}}}, &fakeCreds{}, "OPENAI_API_KEY=env-openai-key\n")
	rt = mustResolve(t, r, "custom:local-llm", "", "", "")
	if rt.BaseURL != "http://localhost:1234/v1" || rt.APIKey != NoKeyPlaceholder || rt.RequestedProvider != "custom:local-llm" {
		t.Fatalf("no leak to local: %+v", rt)
	}
	r = testResolver(t, map[string]any{"custom_providers": []any{map[string]any{"name": "kimi", "base_url": "https://my-custom-kimi.example.com/v1", "api_key": "my-kimi-key"}}}, &fakeCreds{}, "")
	entry := r.getNamedCustomProvider("kimi", nil, true)
	if entry == nil || entry["base_url"] != "https://my-custom-kimi.example.com/v1" || entry["api_key"] != "my-kimi-key" {
		t.Fatalf("alias named entry wins over builtin alias: %v", entry)
	}
}

func TestNamedCustomPoolResultIncludesExtrasAndTargetModel(t *testing.T) {
	cfg := map[string]any{"providers": map[string]any{"lmstudio": map[string]any{"base_url": "https://lmstudio.example.com/v1", "api_key": "not-used-when-pooled",
		"extra_headers": map[string]any{"CF-Access-Client-Id": "xxx.access", "CF-Access-Client-Secret": "yyy"}}}}
	creds := &fakeCreds{creds: map[string]Credential{"lmstudio": {APIKey: "pooled-key", Source: "pool:lmstudio-pool"}}}
	r := testResolver(t, cfg, creds, "")
	rt := mustResolve(t, r, "custom:lmstudio", "", "", "")
	if !reflect.DeepEqual(rt.ExtraHeaders, map[string]string{"CF-Access-Client-Id": "xxx.access", "CF-Access-Client-Secret": "yyy"}) || rt.APIKey != "pooled-key" || rt.Provider != "custom" || rt.RequestedProvider != "custom:lmstudio" {
		t.Fatalf("got %+v", rt)
	}
	proxy := map[string]any{"providers": map[string]any{"myproxy": map[string]any{"base_url": "http://127.0.0.1:10100/v1", "api_key": "no-key-required", "default_model": "default-model"}}}
	r = testResolver(t, proxy, &fakeCreds{}, "")
	if rt := mustResolve(t, r, "myproxy", "", "", "myproxy/gemini-flash"); rt.Provider != "custom" || rt.Model != "myproxy/gemini-flash" || rt.BaseURL != "http://127.0.0.1:10100/v1" {
		t.Fatalf("target model wins: %+v", rt)
	}
	if rt := mustResolve(t, r, "myproxy", "", "", ""); rt.Model != "default-model" {
		t.Fatalf("default kept: %+v", rt)
	}
	r = testResolver(t, proxy, &fakeCreds{creds: map[string]Credential{"myproxy": {APIKey: "pooled-key"}}}, "")
	if rt := mustResolve(t, r, "myproxy", "", "", "myproxy/gemini-flash"); rt.Model != "myproxy/gemini-flash" || rt.APIKey != "pooled-key" {
		t.Fatalf("pool target model wins: %+v", rt)
	}
}

func TestRemovedKeylessFreeProviderPointsAtReplacements(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "")
	for _, name := range []string{"opencode-free", "free", "opencode_free"} {
		_, err := r.resolveProvider(name, "", "", false)
		authErr := authErrOf(t, err)
		if authErr.Code != "invalid_provider" || !strings.Contains(authErr.Message, "opencode-zen") || !strings.Contains(authErr.Message, "opencode-go") {
			t.Fatalf("%s: %+v", name, authErr)
		}
	}
}

func TestBareCustomResolvesModelKeyEnvForConfiguredBaseURL(t *testing.T) {
	cfg := map[string]any{"custom_providers": []any{}, "model": map[string]any{"provider": "custom", "base_url": "https://api.example.test/v1", "key_env": "MY_LLM_API_KEY", "default": "glm-5.2", "api_mode": "chat_completions"}}
	r := testResolver(t, cfg, &fakeCreds{}, "MY_LLM_API_KEY=scw-real-key-0123456789abcdef\n")
	rt := mustResolve(t, r, "custom", "", "", "")
	if rt.BaseURL != "https://api.example.test/v1" || rt.APIKey != "scw-real-key-0123456789abcdef" {
		t.Fatalf("got %+v", rt)
	}
	t.Setenv("CUSTOM_BASE_URL", "https://other.example.test/v1")
	if rt := mustResolve(t, r, "custom", "", "", ""); rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("other endpoint: %+v", rt)
	}
	t.Setenv("CUSTOM_BASE_URL", "")
	direct := mustResolve(t, r, "custom", "", "https://api.example.test/v1", "")
	if direct.Source != "direct-alias" || direct.APIKey != "scw-real-key-0123456789abcdef" {
		t.Fatalf("direct alias: %+v", direct)
	}
	if other := mustResolve(t, r, "custom", "", "https://other.example.test/v1", ""); other.APIKey != NoKeyPlaceholder {
		t.Fatalf("other direct alias: %+v", other)
	}
}

// Captures warnings logged through the default slog logger
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, rec slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, rec.Message)
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	cap := &logCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return cap
}

func TestConfiguredKeyEnvResolvingEmptyIsLogged(t *testing.T) {
	cfg := map[string]any{"custom_providers": []any{
		map[string]any{"name": "scw", "base_url": "https://api.example.test/v1", "key_env": "UNSET_LLM_KEY", "model": "m"},
		map[string]any{"name": "local", "base_url": "http://127.0.0.1:8080/v1", "model": "m"},
	}}
	r := testResolver(t, cfg, &fakeCreds{}, "")
	t.Setenv("UNSET_LLM_KEY", "")
	logs := captureLogs(t)
	if rt := mustResolve(t, r, "custom:scw", "", "", ""); rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("scw: %+v", rt)
	}
	if rt := mustResolve(t, r, "custom:local", "", "", ""); rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("local: %+v", rt)
	}
	var hits []string
	for _, line := range logs.lines {
		if strings.Contains(line, "UNSET_LLM_KEY") {
			hits = append(hits, line)
		}
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "scw") {
		t.Fatalf("hits: %v", hits)
	}
}

// ---- entries keyed like a built in ----

const (
	shadowEndpoint = "http://127.0.0.1:18080/v1"
	shadowEntryKey = "entry-key-A-0123456789"
	shadowBuiltin  = "builtin-key-B-0123456789"
)

func shadowConfig(name string, entryExtra map[string]any) map[string]any {
	entry := map[string]any{"name": name, "api": shadowEndpoint, "transport": "chat_completions", "default_model": "m"}
	if entryExtra == nil {
		entry["key_env"] = "ENTRY_KEY_VAR"
	}
	for k, v := range entryExtra {
		entry[k] = v
	}
	return map[string]any{"model": map[string]any{"default": "m", "provider": "custom:" + name}, "providers": map[string]any{name: entry}}
}

func TestShadowingEntrySendsItsOwnKeyEnv(t *testing.T) {
	for _, c := range []struct{ name, env string }{{"deepseek", "DEEPSEEK_API_KEY"}, {"anthropic", "ANTHROPIC_API_KEY"}} {
		builtinPool := &fakeCreds{creds: map[string]Credential{c.name: {APIKey: shadowBuiltin, Source: "env:" + c.env}},
			scoped: func(poolKey string, entry map[string]any) (Credential, bool) {
				if EntryDeclaresCredential(entry) {
					return Credential{}, false
				}
				return Credential{APIKey: shadowBuiltin, Source: "env:" + c.env}, true
			}}
		r := testResolver(t, shadowConfig(c.name, nil), builtinPool, c.env+"="+shadowBuiltin+"\nENTRY_KEY_VAR="+shadowEntryKey+"\n")
		rt := mustResolve(t, r, "custom:"+c.name, "", "", "")
		if rt.BaseURL != shadowEndpoint || rt.APIKey != shadowEntryKey {
			t.Fatalf("%s: %+v", c.name, rt)
		}
	}
	pool := &fakeCreds{creds: map[string]Credential{"deepseek": {APIKey: shadowBuiltin, Source: "env:DEEPSEEK_API_KEY"}},
		scoped: func(poolKey string, entry map[string]any) (Credential, bool) {
			if EntryDeclaresCredential(entry) {
				return Credential{}, false
			}
			return Credential{APIKey: shadowBuiltin, Source: "env:DEEPSEEK_API_KEY"}, true
		}}
	r := testResolver(t, shadowConfig("deepseek", nil), pool, "DEEPSEEK_API_KEY="+shadowBuiltin+"\n")
	if rt := mustResolve(t, r, "custom:deepseek", "", "", ""); rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("unset key_env never borrows: %+v", rt)
	}
	r = testResolver(t, shadowConfig("deepseek", map[string]any{"api_key": shadowEntryKey}), pool, "DEEPSEEK_API_KEY="+shadowBuiltin+"\n")
	if rt := mustResolve(t, r, "custom:deepseek", "", "", ""); rt.APIKey != shadowEntryKey {
		t.Fatalf("inline api key: %+v", rt)
	}
	r = testResolver(t, shadowConfig("deepseek", map[string]any{}), pool, "DEEPSEEK_API_KEY="+shadowBuiltin+"\n")
	if rt := mustResolve(t, r, "custom:deepseek", "", "", ""); rt.APIKey != shadowBuiltin {
		t.Fatalf("entry without credential keeps pool resolution: %+v", rt)
	}
	r = testResolver(t, shadowConfig("mydeep", nil), pool, "DEEPSEEK_API_KEY="+shadowBuiltin+"\nENTRY_KEY_VAR="+shadowEntryKey+"\n")
	if rt := mustResolve(t, r, "custom:mydeep", "", "", ""); rt.BaseURL != shadowEndpoint || rt.APIKey != shadowEntryKey {
		t.Fatalf("non shadowing: %+v", rt)
	}
	for _, explicit := range []bool{false, true} {
		cfg := shadowConfig("deepseek", nil)
		cfg["model"] = map[string]any{"default": "m", "provider": "custom", "base_url": shadowEndpoint}
		r := testResolver(t, cfg, pool, "DEEPSEEK_API_KEY="+shadowBuiltin+"\nENTRY_KEY_VAR="+shadowEntryKey+"\n")
		base := ""
		if explicit {
			base = shadowEndpoint
		}
		if rt := mustResolve(t, r, "custom", "", base, ""); rt.BaseURL != shadowEndpoint || rt.APIKey != shadowEntryKey {
			t.Fatalf("url matched bare custom explicit=%v: %+v", explicit, rt)
		}
	}
	r = testResolver(t, shadowConfig("deepseek", map[string]any{}), pool, "DEEPSEEK_API_KEY="+shadowBuiltin+"\n")
	if rt := mustResolve(t, r, "custom", "", shadowEndpoint, ""); rt.APIKey != shadowBuiltin {
		t.Fatalf("url matched entry without credential: %+v", rt)
	}
	for _, requested := range []string{"custom:deepseek", "custom"} {
		cfg := shadowConfig("deepseek", nil)
		cfg["providers"].(map[string]any)["deepseek"].(map[string]any)["api"] = "https://api.deepseek.com/v1"
		r := testResolver(t, cfg, pool, "DEEPSEEK_API_KEY="+shadowBuiltin+"\n")
		logs := captureLogs(t)
		base := ""
		if requested == "custom" {
			base = "https://api.deepseek.com/v1"
		}
		rt := mustResolve(t, r, requested, "", base, "")
		if rt.APIKey != NoKeyPlaceholder {
			t.Fatalf("%s declared unset never falls back to host key: %+v", requested, rt)
		}
		found := false
		for _, line := range logs.lines {
			if strings.Contains(line, "ENTRY_KEY_VAR") && strings.Contains(line, "no-key-required") {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: warning missing: %v", requested, logs.lines)
		}
	}
}

// ---- anthropic ----

func TestAzureAnthropicEnvVarHint(t *testing.T) {
	const azureURL = "https://my-resource.services.ai.azure.com/anthropic"
	r := testResolver(t, map[string]any{"model": map[string]any{"provider": "anthropic", "base_url": azureURL, "key_env": "MY_CUSTOM_AZURE_KEY"}}, &fakeCreds{}, "MY_CUSTOM_AZURE_KEY=from-custom-var\n")
	rt := mustResolve(t, r, "anthropic", "", "", "")
	if rt.APIKey != "from-custom-var" || rt.BaseURL != azureURL {
		t.Fatalf("key_env hint: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "anthropic", "base_url": azureURL, "key_env": "UNSET_VAR"}}, &fakeCreds{}, "AZURE_ANTHROPIC_KEY=fallback-works\n")
	if rt := mustResolve(t, r, "anthropic", "", "", ""); rt.APIKey != "fallback-works" {
		t.Fatalf("unset hint falls through: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "anthropic", "base_url": azureURL}}, &fakeCreds{}, "")
	if _, err := r.ResolveRuntime("anthropic", "", "", nil, ""); err == nil || !strings.Contains(err.Error(), "No Azure Anthropic API key found") {
		t.Fatalf("missing azure key: %v", err)
	}
	creds := &fakeCreds{creds: map[string]Credential{"anthropic": {APIKey: "token-from-resolver", Source: "pool"}}}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "anthropic", "base_url": "https://api.anthropic.com", "key_env": "MY_KEY"}}, creds, "MY_KEY=custom-key-value\n")
	if rt := mustResolve(t, r, "anthropic", "", "", ""); rt.APIKey != "token-from-resolver" {
		t.Fatalf("non azure ignores key_env: %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "anthropic"}}, &fakeCreds{}, "")
	if _, err := r.ResolveRuntime("anthropic", "", "", nil, ""); err == nil || authErrOf(t, err).Message != noAnthropicCredentialsMsg {
		t.Fatalf("no credentials: %v", err)
	}
	benched := &fakeCreds{creds: map[string]Credential{"anthropic": {APIKey: "tok-1234"}}, byModel: map[string]map[string]Credential{"anthropic": {}}}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "anthropic"}}, benched, "")
	_, err := r.ResolveRuntime("anthropic", "", "", nil, "claude-opus-4")
	if err == nil || !strings.Contains(err.Error(), "rate-limited for claude-opus-4") {
		t.Fatalf("benched model: %v", err)
	}
}

func TestAutoProviderLocalBaseURLBypassesAnthropicKey(t *testing.T) {
	r := testResolver(t, map[string]any{"model": map[string]any{"default": "ollama/minimax-m2.7:cloud", "provider": "auto", "base_url": "http://localhost:11434"}}, &fakeCreds{}, "ANTHROPIC_API_KEY=sk-ant-fake-key\n")
	rt := mustResolve(t, r, "", "", "", "")
	if strings.Contains(rt.BaseURL, "anthropic.com") || rt.BaseURL != "http://localhost:11434" || rt.Provider == "anthropic" {
		t.Fatalf("got %+v", rt)
	}
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "auto", "base_url": "https://api.anthropic.com"}}, &fakeCreds{}, "ANTHROPIC_API_KEY=sk-ant-fake-key\n")
	if rt := mustResolve(t, r, "", "", "", ""); rt.Provider != "anthropic" {
		t.Fatalf("cloud url still anthropic: %+v", rt)
	}
	lookalike := "http://api.anthropic.com.attacker.test/v1"
	r = testResolver(t, map[string]any{"model": map[string]any{"provider": "auto", "base_url": lookalike}}, &fakeCreds{}, "ANTHROPIC_API_KEY=sk-ant-fake-key\n")
	if rt := mustResolve(t, r, "", "", "", ""); rt.Provider == "anthropic" || rt.BaseURL != lookalike {
		t.Fatalf("lookalike: %+v", rt)
	}
}

// ---- azure foundry ----

func TestAzureFoundryResolution(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "AZURE_FOUNDRY_API_KEY=az-key\n")
	if _, err := r.ResolveRuntime("azure-foundry", "", "", nil, ""); err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("missing base url: %v", err)
	}
	mk := func(model, mode string) map[string]any {
		return map[string]any{"model": map[string]any{"provider": "azure-foundry", "base_url": "https://synopsisse.openai.azure.com/openai/v1", "api_mode": mode, "default": model}}
	}
	r = testResolver(t, mk("gpt-5.3-codex", "chat_completions"), &fakeCreds{}, "AZURE_FOUNDRY_API_KEY=az-key\n")
	rt := mustResolve(t, r, "azure-foundry", "", "", "")
	if rt.APIMode != ModeCodexResponses || rt.BaseURL != "https://synopsisse.openai.azure.com/openai/v1" || rt.APIKey != "az-key" || rt.Source != "config" {
		t.Fatalf("gpt5 upgrade: %+v", rt)
	}
	r = testResolver(t, mk("o3-mini", "chat_completions"), &fakeCreds{}, "AZURE_FOUNDRY_API_KEY=az-key\n")
	if rt := mustResolve(t, r, "azure-foundry", "", "", ""); rt.APIMode != ModeCodexResponses {
		t.Fatalf("o3 upgrade: %+v", rt)
	}
	r = testResolver(t, mk("gpt-4.1", "chat_completions"), &fakeCreds{}, "AZURE_FOUNDRY_API_KEY=az-key\n")
	if rt := mustResolve(t, r, "azure-foundry", "", "", ""); rt.APIMode != ModeChatCompletions {
		t.Fatalf("gpt-4.1 stays: %+v", rt)
	}
	r = testResolver(t, mk("claude-opus-4", "anthropic_messages"), &fakeCreds{}, "AZURE_FOUNDRY_API_KEY=az-key\n")
	if rt := mustResolve(t, r, "azure-foundry", "", "", ""); rt.APIMode != ModeAnthropicMessages || rt.BaseURL != "https://synopsisse.openai.azure.com/openai" {
		t.Fatalf("anthropic strips v1: %+v", rt)
	}
	entra := mk("gpt-4.1", "chat_completions")
	entra["model"].(map[string]any)["auth_mode"] = "entra_id"
	entra["model"].(map[string]any)["entra"] = map[string]any{"scope": "https://cognitiveservices.azure.com/.default"}
	r = testResolver(t, entra, &fakeCreds{}, "")
	rt = mustResolve(t, r, "azure-foundry", "", "", "")
	if rt.AuthMode != "entra_id" || rt.TokenSource == nil || rt.Source != "entra_id" || rt.Entra["scope"] != "https://cognitiveservices.azure.com/.default" {
		t.Fatalf("entra: %+v", rt)
	}
	if rt := mustResolve(t, r, "azure-foundry", "cli-key", "", ""); rt.AuthMode != "api_key" || rt.APIKey != "cli-key" || rt.Source != "explicit" {
		t.Fatalf("explicit key escape hatch: %+v", rt)
	}
	r = testResolver(t, mk("gpt-4.1", "chat_completions"), &fakeCreds{}, "")
	if _, err := r.ResolveRuntime("azure-foundry", "", "", nil, ""); err == nil || !strings.Contains(err.Error(), "Azure Foundry requires an API key") {
		t.Fatalf("missing key: %v", err)
	}
}

// ---- bedrock ----

func TestBedrockClaudeTargetModelUsesAnthropicMessages(t *testing.T) {
	r := testResolver(t, map[string]any{"model": map[string]any{"default": "amazon.nova-pro-v1:0"}, "bedrock": map[string]any{}}, &fakeCreds{}, "")
	rt := mustResolve(t, r, "bedrock", "", "", "global.anthropic.claude-sonnet-4-6")
	if rt.Provider != "bedrock" || rt.APIMode != ModeAnthropicMessages || !rt.BedrockAnthropic || rt.Region != "us-east-1" {
		t.Fatalf("got %+v", rt)
	}
	if rt := mustResolve(t, r, "bedrock", "", "", ""); rt.APIMode != modeBedrockConverse || rt.BedrockAnthropic {
		t.Fatalf("nova stays converse: %+v", rt)
	}
	route := rt.Route()
	if route.Auth != AuthAWSSigV4 || route.Region != "us-east-1" {
		t.Fatalf("route: %+v", route)
	}
}

// ---- moa and vertex shortcuts ----

func TestRequestedShortcuts(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{}, "")
	rt := mustResolve(t, r, "moa", "", "", "")
	if rt.Provider != "moa" || rt.BaseURL != MoaBaseURL || rt.APIKey != MoaAPIKey {
		t.Fatalf("moa: %+v", rt)
	}
	rt = mustResolve(t, r, "anthropic", "", "https://res.services.ai.azure.com/anthropic/", "")
	if rt.Provider != "anthropic" || rt.Source != "azure-explicit" || rt.BaseURL != "https://res.services.ai.azure.com/anthropic" {
		t.Fatalf("azure anthropic: %+v", rt)
	}
	if _, err := r.ResolveRuntime("vertex", "", "", nil, ""); err == nil || !strings.Contains(err.Error(), "Vertex AI credentials could not be resolved") {
		t.Fatalf("vertex: %v", err)
	}
}

// ---- guards ----

func TestDisabledProviderGuardAndUnknownProvider(t *testing.T) {
	r := testResolver(t, map[string]any{"providers": map[string]any{"deepseek": map[string]any{"enabled": false}}}, &fakeCreds{}, "DEEPSEEK_API_KEY=sk-deepseek-1234\n")
	_, err := r.ResolveRuntime("deepseek", "", "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "disabled in config") {
		t.Fatalf("disabled: %v", err)
	}
	if _, isAuth := err.(*AuthError); isAuth {
		t.Fatal("disabled is a plain error, not an AuthError")
	}
	r = testResolver(t, nil, &fakeCreds{}, "")
	_, err = r.ResolveRuntime("no-such-provider", "", "", nil, "")
	if authErr := authErrOf(t, err); authErr.Code != "invalid_provider" || !strings.Contains(authErr.Message, "Unknown provider 'no-such-provider'") {
		t.Fatalf("unknown: %+v", authErr)
	}
	_, err = r.ResolveRuntime("auto", "", "", nil, "")
	if authErr := authErrOf(t, err); authErr.Code != "no_provider_configured" {
		t.Fatalf("nothing configured: %+v", authErr)
	}
	r = testResolver(t, nil, &fakeCreds{free: true}, "")
	if p, err := r.resolveProvider("auto", "", "", false); err != nil || p != "nous" {
		t.Fatalf("free tier: %q %v", p, err)
	}
	if p, err := r.resolveProvider("auto", "", "", true); err == nil || p != "" {
		t.Fatalf("free tier skipped: %q %v", p, err)
	}
	r = testResolver(t, nil, &fakeCreds{}, "")
	r.cfgError = errors.New("bad yaml")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-abcdef")
	if _, err := r.resolveProvider("auto", "", "", false); err == nil || authErrOf(t, err).Code != "corrupt_config" {
		t.Fatalf("corrupt config: %v", err)
	}
}

func TestConfiguredCustomEndpointResolvesAsAProvider(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"provider custom", map[string]any{"model": map[string]any{"default": "nvidia/Nemotron", "provider": "custom", "base_url": "http://127.0.0.1:8000/v1", "api_key": "dummy"}}, "custom"},
		{"local alias", map[string]any{"model": map[string]any{"default": "qwen3", "provider": "vllm", "base_url": "http://127.0.0.1:8000/v1"}}, "custom"},
		{"loopback url only", map[string]any{"model": map[string]any{"default": "qwen3", "base_url": "http://localhost:8080/v1"}}, "custom"},
		{"provider openrouter", map[string]any{"model": map[string]any{"default": "openrouter/auto", "provider": "openrouter"}}, "openrouter"},
		{"openrouter mirror", map[string]any{"model": map[string]any{"default": "openrouter/auto", "provider": "openrouter", "base_url": "https://openrouter-mirror.example.com/api/v1"}}, "openrouter"},
		{"named providers pin", map[string]any{"model": map[string]any{"default": "test-model", "provider": "CPA"}, "providers": map[string]any{"CPA": map[string]any{"api": "http://127.0.0.1:8317/v1", "default_model": "test-model"}}}, "custom"},
		{"provider auto loopback", map[string]any{"model": map[string]any{"provider": "auto", "base_url": "http://127.0.0.1:8000/v1"}}, "custom"},
	}
	for _, c := range cases {
		r := testResolver(t, c.cfg, &fakeCreds{}, "")
		got, err := r.resolveProvider("auto", "", "", false)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q %v", c.name, got, err)
		}
	}
	r := testResolver(t, map[string]any{"model": map[string]any{"default": "some/model", "base_url": "https://api.z.ai/v1"}}, &fakeCreds{}, "")
	if _, err := r.resolveProvider("auto", "", "", false); err == nil {
		t.Fatal("stale remote url without a pin is not a provider")
	}
}

func TestEnvKeyAutoDetectionPrefersOpenRouterAndWarnsOverOAuth(t *testing.T) {
	r := testResolver(t, nil, &fakeCreds{active: "openai-codex"}, "DEEPSEEK_API_KEY=sk-deepseek-1234\n")
	logs := captureLogs(t)
	p, err := r.resolveProvider("auto", "", "", false)
	if err != nil || p != "deepseek" {
		t.Fatalf("got %q %v", p, err)
	}
	if len(logs.lines) == 0 || !strings.Contains(logs.lines[0], "preempting your logged-in OAuth provider") {
		t.Fatalf("warning: %v", logs.lines)
	}
	r = testResolver(t, nil, &fakeCreds{active: "openai-codex"}, "")
	if p, _ := r.resolveProvider("auto", "", "", false); p != "openai-codex" {
		t.Fatalf("oauth last resort: %q", p)
	}
	r = testResolver(t, nil, &fakeCreds{}, "")
	if p, _ := r.resolveProvider("auto", "k", "", false); p != "openrouter" {
		t.Fatalf("explicit creds mean openrouter: %q", p)
	}
	if p, _ := r.resolveProvider("kimi", "", "", false); p != "kimi-coding" {
		t.Fatalf("auth alias table: %q", p)
	}
	if p, _ := r.resolveProvider("openai", "", "", false); p != "" {
		t.Fatalf("openai is not an auth alias: %q", p)
	}
}

// ---- declared key missing under a routed profile ----

func namedProfileResolver(t *testing.T, keyEnv bool, dotenv string, modelExtra map[string]any) *resolver {
	t.Helper()
	clearProviderEnv(t)
	root := filepath.Join(t.TempDir(), ".icebath")
	h := filepath.Join(root, "profiles", "beta")
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte(dotenv), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("CANARY_KEY", "canary-process-env")
	model := map[string]any{"default": "fake-model", "provider": "custom", "base_url": "http://127.0.0.1:18991/v1"}
	for k, v := range modelExtra {
		model[k] = v
	}
	entry := map[string]any{"name": "FakeUp", "base_url": "http://127.0.0.1:18991/v1"}
	if keyEnv {
		entry["key_env"] = "CANARY_KEY"
	}
	cfg := config.Tree{"model": model, "custom_providers": []any{entry}}
	SetMultiplexActive(true)
	t.Cleanup(func() { SetMultiplexActive(false) })
	ctx := WithSecretScope(context.Background(), BuildProfileSecretScope(h), h)
	return newResolver(ctx, cfg, config.NewSecrets(h), h, &fakeCreds{})
}

func TestDeclaredKeyMissingFromProfileScopeFailsBeforeTheRequest(t *testing.T) {
	r := namedProfileResolver(t, true, "", nil)
	_, err := r.ResolveRuntime("", "", "", nil, "")
	authErr := authErrOf(t, err)
	if authErr.Code != "missing_api_key" {
		t.Fatalf("code: %+v", authErr)
	}
	for _, needle := range []string{"FakeUp", "CANARY_KEY", "'beta'", "icebath -p beta config set CANARY_KEY", "server's environment"} {
		if !strings.Contains(authErr.Message, needle) {
			t.Fatalf("missing %q in %q", needle, authErr.Message)
		}
	}
	if strings.Contains(authErr.Message, "canary-process-env") {
		t.Fatal("secret leaked into the message")
	}
	r = namedProfileResolver(t, false, "", nil)
	if rt := mustResolve(t, r, "", "", "", ""); rt.Provider != "custom" || rt.APIKey != NoKeyPlaceholder {
		t.Fatalf("keyless local endpoint: %+v", rt)
	}
	r = namedProfileResolver(t, true, "CANARY_KEY=canary-profile-dotenv\n", nil)
	if rt := mustResolve(t, r, "", "", "", ""); rt.APIKey != "canary-profile-dotenv" {
		t.Fatalf("profile dotenv key: %+v", rt)
	}
	r = namedProfileResolver(t, true, "", map[string]any{"provider": "custom:FakeUp", "openai_runtime": "codex_app_server"})
	if rt := mustResolve(t, r, "", "", "", ""); rt.Provider != "custom" || rt.APIMode != modeCodexAppServer {
		t.Fatalf("codex app server owns auth: %+v", rt)
	}
	r = namedProfileResolver(t, true, "", nil)
	if rt := mustResolve(t, r, "", "no-key-required", "", ""); rt.APIMode != ModeChatCompletions {
		t.Fatalf("explicit placeholder: %+v", rt)
	}
	r = namedProfileResolver(t, true, "", map[string]any{"provider": "custom:FakeUp"})
	_, err = r.ResolveRuntime("", "", "http://127.0.0.1:18992/v1", nil, "")
	if authErr := authErrOf(t, err); authErr.Code != "missing_api_key" || !strings.Contains(authErr.Message, "FakeUp") {
		t.Fatalf("explicit base url keeps entry: %+v", authErr)
	}
}

// ---- auxiliary and fallback entry points ----

func TestResolveTaskProviderModelPrecedence(t *testing.T) {
	cfg := map[string]any{"auxiliary": map[string]any{
		"vision":      map[string]any{"provider": "auto", "model": "auto"},
		"compression": map[string]any{"provider": "deepseek", "model": "deepseek-flash", "api_mode": "responses"},
		"review":      map[string]any{"base_url": "http://127.0.0.1:1/v1", "api_key": "k", "model": "m"},
		"keyed":       map[string]any{"base_url": "http://127.0.0.1:2/v1", "key_env": "AUX_KEY", "provider": "ollama"},
	}}
	r := testResolver(t, cfg, &fakeCreds{}, "AUX_KEY=aux-secret\n")
	if ar := r.resolveTaskProviderModel("vision", "", "", "", ""); ar.provider != "auto" || ar.model != "" {
		t.Fatalf("auto sentinel: %+v", ar)
	}
	if ar := r.resolveTaskProviderModel("compression", "", "", "", ""); ar.provider != "deepseek" || ar.model != "deepseek-flash" || ar.apiMode != ModeCodexResponses {
		t.Fatalf("config provider: %+v", ar)
	}
	if ar := r.resolveTaskProviderModel("review", "", "", "", ""); ar.provider != "custom" || ar.baseURL != "http://127.0.0.1:1/v1" || ar.apiKey != "k" {
		t.Fatalf("bare base url is custom: %+v", ar)
	}
	if ar := r.resolveTaskProviderModel("keyed", "", "", "", ""); ar.provider != "ollama" || ar.apiKey != "aux-secret" {
		t.Fatalf("local alias keeps identity: %+v", ar)
	}
	if ar := r.resolveTaskProviderModel("compression", "anthropic", "claude", "", ""); ar.provider != "anthropic" || ar.model != "claude" {
		t.Fatalf("explicit args win: %+v", ar)
	}
	if ar := r.resolveTaskProviderModel("review", "deepseek", "", "", ""); ar.provider != "deepseek" || ar.baseURL != "http://127.0.0.1:1/v1" || ar.apiKey != "k" {
		t.Fatalf("explicit provider adopts the task endpoint: %+v", ar)
	}
	merged := layerOverInherited(map[string]any{"provider": "a", "model": "m", "base_url": "u", "timeout": 5}, map[string]any{"provider": "auto", "model": "", "timeout": 9})
	if merged["provider"] != "a" || merged["model"] != "m" || merged["timeout"] != 9 {
		t.Fatalf("unpinned follows base: %v", merged)
	}
	pinned := layerOverInherited(map[string]any{"provider": "a", "model": "m", "base_url": "u", "api_key": "k"}, map[string]any{"provider": "b", "model": ""})
	if pinned["provider"] != "b" || pinned["model"] != "" || pinned["base_url"] != nil || pinned["api_key"] != nil {
		t.Fatalf("pinned route replaces the whole route: %v", pinned)
	}
}

func TestResolveAuxiliaryRouteAndFallbackChain(t *testing.T) {
	cfg := map[string]any{
		"model":     map[string]any{"provider": "deepseek", "default": "deepseek-v4-pro"},
		"auxiliary": map[string]any{"compression": map[string]any{"provider": "custom:gw", "model": "small"}},
		"providers": map[string]any{"gw": map[string]any{"api": "https://gw.example/v1", "api_key": "gw-key-1234"}},
		"fallback_providers": []any{
			map[string]any{"provider": "custom:gw", "model": "fb-model"},
			map[string]any{"provider": "nonexistent", "model": "x"},
		},
	}
	clearProviderEnv(t)
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("DEEPSEEK_API_KEY=sk-deepseek-1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	secrets := config.NewSecrets(h)
	route, err := Resolve(context.Background(), config.Tree(cfg), secrets, h, &fakeCreds{}, Overrides{})
	if err != nil || route.Provider != "deepseek" || route.Model != "deepseek-v4-pro" || route.APIKey != "sk-deepseek-1234" {
		t.Fatalf("main: %+v %v", route, err)
	}
	aux, err := ResolveAux(context.Background(), config.Tree(cfg), secrets, h, &fakeCreds{}, "compression")
	if err != nil || aux.Provider != "custom:gw" || aux.Model != "small" || aux.BaseURL != "https://gw.example/v1" || aux.APIKey != "gw-key-1234" {
		t.Fatalf("aux: %+v %v", aux, err)
	}
	routes, err := FallbackRoutes(context.Background(), config.Tree(cfg), secrets, h, &fakeCreds{})
	if err != nil || len(routes) != 1 || routes[0].Provider != "custom:gw" || routes[0].Model != "fb-model" {
		t.Fatalf("fallback: %+v %v", routes, err)
	}
	r := newResolver(context.Background(), config.Tree(cfg), secrets, h, &fakeCreds{})
	rt, entry, err := r.ResolveRuntimeWithFallback("anthropic", "", "", "")
	if err != nil || entry == nil || rt.Provider != "custom:gw" || rt.APIKey != "gw-key-1234" {
		t.Fatalf("with fallback: %+v %v %v", rt, entry, err)
	}
	if rt, _, err := r.ResolveRuntimeWithFallback("no-such-provider", "", "", ""); err != nil || rt.Provider != "custom:gw" {
		t.Fatalf("an unknown provider is an AuthError and walks the chain: %+v %v", rt, err)
	}
	cfg["providers"].(map[string]any)["deepseek"] = map[string]any{"enabled": false}
	r = newResolver(context.Background(), config.Tree(cfg), secrets, h, &fakeCreds{})
	if _, _, err := r.ResolveRuntimeWithFallback("deepseek", "", "", ""); err == nil || !strings.Contains(err.Error(), "disabled in config") {
		t.Fatalf("a plain misconfiguration error propagates: %v", err)
	}
}

func TestRuntimeRouteConversion(t *testing.T) {
	rt := &Runtime{Provider: "custom", APIMode: ModeChatCompletions, BaseURL: "http://127.0.0.1:1/v1", APIKey: NoKeyPlaceholder,
		ExtraHeaders: map[string]string{"X": "1"}, RequestOverrides: map[string]any{"extra_body": map[string]any{"a": 1}}, Model: "m"}
	route := rt.Route()
	if route.Auth != AuthNone || !route.Local || route.Headers["X"] != "1" || route.ExtraBody["a"] != 1 || route.Model != "m" {
		t.Fatalf("got %+v", route)
	}
	rt = &Runtime{Provider: "openai-codex", APIMode: ModeCodexResponses, TokenSource: func(context.Context) (string, error) { return "t", nil }}
	if route := rt.Route(); route.Auth != AuthOAuth {
		t.Fatalf("oauth: %+v", route)
	}
}
