package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Records the level of every log record while installed as the default handler
type auxLevelRecorder struct {
	mu     sync.Mutex
	levels []slog.Level
}

func (r *auxLevelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *auxLevelRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	r.levels = append(r.levels, rec.Level)
	r.mu.Unlock()
	return nil
}
func (r *auxLevelRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *auxLevelRecorder) WithGroup(string) slog.Handler      { return r }

// True when one captured record reached the warning level
func (r *auxLevelRecorder) sawWarning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, level := range r.levels {
		if level >= slog.LevelWarn {
			return true
		}
	}
	return false
}

// Responses client fake that counts its direct create calls
type auxCountingResponsesClient struct {
	auxFakeResponsesClient
	calls int
}

func (c *auxCountingResponsesClient) CreateResponse(ctx context.Context, kwargs map[string]any) (any, error) {
	c.calls++
	return c.auxFakeResponsesClient.CreateResponse(ctx, kwargs)
}

// Swaps the task route resolver for one fixed provider and model
func auxSwapTaskRoute(t *testing.T, provider, model string) {
	t.Helper()
	prev := auxResolveTaskProviderModelFn
	auxResolveTaskProviderModelFn = func(context.Context, any, string, string, string, any) auxTaskRoute {
		return auxTaskRoute{Provider: provider, Model: model}
	}
	t.Cleanup(func() { auxResolveTaskProviderModelFn = prev })
}

func TestNoProgressTimeoutTaskConfigGatingNonCodexClientNeverReceivesTheKwarg(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{"auxiliary": map[string]any{"compression": map[string]any{"no_progress_timeout": 300}}})
	config.ResetCache()
	client := &auxCallFakeClient{base: "https://api.openai.com/v1", key: "k",
		create: func(context.Context, map[string]any) (any, error) { return auxCallOK("ok"), nil }}
	auxSwapTaskRoute(t, "openai", "gpt-4.1")
	auxSwapCachedClient(t, client, "gpt-4.1")
	if _, err := CallLLM(context.Background(), map[string]any{"task": "compression",
		"messages": []any{map[string]any{"role": "user", "content": "summarize"}}}); err != nil {
		t.Fatal(err)
	}
	if len(client.sent) == 0 {
		t.Fatal("the client was never called")
	}
	if _, has := client.sent[len(client.sent)-1]["no_progress_timeout"]; has {
		t.Fatalf("kwargs %v", client.sent[len(client.sent)-1])
	}
}

func TestNoProgressTimeoutTaskConfigGatingRealConfigValueReachesTheStreamGuardPerTask(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	stop := errors.New("guard start intercepted")
	run := func(task string) float64 {
		captured := make(chan float64, 1)
		prevBegin := auxCodexGuardBeginFn
		auxCodexGuardBeginFn = func(g *auxCodexStreamGuard) error {
			select {
			case captured <- g.noProgressTimeout:
			default:
			}
			return stop
		}
		defer func() { auxCodexGuardBeginFn = prevBegin }()
		client := NewCodexAuxiliaryClient(&auxFakeResponsesClient{base: "https://chatgpt.com/backend-api/codex/"}, "gpt-5.6-sol")
		prevCached := auxGetCachedClientFn
		auxGetCachedClientFn = func(context.Context, auxCachedClientRequest) (AuxClient, string, error) {
			return client, "gpt-5.6-sol", nil
		}
		defer func() { auxGetCachedClientFn = prevCached }()
		_, _ = CallLLM(context.Background(), map[string]any{"task": task, "provider": "openai-codex", "model": "gpt-5.6-sol",
			"messages": []any{map[string]any{"role": "user", "content": "summarize"}}})
		select {
		case window := <-captured:
			return window
		default:
			t.Fatal("the stream guard never started")
			return 0
		}
	}
	writeConfig(t, h, map[string]any{"auxiliary": map[string]any{"compression": map[string]any{"timeout": 600, "no_progress_timeout": 5}}})
	config.ResetCache()
	if got := run("compression"); got != 5.0 {
		t.Fatalf("compression window %v", got)
	}
	writeConfig(t, h, map[string]any{"auxiliary": map[string]any{
		"compression":      map[string]any{"timeout": 600, "no_progress_timeout": 5},
		"title_generation": map[string]any{"timeout": 600}}})
	config.ResetCache()
	if got := run("title_generation"); got != 60.0 {
		t.Fatalf("title_generation window %v", got)
	}
	writeConfig(t, h, map[string]any{"auxiliary": map[string]any{"compression": map[string]any{"timeout": 600, "no_progress_timeout": -3}}})
	config.ResetCache()
	recorder := &auxLevelRecorder{}
	prevLog := slog.Default()
	slog.SetDefault(slog.New(recorder))
	t.Cleanup(func() { slog.SetDefault(prevLog) })
	if got := run("compression"); got != 60.0 {
		t.Fatalf("rejected window %v", got)
	}
	if !recorder.sawWarning() {
		t.Fatal("a non positive no_progress_timeout must log a warning")
	}
}

func TestMoaAggregatorStreamingBypassMoaAggregatorStreamBypassesRelayForCodexAuxiliaryClient(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	real := &auxCountingResponsesClient{auxFakeResponsesClient: auxFakeResponsesClient{base: "https://chatgpt.com/backend-api/codex/"}}
	client := NewCodexAuxiliaryClient(real, "gpt-5.6-sol")
	auxSwapCachedClient(t, client, "gpt-5.6-sol")
	prev := auxRelaySyncStreamFn
	auxRelaySyncStreamFn = func(context.Context, AuxClient, map[string]any, string, string) (any, error) {
		return nil, errors.New("_relay_sync_stream must not be used")
	}
	t.Cleanup(func() { auxRelaySyncStreamFn = prev })
	result, err := CallLLM(context.Background(), map[string]any{"task": "moa_aggregator", "provider": "openai-codex", "model": "gpt-5.6-sol",
		"messages": []any{map[string]any{"role": "user", "content": "只回答 OK"}}, "stream": true})
	if err != nil {
		t.Fatal(err)
	}
	if real.calls != 1 {
		t.Fatalf("direct create calls %d", real.calls)
	}
	if got := auxActualContent(t, result); got != "summary text" {
		t.Fatalf("result %v", result)
	}
}

