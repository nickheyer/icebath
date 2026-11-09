package tools

import (
	"context"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Home whose ledger holds the completion a real owner process left pending
func asyncOrphanHome(t *testing.T) (*Env, string) {
	t.Helper()
	h := t.TempDir()
	id := asyncRunChild(t, "orphan", h, "")
	if !strings.HasPrefix(id, "deleg_") {
		t.Fatalf("orphan child printed %q", id)
	}
	return &Env{Home: h, Values: &sync.Map{}}, id
}

func TestOrphanedTerminalCompletionIsOfferedOnceWhileTheProcessRuns(t *testing.T) {
	resetAsyncDelegationForTests()
	t.Cleanup(resetAsyncDelegationForTests)
	env, id := asyncOrphanHome(t)
	if asyncColumn(t, env, id, "state") != "error" || asyncColumn(t, env, id, "delivery_state") != "pending" || asyncText(asyncColumn(t, env, id, "event_json")) == "" {
		t.Fatal("owner left no pending error completion")
	}
	updated := asyncColumnFloat(t, env, id, "updated_at")
	q := newEventQueue()
	if n, _ := SweepOrphanedCompletions(env, q, ptr(updated+1)); n != 0 {
		t.Fatalf("fresh row swept %d", n)
	}
	later := updated + asyncOrphanStaleS + 1
	if n, _ := SweepOrphanedCompletions(env, q, ptr(later)); n != 1 {
		t.Fatalf("stale orphan not offered %d", n)
	}
	events := asyncDrainAll(q)
	if len(events) != 1 || events[0]["delegation_id"] != id || events[0]["status"] != "error" || events[0]["session_key"] != "bot-chat" || events[0]["restored"] != true {
		t.Fatalf("events %v", events)
	}
	if n, _ := SweepOrphanedCompletions(env, q, ptr(later+60)); n != 0 || q.Len() != 0 {
		t.Fatalf("offered row swept again %d", n)
	}
	claim, ok, err := ClaimEventDelivery(env, events[0], "first")
	if err != nil || !ok || claim == "" {
		t.Fatalf("claim %q %v %v", claim, ok, err)
	}
	if _, ok2, _ := ClaimEventDelivery(env, events[0], "second"); ok2 {
		t.Fatal("second consumer claimed the held delivery")
	}
	if ok, err := CompleteCompletionDelivery(env, id, claim); !ok || err != nil {
		t.Fatalf("complete %v %v", ok, err)
	}
	if asyncColumn(t, env, id, "delivery_state") != "delivered" {
		t.Fatal("row not delivered")
	}
}

func TestRowOwnedByALiveProcessIsNeverSwept(t *testing.T) {
	env := asyncTestEnv(t)
	handle, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "mine", Role: "leaf", Model: ptr("m"), SessionKey: "live",
		Runner: runnerOf(map[string]any{"status": "completed", "summary": "done"})})
	if err != nil {
		t.Fatal(err)
	}
	asyncWaitIdle(t)
	id := asyncText(handle["delegation_id"])
	pid, _ := asyncInt(asyncColumn(t, env, id, "owner_pid"))
	if pid != os.Getpid() || asyncColumn(t, env, id, "delivery_state") != "pending" {
		t.Fatalf("row pid %d state %v", pid, asyncColumn(t, env, id, "delivery_state"))
	}
	q := newEventQueue()
	if n, _ := SweepOrphanedCompletions(env, q, ptr(asyncColumnFloat(t, env, id, "updated_at")+3600)); n != 0 || q.Len() != 0 {
		t.Fatalf("live owner row swept %d", n)
	}
}

