package tuigateway

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Socket whose send never completes, recording each close code
type neverSendingWS struct {
	mu         sync.Mutex
	closeCodes []int
	done       chan struct{}
}

func (w *neverSendingWS) SendText(ctx context.Context, payload string) error {
	select {
	case <-w.done:
	case <-ctx.Done():
	}
	return ctx.Err()
}

func (w *neverSendingWS) Close(code int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeCodes = append(w.closeCodes, code)
	return nil
}

func (w *neverSendingWS) codes() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.closeCodes)
}

func awaitFrameCount(t *testing.T, transport *gatedRecorder, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for transport.count() < count {
		if time.Now().After(deadline) {
			t.Fatalf("expected %d frames, got %d", count, transport.count())
		}
		time.Sleep(time.Millisecond)
	}
}

// Emits frames a WebSocket peer blocks on until the slow mailbox overflows, returning the last index
func overflowSlowPeer(t *testing.T, fan *FanoutTransport, healthy *gatedRecorder, slow Transport) int {
	t.Helper()
	for n := range FanoutMaxPendingFrames + 64 {
		if !fan.Write(pyjson.ObjectOf("params", pyjson.ObjectOf("type", "tool.progress", "n", n))) {
			t.Fatal("fanout write failed")
		}
		awaitFrameCount(t, healthy, n+1)
		if !fan.Contains(slow) {
			return n
		}
	}
	t.Fatal("slow peer never overflowed")
	return 0
}

func TestOverflowClosesOnlyTheSlowPeerAndHealthyKeepsStreaming(t *testing.T) {
	stalled := &neverSendingWS{done: make(chan struct{})}
	t.Cleanup(func() { close(stalled.done) })
	slow := NewWSTransport(stalled, "slow", nil)
	healthy := newGatedRecorder(0)
	fan := NewFanoutTransport(healthy, slow)
	defer fan.Close()
	lastN := overflowSlowPeer(t, fan, healthy, slow)
	deadline := time.Now().Add(2 * time.Second)
	for len(stalled.codes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if codes := stalled.codes(); !slices.Equal(codes, []int{1011}) {
		t.Fatalf("close codes = %v", codes)
	}
	slow.Abort()
	time.Sleep(50 * time.Millisecond)
	if codes := stalled.codes(); !slices.Equal(codes, []int{1011}) {
		t.Fatalf("a second overflow signal scheduled another close: %v", codes)
	}
	if !slow.Closed() || healthy.isClosed() || !fan.Contains(healthy) {
		t.Fatalf("slow closed = %v, healthy closed = %v, healthy attached = %v", slow.Closed(), healthy.isClosed(), fan.Contains(healthy))
	}
	after := pyjson.ObjectOf("params", pyjson.ObjectOf("type", "message.complete", "n", lastN+1))
	if !fan.Write(after) {
		t.Fatal("write after the overflow failed")
	}
	awaitFrameCount(t, healthy, lastN+2)
	if got, want := frameText(t, healthy.last()), frameText(t, after); got != want {
		t.Fatalf("last frame = %s want %s", got, want)
	}
}

func TestFanoutCloseAndDetachLeavePeerSocketsOpen(t *testing.T) {
	kept, detached := newGatedRecorder(0), newGatedRecorder(0)
	fan := NewFanoutTransport(kept, detached)
	if !fan.Detach(detached) {
		t.Fatal("detach failed")
	}
	fan.Close()
	if kept.isClosed() || detached.isClosed() {
		t.Fatal("the fanout closed a peer socket")
	}
	if fan.Contains(kept) || fan.Contains(detached) {
		t.Fatal("a closed fanout kept a peer")
	}
}
