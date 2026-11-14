package state

import (
	"reflect"
	"strings"
	"testing"
)

const uidLen = 32

func uidOf(c string) string { return strings.Repeat(c, uidLen) }

func uidCall(id string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "t", "arguments": "{}"}}
}

func TestConsecutiveUserMergeKeepsTheFirstUIDAndRecordsTheAbsorbedOnes(t *testing.T) {
	a := map[string]any{"role": "user", "content": "first", "message_uid": uidOf("a")}
	b := map[string]any{"role": "user", "content": "second", "message_uid": uidOf("b")}
	c := map[string]any{"role": "user", "content": "third", "message_uid": uidOf("c")}
	merged, repairs, err := mergeConsecutiveUsers([]map[string]any{a, b, c})
	if err != nil || repairs != 2 || len(merged) != 1 || reflect.ValueOf(merged[0]).Pointer() != reflect.ValueOf(a).Pointer() {
		t.Fatalf("merged %v repairs %d err %v", merged, repairs, err)
	}
	if a["content"] != "first\n\nsecond\n\nthird" || a["message_uid"] != uidOf("a") {
		t.Fatalf("survivor %v", a)
	}
	if got := UIDList(a["_absorbed_message_uids"]); !reflect.DeepEqual(got, []string{uidOf("b"), uidOf("c")}) {
		t.Fatalf("absorbed %v", a["_absorbed_message_uids"])
	}
	if _, ok := a["_absorbed_row_ids"]; ok {
		t.Fatalf("row ids recorded %v", a)
	}
}

func TestTheLiveListWalkBindsAResultToTheNearestCallThatNamesIt(t *testing.T) {
	call := func(uid string) map[string]any {
		msg := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{uidCall("call_x")}}
		if uid != "" {
			msg["_tool_call_uids"] = map[string]any{"call_x": uid}
		}
		return msg
	}
	result := map[string]any{"role": "tool", "content": "r", "tool_call_id": "call_x"}
	if got, ok := ToolCallUIDFromHistory([]map[string]any{call(uidOf("1")), result, call(uidOf("2")), result}, 3, map[uintptr]*ToolCallOwner{}); !ok || got != uidOf("2") {
		t.Fatalf("nearest call %q %v", got, ok)
	}
	if got, ok := ToolCallUIDFromHistory([]map[string]any{call(uidOf("1")), result, call(""), result}, 3, map[uintptr]*ToolCallOwner{}); ok {
		t.Fatalf("legacy nearer call leaked %q", got)
	}
	if got, ok := ToolCallUIDFromHistory([]map[string]any{call(uidOf("1")), {"role": "user", "content": "q"}, result}, 2, map[uintptr]*ToolCallOwner{}); ok {
		t.Fatalf("crossed a user turn %q", got)
	}
}

func TestASupersededVerificationCandidateIsNotAWitnessConstituent(t *testing.T) {
	candidate := map[string]any{"role": "assistant", "content": "provisional", "finish_reason": "verification_required", "message_uid": uidOf("p"), "_row_id": 7}
	final := map[string]any{"role": "assistant", "content": "verified answer", "message_uid": uidOf("f")}
	merged, repairs, err := mergeConsecutiveAssistants([]map[string]any{candidate, final})
	if err != nil || repairs != 1 || len(merged) != 1 || reflect.ValueOf(merged[0]).Pointer() != reflect.ValueOf(final).Pointer() {
		t.Fatalf("merged %v repairs %d err %v", merged, repairs, err)
	}
	if final["message_uid"] != uidOf("f") {
		t.Fatalf("final %v", final)
	}
	if _, ok := final["_absorbed_message_uids"]; ok {
		t.Fatalf("candidate claimed as constituent %v", final)
	}
	if got := final["_absorbed_row_ids"]; !reflect.DeepEqual(rowIDs(got), []int64{7}) {
		t.Fatalf("absorbed rows %v", got)
	}
}

func rowIDs(v any) []int64 {
	list, _ := v.([]any)
	var out []int64
	for _, item := range list {
		if n, ok := plainRowInt(item); ok {
			out = append(out, n)
		}
	}
	return out
}