func TestLiveOwnerRowWhoseQueueCopyWasLostIsReOfferedAfterALease(t *testing.T) {
	env := asyncTestEnv(t)
	registry := DefaultProcessRegistry()
	shared := registry.CompletionQueue
	lost, q := newEventQueue(), newEventQueue()
	registry.CompletionQueue = lost
	t.Cleanup(func() { registry.CompletionQueue = shared })
	handle, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "mine", Role: "leaf", Model: ptr("m"), SessionKey: "live",
		Runner: runnerOf(map[string]any{"status": "completed", "summary": "done"})})
	if err != nil {
		t.Fatal(err)
	}
	asyncWaitIdle(t)
	id := asyncText(handle["delegation_id"])
	pid, _ := asyncInt(asyncColumn(t, env, id, "owner_pid"))
	if pid != os.Getpid() || asyncColumn(t, env, id, "delivery_state") != "pending" {
		t.Fatal("row not owned and pending")
	}
	registry.CompletionQueue = q
	updated := asyncColumnFloat(t, env, id, "updated_at")
	lease := updated + asyncClaimLeaseS + 1
	if n, _ := SweepOrphanedCompletions(env, q, ptr(updated+asyncOrphanStaleS+1)); n != 0 {
		t.Fatalf("queued copy re-offered %d", n)
	}
	held := TakeReady(lost, 0)
	if n, _ := SweepOrphanedCompletions(env, q, ptr(lease)); n != 0 {
		t.Fatalf("held copy re-offered %d", n)
	}
	ReleaseEvents(held)
	logs := asyncCaptureLogs(t)
	if n, _ := SweepOrphanedCompletions(env, q, ptr(lease)); n != 1 {
		t.Fatalf("lost copy not re-offered %d", n)
	}
	if n, _ := SweepOrphanedCompletions(env, q, ptr(lease+60)); n != 0 {
		t.Fatalf("re-offered copy offered again %d", n)
	}
	events := asyncDrainAll(q)
	if len(events) != 1 {
		t.Fatalf("events %v", events)
	}
	ReturnCompletionOffer(events[0])
	if n, _ := SweepOrphanedCompletions(env, q, ptr(lease+90)); n != 0 {
		t.Fatalf("offer re-offered inside its own lease %d", n)
	}
	if n, _ := SweepOrphanedCompletions(env, q, ptr(lease+asyncClaimLeaseS+1)); n != 1 {
		t.Fatalf("handed back copy not re-offered %d", n)
	}
	var levels []slog.Level
	for _, rec := range logs.snapshot() {
		if strings.Contains(rec.Message, "Re-offering") {
			levels = append(levels, rec.Level)
		}
	}
	if len(levels) != 2 || levels[0] != slog.LevelWarn || levels[1] != slog.LevelDebug {
		t.Fatalf("re-offer log levels %v", levels)
	}
	events = asyncDrainAll(q)
	if len(events) != 1 || events[0]["delegation_id"] != id || events[0]["summary"] != "done" {
		t.Fatalf("events %v", events)
	}
	claim, ok, _ := ClaimEventDelivery(env, events[0], "first")
	if !ok || claim == "" {
		t.Fatal("claim refused")
	}
	if _, ok2, _ := ClaimEventDelivery(env, events[0], "second"); ok2 {
		t.Fatal("second claim admitted")
	}
	if ok, err := CompleteCompletionDelivery(env, id, claim); !ok || err != nil {
		t.Fatalf("complete %v %v", ok, err)
	}
	other, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "theirs", Role: "leaf", Model: ptr("m"), SessionKey: "live",
		Runner: runnerOf(map[string]any{"status": "completed", "summary": "done"})})
	if err != nil {
		t.Fatal(err)
	}
	asyncWaitIdle(t)
	asyncDrainAll(q)
	asyncSetColumns(t, env, asyncText(other["delegation_id"]), map[string]any{"owner_pid": os.Getppid(), "owner_started_at": nil})
	if n, _ := SweepOrphanedCompletions(env, q, ptr(asyncTimeNow()+3600)); n != 0 || q.Len() != 0 {
		t.Fatalf("another live process's row re-offered %d", n)
	}
	if asyncColumn(t, env, id, "delivery_state") != "delivered" {
		t.Fatal("row not delivered")
	}
}

