package gateway

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Replaces the execution of claimed admissions for one test
func replaceExecution(t *testing.T, execute func(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object) (any, error)) {
	t.Helper()
	saved := executeFiniteAdmission
	executeFiniteAdmission = execute
	t.Cleanup(func() { executeFiniteAdmission = saved })
}

// Waits for the running drain of the test session to end
func waitDrain(t *testing.T, a *SessionAuthority, timeout time.Duration) {
	t.Helper()
	a.mu.Lock()
	done := a.Sessions["s"].drain
	a.mu.Unlock()
	if done == nil {
		t.Fatal("no drain was started")
	}
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("the drain did not end")
	}
}

// Admission of the test session by id
func admissionByID(t *testing.T, ctx context.Context, db *state.SessionDB, admissionID string) *pyjson.Object {
	t.Helper()
	rows, err := db.ListSessionAdmissions(ctx, "s", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if rowString(row, "admission_id") == admissionID {
			return row
		}
	}
	t.Fatalf("admission %s is not listed", admissionID)
	return nil
}

func TestSettlementFailureIsLoggedAndDoesNotKillTheDrain(t *testing.T) {
	ctx, db, a := newTestAuthority(t, "")
	logs := captureLogs(t)
	replaceExecution(t, func(ctx context.Context, a *SessionAuthority, _ SessionRef, _ *pyjson.Object) (any, error) {
		return "done", a.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
			_, err := conn.ExecContext(ctx, "UPDATE sessions SET runtime_generation=runtime_generation+1 WHERE id='s'")
			return err
		})
	})
	head, err := submitText(ctx, a, "moved-under-us")
	if err != nil {
		t.Fatal(err)
	}
	waitDrain(t, a, 5*time.Second)
	text := logs.String()
	if strings.Contains(text, "Session drain task died") {
		t.Fatalf("a settlement failure must not escape the drain task: %s", text)
	}
	if !strings.Contains(text, head.AdmissionID) {
		t.Fatalf("the failure is not logged with its admission: %s", text)
	}
	if row := admissionByID(t, ctx, db, head.AdmissionID); rowString(row, "status") != "unknown" {
		t.Fatalf("explicit recovery is possible without inventing a terminal result: %v", row)
	}
	live, _ := a.live("s")
	live.EventStream.Lock.Lock()
	stamped := live.EventStream.Execution.Len()
	live.EventStream.Lock.Unlock()
	if stamped != 0 {
		t.Fatal("stamp cleared even on failure")
	}
}

func TestCancelCommittedBeforeAFailedMediaCleanupStillSettlesObserversOnce(t *testing.T) {
	ctx, _, a := newTestAuthority(t, "")
	scheduled := 0
	saved := scheduleSession
	scheduleSession = func(context.Context, *SessionAuthority, SessionRef) error {
		scheduled++
		return nil
	}
	t.Cleanup(func() { scheduleSession = saved })
	var frames []*pyjson.Object
	receipt, err := submitText(ctx, a, "cancel-then-cleanup-fails")
	if err != nil {
		t.Fatal(err)
	}
	scheduled = 0
	waiter := newAdmissionWaiter()
	a.nativeWaiters[receipt.AdmissionID] = true
	a.waiters[receipt.AdmissionID] = waiter
	a.Sessions["s"].EventStream.Observers[&SessionEventObserver{Observe: func(frame *pyjson.Object) { frames = append(frames, frame) }}] = true
	collect := releaseAdmissionMedia
	t.Cleanup(func() { releaseAdmissionMedia = collect })
	releaseAdmissionMedia = func(context.Context, *state.SessionDB, string) error {
		releaseAdmissionMedia = collect
		return &os.PathError{Op: "write", Path: "media", Err: errors.New("media cleanup write failed")}
	}
	if _, err := a.CancelQueued(ctx, testActor, testRef, receipt.AdmissionID); err == nil || !strings.Contains(err.Error(), "media cleanup") {
		t.Fatalf("the failed cleanup did not surface: %v", err)
	}
	retried, err := a.CancelQueued(ctx, testActor, testRef, receipt.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CancelQueued(ctx, testActor, testRef, receipt.AdmissionID); err != nil {
		t.Fatal(err)
	}
	if retried.Status != "terminal" || retried.Outcome == nil || *retried.Outcome != "cancelled" {
		t.Fatalf("retried receipt %+v", retried)
	}
	if !waiter.isDone() || a.nativeWaiters[receipt.AdmissionID] {
		t.Fatal("the waiter stayed parked")
	}
	var outcomes []any
	for _, frame := range frames {
		params, _ := frame.Get("params")
		if kind, _ := params.(*pyjson.Object).Get("type"); kind == "message.complete" {
			payload, _ := params.(*pyjson.Object).Get("payload")
			outcome, _ := payload.(*pyjson.Object).Get("outcome")
			outcomes = append(outcomes, outcome)
		}
	}
	if len(outcomes) != 1 || outcomes[0] != "cancelled" {
		t.Fatalf("completions %v", outcomes)
	}
	if scheduled != 1 {
		t.Fatalf("the successors of a paused head need exactly one fresh drain, got %d", scheduled)
	}
}
