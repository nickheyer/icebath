package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTitleTimeoutHitsTheProviderOnceAndNamesTheDeadline(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	auxFixedTaskRoute(t, auxTaskRoute{Provider: "mac-ollama", Model: "qwen3.6:27b"})
	primary := auxScriptedCallClient("http://100.121.173.79:11434/v1", "k", &AuxSDKError{Class: "APITimeoutError", Message: "Request timed out."})
	auxSwapCachedClient(t, primary, "qwen3.6:27b")
	prevValidate := auxValidateLLMResponseFn
	t.Cleanup(func() { auxValidateLLMResponseFn = prevValidate })
	auxValidateLLMResponseFn = func(_ context.Context, response any, _, _, _ string) (any, error) { return response, nil }
	auxSwapFallbackSeams(t)
	none := func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
	auxTryConfiguredFallbackChainFn = none
	prevMainAgent := auxTryMainAgentModelFallbackFn
	t.Cleanup(func() { auxTryMainAgentModelFallbackFn = prevMainAgent })
	auxTryMainAgentModelFallbackFn = none
	logs := auxCaptureLogs(t)
	_, err := CallLLM(ctx, map[string]any{"task": "title_generation", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "timeout": 30})
	if err == nil || !auxExcIsSDKClass(err, "APITimeoutError") {
		t.Fatalf("timeout must surface once every fallback is exhausted, got %v", err)
	}
	if auxCallCount(primary) != 1 {
		t.Fatalf("provider hit %d times", auxCallCount(primary))
	}
	var warnings, timedOut []string
	for _, rec := range logs.matching("") {
		if rec.level == slog.LevelWarn {
			warnings = append(warnings, rec.msg)
			if strings.Contains(rec.msg, "timed out after 30s") {
				timedOut = append(timedOut, rec.msg)
			}
		}
	}
	if len(timedOut) == 0 {
		t.Fatalf("warnings %v", warnings)
	}
	if !strings.Contains(timedOut[0], "http://100.121.173.79:11434/v1") || !strings.Contains(timedOut[0], "auxiliary.title_generation.timeout") {
		t.Fatalf("timeout warning %q", timedOut[0])
	}
	for _, m := range warnings {
		if strings.Contains(m, "connection error on") {
			t.Fatalf("timeout logged as a connection error: %v", warnings)
		}
	}
}

func TestUnknownProviderDedupUnknownProviderRepeatIsSuppressed(t *testing.T) {
	ctx := auxResolveHome(t, "")
	auxLoggedUnknownProviderKeys.clear()
	t.Cleanup(auxLoggedUnknownProviderKeys.clear)
	logs := auxCaptureLogs(t)
	for i := 0; i < 3; i++ {
		if _, _, err := ResolveProviderClient(ctx, "no_such_provider_xyz", "", ProviderClientOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if recs := logs.matching("unknown provider"); len(recs) != 1 {
		t.Fatalf("three calls must leave one record, got %d", len(recs))
	}
}

func TestCrossLoopCacheIsolationDifferentLoopsGetDifferentClients(t *testing.T) {
	auxIsolateClientCache(t)
	ctx := auxMainTestHome(t, "")
	prev := auxResolveProviderClientFn
	t.Cleanup(func() { auxResolveProviderClientFn = prev })
	auxResolveProviderClientFn = func(_ context.Context, _, model string, opts ProviderClientOptions) (AuxClient, string, error) {
		if model == "" {
			model = "test-model"
		}
		return &auxCloseCounter{auxBareClient: auxBareClient{base: opts.ExplicitBaseURL, key: "test"}}, model, nil
	}
	results := map[string]AuxClient{}
	loops := map[string]*auxTestLoop{}
	var mu sync.Mutex
	for _, name := range []string{"a", "b"} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			loop := &auxTestLoop{}
			client, _, err := auxGetCachedClient(WithAuxiliaryEventLoop(ctx, loop), auxCachedClientRequest{Provider: "custom", Model: "m1", AsyncMode: true, BaseURL: "http://localhost:8081/v1"})
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			results[name], loops[name] = client, loop
			mu.Unlock()
		}()
		<-done
	}
	if loops["a"] == loops["b"] {
		t.Fatal("Test setup error: same loop on both threads")
	}
	if results["a"] == nil || results["a"] == results["b"] {
		t.Fatal("Different event loops got the SAME cached client — this causes httpx cross-loop deadlocks in gateway mode (#2681)")
	}
}

