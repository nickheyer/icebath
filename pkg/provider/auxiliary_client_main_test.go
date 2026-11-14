package provider

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Temp home with the config text bound to the environment and the returned context
func auxMainTestHome(t *testing.T, configYAML string) context.Context {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if configYAML != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(configYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home.WithOverride(context.Background(), h)
}

// Swaps the anthropic seams and records the token and base URL the client builder receives
func auxFakeAnthropicBuild(t *testing.T, pool func(context.Context, string) (bool, *PooledCredential), token string) *[2]any {
	t.Helper()
	captured := &[2]any{}
	oldBuild, oldPool, oldTok := auxBuildAnthropicClientFn, auxSelectAnthropicEntryFn, auxResolveAnthropicTokenFn
	t.Cleanup(func() {
		auxBuildAnthropicClientFn, auxSelectAnthropicEntryFn, auxResolveAnthropicTokenFn = oldBuild, oldPool, oldTok
	})
	auxBuildAnthropicClientFn = func(_ context.Context, apiKey any, baseURL string, _ float64, _ bool) (*anthropicSDKClient, error) {
		captured[0], captured[1] = apiKey, baseURL
		return &anthropicSDKClient{}, nil
	}
	auxSelectAnthropicEntryFn = pool
	auxResolveAnthropicTokenFn = func(context.Context) string { return token }
	return captured
}

func TestTryAnthropicPoolExhaustedFallsBackToEnv(t *testing.T) {
	presentEmpty := func(context.Context, string) (bool, *PooledCredential) { return true, nil }
	t.Run("pool_present_no_entry_falls_back_to_resolve_token", func(t *testing.T) {
		ctx := auxMainTestHome(t, "")
		captured := auxFakeAnthropicBuild(t, presentEmpty, "sk-ant-oat-oauth-token")
		client, model, err := auxTryAnthropic(ctx, nil, "")
		if err != nil || client == nil || model == "" {
			t.Fatalf("got (%v, %q, %v)", client, model, err)
		}
		if _, ok := client.(*AnthropicAuxiliaryClient); !ok {
			t.Fatalf("got %T", client)
		}
		if captured[0] != "sk-ant-oat-oauth-token" {
			t.Fatalf("token %v", captured[0])
		}
	})
	t.Run("pool_present_no_entry_and_no_token_still_returns_none", func(t *testing.T) {
		ctx := auxMainTestHome(t, "")
		auxFakeAnthropicBuild(t, presentEmpty, "")
		client, model, err := auxTryAnthropic(ctx, nil, "")
		if client != nil || model != "" || err != nil {
			t.Fatalf("got (%v, %q, %v)", client, model, err)
		}
	})
	t.Run("base_url_defaults_when_pool_present_but_no_entry", func(t *testing.T) {
		ctx := auxMainTestHome(t, "")
		captured := auxFakeAnthropicBuild(t, presentEmpty, "sk-ant-oat-oauth-token")
		if client, _, err := auxTryAnthropic(ctx, nil, ""); client == nil || err != nil {
			t.Fatalf("got (%v, %v)", client, err)
		}
		if captured[1] != "https://api.anthropic.com" {
			t.Fatalf("base %v", captured[1])
		}
	})
}

// Runs auxTryAnthropic against a config base_url and checks the builder received the expected one
func auxHostValidationCase(t *testing.T, base, want string) {
	t.Helper()
	absent := func(context.Context, string) (bool, *PooledCredential) { return false, nil }
	ctx := auxMainTestHome(t, "model:\n  provider: anthropic\n  model: claude-haiku-4-5-20251001\n  base_url: "+base+"\n")
	captured := auxFakeAnthropicBuild(t, absent, "***")
	if client, _, err := auxTryAnthropic(ctx, nil, ""); client == nil || err != nil {
		t.Fatalf("got (%v, %v)", client, err)
	}
	if captured[1] != want {
		t.Fatalf("base %v, want %s", captured[1], want)
	}
}

func TestTryAnthropicBaseURLHostValidation(t *testing.T) {
	t.Run("test_openrouter_base_url_does_not_leak_into_auxiliary", func(t *testing.T) {
		auxHostValidationCase(t, "https://openrouter.ai/api/v1", "https://api.anthropic.com")
	})
	t.Run("test_anthropic_default_host_is_preserved", func(t *testing.T) {
		auxHostValidationCase(t, "https://api.anthropic.com", "https://api.anthropic.com")
	})
	t.Run("test_openai_base_url_does_not_leak", func(t *testing.T) {
		auxHostValidationCase(t, "https://api.openai.com/v1", "https://api.anthropic.com")
	})
	t.Run("test_anthropic_suffix_gateway_base_url_is_applied", func(t *testing.T) {
		auxHostValidationCase(t, "https://gateway.example.com/anthropic", "https://gateway.example.com/anthropic")
	})
	t.Run("test_anthropic_host_with_path_is_preserved", func(t *testing.T) {
		auxHostValidationCase(t, "https://api.anthropic.com/v1/messages", "https://api.anthropic.com/v1/messages")
	})
	t.Run("explicit_foreign_base_url_is_refused", func(t *testing.T) {
		absent := func(context.Context, string) (bool, *PooledCredential) { return false, nil }
		ctx := auxMainTestHome(t, "")
		captured := auxFakeAnthropicBuild(t, absent, "***")
		client, model, err := auxTryAnthropic(ctx, "explicit", "https://evil.example.com/v1")
		if client != nil || model != "" || err != nil || captured[1] != nil {
			t.Fatalf("got (%v, %q, %v), built %v", client, model, err, captured[1])
		}
	})
}

func TestProxyEnvRejectsMalformedPort(t *testing.T) {
	for _, key := range auxProxyEnvKeys {
		t.Run(key, func(t *testing.T) {
			for _, other := range auxProxyEnvKeys {
				t.Setenv(other, "")
			}
			t.Setenv(key, "http://127.0.0.1:6153export")
			err := auxValidateProxyEnvURLs()
			if err == nil || !regexp.MustCompile(`(?i)Malformed proxy environment variable `+key+`=.*6153export`).MatchString(err.Error()) {
				t.Fatalf("got %v", err)
			}
			if _, ok := err.(*AuxRuntimeError); !ok {
				t.Fatalf("want RuntimeError, got %T", err)
			}
		})
	}
}

func TestBaseUrlAcceptsValid(t *testing.T) {
	for _, u := range []string{"https://api.example.com/v1", "http://127.0.0.1:6153/v1", "acp://copilot", ""} {
		if err := auxValidateBaseURL(u); err != nil {
			t.Fatalf("%q: %v", u, err)
		}
	}
	if err := auxValidateBaseURL("http://127.0.0.1:6153export/v1"); err == nil {
		t.Fatal("malformed port must be rejected")
	}
}

// Swaps the custom runtime and main model seams
func auxFakeCustomRuntime(t *testing.T, base, key, mode, model string) {
	t.Helper()
	oldRuntime, oldModel := auxResolveCustomRuntimeFn, auxReadMainModel
	t.Cleanup(func() { auxResolveCustomRuntimeFn, auxReadMainModel = oldRuntime, oldModel })
	auxResolveCustomRuntimeFn = func(context.Context) (string, any, string, error) { return base, key, mode, nil }
	auxReadMainModel = func(context.Context) string { return model }
}

func TestTryCustomEndpointAnthropicCustom(t *testing.T) {
	t.Run("custom_endpoint_anthropic_messages_builds_anthropic_wrapper", func(t *testing.T) {
		ctx := auxMainTestHome(t, "")
		auxFakeCustomRuntime(t, "https://api.minimax.io/anthropic", "minimax-key", "anthropic_messages", "claude-sonnet-4-6")
		auxFakeAnthropicBuild(t, nil, "")
		client, model, err := auxTryCustomEndpoint(ctx)
		ac, ok := client.(*AnthropicAuxiliaryClient)
		if err != nil || !ok || model != "claude-sonnet-4-6" {
			t.Fatalf("got (%T, %q, %v)", client, model, err)
		}
		if ac.APIKey() != "minimax-key" || ac.BaseURL() != "https://api.minimax.io/anthropic" {
			t.Fatalf("got key %q base %q", ac.APIKey(), ac.BaseURL())
		}
	})
	t.Run("custom_endpoint_chat_completions_still_uses_openai_wire", func(t *testing.T) {
		ctx := auxMainTestHome(t, "")
		auxFakeCustomRuntime(t, "https://api.example.com/v1", "key", "", "my-model")
		client, model, err := auxTryCustomEndpoint(ctx)
		if err != nil || model != "my-model" {
			t.Fatalf("got (%T, %q, %v)", client, model, err)
		}
		if _, ok := client.(*auxOpenAIClient); !ok {
			t.Fatalf("want the OpenAI wire client, got %T", client)
		}
	})
	t.Run("codex_backend_base_is_never_a_custom_endpoint", func(t *testing.T) {
		ctx := auxMainTestHome(t, "")
		auxFakeCustomRuntime(t, "https://chatgpt.com/backend-api/codex", "key", "", "m")
		if client, model, err := auxTryCustomEndpoint(ctx); client != nil || model != "" || err != nil {
			t.Fatalf("got (%v, %q, %v)", client, model, err)
		}
	})
}

func TestReadMainFieldAndSameOriginKey(t *testing.T) {
	ctx := auxMainTestHome(t, "model:\n  default: cfg-model\n  provider: OpenRouter\n  base_url: https://api.example.com/v1\n  api_key: cfg-key\n")
	if got := auxReadMainModel(ctx); got != "cfg-model" {
		t.Fatalf("model %q", got)
	}
	if got := auxReadMainProvider(ctx); got != "openrouter" {
		t.Fatalf("provider %q", got)
	}
	bound := context.WithValue(ctx, auxRuntimeMainKey{}, map[string]any{"model": " live-model "})
	if got := auxReadMainModel(bound); got != "live-model" {
		t.Fatalf("live model %q", got)
	}
	if got, err := auxReadMainAPIKeyIfSameOrigin(ctx, "https://api.example.com:443/other"); err != nil || got != "cfg-key" {
		t.Fatalf("same origin %v %v", got, err)
	}
	for _, other := range []string{"http://api.example.com/v1", "https://api.example.com:8443/v1", "https://evil.example.com/v1", "not a url"} {
		if got, err := auxReadMainAPIKeyIfSameOrigin(ctx, other); err != nil || got != "" {
			t.Fatalf("%s: %v %v", other, got, err)
		}
	}
	strCtx := auxMainTestHome(t, "model: plain-model\n")
	if got := auxReadMainModel(strCtx); got != "plain-model" {
		t.Fatalf("string model %q", got)
	}
}

func TestRelayAuxCallScopeAndMetadata(t *testing.T) {
	ctx := context.Background()
	if _, _, _, ok := auxRelayAuxiliaryMetadata(ctx, "", ""); ok || auxRelayAuxCallContext(ctx) != nil {
		t.Fatal("no record outside a scope")
	}
	scoped := auxRelayAuxCallScope(ctx, "")
	record := auxRelayAuxCallContext(scoped)
	if record["task"] != "unknown" || !regexp.MustCompile(`^aux-[0-9a-f]{32}$`).MatchString(record["request_id"].(string)) {
		t.Fatalf("record %v", record)
	}
	auxSetRelayAuxiliaryRoute(scoped, "openrouter", "", "")
	provider, model, meta, ok := auxRelayAuxiliaryMetadata(scoped, "", "")
	if !ok || provider != "openrouter" || model != "unknown" || meta["retry_count"] != 0 || meta["call_role"] != "auxiliary:unknown" || meta["api_mode"] != "chat_completions" {
		t.Fatalf("first attempt %s %s %v", provider, model, meta)
	}
	provider, _, meta, _ = auxRelayAuxiliaryMetadata(scoped, "nous", "codex_responses")
	if provider != "nous" || meta["retry_count"] != 1 || meta["api_mode"] != "codex_responses" || meta["api_request_id"] != record["request_id"] {
		t.Fatalf("second attempt %s %v", provider, meta)
	}
	info := map[string]string{}
	auxRecordRouteInfo(info, "", "")
	if info["provider"] != "auto" || info["model"] != "default" {
		t.Fatalf("route info %v", info)
	}
	auxRecordRouteInfo(nil, "x", "y")
}

func TestBypassChatSDKRequestTransform(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	req := map[string]any{"model": "m", "messages": msgs, "extra_body": map[string]any{"tools": "kept"}}
	sdk := newAuxOpenAIClient("k", "https://api.example.com/v1", auxOpenAIOptions{})
	got := BypassChatSDKRequestTransform(req, sdk)
	extra := got["extra_body"].(map[string]any)
	if slot, ok := got["messages"].([]any); !ok || len(slot) != 0 || extra["tools"] != "kept" || len(extra["messages"].([]any)) != 1 {
		t.Fatalf("bypassed %v", got)
	}
	body, err := auxSDKBuildRequest(sdk, got)
	direct, _ := auxSDKBuildRequest(sdk, map[string]any{"model": "m", "messages": msgs, "tools": "kept"})
	if err != nil || string(body.body) != string(direct.body) {
		t.Fatalf("wire bytes differ\n%s\n%s", body.body, direct.body)
	}
	if out := BypassChatSDKRequestTransform(req, &auxProbeClient{}); len(out["messages"].([]any)) != 1 {
		t.Fatal("non SDK facades keep the typed path")
	}
	t.Setenv("ICEBATH_CODEX_SDK_TRANSFORM", "1")
	if out := BypassChatSDKRequestTransform(req, sdk); len(out["messages"].([]any)) != 1 {
		t.Fatal("escape hatch keeps the typed path")
	}
}

func TestAuxReadMainReadersExportTheConfiguredRoute(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("model:\n  provider: openrouter\n  default: acme/forge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := AuxReadMainProvider(ctx); got != "openrouter" {
		t.Fatalf("provider %q", got)
	}
	if got := AuxReadMainModel(ctx); got != "acme/forge" {
		t.Fatalf("model %q", got)
	}
}
