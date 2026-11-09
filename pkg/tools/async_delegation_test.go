package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

type asyncTestAsker struct{}

func (asyncTestAsker) Ask(context.Context, []Question) (FormResult, error) { return FormResult{}, nil }

type asyncTestRetirement struct{}

func (asyncTestRetirement) Work() (bool, func()) { return true, func() {} }
func (asyncTestRetirement) Acquire() bool        { return true }
func (asyncTestRetirement) Release()             {}

func init() {
	if AsyncDelegationOwnerProbes.PidExists == nil {
		AsyncDelegationOwnerProbes.PidExists = proc.Exists
		AsyncDelegationOwnerProbes.ProcessStartTime = processStartTime
	}
	if AsyncDelegationRetirement == nil {
		AsyncDelegationRetirement = asyncTestRetirement{}
	}
}

func resetAsyncDelegationForTests() { ResetAsyncDelegationForTests() }

func asyncTestEnv(t *testing.T) *Env {
	t.Helper()
	resetAsyncDelegationForTests()
	drainCompletionQueue()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for asyncLiveRecordCount() > 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		resetAsyncDelegationForTests()
		drainCompletionQueue()
	})
	return &Env{Home: t.TempDir(), Values: &sync.Map{}}
}

func drainCompletionQueue() {
	for {
		if _, ok := DefaultProcessRegistry().CompletionQueue.GetNowait(); !ok {
			return
		}
	}
}

func drainFor(delegationID any, timeout time.Duration) map[string]any {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if evt, ok := DefaultProcessRegistry().CompletionQueue.GetNowait(); ok {
			if delegationID == nil || evt["delegation_id"] == delegationID {
				return evt
			}
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func runnerOf(out map[string]any) func(context.Context, *Env) (map[string]any, error) {
	return func(context.Context, *Env) (map[string]any, error) { return out, nil }
}

func TestCompletionEventLandsOnSharedQueueWithSessionKey(t *testing.T) {
	env := asyncTestEnv(t)
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{
		Goal: "compute X", Context: ptr("some context"), Toolsets: []string{"web", "file"}, Role: "leaf",
		Model: ptr("test-model"), SessionKey: "agent:main:cli:dm:local", ParentSessionID: ptr("20260703_parent_sid"),
		Runner:           runnerOf(map[string]any{"status": "completed", "summary": "the result", "api_calls": 3, "duration_seconds": 2.0, "model": "test-model"}),
		MaxAsyncChildren: 3,
	})
	if err != nil || res["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", res, err)
	}
	evt := drainFor(res["delegation_id"], 5*time.Second)
	if evt == nil || evt["type"] != "async_delegation" || evt["summary"] != "the result" ||
		evt["session_key"] != "agent:main:cli:dm:local" || evt["parent_session_id"] != "20260703_parent_sid" {
		t.Fatalf("event = %v", evt)
	}
	row, err := GetDurableDelegation(env, res["delegation_id"].(string))
	if err != nil || row["state"] != "completed" || row["delivery_state"] != "pending" || asyncMap(row["result"])["summary"] != "the result" {
		t.Fatalf("durable row = %v, %v", row, err)
	}
}

func TestDispatchRejectedAtCapacity(t *testing.T) {
	env := asyncTestEnv(t)
	gate := make(chan struct{})
	defer close(gate)
	blocker := func(context.Context, *Env) (map[string]any, error) {
		<-gate
		return map[string]any{"status": "completed", "summary": "x"}, nil
	}
	for i := 0; i < 2; i++ {
		r, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "task", Role: "leaf", Model: ptr("m"), Runner: blocker, MaxAsyncChildren: 2})
		if err != nil || r["status"] != "dispatched" {
			t.Fatalf("dispatch %d = %v, %v", i, r, err)
		}
	}
	r3, _ := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "task3", Role: "leaf", Model: ptr("m"), Runner: blocker, MaxAsyncChildren: 2})
	if r3["status"] != "rejected" || !strings.Contains(r3["error"].(string), "capacity reached") {
		t.Fatalf("third dispatch = %v", r3)
	}
}

