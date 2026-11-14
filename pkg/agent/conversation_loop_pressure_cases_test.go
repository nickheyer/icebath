package agent

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

func selectRequest() []map[string]any {
	return []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "hello"}}
}

func selectAgent(engine ContextEngine) *AIAgent {
	return &AIAgent{sessionID: "test-session", contextCompressor: engine}
}

func selectEngine(fn func(req, convo []map[string]any, incoming map[string]any) []map[string]any) *selectingEngine {
	return &selectingEngine{stubEngine: *newStubEngine(1000, 0.75), selectFn: fn}
}

func TestBaseNoopSelectContextIsShortCircuitedNotCalled(t *testing.T) {
	caseHome(t)
	warnings := captureLogger(t, "agent.conversation_loop", slog.LevelWarn)
	request, history := selectRequest(), []map[string]any{{"role": "user", "content": "hello"}}
	out := applyContextEngineSelection(selectAgent(newStubEngine(1000, 0.75)), request, history, history[0])
	if &out[0] != &request[0] || len(warnings()) != 0 {
		t.Fatalf("an engine inheriting the base hook keeps the request untouched, warnings %v", warnings())
	}
}

func TestEmptyListKeepsOriginalRequest(t *testing.T) {
	caseHome(t)
	warnings := captureLogger(t, "agent.conversation_loop", slog.LevelWarn)
	request, history := selectRequest(), []map[string]any{{"role": "user", "content": "hello"}}
	engine := selectEngine(func(_, _ []map[string]any, _ map[string]any) []map[string]any { return []map[string]any{} })
	out := applyContextEngineSelection(selectAgent(engine), request, history, history[0])
	if &out[0] != &request[0] || len(warnings()) == 0 {
		t.Fatalf("an empty selection falls open with a warning, warnings %v", warnings())
	}
}

func TestEngineMutatingInputsCannotCorruptPersistedState(t *testing.T) {
	caseHome(t)
	history := []map[string]any{{"role": "user", "content": "hello"}}
	incoming := history[0]
	engine := selectEngine(func(_, convo []map[string]any, in map[string]any) []map[string]any {
		if convo != nil {
			convo = append(convo, map[string]any{"role": "user", "content": "INJECTED"})
			convo[0]["content"] = "TAMPERED"
		}
		if in != nil {
			in["content"] = "TAMPERED"
		}
		return nil
	})
	applyContextEngineSelection(selectAgent(engine), selectRequest(), history, incoming)
	if !reflect.DeepEqual(history, []map[string]any{{"role": "user", "content": "hello"}}) || incoming["content"] != "hello" {
		t.Fatalf("engine edits reached persisted state %v %v", history, incoming)
	}
}

func TestRoleUnusualReplacementPassedThroughForDownstreamSanitizers(t *testing.T) {
	caseHome(t)
	unusual := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "a"}, {"role": "user", "content": "b"}}
	history := []map[string]any{{"role": "user", "content": "hello"}}
	engine := selectEngine(func(_, _ []map[string]any, _ map[string]any) []map[string]any { return unusual })
	if out := applyContextEngineSelection(selectAgent(engine), selectRequest(), history, history[0]); &out[0] != &unusual[0] {
		t.Fatal("a structurally valid replacement passes through verbatim")
	}
}

func TestOnTurnCompleteCalledWithSnapshotAndMeta(t *testing.T) {
	caseHome(t)
	history := []map[string]any{{"role": "user", "content": "hello"}}
	var gotMsgs []map[string]any
	var gotUsage, gotKw map[string]any
	engine := &selectingEngine{stubEngine: *newStubEngine(1000, 0.75), onComplete: func(m []map[string]any, u, kw map[string]any) {
		gotMsgs, gotUsage, gotKw = m, u, kw
	}}
	notifyContextEngineTurnComplete(selectAgent(engine), history, map[string]any{"total_tokens": 12}, map[string]any{"turn_id": "t1", "api_call_count": 1})
	if !reflect.DeepEqual(gotMsgs, history) || &gotMsgs[0] == &history[0] {
		t.Fatalf("turn complete snapshot %v", gotMsgs)
	}
	if !reflect.DeepEqual(gotUsage, map[string]any{"total_tokens": 12}) || gotKw["turn_id"] != "t1" || gotKw["api_call_count"] != 1 {
		t.Fatalf("usage %v meta %v", gotUsage, gotKw)
	}
}

