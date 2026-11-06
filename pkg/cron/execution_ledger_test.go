package cron

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

func ledgerFixture(t *testing.T) (*Ledger, *Store) {
	t.Helper()
	s, _ := newTestStore(t)
	return openExecutionsLedger(s), s
}

func mustLedger(t *testing.T) func(map[string]any, error) map[string]any {
	t.Helper()
	return func(rec map[string]any, err error) map[string]any {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
}

func TestExecutionTransitionsAreDurable(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	claimed := mustLedger(t)(l.Create(ctx, "job-1", "builtin", nil))
	if claimed["status"] != "claimed" || recText(claimed, "claimed_at") == "" || claimed["started_at"] != nil || claimed["finished_at"] != nil {
		t.Fatalf("claimed %v", claimed)
	}
	running := mustLedger(t)(l.MarkRunning(ctx, recText(claimed, "id")))
	if running["status"] != "running" || recText(running, "started_at") == "" {
		t.Fatalf("running %v", running)
	}
	completed := mustLedger(t)(l.Finish(ctx, recText(claimed, "id"), true, nil, nil, false))
	if completed["status"] != "completed" || recText(completed, "finished_at") == "" || completed["error"] != nil {
		t.Fatalf("completed %v", completed)
	}
	persisted, err := l.List(ctx, strPtr("job-1"), 50, nil)
	if err != nil || len(persisted) != 1 || !reflect.DeepEqual(persisted[0], completed) {
		t.Fatalf("persisted %v %v", persisted, err)
	}
}

func TestExecutionCanBeLoadedByExactAttemptID(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	first := mustLedger(t)(l.Create(ctx, "same-job", "builtin", nil))
	second := mustLedger(t)(l.Create(ctx, "same-job", "builtin", nil))
	for _, want := range []map[string]any{first, second} {
		if got := mustLedger(t)(l.Get(ctx, recText(want, "id"))); !reflect.DeepEqual(got, want) {
			t.Fatalf("get %v != %v", got, want)
		}
	}
	if got := mustLedger(t)(l.Get(ctx, "missing")); got != nil {
		t.Fatalf("missing %v", got)
	}
}

func TestExternalHandoffRecoveryRespectsAdoptionGrace(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	fresh := mustLedger(t)(l.Create(ctx, "handoff-job", "builtin", nil))
	if mustLedger(t)(l.MarkHandoffPending(ctx, recText(fresh, "id"))) == nil {
		t.Fatal("handoff not marked")
	}
	stale := mustLedger(t)(l.Create(ctx, "handoff-stale", "builtin", nil))
	pending := mustLedger(t)(l.MarkHandoffPending(ctx, recText(stale, "id")))
	patchChronos(t, &executionProcessID, "replacement-gateway")
	patchChronos(t, &ownerIsLive, func(int64, any) (bool, error) { return false, nil })
	if n, err := l.RecoverInterrupted(ctx); err != nil || n != 0 {
		t.Fatalf("fresh handoff recovered %d %v", n, err)
	}
	if got := mustLedger(t)(l.Get(ctx, recText(fresh, "id"))); got["status"] != "claimed" {
		t.Fatalf("fresh %v", got)
	}
	adopted := mustLedger(t)(l.AdoptClaimed(ctx, recText(fresh, "id")))
	if adopted["status"] != "running" || recInt(adopted, "handoff_pending") != 0 {
		t.Fatalf("adopted %v", adopted)
	}
	started, _ := pending["handoff_started_at"].(float64)
	patchChronos(t, &ledgerWallTime, func() float64 { return started + HandoffAdoptionGraceSeconds + 1 })
	if n, err := l.RecoverInterrupted(ctx); err != nil || n != 1 {
		t.Fatalf("stale recovery %d %v", n, err)
	}
	if got := mustLedger(t)(l.Get(ctx, recText(stale, "id"))); got["status"] != "unknown" || recInt(got, "handoff_pending") != 0 {
		t.Fatalf("stale %v", got)
	}
}

func TestRecoveryDoesNotOverwriteConcurrentWorkerAdoption(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "adoption-race", "builtin", nil))
	pending := mustLedger(t)(l.MarkHandoffPending(ctx, recText(record, "id")))
	started, _ := pending["handoff_started_at"].(float64)
	patchChronos(t, &executionProcessID, "replacement-scheduler")
	patchChronos(t, &ledgerWallTime, func() float64 { return started + HandoffAdoptionGraceSeconds + 1 })
	patchChronos(t, &ownerIsLive, func(int64, any) (bool, error) {
		executionProcessID = "external-worker"
		ledgerGetpid = func() int { return 4242 }
		ledgerStartTime = func(int) (int64, bool) { return 9876, true }
		if rec, err := l.AdoptClaimed(ctx, recText(record, "id")); err != nil || rec == nil {
			t.Errorf("adoption %v %v", rec, err)
		}
		return false, nil
	})
	patchChronos(t, &ledgerGetpid, ledgerGetpid)
	patchChronos(t, &ledgerStartTime, ledgerStartTime)
	if n, err := l.RecoverInterrupted(ctx); err != nil || n != 0 {
		t.Fatalf("recovered %d %v", n, err)
	}
	current := mustLedger(t)(l.Get(ctx, recText(record, "id")))
	if current["status"] != "running" || current["process_id"] != "external-worker" || recInt(current, "pid") != 4242 {
		t.Fatalf("current %v", current)
	}
}