func TestOfferedCopyLostWithoutHandBackIsReOfferedAfterALease(t *testing.T) {
	resetAsyncDelegationForTests()
	t.Cleanup(resetAsyncDelegationForTests)
	env, id := asyncOrphanHome(t)
	updated := asyncColumnFloat(t, env, id, "updated_at")
	q := newEventQueue()
	if n, _ := RestoreUndeliveredCompletions(env, q); n != 1 {
		t.Fatalf("restore %d", n)
	}
	asyncDrainAll(q)
	if n, _ := SweepOrphanedCompletions(env, q, ptr(updated+asyncOrphanStaleS+1)); n != 0 {
		t.Fatalf("discarded copy re-offered before its lease %d", n)
	}
	if n, _ := SweepOrphanedCompletions(env, q, ptr(updated+asyncClaimLeaseS+1)); n != 1 {
		t.Fatalf("discarded copy not re-offered after its lease %d", n)
	}
	if ids := asyncIDs(asyncDrainAll(q)); len(ids) != 1 || ids[0] != id {
		t.Fatalf("ids %v", ids)
	}
}

func TestSweepIsBoundToTheProfileHomeItRunsUnder(t *testing.T) {
	resetAsyncDelegationForTests()
	t.Cleanup(resetAsyncDelegationForTests)
	envA, idA := asyncOrphanHome(t)
	envB, idB := asyncOrphanHome(t)
	later := math.Max(asyncColumnFloat(t, envA, idA, "updated_at"), asyncColumnFloat(t, envB, idB, "updated_at")) + asyncOrphanStaleS + 1
	q := newEventQueue()
	SweepOrphanedCompletions(envA, q, ptr(later))
	if ids := asyncIDs(asyncDrainAll(q)); len(ids) != 1 || ids[0] != idA {
		t.Fatalf("home a offered %v", ids)
	}
	SweepOrphanedCompletions(envB, q, ptr(later))
	if ids := asyncIDs(asyncDrainAll(q)); len(ids) != 1 || ids[0] != idB {
		t.Fatalf("home b offered %v", ids)
	}
	if n, _ := SweepOrphanedCompletions(envA, q, ptr(later+60)); n != 0 || q.Len() != 0 {
		t.Fatalf("home a offered again %d", n)
	}
	if asyncColumn(t, envB, idB, "delivery_state") != "pending" {
		t.Fatal("home b row changed")
	}
}

func TestRestartReplayAndSweepNeverOfferTheSameRowTwice(t *testing.T) {
	resetAsyncDelegationForTests()
	t.Cleanup(resetAsyncDelegationForTests)
	env, id := asyncOrphanHome(t)
	later := asyncColumnFloat(t, env, id, "updated_at") + asyncOrphanStaleS + 1
	q := newEventQueue()
	if n, _ := RestoreUndeliveredCompletions(env, q); n != 1 {
		t.Fatalf("restore %d", n)
	}
	if n, _ := SweepOrphanedCompletions(env, q, ptr(later)); n != 0 {
		t.Fatalf("sweep offered the replayed row %d", n)
	}
	if ids := asyncIDs(asyncDrainAll(q)); len(ids) != 1 || ids[0] != id {
		t.Fatalf("ids %v", ids)
	}
}

func TestOrphanPastItsDeliveryBudgetConvergesToDropped(t *testing.T) {
	for _, exhausted := range []string{"attempts", "age"} {
		resetAsyncDelegationForTests()
		env, id := asyncOrphanHome(t)
		later := asyncColumnFloat(t, env, id, "updated_at") + asyncOrphanStaleS + 1
		if exhausted == "attempts" {
			asyncSetColumns(t, env, id, map[string]any{"delivery_attempts": asyncMaxDeliveryAttempts})
		} else {
			later = asyncColumnFloat(t, env, id, "completed_at") + asyncMaxCompletionReplayAgeS + 1
		}
		q := newEventQueue()
		if n, _ := SweepOrphanedCompletions(env, q, ptr(later)); n != 0 || q.Len() != 0 {
			t.Fatalf("%s: exhausted orphan offered %d", exhausted, n)
		}
		if asyncColumn(t, env, id, "delivery_state") != "dropped" {
			t.Fatalf("%s: row not dropped", exhausted)
		}
	}
	resetAsyncDelegationForTests()
}

func TestInFlightClaimIsLeftToItsHolder(t *testing.T) {
	resetAsyncDelegationForTests()
	t.Cleanup(resetAsyncDelegationForTests)
	env, id := asyncOrphanHome(t)
	later := asyncColumnFloat(t, env, id, "updated_at") + asyncOrphanStaleS + 1
	asyncSetColumns(t, env, id, map[string]any{"delivery_claim": "other-process", "delivery_claimed_at": later - 1})
	q := newEventQueue()
	if n, _ := SweepOrphanedCompletions(env, q, ptr(later)); n != 0 || q.Len() != 0 {
		t.Fatalf("claimed row swept %d", n)
	}
}