func TestStalledRunnerIsInterruptedThenFinalized(t *testing.T) {
	env := asyncTestEnv(t)
	prevInterval, prevIdle, prevGrace := asyncStaleCheckInterval, asyncStaleIdleSeconds, asyncStallGraceSeconds
	asyncStaleCheckInterval, asyncStaleIdleSeconds, asyncStallGraceSeconds = 20*time.Millisecond, 0.05, 0.05
	t.Cleanup(func() {
		resetAsyncDelegationForTests()
		asyncStaleCheckInterval, asyncStaleIdleSeconds, asyncStallGraceSeconds = prevInterval, prevIdle, prevGrace
	})
	gate := make(chan struct{})
	var interrupted atomic.Int32
	res, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{
		Goal: "stuck child", Role: "leaf", Model: ptr("m"), MaxAsyncChildren: 1,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			select {
			case <-gate:
			case <-time.After(10 * time.Second):
			}
			return map[string]any{"status": "completed", "summary": "too late"}, nil
		},
		InterruptFn: func() error { interrupted.Add(1); return nil },
		ProgressFn:  func() (any, bool, error) { return []any{0, nil}, false, nil },
	})
	if err != nil || res["status"] != "dispatched" {
		t.Fatalf("dispatch = %v, %v", res, err)
	}
	evt := drainFor(res["delegation_id"], 5*time.Second)
	close(gate)
	if evt == nil || evt["status"] != "stalled" || evt["api_calls"] != 0 || !strings.Contains(evt["error"].(string), "stopped responding") {
		t.Fatalf("stalled event = %v", evt)
	}
	if evt["stall_phase"] != "idle" || evt["stall_grace_seconds"] != 0.05 {
		t.Fatalf("stall metadata = %v", evt)
	}
	if interrupted.Load() < 1 || AsyncDelegationActiveCount() != 0 {
		t.Fatalf("interrupts %d, active %d", interrupted.Load(), AsyncDelegationActiveCount())
	}
	if late := drainFor(nil, 500*time.Millisecond); late != nil {
		t.Fatalf("late runner enqueued a second completion %v", late)
	}
}

func TestAsyncInterruptAllAndSessionFence(t *testing.T) {
	env := asyncTestEnv(t)
	release := make(chan struct{})
	var calls atomic.Int32
	r, _ := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{
		Goal: "long", Role: "leaf", SessionKey: "sess-a", MaxAsyncChildren: 3,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			<-release
			return map[string]any{"status": "interrupted", "summary": nil}, nil
		},
		InterruptFn: func() error { calls.Add(1); close(release); return nil },
	})
	if !HasLiveForSession("sess-a", "", "") {
		t.Fatal("session should own a live delegation")
	}
	RefuseAsyncDelegationsForSession("tok", "sess-a", nil)
	refused, _ := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "late", SessionKey: "sess-a", Runner: runnerOf(nil)})
	if refused["status"] != "rejected" || refused["error"] != asyncDelegationRejectedClosing {
		t.Fatalf("fenced dispatch = %v", refused)
	}
	if n := InterruptAllAsyncDelegations("shutdown"); n != 1 || calls.Load() != 1 {
		t.Fatalf("interrupt_all = %d calls %d", n, calls.Load())
	}
	evt := drainFor(r["delegation_id"], 5*time.Second)
	if evt == nil || evt["status"] != "interrupted" {
		t.Fatalf("interrupted completion = %v", evt)
	}
	AllowAsyncDelegationsForSession("tok")
	again, _ := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "after", SessionKey: "sess-a", Runner: runnerOf(map[string]any{"status": "completed"})})
	if again["status"] != "dispatched" {
		t.Fatalf("dispatch after allow = %v", again)
	}
	drainFor(again["delegation_id"], 5*time.Second)
}

