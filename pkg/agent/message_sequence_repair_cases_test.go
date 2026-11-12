package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Repairs the dict rows in place like AIAgent._repair_message_sequence and returns the repair count
func repairRows(t *testing.T, messages *[]map[string]any) int {
	t.Helper()
	n, err := state.RepairMessageSequence(messages)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func seqToolCall(id, name string, extra map[string]any) map[string]any {
	tc := map[string]any{"type": "function", "function": map[string]any{"name": name, "arguments": "{}"}}
	if id != "" {
		tc["id"] = id
	}
	for k, v := range extra {
		tc[k] = v
	}
	return tc
}

func seqAssistantCalls(calls ...map[string]any) map[string]any {
	list := make([]any, len(calls))
	for i, c := range calls {
		list[i] = c
	}
	return map[string]any{"role": "assistant", "content": "", "tool_calls": list}
}

func seqTool(id, content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": content}
}

func seqUser(content any) map[string]any { return map[string]any{"role": "user", "content": content} }

func seqAssistant(content any) map[string]any {
	return map[string]any{"role": "assistant", "content": content}
}

func rolesOf(messages []map[string]any) []string {
	out := make([]string, len(messages))
	for i, m := range messages {
		out[i], _ = m["role"].(string)
	}
	return out
}

func toolContents(messages []map[string]any) []any {
	var out []any
	for _, m := range messages {
		if m["role"] == "tool" {
			out = append(out, m["content"])
		}
	}
	return out
}

func seqRowsCopy(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, len(messages))
	for i, m := range messages {
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		out[i] = c
	}
	return out
}

func TestDropScaffoldingKeepsExecutedToolPair(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	executed := []*state.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", Content: "", ToolCalls: []state.ToolCall{{ID: "t1", Type: "function", Function: state.ToolCallFunction{Name: "f", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "t1", Content: "out"},
	}
	messages := append(append([]*state.Message{}, executed...), state.MessageFromDict(map[string]any{
		"role": "assistant", "content": "(empty)", "_empty_terminal_sentinel": true}))
	a := &AIAgent{}
	a.dropTrailingEmptyResponseScaffolding(&messages)
	if !reflect.DeepEqual(messages, executed) {
		t.Fatalf("messages %v", messages)
	}
}

func TestRepairPreservesUserContentWhenOneSideEmpty(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser(""), seqUser("real message")}
	repairRows(t, &messages)
	if !reflect.DeepEqual(messages, []map[string]any{{"role": "user", "content": "real message"}}) {
		t.Fatalf("messages %v", messages)
	}
}

func TestRepairPlainUserThenMarkerStaysAddressable(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{
		{"role": "user", "_row_id": 12134, "content": "the user's real prompt"},
		{"role": "user", "display_kind": "model_switch", "_row_id": 12135, "content": "[System: The active model for this chat has changed to ds4.]"},
	}
	if repairs := repairRows(t, &messages); repairs != 1 || len(messages) != 1 {
		t.Fatalf("repairs %d messages %v", repairs, messages)
	}
	survivor := messages[0]
	if survivor["role"] != "user" || survivor["_row_id"] != 12134 || pyTruthy(survivor["display_kind"]) {
		t.Fatalf("survivor %v", survivor)
	}
	if survivor["content"] != "the user's real prompt\n\n[System: The active model for this chat has changed to ds4.]" {
		t.Fatalf("content %q", survivor["content"])
	}
	absorbed, _ := survivor["_absorbed_row_ids"].([]any)
	found := false
	for _, id := range absorbed {
		found = found || fmt.Sprint(id) == "12135"
	}
	if !found {
		t.Fatalf("absorbed %v", survivor["_absorbed_row_ids"])
	}
}

func TestRepairDoesNotRewindOngoingDialogToolPair(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("Q1"), seqAssistantCalls(seqToolCall("t1", "f", nil)), seqTool("t1", "out"), seqUser("Q2")}
	original := seqRowsCopy(messages)
	if repairs := repairRows(t, &messages); repairs != 0 || !reflect.DeepEqual(messages, original) {
		t.Fatalf("repairs %d messages %v", repairs, messages)
	}
}

func TestRepairDropsStrayToolWithUnknownToolCallID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("hi"), seqAssistant("hello"), seqTool("orphan", "stray"), seqUser("real")}
	if repairs := repairRows(t, &messages); repairs < 1 {
		t.Fatalf("repairs %d", repairs)
	}
	for _, m := range messages {
		if m["role"] == "tool" {
			t.Fatalf("stray tool kept %v", messages)
		}
	}
}

func TestRepairKeepsToolMatchingCodexCallID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("do it"), seqAssistantCalls(seqToolCall("fc_123", "x", map[string]any{"call_id": "call_ABC"})), seqTool("call_ABC", "result"), seqUser("next")}
	if repairs := repairRows(t, &messages); repairs != 0 {
		t.Fatalf("repairs %d", repairs)
	}
	if !reflect.DeepEqual(rolesOf(messages), []string{"user", "assistant", "tool", "user"}) || messages[2]["tool_call_id"] != "call_ABC" {
		t.Fatalf("messages %v", messages)
	}
}