func TestRearmDecision(t *testing.T) {
	caseHome(t)
	t.Run("test_provider_confirmed_recovery_rearms", func(t *testing.T) {
		if !shouldRearmCompressionBudget(2, true, 7_999, 10_000) {
			t.Fatal("a completed compaction with real headroom rearms")
		}
	})
	t.Run("test_unverified_or_pressured_response_keeps_budget_burned", func(t *testing.T) {
		for _, tc := range []struct {
			attempts          int
			pending           bool
			prompt, threshold int
		}{{0, true, 7_999, 10_000}, {2, false, 7_999, 10_000}, {2, true, 0, 10_000}, {2, true, 10_000, 10_000}, {2, true, 10_001, 10_000}, {2, true, 7_999, 0}} {
			if shouldRearmCompressionBudget(tc.attempts, tc.pending, tc.prompt, tc.threshold) {
				t.Fatalf("rearmed on %+v", tc)
			}
		}
	})
}

func loopCompressor(t *testing.T, edit func(*ContextCompressorOptions)) *ContextCompressor {
	t.Helper()
	window := 96000
	return newCompressorWith(t, window, func(o *ContextCompressorOptions) {
		o.Model, o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN = "test-model", 0.65, 2, 3
		o.ConfigContextLength = &window
		if edit != nil {
			edit(o)
		}
	})
}

func loopSession(turns, wordsPerTurn int) []map[string]any {
	base := strings.TrimSpace(strings.Repeat("a ", wordsPerTurn))
	messages := []map[string]any{{"role": "system", "content": "You are a helpful agent."}}
	for i := range turns {
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("%s (user turn %d)", base, i)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("%s (assistant turn %d)", base, i)})
	}
	return messages
}

func forceNoOpTailCut(t *testing.T) {
	prev := compressorFindTailCutByTokens
	compressorFindTailCutByTokens = func(_ *ContextCompressor, _ []map[string]any, headEnd int, _ *int) int { return headEnd }
	t.Cleanup(func() { compressorFindTailCutByTokens = prev })
}

func TestCompressNoOpRegistersIneffective(t *testing.T) {
	caseHome(t)
	t.Run("test_no_op_arms_structural_backoff", func(t *testing.T) {
		comp := loopCompressor(t, func(o *ContextCompressorOptions) { o.SummaryTargetRatio = 0.45 })
		messages := loopSession(10, 10)
		comp.LastPromptTokens = 73_000
		forceNoOpTailCut(t)
		result, err := comp.Compress(messages, iptr(73_000), nil, false, "")
		if err != nil || len(result) != len(messages) {
			t.Fatalf("a structural no op returns the transcript unchanged, got %d rows err %v", len(result), err)
		}
		if comp.ineffectiveCompressionCount != 0 || comp.structuralNoOpBackoffUntil <= compressorMonotonic() {
			t.Fatalf("strikes %d backoff %v", comp.ineffectiveCompressionCount, comp.structuralNoOpBackoffUntil)
		}
	})
	t.Run("test_two_no_ops_block_should_compress", func(t *testing.T) {
		comp := loopCompressor(t, func(o *ContextCompressorOptions) { o.SummaryTargetRatio = 0.45 })
		messages := loopSession(10, 10)
		comp.LastPromptTokens = 73_000
		forceNoOpTailCut(t)
		for range 2 {
			if _, err := comp.Compress(messages, iptr(73_000), nil, false, ""); err != nil {
				t.Fatal(err)
			}
		}
		if comp.ineffectiveCompressionCount != 0 || comp.ShouldCompress(iptr(73_000)) {
			t.Fatalf("the structural backoff must hold, strikes %d", comp.ineffectiveCompressionCount)
		}
	})
}