func TestAsyncClaimProtocolAndRequeue(t *testing.T) {
	env := asyncTestEnv(t)
	res, _ := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "g", Role: "leaf", Runner: runnerOf(map[string]any{"status": "completed", "summary": "s"})})
	evt := drainFor(res["delegation_id"], 5*time.Second)
	if evt == nil {
		t.Fatal("no completion")
	}
	claim, ok, err := ClaimEventDelivery(env, evt, "gateway")
	if err != nil || !ok || !strings.HasPrefix(claim, "gateway:") {
		t.Fatalf("claim = %q %v %v", claim, ok, err)
	}
	if _, ok2, _ := ClaimEventDelivery(env, evt, "tui"); ok2 {
		t.Fatal("a second consumer claimed a held delivery")
	}
	if ok, _ := DeferCompletionDelivery(env, res["delegation_id"].(string), claim); !ok {
		t.Fatal("defer failed")
	}
	claim, _, _ = ClaimEventDelivery(env, evt, "gateway")
	if err := CompleteEventDelivery(env, evt, claim); err != nil {
		t.Fatal(err)
	}
	row, _ := GetDurableDelegation(env, res["delegation_id"].(string))
	if row["delivery_state"] != "delivered" || row["delivery_attempts"] != 1 {
		t.Fatalf("delivered row = %v", row)
	}
	q := newEventQueue()
	if ok, err := RequeueAdmittedCompletion(env, res["delegation_id"].(string), q); !ok || err != nil || q.Len() != 1 {
		t.Fatalf("requeue = %v %v len %d", ok, err, q.Len())
	}
	again, _ := q.GetNowait()
	if again["restored"] != true || again["startup_replay"] != nil {
		t.Fatalf("requeued copy = %v", again)
	}
	interim := map[string]any{"type": "async_delegation", "delegation_id": "x", "task_failure_notice": true}
	if c, ok, _ := ClaimEventDelivery(env, interim, "gateway"); c != "" || !ok || CompletionCopyID(interim) != "" {
		t.Fatal("interim notice must need no claim and be no copy")
	}
}

func asyncInsertRow(t *testing.T, env *Env, id, stateName string, pid any, task, result string, dispatchedAt float64) {
	t.Helper()
	err := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		_, err := asyncExec(ctx, conn, `INSERT INTO async_delegations (delegation_id, origin_session, origin_ui_session_id,
			parent_session_id, state, dispatched_at, updated_at, delivery_state, delivery_attempts, owner_pid, task_json, result_json)
			VALUES (?, 'sk', 'ui', 'parent', ?, ?, ?, 'pending', 0, ?, ?, ?)`, id, stateName, dispatchedAt, dispatchedAt, pid, task, result)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAsyncRestoreRecoversAbandonedUnitWithRecordedChild(t *testing.T) {
	env := asyncTestEnv(t)
	now := asyncTimeNow()
	asyncInsertRow(t, env, "deleg_dead", "running", 999999, `{"goal":"2 parallel","goals":["a","b"],"is_batch":true}`, "", now)
	if err := RecordUnitChild(env, "deleg_dead", map[string]any{"task_index": 1, "status": "completed", "summary": "b done"}); err != nil {
		t.Fatal(err)
	}
	q := newEventQueue()
	n, err := RestoreUndeliveredCompletions(env, q)
	if err != nil || n != 1 {
		t.Fatalf("restore = %d %v", n, err)
	}
	evt, _ := q.GetNowait()
	if evt["status"] != "unknown" || evt["restored"] != true || evt["startup_replay"] != true || evt["last_known_status"] != "running" {
		t.Fatalf("restored event = %v", evt)
	}
	results := evt["results"].([]any)
	if len(results) != 2 || asyncMap(results[0])["status"] != "unknown" || asyncMap(results[1])["summary"] != "b done" {
		t.Fatalf("recovered results = %v", results)
	}
	if !strings.Contains(evt["error"].(string), "1/2 child results were recorded") {
		t.Fatalf("error = %v", evt["error"])
	}
	if n, _ := RestoreUndeliveredCompletions(env, q); n != 0 {
		t.Fatalf("second restore offered %d again", n)
	}
	ReturnCompletionOffer(evt)
	if n, _ := RestoreUndeliveredCompletions(env, q); n != 1 {
		t.Fatalf("restore after the offer came back = %d", n)
	}
	failed, err := FailedDelegationsForSession(env, "ui", "", 20, nil)
	if err != nil || len(failed) != 1 || failed[0]["task_index"] != 0 || failed[0]["goal"] != "a" || failed[0]["status"] != "unknown" {
		t.Fatalf("failed surface = %v %v", failed, err)
	}
}

func TestAsyncRestoreNeverCreatesLedgerAndDropsStaleReplay(t *testing.T) {
	env := asyncTestEnv(t)
	if n, err := RestoreUndeliveredCompletions(env, newEventQueue()); n != 0 || err != nil {
		t.Fatalf("restore without ledger = %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(env.Home, "state.db")); !os.IsNotExist(err) {
		t.Fatal("restore created the ledger")
	}
	old := asyncTimeNow() - asyncMaxCompletionReplayAgeS - 10
	asyncInsertRow(t, env, "deleg_old", "completed", os.Getpid(), "{}", "{}", old)
	if err := asyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		_, err := asyncExec(ctx, conn, "UPDATE async_delegations SET event_json='{\"type\":\"async_delegation\"}', completed_at=? WHERE delegation_id='deleg_old'", old)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := RestoreUndeliveredCompletions(env, newEventQueue()); n != 0 {
		t.Fatalf("stale row replayed %d", n)
	}
	row, _ := GetDurableDelegation(env, "deleg_old")
	if row["delivery_state"] != "dropped" {
		t.Fatalf("stale row = %v", row)
	}
}

func TestAsyncSweepReoffersLostCopyOnce(t *testing.T) {
	env := asyncTestEnv(t)
	res, _ := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "g", Runner: runnerOf(map[string]any{"status": "completed"})})
	if drainFor(res["delegation_id"], 5*time.Second) == nil {
		t.Fatal("no completion")
	}
	q := newEventQueue()
	future := asyncTimeNow() + asyncClaimLeaseS + asyncOrphanStaleS + 5
	n, err := SweepOrphanedCompletions(env, q, &future)
	if err != nil || n != 1 || q.Len() != 1 {
		t.Fatalf("sweep = %d %v len %d", n, err, q.Len())
	}
	if n, _ := SweepOrphanedCompletions(env, q, &future); n != 0 {
		t.Fatalf("sweep re-offered beside a queued copy %d", n)
	}
	evt, _ := q.GetNowait()
	MarkCopyHeld(CompletionCopyID(evt))
	if n, _ := SweepOrphanedCompletions(env, q, &future); n != 0 {
		t.Fatalf("sweep re-offered beside a held copy %d", n)
	}
	UnmarkCopyHeld(CompletionCopyID(evt))
	if got := MaybeSweepOrphanedCompletions(env, q, ptr(1.0)); got != 0 {
		t.Fatalf("maybe sweep inside the lease = %d", got)
	}
	if got := MaybeSweepOrphanedCompletions(env, q, ptr(2.0)); got != 0 {
		t.Fatalf("maybe sweep ran twice in one interval = %d", got)
	}
}

