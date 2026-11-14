package tuiserver_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/dashboard"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Transport handing every written frame to a queue
type queueTransport struct{ frames chan map[string]any }

func (q *queueTransport) Write(obj *pyjson.Object) bool {
	frame, _ := pyjson.Plain(obj).(map[string]any)
	q.frames <- frame
	return true
}

func (q *queueTransport) Close() {}

func TestQueuedAndRunningRPCHoldAdmissionUntilTheResponse(t *testing.T) {
	prevFence, prevProof := provider.Retirement, provider.RetirementIdleProof
	fence := provider.NewRetirementFence()
	provider.Retirement, provider.RetirementIdleProof = fence, dashboard.RetirementIdleProof
	t.Cleanup(func() { provider.Retirement, provider.RetirementIdleProof = prevFence, prevProof })
	releasePool, entered, releaseHandler := &pythread.Event{}, &pythread.Event{}, &pythread.Event{}
	t.Cleanup(func() { releasePool.Set(); releaseHandler.Set() })
	transport := &queueTransport{frames: make(chan map[string]any, 4)}
	tuiserver.SwapMethod(t, "test.retirement", func(_ context.Context, rid any, _ map[string]any) map[string]any {
		entered.Set()
		if !releaseHandler.Wait(10) {
			t.Error("handler never released")
		}
		return tuiserver.OK(rid, map[string]any{"done": true})
	})
	tuiserver.SwapLongHandlers(t, "test.retirement")
	submit := tuiserver.SwapRPCPool(t, 1)
	blocked := &pythread.Event{}
	submit(func() {
		releasePool.Wait(10)
		blocked.Set()
	})
	if resp := tuiserver.Dispatch(context.Background(), map[string]any{"id": "test", "method": "test.retirement"}, transport); resp != nil {
		t.Fatalf("pooled dispatch answered inline %v", resp)
	}
	notReady := map[string]any{"ok": false, "idle": false}
	if got := fence.Prepare(); !reflect.DeepEqual(got, notReady) {
		t.Fatalf("prepare with a queued request = %v", got)
	}
	if idle := dashboard.IdleProof(nil, nil)["idle"]; idle != false {
		t.Fatalf("idle proof with a queued request = %v", idle)
	}
	releasePool.Set()
	if !blocked.Wait(10) {
		t.Fatal("blocking job never finished")
	}
	if !entered.Wait(10) {
		t.Fatal("handler never entered")
	}
	if got := fence.Prepare(); !reflect.DeepEqual(got, notReady) {
		t.Fatalf("prepare with a running request = %v", got)
	}
	releaseHandler.Set()
	select {
	case frame := <-transport.frames:
		if !reflect.DeepEqual(frame["result"], map[string]any{"done": true}) {
			t.Fatalf("response frame %v", frame)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no response frame")
	}
	deadline := time.Now().Add(10 * time.Second)
	for fence.ActiveCount() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if idle := dashboard.IdleProof(nil, nil)["idle"]; idle != true {
		t.Fatalf("idle proof after the response = %v", idle)
	}
	token, _ := fence.Prepare()["token"].(string)
	late := tuiserver.Dispatch(context.Background(), map[string]any{"id": "late", "method": "test.retirement"}, transport)
	if code := late["error"].(map[string]any)["code"]; code != 5035 {
		t.Fatalf("late dispatch answered %v", late)
	}
	if got := fence.Cancel(token); !reflect.DeepEqual(got, map[string]any{"ok": true}) {
		t.Fatalf("cancel = %v", got)
	}
}
