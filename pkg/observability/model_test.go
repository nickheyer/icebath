package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

type validationHostFake struct {
	ctx   context.Context
	agent *metricsAgentFake
	tries tools.TurnValidationRetries
}

func (h *validationHostFake) ValidToolNames() map[string]bool { return h.agent.ValidToolNames() }
func (h *validationHostFake) RepairToolName(name string) string {
	if name == "Read_File" {
		return "read_file"
	}
	return ""
}
func (h *validationHostFake) Messages() []*state.Message                         { return nil }
func (h *validationHostFake) AppendAssistantMessage()                            {}
func (h *validationHostFake) AppendToolErrorResults(func(state.ToolCall) string) {}
func (h *validationHostFake) PartialExit(string) map[string]any                  { return map[string]any{} }
func (h *validationHostFake) CleanupTaskResources()                              {}
func (h *validationHostFake) RecordToolCallQuality(calls []state.ToolCall, repaired map[int]bool) {
	RecordValidatedToolCalls(h.ctx, h.agent, calls, repaired)
}
func (h *validationHostFake) Print(string, bool)                    {}
func (h *validationHostFake) BufferPrint(string)                    {}
func (h *validationHostFake) FlushStatusBuffer()                    {}
func (h *validationHostFake) SiteCopy(code string) string           { return code }
func (h *validationHostFake) LogPrefix() string                     { return "" }
func (h *validationHostFake) Retries() *tools.TurnValidationRetries { return &h.tries }