func TestAsyncBatchStatusAndTaskFailureNotice(t *testing.T) {
	env := asyncTestEnv(t)
	gate := make(chan struct{})
	res, err := DispatchAsyncDelegationBatch(context.Background(), env, AsyncDispatch{
		Goals: []string{"alpha", "beta"}, Role: "leaf", MaxAsyncChildren: 2,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			<-gate
			return map[string]any{"results": []any{map[string]any{"task_index": 0, "status": "error"}, map[string]any{"task_index": 1, "status": "failed"}}, "total_duration_seconds": 1.0}, nil
		},
	})
	if err != nil || res["status"] != "dispatched" || AsyncDelegationActiveTaskCount() != 2 {
		t.Fatalf("batch dispatch = %v %v tasks %d", res, err, AsyncDelegationActiveTaskCount())
	}
	PushTaskFailureNotice(env, res["delegation_id"].(string), map[string]any{"task_index": 0, "status": "error", "error": "401"}, 2)
	notice := drainFor(res["delegation_id"], time.Second)
	if notice == nil || notice["task_failure_notice"] != true || !IsInterimDelegationEvent(notice) || notice["status"] != "running" {
		t.Fatalf("notice = %v", notice)
	}
	close(gate)
	evt := drainFor(res["delegation_id"], 5*time.Second)
	if evt == nil || evt["status"] != "error" || evt["goal"] != "2 parallel subagents: alpha; beta" {
		t.Fatalf("batch completion = %v", evt)
	}
	items := ListAsyncDelegations()
	if len(items) != 1 || items[0]["status"] != "error" {
		t.Fatalf("listing = %v", items)
	}
}

