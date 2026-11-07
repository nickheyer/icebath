package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

type auxCallFakeClient struct {
	auxEffectiveStamp
	base   string
	key    string
	create func(ctx context.Context, kwargs map[string]any) (any, error)
	mu     sync.Mutex
	sent   []map[string]any
	closed atomic.Bool
}

func (c *auxCallFakeClient) BaseURL() string { return c.base }
func (c *auxCallFakeClient) APIKey() string  { return c.key }
func (c *auxCallFakeClient) Close() error    { c.closed.Store(true); return nil }

func (c *auxCallFakeClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	c.mu.Lock()
	c.sent = append(c.sent, kwargs)
	c.mu.Unlock()
	return c.create(ctx, kwargs)
}

// Chat completion dict holding one assistant message
func auxCallOK(text string) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}}
}

// Restores every fallback seam and the main provider reader after the test
func auxSwapFallbackSeams(t *testing.T) {
	t.Helper()
	route, configured, mainChain, payment, chain, readProvider := auxTryMainProviderRouteFn, auxTryConfiguredFallbackChainFn,
		auxTryMainFallbackChainFn, auxTryPaymentFallbackFn, auxProviderChainFn, auxReadMainProvider
	t.Cleanup(func() {
		auxTryMainProviderRouteFn, auxTryConfiguredFallbackChainFn, auxTryMainFallbackChainFn = route, configured, mainChain
		auxTryPaymentFallbackFn, auxProviderChainFn, auxReadMainProvider = payment, chain, readProvider
	})
}

// Main provider unusable, Nous winning discovery and no configured fallback policy
func auxNousIsTheOnlyWorkingProvider(t *testing.T, persisted string) *auxCallFakeClient {
	auxSwapFallbackSeams(t)
	auxUnhealthyMu.Lock()
	clear(auxUnhealthyUntil)
	auxUnhealthyMu.Unlock()
	nous := &auxCallFakeClient{base: "https://inference-api.nousresearch.com/v1"}
	none := func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return nil, "", "", nil
	}
	auxTryMainProviderRouteFn = func(context.Context, auxMainTarget) (AuxClient, string, string, error) { return nil, "", "", nil }
	auxTryConfiguredFallbackChainFn, auxTryMainFallbackChainFn = none, none
	auxProviderChainFn = func() []auxProviderChainEntry {
		return []auxProviderChainEntry{
			{"openrouter", func(context.Context) (AuxClient, string, error) { return nil, "", nil }},
			{"nous", func(context.Context) (AuxClient, string, error) { return nous, "nous-model", nil }},
		}
	}
	auxReadMainProvider = func(context.Context) string { return persisted }
	return nous
}

func TestAuxSelectedMainProviderDownRefusesToGuessAnotherAccount(t *testing.T) {
	for _, persisted := range []string{"xai-oauth", "auto"} {
		t.Run(persisted, func(t *testing.T) {
			auxNousIsTheOnlyWorkingProvider(t, persisted)
			ctx := context.Background()
			runtime := map[string]any{"provider": "xai-oauth", "model": "grok-4.6", "base_url": "https://api.x.ai/v1", "api_key": "dead"}
			if client, model, label, err := auxResolveAutoRoute(ctx, runtime, "compression"); client != nil || model != "" || label != "" || err != nil {
				t.Fatalf("auto route guessed %v %q %q %v", client, model, label, err)
			}
			if client, model, label, err := auxTryPaymentFallback(ctx, "xai-oauth", "compression", "payment error", "", "", runtime); client != nil || model != "" || label != "" || err != nil {
				t.Fatalf("payment fallback guessed %v %q %q %v", client, model, label, err)
			}
		})
	}
}

