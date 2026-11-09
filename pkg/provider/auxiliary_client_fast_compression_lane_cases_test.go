package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

const auxFastLaneCertifiedYAML = "auxiliary:\n  compression:\n    provider: ollama\n    model: qwen3:8b\n    reasoning_effort: none\n    max_output_tokens: 1400\n"

func auxFastLaneConfig(reasoningEffort any) map[string]any {
	return map[string]any{"provider": "ollama", "model": "qwen3:8b", "reasoning_effort": reasoningEffort, "max_output_tokens": 1400}
}

type auxOpaqueResponse struct{ tag string }

// Stream whose first chunk arrives after the delay
type auxDelayedStream struct {
	auxSliceStream
	delay time.Duration
	once  sync.Once
}

func (s *auxDelayedStream) Next(ctx context.Context) (map[string]any, error) {
	s.once.Do(func() { time.Sleep(s.delay) })
	return s.auxSliceStream.Next(ctx)
}

// Compression call_llm through a scripted ollama client, returning the wire request it recorded
func auxFastLaneRequest(t *testing.T, configYAML, clientModel string, kwargs map[string]any) map[string]any {
	t.Helper()
	ctx := auxMainTestHome(t, configYAML)
	resp := &auxOpaqueResponse{tag: "response"}
	client := &auxScriptedClient{base: "http://127.0.0.1:11434/v1", create: func(map[string]any) (any, error) { return resp, nil }}
	auxSwapCachedClient(t, client, clientModel)
	prev := auxValidateLLMResponseFn
	t.Cleanup(func() { auxValidateLLMResponseFn = prev })
	auxValidateLLMResponseFn = func(context.Context, any, string, string, string) (any, error) { return resp, nil }
	kwargs["task"] = "compression"
	if _, ok := kwargs["messages"]; !ok {
		kwargs["messages"] = auxConcurrencyMessages("summary request")
	}
	got, err := CallLLM(ctx, kwargs)
	if err != nil {
		t.Fatal(err)
	}
	if got != resp {
		t.Fatalf("call_llm returned %v, not the provider response", got)
	}
	if n := client.callCount(); n == 0 {
		t.Fatal("create never called")
	}
	return client.calls[client.callCount()-1]
}

func auxExtraBody(request map[string]any) map[string]any {
	body, _ := request["extra_body"].(map[string]any)
	return body
}

func auxLatencyMs(t *testing.T, timings map[string]any, key string) float64 {
	t.Helper()
	auxLatencyMu.Lock()
	defer auxLatencyMu.Unlock()
	switch v := timings[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	t.Fatalf("%s missing from %v", key, timings)
	return 0
}

func TestExplicitNonReasoningCompressionRouteIsCertified(t *testing.T) {
	lane, err := ResolveCompressionFastLane(auxMainTestHome(t, ""), "ollama", "qwen3:8b", "", "", auxFastLaneConfig("none"))
	if err != nil || !lane.CertifiedNonReasoning {
		t.Fatalf("lane %+v %v", lane, err)
	}
	if !reflect.DeepEqual(lane.ReasoningConfig, map[string]any{"enabled": false, "effort": "none"}) {
		t.Fatalf("reasoning config %v", lane.ReasoningConfig)
	}
}

func TestInheritedAutoOrUncertifiedCompressionRoutesRemainUncapped(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	for name, cfg := range map[string]map[string]any{
		"inherited": {"provider": "auto", "model": "", "reasoning_effort": "none", "max_output_tokens": 1400},
		"unknown":   {"provider": "ollama", "model": "qwen3:8b", "max_output_tokens": 1400},
		"reasoning": auxFastLaneConfig("low"),
	} {
		lane, err := ResolveCompressionFastLane(ctx, "ollama", "qwen3:8b", "", "", cfg)
		if err != nil || lane.CertifiedNonReasoning || lane.ReasoningConfig != nil {
			t.Fatalf("%s lane %+v %v", name, lane, err)
		}
	}
}

func TestInheritedReasoningControlIsPreservedWithoutEnablingACap(t *testing.T) {
	for _, configYAML := range []string{
		auxFastLaneCertifiedYAML,
		"auxiliary:\n  compression:\n    provider: auto\n    model: \"\"\n    reasoning_effort: none\n    max_output_tokens: 1400\n",
	} {
		body, err := auxGetTaskExtraBody(auxMainTestHome(t, configYAML), "compression")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(body["reasoning"], map[string]any{"enabled": false}) {
			t.Fatalf("reasoning %v in %v", body["reasoning"], body)
		}
	}
}

func TestSummaryModelOverrideIsCertifiedAgainstTheEffectiveModel(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	override, err := ResolveCompressionFastLane(ctx, "ollama", "qwen3:14b", "", "qwen3:14b", auxFastLaneConfig("none"))
	if err != nil || !override.CertifiedNonReasoning {
		t.Fatalf("override %+v %v", override, err)
	}
	drifted, err := ResolveCompressionFastLane(ctx, "ollama", "server-selected-model", "", "qwen3:14b", auxFastLaneConfig("none"))
	if err != nil || drifted.CertifiedNonReasoning {
		t.Fatalf("drifted %+v %v", drifted, err)
	}
}

func TestCompressionLatencyRecordsDelayedFirstProviderChunk(t *testing.T) {
	auxResetAuxSemaphores()
	t.Cleanup(auxResetAuxSemaphores)
	ctx := auxMainTestHome(t, "auxiliary:\n  compression:\n    max_concurrency: 1\n")
	sem, err := auxAcquireSyncAuxSemaphore(ctx, "compression")
	if err != nil || sem == nil {
		t.Fatalf("semaphore %v %v", sem, err)
	}
	if err := sem.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	client := &auxScriptedClient{base: "http://127.0.0.1:11434/v1", create: func(map[string]any) (any, error) {
		return &auxDelayedStream{auxSliceStream: auxSliceStream{chunks: []map[string]any{auxChunk(map[string]any{"content": "summary", "tool_calls": nil}, "stop", nil)}}, delay: 20 * time.Millisecond}, nil
	}}
	prev := auxGetCachedClientFn
	t.Cleanup(func() { auxGetCachedClientFn = prev })
	auxGetCachedClientFn = func(ctx context.Context, _ auxCachedClientRequest) (AuxClient, string, error) {
		auxNotifyAuxProgress(ctx)
		return client, "qwen3:8b", nil
	}
	timings := map[string]any{}
	type outcome struct {
		resp any
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxConcurrencyMessages("summary request"), "latency_info": timings})
		done <- outcome{resp, err}
	}()
	time.Sleep(10 * time.Millisecond)
	if err := sem.Release(); err != nil {
		t.Fatal(err)
	}
	var out outcome
	select {
	case out = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("compression call never returned")
	}
	if out.err != nil {
		t.Fatal(out.err)
	}
	if got := auxActualContent(t, out.resp); got != "summary" {
		t.Fatalf("content %q", got)
	}
	queue, dispatch := auxLatencyMs(t, timings, "queue_wait_ms"), auxLatencyMs(t, timings, "provider_dispatch_ms")
	first, total := auxLatencyMs(t, timings, "time_to_first_progress_ms"), auxLatencyMs(t, timings, "summary_generation_ms")
	if queue < 5 || dispatch < 0 || first < 15 || first < dispatch || total < first {
		t.Fatalf("timings %v", timings)
	}
}

