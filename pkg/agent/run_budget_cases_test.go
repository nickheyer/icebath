package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func runBudgetAgent(t *testing.T, configBody string, edit func(*AIAgentOptions)) *AIAgent {
	t.Helper()
	for _, name := range []string{"ICEBATH_API_CALL_STALE_TIMEOUT", "ICEBATH_STREAM_STALE_TIMEOUT", "ICEBATH_LOCAL_STREAM_STALE_TIMEOUT"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	if configBody == "" {
		configBody = "{}\n"
	}
	o := AIAgentOptions{Model: "gpt-5.5", Provider: "openai", APIKey: "sk-dummy", BaseURL: "https://api.openai.com/v1", Platform: "cli"}
	if edit != nil {
		edit(&o)
	}
	a := newFixtureAgentWithConfig(t, configBody, o)
	t.Cleanup(func() { a.Close() })
	return a
}

func budgetOf(seconds float64) func(*AIAgentOptions) {
	return func(o *AIAgentOptions) { o.RunBudgetSeconds = &seconds }
}

func deepseekBudget(o *AIAgentOptions) {
	budget := 900.0
	o.Model, o.RunBudgetSeconds = "deepseek/deepseek-v4-pro", &budget
}

func budgetToolRows() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": "do the task"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t1"}}},
		{"role": "tool", "tool_call_id": "t1", "content": "result one"},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t2"}}},
		{"role": "tool", "tool_call_id": "t2", "content": "result two"},
	}
}