func TestRepairKeepsToolMatchingOnlyCallID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("do it"), seqAssistantCalls(seqToolCall("", "x", map[string]any{"call_id": "call_XYZ"})), seqTool("call_XYZ", "result"), seqUser("next")}
	if repairs := repairRows(t, &messages); repairs != 0 || len(toolContents(messages)) == 0 {
		t.Fatalf("repairs %d messages %v", repairs, messages)
	}
}

func TestRepairKeepsToolResultKeyedByResponseItemID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("do it"),
		seqAssistantCalls(seqToolCall("call_ABC", "x", map[string]any{"call_id": "call_ABC", "response_item_id": "fc_123"})), seqTool("fc_123", "result")}
	if repairs := repairRows(t, &messages); repairs != 0 || messages[len(messages)-1]["content"] != "result" {
		t.Fatalf("repairs %d messages %v", repairs, messages)
	}
}

func TestCoalesceToolCallIDUsesCallHalfOfCompositeID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := state.CoalesceToolCallID(map[string]any{"id": "call_ABC|fc_123"}); got != "call_ABC" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeKeepsParallelResultsKeyedByResponsesIDVariant(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	const batch = 8
	calls := make([]map[string]any, batch)
	messages := []map[string]any{seqUser("run the independent calls")}
	for i := range batch {
		calls[i] = seqToolCall(fmt.Sprintf("fc_%d", i), "read_file", map[string]any{"call_id": fmt.Sprintf("call_%d", i)})
	}
	messages = append(messages, seqAssistantCalls(calls...))
	for i := range batch {
		messages = append(messages, seqTool(fmt.Sprintf("fc_%d", i), fmt.Sprintf("real-result-%d", i)))
	}
	out := sanitizeAPIMessages(context.Background(), "", messages)
	var ids, contents []any
	for _, m := range out {
		if m["role"] == "tool" {
			ids, contents = append(ids, m["tool_call_id"]), append(contents, m["content"])
		}
	}
	var wantIDs, wantContents []any
	for i := range batch {
		wantIDs, wantContents = append(wantIDs, fmt.Sprintf("fc_%d", i)), append(wantContents, fmt.Sprintf("real-result-%d", i))
	}
	if !reflect.DeepEqual(ids, wantIDs) || !reflect.DeepEqual(contents, wantContents) {
		t.Fatalf("ids %v contents %v", ids, contents)
	}
}

func TestRepairKeepsTwoParallelCallsAnsweredByMixedVariants(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("do both"), seqAssistantCalls(
		seqToolCall("call_1", "x", map[string]any{"call_id": "call_1", "response_item_id": "fc_1"}),
		seqToolCall("call_2", "y", map[string]any{"call_id": "call_2", "response_item_id": "fc_2"})),
		seqTool("call_1", "r1"), seqTool("fc_2", "r2")}
	if repairs := repairRows(t, &messages); repairs != 0 || !reflect.DeepEqual(toolContents(messages), []any{"r1", "r2"}) {
		t.Fatalf("repairs %d messages %v", repairs, messages)
	}
}

func TestRepairMergeDropsStaleAPIContentSidecarOnSurvivingTurn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("Q1"), {"role": "assistant", "content": "first reply", "api_content": "first reply (stale pre-merge bytes)"}, seqAssistant("second reply")}
	if repairs := repairRows(t, &messages); repairs != 1 || len(messages) != 2 {
		t.Fatalf("repairs %d messages %v", repairs, messages)
	}
	if _, ok := messages[1]["api_content"]; ok || messages[1]["content"] != "first reply\nsecond reply" {
		t.Fatalf("merged %v", messages[1])
	}
}

func TestRepairMergePreservesAPIContentSidecarWhenContentUnchanged(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("Q1"), {"role": "assistant", "content": "clean", "api_content": "wire bytes"}, seqAssistant(nil)}
	if repairs := repairRows(t, &messages); repairs != 1 || len(messages) != 2 || messages[1]["content"] != "clean" || messages[1]["api_content"] != "wire bytes" {
		t.Fatalf("repairs %d messages %v", repairs, messages)
	}
}

func TestRepairMergePreservesAPIContentSidecarWhenContentUnchangedByEmptyString(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("Q1"), {"role": "assistant", "content": "clean", "api_content": "wire bytes"}, seqAssistant("")}
	if repairs := repairRows(t, &messages); repairs != 1 || len(messages) != 2 || messages[1]["content"] != "clean" || messages[1]["api_content"] != "wire bytes" {
		t.Fatalf("repairs %d messages %v", repairs, messages)
	}
}

func TestRepairMergePreservesAPIContentSidecarWithMultimodalContent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("Q1"), {"role": "assistant", "content": "clean", "api_content": "wire bytes"},
		seqAssistant([]any{map[string]any{"type": "text", "text": "img context"}})}
	repairRows(t, &messages)
	if len(messages) != 2 || messages[1]["content"] != "clean" || messages[1]["api_content"] != "wire bytes" {
		t.Fatalf("messages %v", messages)
	}
}

