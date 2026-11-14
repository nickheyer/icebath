package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

type oneshotFrames struct {
	mu     sync.Mutex
	frames []*pyjson.Object
}

func (t *oneshotFrames) Write(obj *pyjson.Object) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.frames = append(t.frames, obj)
	return true
}

func (t *oneshotFrames) Close() {}

func (t *oneshotFrames) snapshot() []*pyjson.Object {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*pyjson.Object(nil), t.frames...)
}

func oneshotConnection(a *SessionAuthority) (*AuthorityConnection, *oneshotFrames) {
	frames := &oneshotFrames{}
	return &AuthorityConnection{authority: a, transport: frames}, frames
}

type oneshotCalls struct {
	gate       chan struct{}
	mu         sync.Mutex
	ran        []string
	live, peak int
	gateOnce   sync.Once
}

func newOneshotCalls() *oneshotCalls { return &oneshotCalls{gate: make(chan struct{})} }

func (c *oneshotCalls) open() { c.gateOnce.Do(func() { close(c.gate) }) }

func (c *oneshotCalls) work(label string) func(context.Context) (any, error) {
	return func(context.Context) (any, error) {
		c.mu.Lock()
		c.ran = append(c.ran, label)
		c.live++
		c.peak = max(c.peak, c.live)
		c.mu.Unlock()
		select {
		case <-c.gate:
		case <-time.After(10 * time.Second):
		}
		c.mu.Lock()
		c.live--
		c.mu.Unlock()
		return pyjson.ObjectOf("text", label), nil
	}
}

func (c *oneshotCalls) ranCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ran)
}

func oneshotWaitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func poolCounts(a *SessionAuthority) (answers, calls int) {
	pool := oneshotPoolOf(a)
	pool.lock.Lock()
	defer pool.lock.Unlock()
	return len(pool.answers), len(pool.calls)
}

func oneshotAuthority() *SessionAuthority { return &SessionAuthority{} }

func oneshotRefusalReason(t *testing.T, err error) string {
	t.Helper()
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) {
		t.Fatalf("not a refusal %v", err)
	}
	return refusal.Reason
}

func TestAFullPoolRefusesByNameAndNeverRunsPastItsWorkers(t *testing.T) {
	calls := newOneshotCalls()
	conn, frames := oneshotConnection(oneshotAuthority())
	ctx := context.Background()
	for n := range oneshotAdmitted {
		if err := NewDeferredReply(calls.work(fmt.Sprintf("call%d", n))).Start(ctx, conn, n, nil); err != nil {
			t.Fatal(err)
		}
	}
	err := NewDeferredReply(calls.work("extra")).Start(ctx, conn, "extra", nil)
	oneshotWaitUntil(t, func() bool { return calls.ranCount() == oneshotWorkers })
	calls.open()
	oneshotWaitUntil(t, func() bool { return len(frames.snapshot()) == oneshotAdmitted })
	if reason := oneshotRefusalReason(t, err); reason != "oneshot_busy" {
		t.Fatalf("reason %q", reason)
	}
	if calls.peak != oneshotWorkers || calls.ranCount() != oneshotAdmitted {
		t.Fatalf("peak %d ran %d", calls.peak, calls.ranCount())
	}
	ids := []int{}
	for _, frame := range frames.snapshot() {
		ids = append(ids, frame.GetOr("id", nil).(int))
	}
	sort.Ints(ids)
	for i, id := range ids {
		if id != i {
			t.Fatalf("ids %v", ids)
		}
	}
}

func TestAClosedSocketDropsItsQueuedCallsAndItsRunningReply(t *testing.T) {
	calls := newOneshotCalls()
	a := oneshotAuthority()
	gone, goneFrames := oneshotConnection(a)
	kept, keptFrames := oneshotConnection(a)
	ctx := context.Background()
	for n := range oneshotWorkers {
		label := fmt.Sprintf("running%d", n)
		if err := NewDeferredReply(calls.work(label)).Start(ctx, gone, label, nil); err != nil {
			t.Fatal(err)
		}
	}
	oneshotWaitUntil(t, func() bool { return calls.ranCount() == oneshotWorkers })
	_ = NewDeferredReply(calls.work("queued")).Start(ctx, gone, "queued", nil)
	_ = NewDeferredReply(calls.work("other")).Start(ctx, kept, "other", nil)
	forgetOneshots(gone)
	pool := oneshotPoolOf(a)
	oneshotWaitUntil(t, func() bool {
		pool.lock.Lock()
		defer pool.lock.Unlock()
		for answer := range pool.answers {
			if answer.conn == gone {
				return false
			}
		}
		return true
	})
	_, held := poolCounts(a)
	calls.open()
	oneshotWaitUntil(t, func() bool { answers, live := poolCounts(a); return answers == 0 && live == 0 })
	if held != oneshotWorkers+1 {
		t.Fatalf("held %d", held)
	}
	for _, label := range calls.ran {
		if label == "queued" {
			t.Fatal("queued call ran")
		}
	}
	if len(goneFrames.snapshot()) != 0 {
		t.Fatalf("gone frames %v", goneFrames.snapshot())
	}
	got := keptFrames.snapshot()
	want, _ := pyjson.Dumps(pyjson.ObjectOf("jsonrpc", "2.0", "id", "other", "result", pyjson.ObjectOf("text", "other")))
	if len(got) != 1 {
		t.Fatalf("kept frames %v", got)
	}
	if text, _ := pyjson.Dumps(got[0]); text != want {
		t.Fatalf("kept frame %s", text)
	}
}