func TestThrottleRunsAtMostOneSweepPerHomePerInterval(t *testing.T) {
	asyncTestEnv(t)
	var calls []string
	prev := asyncSweepOrphanedFn
	asyncSweepOrphanedFn = func(env *Env, _ *EventQueue, _ *float64) (int, error) {
		calls = append(calls, env.Home)
		return 0, nil
	}
	t.Cleanup(func() { asyncSweepOrphanedFn = prev })
	a, b := &Env{Home: t.TempDir()}, &Env{Home: t.TempDir()}
	q := newEventQueue()
	MaybeSweepOrphanedCompletions(a, q, ptr(100.0))
	MaybeSweepOrphanedCompletions(a, q, ptr(101.0))
	MaybeSweepOrphanedCompletions(b, q, ptr(101.0))
	MaybeSweepOrphanedCompletions(a, q, ptr(100.0+OrphanSweepIntervalS+1))
	if strings.Join(calls, ",") != strings.Join([]string{a.Home, b.Home, a.Home}, ",") {
		t.Fatalf("sweeps %v", calls)
	}
}

// Batch record of the failure notice tests
func asyncNoticeRecord(status string) map[string]any {
	return map[string]any{"delegation_id": "deleg_x", "status": status, "is_batch": true, "goals": []any{"a", "b", "c"}, "goal": "a",
		"session_key": "sk", "origin_ui_session_id": "ui", "origin_session_id": "", "parent_session_id": "root",
		"dispatched_at": 1.0, "role": "leaf", "model": "m", "context": nil, "toolsets": nil}
}

func TestFailureNoticeReachesTheQueueWhileTheBatchKeepsRunningAndFormatsAsEarlyWarning(t *testing.T) {
	env := asyncTestEnv(t)
	asyncRecordsMu.Lock()
	asyncRecords["deleg_x"] = &asyncRecord{m: asyncNoticeRecord("running")}
	asyncRecordsMu.Unlock()
	entry := map[string]any{"task_index": 1, "status": "error", "error": "401 authentication_error: key invalid", "duration_seconds": 12.5,
		"live_transcript": "/tmp/live/task-1.log"}
	PushTaskFailureNotice(env, "deleg_x", entry, 3)
	asyncRecordsMu.Lock()
	status := asyncRecords["deleg_x"].status()
	asyncRecordsMu.Unlock()
	if status != "running" {
		t.Fatalf("notice finalized the batch: %s", status)
	}
	evt, ok := DefaultProcessRegistry().CompletionQueue.GetNowait()
	if !ok || evt["type"] != "async_delegation" || evt["task_failure_notice"] != true {
		t.Fatalf("event %v", evt)
	}
	if evt["session_key"] != "sk" || evt["origin_ui_session_id"] != "ui" || evt["parent_session_id"] != "root" {
		t.Fatalf("routing %v", evt)
	}
	text := FormatProcessNotification(evt)
	if !strings.HasPrefix(text, "[ASYNC DELEGATION TASK FAILED — deleg_x, task 2/3]") {
		t.Fatalf("header %q", text)
	}
	for _, needle := range []string{"Task: b", "401 authentication_error", "/tmp/live/task-1.log", "consolidated results will still arrive"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("missing %q in %q", needle, text)
		}
	}
}

func TestInterimNoticeNeverClaimsOrAcknowledgesTheBatchFinalRow(t *testing.T) {
	env := asyncTestEnv(t)
	notice := map[string]any{"type": "async_delegation", "delegation_id": "deleg_x", "task_failure_notice": true, "results": []any{map[string]any{"task_index": 0}}}
	final := map[string]any{"type": "async_delegation", "delegation_id": "deleg_x", "is_batch": true, "results": []any{}}
	if claim, _, err := ClaimEventDelivery(env, notice, "tui-poller"); claim != "" || err != nil {
		t.Fatalf("interim claim %q %v", claim, err)
	}
	if err := CompleteEventDelivery(env, notice, ""); err != nil {
		t.Fatal(err)
	}
	if !IsInterimDelegationEvent(notice) || IsInterimDelegationEvent(final) {
		t.Fatal("interim classification")
	}
}