func TestForeignProcessCannotStartOrFinishExecution(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "owner-fence", "builtin", nil))
	id, pid := recText(record, "id"), int(recInt(record, "pid"))
	original := executionProcessID
	patchChronos(t, &executionProcessID, "foreign-process")
	patchChronos(t, &ledgerGetpid, func() int { return pid + 1 })
	if r, _ := l.MarkRunning(ctx, id); r != nil {
		t.Fatal("foreign start")
	}
	if r, _ := l.Finish(ctx, id, true, nil, nil, false); r != nil {
		t.Fatal("foreign finish")
	}
	executionProcessID, ledgerGetpid = original, func() int { return pid }
	if r, _ := l.MarkRunning(ctx, id); r == nil {
		t.Fatal("owner start refused")
	}
	if r, _ := l.Finish(ctx, id, true, nil, nil, false); r == nil {
		t.Fatal("owner finish refused")
	}
}

func TestExecutionLedgerFollowsTheStoreHome(t *testing.T) {
	root := t.TempDir()
	defaultStore := openCronStore(filepath.Join(root, "default"), nil)
	workerStore := openCronStore(filepath.Join(root, "worker"), nil)
	ctx := context.Background()
	defaultRow := mustLedger(t)(openExecutionsLedger(defaultStore).Create(ctx, "default-job", "builtin", nil))
	workerRow := mustLedger(t)(openExecutionsLedger(workerStore).Create(ctx, "worker-job", "builtin", nil))
	for _, tc := range []struct {
		s    *Store
		want map[string]any
	}{{workerStore, workerRow}, {defaultStore, defaultRow}} {
		rows, err := openExecutionsLedger(tc.s).List(ctx, nil, 50, nil)
		if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0], tc.want) {
			t.Fatalf("rows %v %v", rows, err)
		}
	}
	for _, name := range []string{"default", "worker"} {
		if st, err := os.Stat(filepath.Join(root, name, "cron", "executions.db")); err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s ledger %v", name, err)
		}
	}
}

func TestTerminalExecutionCannotBeRewrittenAndFailureKeepsError(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "immutable", "builtin", nil))
	_, _ = l.MarkRunning(ctx, recText(record, "id"))
	_, _ = l.Finish(ctx, recText(record, "id"), true, nil, nil, false)
	if r, _ := l.Finish(ctx, recText(record, "id"), false, strPtr("late writer"), nil, false); r != nil {
		t.Fatal("terminal row rewritten")
	}
	if latest := mustLedger(t)(l.Latest(ctx, "immutable")); latest["status"] != "completed" {
		t.Fatalf("latest %v", latest)
	}
	other := mustLedger(t)(l.Create(ctx, "job-2", "external", nil))
	failed := mustLedger(t)(l.Finish(ctx, recText(other, "id"), false, strPtr("provider exploded"), nil, false))
	if failed["status"] != "failed" || failed["error"] != "provider exploded" {
		t.Fatalf("failed %v", failed)
	}
}

func TestRetentionBoundsTerminalHistoryButPreservesInflight(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	patchChronos(t, &maxTerminalExecutions, 3)
	inflight := mustLedger(t)(l.Create(ctx, "live", "builtin", nil))
	_, _ = l.MarkRunning(ctx, recText(inflight, "id"))
	for i := range 8 {
		row := mustLedger(t)(l.Create(ctx, "done-"+string(rune('0'+i)), "builtin", nil))
		_, _ = l.Finish(ctx, recText(row, "id"), true, nil, nil, false)
	}
	records, _ := l.List(ctx, nil, 100, nil)
	completed := 0
	for _, r := range records {
		if r["status"] == "completed" {
			completed++
		}
	}
	if completed != 3 {
		t.Fatalf("completed kept %d", completed)
	}
	if latest := mustLedger(t)(l.Latest(ctx, "live")); latest["status"] != "running" {
		t.Fatalf("inflight %v", latest)
	}
}

