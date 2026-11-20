package slack

import (
	"context"
	"testing"
	"time"
)

// An Events API envelope whose inner type slack-go cannot type is still acked and reaches its Bolt listener
func TestSocketModeRoutesEnvelopesTheSDKCannotType(t *testing.T) {
	seen := make(chan map[string]any, 1)
	_, f := attachedSocketAdapter(t, func(a *SlackAdapter) {
		a.app.event("icebath_untyped_event", func(_ context.Context, event, _ map[string]any) { seen <- event })
	})
	conn := f.Conns()[0]
	id, err := conn.SendEvent("T_FAKE", map[string]any{"type": "icebath_untyped_event", "channel": "C1", "value": "v"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Ack(id, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-seen:
		if event["value"] != "v" || event["channel"] != "C1" {
			t.Fatalf("event = %v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never ran")
	}
}