func TestSanitizeConsumesAllResponsesIDVariantsForDuplicateResult(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{
		seqAssistantCalls(seqToolCall("call_ABC", "x", map[string]any{"call_id": "call_ABC", "response_item_id": "fc_123"})),
		seqTool("fc_123", "real"), seqTool("call_ABC", "replayed"),
	}
	if got := toolContents(sanitizeAPIMessages(context.Background(), "", messages)); !reflect.DeepEqual(got, []any{"real"}) {
		t.Fatalf("tool contents %v", got)
	}
}

func TestToolExecutorUsesCanonicalResponsesPairingID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := state.CoalesceToolCallID(state.ToolCall{ID: "fc_123", Extra: map[string]any{"call_id": "call_ABC"}}); got != "call_ABC" {
		t.Fatalf("got %q", got)
	}
	if got := state.CoalesceToolCallID(state.ToolCall{ID: "call_ABC|fc_123"}); got != "call_ABC" {
		t.Fatalf("got %q", got)
	}
}

func TestCursorClampedWhenCompactionShrinksBelowCursor(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{lastFlushedDBIdx: 2}
	messages := []*state.Message{{Role: "user", Content: "first"}, {Role: "user", Content: "second"}}
	repairs, err := repairMessageSequenceWithCursor(a, &messages)
	if err != nil || repairs != 1 || len(messages) != 1 || a.lastFlushedDBIdx != 1 {
		t.Fatalf("repairs %d err %v len %d cursor %d", repairs, err, len(messages), a.lastFlushedDBIdx)
	}
}

func TestCursorRewindsWhenCompactionHappensBeforeCursor(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	unflushed := &state.Message{Role: "assistant", Content: "answer"}
	messages := []*state.Message{{Role: "user", Content: "first"}, {Role: "user", Content: "second"}, unflushed}
	a := &AIAgent{lastFlushedDBIdx: 2}
	repairs, err := repairMessageSequenceWithCursor(a, &messages)
	if err != nil || repairs != 1 || len(messages) != 2 || a.lastFlushedDBIdx != 1 {
		t.Fatalf("repairs %d err %v len %d cursor %d", repairs, err, len(messages), a.lastFlushedDBIdx)
	}
	if messages[a.lastFlushedDBIdx] != unflushed {
		t.Fatal("the cursor does not point at the unflushed assistant")
	}
}

func TestFlushGuardClampsOvershootingCursor(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	db, err := state.Open(filepath.Join(h, "state.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	a := &AIAgent{sessionDB: db, sessionDBCreated: true, sessionID: "s1", lastFlushedDBIdx: 5}
	setSessionStateDefaults(a)
	a.lastFlushedDBIdx = 5
	a.sessionDBCreated = true
	messages := []map[string]any{{"role": "user", "content": "q"}, {"role": "assistant", "content": "a"}}
	if _, err := a.flushMessagesToSessionDBWithHistory(messages, []map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if a.lastFlushedDBIdx != 2 {
		t.Fatalf("cursor %d", a.lastFlushedDBIdx)
	}
}

func sanitizeRows(messages []map[string]any) []map[string]any {
	return sanitizeAPIMessages(context.Background(), "", append([]map[string]any(nil), messages...))
}

func toolIDs(messages []map[string]any) []any {
	var out []any
	for _, m := range messages {
		if m["role"] == "tool" {
			out = append(out, m["tool_call_id"])
		}
	}
	return out
}

func assistantCallIDs(m map[string]any) []any {
	var out []any
	calls, _ := m["tool_calls"].([]any)
	for _, c := range calls {
		out = append(out, c.(map[string]any)["id"])
	}
	return out
}

func assistantsWithCalls(messages []map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range messages {
		if m["role"] == "assistant" && pyTruthy(m["tool_calls"]) {
			out = append(out, m)
		}
	}
	return out
}

func firstAssistant(messages []map[string]any) map[string]any {
	for _, m := range messages {
		if m["role"] == "assistant" {
			return m
		}
	}
	return nil
}

func TestSanitizeDeduplicatesDuplicateToolResults(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("hi"), {"role": "assistant", "content": nil, "tool_calls": []any{seqToolCall("call_X", "foo", nil)}},
		seqTool("call_X", "A"), seqTool("call_X", "B (duplicate)"), seqAssistant("done")})
	if got := toolIDs(out); !reflect.DeepEqual(got, []any{"call_X"}) {
		t.Fatalf("tool ids %v", got)
	}
}

func TestSanitizeDeduplicatesDuplicateAssistantToolCallIDs(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{{"role": "assistant", "content": nil, "tool_calls": []any{seqToolCall("call_Y", "foo", nil), seqToolCall("call_Y", "bar", nil)}},
		seqTool("call_Y", "r")})
	if got := assistantCallIDs(firstAssistant(out)); !reflect.DeepEqual(got, []any{"call_Y"}) {
		t.Fatalf("call ids %v", got)
	}
}

