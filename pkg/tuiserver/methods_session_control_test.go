package tuiserver

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/goals"
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

// Calls a registered method the way the dispatcher does
func controlCall(ctx context.Context, method string, params map[string]any) map[string]any {
	return lookupMethod(method)(ctx, 91, params)
}

func readControl(t *testing.T, sid string) map[string]any {
	t.Helper()
	resp := controlCall(context.Background(), "session.control.read", map[string]any{"session_id": sid})
	result, _ := resp["result"].(map[string]any)
	control, isMap := result["control"].(map[string]any)
	if !isMap {
		t.Fatalf("read response %v", resp)
	}
	return control
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

func saveGoal(key string, edit func(*goals.GoalState)) *goals.GoalState {
	st := &goals.GoalState{Goal: "Finish the TUI control card", Status: "active", TurnsUsed: 3, MaxTurns: 12,
		CreatedAt: 100.0, LastTurnAt: 200.0}
	if edit != nil {
		edit(st)
	}
	goals.SaveGoal(context.Background(), key, st)
	return st
}

func saveLoop(key string, edit func(*agent.SessionLoopState)) {
	st := &agent.SessionLoopState{Prompt: "Check the deployment", Status: "active", Mode: "interval", IntervalSeconds: 300,
		CurrentDelay: 300, CreatedAt: 100.0, NextDueAt: 400.0}
	if edit != nil {
		edit(st)
	}
	agent.SaveLoop(context.Background(), key, st)
}

func saveHeartbeat(key string, edit func(*agent.HeartbeatState)) {
	st := &agent.HeartbeatState{Prompt: "Check the deployment", IntervalSeconds: big.NewInt(600), Status: "active", CreatedAt: 100.0,
		LastFiredAt: 150.0, FireCount: 2}
	if edit != nil {
		edit(st)
	}
	agent.SaveHeartbeat(context.Background(), key, st)
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
