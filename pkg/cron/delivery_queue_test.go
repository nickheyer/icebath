package cron

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

type recordedSend struct {
	job        *Job
	content    string
	forFailure bool
}

func pointDeliveryDB(t *testing.T) (*Store, *DeliveryQueue, context.Context) {
	t.Helper()
	s, _ := newTestStore(t)
	deliveryDBOverride = filepath.Join(t.TempDir(), "deliveries.db")
	t.Cleanup(func() { deliveryDBOverride = "" })
	return s, openDeliveryQueue(s), context.Background()
}

func recorder(sends *[]recordedSend, refusal *string) DeliverySend {
	return func(job *Job, content string, forFailure bool) (*string, error) {
		*sends = append(*sends, recordedSend{job, content, forFailure})
		return refusal, nil
	}
}

func mustEnqueue(t *testing.T, q *DeliveryQueue, ctx context.Context, id string, job *Job, content string, forFailure bool) map[string]any {
	t.Helper()
	row, err := q.Enqueue(ctx, id, job, content, forFailure)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func mustDrain(t *testing.T, q *DeliveryQueue, ctx context.Context, send DeliverySend) int {
	t.Helper()
	n, err := q.Drain(ctx, send, 20)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func mustStatus(t *testing.T, q *DeliveryQueue, ctx context.Context, id string) map[string]any {
	t.Helper()
	row, err := q.GetStatus(ctx, id)
	if err != nil || row == nil {
		t.Fatal(id, row, err)
	}
	return row
}

func TestPendingDeliveryClaimedAndSentOnce(t *testing.T) {
	_, q, ctx := pointDeliveryDB(t)
	mustEnqueue(t, q, ctx, "exec-1", &Job{ID: "job-1"}, "brief", false)
	var sends []recordedSend
	if mustDrain(t, q, ctx, recorder(&sends, nil)) != 1 || mustDrain(t, q, ctx, recorder(&sends, nil)) != 0 {
		t.Fatal("drain counts")
	}
	if len(sends) != 1 || sends[0].job.ID != "job-1" || sends[0].content != "brief" || sends[0].forFailure {
		t.Fatal(sends)
	}
	st := mustStatus(t, q, ctx, "exec-1")
	if st["status"] != "delivered" || st["job_json"] != "{}" || st["content"] != "" {
		t.Fatal(st)
	}
	if info, err := os.Stat(q.path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
}

func TestPendingDeliveriesClaimedInInstantOrderAcrossDSTFallBack(t *testing.T) {
	s, q, ctx := pointDeliveryDB(t)
	ny, _ := time.LoadLocation("America/New_York")
	s.clk.nowFn = func() time.Time { return time.Date(2026, 11, 1, 5, 50, 0, 0, time.UTC).In(ny) }
	mustEnqueue(t, q, ctx, "exec-z-earlier", &Job{ID: "job-1"}, "first", false)
	s.clk.nowFn = func() time.Time { return time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC).In(ny) }
	mustEnqueue(t, q, ctx, "exec-a-later", &Job{ID: "job-2"}, "second", false)
	for _, want := range []string{"exec-z-earlier", "exec-a-later"} {
		if row, err := q.ClaimNext(ctx); err != nil || row["execution_id"] != want {
			t.Fatal(row, err, want)
		}
	}
}

func TestTerminalDeliveryRetentionIsBounded(t *testing.T) {
	_, q, ctx := pointDeliveryDB(t)
	prev := MaxTerminalDeliveries
	MaxTerminalDeliveries = 2
	t.Cleanup(func() { MaxTerminalDeliveries = prev })
	for i, id := range []string{"exec-0", "exec-1", "exec-2", "exec-3"} {
		mustEnqueue(t, q, ctx, id, &Job{ID: "job-" + string(rune('0'+i))}, "brief", false)
		if row, _ := q.ClaimNext(ctx); row["execution_id"] != id {
			t.Fatal(row)
		}
		if ok, err := q.finish(ctx, id, nil, false); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	for _, id := range []string{"exec-0", "exec-1", "exec-2", "exec-3"} {
		if mustStatus(t, q, ctx, id)["status"] != "delivered" {
			t.Fatal(id)
		}
	}
	if st := mustStatus(t, q, ctx, "exec-0"); st["error"] != nil || len(st) != 4 {
		t.Fatal("pruned row must read back as a tombstone", st)
	}
	mustEnqueue(t, q, ctx, "exec-0", &Job{ID: "job-replayed"}, "duplicate brief", false)
	var sends []recordedSend
	if mustDrain(t, q, ctx, recorder(&sends, nil)) != 0 || len(sends) != 0 {
		t.Fatal("tombstoned request replayed")
	}
}

func TestFailureDeliveryLaneAndLegacySchema(t *testing.T) {
	_, q, ctx := pointDeliveryDB(t)
	local := "local"
	mustEnqueue(t, q, ctx, "exec-failure", &Job{ID: "job-failure", FailureDeliver: &local}, "failed", true)
	var sends []recordedSend
	if mustDrain(t, q, ctx, recorder(&sends, nil)) != 1 || len(sends) != 1 || !sends[0].forFailure || sends[0].content != "failed" || *sends[0].job.FailureDeliver != "local" {
		t.Fatal(sends)
	}
	legacy := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE deliveries (
                 execution_id TEXT PRIMARY KEY, job_json TEXT NOT NULL, content TEXT NOT NULL,
                 status TEXT NOT NULL, owner_process_id TEXT, owner_pid INTEGER, owner_started_at INTEGER,
                 created_at TEXT NOT NULL, finished_at TEXT, error TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	deliveryDBOverride = legacy
	q = openDeliveryQueue(q.s)
	mustEnqueue(t, q, ctx, "exec-migrated", &Job{ID: "job-migrated"}, "failed", true)
	if v := mustStatus(t, q, ctx, "exec-migrated")["for_failure"]; v != int64(1) {
		t.Fatal(v)
	}
}

func TestWaitTimeoutMarksInflightDeliveryUnknownWithoutRetry(t *testing.T) {
	_, q, ctx := pointDeliveryDB(t)
	zero := 0.0
	mustEnqueue(t, q, ctx, "exec-inflight", &Job{ID: "job-inflight"}, "result", false)
	if row, _ := q.ClaimNext(ctx); row == nil {
		t.Fatal("claim")
	}
	errText, err := q.EnqueueAndWait(ctx, "exec-inflight", &Job{ID: "job-inflight"}, "result", false, &zero)
	if err != nil || errText == nil || !strings.Contains(*errText, "outcome is unknown") {
		t.Fatal(errText, err)
	}
	if status := mustStatus(t, q, ctx, "exec-inflight"); status == nil || status["status"] != "unknown" {
		t.Fatal(status)
	}
	var sends []recordedSend
	if mustDrain(t, q, ctx, recorder(&sends, nil)) != 0 || len(sends) != 0 {
		t.Fatal("fenced row resent", sends)
	}
}

func TestDeadDeliveryOwnerBecomesUnknownAndIsNotRetried(t *testing.T) {
	_, q, ctx := pointDeliveryDB(t)
	mustEnqueue(t, q, ctx, "exec-1", &Job{ID: "job-1"}, "brief", false)
	if row, _ := q.ClaimNext(ctx); row == nil {
		t.Fatal("claim")
	}
	prevID, prevLive := deliveryProcessID, deliveryOwnerIsLive
	t.Cleanup(func() { deliveryProcessID, deliveryOwnerIsLive = prevID, prevLive })
	deliveryProcessID = "replacement-gateway"
	deliveryOwnerIsLive = func(int64, any) (bool, error) { return false, nil }
	if n, err := q.RecoverAbandoned(ctx); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	var sends []recordedSend
	if mustDrain(t, q, ctx, recorder(&sends, nil)) != 0 || len(sends) != 0 {
		t.Fatal("dead owner row resent", sends)
	}
	if status := mustStatus(t, q, ctx, "exec-1"); status["status"] != "unknown" {
		t.Fatal(status)
	}
}

func TestDeliveryFailureIsTerminalNotRetriedAndRedacted(t *testing.T) {
	_, q, ctx := pointDeliveryDB(t)
	mustEnqueue(t, q, ctx, "exec-1", &Job{ID: "job-1"}, "brief", false)
	refusal := "request failed: https://example.test/?token=TOKEN123"
	var sends []recordedSend
	if mustDrain(t, q, ctx, recorder(&sends, &refusal)) != 1 || mustDrain(t, q, ctx, recorder(&sends, &refusal)) != 0 || len(sends) != 1 {
		t.Fatal(sends)
	}
	st := mustStatus(t, q, ctx, "exec-1")
	if st == nil || st["status"] != "failed" || strings.Contains(st["error"].(string), "TOKEN123") || !strings.Contains(st["error"].(string), "token=***") {
		t.Fatal(st)
	}
}

func TestWaitTimeoutLeavesUnclaimedDeliveryQueuedForNextGateway(t *testing.T) {
	_, q, ctx := pointDeliveryDB(t)
	zero := 0.0
	job := &Job{ID: "job-3", Deliver: prompt("origin")}
	errText, err := q.EnqueueAndWait(ctx, "exec-3", job, "result", false, &zero)
	if err != nil || errText != nil {
		t.Fatal(errText, err)
	}
	if status := mustStatus(t, q, ctx, "exec-3"); status == nil || status["status"] != "pending" {
		t.Fatal(status)
	}
	var sends []recordedSend
	if mustDrain(t, q, ctx, recorder(&sends, nil)) != 1 || len(sends) != 1 || sends[0].job.ID != "job-3" || deref(sends[0].job.Deliver) != "origin" ||
		sends[0].content != "result" || sends[0].forFailure {
		t.Fatal(sends)
	}
	if status := mustStatus(t, q, ctx, "exec-3"); status["status"] != "delivered" {
		t.Fatal(status)
	}
}

func TestSameGatewayRecoversTerminalizationFailureWithoutResending(t *testing.T) {
	_, q, ctx := pointDeliveryDB(t)
	mustEnqueue(t, q, ctx, "exec-4", &Job{ID: "job-4"}, "result", false)
	var sends []recordedSend
	prev := deliveryFinish
	deliveryFinish = func(*DeliveryQueue, context.Context, string, *string, bool) (bool, error) {
		return false, errors.New("database temporarily unavailable")
	}
	if _, err := q.Drain(ctx, recorder(&sends, nil), 20); err == nil || !strings.Contains(err.Error(), "temporarily unavailable") {
		t.Fatal(err)
	}
	deliveryFinish = prev
	if mustDrain(t, q, ctx, recorder(&sends, nil)) != 0 || len(sends) != 1 {
		t.Fatal(sends)
	}
	st := mustStatus(t, q, ctx, "exec-4")
	if st["status"] != "unknown" || !strings.Contains(st["error"].(string), "not retried") {
		t.Fatal(st)
	}
}

func TestEnqueueDuringRetentionNeverRecreatesDeliveredRequest(t *testing.T) {
	_, q, ctx := pointDeliveryDB(t)
	mustEnqueue(t, q, ctx, "execution", &Job{ID: "job"}, "original", false)
	var sends []recordedSend
	if mustDrain(t, q, ctx, recorder(&sends, nil)) != 1 {
		t.Fatal("first drain")
	}
	var prunes []string
	prevProbe, prevMax := deliveryAfterTombstoneProbe, MaxTerminalDeliveries
	deliveryAfterTombstoneProbe = func(context.Context) {
		db, err := sql.Open("sqlite", q.path+"?_pragma=busy_timeout(0)")
		if err != nil {
			t.Error(err)
			return
		}
		defer db.Close()
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		MaxTerminalDeliveries = 0
		err = pruneTerminalDeliveries(context.Background(), conn)
		MaxTerminalDeliveries = prevMax
		switch {
		case err == nil:
			prunes = append(prunes, "pruned")
		case strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "busy"):
			prunes = append(prunes, "locked")
		default:
			t.Error(err)
		}
	}
	replay, err := q.Enqueue(ctx, "execution", &Job{ID: "job"}, "duplicate", false)
	deliveryAfterTombstoneProbe = prevProbe
	if err != nil || len(prunes) == 0 {
		t.Fatal(err, prunes)
	}
	MaxTerminalDeliveries = 0
	err = q.transaction(ctx, false, func(conn state.SQLiteConn) error { return pruneTerminalDeliveries(ctx, conn) })
	MaxTerminalDeliveries = prevMax
	if err != nil || replay["status"] != "delivered" || mustStatus(t, q, ctx, "execution")["status"] != "delivered" {
		t.Fatal(err, replay)
	}
	if mustDrain(t, q, ctx, recorder(&sends, nil)) != 0 || len(sends) != 1 {
		t.Fatal(sends)
	}
}

func TestDrainedOutcomeSettlesLedgerAndCurrentJobOnly(t *testing.T) {
	s, q, ctx := pointDeliveryDB(t)
	job, err := s.createJob(ctx, CreateJobOptions{Prompt: strp("p"), Schedule: "every 1h", Deliver: strp("local")})
	if err != nil {
		t.Fatal(err)
	}
	ledger := openExecutionsLedger(s)
	exec, err := ledger.Create(ctx, job.ID, "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	execID := recText(exec, "id")
	queued := "queued"
	if _, err := ledger.Finish(ctx, execID, true, nil, &queued, false); err != nil {
		t.Fatal(err)
	}
	if err := s.withJobsLock(ctx, func(tx *jobsTx) error {
		jobs, err := s.loadJobs(ctx, tx)
		if err != nil {
			return err
		}
		status := "delivery_queued"
		jobs[0].LastStatus, jobs[0].CanonicalCompletions = &status, []string{execID}
		return s.saveJobsLocked(ctx, tx, jobs, nil, false)
	}); err != nil {
		t.Fatal(err)
	}
	if out, _ := s.settledOutcome(ctx, execID); out != nil {
		t.Fatal("outcome before the drain", *out)
	}
	if ok, _ := s.settleQueuedDelivery(ctx, job.ID, execID); ok {
		t.Fatal("settled while queued")
	}
	refusal := "chat not found"
	mustEnqueue(t, q, ctx, execID, job, "brief", false)
	var sends []recordedSend
	if mustDrain(t, q, ctx, recorder(&sends, &refusal)) != 1 {
		t.Fatal("drain")
	}
	saved, _ := s.getJob(ctx, job.ID)
	if saved.LastStatus == nil || *saved.LastStatus != "delivery_failed" || saved.LastDeliveryError == nil || *saved.LastDeliveryError != "chat not found" {
		t.Fatal(saved.LastStatus, saved.LastDeliveryError)
	}
	row, _ := ledger.Get(ctx, execID)
	if row["delivery_outcome"] != "failed" {
		t.Fatal(row["delivery_outcome"])
	}
	if out, _ := s.settledOutcome(ctx, execID); out == nil || *out != "failed" {
		t.Fatal(out)
	}
	if ok, err := s.settleQueuedDelivery(ctx, "", execID); ok || err != nil {
		t.Fatal(ok, err)
	}
}