func TestSanitizePreservesDistinctToolCallIDs(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{{"role": "assistant", "content": nil, "tool_calls": []any{seqToolCall("call_A", "a", nil), seqToolCall("call_B", "b", nil)}},
		seqTool("call_A", "ra"), seqTool("call_B", "rb")})
	if got := assistantCallIDs(firstAssistant(out)); !reflect.DeepEqual(got, []any{"call_A", "call_B"}) {
		t.Fatalf("call ids %v", got)
	}
	if got := toolIDs(out); !reflect.DeepEqual(got, []any{"call_A", "call_B"}) {
		t.Fatalf("tool ids %v", got)
	}
}

func TestSanitizeDropsToolResultWithMissingToolCallID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("hi"), seqAssistantCalls(seqToolCall("call_Z", "f", nil)), seqTool("call_Z", "real result"),
		seqTool("", "no id"), {"role": "tool", "content": "id key entirely absent"}})
	if got := toolIDs(out); !reflect.DeepEqual(got, []any{"call_Z"}) {
		t.Fatalf("tool ids %v", got)
	}
}

const constantToolCallID = "ZsSt4SkIFMRz0HtqT7MTlimNvzlKM896"

func constantCall(cid string) map[string]any {
	return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{seqToolCall(cid, "terminal", nil)}}
}

func constantResult(cid, content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": cid, "name": "terminal", "content": content}
}

func TestSanitizeKeepsResultsWhenServerReusesOneToolCallID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("do three steps")}
	for i := range 3 {
		messages = append(messages, constantCall(constantToolCallID), constantResult(constantToolCallID, fmt.Sprintf("step %d output", i)))
	}
	out := sanitizeRows(messages)
	if got := toolContents(out); !reflect.DeepEqual(got, []any{"step 0 output", "step 1 output", "step 2 output"}) {
		t.Fatalf("results %v", got)
	}
	if n := len(assistantsWithCalls(out)); n != 3 {
		t.Fatalf("calls %d", n)
	}
}

func TestSanitizeStillDropsReplayedResultForRetiredCall(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("hi"), constantCall(constantToolCallID), constantResult(constantToolCallID, "real"),
		constantResult(constantToolCallID, "replayed by a retry/resume glitch")})
	if got := toolContents(out); !reflect.DeepEqual(got, []any{"real"}) {
		t.Fatalf("results %v", got)
	}
}

func TestSanitizePreservesDeterministicLocalIDsAcrossTurns(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("make images")}
	for _, turn := range []string{"one", "two"} {
		messages = append(messages, map[string]any{"role": "assistant", "content": turn,
			"tool_calls": []any{seqToolCall("image_generate:0", "image_generate", nil), seqToolCall("image_generate:1", "image_generate", nil)}},
			seqTool("image_generate:0", "imgA-"+turn), seqTool("image_generate:1", "imgB-"+turn))
	}
	out := sanitizeRows(messages)
	assistants := assistantsWithCalls(out)
	if len(assistants) != 2 {
		t.Fatalf("assistants %v", assistants)
	}
	for _, m := range assistants {
		if got := assistantCallIDs(m); !reflect.DeepEqual(got, []any{"image_generate:0", "image_generate:1"}) {
			t.Fatalf("call ids %v", got)
		}
	}
	counts := map[any]int{}
	for _, id := range toolIDs(out) {
		counts[id]++
	}
	if len(toolIDs(out)) != 4 || counts["image_generate:0"] != 2 || counts["image_generate:1"] != 2 {
		t.Fatalf("tool ids %v", toolIDs(out))
	}
}

func TestSanitizeKeepsAllResultsOverFiftyTurnConstantIDSession(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("Run 50 steps.")}
	for step := range 50 {
		messages = append(messages, constantCall(constantToolCallID), constantResult(constantToolCallID, fmt.Sprintf("completed-step-%d", step)))
	}
	out := sanitizeRows(messages)
	results := toolContents(out)
	if len(assistantsWithCalls(out)) != 50 || len(results) != 50 || results[49] != "completed-step-49" {
		t.Fatalf("calls %d results %d", len(assistantsWithCalls(out)), len(results))
	}
}

func TestSanitizeDropsResultWithNoPrecedingCall(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if got := toolContents(sanitizeRows([]map[string]any{seqUser("hi"), constantResult("id_never_requested", "orphan")})); len(got) != 0 {
		t.Fatalf("results %v", got)
	}
}

func TestSanitizeDropsEmptyToolCallsArray(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("hi"), {"role": "assistant", "content": "answer", "tool_calls": []any{}}})
	assistant := firstAssistant(out)
	if _, ok := assistant["tool_calls"]; ok || assistant["content"] != "answer" {
		t.Fatalf("assistant %v", assistant)
	}
}