func TestAReconnectLoopNeverRetainsMoreQueuedPayloadsThanTheBound(t *testing.T) {
	calls := newOneshotCalls()
	a := oneshotAuthority()
	busy, busyFrames := oneshotConnection(a)
	ctx := context.Background()
	for n := range oneshotWorkers {
		label := fmt.Sprintf("running%d", n)
		_ = NewDeferredReply(calls.work(label)).Start(ctx, busy, label, nil)
	}
	oneshotWaitUntil(t, func() bool { return calls.ranCount() == oneshotWorkers })
	pool := oneshotPoolOf(a)
	retained := []int{}
	for cycle := range 5 {
		gone, _ := oneshotConnection(a)
		for n := range oneshotAdmitted - oneshotWorkers {
			label := fmt.Sprintf("%d.%d", cycle, n)
			if err := NewDeferredReply(func(context.Context) (any, error) { return pyjson.ObjectOf("text", label), nil }).Start(ctx, gone, label, nil); err != nil {
				t.Fatal(err)
			}
		}
		forgetOneshots(gone)
		oneshotWaitUntil(t, func() bool { answers, _ := poolCounts(a); return answers == oneshotWorkers })
		oneshotWaitUntil(t, func() bool { pool.lock.Lock(); defer pool.lock.Unlock(); return len(pool.queue) == 0 })
		pool.lock.Lock()
		retained = append(retained, len(pool.work))
		pool.lock.Unlock()
	}
	calls.open()
	oneshotWaitUntil(t, func() bool { answers, live := poolCounts(a); return answers == 0 && live == 0 })
	for _, count := range retained {
		if count != 0 {
			t.Fatalf("retained %v", retained)
		}
	}
	if calls.ranCount() != oneshotWorkers || len(busyFrames.snapshot()) != oneshotWorkers {
		t.Fatalf("ran %d frames %d", calls.ranCount(), len(busyFrames.snapshot()))
	}
}

func drainingFrame(id any) string {
	text, _ := pyjson.Dumps(pyjson.ObjectOf("jsonrpc", "2.0", "id", id, "error", pyjson.ObjectOf("code", 4001,
		"message", "runtime_draining", "data", pyjson.ObjectOf("reason", "runtime_draining"))))
	return text
}

func TestOwnerShutdownRefusesNewCallsAndCancelsTheQueue(t *testing.T) {
	calls := newOneshotCalls()
	a := oneshotAuthority()
	conn, frames := oneshotConnection(a)
	ctx := context.Background()
	idle := oneshotAuthority()
	StopOneshots(idle)
	idleConn, _ := oneshotConnection(idle)
	if err := NewDeferredReply(calls.work("idle")).Start(ctx, idleConn, "idle", nil); err == nil {
		t.Fatal("idle owner admitted a call")
	}
	for n := range oneshotWorkers + 2 {
		_ = NewDeferredReply(calls.work(fmt.Sprintf("call%d", n))).Start(ctx, conn, n, nil)
	}
	oneshotWaitUntil(t, func() bool { return calls.ranCount() == oneshotWorkers })
	StopOneshots(a)
	err := NewDeferredReply(calls.work("late")).Start(ctx, conn, "late", nil)
	calls.open()
	oneshotWaitUntil(t, func() bool { answers, live := poolCounts(a); return answers == 0 && live == 0 })
	if reason := oneshotRefusalReason(t, err); reason != "runtime_draining" {
		t.Fatalf("reason %q", reason)
	}
	if calls.ranCount() != oneshotWorkers {
		t.Fatalf("ran %d", calls.ranCount())
	}
	got := frames.snapshot()
	ids := []int{}
	for _, frame := range got {
		id := frame.GetOr("id", nil).(int)
		ids = append(ids, id)
		if text, _ := pyjson.Dumps(frame); text != drainingFrame(id) {
			t.Fatalf("frame %s", text)
		}
	}
	sort.Ints(ids)
	if len(ids) != oneshotWorkers+2 {
		t.Fatalf("ids %v", ids)
	}
	for i, id := range ids {
		if id != i {
			t.Fatalf("ids %v", ids)
		}
	}
}

