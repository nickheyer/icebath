package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Reader with frame hooks logs a malformed frame and keeps answering, the ui-tui link behaviour
func TestFrameHooksReadPastMalformedFrames(t *testing.T) {
	owner := newScriptedOwner(func(request map[string]any) any {
		return map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{"ok": true}}
	})
	var mu sync.Mutex
	var malformed, inbound []string
	client := NewGatewayClient(owner)
	client.SetFrameHooks(FrameHooks{
		Inbound:   func(raw string, _ *pyjson.Object) { mu.Lock(); inbound = append(inbound, raw); mu.Unlock() },
		Malformed: func(raw string) { mu.Lock(); malformed = append(malformed, raw); mu.Unlock() },
	})
	client.Start()
	defer client.Close()
	owner.inbox <- "{not json"
	owner.inbox <- "[1, 2]"
	result, err := client.RPC(context.Background(), "session.info", nil)
	if err != nil || result["ok"] != true {
		t.Fatalf("rpc after malformed frames %v %v", result, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(malformed) != 2 || malformed[0] != "{not json" || malformed[1] != "[1, 2]" {
		t.Fatalf("malformed %q", malformed)
	}
	if len(inbound) != 1 {
		t.Fatalf("inbound %q", inbound)
	}
	if client.ReaderDone() {
		t.Fatal("reader ended on a malformed frame")
	}
}

// Reader without hooks ends on a malformed frame as the Python thin client does, recording no read error
func TestFrameHooksAbsentEndOnMalformedFrame(t *testing.T) {
	owner := newScriptedOwner(func(map[string]any) any { return nil })
	client := NewGatewayClient(owner).Start()
	defer client.Close()
	owner.inbox <- "{not json"
	deadline := time.Now().Add(5 * time.Second)
	for !client.ReaderDone() {
		if time.Now().After(deadline) {
			t.Fatal("reader kept reading")
		}
		time.Sleep(time.Millisecond)
	}
	if client.ReadError() != nil {
		t.Fatalf("read error %v", client.ReadError())
	}
}

// Read error keeps the error the socket failed with
func TestFrameHooksReadErrorKeepsSocketFailure(t *testing.T) {
	owner := newScriptedOwner(func(map[string]any) any { return nil })
	owner.recv = func() (string, error) { return "", errors.New("close 1011") }
	client := NewGatewayClient(owner).Start()
	defer client.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !client.ReaderDone() {
		if time.Now().After(deadline) {
			t.Fatal("reader kept reading")
		}
		time.Sleep(time.Millisecond)
	}
	if err := client.ReadError(); err == nil || err.Error() != "close 1011" {
		t.Fatalf("read error %v", err)
	}
}