func TestRepairDropsStaleEmptyToolCallsOnMergedAssistant(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("hi"), {"role": "assistant", "content": "first", "tool_calls": []any{}}, seqAssistant("second")}
	if n := repairRows(t, &messages); n < 0 {
		t.Fatalf("repairs %d", n)
	}
	var assistants []map[string]any
	for _, m := range messages {
		if m["role"] == "assistant" {
			assistants = append(assistants, m)
		}
	}
	if len(assistants) != 1 {
		t.Fatalf("assistants %v", assistants)
	}
	if _, ok := assistants[0]["tool_calls"]; ok || !strings.Contains(fmt.Sprint(assistants[0]["content"]), "second") {
		t.Fatalf("assistant %v", assistants[0])
	}
}

func TestSanitizeInterruptedCallStubbedAndReplayKept(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	callA := func() map[string]any { return seqToolCall("call_A", "foo", nil) }
	out := sanitizeRows([]map[string]any{seqUser("step 1"),
		{"role": "assistant", "content": "running", "tool_calls": []any{callA()}},
		{"role": "assistant", "content": "retrying", "tool_calls": []any{callA()}},
		seqTool("call_A", "result 1")})
	if !reflect.DeepEqual(rolesOf(out), []string{"user", "assistant", "tool", "assistant", "tool"}) {
		t.Fatalf("roles %v", rolesOf(out))
	}
	if out[2]["tool_call_id"] != "call_A" || !strings.Contains(fmt.Sprint(out[2]["content"]), "Result unavailable") {
		t.Fatalf("stub %v", out[2])
	}
	for _, m := range out {
		if m["role"] == "assistant" {
			if got := assistantCallIDs(m); !reflect.DeepEqual(got, []any{"call_A"}) {
				t.Fatalf("assistant %v", m)
			}
		}
	}
	if out[4]["content"] != "result 1" {
		t.Fatalf("replay result %v", out[4])
	}
}

func TestRepairDropsDuplicateToolResultKeyedOnSiblingID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("task"), seqAssistantCalls(seqToolCall("fc_1", "f", map[string]any{"call_id": "call_1"})),
		seqTool("call_1", "first"), seqTool("fc_1", "duplicate")}
	if n := repairRows(t, &messages); n != 1 || !reflect.DeepEqual(toolContents(messages), []any{"first"}) {
		t.Fatalf("repairs %d messages %v", n, messages)
	}
}

func TestRepairKeepsBothResultsForTwoCodexCallsMixedKeys(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("task"), seqAssistantCalls(seqToolCall("fc_1", "f", map[string]any{"call_id": "call_1"}),
		seqToolCall("fc_2", "g", map[string]any{"call_id": "call_2"})), seqTool("call_1", "r1"), seqTool("fc_2", "r2")}
	if n := repairRows(t, &messages); n != 0 || !reflect.DeepEqual(toolContents(messages), []any{"r1", "r2"}) {
		t.Fatalf("repairs %d messages %v", n, messages)
	}
}

func TestSanitizeDedupPassKeepsBatchResultsKeyedOnDivergentID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	calls := make([]map[string]any, 4)
	for i := range 4 {
		calls[i] = seqToolCall(fmt.Sprintf("fc_%d", i), "terminal", map[string]any{"call_id": fmt.Sprintf("call_%d", i)})
	}
	messages := []map[string]any{seqUser("task"), seqAssistantCalls(calls...)}
	for i := range 4 {
		messages = append(messages, seqTool(fmt.Sprintf("fc_%d", i), fmt.Sprintf("REAL %d", i)))
	}
	got := toolContents(sanitizeAPIMessages(context.Background(), "", messages))
	if !reflect.DeepEqual(got, []any{"REAL 0", "REAL 1", "REAL 2", "REAL 3"}) {
		t.Fatalf("results %v", got)
	}
}

func TestSanitizeDedupPassStillDropsResultReplayedOnSiblingID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("task"), seqAssistantCalls(seqToolCall("fc_1", "f", map[string]any{"call_id": "call_1"})),
		seqTool("fc_1", "first"), seqTool("call_1", "sibling replay")}
	if got := toolContents(sanitizeAPIMessages(context.Background(), "", messages)); !reflect.DeepEqual(got, []any{"first"}) {
		t.Fatalf("results %v", got)
	}
}

func TestSanitizeDedupPassRearmsConstantLlamacppID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("task"), seqAssistantCalls(seqToolCall("call_K", "f", nil)), seqTool("call_K", "round1"),
		seqAssistantCalls(seqToolCall("call_K", "f", nil)), seqTool("call_K", "round2")}
	if got := toolContents(sanitizeAPIMessages(context.Background(), "", messages)); !reflect.DeepEqual(got, []any{"round1", "round2"}) {
		t.Fatalf("results %v", got)
	}
}

func TestSanitizeKeepsResultKeyedOnCompositeBridgeID(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("task"), seqAssistantCalls(seqToolCall("fc_1", "f", map[string]any{"call_id": "call_1"})), seqTool("call_1|fc_1", "REAL")}
	if got := toolContents(sanitizeAPIMessages(context.Background(), "", messages)); !reflect.DeepEqual(got, []any{"REAL"}) {
		t.Fatalf("composite keyed result %v", got)
	}
	messages = []map[string]any{seqUser("task"), seqAssistantCalls(seqToolCall("call_2|fc_2", "f", nil)), seqTool("call_2", "REAL bare")}
	if got := toolContents(sanitizeAPIMessages(context.Background(), "", messages)); !reflect.DeepEqual(got, []any{"REAL bare"}) {
		t.Fatalf("bare keyed result %v", got)
	}
}

