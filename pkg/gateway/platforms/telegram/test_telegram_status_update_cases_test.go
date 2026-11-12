package telegram

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// Loopback responder handing out the given sendMessage ids in order
func sequentialSendIDs(ids ...int) func(string, map[string]string) (int, string) {
	var mu sync.Mutex
	next := 0
	return func(method string, _ map[string]string) (int, string) {
		if method != "sendMessage" {
			return 0, ""
		}
		mu.Lock()
		defer mu.Unlock()
		id := ids[next]
		next++
		return 200, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d,"date":0,"chat":{"id":1,"type":"private"}}}`, id)
	}
}

func TestFirstCallSendsAndCachesMessageID(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	a.richSendDisabled.Store(true)
	fake.respond = sequentialSendIDs(100)
	result, err := a.SendOrUpdateStatus(context.Background(), "chat-1", "lifecycle", "starting", nil)
	if err != nil || !result.Success || result.MessageID != "100" {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(fake.callsOf("sendMessage")) != 1 || len(fake.callsOf("editMessageText")) != 0 {
		t.Fatal("first call must send, not edit")
	}
	if got, _ := a.statusMessageIDs.Get([2]string{"chat-1", "lifecycle"}); got != "100" {
		t.Fatalf("cached %q", got)
	}
}

func TestDistinctStatusKeysDoNotCollide(t *testing.T) {
	a, fake := newScriptedAdapter(t, nil)
	a.richSendDisabled.Store(true)
	fake.respond = sequentialSendIDs(100, 200)
	ctx := context.Background()
	if _, err := a.SendOrUpdateStatus(ctx, "chat-1", "lifecycle", "ctx pressure", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SendOrUpdateStatus(ctx, "chat-1", "model-switch", "switched to opus", nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.callsOf("sendMessage")) != 2 || len(fake.callsOf("editMessageText")) != 0 {
		t.Fatal("distinct keys must each send")
	}
	first, _ := a.statusMessageIDs.Get([2]string{"chat-1", "lifecycle"})
	second, _ := a.statusMessageIDs.Get([2]string{"chat-1", "model-switch"})
	if first != "100" || second != "200" {
		t.Fatalf("cached %q %q", first, second)
	}
}
