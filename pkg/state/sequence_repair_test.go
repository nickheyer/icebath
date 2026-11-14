package state

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func seqRepair(t *testing.T, msgs *[]map[string]any) int {
	t.Helper()
	n, err := RepairMessageSequence(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func seqCall(id string, extra ...string) map[string]any {
	tc := map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}
	for i := 0; i+1 < len(extra); i += 2 {
		tc[extra[i]] = extra[i+1]
	}
	if id == "" {
		delete(tc, "id")
	}
	return tc
}

func seqAssistant(content any, calls ...map[string]any) map[string]any {
	m := map[string]any{"role": "assistant", "content": content}
	if len(calls) > 0 {
		list := make([]any, len(calls))
		for i, c := range calls {
			list[i] = c
		}
		m["tool_calls"] = list
	}
	return m
}

func seqUser(content any) map[string]any { return map[string]any{"role": "user", "content": content} }

func seqTool(id string, content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": id, "content": content}
}

func seqCopies(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		out[i] = copyMessage(m)
	}
	return out
}

func seqRoles(msgs []map[string]any) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, pyStr(m["role"]))
	}
	return out
}

func seqToolContents(msgs []map[string]any) []string {
	var out []string
	for _, m := range msgs {
		if m["role"] == "tool" {
			out = append(out, pyStr(m["content"]))
		}
	}
	return out
}

func TestRepairMergesConsecutiveUserMessages(t *testing.T) {
	msgs := []map[string]any{seqUser("first"), seqUser("second")}
	if n := seqRepair(t, &msgs); n != 1 || len(msgs) != 1 || msgs[0]["role"] != "user" || msgs[0]["content"] != "first\n\nsecond" {
		t.Fatalf("%d %v", n, msgs)
	}
	msgs = []map[string]any{seqUser(""), seqUser("real message")}
	seqRepair(t, &msgs)
	if !reflect.DeepEqual(msgs, []map[string]any{seqUser("real message")}) {
		t.Fatalf("one side empty %v", msgs)
	}
}

func TestRepairMarkerUserMergeKeepsPlainRowAddressable(t *testing.T) {
	marker := "[System: The active model for this chat has changed to ds4.]"
	msgs := []map[string]any{
		seqAssistant("reply"),
		{"role": "user", "display_kind": "model_switch", "_row_id": int64(12134), "content": marker},
		{"role": "user", "_row_id": int64(12135), "content": "the user's real prompt"},
	}
	if n := seqRepair(t, &msgs); n != 1 || len(msgs) != 2 {
		t.Fatalf("%d %v", n, msgs)
	}
	merged := msgs[1]
	content := merged["content"].(string)
	if merged["role"] != "user" || pyTruthy(merged["display_kind"]) || merged["_row_id"] != int64(12135) ||
		!strings.Contains(content, "the user's real prompt") || !strings.Contains(content, "[System:") {
		t.Fatalf("merged %v", merged)
	}
	if !seqContains(merged["_absorbed_row_ids"].([]any), int64(12134)) {
		t.Fatalf("marker id not retired %v", merged["_absorbed_row_ids"])
	}
	msgs = []map[string]any{
		{"role": "user", "_row_id": int64(12134), "content": "the user's real prompt"},
		{"role": "user", "display_kind": "model_switch", "_row_id": int64(12135), "content": marker},
	}
	if n := seqRepair(t, &msgs); n != 1 || len(msgs) != 1 {
		t.Fatalf("%d %v", n, msgs)
	}
	s := msgs[0]
	if s["role"] != "user" || s["_row_id"] != int64(12134) || pyTruthy(s["display_kind"]) ||
		s["content"] != "the user's real prompt\n\n"+marker || !seqContains(s["_absorbed_row_ids"].([]any), int64(12135)) {
		t.Fatalf("plain then marker %v", s)
	}
}

func TestRepairLeavesValidToolPairsAlone(t *testing.T) {
	cases := map[string][]map[string]any{
		"ongoing dialog": {seqUser("Q1"), seqAssistant("", seqCall("t1")), seqTool("t1", "out"), seqUser("Q2")},
		"codex call id": {seqUser("do it"), seqAssistant("", seqCall("fc_123", "call_id", "call_ABC")),
			seqTool("call_ABC", "result"), seqUser("next")},
		"only call id":     {seqUser("do it"), seqAssistant("", seqCall("", "call_id", "call_XYZ")), seqTool("call_XYZ", "result"), seqUser("next")},
		"response item id": {seqUser("do it"), seqAssistant("", seqCall("call_ABC", "call_id", "call_ABC", "response_item_id", "fc_123")), seqTool("fc_123", "result")},
		"parallel mixed variants": {seqUser("do both"),
			seqAssistant("", seqCall("call_1", "call_id", "call_1", "response_item_id", "fc_1"), seqCall("call_2", "call_id", "call_2", "response_item_id", "fc_2")),
			seqTool("call_1", "r1"), seqTool("fc_2", "r2")},
		"two codex calls mixed keys": {seqUser("task"), seqAssistant("", seqCall("fc_1", "call_id", "call_1"), seqCall("fc_2", "call_id", "call_2")),
			seqTool("call_1", "r1"), seqTool("fc_2", "r2")},
		"answered within following run": {seqUser("Q1"), seqAssistant("", seqCall("t1")), seqTool("t1", "out"), seqUser("Q2")},
	}
	for name, msgs := range cases {
		original := seqCopies(msgs)
		if n := seqRepair(t, &msgs); n != 0 || !reflect.DeepEqual(msgs, original) {
			t.Fatalf("%s: %d %v", name, n, msgs)
		}
	}
}