func TestCompressorSanitizeKeepsCompositeKeyedPair(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cc := &ContextCompressor{}
	cc.quietMode = true
	out := cc.sanitizeToolPairs([]map[string]any{seqAssistantCalls(seqToolCall("fc_7", "s", map[string]any{"call_id": "call_7"})), seqTool("call_7|fc_7", "res"), seqUser("next")})
	if asst := firstAssistant(out); !pyTruthy(asst["tool_calls"]) {
		t.Fatalf("valid tool_call stripped %v", asst)
	}
	if got := toolContents(out); !reflect.DeepEqual(got, []any{"res"}) {
		t.Fatalf("results %v", got)
	}
	out = cc.sanitizeToolPairs([]map[string]any{seqAssistant("hi"), seqTool("call_z|fc_z", "orphan"), seqUser("next")})
	if len(toolContents(out)) != 0 {
		t.Fatalf("composite orphan kept %v", out)
	}
}

func assistantWithCall(callID, content string) map[string]any {
	return map[string]any{"role": "assistant", "content": content, "tool_calls": []any{seqToolCall(callID, "f", nil)}}
}

func TestRepairPrunesToolCallWhoseResultWasDisplaced(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("do it"), assistantWithCall("call_A", ""), assistantWithCall("call_B", "second"),
		seqTool("call_B", "out"), seqUser("meanwhile"), seqTool("call_A", "late result")}
	if n := repairRows(t, &messages); n < 1 {
		t.Fatalf("repairs %d", n)
	}
	var assistants []map[string]any
	for _, m := range messages {
		if m["role"] == "assistant" {
			assistants = append(assistants, m)
		}
	}
	if len(assistants) != 1 || !reflect.DeepEqual(assistantCallIDs(assistants[0]), []any{"call_B"}) || assistants[0]["content"] != "second" {
		t.Fatalf("assistants %v", assistants)
	}
	if ids := toolIDs(messages); !reflect.DeepEqual(ids, []any{"call_B"}) {
		t.Fatalf("tools %v", ids)
	}
}

func TestRepairDropsTurnWhenPrunedCallsWereOnlyPayload(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("do it"), assistantWithCall("call_A", ""), seqAssistant(""), seqUser("redirected"), seqTool("call_A", "late")}
	if n := repairRows(t, &messages); n < 2 {
		t.Fatalf("repairs %d", n)
	}
	var users []map[string]any
	for _, m := range messages {
		if m["role"] == "assistant" {
			t.Fatalf("assistant kept %v", messages)
		}
		if m["role"] == "user" {
			users = append(users, m)
		}
	}
	text := fmt.Sprint(users[0]["content"])
	if len(users) != 1 || !strings.Contains(text, "do it") || !strings.Contains(text, "redirected") {
		t.Fatalf("users %v", users)
	}
}

func TestRepairKeepsCallsAnsweredWithinFollowingRun(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("Q1"), assistantWithCall("t1", ""), seqTool("t1", "out"), seqUser("Q2")}
	original := seqRowsCopy(messages)
	if n := repairRows(t, &messages); n != 0 || !reflect.DeepEqual(messages, original) {
		t.Fatalf("repairs %d messages %v", n, messages)
	}
}

func TestSanitizeStubsCallUnansweredPositionallyEvenIfResultExistsElsewhere(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("do it"), assistantWithCall("call_A", ""), seqUser("meanwhile"), seqTool("call_A", "late result")})
	if !reflect.DeepEqual(rolesOf(out), []string{"user", "assistant", "tool", "user"}) {
		t.Fatalf("roles %v", rolesOf(out))
	}
	if out[2]["tool_call_id"] != "call_A" || !strings.Contains(fmt.Sprint(out[2]["content"]), "Result unavailable") {
		t.Fatalf("stub %v", out[2])
	}
	for _, m := range out {
		if m["content"] == "late result" {
			t.Fatal("displaced result kept")
		}
	}
}

func TestSanitizeDropsResultAppearingBeforeItsCall(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("do it"), seqTool("call_A", "out"), assistantWithCall("call_A", ""), seqTool("call_A", "out")})
	if ids := toolIDs(out); !reflect.DeepEqual(ids, []any{"call_A"}) {
		t.Fatalf("tools %v", ids)
	}
}

func TestSanitizePositionalPairingUntouchedValidTranscript(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("do it"), assistantWithCall("call_A", ""), seqTool("call_A", "out"),
		assistantWithCall("call_B", ""), seqTool("call_B", "out"), seqAssistant("done")})
	if !reflect.DeepEqual(rolesOf(out), []string{"user", "assistant", "tool", "assistant", "tool", "assistant"}) {
		t.Fatalf("roles %v", rolesOf(out))
	}
	for _, m := range out {
		if strings.Contains(fmt.Sprint(m["content"]), "Result unavailable") {
			t.Fatalf("stub injected %v", m)
		}
	}
}