func TestAuxQuarantinedFallbackHandsOverToTheNextConfiguredEntry(t *testing.T) {
	auxSwapFallbackSeams(t)
	healthy := &auxCallFakeClient{}
	auxTryConfiguredFallbackChainFn = func(context.Context, string, string, string, string, string, FailureScope) (AuxClient, string, string, error) {
		return healthy, "m2", "fallback_chain[1](nous)", nil
	}
	discovery := 0
	auxTryPaymentFallbackFn = func(context.Context, string, string, string, string, FailureScope, map[string]any) (AuxClient, string, string, error) {
		discovery++
		return nil, "", "", nil
	}
	route := auxLadderRoute{Task: "compression", ResolvedProvider: "xai-oauth", MainRuntime: map[string]any{"provider": "xai-oauth"}}
	client, _, label, err := auxNextFallbackAfterQuarantine(context.Background(), "compression", "auto", true, route, "", "", false)
	if err != nil || client != AuxClient(healthy) || label != "fallback_chain[1](nous)" {
		t.Fatalf("next fallback %v %q %v", client, label, err)
	}
	if discovery != 0 {
		t.Fatalf("discovery called %d times", discovery)
	}
}

func TestAuxNoSelectedMainProviderStillDiscovers(t *testing.T) {
	nous := auxNousIsTheOnlyWorkingProvider(t, "auto")
	client, model, label, err := auxResolveAutoRoute(context.Background(), map[string]any{"provider": "auto"}, "compression")
	if err != nil || client != AuxClient(nous) || model != "nous-model" || label != "nous" {
		t.Fatalf("discovery %v %q %q %v", client, model, label, err)
	}
}

// Fake client rejecting with a 400 while any listed field is still on the wire
func auxRejectingClient(fields ...string) *auxCallFakeClient {
	phrases := map[string]string{
		"reasoning":        "Extra inputs are not permitted, field: 'reasoning'",
		"reasoning_effort": "Unsupported value: 'reasoning_effort' does not support 'none' with this model.",
		"temperature":      "Unsupported value: 'temperature' does not support 0.3 with this model.",
		"max_tokens":       "Unsupported parameter: 'max_tokens' is not supported with this model.",
	}
	return &auxCallFakeClient{base: "https://api.example/v1", create: func(_ context.Context, kwargs map[string]any) (any, error) {
		body := map[string]any{}
		for k, v := range kwargs {
			if k != "extra_body" {
				body[k] = v
			}
		}
		if extra, ok := kwargs["extra_body"].(map[string]any); ok {
			for k, v := range extra {
				body[k] = v
			}
		}
		for _, f := range fields {
			if _, ok := body[f]; ok {
				return nil, &AuxSDKError{StatusCode: 400, Message: "Error code: 400 - " + phrases[f]}
			}
		}
		return auxCallOK("ok"), nil
	}}
}

func TestAuxPrimaryRungsChainInProviderOrderAndStripEachFieldOnce(t *testing.T) {
	auxFreshFloors(t)
	client := auxRejectingClient("reasoning_effort", "temperature", "max_tokens")
	kwargs := map[string]any{"model": "m", "messages": []any{}, "temperature": 0.3, "max_tokens": 64, "reasoning_effort": "none"}
	route := auxLadderRoute{Client: client, Task: "title_generation"}
	firstErr := &AuxSDKError{StatusCode: 400, Message: "Error code: 400 - Unsupported value: 'reasoning_effort' does not support 'none'"}
	var finalKwargs map[string]any
	var narrowed error
	resp, err := auxDriveLadder(context.Background(), func(yield auxLadderYield) (any, error) {
		resp, n, kw, raised := auxLadderParameterRungs(yield, firstErr, route, kwargs, 64)
		finalKwargs, narrowed = kw, n
		return resp, raised
	}, func(ctx context.Context, step auxLadderStep) (any, error) {
		return step.Args[0].(AuxClient).CreateChatCompletion(ctx, step.Args[1].(map[string]any))
	})
	if err != nil || narrowed != nil || ExtractContentOrReasoning(resp, -1) != "ok" {
		t.Fatalf("ladder %v %v %v", resp, narrowed, err)
	}
	for _, f := range []string{"temperature", "max_tokens", "reasoning_effort"} {
		if _, ok := finalKwargs[f]; ok {
			t.Fatalf("%s still sent %v", f, finalKwargs)
		}
	}
	if len(client.sent) != 3 {
		t.Fatalf("retries %d", len(client.sent))
	}
}