func TestAssistantMergeKeepsTheAbsorbedTurnsToolCallUIDs(t *testing.T) {
	first := map[string]any{"role": "assistant", "content": "a", "tool_calls": []any{uidCall("call_1")}, "_tool_call_uids": map[string]any{"call_1": uidOf("1")}}
	second := map[string]any{"role": "assistant", "content": "b", "tool_calls": []any{uidCall("call_2")}, "_tool_call_uids": map[string]any{"call_2": uidOf("2")}}
	merged, repairs, err := mergeConsecutiveAssistants([]map[string]any{first, second})
	if err != nil || repairs != 1 || len(merged) != 1 || reflect.ValueOf(merged[0]).Pointer() != reflect.ValueOf(first).Pointer() {
		t.Fatalf("merged %v repairs %d err %v", merged, repairs, err)
	}
	if got := plainDictTree(first["_tool_call_uids"]); !reflect.DeepEqual(got, map[string]any{"call_1": uidOf("1"), "call_2": uidOf("2")}) {
		t.Fatalf("uids %v", got)
	}
}

func reusedCall(content, occurrenceUID string) map[string]any {
	msg := map[string]any{"role": "assistant", "content": content, "tool_calls": []any{uidCall("call_x")}}
	if occurrenceUID != "" {
		msg["_tool_call_uids"] = map[string]any{"call_x": occurrenceUID}
	}
	return msg
}

func TestAssistantMergeKeepsEveryOccurrenceOfAReusedProviderID(t *testing.T) {
	first, second := reusedCall("", uidOf("1")), reusedCall("", uidOf("2"))
	merged, repairs, err := mergeConsecutiveAssistants([]map[string]any{first, second})
	if err != nil || repairs != 1 || len(merged) != 1 || reflect.ValueOf(merged[0]).Pointer() != reflect.ValueOf(first).Pointer() {
		t.Fatalf("merged %v repairs %d err %v", merged, repairs, err)
	}
	var ids []any
	for _, tc := range first["tool_calls"].([]any) {
		ids = append(ids, ToolCallField(tc, "id"))
	}
	if !reflect.DeepEqual(ids, []any{"call_x", "call_x"}) {
		t.Fatalf("ids %v", ids)
	}
	if got := plainDictTree(first["_tool_call_uids"]); !reflect.DeepEqual(got, map[string]any{"call_x": []any{uidOf("1"), uidOf("2")}}) {
		t.Fatalf("uids %v", got)
	}
	index := map[string]string{}
	IndexToolCallUIDs(index, first)
	if got, ok := ResolveToolCallUID(index, "call_x"); !ok || got != uidOf("2") {
		t.Fatalf("resolved %q %v", got, ok)
	}
}

func TestAnAssistantFoldThatDiscardsTheLaterTextIsNoWitness(t *testing.T) {
	text := func(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }
	for _, c := range []struct{ kept, dropped any }{
		{text("kept"), "UNIQUE-DROPPED-TEXT"},
		{"kept", text("UNIQUE-DROPPED-PART")},
		{text("kept"), text("UNIQUE-DROPPED-PART")},
	} {
		survivor := map[string]any{"role": "assistant", "content": c.kept, "message_uid": uidOf("a")}
		later := map[string]any{"role": "assistant", "content": c.dropped, "message_uid": uidOf("b"), "_row_id": 9}
		merged, repairs, err := mergeConsecutiveAssistants([]map[string]any{survivor, later})
		if err != nil || repairs != 1 || len(merged) != 1 || reflect.ValueOf(merged[0]).Pointer() != reflect.ValueOf(survivor).Pointer() || !reflect.DeepEqual(survivor["content"], c.kept) {
			t.Fatalf("merged %v repairs %d err %v", merged, repairs, err)
		}
		if _, ok := survivor["_absorbed_message_uids"]; ok {
			t.Fatalf("dropped text claimed %v", survivor)
		}
		if got := rowIDs(survivor["_absorbed_row_ids"]); !reflect.DeepEqual(got, []int64{9}) {
			t.Fatalf("absorbed rows %v", survivor["_absorbed_row_ids"])
		}
	}
}

func TestAFoldIgnoresANonDictUIDMapOnEitherTurn(t *testing.T) {
	turn := func(callID string, uids any) map[string]any {
		return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{uidCall(callID)}, "_tool_call_uids": uids}
	}
	prev, next := turn("X", "junk"), turn("Y", map[string]any{"Y": uidOf("b")})
	if _, err := mergeAssistantInto(prev, next); err != nil {
		t.Fatal(err)
	}
	if got := plainDictTree(prev["_tool_call_uids"]); !reflect.DeepEqual(got, map[string]any{"Y": uidOf("b")}) {
		t.Fatalf("uids %v", got)
	}
	prev, next = turn("X", map[string]any{"X": uidOf("a")}), turn("Y", []any{"junk"})
	if _, err := mergeAssistantInto(prev, next); err != nil {
		t.Fatal(err)
	}
	if got := plainDictTree(prev["_tool_call_uids"]); !reflect.DeepEqual(got, map[string]any{"X": uidOf("a")}) {
		t.Fatalf("uids %v", got)
	}
}
