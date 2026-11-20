package state

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Durable wedge of an assistant row followed by two user rows with no reply between
func seedWedgedSession(t *testing.T, sessionID, source string) *SessionDB {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db, err := Open(filepath.Join(t.TempDir(), "test_state.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(NewSession{ID: sessionID, Source: source}); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][2]string{{"user", "first ask"}, {"assistant", "first reply"}, {"user", "unanswered turn"}, {"user", "next turn"}, {"assistant", "next reply"}} {
		if _, err := db.AppendMessage(context.Background(), sessionID, map[string]any{"role": row[0], "content": row[1]}, AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestRepairAlternationMergesUserPair(t *testing.T) {
	db := seedWedgedSession(t, "s1", "system prompt")
	messages, err := db.GetMessagesAsConversation(context.Background(), "s1", ConversationOptions{RepairAlternation: true})
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range messages {
		roles = append(roles, m["role"].(string))
	}
	if !reflect.DeepEqual(roles, []string{"user", "assistant", "user", "assistant"}) {
		t.Fatalf("roles %v", roles)
	}
	merged, _ := messages[2]["content"].(string)
	if !strings.Contains(merged, "unanswered turn") || !strings.Contains(merged, "next turn") || strings.Index(merged, "unanswered turn") > strings.Index(merged, "next turn") {
		t.Fatalf("merged %q", merged)
	}
}

func TestRepairedLoadIsStableUnderPrerequestRepair(t *testing.T) {
	db := seedWedgedSession(t, "s1", "system prompt")
	messages, err := db.GetMessagesAsConversation(context.Background(), "s1", ConversationOptions{RepairAlternation: true})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := RepairMessageSequence(&messages); err != nil || n != 0 {
		t.Fatalf("repairs %d err %v", n, err)
	}
}