func TestAsyncRunnerErrorBecomesCrashResult(t *testing.T) {
	env := asyncTestEnv(t)
	res, _ := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "boom", Runner: func(context.Context, *Env) (map[string]any, error) {
		return nil, NewWorkerPersistenceError("kaput")
	}})
	evt := drainFor(res["delegation_id"], 5*time.Second)
	if evt == nil || evt["status"] != "error" || evt["error"] != "WorkerPersistenceError: kaput" || evt["api_calls"] != 0 {
		t.Fatalf("crash event = %v", evt)
	}
}

func TestChildrenActivityFromToken(t *testing.T) {
	got := childrenActivityFromToken([]any{[]any{3, "terminal", 90.0}, "odd"}, 100)
	if len(got) != 2 || asyncMap(got[0])["seconds_since_activity"] != 10.0 || got[1] != nil {
		t.Fatalf("activity = %v", got)
	}
	if childrenActivityFromToken(5, 0) != nil {
		t.Fatal("a non sequence token must give nil")
	}
	if txt := stalledErrorText(map[string]any{"goal": "  fix   the\nbug ", "_stall_quiet_seconds": 150.0}); txt != `Background task "fix the bug" stopped responding after 2 min of no progress and was cancelled. Nothing else was affected; ask me to run it again if you still need it.` {
		t.Fatalf("stall text = %q", txt)
	}
}

func TestAsyncDelegationExports(t *testing.T) {
	env := asyncTestEnv(t)
	BindSessionContext(env, map[string]string{"ICEBATH_SESSION_PLATFORM": "api_server", "ICEBATH_SESSION_CHAT_ID": "raw-sid"})
	if CurrentOriginSessionID(env) != "raw-sid" {
		t.Fatalf("origin session id = %q", CurrentOriginSessionID(env))
	}
	BindSessionContext(env, map[string]string{"ICEBATH_SESSION_PLATFORM": "telegram", "ICEBATH_SESSION_CHAT_ID": "chat"})
	if CurrentOriginSessionID(env) != "" {
		t.Fatal("a push platform chat id is no session id")
	}
	SetCurrentSessionKey(env, "sess-key")
	if CurrentSessionKeyOr(env, "dflt") != "sess-key" || CurrentSessionKeyOr(&Env{Values: &sync.Map{}}, "dflt") != "dflt" {
		t.Fatal("session key fallback")
	}
	env.Values.Store("session.async_delivery", false)
	if SessionAsyncDeliverySupported(env) {
		t.Fatal("async delivery flag ignored")
	}
	res := DispatchAsyncDelegation(env, AsyncDelegationRequest{Goal: "cron run", Context: "ctx", Role: "cron_run", SessionKey: "sess-key",
		Runner: func() *pyjson.Object {
			o := pyjson.NewObject()
			o.Set("status", "completed")
			o.Set("summary", "ran")
			return o
		}})
	evt := drainFor(res["delegation_id"], 5*time.Second)
	if res["status"] != "dispatched" || evt == nil || evt["summary"] != "ran" || evt["role"] != "cron_run" || evt["context"] != "ctx" {
		t.Fatalf("exported dispatch = %v event %v", res, evt)
	}
}

func TestPropagateContextToThreadCarriesVarsAndCallbacks(t *testing.T) {
	v := pyctx.NewVar("probe", "unset")
	ctx := pyctx.With(context.Background(), pyctx.New())
	v.Set(ctx, "parent")
	var slot any = "parent-callback"
	var installed, cleared any = nil, "never"
	RegisterThreadCallback(ThreadCallback{
		Get: func(context.Context) any { return slot },
		Set: func(c context.Context, cb any) {
			ThreadLocalSet(c, "probe_cb", cb)
			if cb == nil {
				cleared = ThreadLocalGet(c, "probe_cb")
			} else {
				installed = ThreadLocalGet(c, "probe_cb")
			}
		},
	})
	t.Cleanup(func() {
		threadCallbacksMu.Lock()
		threadCallbacks = threadCallbacks[:len(threadCallbacks)-1]
		threadCallbacksMu.Unlock()
	})
	env := &Env{Ask: asyncTestAsker{}}
	run := PropagateContextToThread(ctx, env, func(wctx context.Context, wenv *Env) (any, error) {
		v.Set(wctx, "worker")
		return []any{v.Get(wctx), wenv.Ask != nil}, nil
	})
	v.Set(ctx, "parent-later")
	done := make(chan []any)
	go func() {
		out, _ := run()
		done <- out.([]any)
	}()
	out := <-done
	if out[0] != "worker" || out[1] != true || v.Get(ctx) != "parent-later" {
		t.Fatalf("worker saw %v, parent now %v", out, v.Get(ctx))
	}
	if installed != "parent-callback" || cleared != nil || env.Ask == nil {
		t.Fatalf("installed %v cleared %v", installed, cleared)
	}
}