func TestRunBudget(t *testing.T) {
	ctx := context.Background()
	payload := map[string]any{"input": "hi"}
	t.Run("test_normalize_run_budget_seconds", func(t *testing.T) {
		f := func(v float64) *float64 { return &v }
		for _, c := range []struct {
			raw  any
			want *float64
		}{{nil, nil}, {0, nil}, {-5, nil}, {"abc", nil}, {true, nil}, {false, nil}, {math.NaN(), nil}, {900, f(900)}, {"850", f(850)}, {0.5, f(0.5)}} {
			got := normalizeRunBudgetSeconds(c.raw)
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Errorf("normalizeRunBudgetSeconds(%v) = %v", c.raw, got)
			}
		}
	})
	t.Run("test_config_key_sets_budget", func(t *testing.T) {
		if a := runBudgetAgent(t, "agent:\n  run_budget_seconds: 750\n", nil); a.runBudgetSeconds != 750.0 {
			t.Fatalf("budget %v", a.runBudgetSeconds)
		}
	})
	t.Run("test_constructor_arg_wins_over_config", func(t *testing.T) {
		if a := runBudgetAgent(t, "agent:\n  run_budget_seconds: 750\n", budgetOf(900)); a.runBudgetSeconds != 900.0 {
			t.Fatalf("budget %v", a.runBudgetSeconds)
		}
	})
	t.Run("test_no_budget_stale_timeout_unchanged", func(t *testing.T) {
		a := runBudgetAgent(t, "", nil)
		base, _ := a.resolvedAPICallStaleTimeoutBase(ctx)
		if got := a.computeNonStreamStaleTimeout(ctx, payload); got != base {
			t.Fatalf("timeout %v base %v", got, base)
		}
	})
	t.Run("test_active_budget_caps_implicit_reasoning_floor", func(t *testing.T) {
		a := runBudgetAgent(t, "", deepseekBudget)
		a.runBudgetStartedAt = nowSeconds() - 800
		if got := a.computeNonStreamStaleTimeout(ctx, payload); got != 60.0 {
			t.Fatalf("timeout %v", got)
		}
	})
	t.Run("test_active_budget_cap_half_remaining", func(t *testing.T) {
		a := runBudgetAgent(t, "", deepseekBudget)
		a.runBudgetStartedAt = nowSeconds() - 100
		if got := a.computeNonStreamStaleTimeout(ctx, payload); got < 395.0 || got > 400.0 {
			t.Fatalf("timeout %v", got)
		}
	})
	t.Run("test_active_budget_never_raises_timeout", func(t *testing.T) {
		a := runBudgetAgent(t, "", budgetOf(900))
		base, _ := a.resolvedAPICallStaleTimeoutBase(ctx)
		a.runBudgetStartedAt = nowSeconds() - 10
		if got := a.computeNonStreamStaleTimeout(ctx, payload); got != base {
			t.Fatalf("timeout %v base %v", got, base)
		}
	})
	t.Run("test_explicit_provider_config_wins_over_budget_cap", func(t *testing.T) {
		a := runBudgetAgent(t, "providers:\n  openai:\n    stale_timeout_seconds: 1800\n", budgetOf(900))
		a.runBudgetStartedAt = nowSeconds() - 800
		if got := a.computeNonStreamStaleTimeout(ctx, payload); got != 1800.0 {
			t.Fatalf("timeout %v", got)
		}
	})
	t.Run("test_explicit_env_var_wins_over_budget_cap", func(t *testing.T) {
		a := runBudgetAgent(t, "", budgetOf(900))
		t.Setenv("ICEBATH_API_CALL_STALE_TIMEOUT", "1200")
		a.runBudgetStartedAt = nowSeconds() - 800
		if got := a.computeNonStreamStaleTimeout(ctx, payload); got != 1200.0 {
			t.Fatalf("timeout %v", got)
		}
	})
	t.Run("test_budget_without_started_clock_is_inert", func(t *testing.T) {
		a := runBudgetAgent(t, "", deepseekBudget)
		a.runBudgetStartedAt = 0
		base, _ := a.resolvedAPICallStaleTimeoutBase(ctx)
		if got := a.computeNonStreamStaleTimeout(ctx, payload); got != base {
			t.Fatalf("timeout %v base %v", got, base)
		}
	})
	t.Run("test_wrapup_not_injected_when_unset", func(t *testing.T) {
		a := &AIAgent{runBudgetStartedAt: nowSeconds() - 10_000}
		messages := rowsToMessages(budgetToolRows())
		if maybeInjectRunBudgetWrapup(a, messages) || !reflect.DeepEqual(messagesToRows(messages), messagesToRows(rowsToMessages(budgetToolRows()))) {
			t.Fatal("no budget injects nothing")
		}
	})
	t.Run("test_wrapup_not_injected_before_threshold", func(t *testing.T) {
		a := &AIAgent{runBudgetSeconds: 900, runBudgetStartedAt: nowSeconds() - 100}
		if maybeInjectRunBudgetWrapup(a, rowsToMessages(budgetToolRows())) || a.runBudgetWrapupInjected {
			t.Fatal("eleven percent elapsed injects nothing")
		}
	})
	t.Run("test_wrapup_injected_once_after_threshold", func(t *testing.T) {
		a := &AIAgent{runBudgetSeconds: 900, runBudgetStartedAt: nowSeconds() - 800}
		messages := rowsToMessages(budgetToolRows())
		if !maybeInjectRunBudgetWrapup(a, messages) || !a.runBudgetWrapupInjected {
			t.Fatal("eighty nine percent elapsed injects the notice")
		}
		last, _ := messages[4].Content.(string)
		if !strings.Contains(last, RunBudgetWrapupNotice) || !strings.HasPrefix(last, "result two") || messages[2].Content != "result one" {
			t.Fatalf("tool rows %q %q", messages[2].Content, last)
		}
		var roles []string
		for _, m := range messages {
			roles = append(roles, m.Role)
		}
		if !reflect.DeepEqual(roles, []string{"user", "assistant", "tool", "assistant", "tool"}) {
			t.Fatalf("roles %v", roles)
		}
		snapshot := messagesToRows(messages)
		if maybeInjectRunBudgetWrapup(a, messages) || !reflect.DeepEqual(messagesToRows(messages), snapshot) ||
			strings.Count(messages[4].Content.(string), RunBudgetWrapupNotice) != 1 {
			t.Fatal("the latch must stop a second injection")
		}
	})
	t.Run("test_wrapup_retries_when_no_tool_message_yet", func(t *testing.T) {
		a := &AIAgent{runBudgetSeconds: 900, runBudgetStartedAt: nowSeconds() - 800}
		messages := rowsToMessages([]map[string]any{{"role": "user", "content": "do the task"}})
		if maybeInjectRunBudgetWrapup(a, messages) || a.runBudgetWrapupInjected {
			t.Fatal("no tool row keeps the latch open")
		}
		messages = append(messages, rowsToMessages([]map[string]any{
			{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t1"}}},
			{"role": "tool", "tool_call_id": "t1", "content": "result"}})...)
		if !maybeInjectRunBudgetWrapup(a, messages) || !strings.Contains(messages[2].Content.(string), RunBudgetWrapupNotice) {
			t.Fatal("the next iteration delivers the notice")
		}
	})
	t.Run("test_wrapup_not_injected_without_turn_clock", func(t *testing.T) {
		if maybeInjectRunBudgetWrapup(&AIAgent{runBudgetSeconds: 900}, rowsToMessages(budgetToolRows())) {
			t.Fatal("no turn clock injects nothing")
		}
	})
	t.Run("test_wrapup_skips_already_persisted_tool_row", func(t *testing.T) {
		a := &AIAgent{runBudgetSeconds: 900, runBudgetStartedAt: nowSeconds() - 800}
		rows := budgetToolRows()
		rows[4][dbPersistedMarker] = true
		messages := rowsToMessages(rows)
		snapshot := messagesToRows(messages)
		if snapshot[4][dbPersistedMarker] != true {
			t.Fatalf("the persisted marker did not survive the row conversion %v", snapshot[4])
		}
		if maybeInjectRunBudgetWrapup(a, messages) || a.runBudgetWrapupInjected || !reflect.DeepEqual(messagesToRows(messages), snapshot) {
			t.Fatal("an already flushed tool row is append only")
		}
	})
	t.Run("test_wrapup_multimodal_tool_content", func(t *testing.T) {
		a := &AIAgent{runBudgetSeconds: 900, runBudgetStartedAt: nowSeconds() - 800}
		messages := rowsToMessages([]map[string]any{{"role": "user", "content": "task"},
			{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t1"}}},
			{"role": "tool", "tool_call_id": "t1", "content": []any{map[string]any{"type": "text", "text": "block"}}}})
		if !maybeInjectRunBudgetWrapup(a, messages) {
			t.Fatal("content blocks take the notice")
		}
		blocks := anyRows(messages[2].Content)
		if len(blocks) != 2 || !reflect.DeepEqual(blocks[0], map[string]any{"type": "text", "text": "block"}) ||
			!reflect.DeepEqual(blocks[1], map[string]any{"type": "text", "text": RunBudgetWrapupNotice}) {
			t.Fatalf("blocks %v", blocks)
		}
	})
	t.Run("test_wrapup_lands_in_the_persisted_row_via_pre_flush_hook", func(t *testing.T) {
		caseHome(t)
		a, db := newPersistTestAgent(t, "budget-proof")
		a.runBudgetSeconds, a.runBudgetStartedAt = 900, nowSeconds()-800
		reg := tools.NewRegistry()
		reg.Register(tools.Def{Name: "probe", Toolset: "web", Schema: map[string]any{"name": "probe", "parameters": map[string]any{"type": "object"}},
			Handler: func(context.Context, *tools.Call) string { return "result two" }})
		a.validToolNames = map[string]bool{"probe": true}
		a.toolGuardrails = tools.NewToolCallGuardrailController(nil)
		a.toolDispatcher = tools.NewDispatcher(reg)
		messages := rowsToMessages([]map[string]any{{"role": "user", "content": "do the task"},
			{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "t2", "type": "function", "function": map[string]any{"name": "probe", "arguments": "{}"}}}}})
		if err := a.toolExecutor().ExecuteToolCallsSequential(t.Context(), []state.ToolCall{toolCall("t2", "probe", "{}")}, &messages, "task-1", true); err != nil {
			t.Fatal(err)
		}
		persisted := persistConversation(t, db, "budget-proof", false)
		found := false
		for _, row := range persisted {
			found = found || strings.Contains(fmt.Sprint(row["content"]), RunBudgetWrapupNotice)
		}
		if !found || !a.runBudgetWrapupInjected {
			t.Fatalf("the notice never reached the durable rows %v", persisted)
		}
	})
	t.Run("test_gemini_long_decode_scales_implicit_stale_timeout_by_hops", func(t *testing.T) {
		a := runBudgetAgent(t, "", func(o *AIAgentOptions) {
			o.Model, o.Provider, o.APIKey, o.BaseURL = "gemini-4-argon", "gemini", "AQ.dummy", "https://generativelanguage.googleapis.com/v1beta"
		})
		request := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "model": "gemini-4-argon"}
		if got := a.computeNonStreamStaleTimeout(ctx, request); got != 600.0 {
			t.Fatalf("timeout %v", got)
		}
		longDecode := map[string]any{"messages": request["messages"], "model": "gemini-4-argon", "extra_body": map[string]any{"max_output_tokens": 262_144}}
		if got := a.computeNonStreamStaleTimeout(ctx, longDecode); got != 600.0*8 {
			t.Fatalf("long decode timeout %v", got)
		}
		t.Setenv("ICEBATH_API_CALL_STALE_TIMEOUT", "120")
		if got := a.computeNonStreamStaleTimeout(ctx, longDecode); got != 120.0 {
			t.Fatalf("explicit timeout %v", got)
		}
	})
}
