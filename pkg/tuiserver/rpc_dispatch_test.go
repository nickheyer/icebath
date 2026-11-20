package tuiserver

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

func TestDispatchInlineRPCDoesNotBlockUnderGilPressure(t *testing.T) {
	released := &pythread.Event{}
	t.Cleanup(released.Set)
	slowSessionList := func(_ context.Context, rid any, _ map[string]any) map[string]any {
		released.Wait(5)
		return ok(rid, map[string]any{"sessions": []any{}})
	}
	swapMethod(t, "session.list", slowSessionList)
	swapMethod(t, "session.save", slowSessionList)
	swapMethod(t, "fast.check", func(_ context.Context, rid any, _ map[string]any) map[string]any {
		return ok(rid, map[string]any{"ok": true})
	})
	sink := &recordingTransport{}
	start := time.Now()
	var answers []map[string]any
	for _, call := range []struct {
		method string
		params map[string]any
	}{{"session.list", map[string]any{}}, {"session.save", map[string]any{"session_id": "s"}}} {
		answers = append(answers, Dispatch(context.Background(), map[string]any{"id": call.method, "method": call.method, "params": call.params}, sink))
	}
	if !reflect.DeepEqual(answers, []map[string]any{nil, nil}) {
		t.Fatalf("long handlers answered inline: %v", answers)
	}
	fastResp := Dispatch(context.Background(), map[string]any{"id": "fast", "method": "fast.check", "params": map[string]any{}}, sink)
	fastElapsed := time.Since(start)
	if !reflect.DeepEqual(fastResp["result"], map[string]any{"ok": true}) {
		t.Fatalf("fast.check answered %v", fastResp)
	}
	if fastElapsed >= 2*time.Second {
		t.Fatalf("fast handler blocked for %s behind slow session.list", fastElapsed)
	}
	released.Set()
}

// Transport recording every frame written to it
type recordingTransport struct {
	mu     sync.Mutex
	frames []map[string]any
}

func (r *recordingTransport) Write(obj *pyjson.Object) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	frame, _ := pyjson.Plain(obj).(map[string]any)
	r.frames = append(r.frames, frame)
	return true
}

func (r *recordingTransport) Close() {}
