package telegram

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestEditOverflowSplitReportsLaterPartialFailureAfterSomeContinuationsLand(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	var mu sync.Mutex
	sends := 0
	fake.respond = func(method string, _ map[string]string) (int, string) {
		if method != "sendMessage" {
			return 0, ""
		}
		mu.Lock()
		defer mu.Unlock()
		sends++
		if sends == 1 {
			return 200, `{"ok":true,"result":{"message_id":202,"date":0,"chat":{"id":12345,"type":"private"}}}`
		}
		return botError(500, "Internal Server Error: telegram send failed")
	}
	content := strings.Repeat("word ", 2200)
	result, err := a.editOverflowSplit(context.Background(), "12345", "201", content, false, map[string]any{"thread_id": "77"})
	if err != nil || result.Success || result.MessageID != "202" {
		t.Fatalf("result %+v err %v", result, err)
	}
	raw, _ := result.RawResponse.(map[string]any)
	if raw["partial_overflow"] != true || raw["delivered_chunks"] != 2 || raw["last_message_id"] != "202" {
		t.Fatalf("raw %+v", raw)
	}
	if len(result.ContinuationMessageIDs) != 1 || result.ContinuationMessageIDs[0] != "202" {
		t.Fatalf("continuations %v", result.ContinuationMessageIDs)
	}
	if n := len(fake.callsOf("editMessageText")); n != 1 {
		t.Fatalf("edits %d", n)
	}
}