func TestSanitizeStubsReplayedCallMaskedByHistoricalResult(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("step 1"), assistantWithCall("call_old", ""), seqTool("call_old", "historical result"), seqUser("step 2"),
		seqAssistantCalls(seqToolCall("call_old", "f", nil), seqToolCall("call_new", "g", nil)), seqTool("call_new", "fresh result")})
	if !reflect.DeepEqual(rolesOf(out), []string{"user", "assistant", "tool", "user", "assistant", "tool", "tool"}) {
		t.Fatalf("roles %v", rolesOf(out))
	}
	second := out[4:]
	if ids := toolIDs(second); !reflect.DeepEqual(ids, []any{"call_new", "call_old"}) {
		t.Fatalf("second run ids %v", ids)
	}
	if stub := second[2]; stub["role"] != "tool" || stub["tool_call_id"] != "call_old" || !strings.Contains(fmt.Sprint(stub["content"]), "Result unavailable") {
		t.Fatalf("stub %v", stub)
	}
	contents := fmt.Sprint(toolContents(out))
	if !strings.Contains(contents, "historical result") || !strings.Contains(contents, "fresh result") {
		t.Fatalf("contents %v", contents)
	}
}

func TestSanitizeStubsInterruptedFirstOccurrenceKeepsReplayPair(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("do it"), assistantWithCall("call_A", ""), seqUser("resumed"), assistantWithCall("call_A", ""), seqTool("call_A", "real result")})
	if !reflect.DeepEqual(rolesOf(out), []string{"user", "assistant", "tool", "user", "assistant", "tool"}) {
		t.Fatalf("roles %v", rolesOf(out))
	}
	if out[2]["tool_call_id"] != "call_A" || !strings.Contains(fmt.Sprint(out[2]["content"]), "Result unavailable") || out[5]["content"] != "real result" {
		t.Fatalf("out %v", out)
	}
}

func TestSanitizeRealignsBridgedToolResultNameWithCallName(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("file an issue"),
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function",
			"function": map[string]any{"name": "tool_call", "arguments": `{"name": "mcp__github__create_issue", "arguments": {"title": "Bug"}}`}}}},
		{"role": "tool", "name": "mcp__github__create_issue", "tool_name": "mcp__github__create_issue", "tool_call_id": "call_1", "content": `{"number": 123}`}}
	out := sanitizeRows(messages)
	var result map[string]any
	for _, m := range out {
		if m["role"] == "tool" {
			result = m
			break
		}
	}
	if result["name"] != "tool_call" || result["tool_name"] != "mcp__github__create_issue" || messages[2]["name"] != "mcp__github__create_issue" {
		t.Fatalf("result %v original %v", result, messages[2])
	}
}

func TestSanitizeLeavesAlreadyMatchingToolResultNameAlone(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("weather?"), {"role": "assistant", "content": nil, "tool_calls": []any{seqToolCall("call_1", "get_weather", nil)}},
		{"role": "tool", "name": "get_weather", "tool_call_id": "call_1", "content": "sunny"}})
	var names []any
	for _, m := range out {
		if m["role"] == "tool" {
			names = append(names, m["name"])
		}
	}
	if !reflect.DeepEqual(names, []any{"get_weather"}) {
		t.Fatalf("names %v", names)
	}
}

func TestSanitizeDoesNotInventANameOnUnnamedToolResults(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("run it"), {"role": "assistant", "content": nil, "tool_calls": []any{seqToolCall("call_1", "terminal", nil)}}, seqTool("call_1", "ok")}
	if out := sanitizeRows(messages); !reflect.DeepEqual(out, messages) {
		t.Fatalf("out %v", out)
	}
}

func TestSanitizeRealignsBridgedNameWhenCallIDIsPadded(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("file an issue"), seqAssistantCalls(seqToolCall(" call_1 ", "tool_call", nil)),
		{"role": "tool", "name": "mcp__github__create_issue", "tool_call_id": "call_1", "content": "{}"}})
	var names []any
	for _, m := range out {
		if m["role"] == "tool" {
			names = append(names, m["name"])
		}
	}
	if !reflect.DeepEqual(names, []any{"tool_call"}) {
		t.Fatalf("names %v", names)
	}
}

func TestSanitizeDropsBridgedResultWhoseCallFrameWasPruned(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	out := sanitizeRows([]map[string]any{seqUser("file an issue"), {"role": "tool", "name": "mcp__github__create_issue", "tool_call_id": "call_1", "content": "{}"}})
	if !reflect.DeepEqual(rolesOf(out), []string{"user"}) {
		t.Fatalf("roles %v", rolesOf(out))
	}
}