func TestWorkerDelegationLedgerBindingIsOwnerScoped(t *testing.T) {
	prev := IsWorkerProcess
	t.Cleanup(func() {
		IsWorkerProcess = prev
		workerDelegationStoreMu.Lock()
		workerDelegationStore = nil
		workerDelegationStoreMu.Unlock()
	})
	IsWorkerProcess = func() bool { return false }
	if l, err := workerLedger(); l != nil || err != nil || requireLedgerOwner() != nil {
		t.Fatal("the owner routes locally")
	}
	if err := BindWorkerDelegationStore(&fakeWorkerStore{pid: os.Getpid()}); err == nil || err.Error() != "permission_denied" {
		t.Fatalf("owner bind = %v", err)
	}
	IsWorkerProcess = func() bool { return true }
	if _, err := workerLedger(); err == nil || err.Error() != "worker_delegation_ledger_unbound" {
		t.Fatalf("unbound worker = %v", err)
	}
	if err := requireLedgerOwner(); err == nil || err.Error() != "worker_delegation_owner_only" {
		t.Fatalf("owner only = %v", err)
	}
	store := &fakeWorkerStore{pid: os.Getpid(), sid: "parent"}
	if err := BindWorkerDelegationStore(store); err != nil {
		t.Fatal(err)
	}
	if err := BindWorkerDelegationStore(&fakeWorkerStore{pid: os.Getpid()}); err == nil || err.Error() != "worker_delegation_already_bound" {
		t.Fatalf("second bind = %v", err)
	}
	l, _ := workerLedger()
	if _, err := l.Dispatch(map[string]any{"parent_session_id": "other"}); err == nil || err.Error() != "permission_denied" {
		t.Fatalf("foreign parent = %v", err)
	}
	if _, err := l.Dispatch(map[string]any{"parent_session_id": "parent", "scope_id": "s"}); err == nil || err.Error() != "unsupported_producer" {
		t.Fatalf("routed producer = %v", err)
	}
	if ok, err := ClaimCompletionDelivery(nil, "deleg_x", "c1"); !ok || err != nil || store.ops[len(store.ops)-1] != "delegation.claim" {
		t.Fatalf("worker claim = %v %v %v", ok, err, store.ops)
	}
	if _, err := asyncConnect(context.Background(), nil); err == nil || err.Error() != "worker_delegation_ledger_unavailable" {
		t.Fatalf("worker connect = %v", err)
	}
}

type fakeWorkerStore struct {
	pid int
	sid string
	ops []string
}

func (s *fakeWorkerStore) ScopePID() int          { return s.pid }
func (s *fakeWorkerStore) ScopeSessionID() string { return s.sid }
func (s *fakeWorkerStore) Apply(op string, payload map[string]any) (map[string]any, error) {
	s.ops = append(s.ops, op)
	return map[string]any{"value": true}, nil
}

func TestSafeScheduleThreadsafe(t *testing.T) {
	if f := SafeScheduleThreadsafe(context.Background(), nil, func(context.Context) (any, error) { return 1, nil }, nil, "", 0); f != nil {
		t.Fatal("a missing loop must return nil")
	}
	pool := NewDaemonPool(1, "t")
	f := SafeScheduleThreadsafe(context.Background(), pool, func(context.Context) (any, error) { return 7, nil }, nil, "", 0)
	if v, err := f.Result(time.Second); v != 7 || err != nil {
		t.Fatalf("scheduled = %v %v", v, err)
	}
	ConsumeDetachedTaskResult(f)
	pool.Shutdown(true, false)
	if SafeScheduleThreadsafe(context.Background(), pool, func(context.Context) (any, error) { return nil, nil }, nil, "", 0) != nil {
		t.Fatal("a shut down loop must refuse")
	}
}