func TestRecentlyFinishedLongRunningExecutionSurvivesRetention(t *testing.T) {
	cur := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := mutableClockStore(t, &cur, time.UTC)
	l := openExecutionsLedger(s)
	ctx := context.Background()
	patchChronos(t, &maxTerminalExecutions, 1)
	long := mustLedger(t)(l.Create(ctx, "long-running", "builtin", nil))
	if mustLedger(t)(l.MarkRunning(ctx, recText(long, "id"))) == nil {
		t.Fatal("not running")
	}
	cur = cur.Add(time.Second)
	newer := mustLedger(t)(l.Create(ctx, "newer", "builtin", nil))
	if mustLedger(t)(l.Finish(ctx, recText(newer, "id"), true, nil, nil, false)) == nil {
		t.Fatal("newer not finished")
	}
	cur = cur.Add(time.Second)
	finished := mustLedger(t)(l.Finish(ctx, recText(long, "id"), true, nil, nil, false))
	if finished == nil || finished["status"] != "completed" {
		t.Fatalf("finished %v", finished)
	}
	if got := mustLedger(t)(l.Get(ctx, recText(long, "id"))); got["status"] != "completed" {
		t.Fatalf("long %v", got)
	}
	if got := mustLedger(t)(l.Get(ctx, recText(newer, "id"))); got != nil {
		t.Fatalf("newer kept %v", got)
	}
}

func TestCorruptLedgerFailsClosedWithoutOverwrite(t *testing.T) {
	l, _ := ledgerFixture(t)
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Create(context.Background(), "new", "builtin", nil); err == nil {
		t.Fatal("corrupt ledger accepted a write")
	}
	if b, _ := os.ReadFile(l.path); !bytes.Equal(b, []byte("not a sqlite database")) {
		t.Fatalf("corrupt ledger overwritten %q", b)
	}
}

func TestRecoveryLeavesLiveProcessExecutionRunning(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "still-live", "builtin", nil))
	_, _ = l.MarkRunning(ctx, recText(record, "id"))
	if n, err := l.RecoverInterrupted(ctx); err != nil || n != 0 {
		t.Fatalf("recovered %d %v", n, err)
	}
	if latest := mustLedger(t)(l.Latest(ctx, "still-live")); latest["status"] != "running" {
		t.Fatalf("latest %v", latest)
	}
}

func TestRestartMarksInterruptedExecutionUnknownWithoutRequeue(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	child := exec.Command("true")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	original := executionProcessID
	executionProcessID = "exited-process"
	patchChronos(t, &ledgerGetpid, func() int { return child.Process.Pid })
	record := mustLedger(t)(l.Create(ctx, "restart-job", "builtin", nil))
	_, err := l.MarkRunning(ctx, recText(record, "id"))
	executionProcessID = original
	if err != nil {
		t.Fatal(err)
	}
	if n, err := l.RecoverInterrupted(ctx); err != nil || n != 1 {
		t.Fatalf("recovered %d %v", n, err)
	}
	records, _ := l.List(ctx, strPtr("restart-job"), 50, nil)
	if len(records) != 1 || records[0]["id"] != record["id"] || records[0]["status"] != "unknown" || recText(records[0], "finished_at") == "" || !strings.Contains(strings.ToLower(recText(records[0], "error")), "restart") {
		t.Fatalf("records %v", records)
	}
}

type trackingConn struct {
	state.SQLiteConn
	id     int
	closed *[]int
	mu     *sync.Mutex
}

func (c trackingConn) Close() error {
	c.mu.Lock()
	*c.closed = append(*c.closed, c.id)
	c.mu.Unlock()
	return c.SQLiteConn.Close()
}

func (c trackingConn) Raw(f func(driverConn any) error) error {
	conn := c.SQLiteConn
	for {
		switch inner := conn.(type) {
		case interface{ Raw(func(any) error) error }:
			return inner.Raw(f)
		case *state.TrackedConn:
			conn = inner.SQLiteConn
		default:
			return fmt.Errorf("connection exposes no SQLite handle: %T", conn)
		}
	}
}

func countLedgerConnections(t *testing.T) (*[]int, *[]int) {
	t.Helper()
	var opened, closed []int
	var mu sync.Mutex
	real := state.OpenDB
	patchChronos(t, &state.OpenDB, func(ctx context.Context, path, label string, opts ...state.OpenDBOption) (state.SQLiteConn, error) {
		conn, err := real(ctx, path, label, opts...)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, len(opened))
		return trackingConn{SQLiteConn: conn, id: len(opened) - 1, closed: &closed, mu: &mu}, nil
	})
	return &opened, &closed
}

