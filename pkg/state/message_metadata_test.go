package state

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type contentObjectPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func TestFlattenMessageTextAcceptsChatAndResponsesTextParts(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "chat text"},
		map[string]any{"type": "input_text", "text": "user text"},
		map[string]any{"type": "output_text", "text": "assistant text"},
		map[string]any{"type": "summary_text", "text": "summary text"},
	}
	if got := FlattenMessageText(content, "\n"); got != "chat text\nuser text\nassistant text\nsummary text" {
		t.Fatalf("got %q", got)
	}
}

func TestFlattenMessageTextAcceptsObjectParts(t *testing.T) {
	content := []any{
		contentObjectPart{Type: "output_text", Text: "object text"},
		map[string]any{"content": "legacy content"},
	}
	if got := FlattenMessageText(content, "\n"); got != "object text\nlegacy content" {
		t.Fatalf("got %q", got)
	}
}

func TestFlattenMessageTextSkipsMediaAndStringifiesScalars(t *testing.T) {
	content := []any{map[string]any{"type": " IMAGE_URL ", "text": "hidden"}, "plain", nil}
	if got := FlattenMessageText(content, " "); got != "plain" {
		t.Fatalf("got %q", got)
	}
	if got := FlattenMessageText(42, "\n"); got != "42" {
		t.Fatalf("got %q", got)
	}
	if got := FlattenMessageText(map[string]any{"output_text": "x"}, "\n"); got != "x" {
		t.Fatalf("got %q", got)
	}
}

func TestFlattenMessageTextReadsEveryMappingKind(t *testing.T) {
	ordered := pyjson.NewObject()
	ordered.Set("type", "text")
	ordered.Set("text", "ordered text")
	content := []any{
		ordered,
		map[string]string{"type": "text", "text": "string map"},
		map[any]any{"content": "any map", 3: "skipped"},
		map[string]string{"type": "image", "text": "hidden"},
		map[int]string{1: "no string keys"},
	}
	if got := FlattenMessageText(content, "|"); got != "ordered text|string map|any map" {
		t.Fatalf("got %q", got)
	}
}

func TestStampPreservesSourceTimestamp(t *testing.T) {
	prev := metadataWallTime
	metadataWallTime = func() float64 { return 999.0 }
	defer func() { metadataWallTime = prev }()
	message := map[string]any{"role": "user", "content": "hello", "timestamp": 123.0}
	result := StampMessageTimestamp(message, nil)
	if reflect.ValueOf(result).Pointer() != reflect.ValueOf(message).Pointer() {
		t.Fatal("stamp returned a different mapping")
	}
	if message["timestamp"] != 123.0 {
		t.Fatalf("timestamp %v", message["timestamp"])
	}
}

func TestAppendStampsSameMappingAtAppendTime(t *testing.T) {
	prev := metadataWallTime
	metadataWallTime = func() float64 { return 456.0 }
	defer func() { metadataWallTime = prev }()
	var messages []map[string]any
	message := map[string]any{"role": "tool", "content": "ok"}
	result := AppendMessage(&messages, message, nil)
	if reflect.ValueOf(result).Pointer() != reflect.ValueOf(message).Pointer() {
		t.Fatal("append returned a different mapping")
	}
	if len(messages) != 1 || reflect.ValueOf(messages[0]).Pointer() != reflect.ValueOf(message).Pointer() {
		t.Fatalf("messages %v", messages)
	}
	if message["timestamp"] != 456.0 {
		t.Fatalf("timestamp %v", message["timestamp"])
	}
}

func TestRecordAbsorbedMessageOrdersWitnesses(t *testing.T) {
	survivor := map[string]any{MessageUID: "a", AbsorbedMessageUIDs: []any{"b"}}
	dropped := map[string]any{MessageUID: "c", AbsorbedMessageUIDs: []any{"d", "a"}}
	RecordAbsorbedMessage(survivor, dropped, false)
	if !reflect.DeepEqual(survivor[AbsorbedMessageUIDs], []any{"b", "c", "d"}) {
		t.Fatalf("absorbed %v", survivor[AbsorbedMessageUIDs])
	}
	survivor = map[string]any{MessageUID: "a"}
	RecordAbsorbedMessage(survivor, map[string]any{MessageUID: "c"}, true)
	if survivor[MessageUID] != "c" || !reflect.DeepEqual(survivor[AbsorbedMessageUIDs], []any{"a"}) {
		t.Fatalf("survivor %v", survivor)
	}
}

func TestToolCallUIDFromHistoryPairsNearestAssistantOccurrence(t *testing.T) {
	assistant := map[string]any{
		"role":       "assistant",
		"tool_calls": []any{map[string]any{"id": "call_1|item_9"}, map[string]any{"id": "call_1"}},
		ToolCallUIDs: map[string]any{"call_1": []any{"u1", "u2"}},
	}
	index := map[string]string{"call_1": "stale"}
	named := IndexToolCallUIDs(index, assistant)
	if !named["call_1"] || !named["item_9"] || !named["call_1|item_9"] {
		t.Fatalf("named %v", named)
	}
	if index["item_9"] != "u1" || index["call_1"] != "u2" {
		t.Fatalf("index %v", index)
	}
	messages := []map[string]any{
		assistant,
		{"role": "tool", "tool_call_id": "item_9"},
	}
	owners := map[uintptr]*ToolCallOwner{}
	if uid, ok := ToolCallUIDFromHistory(messages, 1, owners); !ok || uid != "u1" {
		t.Fatalf("uid %q %v", uid, ok)
	}
	if len(owners) != 1 {
		t.Fatalf("owners %d", len(owners))
	}
	messages = append([]map[string]any{assistant, {"role": "user"}}, map[string]any{"role": "tool", "tool_call_id": "call_1"})
	if _, ok := ToolCallUIDFromHistory(messages, 2, map[uintptr]*ToolCallOwner{}); ok {
		t.Fatal("a tool result paired across a user turn")
	}
}