func TestTailCutRawBudgetFallback(t *testing.T) {
	caseHome(t)
	t.Run("test_meaningful_cut_with_large_ratio", func(t *testing.T) {
		comp := loopCompressor(t, func(o *ContextCompressorOptions) { o.SummaryTargetRatio = 0.45 })
		messages := loopSession(20, 20)
		headEnd := comp.AlignBoundaryForward(messages, comp.protectHeadSize(messages))
		cut := comp.findTailCutByTokens(messages, headEnd, nil)
		if cut-headEnd < 3 {
			t.Fatalf("compressable region of %d rows (cut=%d head_end=%d n=%d)", cut-headEnd, cut, headEnd, len(messages))
		}
	})
}

func TestAntiThrashing(t *testing.T) {
	caseHome(t)
	t.Run("test_ineffective_count_2_blocks", func(t *testing.T) {
		comp := loopCompressor(t, nil)
		comp.LastPromptTokens = 73_000
		comp.ineffectiveCompressionCount = 2
		if comp.ShouldCompress(iptr(73_000)) {
			t.Fatal("two ineffective compressions block the next one")
		}
	})
}

func TestCooldownGuard(t *testing.T) {
	caseHome(t)
	t.Run("test_active_cooldown_blocks", func(t *testing.T) {
		comp := loopCompressor(t, nil)
		comp.LastPromptTokens = 73_000
		comp.summaryFailureCooldownUntil = compressorMonotonic() + 60
		if comp.ShouldCompress(iptr(73_000)) {
			t.Fatal("an active summary cooldown blocks compression")
		}
	})
}

func TestCodexSparkShortSessionBoundary(t *testing.T) {
	caseHome(t)
	t.Run("test_short_high_token_session_has_compressible_window", func(t *testing.T) {
		window := 128000
		comp := newCompressorWith(t, 96000, func(o *ContextCompressorOptions) {
			o.Model, o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN, o.ConfigContextLength = "gpt-5.3-codex-spark", 0.70, 3, 20, &window
		})
		bigTool := strings.Repeat("x", 20000)
		messages := []map[string]any{{"role": "system", "content": "You are a helpful agent."}}
		for i := range 3 {
			messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("Question %d", i)},
				map[string]any{"role": "assistant", "content": fmt.Sprintf("Answer %d", i)})
		}
		for i := range 3 {
			id := fmt.Sprintf("tc%d", i)
			messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("Run command %d", i)},
				map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id, "type": "function",
					"function": map[string]any{"name": "terminal", "arguments": "{}"}}}},
				map[string]any{"role": "tool", "tool_call_id": id, "content": bigTool})
		}
		messages = append(messages, map[string]any{"role": "user", "content": "Final question"}, map[string]any{"role": "assistant", "content": "Final answer"})
		start := comp.AlignBoundaryForward(messages, comp.protectHeadSize(messages))
		end := comp.findTailCutByTokens(messages, start, nil)
		if start >= end || !comp.HasContentToCompress(messages) {
			t.Fatalf("no compressible window start=%d end=%d", start, end)
		}
	})
}

type realFloorEngine struct {
	stubEngine
	lastReal int
	awaiting bool
}

func (e *realFloorEngine) LastRealPromptTokens() (int, bool)       { return e.lastReal, true }
func (e *realFloorEngine) AwaitingRealUsageAfterCompression() bool { return e.awaiting }