func TestCertifiedFastLaneIgnoresLegacyCapAndPreservesReasoning(t *testing.T) {
	request := auxFastLaneRequest(t, auxFastLaneCertifiedYAML, "qwen3:8b", map[string]any{})
	if _, ok := request["max_tokens"]; ok {
		t.Fatalf("legacy cap reached the wire %v", request)
	}
	if request["reasoning_effort"] != "none" {
		t.Fatalf("reasoning_effort %v", request["reasoning_effort"])
	}
	body := auxExtraBody(request)
	if body["think"] != false {
		t.Fatalf("think %v in %v", body["think"], body)
	}
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("reasoning leaked %v", body)
	}
}

// Primary request on an uncertified effective route carries neither cap nor reasoning control
func auxUncertifiedPrimaryRequest(t *testing.T, configYAML string) {
	t.Helper()
	request := auxFastLaneRequest(t, configYAML, "server-selected-model", map[string]any{})
	if _, ok := request["max_tokens"]; ok {
		t.Fatalf("max_tokens %v", request["max_tokens"])
	}
	if _, ok := request["max_completion_tokens"]; ok {
		t.Fatalf("max_completion_tokens %v", request["max_completion_tokens"])
	}
	if _, ok := auxExtraBody(request)["reasoning"]; ok {
		t.Fatalf("reasoning leaked %v", request["extra_body"])
	}
}

func TestUncertifiedEffectivePrimaryRouteDoesNotReceiveFastCap(t *testing.T) {
	auxUncertifiedPrimaryRequest(t, auxFastLaneCertifiedYAML)
}

func TestLegacyBooleanCapDoesNotBypassRouteCertification(t *testing.T) {
	auxUncertifiedPrimaryRequest(t, "auxiliary:\n  compression:\n    provider: ollama\n    model: qwen3:8b\n    reasoning_effort: none\n    max_output_tokens: true\n")
}

