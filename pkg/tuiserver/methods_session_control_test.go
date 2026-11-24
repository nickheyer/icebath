package tuiserver

import (
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Transport recording every frame written to it
type frameRecorder struct {
	mu     sync.Mutex
	frames []*pyjson.Object
}

func (r *frameRecorder) Write(obj *pyjson.Object) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, obj)
	return true
}

func (r *frameRecorder) Close() {}

func (r *frameRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

// Replaces a registered method for the test
func swapMethod(t *testing.T, name string, fn Handler) {
	t.Helper()
	methodsMu.Lock()
	previous, had := methods[name]
	methods[name] = fn
	methodsMu.Unlock()
	t.Cleanup(func() {
		methodsMu.Lock()
		defer methodsMu.Unlock()
		if had {
			methods[name] = previous
		} else {
			delete(methods, name)
		}
	})
}

// Transport stalling every frame of one session until released, recording the session of each frame written
type stallingTransport struct {
	stallSID string
	stalled  chan struct{}
	release  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	written  []string
}

func (s *stallingTransport) Write(obj *pyjson.Object) bool {
	params, _ := obj.Get("params")
	frameSID := ""
	if p, isObj := params.(*pyjson.Object); isObj {
		v, _ := p.Get("session_id")
		frameSID, _ = v.(string)
	}
	if frameSID == s.stallSID {
		s.once.Do(func() { close(s.stalled) })
		select {
		case <-s.release:
		case <-time.After(5 * time.Second):
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, frameSID)
	return true
}

func (s *stallingTransport) Close() {}