func TestPerOccurrenceAndMergeToolCallUIDs(t *testing.T) {
	calls := []any{map[string]any{"id": "x"}, map[string]any{"id": "x"}, map[string]any{"call_id": "y"}}
	got := PerOccurrenceToolCallUIDs(map[string]any{"x": "u", "y": "v"}, calls)
	want := PyObject{{Key: "x", Value: []any{"u", "u"}}, {Key: "y", Value: "v"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded %v", got)
	}
	merged := MergeToolCallUIDs(PyObject{{Key: "x", Value: "u"}}, PyObject{{Key: "x", Value: []any{"w"}}, {Key: "z", Value: "q"}})
	want = PyObject{{Key: "x", Value: []any{"u", "w"}}, {Key: "z", Value: "q"}}
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged %v", merged)
	}
}

func TestWithheldPromptRoundTrip(t *testing.T) {
	row := map[string]any{"role": "user", "content": PreLLMBlockWithheldText, "display_metadata": map[string]any{WithheldPrompt: "real ask", "k": 1}}
	if got := TypedPromptContent(row["content"], row); got != "real ask" {
		t.Fatalf("typed %v", got)
	}
	out := WithoutWithheldPrompts([]map[string]any{row}).([]map[string]any)
	if _, has := out[0]["display_metadata"].(map[string]any)[WithheldPrompt]; has {
		t.Fatal("withheld prompt survived")
	}
	if _, has := row["display_metadata"].(map[string]any)[WithheldPrompt]; !has {
		t.Fatal("source row was mutated")
	}
	plain := []map[string]any{{"role": "user"}}
	if reflect.ValueOf(WithoutWithheldPrompts(plain)).Pointer() != reflect.ValueOf(plain).Pointer() {
		t.Fatal("rows without a withheld prompt were copied")
	}
}

func TestDataUseStampAndLift(t *testing.T) {
	msg := map[string]any{"display_metadata": map[string]any{"a": 1}}
	StampDataUse(msg, []string{"zdr"})
	if !reflect.DeepEqual(msg[DataUse], []any{"zdr"}) || !reflect.DeepEqual(msg["display_metadata"], map[string]any{"a": 1, DataUse: []any{"zdr"}}) {
		t.Fatalf("stamped %v", msg)
	}
	req := DataUseRequirement([]any{msg, "skip", map[string]any{DataUse: []any{"eu"}}})
	if !req["zdr"] || !req["eu"] || len(req) != 2 {
		t.Fatalf("requirement %v", req)
	}
	replayed := map[string]any{"display_metadata": map[string]any{DataUse: []any{"zdr"}}}
	LiftDataUse(replayed)
	if !reflect.DeepEqual(replayed[DataUse], []any{"zdr"}) {
		t.Fatalf("lifted %v", replayed)
	}
	if got := WithoutPersistenceFields(map[string]any{"role": "user", "_row_id": 3, DBRowSnapshot: "x"}); !reflect.DeepEqual(got, map[string]any{"role": "user"}) {
		t.Fatalf("stripped %v", got)
	}
}

type fakeRoleReader map[int64]string

func (f fakeRoleReader) GetMessageRole(ctx context.Context, sessionID string, messageID int64) (string, bool, error) {
	role, ok := f[messageID]
	return role, ok, nil
}

func TestCoverageForCommit(t *testing.T) {
	held := []map[string]any{
		{"_row_id": int64(4), DBPersistedMarker: true, AbsorbedRowIDs: []any{int64(3), true, -1}},
		{"role": "user", "content": "unpersisted"},
	}
	covered, unresolved := HeldArchiveCoverage(held, nil)
	if !reflect.DeepEqual(covered, []int64{4, 3}) || len(unresolved) != 1 {
		t.Fatalf("coverage %v %v", covered, unresolved)
	}
	if _, _, ok, err := CoverageForCommit(fakeRoleReader{}, "sid", held, nil); ok || err != nil {
		t.Fatal("an inactive newest row kept coverage")
	}
	covered, unresolved, ok, err := CoverageForCommit(fakeRoleReader{4: "user"}, "sid", held, nil)
	if !ok || err != nil || !reflect.DeepEqual(covered, []int64{4, 3}) || len(unresolved) != 1 {
		t.Fatalf("coverage %v %v %v %v", covered, unresolved, ok, err)
	}
	retiring := []map[string]any{{"role": "assistant", UnnamedDurableRows: 1, RetiredDurableRows: []any{map[string]any{"role": "assistant", "content": "x"}}}}
	_, unresolved, ok, _ = CoverageForCommit(nil, "sid", retiring, nil)
	if !ok || len(unresolved) != 2 || unresolved[1][RetiredRow] != true || unresolved[1][DBPersistedMarker] != true {
		t.Fatalf("retired %v %v", unresolved, ok)
	}
	retiring[0][UnnamedDurableRows] = 2
	if _, _, ok, _ := CoverageForCommit(nil, "sid", retiring, nil); ok {
		t.Fatal("more unnamed rows than named kept coverage")
	}
	payload := RetiredRowPayload(map[string]any{"role": "assistant", "content": "c", "tool_calls": []any{map[string]any{"id": "a"}, "junk"}})
	if !reflect.DeepEqual(payload, map[string]any{"role": "assistant", "content": "c", "tool_call_ids": []any{"a"}}) {
		t.Fatalf("payload %v", payload)
	}
	if id, ok := NewestExactHeldID(nil, []map[string]any{{"_row_id": 9}}); !ok || id != 9 {
		t.Fatalf("newest %v %v", id, ok)
	}
}