// Agent with a read_file tool requiring path and a parameterless todo tool
func qualityAgent(provider, model string) *metricsAgentFake {
	return &metricsAgentFake{provider: provider, model: model, sessionID: "s1", tools: []map[string]any{
		{"type": "function", "function": map[string]any{"name": "read_file", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}}},
		{"type": "function", "function": map[string]any{"name": "todo", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
	}}
}

func qualityCall(id, name, arguments string, argsRepaired bool) state.ToolCall {
	fn := state.ToolCallFunction{Name: name, Arguments: arguments}
	if argsRepaired {
		fn.Extra = map[string]any{"args_repaired": true}
	}
	return state.ToolCall{ID: id, Type: "function", Function: fn}
}

func validateCalls(ctx context.Context, agent *metricsAgentFake, calls ...state.ToolCall) {
	tools.ValidateToolCalls(&validationHostFake{ctx: ctx, agent: agent}, calls, "tool_calls")
}

// One user turn on openrouter whose single call may fail with a retryable error before it reports usage
func modelTurn(ctx context.Context, sessionID, taskID, model string, result, usage map[string]any, contextLength any, errReason string, start map[string]any) {
	base := map[string]any{"session_id": sessionID, "task_id": taskID, "api_request_id": taskID + "-r", "provider": "openrouter", "model": model}
	invokeHook(ctx, "pre_llm_call", base, map[string]any{"platform": "cli"}, start)
	invokeHook(ctx, "pre_api_request", base)
	if errReason != "" {
		invokeHook(ctx, "api_request_error", base, map[string]any{"retryable": true, "reason": errReason})
	}
	if usage != nil || errReason == "" {
		var u any
		if usage != nil {
			u = usage
		}
		invokeHook(ctx, "post_api_request", base, map[string]any{"usage": u, "context_length": contextLength})
	}
	if result != nil {
		FinishTaskRun(ctx, sessionID, taskID, "cli", result, nil)
	}
}

// Flushed rows of one metric as the listed dimensions joined by bars with the value last
func modelRows(t *testing.T, ctx context.Context, metric string, keys ...string) []string {
	t.Helper()
	if err := getRuntime(ctx, true, nil).relay.Subscribers.Flush(); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range storedValues(t, ctx, metric) {
		parts := make([]string, 0, len(keys)+1)
		for _, k := range keys {
			parts = append(parts, fmt.Sprint(v.dims[k]))
		}
		out = append(out, strings.Join(append(parts, fmt.Sprint(v.value)), "|"))
	}
	sort.Strings(out)
	return out
}

func TestEveryEmittedToolCallCountsOnceWithItsIssue(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter", "anthropic")
	validateCalls(ctx, qualityAgent("openrouter", "anthropic/claude-sonnet"),
		qualityCall("c1", "read_file", `{"path": "a"}`, false),
		qualityCall("c2", "todo", "", false),
		qualityCall("c3", "read_file", `{"path": "a"`, false),
		qualityCall("c4", "acme_secret_tool", "{}", false),
		qualityCall("c5", "read_file", "", false),
		qualityCall("c6", "read_file", `{"file": "a"}`, false),
		qualityCall("c7", "Read_File", `{"path": "a"}`, false),
		qualityCall("c8", "read_file", `{"path": "a"}`, true))
	validateCalls(ctx, qualityAgent("custom:acme-private", "acme-internal"), qualityCall("c9", "todo", "{}", false))
	got := modelRows(t, ctx, ModelToolQualityMetric, "provider", "model", "call_role", "issue")
	raw, _ := json.Marshal(storedValues(t, ctx, ModelToolQualityMetric))
	if strings.Contains(fmt.Sprint(got), "acme") || strings.Contains(string(raw), "acme") {
		t.Fatalf("user named provider left %v", got)
	}
	want := rowSet("openrouter|anthropic/claude-sonnet|primary|none|2", "openrouter|anthropic/claude-sonnet|primary|invalid_json|1",
		"openrouter|anthropic/claude-sonnet|primary|unknown_tool|1", "openrouter|anthropic/claude-sonnet|primary|empty_arguments|1",
		"openrouter|anthropic/claude-sonnet|primary|schema_mismatch|1", "openrouter|anthropic/claude-sonnet|primary|repaired|2",
		"custom|custom|primary|none|1")
	if !slices.Equal(got, want) {
		t.Fatalf("quality rows %v", got)
	}
}

func TestToolCallQualityRecordsNothingWhileDisabled(t *testing.T) {
	_, ctx := directRuntime(t)
	writeProfileConfig(t, home.Dir(ctx), "{}\n")
	validateCalls(ctx, qualityAgent("openrouter", "anthropic/claude-sonnet"), qualityCall("c1", "nope", "{", false))
	if got := storedValues(t, ctx, ModelToolQualityMetric); len(got) != 0 {
		t.Fatalf("disabled quality rows %v", got)
	}
}

func TestFrictionIsAttributedToTheModelThatProducedTheTurn(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter", "anthropic")
	done := map[string]any{"completed": true}
	modelTurn(ctx, "s1", "t1", "model-a", done, nil, nil, "", nil)
	RecordModelSwitch(ctx, "openrouter", "anthropic", "cli", "model-a", nil)
	RecordModelFriction(ctx, "retry", FrictionOptions{SessionID: "s1", Provider: "anthropic", Model: "model-b"})
	RecordModelFriction(ctx, "undo", FrictionOptions{SessionID: "gone", Provider: "anthropic", Model: "model-b"})
	modelTurn(ctx, "s1", "t2", "model-a", map[string]any{"interrupted": true, "turn_exit_reason": "interrupted_by_user"}, nil, nil, "", nil)
	modelTurn(ctx, "s1", "t3", "model-a", map[string]any{"failed": true, "turn_exit_reason": "failed"}, nil, nil, "", nil)
	finalizeSession(ctx, "s1")
	modelTurn(ctx, "s2", "t1", "model-c", done, nil, nil, "", nil)
	modelTurn(ctx, "s2", "t2", "model-c", nil, nil, nil, "", nil)
	finalizeSession(ctx, "s2")
	modelTurn(ctx, "child", "t1", "model-d", map[string]any{"interrupted": true}, nil, nil, "", map[string]any{"parent_session_id": "s1"})
	finalizeSession(ctx, "child")
	got := modelRows(t, ctx, ModelFrictionMetric, "provider", "model", "signal")
	want := rowSet("openrouter|model-a|switch_away|1", "openrouter|model-a|retry|1", "anthropic|model-b|undo|1",
		"openrouter|model-a|interrupt|1", "openrouter|model-a|quick_abandon|1")
	if !slices.Equal(got, want) {
		t.Fatalf("friction rows %v", got)
	}
}

func TestContextPeakIsOneBucketedRowPerSession(t *testing.T) {
	_, ctx := directRuntime(t)
	withProviderNames(t, "openrouter")
	done := map[string]any{"completed": true}
	modelTurn(ctx, "s1", "t1", "model-a", done, map[string]any{"prompt_tokens": 50_000}, 200_000, "", nil)
	modelTurn(ctx, "s1", "t2", "model-a", done, map[string]any{"prompt_tokens": 160_000}, 200_000, "context_overflow", nil)
	modelTurn(ctx, "s1", "t3", "model-a", done, map[string]any{"prompt_tokens": 20_000}, 200_000, "", nil)
	finalizeSession(ctx, "s1")
	modelTurn(ctx, "s2", "t1", "model-b", done, map[string]any{"input_tokens": 10_000}, 1_048_576, "", nil)
	finalizeSession(ctx, "s2")
	modelTurn(ctx, "s3", "t1", "model-c", nil, nil, nil, "context_overflow", nil)
	FinishTaskRun(ctx, "s3", "t1", "cli", map[string]any{"failed": true}, nil)
	finalizeSession(ctx, "s3")
	modelTurn(ctx, "child", "t1", "model-d", done, map[string]any{"prompt_tokens": 1}, 8_000, "", map[string]any{"parent_session_id": "s1"})
	finalizeSession(ctx, "child")
	got := modelRows(t, ctx, ContextPeakMetric, "model", "peak_fill_bucket", "window_bucket", "limit_hit")
	want := []string{"model-a|75_to_90|128k_to_256k|yes|1", "model-b|lt_50|gte_1m|no|1", "model-c|unknown|unknown|yes|1"}
	if !slices.Equal(got, want) {
		t.Fatalf("context peak rows %v", got)
	}
}
