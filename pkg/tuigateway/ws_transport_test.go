package tuigateway

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Socket counting the sends in flight while each one takes a moment
type countingWS struct {
	mu                sync.Mutex
	active, maxActive int
	sent              []string
}

func (w *countingWS) SendText(ctx context.Context, line string) error {
	w.mu.Lock()
	w.active++
	w.maxActive = max(w.maxActive, w.active)
	w.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	w.mu.Lock()
	w.sent = append(w.sent, line)
	w.active--
	w.mu.Unlock()
	return nil
}

func (w *countingWS) Close(code int) error { return nil }

func TestWsTransportSerializesConcurrentSends(t *testing.T) {
	ws := &countingWS{}
	transport := NewWSTransport(ws, "serialize-test", nil)
	results := make(chan bool, 2)
	for idx := 1; idx <= 2; idx++ {
		go func() { results <- transport.Write(pyjson.ObjectOf("idx", idx)) }()
	}
	for range 2 {
		select {
		case ok := <-results:
			if !ok {
				t.Fatal("write failed")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("write never returned")
		}
	}
	if len(ws.sent) != 2 || ws.maxActive != 1 || transport.Closed() {
		t.Fatalf("sent = %d, max active = %d, closed = %v", len(ws.sent), ws.maxActive, transport.Closed())
	}
}

func TestWsTransportRepliesWithErrorForUnserializableResponse(t *testing.T) {
	var logs bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })
	ws := &recordingWS{}
	bad := func() *pyjson.Object {
		return pyjson.ObjectOf("jsonrpc", "2.0", "id", "profiles",
			"result", pyjson.ObjectOf("created", pydatetime.DateTime{Year: 2026, Month: 8, Day: 22}))
	}
	transport := NewWSTransport(ws, "serialization-test", nil)
	if !transport.Write(bad()) {
		t.Fatal("write of the unserializable frame failed")
	}
	if !transport.Write(pyjson.ObjectOf("jsonrpc", "2.0", "id", "next", "result", pyjson.NewObject())) {
		t.Fatal("write after the unserializable frame failed")
	}
	if !transport.WriteAsync(bad()) {
		t.Fatal("awaited write of the unserializable frame failed")
	}
	if transport.Closed() {
		t.Fatal("transport closed")
	}
	sent := ws.lines()
	ids := []any{}
	frames := []*pyjson.Object{}
	for _, line := range sent {
		frame, err := pyjson.LoadsDict(line)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
		ids = append(ids, frame.GetOr("id", nil))
	}
	if !slices.Equal(ids, []any{"profiles", "next", "profiles"}) {
		t.Fatalf("ids = %v", ids)
	}
	for _, frame := range []*pyjson.Object{frames[0], frames[2]} {
		errObj, _ := frame.GetOr("error", nil).(*pyjson.Object)
		if errObj == nil || !jsonIsInt(errObj.GetOr("code", nil), -32603) {
			t.Fatalf("frame = %s", frameText(t, frame))
		}
		message, _ := errObj.GetOr("message", nil).(string)
		if !strings.HasPrefix(message, "response serialization error") || !strings.Contains(message, "datetime") {
			t.Fatalf("message = %q", message)
		}
	}
	if sent[1] != `{"jsonrpc": "2.0", "id": "next", "result": {}}` {
		t.Fatalf("second frame = %s", sent[1])
	}
	if count := strings.Count(logs.String(), "frame serialization failed"); count != 2 {
		t.Fatalf("serialization failures logged = %d: %s", count, logs.String())
	}
}

// Socket recording the lines it entered and holding the line A1 until released
type batchOrderWS struct {
	mu           sync.Mutex
	entered      []string
	firstEntered chan struct{}
	releaseFirst chan struct{}
}

func (w *batchOrderWS) SendText(ctx context.Context, line string) error {
	w.mu.Lock()
	w.entered = append(w.entered, line)
	w.mu.Unlock()
	if line == "A1" {
		close(w.firstEntered)
		<-w.releaseFirst
	}
	return nil
}

func (w *batchOrderWS) Close(code int) error { return nil }

func (w *batchOrderWS) lines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.entered)
}

func TestWsTransportPreservesCrossBatchOrder(t *testing.T) {
	ws := &batchOrderWS{firstEntered: make(chan struct{}), releaseFirst: make(chan struct{})}
	transport := NewWSTransport(ws, "batch-order-test", nil)
	var done sync.WaitGroup
	done.Add(2)
	go func() {
		defer done.Done()
		transport.safeSendMany(transport.sendLock.reserve(), []string{"A1", "A2"}, nil, 0)
	}()
	<-ws.firstEntered
	go func() {
		defer done.Done()
		transport.safeSendMany(transport.sendLock.reserve(), []string{"B1", "B2"}, nil, 0)
	}()
	waitUntil(t, "the second batch reached the transport", func() bool { return transport.sendLock.waiting() == 1 })
	if entered := ws.lines(); !slices.Equal(entered, []string{"A1"}) {
		t.Fatalf("entered = %v", entered)
	}
	close(ws.releaseFirst)
	done.Wait()
	if entered := ws.lines(); !slices.Equal(entered, []string{"A1", "A2", "B1", "B2"}) {
		t.Fatalf("entered = %v", entered)
	}
}
