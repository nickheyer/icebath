package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func auxClearUnhealthy(t *testing.T) {
	t.Helper()
	auxUnhealthyMu.Lock()
	clear(auxUnhealthyUntil)
	auxUnhealthyMu.Unlock()
	t.Cleanup(func() {
		auxUnhealthyMu.Lock()
		clear(auxUnhealthyUntil)
		auxUnhealthyMu.Unlock()
	})
}

func TestAuxExpiredCodexOpenRouterWins(t *testing.T) {
	auxClearUnhealthy(t)
	enc := base64.RawURLEncoding
	payload, _ := json.Marshal(map[string]any{"exp": time.Now().Unix() - 3600})
	expired := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(payload) + ".fakesig"
	h := filepath.Join(t.TempDir(), "icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	auth, _ := json.Marshal(map[string]any{"version": 1, "providers": map[string]any{"openai-codex": map[string]any{
		"tokens": map[string]any{"access_token": expired, "refresh_token": "r"}}}})
	if err := os.WriteFile(filepath.Join(h, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("OPENROUTER_API_KEY", "or-test-key")
	client, _, _, err := auxResolveAutoRoute(context.Background(), nil, "")
	if err != nil || client == nil {
		t.Fatalf("client %v err %v", client, err)
	}
	if !strings.Contains(client.BaseURL(), "openrouter.ai") || client.APIKey() != "or-test-key" {
		t.Fatalf("OpenRouter is first in the chain and must win, got %s", client.BaseURL())
	}
}

func TestAuxTryPaymentFallbackSkipsFailedProvider(t *testing.T) {
	nous := auxNousIsTheOnlyWorkingProvider(t, "auto")
	client, model, label, err := auxTryPaymentFallback(context.Background(), "openrouter", "compression", "payment error", "", FailureScope(""), nil)
	if err != nil || client != nous || model != "nous-model" || label != "nous" {
		t.Fatalf("client %v model %q label %q err %v", client, model, label, err)
	}
}

func TestAuxPaymentFallbackSkipsUnhealthy(t *testing.T) {
	auxSwapFallbackSeams(t)
	auxClearUnhealthy(t)
	ctx := context.Background()
	auxMarkProviderUnhealthy(ctx, "local/custom", 600, "", "", slog.LevelInfo)
	nous := &auxCallFakeClient{base: "https://inference-api.nousresearch.com/v1"}
	var orCalls, customCalls atomic.Int32
	auxProviderChainFn = func() []auxProviderChainEntry {
		return []auxProviderChainEntry{
			{"openrouter", func(context.Context) (AuxClient, string, error) { orCalls.Add(1); return nil, "", nil }},
			{"nous", func(context.Context) (AuxClient, string, error) { return nous, "n-model", nil }},
			{"local/custom", func(context.Context) (AuxClient, string, error) { customCalls.Add(1); return nil, "", nil }},
			{"api-key", func(context.Context) (AuxClient, string, error) { return nil, "", nil }},
		}
	}
	auxReadMainProvider = func(context.Context) string { return "auto" }
	client, _, label, err := auxTryPaymentFallback(ctx, "openrouter", "compression", "payment error", "", FailureScope(""), nil)
	if err != nil || client != nous || label != "nous" {
		t.Fatalf("client %v label %q err %v", client, label, err)
	}
	if orCalls.Load() != 0 || customCalls.Load() != 0 {
		t.Fatalf("failed provider and unhealthy custom must be skipped, openrouter %d custom %d", orCalls.Load(), customCalls.Load())
	}
}

func TestAuxFallbackEntryOpenAICodexUsesOAuthPoolWithoutInlineKey(t *testing.T) {
	auxClearUnhealthy(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := auxSelectPoolEntryFn
	t.Cleanup(func() { auxSelectPoolEntryFn = saved })
	auxSelectPoolEntryFn = func(_ context.Context, provider string) (bool, *PooledCredential) {
		if provider != "openai-codex" {
			return false, nil
		}
		return true, &PooledCredential{ID: "codex-pool-1", Provider: "openai-codex", AccessToken: "codex-oauth-token",
			BaseURL: "https://chatgpt.com/backend-api/codex"}
	}
	client, model, err := auxResolveFallbackEntry(context.Background(), map[string]any{"provider": "openai-codex", "model": "gpt-5.4-mini"})
	if err != nil || client == nil || model != "gpt-5.4-mini" {
		t.Fatalf("client %v model %q err %v", client, model, err)
	}
	if client.APIKey() != "codex-oauth-token" {
		t.Fatalf("the pool token must reach the client, got %q", client.APIKey())
	}
}

func TestAuxMainAgentModelFallbackNoneWhenMainProviderAuto(t *testing.T) {
	auxSwapFallbackSeams(t)
	savedModel := auxReadMainModel
	t.Cleanup(func() { auxReadMainModel = savedModel })
	auxReadMainProvider = func(context.Context) string { return "auto" }
	auxReadMainModel = func(context.Context) string { return "some-model" }
	client, model, label, err := auxTryMainAgentModelFallback(context.Background(), "glm", "vision", "", "", "", FailureScope(""))
	if err != nil || client != nil || model != "" || label != "" {
		t.Fatalf("client %v model %q label %q err %v", client, model, label, err)
	}
}

func TestTryPaymentFallbackCodexNotInFallbackChain(t *testing.T) {
	ctx := context.Background()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{"model": map[string]any{"provider": "auto"}})
	auxClearUnhealthy(t)
	prev := auxProviderChainFn
	t.Cleanup(func() { auxProviderChainFn = prev })
	var tried []string
	auxProviderChainFn = func() []auxProviderChainEntry {
		var out []auxProviderChainEntry
		for _, entry := range prev() {
			label := entry.label
			out = append(out, auxProviderChainEntry{label, func(context.Context) (AuxClient, string, error) {
				tried = append(tried, label)
				return nil, "", nil
			}})
		}
		return out
	}
	client, model, label, err := auxTryPaymentFallback(ctx, "openrouter", "", "", "", FailureScope(""), nil)
	if err != nil || client != nil || model != "" || label != "" {
		t.Fatalf("payment fallback returned %v %q %q %v", client, model, label, err)
	}
	if fmt.Sprint(tried) != "[nous local/custom api-key]" {
		t.Fatalf("payment fallback walked %v, Codex must never be a rung", tried)
	}
}

func TestCallLLMUnwrapsDataEnvelope(t *testing.T) {
	for _, asyncMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%v", asyncMode), func(t *testing.T) {
			ctx := context.Background()
			ShutdownCachedClients(ctx)
			t.Cleanup(func() { ShutdownCachedClients(ctx) })
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": nil, "success": true, "data": map[string]any{
					"id": "chatcmpl-env", "object": "chat.completion", "created": 1751000000, "model": "cline-pass/kimi-k2.7-code",
					"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "unwrapped"}, "finish_reason": "stop"}},
					"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}})
			}))
			t.Cleanup(srv.Close)
			task := "title_generation"
			call := CallLLM
			if asyncMode {
				task, call = "vision", AsyncCallLLM
			}
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			writeConfig(t, h, map[string]any{"auxiliary": map[string]any{task: map[string]any{
				"provider": "custom", "model": "cline-pass/kimi-k2.7-code", "base_url": srv.URL + "/v1", "api_key": "k"}}})
			prev := auxTryConfiguredFallbackChainFn
			t.Cleanup(func() { auxTryConfiguredFallbackChainFn = prev })
			auxTryConfiguredFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
				t.Error("an unwrapped envelope must not reach the fallback chain")
				return nil, "", "", nil
			}
			resp, err := call(ctx, map[string]any{"task": task, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
			if err != nil {
				t.Fatal(err)
			}
			if got := auxActualContent(t, resp); got != "unwrapped" {
				t.Fatalf("content %q", got)
			}
		})
	}
}