func TestRepairUserMergePopsPersistMarkerOnStampedSurvivor(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{{"role": "user", "content": "first", state.DBPersistedMarker: true}, seqUser("second")}
	if n := repairRows(t, &messages); n != 1 || len(messages) != 1 || messages[0]["content"] != "first\n\nsecond" {
		t.Fatalf("repairs %d messages %v", n, messages)
	}
	if _, ok := messages[0][state.DBPersistedMarker]; ok {
		t.Fatalf("marker kept %v", messages[0])
	}
}

func TestRepairAssistantMergePopsPersistMarkerOnContentRewrite(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []map[string]any{seqUser("Q1"), {"role": "assistant", "content": "first reply", state.DBPersistedMarker: true}, seqAssistant("second reply")}
	if n := repairRows(t, &messages); n != 1 || messages[1]["content"] != "first reply\nsecond reply" {
		t.Fatalf("repairs %d messages %v", n, messages)
	}
	if _, ok := messages[1][state.DBPersistedMarker]; ok {
		t.Fatalf("marker kept %v", messages[1])
	}
}

func TestRepairPruneUnansweredToolCallsPopsPersistMarker(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	stamped := map[string]any{"role": "assistant", "content": "calling tools",
		"tool_calls": []any{seqToolCall("t1", "f", nil), seqToolCall("t2", "g", nil)}, state.DBPersistedMarker: true}
	messages := []map[string]any{seqUser("Q1"), stamped, seqTool("t1", "out1"), seqUser("next")}
	if n := repairRows(t, &messages); n < 1 {
		t.Fatalf("repairs %d", n)
	}
	var surviving map[string]any
	for _, m := range messages {
		if reflect.ValueOf(m).Pointer() == reflect.ValueOf(stamped).Pointer() {
			surviving = m
		}
	}
	if surviving == nil || !reflect.DeepEqual(assistantCallIDs(surviving), []any{"t1"}) {
		t.Fatalf("surviving %v", surviving)
	}
	if _, ok := surviving[state.DBPersistedMarker]; ok {
		t.Fatalf("marker kept %v", surviving)
	}
}

func TestRepairCursorInvalidatesScanPrefixWhenStampedDictDirtied(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	messages := []*state.Message{state.MessageFromDict(map[string]any{"role": "user", "content": "first", state.DBPersistedMarker: true}), {Role: "user", Content: "second"}}
	a := &AIAgent{lastFlushedDBIdx: 2, dbFlushScanPrefix: append([]*state.Message(nil), messages...)}
	repairs, err := repairMessageSequenceWithCursor(a, &messages)
	if err != nil || repairs != 1 {
		t.Fatalf("repairs %d err %v", repairs, err)
	}
	if _, ok := state.MessageToDict(messages[0])[state.DBPersistedMarker]; ok {
		t.Fatalf("marker kept %v", messages[0])
	}
	if a.dbFlushScanPrefix != nil {
		t.Fatalf("scan prefix kept %v", a.dbFlushScanPrefix)
	}
}

func TestRepairDecodeOfDurableSentinelRowDoesNotReappend(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	parts := []any{map[string]any{"type": "text", "text": "look"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", 2000)}}}
	body, err := pyjson.Dumps(parts)
	if err != nil {
		t.Fatal(err)
	}
	encoded := "\x00json:" + body
	ctx := context.Background()
	db, err := state.Open(filepath.Join(h, "t.db"), state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const sid = "20260928_000000_dup"
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli", Model: "test/model"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []map[string]any{{"role": "user", "content": encoded}, {"role": "assistant", "content": "ok"}} {
		if _, err := db.AppendMessage(ctx, sid, row, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	activeRoles := func() []string {
		rows, err := db.GetMessagesAsConversation(ctx, sid, state.ConversationOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return rolesOf(rows)
	}
	if before := activeRoles(); !reflect.DeepEqual(before, []string{"user", "assistant"}) {
		t.Fatalf("before %v", before)
	}
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "test-key", BaseURL: "https://openrouter.ai/api/v1", Model: "test/model", QuietMode: true,
		SessionDB: db, SessionID: sid, SkipContextFiles: true, SkipMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.sessionDBCreated = true
	a.lastFlushedDBIdx = 2
	messages := []*state.Message{
		state.MessageFromDict(map[string]any{"role": "user", "content": encoded, state.DBPersistedMarker: true}),
		state.MessageFromDict(map[string]any{"role": "assistant", "content": "ok", state.DBPersistedMarker: true}),
	}
	repairs, err := repairMessageSequenceWithCursor(a, &messages)
	if err != nil || repairs != 0 {
		t.Fatalf("repairs %d err %v", repairs, err)
	}
	if !reflect.DeepEqual(messages[0].Content, parts) || state.MessageToDict(messages[0])[state.DBPersistedMarker] != true {
		t.Fatalf("decoded row %v", state.MessageToDict(messages[0]))
	}
	for range 2 {
		if err := a.persistSession(&messages, nil); err != nil {
			t.Fatal(err)
		}
	}
	if after := activeRoles(); !reflect.DeepEqual(after, []string{"user", "assistant"}) {
		t.Fatalf("flush changed the durable transcript: %v", after)
	}
}