func TestOllamaLaneWithBareLocalHostResolvesCustomClientUnderV1(t *testing.T) {
	for _, tc := range []struct{ apiKey, envOpenAIKey, expectedKey string }{
		{"", "", "no-key-required"}, {"test-key-123", "", "test-key-123"}, {"", "sk-USER-OPENAI-SECRET", "no-key-required"},
	} {
		auxIsolateClientCache(t)
		auxUnsetEnv(t, "OPENAI_API_KEY", "OPENAI_BASE_URL")
		prev := auxReadMainAPIKeyIfSameOriginFn
		t.Cleanup(func() { auxReadMainAPIKeyIfSameOriginFn = prev })
		auxReadMainAPIKeyIfSameOriginFn = func(context.Context, string) (any, error) { return nil, nil }
		if tc.envOpenAIKey != "" {
			t.Setenv("OPENAI_API_KEY", tc.envOpenAIKey)
			auxReadMainAPIKeyIfSameOriginFn = func(context.Context, string) (any, error) { return "sk-MAIN-SECRET", nil }
		}
		ctx := auxMainTestHome(t, fmt.Sprintf("auxiliary:\n  title_generation:\n    provider: ollama\n    model: qwen3.8:27b\n    base_url: http://127.0.0.1:11434\n    api_key: %q\n", tc.apiKey))
		route := auxResolveTaskProviderModel(ctx, "title_generation", "", "", "", nil)
		client, _, err := ResolveProviderClient(ctx, route.Provider, route.Model, ProviderClientOptions{ExplicitBaseURL: route.BaseURL, ExplicitAPIKey: route.APIKey})
		if err != nil || client == nil {
			t.Fatalf("%+v: provider: ollama + local base_url must build a client via the custom branch, got (%v, %v)", tc, client, err)
		}
		if base := strings.TrimRight(client.BaseURL(), "/"); base != "http://127.0.0.1:11434/v1" || client.APIKey() != tc.expectedKey {
			t.Fatalf("%+v: base %q key %q", tc, base, client.APIKey())
		}
	}
}

func TestResolveProviderClientKimiCodingWrapsAnthropic(t *testing.T) {
	auxIsolateClientCache(t)
	auxUnsetEnv(t, "OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "KIMI_API_KEY", "KIMI_CODING_API_KEY", "KIMI_BASE_URL")
	ctx := auxMainTestHome(t, "")
	t.Setenv("KIMI_API_KEY", "sk-kimi-faketesttoken123")
	client, _, err := ResolveProviderClient(ctx, "kimi-coding", "kimi-for-coding", ProviderClientOptions{})
	if err != nil || client == nil {
		t.Fatalf("Should resolve a client, got (%v, %v)", client, err)
	}
	ac, ok := client.(*AnthropicAuxiliaryClient)
	if !ok {
		t.Fatalf("Kimi Coding Plan endpoint (api.kimi.com/coding) speaks Anthropic Messages — aux client MUST be AnthropicAuxiliaryClient, got %T", client)
	}
	if !strings.Contains(ac.BaseURL(), "kimi.com/coding") {
		t.Fatalf("base %q", ac.BaseURL())
	}
}

// Cancel source answering the stop flag
type auxStopCheck struct{ stop *atomic.Bool }

func (c auxStopCheck) CancelRequested() (bool, error) { return c.stop.Load(), nil }

func auxSSEChunk(text, finish string) string {
	delta := "{}"
	if text != "" {
		delta = fmt.Sprintf(`{"content": %q}`, text)
	}
	finishJSON := "null"
	if finish != "" {
		finishJSON = fmt.Sprintf("%q", finish)
	}
	return fmt.Sprintf("data: {\"id\": \"c\", \"object\": \"chat.completion.chunk\", \"created\": 1, \"model\": \"m\", \"choices\": [{\"index\": 0, \"delta\": %s, \"finish_reason\": %s}]}\n\n", delta, finishJSON)
}

func TestHardCancelClosesTheStreamedSummaryAtItsNextChunk(t *testing.T) {
	auxIsolateClientCache(t)
	auxClearUnhealthy(t)
	sentSome, clientClosed := make(chan struct{}), make(chan struct{})
	var sentOnce, closedOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for n := 0; n < 600; n++ {
			if _, err := w.Write([]byte(auxSSEChunk("word ", ""))); err != nil {
				closedOnce.Do(func() { close(clientClosed) })
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if n == 3 {
				sentOnce.Do(func() { close(sentSome) })
			}
			select {
			case <-r.Context().Done():
				closedOnce.Do(func() { close(clientClosed) })
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	ctx := auxMainTestHome(t, "auxiliary:\n  compression:\n    provider: custom\n    base_url: "+srv.URL+"/v1\n    api_key: dummy\n    model: slow-summarizer\n")
	var stop atomic.Bool
	go func() {
		select {
		case <-sentSome:
			stop.Store(true)
		case <-time.After(10 * time.Second):
		}
	}()
	ctx = AuxProgressHook(ctx, func() error { return nil })
	ctx = AuxInterruptProtection(ctx, true, auxStopCheck{&stop}, nil)
	timeout := 20.0
	_, err := auxCallLLM(ctx, auxCallRequest{Task: "compression", Messages: []any{map[string]any{"role": "user", "content": "Summarize."}}, Timeout: &timeout})
	var cancelled *AuxiliaryExplicitCancellation
	if !errors.As(err, &cancelled) {
		t.Fatalf("want AuxiliaryExplicitCancellation, got %v", err)
	}
	if !stop.Load() {
		t.Fatal("the stop flag was never raised")
	}
	select {
	case <-clientClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled summary stream kept streaming after the hard stop")
	}
}
