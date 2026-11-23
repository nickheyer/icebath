package tuiserver

import (
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
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
