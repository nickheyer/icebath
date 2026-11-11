package agent

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestWorkerPersistenceExplicitlyBypassesProxy(t *testing.T) {
	home := t.TempDir()
	owner := startLoopbackOwner(t, home, "reply-close", 2)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	rpc := NewWorkerRPC(home)
	defer rpc.Close()
	ctx := context.Background()
	for i := range 2 {
		result, err := rpc.Call(ctx, "worker.persist", nil)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !pyjson.Equal(result, pyjson.ObjectOf("ok", true)) {
			t.Fatalf("call %d result %v", i, result)
		}
		if i == 0 {
			select {
			case <-rpc.link.done:
			case <-time.After(5 * time.Second):
				t.Fatal("the link reader never saw the owner drop the connection")
			}
		}
	}
	if connects := owner.eventsOf(t, "connect"); len(connects) != 2 {
		t.Fatalf("owner saw %d connections, want 2", len(connects))
	}
}