// Dispatch and terminal completion written through the module's own persistence
func asyncFailedDelegation(t *testing.T, env *Env, delegationID, status string, result map[string]any, ui, parent string, completedAt float64, task map[string]any) {
	t.Helper()
	record := map[string]any{"delegation_id": delegationID, "session_key": "", "origin_ui_session_id": ui, "parent_session_id": parent, "dispatched_at": asyncTimeNow() - 60}
	for k, v := range task {
		record[k] = v
	}
	if err := PersistDispatch(env, record); err != nil {
		t.Fatal(err)
	}
	if completedAt == 0 {
		completedAt = asyncTimeNow()
	}
	if err := persistCompletion(env, map[string]any{"delegation_id": delegationID, "status": status, "completed_at": completedAt}, result); err != nil {
		t.Fatal(err)
	}
}

func TestAFailedSingleDelegationIsListedForItsSession(t *testing.T) {
	env := asyncTestEnv(t)
	asyncFailedDelegation(t, env, "d-fail", "error", map[string]any{"status": "error", "error": "interrupted: waiting for model response"}, "ui-1", "agent-1", 0, map[string]any{"goal": "audit billing"})
	asyncFailedDelegation(t, env, "d-ok", "completed", map[string]any{"status": "completed", "summary": "done"}, "ui-1", "agent-1", 0, map[string]any{"goal": "write docs"})
	rows, err := FailedDelegationsForSession(env, "ui-1", "", 20, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v %v", rows, err)
	}
	row := rows[0]
	dispatched, _ := asyncFloat(row["dispatched_at"])
	completed, _ := asyncFloat(row["completed_at"])
	if row["delegation_id"] != "d-fail" || asyncText(row["task_index"]) != "0" || row["status"] != "error" || row["goal"] != "audit billing" ||
		row["error"] != "interrupted: waiting for model response" || math.Abs(dispatched-(asyncTimeNow()-60)) > 30 || math.Abs(completed-asyncTimeNow()) > 30 {
		t.Fatalf("row %v", row)
	}
}

func TestABatchThatCompletedStillSurfacesItsFailedTask(t *testing.T) {
	env := asyncTestEnv(t)
	asyncFailedDelegation(t, env, "d-batch", "completed", map[string]any{"results": []any{
		map[string]any{"task_index": 3, "status": "completed", "summary": "ok"},
		map[string]any{"task_index": 4, "status": "timeout", "error": "no progress"}}}, "ui-1", "agent-1", 0,
		map[string]any{"is_batch": true, "goals": []any{"scan api", "scan web"}, "task_indexes": []any{3, 4}})
	rows, err := FailedDelegationsForSession(env, "ui-1", "", 20, nil)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v %v", rows, err)
	}
	row := rows[0]
	if asyncText(row["task_index"]) != "4" || row["goal"] != "scan web" || row["status"] != "timeout" || row["error"] != "no progress" {
		t.Fatalf("row %v", row)
	}
}

func TestTheDurableParentIdClaimsRowsAfterAReloadRemintsTheUiSession(t *testing.T) {
	env := asyncTestEnv(t)
	asyncFailedDelegation(t, env, "d-fail", "stalled", map[string]any{"status": "error", "error": "stalled"}, "ui-1", "agent-1", 0, map[string]any{"goal": "refactor"})
	rows, err := FailedDelegationsForSession(env, "ui-reminted", "agent-1", 20, nil)
	if err != nil || len(rows) != 1 || rows[0]["delegation_id"] != "d-fail" {
		t.Fatalf("rows %v %v", rows, err)
	}
}