func TestNewDelegationIDWrapperShape(t *testing.T) {
	id := NewDelegationID()
	if !strings.HasPrefix(id, "deleg_") || len(id) != len("deleg_")+8 {
		t.Fatalf("NewDelegationID = %q", id)
	}
}

// Durable dispatch record of test_delegated_output_redaction.py
func redactionTestRecord(t *testing.T, env *Env) map[string]any {
	t.Helper()
	record := map[string]any{
		"delegation_id": "synthetic-delegation", "dispatched_at": asyncTimeNow(), "session_key": "agent:example:dm:route",
		"origin_session_id": "origin-session", "parent_session_id": "parent-session", "goal": "retain the parent goal",
		"context": "retain the parent context", "model": "test-model",
	}
	if err := PersistDispatch(env, record); err != nil {
		t.Fatal(err)
	}
	return record
}

// test_delegated_output_redaction.py test_old_durable_results_are_redacted_on_read_replay_and_failure_roster
func TestOldDurableResultsAreRedactedOnReadReplayAndFailureRoster(t *testing.T) {
	env := asyncTestEnv(t)
	record := redactionTestRecord(t, env)
	event := map[string]any{}
	for k, v := range record {
		event[k] = v
	}
	event["type"], event["status"], event["summary"], event["error"] = "async_delegation", "failed", delegationSecret, delegationSecret
	eventJSON, _ := pyjson.Dumps(event)
	resultJSON, _ := pyjson.Dumps(map[string]any{"status": "failed", "summary": delegationSecret, "error": delegationSecret})
	err := AsyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		_, err := asyncExec(ctx, conn, "UPDATE async_delegations SET state='failed',completed_at=?,event_json=?,result_json=?", asyncTimeNow(), eventJSON, resultJSON)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := GetDurableDelegation(env, "synthetic-delegation")
	if err != nil || stored == nil {
		t.Fatalf("stored %v %v", stored, err)
	}
	if text, _ := pyjson.Dumps(stored["result"]); strings.Contains(text, delegationSecret) {
		t.Fatalf("stored result %s", text)
	}
	target := newEventQueue()
	if n, err := RestoreUndeliveredCompletions(env, target); n != 1 || err != nil {
		t.Fatalf("restored %d %v", n, err)
	}
	replay, _ := target.GetNowait()
	if replay["restored"] != true || replay["session_key"] != record["session_key"] {
		t.Fatalf("replay %v", replay)
	}
	if text, _ := pyjson.Dumps(replay); strings.Contains(text, delegationSecret) {
		t.Fatalf("replay %s", text)
	}
	failures, err := FailedDelegationsForSession(env, "", "parent-session", 20, nil)
	if err != nil || len(failures) != 1 || failures[0]["goal"] != record["goal"] {
		t.Fatalf("failures %v %v", failures, err)
	}
	if text, _ := pyjson.Dumps(failures); strings.Contains(text, delegationSecret) {
		t.Fatalf("failures %s", text)
	}
}

// The exported completion push keeps the opt out copy in process while the durable row stays forced
func TestPushCompletionEventExportHonoursOptOutOnlyInProcess(t *testing.T) {
	env := asyncTestEnv(t)
	env.Secrets = config.NewSecrets(env.Home)
	if err := env.Secrets.Set("ICEBATH_REDACT_SECRETS", "false"); err != nil {
		t.Fatal(err)
	}
	record := redactionTestRecord(t, env)
	PushCompletionEvent(env, record, map[string]any{"status": "completed", "summary": delegationSecret}, "completed")
	evt, ok := DefaultProcessRegistry().CompletionQueue.GetNowait()
	if !ok || evt["summary"] != delegationSecret {
		t.Fatalf("in process event %v", evt)
	}
	var eventJSON, resultJSON string
	err := AsyncTransaction(env, func(ctx context.Context, conn state.SQLiteConn) error {
		return conn.QueryRowContext(ctx, "SELECT event_json,result_json FROM async_delegations").Scan(&eventJSON, &resultJSON)
	})
	if err != nil || strings.Contains(eventJSON+resultJSON, delegationSecret) {
		t.Fatalf("durable row %s %s %v", eventJSON, resultJSON, err)
	}
}