func TestRepairDropsStrayAndDuplicateToolResults(t *testing.T) {
	msgs := []map[string]any{seqUser("hi"), seqAssistant("hello"), seqTool("orphan", "stray"), seqUser("real")}
	if n := seqRepair(t, &msgs); n < 1 || slices.Contains(seqRoles(msgs), "tool") {
		t.Fatalf("%d %v", n, msgs)
	}
	msgs = []map[string]any{seqUser("task"), seqAssistant("", seqCall("fc_1", "call_id", "call_1")),
		seqTool("call_1", "first"), seqTool("fc_1", "duplicate")}
	if n := seqRepair(t, &msgs); n != 1 || !slices.Equal(seqToolContents(msgs), []string{"first"}) {
		t.Fatalf("%d %v", n, msgs)
	}
}

type sdkFunction struct{ Name, Arguments string }

type sdkToolCall struct {
	ID       string  `json:"id"`
	CallID   *string `json:"call_id"`
	Function sdkFunction
}

func TestRepairKeepsToolResultWhenToolCallsAreSDKObjects(t *testing.T) {
	msgs := []map[string]any{seqUser("read the file"),
		{"role": "assistant", "content": "", "tool_calls": []any{&sdkToolCall{ID: "call_1", Function: sdkFunction{"read_file", "{}"}}}},
		seqTool("call_1", "file contents")}
	seqRepair(t, &msgs)
	if !slices.Equal(seqToolContents(msgs), []string{"file contents"}) {
		t.Fatalf("legitimate tool result was dropped as a false orphan %v", msgs)
	}
}

func TestRepairAssistantMergeAPIContentSidecar(t *testing.T) {
	msgs := []map[string]any{seqUser("Q1"),
		{"role": "assistant", "content": "first reply", "api_content": "first reply (stale pre-merge bytes)"},
		seqAssistant("second reply")}
	if n := seqRepair(t, &msgs); n != 1 || len(msgs) != 2 || msgs[1]["api_content"] != nil || msgs[1]["content"] != "first reply\nsecond reply" {
		t.Fatalf("%d %v", n, msgs)
	}
	for _, later := range []any{nil, "", []any{map[string]any{"type": "text", "text": "img context"}}} {
		msgs = []map[string]any{seqUser("Q1"), {"role": "assistant", "content": "clean", "api_content": "wire bytes"}, seqAssistant(later)}
		seqRepair(t, &msgs)
		if len(msgs) != 2 || msgs[1]["content"] != "clean" || msgs[1]["api_content"] != "wire bytes" {
			t.Fatalf("later %v: %v", later, msgs)
		}
	}
}

func TestRepairDropsStaleEmptyToolCallsOnMergedAssistant(t *testing.T) {
	msgs := []map[string]any{seqUser("hi"), {"role": "assistant", "content": "first", "tool_calls": []any{}}, seqAssistant("second")}
	seqRepair(t, &msgs)
	var assistants []map[string]any
	for _, m := range msgs {
		if m["role"] == "assistant" {
			assistants = append(assistants, m)
		}
	}
	if len(assistants) != 1 || assistants[0]["tool_calls"] != nil || !strings.Contains(assistants[0]["content"].(string), "second") {
		t.Fatalf("%v", msgs)
	}
	if _, ok := assistants[0]["tool_calls"]; ok {
		t.Fatal("stale tool_calls key kept")
	}
}

func TestRepairPrunesDisplacedToolCalls(t *testing.T) {
	msgs := []map[string]any{seqUser("do it"), seqAssistant("", seqCall("call_A")), seqAssistant("second", seqCall("call_B")),
		seqTool("call_B", "out"), seqUser("meanwhile"), seqTool("call_A", "late result")}
	if n := seqRepair(t, &msgs); n < 1 {
		t.Fatal(n)
	}
	if !slices.Equal(seqRoles(msgs), []string{"user", "assistant", "tool", "user"}) {
		t.Fatalf("%v", msgs)
	}
	calls := msgs[1]["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "call_B" || msgs[1]["content"] != "second" || msgs[2]["tool_call_id"] != "call_B" {
		t.Fatalf("%v", msgs)
	}
	msgs = []map[string]any{seqUser("do it"), seqAssistant("", seqCall("call_A")), seqAssistant(""), seqUser("redirected"), seqTool("call_A", "late")}
	if n := seqRepair(t, &msgs); n < 2 || len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Fatalf("%d %v", n, msgs)
	}
	if c := msgs[0]["content"].(string); !strings.Contains(c, "do it") || !strings.Contains(c, "redirected") {
		t.Fatalf("%v", msgs)
	}
}

