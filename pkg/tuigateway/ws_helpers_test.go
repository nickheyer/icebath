package tuigateway

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Polls a condition for up to ten seconds
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Swaps a package duration for one test
func setDuration(t *testing.T, target *time.Duration, value time.Duration) {
	t.Helper()
	saved := *target
	*target = value
	t.Cleanup(func() { *target = saved })
}

// Socket recording each line it is sent and each close code
type recordingWS struct {
	mu         sync.Mutex
	sent       []string
	closeCodes []int
}

func (w *recordingWS) SendText(ctx context.Context, line string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent = append(w.sent, line)
	return nil
}

func (w *recordingWS) Close(code int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeCodes = append(w.closeCodes, code)
	return nil
}

func (w *recordingWS) lines() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.sent)
}

// Event frame of one type with no session
func wsEvent(kind string) *pyjson.Object {
	return pyjson.ObjectOf("method", "event", "params", pyjson.ObjectOf("type", kind))
}

// Text of a frame as every transport serializes it
func frameText(t *testing.T, frame *pyjson.Object) string {
	t.Helper()
	text, err := pyjson.Dumps(frame, pyjson.EnsureASCII(false))
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// Whether no peer writer of a fanout is running
func fanoutIdle(f *FanoutTransport) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, peer := range f.peers {
		if peer.writing {
			return false
		}
	}
	return true
}

// Transport recording frames, an optional delay holding each write until released or closed
type gatedRecorder struct {
	mu       sync.Mutex
	frames   []*pyjson.Object
	closed   bool
	delay    time.Duration
	released chan struct{}
	once     sync.Once
}

func newGatedRecorder(delay time.Duration) *gatedRecorder {
	return &gatedRecorder{delay: delay, released: make(chan struct{})}
}

func (r *gatedRecorder) Write(obj *pyjson.Object) bool {
	if r.delay > 0 {
		select {
		case <-r.released:
		case <-time.After(r.delay):
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, obj)
	return true
}

func (r *gatedRecorder) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.once.Do(func() { close(r.released) })
}

func (r *gatedRecorder) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func (r *gatedRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

func (r *gatedRecorder) last() *pyjson.Object {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frames[len(r.frames)-1]
}
