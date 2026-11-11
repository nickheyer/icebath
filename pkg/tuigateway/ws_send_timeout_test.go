package tuigateway

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Socket whose send never completes until it is closed, recording each close code
type stalledWS struct {
	mu         sync.Mutex
	closedWith []int
	entered    chan struct{}
	release    chan struct{}
	enterOnce  sync.Once
	once       sync.Once
}

func newStalledWS() *stalledWS {
	return &stalledWS{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *stalledWS) SendText(ctx context.Context, line string) error {
	w.enterOnce.Do(func() { close(w.entered) })
	<-w.release
	return nil
}

func (w *stalledWS) Close(code int) error {
	w.mu.Lock()
	w.closedWith = append(w.closedWith, code)
	w.mu.Unlock()
	w.once.Do(func() { close(w.release) })
	return nil
}

func (w *stalledWS) codes() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.closedWith)
}

func TestStalledSendClosesSocketAndReleasesQueuedReply(t *testing.T) {
	setDuration(t, &wsSendDeadline, 50*time.Millisecond)
	ws := newStalledWS()
	t.Cleanup(func() { ws.once.Do(func() { close(ws.release) }) })
	transport := NewWSTransport(ws, "127.0.0.1:1", nil)
	progress := make(chan bool, 1)
	go func() { progress <- transport.WriteAsync(wsEvent("tool.progress")) }()
	select {
	case <-ws.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the progress frame never reached the socket")
	}
	reply := make(chan bool, 1)
	go func() {
		reply <- transport.WriteAsync(pyjson.ObjectOf("id", "submit", "result", pyjson.ObjectOf("status", "streaming")))
	}()
	results := []bool{}
	for _, pending := range []chan bool{progress, reply} {
		select {
		case result := <-pending:
			results = append(results, result)
		case <-time.After(2 * time.Second):
			t.Fatal("a send behind a stalled socket never terminated")
		}
	}
	if !slices.Equal(results, []bool{false, false}) {
		t.Fatalf("a send that missed the deadline must report failure, not success: %v", results)
	}
	if !transport.Closed() {
		t.Fatal("the transport must latch closed so the connection teardown and reconnect can run")
	}
	waitUntil(t, "the stalled socket closed", func() bool { return len(ws.codes()) > 0 })
	if codes := ws.codes(); !slices.Equal(codes, []int{1011}) {
		t.Fatalf("the stalled socket must be closed, not left half open: %v", codes)
	}
}

func TestProgressThenFinalOrderingPreservedOnHealthySocket(t *testing.T) {
	ws := &recordingWS{}
	transport := NewWSTransport(ws, "127.0.0.1:1", nil)
	for _, frame := range []*pyjson.Object{wsEvent("tool.progress"), wsEvent("message.complete"),
		pyjson.ObjectOf("id", "submit", "result", pyjson.ObjectOf("status", "done"))} {
		if !transport.WriteAsync(frame) {
			t.Fatalf("write of %s failed", frameText(t, frame))
		}
	}
	if transport.Closed() {
		t.Fatal("transport closed")
	}
	kinds := []string{}
	for _, line := range ws.lines() {
		frame, err := pyjson.LoadsDict(line)
		if err != nil {
			t.Fatal(err)
		}
		kind := "reply"
		if params, ok := frame.GetOr("params", nil).(*pyjson.Object); ok {
			kind, _ = params.GetOr("type", "reply").(string)
		}
		kinds = append(kinds, kind)
	}
	if !slices.Equal(kinds, []string{"tool.progress", "message.complete", "reply"}) {
		t.Fatalf("kinds = %v", kinds)
	}
}
