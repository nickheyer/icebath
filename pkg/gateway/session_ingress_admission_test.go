package gateway

import (
	"context"
	"testing"
)

func TestPreAdmissionRefusalReachesReceiveLoop(t *testing.T) {
	adapter, _ := newSessionAdapter(t)
	adapter.SetMessageHandler(func(context.Context, *MessageEvent) (any, error) {
		return nil, runtimeRefusal("permission_denied")
	})
	event := NewMessageEvent("refused")
	event.Source = &SessionSource{Platform: PlatformTelegram, ChatID: "chat"}
	if err := dispatchSharedBusy(context.Background(), adapter, event, "route", false); refusalReason(err) != "permission_denied" {
		t.Fatalf("refusal %v", err)
	}
	adapter.Base().mu.Lock()
	tracked := len(adapter.Base().backgroundTasks)
	adapter.Base().mu.Unlock()
	if tracked != 0 {
		t.Fatalf("background tasks %d", tracked)
	}
}
