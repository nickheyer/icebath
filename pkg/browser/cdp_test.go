package browser

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// Minimal CDP server: answers Browser.getVersion, errors on Unknown.method, emits an event before a reply
func fakeCDPServer(t *testing.T, calls *[]string) (string, func()) {
	var mu sync.Mutex
	srv, wsURL := newTestWSServer(func(s *testWSServer) {
		for {
			op, payload, err := s.readMessage()
			if err != nil || op == opClose {
				return
			}
			var req struct {
				ID        int64           `json:"id"`
				Method    string          `json:"method"`
				Params    json.RawMessage `json:"params"`
				SessionID string          `json:"sessionId"`
			}
			if json.Unmarshal(payload, &req) != nil {
				continue
			}
			mu.Lock()
			*calls = append(*calls, req.Method)
			mu.Unlock()
			switch req.Method {
			case "Browser.getVersion":
				_ = s.writeText(`{"method":"Target.targetCreated","params":{"targetInfo":{"targetId":"T1","type":"page"}}}`)
				reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"product": "Chrome/1", "session": req.SessionID}})
				_ = s.writeText(string(reply))
			case "Slow.method":
				time.Sleep(300 * time.Millisecond)
				reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{}})
				_ = s.writeText(string(reply))
			case "Target.attachToTarget":
				reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"sessionId": "S9"}})
				_ = s.writeText(string(reply))
			default:
				reply, _ := json.Marshal(map[string]any{"id": req.ID, "error": map[string]any{"code": -32601, "message": "'" + req.Method + "' wasn't found"}})
				_ = s.writeText(string(reply))
			}
		}
	})
	return wsURL, srv.Close
}

func TestCDPCorrelatesRepliesAndEvents(t *testing.T) {
	var calls []string
	wsURL, stop := fakeCDPServer(t, &calls)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := DialCDP(ctx, wsURL, 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	events := make(chan CDPEvent, 4)
	off := conn.OnEvent(func(ev CDPEvent) { events <- ev })
	defer off()
	var out struct {
		Product string `json:"product"`
		Session string `json:"session"`
	}
	if err := conn.CallInto(ctx, "Browser.getVersion", nil, "S1", &out); err != nil {
		t.Fatalf("call: %v", err)
	}
	if out.Product != "Chrome/1" || out.Session != "S1" {
		t.Fatalf("unexpected result %+v", out)
	}
	select {
	case ev := <-events:
		if ev.Method != "Target.targetCreated" {
			t.Fatalf("unexpected event %s", ev.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not delivered")
	}
	_, err = conn.Call(ctx, "Unknown.method", map[string]any{}, "")
	var cdpErr *CDPError
	if !errors.As(err, &cdpErr) || cdpErr.Code != -32601 {
		t.Fatalf("expected protocol error, got %v", err)
	}
	sid, err := conn.Attach(ctx, "T1")
	if err != nil || sid != "S9" {
		t.Fatalf("attach: %s %v", sid, err)
	}
}

func TestCDPCallTimeoutAndClose(t *testing.T) {
	var calls []string
	wsURL, stop := fakeCDPServer(t, &calls)
	defer stop()
	conn, err := DialCDP(context.Background(), wsURL, 0)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := conn.Call(ctx, "Slow.method", nil, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if conn.Alive() {
		t.Fatal("connection still alive after close")
	}
	if _, err := conn.Call(context.Background(), "Browser.getVersion", nil, ""); !errors.Is(err, ErrCDPClosed) {
		t.Fatalf("expected closed error, got %v", err)
	}
}
