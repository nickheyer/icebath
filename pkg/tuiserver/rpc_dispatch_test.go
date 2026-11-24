package tuiserver

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	syncutil "github.com/nickheyer/icebath/pkg/utils/sync"
)

func TestDispatchInlineRPCDoesNotBlockUnderGilPressure(t *testing.T) {
	released := &syncutil.Event{}
	t.Cleanup(released.Set)
	slowManage := func(_ context.Context, rid any, _ map[string]any) map[string]any {
		released.Wait(5)
		return ok(rid, map[string]any{})
	}
	swapMethod(t, "browser.manage", slowManage)
	swapMethod(t, "plugins.manage", slowManage)
	swapMethod(t, "fast.check", func(_ context.Context, rid any, _ map[string]any) map[string]any {
		return ok(rid, map[string]any{"ok": true})
	})
	sink := &recordingTransport{}
	start := time.Now()
	var answers []map[string]any
	for _, call := range []struct {
		method string
		params map[string]any
	}{{"browser.manage", map[string]any{}}, {"plugins.manage", map[string]any{"action": "list"}}} {
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
		t.Fatalf("fast handler blocked for %s behind slow browser.manage", fastElapsed)
	}
	released.Set()
}

// Transport recording every frame written to it
type recordingTransport struct {
	mu     sync.Mutex
	frames []map[string]any
}

func (r *recordingTransport) Write(obj *jsonutil.Object) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	frame, _ := jsonutil.Plain(obj).(map[string]any)
	r.frames = append(r.frames, frame)
	return true
}

func (r *recordingTransport) Close() {}
