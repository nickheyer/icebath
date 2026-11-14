package tuiserver

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Installs a fresh retirement fence for one case and restores the process fence after it
func swapRetirementFence(t *testing.T) *provider.RetirementFence {
	t.Helper()
	prev := provider.Retirement
	fence := provider.NewRetirementFence()
	provider.Retirement = fence
	t.Cleanup(func() { provider.Retirement = prev })
	return fence
}

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

func TestPromptClaimAndAutomaticContinuationsCannotCrossPrepare(t *testing.T) {
	fence := swapRetirementFence(t)
	prevProof := provider.RetirementIdleProof
	provider.RetirementIdleProof = func() (map[string]any, error) { return map[string]any{"idle": true}, nil }
	t.Cleanup(func() { provider.RetirementIdleProof = prevProof })
	session := tuigateway.NewSession(map[string]any{"history_lock": &sync.Mutex{}, "running": false, "history": []any{}})
	var dispatchedMu sync.Mutex
	var dispatched []any
	prevRun := runPromptSubmit
	runPromptSubmit = func(_ context.Context, rid any, sid string, _ *tuigateway.Session, text any, _ promptSubmitOptions) bool {
		dispatchedMu.Lock()
		defer dispatchedMu.Unlock()
		dispatched = append(dispatched, []any{rid, sid, text})
		return true
	}
	t.Cleanup(func() { runPromptSubmit = prevRun })
	token, _ := fence.Prepare()["token"].(string)
	ctx := context.Background()
	errResp, _ := lockInSubmitTurn(ctx, "r", "s", session, "hello", map[string]any{}, false, nil, nil, "")
	if errResp == nil || errorCode(t, errResp) != 5035 {
		t.Fatalf("lock in answered %v", errResp)
	}
	if session.Get("running") != false {
		t.Fatalf("running = %v", session.Get("running"))
	}
	session.Set("queued_prompt", map[string]any{"text": "next", "transport": nil})
	if drainQueuedPrompt(ctx, "r", "s", session) {
		t.Fatal("queued prompt drained across prepare")
	}
	if queued, _ := session.Get("queued_prompt").(map[string]any); queued["text"] != "next" {
		t.Fatalf("queued prompt = %v", session.Get("queued_prompt"))
	}
	if notifClaimTurn(session) {
		t.Fatal("notification claimed a turn across prepare")
	}
	goalSession := tuigateway.NewSession(map[string]any{"history_lock": &sync.Mutex{}, "running": false})
	runPostTurnFollowups(ctx, "r", "s", goalSession, map[string]any{}, "keep going")
	if goalSession.Get("running") != false {
		t.Fatalf("goal session running = %v", goalSession.Get("running"))
	}
	dispatchedMu.Lock()
	got := len(dispatched)
	dispatchedMu.Unlock()
	if got != 0 {
		t.Fatalf("dispatched %v", dispatched)
	}
	if !reflect.DeepEqual(fence.Cancel(token), map[string]any{"ok": true}) {
		t.Fatal("cancel refused the permit")
	}
	if !notifClaimTurn(session) {
		t.Fatal("notification claim refused after cancel")
	}
	if session.Get("running") != true {
		t.Fatalf("running = %v after claim", session.Get("running"))
	}
}