func TestPressureRealFloor(t *testing.T) {
	caseHome(t)
	engine := func(lastReal int, awaiting bool) ContextEngine {
		return &realFloorEngine{stubEngine: *newStubEngine(1000, 0.75), lastReal: lastReal, awaiting: awaiting}
	}
	t.Run("test_real_floor_lifts_undercounted_rough", func(t *testing.T) {
		if got := pressureWithRealFloor(engine(64_842, false), 45_000); got != 64_842 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("test_rough_wins_when_larger", func(t *testing.T) {
		if got := pressureWithRealFloor(engine(30_000, false), 45_000); got != 45_000 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("test_stale_real_ignored_right_after_compaction", func(t *testing.T) {
		if got := pressureWithRealFloor(engine(64_842, true), 20_000); got != 20_000 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("test_zero_and_missing_real_are_safe", func(t *testing.T) {
		if pressureWithRealFloor(engine(0, false), 10_000) != 10_000 || pressureWithRealFloor(newStubEngine(1000, 0.75), 10_000) != 10_000 {
			t.Fatal("zero and missing real usage keep the rough figure")
		}
	})
}

func nativeCodexAgent(edit func(*AIAgent)) *AIAgent {
	engine := newStubEngine(0, 0)
	engine.ThresholdTokens = 765_000
	a := &AIAgent{apiMode: "codex_responses", provider: "openai-codex", model: "gpt-5.6", codexResponsesNativeCompaction: true,
		compressionEnabled: true, codexReasoningReplayEnabled: true, contextCompressor: engine}
	a.setBaseURL("https://chatgpt.com/backend-api/codex")
	if edit != nil {
		edit(a)
	}
	return a
}

func historyWithCheckpoint() []map[string]any {
	var rows []map[string]any
	for i := range 30 {
		rows = append(rows, map[string]any{"role": "user", "content": fmt.Sprintf("ask %d", i)},
			map[string]any{"role": "assistant", "content": "working " + strings.Repeat("tool output ", 400)},
			map[string]any{"role": "tool", "content": "result " + strings.Repeat("payload ", 400), "tool_call_id": fmt.Sprintf("call-%d", i)})
	}
	return append(rows, map[string]any{"role": "assistant", "content": "checkpointed turn", "codex_reasoning_items": []any{
		map[string]any{"type": "compaction", "encrypted_content": "blob", "_issuer_kind": "codex_backend"},
	}}, map[string]any{"role": "user", "content": "follow-up after checkpoint"})
}

func rowsAsAny(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

func TestReturnsNoneWhenApiModeIsNotCodexResponses(t *testing.T) {
	caseHome(t)
	a := nativeCodexAgent(func(a *AIAgent) { a.apiMode = "chat_completions" })
	if _, ok := estimateNativeResponsesPreflightTokens(context.Background(), a, rowsToMessages(historyWithCheckpoint()), "", nil); ok {
		t.Fatal("a chat completions route has no native estimate")
	}
}

func TestReturnsNoneWhenNativeCompactionIsDisabled(t *testing.T) {
	caseHome(t)
	a := nativeCodexAgent(func(a *AIAgent) { a.codexResponsesNativeCompaction = false })
	if _, ok := estimateNativeResponsesPreflightTokens(context.Background(), a, rowsToMessages(historyWithCheckpoint()), "", nil); ok {
		t.Fatal("disabled native compaction has no native estimate")
	}
}

func TestPrunedEstimateIsFarBelowDurableTranscript(t *testing.T) {
	caseHome(t)
	ctx := context.Background()
	messages := rowsToMessages(historyWithCheckpoint())
	generic := provider.EstimateRequestTokens(ctx, messages, "", nil, true)
	native, ok := estimateNativeResponsesPreflightTokens(ctx, nativeCodexAgent(nil), messages, "", nil)
	if !ok || generic <= native*2 || native >= 8_000 {
		t.Fatalf("native %d ok %v generic %d", native, ok, generic)
	}
}

func TestPreflightWrapperUsesPrunedEstimateWhenEligible(t *testing.T) {
	caseHome(t)
	ctx := context.Background()
	a := nativeCodexAgent(nil)
	messages := rowsToMessages(historyWithCheckpoint())
	native, ok := estimateNativeResponsesPreflightTokens(ctx, a, messages, "", nil)
	if wrapped := preflightRequestTokens(ctx, a, messages, ""); !ok || wrapped != native {
		t.Fatalf("wrapped %d native %d ok %v", wrapped, native, ok)
	}
}

func TestPreflightWrapperFallsBackToGenericWhenIneligible(t *testing.T) {
	caseHome(t)
	ctx := context.Background()
	a := nativeCodexAgent(func(a *AIAgent) { a.apiMode = "chat_completions" })
	messages := rowsToMessages(historyWithCheckpoint())
	if got, generic := preflightRequestTokens(ctx, a, messages, ""), provider.EstimateRequestTokens(ctx, messages, "", nil, true); got != generic {
		t.Fatalf("got %d generic %d", got, generic)
	}
}

func TestMidturnPressureUsesPrunedEstimateWhenEligible(t *testing.T) {
	caseHome(t)
	ctx := context.Background()
	a := nativeCodexAgent(nil)
	rows := append([]map[string]any{{"role": "system", "content": "be brief"}}, historyWithCheckpoint()...)
	messages := rowsToMessages(rows)
	native, ok := estimateNativeResponsesPreflightTokens(ctx, a, messages, "be brief", nil)
	generic := provider.EstimateMessagesTokens(messages, provider.CurrentImageTokenCost(ctx), true)
	if !ok || generic <= native*2 {
		t.Fatalf("native %d ok %v generic %d", native, ok, generic)
	}
	if got := midturnRequestPressureTokens(ctx, a, rowsAsAny(rows), "be brief", generic); got != native {
		t.Fatalf("mid turn pressure %d want the pruned estimate %d", got, native)
	}
}

func TestMidturnPressureFallsBackToGenericPlusToolsWhenIneligible(t *testing.T) {
	caseHome(t)
	ctx := context.Background()
	tool := map[string]any{"type": "function", "function": map[string]any{"name": "t", "parameters": map[string]any{}}}
	a := nativeCodexAgent(func(a *AIAgent) { a.apiMode, a.tools = "chat_completions", []any{tool} })
	rows := append([]map[string]any{{"role": "system", "content": "be brief"}}, historyWithCheckpoint()...)
	approx := provider.EstimateMessagesTokens(rowsToMessages(rows), provider.CurrentImageTokenCost(ctx), true)
	if got, want := midturnRequestPressureTokens(ctx, a, rowsAsAny(rows), "be brief", approx), approx+provider.EstimateToolsTokens([]map[string]any{tool}); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

const (
	sendTruncatedArgs = `{"content": "# chapter draft\nline one`
	sendLoneSurrogate = "hello \xed\xa0\xbd world"
)

func sendValidArgs(t *testing.T) string {
	return mustDumps(t, pyjson.ObjectOf("path", "a.txt", "text", "héllo"))
}

func adversarialHistory(t *testing.T) []any {
	return []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look at this " + sendLoneSurrogate},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
		}},
		map[string]any{"role": "assistant", "content": "ok " + sendLoneSurrogate, "reasoning_content": "thinking … " + sendLoneSurrogate,
			"reasoning_details": []any{map[string]any{"type": "reasoning.text", "text": "chaîne " + sendLoneSurrogate}},
			"tool_calls": []any{
				map[string]any{"id": "c1" + sendLoneSurrogate, "type": "function", "function": map[string]any{"name": "write_file", "arguments": sendTruncatedArgs}},
				map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "read_file", "arguments": sendValidArgs(t)}},
			}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "done"},
	}
}

func sendAPICopy(history []any) []any {
	out := make([]any, len(history))
	for i, m := range history {
		out[i] = cloneMessageForSend(m)
	}
	return out
}

func TestSendPathNeverMutatesHistory(t *testing.T) {
	caseHome(t)
	transforms := []struct {
		name string
		fn   func([]any)
	}{
		{"canonicalize/repair", canonicalizeAPIToolCalls},
		{"surrogate sanitizer", func(m []any) { state.SanitizeMessagesSurrogates(m) }},
		{"non-ascii sanitizer", func(m []any) { state.SanitizeMessagesNonASCII(m) }},
	}
	t.Run("test_full_pipeline_leaves_history_byte_identical", func(t *testing.T) {
		resetCanonCache(t)
		history, before := adversarialHistory(t), adversarialHistory(t)
		api := sendAPICopy(history)
		for _, row := range api {
			am := row.(map[string]any)
			if s, ok := am["content"].(string); ok {
				am["content"] = strings.TrimSpace(s)
			}
		}
		for _, tr := range transforms {
			tr.fn(api)
		}
		if !reflect.DeepEqual(history, before) {
			t.Fatal("a send path transform wrote through the api copy into the persisted history")
		}
	})
	t.Run("test_each_transform_in_isolation", func(t *testing.T) {
		for _, tr := range transforms {
			resetCanonCache(t)
			history, before := adversarialHistory(t), adversarialHistory(t)
			tr.fn(sendAPICopy(history))
			if !reflect.DeepEqual(history, before) {
				t.Fatalf("%s mutated persisted history", tr.name)
			}
		}
	})
	nested := func(rows []any, i int) map[string]any { return rows[i].(map[string]any) }
	call := func(rows []any, i int) map[string]any {
		return nested(rows, 1)["tool_calls"].([]any)[i].(map[string]any)
	}
	t.Run("test_clone_decouples_every_nested_container", func(t *testing.T) {
		history := adversarialHistory(t)
		api := sendAPICopy(history)
		part := func(rows []any, i int) map[string]any { return nested(rows, 0)["content"].([]any)[i].(map[string]any) }
		part(api, 0)["text"] = "MUTATED"
		part(api, 1)["image_url"].(map[string]any)["url"] = "MUTATED"
		call(api, 0)["function"].(map[string]any)["arguments"] = "{}"
		call(api, 1)["id"] = "MUTATED"
		nested(api, 1)["reasoning_details"].([]any)[0].(map[string]any)["text"] = "MUTATED"
		if !strings.HasPrefix(part(history, 0)["text"].(string), "look at this") || !strings.HasPrefix(part(history, 1)["image_url"].(map[string]any)["url"].(string), "data:") ||
			call(history, 0)["function"].(map[string]any)["arguments"] != sendTruncatedArgs || call(history, 1)["id"] != "c2" ||
			!strings.HasPrefix(nested(history, 1)["reasoning_details"].([]any)[0].(map[string]any)["text"].(string), "chaîne") {
			t.Fatalf("a write into the copy reached the history %v", history)
		}
	})
	t.Run("test_clone_shares_immutable_leaves", func(t *testing.T) {
		history := adversarialHistory(t)
		api := sendAPICopy(history)
		ptr := func(v any) uintptr { return reflect.ValueOf(v).Pointer() }
		if call(api, 1)["function"].(map[string]any)["arguments"] != call(history, 1)["function"].(map[string]any)["arguments"] {
			t.Fatal("leaf strings are shared")
		}
		if ptr(api[1]) == ptr(history[1]) || ptr(nested(api, 1)["tool_calls"]) == ptr(nested(history, 1)["tool_calls"]) ||
			ptr(call(api, 0)) == ptr(call(history, 0)) || ptr(call(api, 0)["function"]) == ptr(call(history, 0)["function"]) {
			t.Fatal("every container level must be a different object")
		}
	})
	t.Run("test_non_dict_messages_pass_through", func(t *testing.T) {
		sentinel := &struct{ n int }{1}
		if cloneMessageForSend(sentinel) != any(sentinel) || cloneMessageForSend("plain") != "plain" || cloneMessageForSend(nil) != nil {
			t.Fatal("non dict messages pass through")
		}
	})
}