func TestRepairPopsPersistMarkerOnRewrite(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "first", DBPersistedMarker: true}, seqUser("second")}
	if n := seqRepair(t, &msgs); n != 1 || len(msgs) != 1 || msgs[0]["content"] != "first\n\nsecond" {
		t.Fatalf("%d %v", n, msgs)
	}
	if _, ok := msgs[0][DBPersistedMarker]; ok {
		t.Fatal("user merge kept the marker")
	}
	msgs = []map[string]any{seqUser("Q1"), {"role": "assistant", "content": "first reply", DBPersistedMarker: true}, seqAssistant("second reply")}
	if n := seqRepair(t, &msgs); n != 1 || msgs[1]["content"] != "first reply\nsecond reply" {
		t.Fatalf("%d %v", n, msgs)
	}
	if _, ok := msgs[1][DBPersistedMarker]; ok {
		t.Fatal("assistant merge kept the marker")
	}
	stamped := seqAssistant("calling tools", seqCall("t1"), seqCall("t2"))
	stamped[DBPersistedMarker] = true
	msgs = []map[string]any{seqUser("Q1"), stamped, seqTool("t1", "out1"), seqUser("next")}
	if n := seqRepair(t, &msgs); n < 1 {
		t.Fatal(n)
	}
	calls := stamped["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "t1" {
		t.Fatalf("%v", stamped)
	}
	if _, ok := stamped[DBPersistedMarker]; ok {
		t.Fatal("prune kept the marker")
	}
}

func TestRepairDecodesSentinelMultimodalAndSkipsTextMerge(t *testing.T) {
	parts := []any{
		map[string]any{"type": "text", "text": "look at this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", 4000)}},
	}
	msgs := []map[string]any{seqUser(encodeMessageContent(parts)), seqUser("any follow-up thoughts?")}
	seqRepair(t, &msgs)
	if len(msgs) != 2 || !pyValueEqual(msgs[0]["content"], parts) || msgs[1]["content"] != "any follow-up thoughts?" {
		t.Fatalf("%v", msgs)
	}
	corrupt := contentJSONPrefix + `[{"type": "text"} EXTRA garbage`
	msgs = []map[string]any{seqUser(corrupt), seqUser("still there?")}
	seqRepair(t, &msgs)
	if len(msgs) != 2 || msgs[0]["content"] != corrupt || msgs[1]["content"] != "still there?" {
		t.Fatalf("%v", msgs)
	}
}

func TestRepairRecordsDroppedToolRowOnSurvivor(t *testing.T) {
	unanswered := map[string]any{"role": "assistant", "content": "", "_row_id": int64(21), "tool_calls": []any{seqCall("unanswered")}}
	stray := map[string]any{"role": "tool", "tool_call_id": "orphan", "content": "out", "_row_id": int64(21)}
	for _, dropped := range []map[string]any{unanswered, stray} {
		for _, ahead := range []bool{false, true} {
			prompt := map[string]any{"role": "user", "content": "prompt", "_row_id": int64(20)}
			msgs := []map[string]any{prompt, copyMessage(dropped)}
			if ahead {
				msgs = []map[string]any{copyMessage(dropped), prompt}
			}
			if n := seqRepair(t, &msgs); n != 1 || len(msgs) != 1 || !reflect.DeepEqual(msgs[0]["_absorbed_row_ids"], []any{int64(21)}) {
				t.Fatalf("%v ahead %v: %d %v", dropped["role"], ahead, n, msgs)
			}
		}
	}
}

func TestRepairRaisesWhereHermesRaises(t *testing.T) {
	msgs := []map[string]any{seqUser("Q"), seqAssistant("", seqCall("t1")), {"role": "tool", "tool_call_id": 7, "content": "x"}}
	if _, err := RepairMessageSequence(&msgs); err == nil || !strings.Contains(err.Error(), "AttributeError") {
		t.Fatalf("non string tool_call_id %v", err)
	}
	msgs = []map[string]any{seqUser("Q"), {"role": "assistant", "content": "", "tool_calls": 5}}
	if _, err := RepairMessageSequence(&msgs); err == nil || !strings.Contains(err.Error(), "not iterable") {
		t.Fatalf("non iterable calls %v", err)
	}
	if n, err := RepairMessageSequence(&[]map[string]any{}); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

func TestMsgHasPayload(t *testing.T) {
	cases := []struct {
		msg  map[string]any
		want bool
	}{
		{map[string]any{"content": "  "}, false},
		{map[string]any{"content": "x"}, true},
		{map[string]any{"content": []any{map[string]any{"type": "text", "text": " "}}}, false},
		{map[string]any{"content": []any{map[string]any{"type": "image_url"}}}, true},
		{map[string]any{"content": 0}, true},
		{map[string]any{"content": "", "reasoning_content": " "}, false},
		{map[string]any{"content": "", "reasoning_content": "why"}, true},
		{map[string]any{"content": "", "codex_message_items": []any{1}}, true},
		{map[string]any{}, false},
	}
	for _, c := range cases {
		if got := MsgHasPayload(c.msg); got != c.want {
			t.Fatalf("%v: %v", c.msg, got)
		}
	}
}
