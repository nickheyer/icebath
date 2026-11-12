package state

import (
	"context"
	"reflect"
	"testing"
)

func TestFlushedSurvivorPersistsTheAbsorbedUidsAndRestoreReturnsThem(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	db := usageDB(t)
	if err := db.CreateSession(NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	dangling := map[string]any{"role": "user", "content": "first, never answered"}
	prompt := map[string]any{"role": "user", "content": "second"}
	for _, m := range []map[string]any{dangling, prompt} {
		if _, err := db.AppendMessagesBatch(ctx, "s", []map[string]any{m}, AppendGuards{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := mergeConsecutiveUsers([]map[string]any{dangling, prompt}); err != nil {
		t.Fatal(err)
	}
	promptUID, _ := prompt[MessageUID].(string)
	if got := UIDList(dangling[AbsorbedMessageUIDs]); !reflect.DeepEqual(got, []string{promptUID}) {
		t.Fatalf("absorbed %v", dangling[AbsorbedMessageUIDs])
	}
	if _, err := db.AppendMessagesBatch(ctx, "s", []map[string]any{dangling}, AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := db.readAll(ctx, "SELECT content, message_uid, absorbed_message_uids FROM messages WHERE session_id = ? AND active = 1 ORDER BY id", "s")
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows %v err %v", rows, err)
	}
	c0, _ := rows[0].Get("content")
	c1, _ := rows[1].Get("content")
	u0, _ := rows[0].Get("message_uid")
	a0, _ := rows[0].Get("absorbed_message_uids")
	a1, _ := rows[1].Get("absorbed_message_uids")
	if c0 != "first, never answered\n\nsecond" || c1 != "second" || u0 != dangling[MessageUID] {
		t.Fatalf("stored %v %v %v", c0, c1, u0)
	}
	if a0 != `["`+promptUID+`"]` || a1 != nil {
		t.Fatalf("absorbed columns %v %v", a0, a1)
	}
	restored, err := db.GetMessagesAsConversation(ctx, "s", ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := UIDList(restored[0][AbsorbedMessageUIDs]); !reflect.DeepEqual(got, []string{promptUID}) {
		t.Fatalf("restored absorbed %v", restored[0][AbsorbedMessageUIDs])
	}
	if _, has := restored[1][AbsorbedMessageUIDs]; has {
		t.Fatalf("second row restored with absorbed uids %v", restored[1])
	}
}

func TestAFoldAfterASharedUidRowKeepsEachCallAlignedWithItsUid(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	shared, own := uidOf("1"), uidOf("2")
	first := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{uidCall("call_x"), uidCall("call_x")},
		ToolCallUIDs: map[string]any{"call_x": shared}}
	second := map[string]any{"role": "assistant", "content": "", "tool_calls": []any{uidCall("call_x")}, ToolCallUIDs: map[string]any{"call_x": own}}
	if _, _, err := mergeConsecutiveAssistants([]map[string]any{first, second}); err != nil {
		t.Fatal(err)
	}
	if got := plainDictTree(first[ToolCallUIDs]); !reflect.DeepEqual(got, map[string]any{"call_x": []any{shared, shared, own}}) {
		t.Fatalf("uids %v", got)
	}
	index := map[string]string{}
	IndexToolCallUIDs(index, first)
	if got, ok := ResolveToolCallUID(index, "call_x"); !ok || got != own {
		t.Fatalf("resolved %q %v", got, ok)
	}
}