func TestBedrockConverseTtfpWaitsForTheNonstreamingResponse(t *testing.T) {
	ctx := bedrockAuxEnv(t, "us-east-1")
	if err := os.WriteFile(filepath.Join(home.Dir(ctx), "config.yaml"), []byte("auxiliary:\n  compression:\n    provider: auto\n    model: \"\"\n    max_output_tokens: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := NewBedrockAuxiliaryClient("us-east-1", "amazon.nova-lite-v1:0")
	auxSwapCachedClient(t, client, "amazon.nova-lite-v1:0")
	prev := callBedrockConverseFn
	t.Cleanup(func() { callBedrockConverseFn = prev })
	callBedrockConverseFn = func(_ context.Context, _ string, in bedrockConverseInput) (*bedrockConverseResult, error) {
		time.Sleep(20 * time.Millisecond)
		return &bedrockConverseResult{Content: "summary", FinishReason: "stop", Model: in.Model}, nil
	}
	timings := map[string]any{}
	resp, err := CallLLM(ctx, map[string]any{"task": "compression", "messages": auxConcurrencyMessages("summary request"), "latency_info": timings})
	if err != nil {
		t.Fatal(err)
	}
	if got := auxActualContent(t, resp); got != "summary" {
		t.Fatalf("content %q", got)
	}
	dispatch, first := auxLatencyMs(t, timings, "provider_dispatch_ms"), auxLatencyMs(t, timings, "time_to_first_progress_ms")
	if dispatch < 0 || first < 15 || first < dispatch {
		t.Fatalf("timings %v", timings)
	}
}

func TestSummaryModelOverrideCapUsesTheActualPrimaryRequest(t *testing.T) {
	request := auxFastLaneRequest(t, auxFastLaneCertifiedYAML, "qwen3:14b", map[string]any{"model": "qwen3:14b"})
	if request["model"] != "qwen3:14b" {
		t.Fatalf("model %v", request["model"])
	}
	if _, ok := request["max_tokens"]; ok {
		t.Fatalf("max_tokens %v", request["max_tokens"])
	}
}

func TestFallbackReasoningRequiresIndependentRouteCertification(t *testing.T) {
	requestFor := func(entry string) map[string]any {
		ctx := auxMainTestHome(t, "auxiliary:\n  compression:\n    fallback_chain:\n"+entry+"    provider: ollama\n    model: qwen3:8b\n    reasoning_effort: none\n    max_output_tokens: 1400\n")
		resp := &auxOpaqueResponse{tag: "fallback"}
		client := &auxScriptedClient{base: "http://127.0.0.1:11434/v1", create: func(map[string]any) (any, error) { return resp, nil }}
		prev := auxValidateLLMResponseFn
		t.Cleanup(func() { auxValidateLLMResponseFn = prev })
		auxValidateLLMResponseFn = func(context.Context, any, string, string, string) (any, error) { return resp, nil }
		got, err := auxCallFallbackCandidate(ctx, client, "qwen3:14b", "fallback_chain[0](ollama)", auxFallbackRequest{
			Task: "compression", Messages: []map[string]any{{"role": "user", "content": "summary request"}}, EffectiveTimeout: 300,
			EffectiveExtraBody: map[string]any{"reasoning": map[string]any{"enabled": false, "effort": "none"}}}, false)
		if err != nil || got != resp {
			t.Fatalf("fallback candidate returned %v %v", got, err)
		}
		return client.calls[client.callCount()-1]
	}
	uncertified := requestFor("      - provider: ollama\n        model: qwen3:14b\n")
	certified := requestFor("      - provider: ollama\n        model: qwen3:14b\n        reasoning_effort: none\n        max_output_tokens: 900\n")
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if _, ok := uncertified[key]; ok {
			t.Fatalf("uncertified sent %s %v", key, uncertified[key])
		}
	}
	for _, key := range []string{"reasoning", "think"} {
		if _, ok := auxExtraBody(uncertified)[key]; ok {
			t.Fatalf("uncertified extra_body carries %s %v", key, uncertified["extra_body"])
		}
	}
	if _, ok := certified["max_tokens"]; ok {
		t.Fatalf("certified sent max_tokens %v", certified["max_tokens"])
	}
	if certified["reasoning_effort"] != "none" {
		t.Fatalf("certified reasoning_effort %v", certified["reasoning_effort"])
	}
	body := auxExtraBody(certified)
	if body["think"] != false {
		t.Fatalf("certified think %v in %v", body["think"], body)
	}
	if _, ok := body["reasoning"]; ok {
		t.Fatalf("certified reasoning leaked %v", body)
	}
}

func TestReasoningEffortAliasesCertifyLikeNone(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	for _, alias := range []any{"none", "false", "disabled", false} {
		lane, err := ResolveCompressionFastLane(ctx, "ollama", "qwen3:8b", "", "", auxFastLaneConfig(alias))
		if err != nil || !lane.CertifiedNonReasoning {
			t.Fatalf("%v: %+v %v", alias, lane, err)
		}
	}
	for _, notDisabled := range []any{"", nil, "low", "high", true} {
		lane, err := ResolveCompressionFastLane(ctx, "ollama", "qwen3:8b", "", "", auxFastLaneConfig(notDisabled))
		if err != nil || lane.CertifiedNonReasoning {
			t.Fatalf("%v: %+v %v", notDisabled, lane, err)
		}
	}
}

func TestExplicitCallerMaxTokensKeepsProviderQuirkHandling(t *testing.T) {
	request := auxFastLaneRequest(t, "auxiliary:\n  compression:\n    provider: auto\n    model: \"\"\n    max_output_tokens: 0\n", "qwen3:8b", map[string]any{"max_tokens": 1500})
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if _, ok := request[key]; ok {
			t.Fatalf("%s forced onto the wire %v", key, request)
		}
	}
}