func TestLedgerOperationsCloseEveryConnection(t *testing.T) {
	l, s := ledgerFixture(t)
	ctx := context.Background()
	opened, closed := countLedgerConnections(t)
	record := mustLedger(t)(l.Create(ctx, "leak-check", "builtin", nil))
	_, _ = l.MarkRunning(ctx, recText(record, "id"))
	_, _ = l.Finish(ctx, recText(record, "id"), true, nil, nil, false)
	_, _ = l.List(ctx, strPtr("leak-check"), 50, nil)
	_, _ = latestExecutions(s, []string{"leak-check"})
	_, _ = l.RecoverInterrupted(ctx)
	if len(*opened) == 0 || !reflect.DeepEqual(*opened, *closed) {
		t.Fatalf("opened %v closed %v", *opened, *closed)
	}
	*opened, *closed = nil, nil
	if r, _ := l.MarkRunning(ctx, "does-not-exist"); r != nil {
		t.Fatal("missing row started")
	}
	if len(*opened) != 1 || len(*closed) != 1 {
		t.Fatalf("early return opened %v closed %v", *opened, *closed)
	}
}

func TestJobListingExposesLatestExecution(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job, err := s.createJob(CreateJobOptions{Prompt: strPtr("audit me"), Schedule: "every 1h", Name: strPtr("audit")})
	if err != nil {
		t.Fatal(err)
	}
	l := openExecutionsLedger(s)
	record := mustLedger(t)(l.Create(ctx, job.ID, "builtin", nil))
	_, _ = l.MarkRunning(ctx, recText(record, "id"))
	listed, err := s.listJobs(true)
	if err != nil || len(listed) != 1 || listed[0].LatestExecution["id"] != record["id"] || listed[0].LatestExecution["status"] != "running" {
		t.Fatalf("listed %v %v", listed, err)
	}
}

func TestExecutionHistoryOrdersByInstantAcrossDSTFallBack(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cur := time.Date(2026, 11, 1, 5, 50, 0, 0, time.UTC)
	s := mutableClockStore(t, &cur, ny)
	l := openExecutionsLedger(s)
	ctx := context.Background()
	earlier := mustLedger(t)(l.Create(ctx, "dst-job", "builtin", nil))
	cur = time.Date(2026, 11, 1, 6, 10, 0, 0, time.UTC)
	later := mustLedger(t)(l.Create(ctx, "dst-job", "builtin", nil))
	if recText(later, "claimed_at") >= recText(earlier, "claimed_at") {
		t.Fatalf("wall clock strings %s %s", later["claimed_at"], earlier["claimed_at"])
	}
	if latest := mustLedger(t)(l.Latest(ctx, "dst-job")); latest["id"] != later["id"] {
		t.Fatalf("latest %v", latest)
	}
	if many, _ := latestExecutions(s, []string{"dst-job"}); many["dst-job"]["id"] != later["id"] {
		t.Fatalf("latest many %v", many)
	}
	rows, _ := l.List(ctx, strPtr("dst-job"), 50, nil)
	if len(rows) != 2 || rows[0]["id"] != later["id"] || rows[1]["id"] != earlier["id"] {
		t.Fatalf("history %v", rows)
	}
	page, _ := l.List(ctx, strPtr("dst-job"), 50, strPtr(recText(later, "claimed_at")))
	if len(page) != 1 || page[0]["id"] != earlier["id"] {
		t.Fatalf("page %v", page)
	}
}

func TestProviderStartRecoversInterruptedRecordsBeforeHeartbeat(t *testing.T) {
	s, _ := newTestStore(t)
	var events []string
	real := state.OpenDB
	patchChronos(t, &state.OpenDB, func(ctx context.Context, path, label string, opts ...state.OpenDBOption) (state.SQLiteConn, error) {
		if s.readMarkerFields("ticker_heartbeat") == nil {
			events = append(events, "recover")
		} else {
			events = append(events, "recover-after-heartbeat")
		}
		return real(ctx, path, label, opts...)
	})
	stop := make(chan struct{})
	close(stop)
	interval := 1.0
	(&InProcessCronScheduler{}).Start(withCronStore(context.Background(), s), stop, StartOptions{Interval: &interval})
	if s.readMarkerFields("ticker_heartbeat") != nil {
		events = append(events, "heartbeat")
	}
	if len(events) < 2 || events[0] != "recover" || events[len(events)-1] != "heartbeat" {
		t.Fatalf("events %v", events)
	}
}
