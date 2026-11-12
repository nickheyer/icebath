package tools

import "testing"

func TestResolveExecutionContextBypassesInsideGuard(t *testing.T) {
	_, ctx, _ := relayToolsTurn(t)
	runtime, session, parent, err := ResolveRelayExecutionContext(RelayManagedCallbackGuard(ctx), "session-1")
	if err != nil || runtime != nil || session != nil || parent != nil {
		t.Fatalf("inside the guard resolved %v %v %v %v", runtime, session, parent, err)
	}
	runtime, session, _, err = ResolveRelayExecutionContext(ctx, "session-1")
	if err != nil || runtime == nil || session == nil {
		t.Fatalf("outside the guard resolved %v %v %v", runtime, session, err)
	}
}