func TestAuxDriveLadderFeedsResultsBackAndSleepsWaitSteps(t *testing.T) {
	performed := []string{}
	started := time.Now()
	resp, err := auxDriveLadder(context.Background(), func(yield auxLadderYield) (any, error) {
		if _, err := yield(auxLadderStep{Kind: "wait", Args: []any{0.05}}); err != nil {
			return nil, err
		}
		_, accepted, raised := auxRung(yield, auxLadderStep{Kind: "call", Args: []any{"boom"}}, auxCredentialRungAccepts)
		if raised != nil || accepted == nil {
			return nil, errors.New("rate limit was not accepted")
		}
		resp, _, raised := auxRung(yield, auxLadderStep{Kind: "call", Args: []any{"ok"}}, auxCredentialRungAccepts)
		return resp, raised
	}, func(_ context.Context, step auxLadderStep) (any, error) {
		performed = append(performed, step.Args[0].(string))
		if step.Args[0] == "boom" {
			return nil, &AuxSDKError{StatusCode: 429, Message: "rate limited"}
		}
		return "done", nil
	})
	if err != nil || resp != "done" || strings.Join(performed, ",") != "boom,ok" || time.Since(started) < 50*time.Millisecond {
		t.Fatalf("ladder %v %v %v", resp, err, performed)
	}
	_, _, raised := auxRung(func(auxLadderStep) (any, error) { return nil, errors.New("plain") }, auxLadderStep{Kind: "call"}, auxCredentialRungAccepts)
	if raised == nil {
		t.Fatal("an unaccepted error must be raised")
	}
}

type auxTestCancelEvent struct{ set atomic.Bool }

func (e *auxTestCancelEvent) IsSet() bool { return e.set.Load() }

func TestAuxCancelledAttemptDoesNotCloseOrFailConcurrentSharedClientCall(t *testing.T) {
	aStarted, aRelease := make(chan struct{}), make(chan struct{})
	bStarted, bRelease := make(chan struct{}), make(chan struct{})
	var aOnce, bOnce sync.Once
	client := &auxCallFakeClient{}
	client.create = func(_ context.Context, kwargs map[string]any) (any, error) {
		if kwargs["model"] == "session-a" {
			aOnce.Do(func() { close(aStarted) })
			select {
			case <-aRelease:
			case <-time.After(5 * time.Second):
			}
		} else {
			bOnce.Do(func() { close(bStarted) })
			select {
			case <-bRelease:
			case <-time.After(5 * time.Second):
			}
		}
		if client.closed.Load() {
			return nil, errors.New("shared client was closed")
		}
		return auxCallOK("ok"), nil
	}
	cancel := &auxTestCancelEvent{}
	outcomes := sync.Map{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ctx := AuxInterruptProtection(context.Background(), true, nil, cancel)
		_, err := auxRelaySyncCompletion(ctx, client, map[string]any{"model": "session-a", "messages": []any{}, "timeout": 30}, "", "", nil)
		outcomes.Store("a", err)
	}()
	bDone := make(chan struct{})
	go func() {
		defer wg.Done()
		defer close(bDone)
		resp, err := auxRelaySyncCompletion(context.Background(), client, map[string]any{"model": "session-b", "messages": []any{}, "timeout": 30}, "", "", nil)
		outcomes.Store("b", resp)
		if err != nil {
			outcomes.Store("b", err)
		}
	}()
	defer close(aRelease)
	for _, ch := range []chan struct{}{aStarted, bStarted} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("session did not start")
		}
	}
	cancel.set.Store(true)
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := outcomes.Load("a"); ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	a, _ := outcomes.Load("a")
	var cancelled *AuxiliaryExplicitCancellation
	if err, _ := a.(error); !errors.As(err, &cancelled) {
		t.Fatalf("session a outcome %v", a)
	}
	if client.closed.Load() {
		t.Fatal("the shared client was closed")
	}
	close(bRelease)
	select {
	case <-bDone:
	case <-time.After(time.Second):
		t.Fatal("session b did not finish")
	}
	b, _ := outcomes.Load("b")
	if _, failed := b.(error); failed || ExtractContentOrReasoning(b, -1) != "ok" {
		t.Fatalf("session b outcome %v", b)
	}
	wg.Wait()
}