func TestOwnerShutdownAnswersACallWhoseReplyTaskHasNotStartedYet(t *testing.T) {
	calls := newOneshotCalls()
	a := oneshotAuthority()
	conn, frames := oneshotConnection(a)
	_ = NewDeferredReply(calls.work("fresh")).Start(context.Background(), conn, "fresh", nil)
	StopOneshots(a)
	oneshotWaitUntil(t, func() bool { answers, _ := poolCounts(a); return answers == 0 })
	calls.open()
	oneshotWaitUntil(t, func() bool { _, live := poolCounts(a); return live == 0 })
	got := frames.snapshot()
	if len(got) != 1 {
		t.Fatalf("frames %v", got)
	}
	if text, _ := pyjson.Dumps(got[0]); text != drainingFrame("fresh") {
		t.Fatalf("frame %s", text)
	}
}

type oneshotScopeKey struct{}

func TestTheCallRunsInTheHandlersContextAndItsResultIsCheckedBeforeTheReply(t *testing.T) {
	conn, frames := oneshotConnection(oneshotAuthority())
	scoped := context.WithValue(context.Background(), oneshotScopeKey{}, "secondary-profile")
	_ = NewDeferredReply(func(ctx context.Context) (any, error) {
		return pyjson.ObjectOf("text", ctx.Value(oneshotScopeKey{})), nil
	}).Start(scoped, conn, "scoped", func(any) error { return nil })
	badResult := func(result any) error {
		text, _ := pyjson.Dumps(result)
		return &PyError{Kind: "ValueError", Msg: "result violates its contract: " + text}
	}
	_ = NewDeferredReply(func(context.Context) (any, error) { return pyjson.ObjectOf("text", "x"), nil }).Start(context.Background(), conn, "checked", badResult)
	oneshotWaitUntil(t, func() bool { return len(frames.snapshot()) == 2 })
	byID := map[string]*pyjson.Object{}
	for _, frame := range frames.snapshot() {
		byID[frame.GetOr("id", nil).(string)] = frame
	}
	if text, _ := pyjson.Dumps(byID["scoped"].GetOr("result", nil)); text != `{"text": "secondary-profile"}` {
		t.Fatalf("scoped %s", text)
	}
	failure := byID["checked"].GetOr("error", nil).(*pyjson.Object)
	if failure.GetOr("code", nil) != 5030 || !strings.Contains(failure.GetOr("message", "").(string), "violates its contract") {
		t.Fatalf("checked %v", failure)
	}
}

func TestOwnerShutdownDropsAResultThatArrivedButWasNotYetWritten(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	calls := newOneshotCalls()
	a := oneshotAuthority()
	conn, frames := oneshotConnection(a)
	_ = NewDeferredReply(calls.work("late")).Start(context.Background(), conn, "late", nil)
	oneshotWaitUntil(t, func() bool { return calls.ranCount() == 1 })
	pool := oneshotPoolOf(a)
	pool.lock.Lock()
	running := []*oneshotCall{}
	for call := range pool.calls {
		running = append(running, call)
	}
	pool.lock.Unlock()
	pool.loop.call(func() {
		calls.open()
		for _, call := range running {
			select {
			case <-call.done:
			case <-time.After(10 * time.Second):
				t.Error("the call did not set its result")
			}
		}
		pool.stop()
	})
	oneshotWaitUntil(t, func() bool { answers, live := poolCounts(a); return answers == 0 && live == 0 })
	got := frames.snapshot()
	if len(got) != 1 {
		t.Fatalf("frames %v", got)
	}
	if text, _ := pyjson.Dumps(got[0]); text != drainingFrame("late") {
		t.Fatalf("frame %s", text)
	}
}

func TestAWorkerThatCannotStartGivesItsSlotBack(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	pool := newOneshotPool()
	real := pool.executor
	unstartable := tools.NewDaemonPool(oneshotWorkers, "icebath-oneshot")
	unstartable.Shutdown(false, false)
	pool.executor = unstartable
	refusals := []string{}
	for range oneshotAdmitted + 1 {
		_, err := pool.submit(func() (any, error) { return pyjson.ObjectOf("text", "never"), nil })
		var fault *PyError
		var refusal *state.RuntimeStoreError
		switch {
		case errors.As(err, &fault):
			refusals = append(refusals, fault.Kind)
		case errors.As(err, &refusal):
			refusals = append(refusals, "RuntimeStoreError")
		case err == nil:
			refusals = append(refusals, "accepted")
		}
	}
	want := []string{}
	for range oneshotAdmitted + 1 {
		want = append(want, "RuntimeError")
	}
	if fmt.Sprint(refusals) != fmt.Sprint(want) {
		t.Fatalf("refusals %v", refusals)
	}
	pool.lock.Lock()
	calls, queued, work, threads := len(pool.calls), len(pool.queue), len(pool.work), pool.threads
	pool.lock.Unlock()
	if calls != 0 || queued != 0 || work != 0 || threads != 0 {
		t.Fatalf("calls %d queue %d work %d threads %d", calls, queued, work, threads)
	}
	pool.executor = real
	call, err := pool.submit(func() (any, error) { return pyjson.ObjectOf("text", "ok"), nil })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-call.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not run")
	}
	if text, _ := pyjson.Dumps(call.result); text != `{"text": "ok"}` || call.err != nil {
		t.Fatalf("result %s %v", text, call.err)
	}
}
