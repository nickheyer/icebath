package state

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestUIDCodecs(t *testing.T) {
	if got := uidList(`["a", "", "b", "a", 3]`); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("uid list %v", got)
	}
	if got := uidList("not json"); len(got) != 0 {
		t.Fatalf("bad text %v", got)
	}
	if got := uidList([]byte(`["a"]`)); len(got) != 0 {
		t.Fatalf("bytes are not a list %v", got)
	}
	got := uidMap(`{"c1": "u1", "": "x", "c2": "", "c3": ["u3", "u4"], "c4": [], "c5": ["u", ""], "c6": 5}`)
	want := pyObject{{"c1", "u1"}, {"c3", []any{"u3", "u4"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("uid map %v", got)
	}
	if got := uidMap(`[1]`); len(got) != 0 {
		t.Fatalf("non map %v", got)
	}
	if got := uidMap(map[string]any{"b": "2", "a": "1"}); !reflect.DeepEqual(got, pyObject{{"a", "1"}, {"b", "2"}}) {
		t.Fatalf("live map %v", got)
	}
	for tc, want := range map[string]string{
		`{"call_id": " c1|item ", "id": "x"}`: "c1", `{"id": "plain"}`: "plain", `{"id": "|tail"}`: "|tail", `{"call_id": "  ", "id": "fallback"}`: "fallback", `{"id": 7}`: "", `"str"`: "",
	} {
		v, _ := pyLoads(tc)
		if got := CoalesceToolCallID(v); got != want {
			t.Fatalf("coalesce %s -> %q", tc, got)
		}
	}
}

func TestIdentityColumnSerialization(t *testing.T) {
	msg := map[string]any{AbsorbedMessageUIDs: []any{"u2", "u1", "u2"}, "absorbed_message_uids": `["ignored"]`,
		"tool_call_uids": `{"b": "2", "a": "1"}`, ToolCallUID: "", "tool_call_uid": "t1"}
	if got, ok, err := absorbedUIDsJSON(msg); err != nil || !ok || got != `["u2", "u1"]` {
		t.Fatalf("absorbed %q %v %v", got, ok, err)
	}
	if got, ok, err := toolCallUIDsJSON(msg); err != nil || !ok || got != `{"a": "1", "b": "2"}` {
		t.Fatalf("tool call uids %q %v %v", got, ok, err)
	}
	if _, ok := toolCallUIDOrNone(msg); ok {
		t.Fatal("live empty uid wins over the column and reads as None")
	}
	if _, ok, _ := absorbedUIDsJSON(map[string]any{}); ok {
		t.Fatal("no absorbed uids is NULL")
	}
	if uid, ok := toolCallUIDOrNone(map[string]any{"tool_call_uid": "t9"}); !ok || uid != "t9" {
		t.Fatal("import payload column")
	}
}

func TestRestoreAndFillIdentity(t *testing.T) {
	row := map[string]any{MessageUID: "m1", "tool_call_uids": `{"c1": "s1", "c2": "s2", "c3": "s3"}`, "tool_call_uid": "tu", "absorbed_message_uids": `["a1", "a1", "a2"]`}
	msg := map[string]any{ToolCallUIDs: pyObject{{"c2", []any{"s2", "later"}}, {"c9", "live"}}}
	restoreIdentityColumns(row, msg)
	want := pyObject{{"c2", []any{"s2", "later"}}, {"c9", "live"}, {"c1", "s1"}, {"c3", "s3"}}
	if !reflect.DeepEqual(msg[ToolCallUIDs], want) || msg[MessageUID] != "m1" || msg[ToolCallUID] != "tu" || !reflect.DeepEqual(msg[AbsorbedMessageUIDs], []string{"a1", "a2"}) {
		t.Fatalf("restored %v", msg)
	}
	empty := map[string]any{MessageUID: "", "tool_call_uids": nil, "tool_call_uid": nil, "absorbed_message_uids": "[]"}
	untouched := map[string]any{"role": "user"}
	restoreIdentityColumns(empty, untouched)
	if len(untouched) != 1 {
		t.Fatalf("empty columns add nothing %v", untouched)
	}
	calls, _ := pyLoads(`[{"id": "c1"}, {"call_id": "c3|x"}]`)
	clone := map[string]any{"tool_calls": calls, ToolCallUIDs: pyObject{{"c1", "live1"}}}
	fillMissingToolCallUIDs(row, clone)
	if !reflect.DeepEqual(clone[ToolCallUIDs], pyObject{{"c1", "live1"}, {"c3", "s3"}}) {
		t.Fatalf("filled %v", clone[ToolCallUIDs])
	}
	noCalls := map[string]any{"tool_calls": "x"}
	fillMissingToolCallUIDs(row, noCalls)
	if _, ok := noCalls[ToolCallUIDs]; ok {
		t.Fatal("non list tool_calls is left alone")
	}
	if !liveExtends([]any{"a", "b"}, "a") || !liveExtends([]any{"a", "b"}, []any{"a", "b"}) || liveExtends([]any{"a"}, []any{"a", "b"}) || liveExtends("a", "a") {
		t.Fatal("live extends")
	}
}

func TestStableToolKey(t *testing.T) {
	row := map[string]any{"role": "assistant", "tool_calls": `[{"id": "c1"}, {"call_id": "c2|r"}]`, "timestamp": 12.5, "tool_call_id": nil, "tool_name": nil}
	if got, ok, err := stableToolKey(row); err != nil || !ok || got != "('assistant', None, 12.5, None, ('c1', 'c2'), None)" {
		t.Fatalf("key %q %v %v", got, ok, err)
	}
	row["tool_calls"] = `[{"id": "only"}]`
	if got, _, _ := stableToolKey(row); got != "('assistant', None, 12.5, None, ('only',), None)" {
		t.Fatalf("single call tuple %q", got)
	}
	for _, calls := range []any{`[{"id": "c1"}, {"function": {}}]`, nil, "", `{"c1": 1}`, `"abc"`, `[]`, `0`, `not json`} {
		row["tool_calls"] = calls
		if _, ok, err := stableToolKey(row); ok || err != nil {
			t.Fatalf("%v must keep the content key, err %v", calls, err)
		}
	}
	row["tool_calls"] = `5`
	if _, _, err := stableToolKey(row); err == nil {
		t.Fatal("iterating a number raises TypeError")
	}
	if _, ok, _ := stableToolKey(map[string]any{"role": "tool", "tool_calls": `[{"id": "c1"}]`}); ok {
		t.Fatal("only assistant rows")
	}
	if !pyValueEqual(json.Number("1"), 1.0) || pyValueEqual("1", json.Number("1")) || !pyValueEqual(pyObject{{"a", []any{true}}}, map[string]any{"a": []any{json.Number("1")}}) {
		t.Fatal("python equality")
	}
}