func TestCallLLMRejectsUnknownAndMissingKeywords(t *testing.T) {
	if _, err := CallLLM(context.Background(), map[string]any{"task": "title_generation"}); err == nil || !strings.Contains(err.Error(), "'messages'") {
		t.Fatalf("missing messages %v", err)
	}
	if _, err := AsyncCallLLM(context.Background(), map[string]any{"messages": []any{}, "stream": true}); err == nil || !strings.Contains(err.Error(), "'stream'") {
		t.Fatalf("async stream keyword %v", err)
	}
}

type auxFakeProviderClient struct {
	route Route
	got   *Request
}

func (c *auxFakeProviderClient) Route() Route { return c.route }
func (c *auxFakeProviderClient) Complete(_ context.Context, req *Request) (*Response, error) {
	c.got = req
	return &Response{Message: &state.Message{Role: "assistant", Content: "hello"}, FinishReason: "stop", Usage: Usage{InputTokens: 3, OutputTokens: 2}, Model: "m"}, nil
}
func (c *auxFakeProviderClient) Stream(_ context.Context, req *Request, fn StreamFunc) (*Response, error) {
	c.got = req
	for _, piece := range []string{"he", "llo"} {
		if err := fn(StreamEvent{Kind: EventTextDelta, Text: piece}); err != nil {
			return nil, err
		}
	}
	return &Response{FinishReason: "stop", Model: "m"}, nil
}
func (c *auxFakeProviderClient) CountTokens(context.Context, *Request) (int, error) { return 0, nil }

func TestAuxProviderClientAuxTranslatesChatKwargsAndResponses(t *testing.T) {
	inner := &auxFakeProviderClient{route: Route{BaseURL: "https://p.example/v1", APIKey: "k"}}
	aux := auxProviderClientAux(context.Background(), inner)
	if aux.BaseURL() != "https://p.example/v1" || aux.APIKey() != "k" {
		t.Fatalf("identity %q %q", aux.BaseURL(), aux.APIKey())
	}
	resp, err := aux.CreateChatCompletion(context.Background(), map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 16, "temperature": 0.2, "timeout": 30.0, "stop": "END"})
	if err != nil || ExtractContentOrReasoning(resp, -1) != "hello" {
		t.Fatalf("complete %v %v", resp, err)
	}
	if inner.got.MaxTokens != 16 || *inner.got.Temperature != 0.2 || inner.got.Timeout != 30*time.Second || len(inner.got.Messages) != 1 || inner.got.Stop[0] != "END" {
		t.Fatalf("request %+v", inner.got)
	}
	if usage := resp.(map[string]any)["usage"].(map[string]any); usage["total_tokens"] != 5 {
		t.Fatalf("usage %v", usage)
	}
	streamed, err := aux.CreateChatCompletion(context.Background(), map[string]any{"model": "m", "messages": []any{}, "stream": true})
	if err != nil {
		t.Fatal(err)
	}
	stream := streamed.(AuxChatStream)
	defer stream.Close()
	var text strings.Builder
	for {
		chunk, err := stream.Next(context.Background())
		if err != nil {
			break
		}
		delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			text.WriteString(s)
		}
	}
	if text.String() != "hello" {
		t.Fatalf("streamed %q", text.String())
	}
}

// Swaps the cached client acquisition for one fixed client and model
func auxSwapCachedClient(t *testing.T, client AuxClient, model string) {
	t.Helper()
	prev := auxGetCachedClientFn
	t.Cleanup(func() { auxGetCachedClientFn = prev })
	auxGetCachedClientFn = func(context.Context, auxCachedClientRequest) (AuxClient, string, error) { return client, model, nil }
}

func TestKimiTemperatureOmittedAsyncCall(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{})
	response := auxCallOK("ok")
	client := &auxCallFakeClient{base: "https://api.kimi.com/coding/v1", key: "k",
		create: func(context.Context, map[string]any) (any, error) { return response, nil }}
	auxSwapCachedClient(t, client, "kimi-for-coding")
	result, err := AsyncCallLLM(context.Background(), map[string]any{"task": "session_search",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}}, "temperature": 0.1})
	if err != nil || auxActualContent(t, result) != "ok" {
		t.Fatalf("result %v err %v", result, err)
	}
	kwargs := client.sent[len(client.sent)-1]
	if _, has := kwargs["temperature"]; has || kwargs["model"] != "kimi-for-coding" {
		t.Fatalf("kwargs %v", kwargs)
	}
}

