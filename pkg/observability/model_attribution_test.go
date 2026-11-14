package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const (
	attributionSecret = "acme-internal-secret-v2"
	attributionPublic = "anthropic/claude-sonnet-4"
)

var attributionModelMetrics = []string{"icebath.model_route.count", "icebath.model_tokens.sum", ContextPeakMetric,
	"icebath.fallback.count", "icebath.model_switch.count", "icebath.setup.completed", ModelFrictionMetric, ModelToolQualityMetric}

// One completed cli turn of one model call with its prompt tokens against a 200k window
func attributionTurn(ctx context.Context, sessionID, taskID string, provider, model any, promptTokens int, start map[string]any) {
	base := map[string]any{"session_id": sessionID, "task_id": taskID, "api_request_id": taskID + "-r", "provider": provider, "model": model}
	invokeHook(ctx, "pre_llm_call", base, map[string]any{"platform": "cli"}, start)
	invokeHook(ctx, "pre_api_request", base)
	invokeHook(ctx, "post_api_request", base, map[string]any{"usage": map[string]any{"prompt_tokens": promptTokens, "output_tokens": 5}, "context_length": 200_000})
	FinishTaskRun(ctx, sessionID, taskID, "cli", map[string]any{"completed": true}, nil)
}

// Drives every emitter carrying a provider or model dimension then returns the install snapshot of that route
func emitEveryModelMetric(t *testing.T, ctx context.Context, provider, model any) map[string]string {
	t.Helper()
	attributionTurn(ctx, "s1", "t1", provider, model, 1_000, nil)
	invokeHook(ctx, "post_auxiliary_call", map[string]any{"session_id": "s1", "provider": provider, "model": model,
		"aux_task": "compression", "usage": map[string]any{"input_tokens": 10}})
	RecordFallback(ctx, provider, "openrouter", "rate_limit")
	RecordModelSwitch(ctx, provider, "openrouter", "cli", model, nil)
	RecordSetupCompleted(ctx, "cli", provider)
	RecordModelFriction(ctx, "retry", FrictionOptions{SessionID: "not-seen", Provider: provider, Model: model})
	agent := &metricsAgentFake{provider: fmt.Sprint(provider), model: fmt.Sprint(model), tools: []map[string]any{
		{"type": "function", "function": map[string]any{"name": "todo", "parameters": map[string]any{"type": "object"}}}}}
	if provider == nil {
		agent.provider = ""
	}
	RecordToolCallQuality(ctx, agent, []EmittedToolCall{{Name: "todo", Arguments: "{}"}})
	finalizeSession(ctx, "s1")
	flushAllRuntimes(t)
	snapshot, err := CollectInstallSnapshot(ctx, map[string]any{"model": map[string]any{"provider": provider, "default": model}})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// Flushes the subscribers of every live runtime
func flushAllRuntimes(t *testing.T) {
	t.Helper()
	runtimesMu.Lock()
	live := runtimes.Values()
	runtimesMu.Unlock()
	for _, rt := range live {
		if err := rt.relay.Subscribers.Flush(); err != nil {
			t.Fatal(err)
		}
	}
}

// Asserts every model metric landed and every provider and model dimension reads one of the allowed values
func assertModelDims(t *testing.T, ctx context.Context, providers []string, model string, snapshot map[string]string, mainProviders ...string) {
	t.Helper()
	rows := storedRows(t, ctx)
	raw, _ := json.Marshal(rows)
	snap, _ := json.Marshal(snapshot)
	if strings.Contains(string(raw)+string(snap), attributionSecret) {
		t.Fatalf("secret model left %s %s", raw, snap)
	}
	seen := map[string]bool{}
	auxSeen := false
	for _, row := range rows {
		seen[row.MetricName] = true
		dims := dimsOf(row.Dimensions)
		auxSeen = auxSeen || (row.MetricName == "icebath.model_tokens.sum" && dims["aux_task"] == "compression")
		for _, key := range []string{"provider", "from_provider"} {
			if v, ok := dims[key]; ok && !slices.Contains(providers, fmt.Sprint(v)) {
				t.Fatalf("%s %s=%v", row.MetricName, key, v)
			}
		}
		if v, ok := dims["model"]; ok && v != model {
			t.Fatalf("%s model=%v", row.MetricName, v)
		}
	}
	for _, name := range attributionModelMetrics {
		if !seen[name] {
			t.Fatalf("metric %s missing from %v", name, seen)
		}
	}
	if !auxSeen {
		t.Fatalf("auxiliary compression token row missing from %v", seen)
	}
	if !slices.Contains(mainProviders, snapshot["main_provider"]) {
		t.Fatalf("main_provider %q", snapshot["main_provider"])
	}
}

func TestUserNamedModelsNeverLeaveInAnyModelMetric(t *testing.T) {
	for _, provider := range []any{"ollama", "local", "vllm", "llamacpp", "llama.cpp", "custom:acme-lab", nil, "", "acme-gpu-box"} {
		t.Run(fmt.Sprint(provider), func(t *testing.T) {
			_, ctx := directRuntime(t)
			snapshot := emitEveryModelMetric(t, ctx, provider, attributionSecret)
			allowed := []string{"custom", "unknown", "none"}
			assertModelDims(t, ctx, allowed, "custom", snapshot, allowed...)
		})
	}
}

func TestShippedProviderAndPublicModelAreReportedAsIs(t *testing.T) {
	_, ctx := directRuntime(t)
	snapshot := emitEveryModelMetric(t, ctx, "openrouter", attributionPublic)
	assertModelDims(t, ctx, []string{"openrouter"}, attributionPublic, snapshot, "openrouter")
}

func TestNetworkAddressModelIDsAndRawInjectedMarksNeverReachCounters(t *testing.T) {
	_, ctx := directRuntime(t)
	for _, model := range []string{"127.0.0.1:8080/x", "localhost/qwen", "10.0.0.5/x", "gpu-box.lan:8000/qwen", "gpu-box.lan:8000"} {
		if got := ModelRoute("openrouter", model)["model"]; got != "custom" {
			t.Fatalf("%s read %s", model, got)
		}
	}
	for _, pm := range [][2]string{{"bedrock", "arn:aws:bedrock:us-east-1:123456789012:inference-profile/x"}, {"lm-studio", "my-model-x"}, {"lm_studio", "my-model-x"}} {
		if got := ModelRoute(pm[0], pm[1])["model"]; got != "custom" {
			t.Fatalf("%v read %s", pm, got)
		}
	}
	for _, provider := range []string{"azure-foundry", "azure"} {
		if got := ModelRoute(provider, "acme-legal-prod-eastus")["model"]; got != "custom" {
			t.Fatalf("%s deployment read %s", provider, got)
		}
		if got := ModelRoute(provider, "GPT-4o")["model"]; got != "gpt-4o" {
			t.Fatalf("%s public read %s", provider, got)
		}
	}
	for _, pm := range [][2]string{{"bedrock", "anthropic.claude-3-5-sonnet-20241022-v2:0"}, {"openrouter", "openai/gpt-4o:free"}, {"ollama-cloud", "gpt-oss:120b"}} {
		if got := ModelRoute(pm[0], pm[1])["model"]; got != pm[1] {
			t.Fatalf("%v read %s", pm, got)
		}
	}
	rt := getRuntime(ctx, true, nil)
	for _, pm := range [][2]string{{"openrouter", "127.0.0.1:8080/x"}, {"openrouter", "http://127.0.0.1:8080/v1"}, {"custom:acme", "x"},
		{"ollama", "qwen-private"}, {"acmecorp-gateway", "acme-7b"}, {"anthropic", "claude-sonnet-4-5"}} {
		if err := rt.recordProcessMark(ctx, ModelSwitchAfterMark, map[string]any{"model": pm[1], "provider": pm[0], "turns_before_switch_bucket": "1"}); err != nil {
			t.Fatal(err)
		}
	}
	flushAllRuntimes(t)
	got := storedValues(t, ctx, ModelSwitchAfterMetric)
	if len(got) != 1 || dimsKey(got[0].dims) != "model=claude-sonnet-4-5,provider=anthropic,turns_before_switch_bucket=1" || got[0].value != 1 {
		t.Fatalf("switch after rows %v", got)
	}
}