func TestAsynchronousFallbackCachePlansAsyncFallbackReplansCacheSectionsLikeSync(t *testing.T) {
	entry := map[string]any{"provider": "anthropic", "model": "claude-sonnet-4-6", "base_url": "https://api.anthropic.com", "api_mode": "anthropic_messages"}
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{"auxiliary": map[string]any{"moa_aggregator": map[string]any{"fallback_chain": []any{entry}}}})
	config.ResetCache()
	ctx := context.Background()
	client := &auxCallFakeClient{base: "https://api.anthropic.com", create: func(context.Context, map[string]any) (any, error) { return auxCallOK("ok"), nil }}
	prev := auxResolveProviderClientFn
	t.Cleanup(func() { auxResolveProviderClientFn = prev })
	auxResolveProviderClientFn = func(_ context.Context, _, model string, _ ProviderClientOptions) (AuxClient, string, error) {
		return client, model, nil
	}
	fbClient, fbModel, label, err := auxTryConfiguredFallbackChain(ctx, "moa_aggregator", "primary", "", "", "", "")
	if err != nil || fbClient == nil {
		t.Fatalf("fallback client %v err %v", fbClient, err)
	}
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}
	if _, err := auxCallFallbackCandidate(ctx, fbClient, fbModel, label, auxFallbackRequest{Task: "moa_aggregator",
		Messages: []map[string]any{{"role": "system", "content": "stable prefix"}, {"role": "user", "content": "lookup"}},
		Tools:    tools, EffectiveTimeout: 30, EffectiveExtraBody: map[string]any{}}, true); err != nil {
		t.Fatal(err)
	}
	if len(client.sent) == 0 {
		t.Fatal("the fallback client was never called")
	}
	wireTools := auxDictList(client.sent[len(client.sent)-1]["tools"])
	if _, has := wireTools[len(wireTools)-1]["cache_control"]; !has {
		t.Fatalf("async fallback tools carry the marker %v", wireTools)
	}
	if _, has := tools[len(tools)-1]["cache_control"]; has {
		t.Fatal("caller tools must stay unmarked")
	}
}

func TestAutoRoutedProviderProfileHooksCachedAutoRouteProjectsSelectedProviderOnEveryRequest(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	var hookCalls []map[string]any
	profile := Profile{Name: "agentgateway", BuildAPIKwargsExtrasHook: func(_ *Reasoning, hookCtx map[string]any) (map[string]any, map[string]any) {
		hookCalls = append(hookCalls, hookCtx)
		return map[string]any{}, map[string]any{"extra_headers": map[string]any{"Authorization": fmt.Sprintf("Bearer token-%d", len(hookCalls))}}
	}}
	client := &auxCallFakeClient{base: "https://gateway.example.com/v1", key: "k",
		create: func(context.Context, map[string]any) (any, error) { return auxCallOK("ok"), nil }}
	response := auxCallOK("ok")
	ShutdownCachedClients(ctx)
	t.Cleanup(func() { ShutdownCachedClients(ctx) })
	auxSwapTaskRoute(t, "auto", "gateway/model")
	autoCalls := 0
	prevAuto, prevProfile, prevRelay := auxResolveAutoRouteFn, auxGetProviderProfileFn, auxRelaySyncCompletionFn
	t.Cleanup(func() {
		auxResolveAutoRouteFn, auxGetProviderProfileFn, auxRelaySyncCompletionFn = prevAuto, prevProfile, prevRelay
	})
	auxResolveAutoRouteFn = func(context.Context, map[string]any, string) (AuxClient, string, string, error) {
		autoCalls++
		return client, "gateway/model", "agentgateway", nil
	}
	auxGetProviderProfileFn = func(_ context.Context, name string) (Profile, bool) {
		if name == "agentgateway" {
			return profile, true
		}
		return Profile{}, false
	}
	var relayKwargs []map[string]any
	auxRelaySyncCompletionFn = func(_ context.Context, _ AuxClient, kwargs map[string]any, _, _ string, _ func(context.Context, map[string]any) (any, error)) (any, error) {
		relayKwargs = append(relayKwargs, kwargs)
		return response, nil
	}
	for i := 0; i < 2; i++ {
		result, err := CallLLM(ctx, map[string]any{"task": "title_generation", "messages": []any{map[string]any{"role": "user", "content": "title this"}}})
		if err != nil {
			t.Fatal(err)
		}
		if reflect.ValueOf(result).Pointer() != reflect.ValueOf(response).Pointer() {
			t.Fatalf("call %d returned %v", i, result)
		}
	}
	if autoCalls != 1 {
		t.Fatalf("auto route resolved %d times", autoCalls)
	}
	if len(hookCalls) != 2 {
		t.Fatalf("hook calls %d", len(hookCalls))
	}
	for i, want := range []string{"Bearer token-1", "Bearer token-2"} {
		if !reflect.DeepEqual(relayKwargs[i]["extra_headers"], map[string]any{"Authorization": want}) {
			t.Fatalf("request %d extra_headers %v", i, relayKwargs[i]["extra_headers"])
		}
	}
}