func TestOtherSessionsAndOldFailuresStayOut(t *testing.T) {
	env := asyncTestEnv(t)
	asyncFailedDelegation(t, env, "d-other", "error", map[string]any{"status": "error"}, "ui-2", "agent-2", 0, map[string]any{"goal": "x"})
	asyncFailedDelegation(t, env, "d-old", "error", map[string]any{"status": "error"}, "ui-1", "agent-1", asyncTimeNow()-3*86400, map[string]any{"goal": "y"})
	if rows, err := FailedDelegationsForSession(env, "ui-1", "agent-1", 20, nil); err != nil || len(rows) != 0 {
		t.Fatalf("rows %v %v", rows, err)
	}
	if rows, err := FailedDelegationsForSession(env, "", "", 20, nil); err != nil || len(rows) != 0 {
		t.Fatalf("unscoped rows %v %v", rows, err)
	}
}

// Completion persisted and, when asked, claimed and delivered
func asyncPersistedCompletion(t *testing.T, env *Env, delegationID string, delivered bool) {
	t.Helper()
	event := map[string]any{"type": "async_delegation", "delegation_id": delegationID, "session_key": "agent:main:telegram:dm:1",
		"parent_session_id": "parent", "dispatched_at": 1.0, "status": "completed", "summary": "done"}
	if err := PersistDispatch(env, event); err != nil {
		t.Fatal(err)
	}
	if err := persistCompletion(env, event, map[string]any{"status": "completed", "summary": "done"}); err != nil {
		t.Fatal(err)
	}
	if delivered {
		if ok, err := ClaimCompletionDelivery(env, delegationID, "claim"); !ok || err != nil {
			t.Fatalf("claim %v %v", ok, err)
		}
		if ok, err := CompleteCompletionDelivery(env, delegationID, "claim"); !ok || err != nil {
			t.Fatalf("complete %v %v", ok, err)
		}
	}
}

func TestRequeueReturnsAnAcknowledgedCompletionToPendingAndOffersIt(t *testing.T) {
	env := asyncTestEnv(t)
	asyncPersistedCompletion(t, env, "requeue-ok", true)
	offered := newEventQueue()
	if ok, err := RequeueAdmittedCompletion(env, "requeue-ok", offered); !ok || err != nil {
		t.Fatalf("requeue %v %v", ok, err)
	}
	if asyncColumn(t, env, "requeue-ok", "delivery_state") != "pending" || asyncColumn(t, env, "requeue-ok", "delivered_at") != nil {
		t.Fatal("row not returned to pending")
	}
	evt, ok := offered.GetNowait()
	if !ok || evt["delegation_id"] != "requeue-ok" || evt["restored"] != true {
		t.Fatalf("offered %v", evt)
	}
	asyncOrphanMu.Lock()
	seen := asyncOffered[[2]string{home.Key(env.Home), "requeue-ok"}]
	asyncOrphanMu.Unlock()
	if !seen {
		t.Fatal("offer not recorded")
	}
}

func TestRequeuePastTheDeliveryBudgetDropsTheCompletion(t *testing.T) {
	env := asyncTestEnv(t)
	asyncPersistedCompletion(t, env, "requeue-spent", true)
	asyncSetColumns(t, env, "requeue-spent", map[string]any{"delivery_attempts": asyncMaxDeliveryAttempts})
	offered := newEventQueue()
	if ok, _ := RequeueAdmittedCompletion(env, "requeue-spent", offered); ok || offered.Len() != 0 {
		t.Fatal("spent completion requeued")
	}
	if asyncColumn(t, env, "requeue-spent", "delivery_state") != "dropped" {
		t.Fatal("row not dropped")
	}
}

func TestRequeueLeavesRowsItCannotOffer(t *testing.T) {
	env := asyncTestEnv(t)
	for _, c := range []string{"not_delivered", "no_stored_event"} {
		asyncPersistedCompletion(t, env, c, c != "not_delivered")
		if c == "no_stored_event" {
			asyncSetColumns(t, env, c, map[string]any{"event_json": nil})
		}
		before := asyncColumn(t, env, c, "delivery_state")
		offered := newEventQueue()
		if ok, _ := RequeueAdmittedCompletion(env, c, offered); ok || offered.Len() != 0 {
			t.Fatalf("%s: requeued", c)
		}
		if asyncColumn(t, env, c, "delivery_state") != before {
			t.Fatalf("%s: delivery state changed", c)
		}
	}
}

// Worker store recording every ledger operation
type recordingWorkerStore struct {
	mu    sync.Mutex
	calls [][2]any
}