func TestKimiTemperatureNonKimiModelsPreserveTemperature(t *testing.T) {
	temp := 0.3
	for _, model := range []string{"anthropic/claude-sonnet-4-6", "gpt-4.1", "deepseek-chat"} {
		kwargs, err := auxBuildCallKwargs(context.Background(), "openrouter", model, []any{map[string]any{"role": "user", "content": "hello"}},
			auxCallKwargsOptions{Temperature: &temp})
		if err != nil || kwargs["temperature"] != 0.3 {
			t.Fatalf("%s kwargs %v err %v", model, kwargs, err)
		}
	}
}

func TestAuxiliaryTaskExtraBody(t *testing.T) {
	for _, tc := range []struct {
		name      string
		async     bool
		taskExtra map[string]any
		explicit  map[string]any
		want      map[string]any
	}{
		{"sync_call_merges_task_extra_body_from_config", false, map[string]any{"enable_thinking": false, "reasoning": map[string]any{"effort": "none"}},
			map[string]any{"metadata": map[string]any{"source": "test"}},
			map[string]any{"enable_thinking": false, "reasoning": map[string]any{"effort": "none"}, "metadata": map[string]any{"source": "test"}}},
		{"async_call_explicit_extra_body_overrides_task_config", true, map[string]any{"enable_thinking": false},
			map[string]any{"enable_thinking": true}, map[string]any{"enable_thinking": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := t.TempDir()
			t.Setenv("ICEBATH_HOME", h)
			writeConfig(t, h, map[string]any{"auxiliary": map[string]any{"session_search": map[string]any{"extra_body": tc.taskExtra}}})
			client := &auxCallFakeClient{base: "https://api.example.com/v1", key: "k",
				create: func(context.Context, map[string]any) (any, error) { return auxCallOK("ok"), nil }}
			auxSwapCachedClient(t, client, "glm-4.5-air")
			call := CallLLM
			if tc.async {
				call = AsyncCallLLM
			}
			if _, err := call(context.Background(), map[string]any{"task": "session_search",
				"messages": []any{map[string]any{"role": "user", "content": "hello"}}, "extra_body": tc.explicit}); err != nil {
				t.Fatal(err)
			}
			body, _ := client.sent[len(client.sent)-1]["extra_body"].(map[string]any)
			for k, v := range tc.want {
				if fmt.Sprint(body[k]) != fmt.Sprint(v) {
					t.Fatalf("extra_body %v, want %s=%v", body, k, v)
				}
			}
		})
	}
}

func TestAuxiliaryPoolAwarenessCallLLMRetriesNousAfter401(t *testing.T) {
	ctx := context.Background()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, map[string]any{"auxiliary": map[string]any{"compression": map[string]any{"provider": "nous", "model": "nous-model"}}})
	ShutdownCachedClients(ctx)
	t.Cleanup(func() { ShutdownCachedClients(ctx) })
	var freshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		freshCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(auxCallOK("fresh"))
	}))
	t.Cleanup(srv.Close)
	stale := &auxCallFakeClient{base: "https://inference-api.nousresearch.com/v1", key: "stale-key",
		create: func(context.Context, map[string]any) (any, error) {
			return nil, &AuxSDKError{Class: "APIStatusError", Message: "stale nous key", StatusCode: 401}
		}}
	auxSwapCachedClient(t, stale, "nous-model")
	prevPaid, prevNous := auxNousFreshPaidAccessFn, auxResolveNousRuntimeAPIFn
	t.Cleanup(func() { auxNousFreshPaidAccessFn, auxResolveNousRuntimeAPIFn = prevPaid, prevNous })
	auxNousFreshPaidAccessFn = func(context.Context) bool { return false }
	auxResolveNousRuntimeAPIFn = func(context.Context, bool, string) (string, string, bool) {
		return "fresh-agent-key", srv.URL + "/v1", true
	}
	result, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if err != nil || auxActualContent(t, result) != "fresh" {
		t.Fatalf("result %v err %v", result, err)
	}
	if len(stale.sent) != 1 || freshCalls.Load() != 1 {
		t.Fatalf("stale calls %d fresh calls %d", len(stale.sent), freshCalls.Load())
	}
}
