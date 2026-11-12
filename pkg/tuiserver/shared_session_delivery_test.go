package tuiserver

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Observer peer recording frames, writes failing once it closed
type deliveryPeer struct {
	mu     sync.Mutex
	id     int
	frames []map[string]any
	closed bool
}

func (p *deliveryPeer) Write(obj *pyjson.Object) bool {
	frame, _ := pyjson.Plain(obj).(map[string]any)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames = append(p.frames, frame)
	return !p.closed
}

func (p *deliveryPeer) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

func (p *deliveryPeer) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Frames received once at least want arrived or five seconds passed
func (p *deliveryPeer) received(want int) []map[string]any {
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		p.mu.Lock()
		frames := append([]map[string]any(nil), p.frames...)
		p.mu.Unlock()
		if len(frames) >= want || time.Now().After(deadline) {
			return frames
		}
	}
}

func TestReattachPreservesTerminalDelivery(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	first, second := &deliveryPeer{id: 1}, &deliveryPeer{id: 2}
	session := tuigateway.NewSession(map[string]any{"transport": first, "history_lock": &sync.Mutex{}, "running": true})
	swapSessions(t, map[string]*tuigateway.Session{"shared": session})
	unlock := lockHistory(session)
	rebindLiveTransport(ctx, "shared", session, second)
	unlock()
	tuigateway.Emit(ctx, "message.complete", "shared", map[string]any{"text": "finished"})
	got1, got2 := first.received(1), second.received(1)
	if len(got1) != 1 || !reflect.DeepEqual(got1, got2) {
		t.Fatalf("first %v second %v", got1, got2)
	}
	second.Close()
	if reaped, detached := closeSessionsForTransport(ctx, second, "ws_disconnect"); reaped != 0 || detached != 0 {
		t.Fatalf("counts %d %d", reaped, detached)
	}
	if viewers, _ := session.Get("viewers").(map[tuigateway.Transport]float64); viewers != nil {
		if _, kept := viewers[second]; kept {
			t.Fatal("departed peer still a viewer")
		}
	}
	tuigateway.Emit(ctx, "message.complete", "shared", map[string]any{"text": "still attached"})
	if got := first.received(2); len(got) != 2 {
		t.Fatalf("first %v", got)
	}
}