func (s *recordingWorkerStore) ScopePID() int          { return os.Getpid() }
func (s *recordingWorkerStore) ScopeSessionID() string { return "parent" }
func (s *recordingWorkerStore) Apply(op string, payload map[string]any) (map[string]any, error) {
	s.mu.Lock()
	s.calls = append(s.calls, [2]any{op, payload})
	s.mu.Unlock()
	if op == "delegation.read" {
		return map[string]any{"value": map[string]any{"delegation_id": payload["delegation_id"], "result": map[string]any{"summary": "done"}}}, nil
	}
	return map[string]any{"value": true}, nil
}

func installWorkerLedger(t *testing.T, store WorkerDelegationStore) {
	t.Helper()
	prev := IsWorkerProcess
	IsWorkerProcess = func() bool { return true }
	workerDelegationStoreMu.Lock()
	workerDelegationStore = nil
	workerDelegationStoreMu.Unlock()
	t.Cleanup(func() {
		IsWorkerProcess = prev
		workerDelegationStoreMu.Lock()
		workerDelegationStore = nil
		workerDelegationStoreMu.Unlock()
	})
	if store != nil {
		if err := BindWorkerDelegationStore(store); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkerLedgerPathsAreClosedAndProcessScoped(t *testing.T) {
	asyncTestEnv(t)
	store := &recordingWorkerStore{}
	installWorkerLedger(t, store)
	record := map[string]any{"delegation_id": "deleg_owned", "parent_session_id": "parent", "goal": "inert", "dispatched_at": asyncTimeNow(),
		"session_key": "", "origin_session_id": "", "origin_ui_session_id": ""}
	event := map[string]any{}
	for k, v := range record {
		event[k] = v
	}
	event["type"], event["status"], event["completed_at"] = "async_delegation", "completed", asyncTimeNow()
	if err := persistDispatch(nil, record); err != nil {
		t.Fatal(err)
	}
	if err := RecordUnitChild(nil, "deleg_owned", map[string]any{"task_index": 0, "summary": "partial"}); err != nil {
		t.Fatal(err)
	}
	if err := persistCompletion(nil, event, map[string]any{"summary": "done"}); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func(*Env, string, string) (bool, error){"claim": ClaimCompletionDelivery, "defer": DeferCompletionDelivery,
		"release": ReleaseCompletionDelivery, "drop": DropCompletionDelivery, "complete": CompleteCompletionDelivery} {
		if ok, err := fn(nil, "deleg_owned", "token"); !ok || err != nil {
			t.Fatalf("%s %v %v", name, ok, err)
		}
	}
	if row, err := GetDurableDelegation(nil, "deleg_owned"); err != nil || row == nil {
		t.Fatalf("read %v %v", row, err)
	}
	if n, err := RestoreUndeliveredCompletions(nil, newEventQueue()); n != 0 || err != nil {
		t.Fatalf("restore %d %v", n, err)
	}
	if _, err := MarkCompletionDelivered(nil, "deleg_owned"); err == nil || !strings.Contains(err.Error(), "owner_only") {
		t.Fatalf("mark delivered %v", err)
	}
	if _, err := RecoverAbandonedDelegations(nil); err == nil || !strings.Contains(err.Error(), "owner_only") {
		t.Fatalf("recover %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	ops := map[string]bool{}
	for _, c := range store.calls {
		ops[c[0].(string)] = true
	}
	for _, name := range []string{"dispatch", "child", "complete", "claim", "defer", "release", "drop", "ack", "read"} {
		if !ops["delegation."+name] {
			t.Fatalf("store never saw delegation.%s: %v", name, ops)
		}
	}
	if len(ops) != 9 {
		t.Fatalf("unexpected store operations %v", ops)
	}
	task, _ := store.calls[0][1].(map[string]any)["task"].(map[string]any)
	if len(task) != 1 || task["goal"] != "inert" {
		t.Fatalf("dispatch task %v", task)
	}
}

func TestUnboundWorkerRefusesBeforePersistenceAndDispatchState(t *testing.T) {
	env := asyncTestEnv(t)
	installWorkerLedger(t, nil)
	_, err := DispatchAsyncDelegationUnit(context.Background(), env, AsyncDispatch{Goal: "inert", Role: "leaf", ParentSessionID: ptr("parent"),
		Runner: runnerOf(map[string]any{"summary": "not run"})})
	if err == nil || !strings.Contains(err.Error(), "worker_delegation_ledger_unbound") {
		t.Fatalf("dispatch error %v", err)
	}
	if AsyncDelegationActiveCount() != 0 {
		t.Fatal("refused dispatch left a record")
	}
}

type asyncTurnStateKey struct{}

func TestCompletedRecordReleasesDispatchContext(t *testing.T) {
	env := asyncTestEnv(t)
	ctx := context.WithValue(context.Background(), asyncTurnStateKey{}, "turn-state")
	handle, err := DispatchAsyncDelegationUnit(ctx, env, AsyncDispatch{Goal: "retention", Role: "leaf", SessionKey: "retention-test",
		Runner: runnerOf(map[string]any{"summary": "done"})})
	if err != nil || handle["status"] != "dispatched" {
		t.Fatalf("dispatch %v %v", handle, err)
	}
	asyncWaitIdle(t)
	drainCompletionQueue()
	asyncRecordsMu.Lock()
	defer asyncRecordsMu.Unlock()
	rec := asyncRecords[asyncText(handle["delegation_id"])]
	if rec == nil {
		t.Fatal("record forgotten")
	}
	if rec.ctx != nil || rec.env != nil {
		t.Fatal("finished record still holds its dispatch context")
	}
}

func TestStaleMonitorForceFinalizeUpdatesOriginProfileLedger(t *testing.T) {
	launchHome := t.TempDir()
	profileHome := launchHome + "/profiles/secondary"
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launchHome)
	resetAsyncDelegationForTests()
	drainCompletionQueue()
	prevEnsure, prevInterval, prevGrace := asyncEnsureStaleMonitor, asyncStaleCheckInterval, asyncStallGraceSeconds
	asyncEnsureStaleMonitor = func() {}
	asyncStaleCheckInterval, asyncStallGraceSeconds = 10*time.Millisecond, 0.0
	t.Cleanup(func() {
		asyncEnsureStaleMonitor, asyncStaleCheckInterval, asyncStallGraceSeconds = prevEnsure, prevInterval, prevGrace
		resetAsyncDelegationForTests()
		drainCompletionQueue()
	})
	gate := make(chan struct{})
	defer asyncFinishBlocked(t, gate)
	profileEnv := &Env{Home: profileHome, Values: &sync.Map{}}
	handle, err := DispatchAsyncDelegationUnit(context.Background(), profileEnv, AsyncDispatch{Goal: "profile-owned stalled task", Role: "leaf", Model: ptr("test-model"),
		SessionKey: "secondary-session", MaxAsyncChildren: 1,
		Runner: func(context.Context, *Env) (map[string]any, error) {
			select {
			case <-gate:
			case <-time.After(30 * time.Second):
			}
			return map[string]any{"status": "completed", "summary": "late runner result"}, nil
		},
		ProgressFn: func() (any, bool, error) { return "frozen", false, nil }})
	if err != nil {
		t.Fatal(err)
	}
	id := asyncText(handle["delegation_id"])
	if asyncColumn(t, profileEnv, id, "state") != "running" {
		t.Fatal("profile ledger lacks the running row")
	}
	asyncRecordsMu.Lock()
	rec := asyncRecords[id]
	rec.m["status"] = "stalling"
	rec.m["_started"] = true
	rec.m["_interrupted_at"] = asyncTimeNow() - 1
	rec.m["_stall_quiet_seconds"] = 1.0
	rec.m["_stall_threshold_seconds"] = 0.5
	rec.m["_stall_in_tool"] = false
	asyncRecordsMu.Unlock()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		staleMonitorLoop(stop)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(stop)
		t.Fatal("monitor did not finish")
	}
	if asyncColumn(t, profileEnv, id, "state") != "stalled" {
		t.Fatal("profile ledger not force finalized")
	}
	if _, err := os.Stat(launchHome + "/state.db"); !os.IsNotExist(err) {
		t.Fatal("launch profile ledger was written")
	}
}
