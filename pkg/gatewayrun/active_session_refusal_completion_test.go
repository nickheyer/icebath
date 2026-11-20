package gatewayrun

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestRefusedTurnPublishesCompletionWithItsReason(t *testing.T) {
	live := gateway.NewLiveSession(nil, "route")
	var received []*pyjson.Object
	live.EventStream.Observers[&gateway.SessionEventObserver{Observe: func(frame *pyjson.Object) { received = append(received, frame) }}] = true
	refusal := &activesessions.Refusal{Message: "Too many active sessions.", Reason: activesessions.MaxConcurrentSessions}
	live.EventStream.Lock.Lock()
	err := gateway.PublishCompletion(context.Background(), &gateway.SessionAuthority{}, live, gateway.SessionRef{SessionID: "s"},
		pyjson.ObjectOf("admission_id", "a1", "request_id", "r1"), pyjson.ObjectOf("outcome", "failed"), refusal, nil)
	live.EventStream.Lock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(received) != 1 {
		t.Fatalf("frames %d", len(received))
	}
	params := received[0].GetOr("params", nil).(*pyjson.Object)
	if params.GetOr("type", nil) != "message.complete" {
		t.Fatalf("type %v", params.GetOr("type", nil))
	}
	text, ok := params.GetOr("payload", nil).(*pyjson.Object).GetOr("text", nil).(*activesessions.Refusal)
	if !ok || text.Message != "Too many active sessions." || text.Reason != activesessions.MaxConcurrentSessions {
		t.Fatalf("text %#v", text)
	}
}
