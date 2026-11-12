package cron

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

// Environment naming the ledger operation a re-executed test binary runs instead of the tests
const ledgerChildEnv = "ICEBATH_TEST_LEDGER_CHILD"

// Runs the ledger operation a parent test asked of this re-executed binary and exits with its status
func dispatchLedgerChild() {
	mode := os.Getenv(ledgerChildEnv)
	if mode == "" {
		return
	}
	ctx := context.Background()
	s := openCronStore(home.Dir(ctx), nil)
	if p := os.Getenv(ledgerChildEnv + "_FILE"); p != "" {
		s.executionsFile = p
	}
	if err := runLedgerChild(ctx, openExecutionsLedger(s), mode, os.Getenv(ledgerChildEnv+"_ARG")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// One ledger operation of a child process, printing what the parent reads back
func runLedgerChild(ctx context.Context, l *Ledger, mode, arg string) error {
	switch mode {
	case "create-running":
		r, err := l.Create(ctx, "restart-job", "builtin", nil)
		if err != nil {
			return err
		}
		if _, err := l.MarkRunning(ctx, recText(r, "id")); err != nil {
			return err
		}
		fmt.Println(recText(r, "id"))
	case "create-running-direct":
		r, err := l.Create(ctx, arg, "direct", nil)
		if err != nil {
			return err
		}
		if _, err := l.MarkRunning(ctx, recText(r, "id")); err != nil {
			return err
		}
		fmt.Println(recText(r, "id"))
	case "create-running-hold":
		r, err := l.Create(ctx, arg, "receipt-fixture", nil)
		if err != nil {
			return err
		}
		if _, err := l.MarkRunning(ctx, recText(r, "id")); err != nil {
			return err
		}
		fmt.Println(recText(r, "id"))
		_, err = io.Copy(io.Discard, os.Stdin)
		return err
	case "recover":
		n, err := l.RecoverInterrupted(ctx)
		if err != nil {
			return err
		}
		fmt.Println(n)
		rows, err := l.List(ctx, strPtr("restart-job"), 50, nil)
		if err != nil {
			return err
		}
		text, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		fmt.Println(string(text))
	case "adopt":
		rec, err := l.AdoptClaimed(ctx, arg)
		if err != nil {
			return err
		}
		if rec == nil {
			return errors.New("adoption refused")
		}
		fmt.Println("adopted")
		time.Sleep(60 * time.Second)
	default:
		return runOccurrenceChild(ctx, l.s, mode, arg)
	}
	return nil
}

// Child process of this test binary running one ledger mode against a home
func ledgerChildCommand(h, file, mode, arg string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), home.EnvHome+"="+h, ledgerChildEnv+"="+mode, ledgerChildEnv+"_FILE="+file, ledgerChildEnv+"_ARG="+arg)
	return cmd
}

// Stdout of a child ledger mode that must exit zero
func runLedgerChildProcess(t *testing.T, h, file, mode string) string {
	t.Helper()
	cmd := ledgerChildCommand(h, file, mode, "")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s child: %v %s", mode, err, stderr.String())
	}
	return string(out)
}

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

func TestFreshExternalHandoffIsNotRecoveredBeforeWorkerAdopts(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "handoff-job", "builtin", nil))
	if mustLedger(t)(l.MarkHandoffPending(ctx, recText(record, "id"))) == nil {
		t.Fatal("handoff not marked")
	}
	patchChronos(t, &executionProcessID, "replacement-gateway")
	patchChronos(t, &ownerIsLive, func(int64, any) (bool, error) { return false, nil })
	if n, err := l.RecoverInterrupted(ctx); err != nil || n != 0 {
		t.Fatalf("fresh handoff recovered %d %v", n, err)
	}
	if got := mustLedger(t)(l.Get(ctx, recText(record, "id"))); got["status"] != "claimed" {
		t.Fatalf("fresh %v", got)
	}
	adopted := mustLedger(t)(l.AdoptClaimed(ctx, recText(record, "id")))
	if adopted["status"] != "running" || adopted["handoff_pending"] != int64(0) {
		t.Fatalf("adopted %v", adopted)
	}
}

func TestStaleExternalHandoffIsRecoveredUnknown(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "handoff-job", "builtin", nil))
	pending := mustLedger(t)(l.MarkHandoffPending(ctx, recText(record, "id")))
	patchChronos(t, &executionProcessID, "replacement-gateway")
	patchChronos(t, &ownerIsLive, func(int64, any) (bool, error) { return false, nil })
	started, _ := pending["handoff_started_at"].(float64)
	patchChronos(t, &ledgerWallTime, func() float64 { return started + HandoffAdoptionGraceSeconds + 1 })
	if n, err := l.RecoverInterrupted(ctx); err != nil || n != 1 {
		t.Fatalf("stale recovery %d %v", n, err)
	}
	recovered := mustLedger(t)(l.Get(ctx, recText(record, "id")))
	if recovered["status"] != "unknown" || recovered["handoff_pending"] != int64(0) {
		t.Fatalf("stale %v", recovered)
	}
}

func TestRecoveryDoesNotOverwriteConcurrentWorkerAdoption(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "adoption-race", "builtin", nil))
	pending := mustLedger(t)(l.MarkHandoffPending(ctx, recText(record, "id")))
	if pending == nil {
		t.Fatal("handoff not marked")
	}
	started, _ := pending["handoff_started_at"].(float64)
	patchChronos(t, &executionProcessID, "replacement-scheduler")
	patchChronos(t, &ledgerWallTime, func() float64 { return started + HandoffAdoptionGraceSeconds + 1 })
	patchChronos(t, &ledgerGetpid, ledgerGetpid)
	patchChronos(t, &ledgerStartTime, ledgerStartTime)
	patchChronos(t, &ownerIsLive, func(int64, any) (bool, error) {
		executionProcessID = "external-worker"
		ledgerGetpid = func() int { return 4242 }
		ledgerStartTime = func(int) (int64, bool) { return 9876, true }
		if rec, err := l.AdoptClaimed(ctx, recText(record, "id")); err != nil || rec == nil {
			t.Errorf("adoption %v %v", rec, err)
		}
		return false, nil
	})
	if n, err := l.RecoverInterrupted(ctx); err != nil || n != 0 {
		t.Fatalf("recovered %d %v", n, err)
	}
	current := mustLedger(t)(l.Get(ctx, recText(record, "id")))
	if current == nil || current["status"] != "running" || current["process_id"] != "external-worker" || current["pid"] != int64(4242) {
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
	if r := mustLedger(t)(l.MarkRunning(ctx, id)); r != nil {
		t.Fatal("foreign start")
	}
	if r := mustLedger(t)(l.Finish(ctx, id, true, nil, nil, false)); r != nil {
		t.Fatal("foreign finish")
	}
	executionProcessID, ledgerGetpid = original, func() int { return pid }
	if r := mustLedger(t)(l.MarkRunning(ctx, id)); r == nil {
		t.Fatal("owner start refused")
	}
	if r := mustLedger(t)(l.Finish(ctx, id, true, nil, nil, false)); r == nil {
		t.Fatal("owner finish refused")
	}
}

func TestExecutionLedgerFollowsTheCurrentProfileHome(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	current := filepath.Join(root, "default")
	ledger := func() *Ledger { return openExecutionsLedger(cronStoreFrom(home.WithOverride(ctx, current))) }
	defaultRow := mustLedger(t)(ledger().Create(ctx, "default-job", "builtin", nil))
	current = filepath.Join(root, "worker")
	workerRow := mustLedger(t)(ledger().Create(ctx, "worker-job", "builtin", nil))
	if rows, err := ledger().List(ctx, nil, 50, nil); err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0], workerRow) {
		t.Fatalf("worker rows %v %v", rows, err)
	}
	current = filepath.Join(root, "default")
	if rows, err := ledger().List(ctx, nil, 50, nil); err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0], defaultRow) {
		t.Fatalf("default rows %v %v", rows, err)
	}
	for _, name := range []string{"default", "worker"} {
		if st, err := os.Stat(filepath.Join(root, name, "cron", "executions.db")); err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s ledger %v", name, err)
		}
	}
}

func TestTerminalExecutionCannotBeRewritten(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "immutable", "builtin", nil))
	mustLedger(t)(l.MarkRunning(ctx, recText(record, "id")))
	mustLedger(t)(l.Finish(ctx, recText(record, "id"), true, nil, nil, false))
	if r := mustLedger(t)(l.Finish(ctx, recText(record, "id"), false, strPtr("late writer"), nil, false)); r != nil {
		t.Fatal("terminal row rewritten")
	}
	if latest := mustLedger(t)(l.Latest(ctx, "immutable")); latest["status"] != "completed" {
		t.Fatalf("latest %v", latest)
	}
}

func TestFailedExecutionKeepsError(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "job-2", "external", nil))
	failed := mustLedger(t)(l.Finish(ctx, recText(record, "id"), false, strPtr("provider exploded"), nil, false))
	if failed["status"] != "failed" || failed["error"] != "provider exploded" {
		t.Fatalf("failed %v", failed)
	}
}

func TestRetentionBoundsTerminalHistoryButPreservesInflight(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	patchChronos(t, &maxTerminalExecutions, 3)
	inflight := mustLedger(t)(l.Create(ctx, "live", "builtin", nil))
	mustLedger(t)(l.MarkRunning(ctx, recText(inflight, "id")))
	for i := range 8 {
		row := mustLedger(t)(l.Create(ctx, fmt.Sprintf("done-%d", i), "builtin", nil))
		mustLedger(t)(l.Finish(ctx, recText(row, "id"), true, nil, nil, false))
	}
	records, err := l.List(ctx, nil, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
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

func TestCorruptStoreFailsClosedWithoutOverwrite(t *testing.T) {
	l, _ := ledgerFixture(t)
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := l.Create(context.Background(), "new", "builtin", nil)
	if class, ok := state.SQLiteClassOf(err); !ok || class == state.SQLiteInterfaceError {
		t.Fatalf("corrupt ledger raised %T %v, want a sqlite3 DatabaseError", err, err)
	}
	if b, _ := os.ReadFile(l.path); !bytes.Equal(b, []byte("not a sqlite database")) {
		t.Fatalf("corrupt ledger overwritten %q", b)
	}
}

func TestRecoveryDoesNotMarkLiveProcessExecutionUnknown(t *testing.T) {
	l, _ := ledgerFixture(t)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "still-live", "builtin", nil))
	mustLedger(t)(l.MarkRunning(ctx, recText(record, "id")))
	if n, err := l.RecoverInterrupted(ctx); err != nil || n != 0 {
		t.Fatalf("recovered %d %v", n, err)
	}
	if latest := mustLedger(t)(l.Latest(ctx, "still-live")); latest["status"] != "running" {
		t.Fatalf("latest %v", latest)
	}
}

func TestRestartMarksInterruptedExecutionUnknownWithoutRequeue(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	executionID := strings.TrimSpace(runLedgerChildProcess(t, h, "", "create-running"))
	lines := strings.Split(strings.TrimSpace(runLedgerChildProcess(t, h, "", "recover")), "\n")
	if len(lines) != 2 || lines[0] != "1" {
		t.Fatalf("recover output %q", lines)
	}
	var records []map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0]["id"] != executionID || records[0]["status"] != "unknown" || recText(records[0], "finished_at") == "" || !strings.Contains(strings.ToLower(recText(records[0], "error")), "restart") {
		t.Fatalf("records %v", records)
	}
	var statuses []any
	for _, r := range records {
		statuses = append(statuses, r["status"])
	}
	if !reflect.DeepEqual(statuses, []any{"unknown"}) {
		t.Fatalf("statuses %v", statuses)
	}
}

func TestTerminalizerRefusesAnAttemptWhoseWorkerIsStillAlive(t *testing.T) {
	l, s := ledgerFixture(t)
	s.executionsFile = filepath.Join(t.TempDir(), "executions.db")
	l = openExecutionsLedger(s)
	ctx := context.Background()
	record := mustLedger(t)(l.Create(ctx, "job-live", "direct", nil))
	id := recText(record, "id")
	if mustLedger(t)(l.MarkHandoffPending(ctx, id)) == nil {
		t.Fatal("handoff not marked")
	}
	worker := ledgerChildCommand(s.Home(), s.executionsFile, "adopt", id)
	worker.Stdout = io.Discard
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = worker.Process.Kill()
		_ = worker.Wait()
	}()
	status := func() any { return mustLedger(t)(l.Get(ctx, id))["status"] }
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if status() == "running" {
			break
		}
	}
	if got := status(); got != "running" {
		t.Fatalf("worker never adopted, status %v", got)
	}
	if ok, err := l.TerminalizeDeadOwner(ctx, id, "exit 9"); err != nil || ok {
		t.Fatalf("live worker terminalised %v %v", ok, err)
	}
	if got := status(); got != "running" {
		t.Fatalf("status after refusal %v", got)
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

// Wraps the database opener so each ledger connection records its open and close
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
	mustLedger(t)(l.MarkRunning(ctx, recText(record, "id")))
	mustLedger(t)(l.Finish(ctx, recText(record, "id"), true, nil, nil, false))
	if _, err := l.List(ctx, strPtr("leak-check"), 50, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := latestExecutions(s, []string{"leak-check"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*opened) == 0 || !reflect.DeepEqual(*opened, *closed) {
		t.Fatalf("opened %v closed %v", *opened, *closed)
	}
}

func TestEarlyReturnStillClosesConnection(t *testing.T) {
	l, _ := ledgerFixture(t)
	opened, closed := countLedgerConnections(t)
	if r := mustLedger(t)(l.MarkRunning(context.Background(), "does-not-exist")); r != nil {
		t.Fatal("missing row started")
	}
	if len(*opened) != 1 || len(*closed) != 1 {
		t.Fatalf("early return opened %v closed %v", *opened, *closed)
	}
}

func TestJobListingExposesLatestExecution(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	job, err := s.createJob(ctx, CreateJobOptions{Prompt: strPtr("audit me"), Schedule: "every 1h", Name: strPtr("audit")})
	if err != nil {
		t.Fatal(err)
	}
	l := openExecutionsLedger(s)
	record := mustLedger(t)(l.Create(ctx, job.ID, "builtin", nil))
	mustLedger(t)(l.MarkRunning(ctx, recText(record, "id")))
	listed, err := s.listJobs(ctx, true)
	if err != nil || len(listed) != 1 || listed[0].LatestExecution["id"] != record["id"] || listed[0].LatestExecution["status"] != "running" {
		t.Fatalf("listed %v %v", listed, err)
	}
}

func TestHistoryOrdersByInstantAcrossDstFallBack(t *testing.T) {
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
	if many, err := latestExecutions(s, []string{"dst-job"}); err != nil || many["dst-job"]["id"] != later["id"] {
		t.Fatalf("latest many %v %v", many, err)
	}
	rows, err := l.List(ctx, strPtr("dst-job"), 50, nil)
	if err != nil || len(rows) != 2 || rows[0]["id"] != later["id"] || rows[1]["id"] != earlier["id"] {
		t.Fatalf("history %v %v", rows, err)
	}
	page, err := l.List(ctx, strPtr("dst-job"), 50, strPtr(recText(later, "claimed_at")))
	if err != nil || len(page) != 1 || page[0]["id"] != earlier["id"] {
		t.Fatalf("page %v %v", page, err)
	}
}

func TestProviderStartRecoversInterruptedRecordsBeforeTick(t *testing.T) {
	s, _ := newTestStore(t)
	var events []string
	patchChronos(t, &recoverInterruptedExecutions, func(context.Context, *Store) (int, error) {
		events = append(events, "recover")
		return 0, nil
	})
	patchChronos(t, &recordTickerHeartbeat, func(*Store, context.Context, bool) { events = append(events, "heartbeat") })
	stop := make(chan struct{})
	close(stop)
	interval := 1.0
	(&InProcessCronScheduler{}).Start(withCronStore(context.Background(), s), stop, StartOptions{Interval: &interval})
	if len(events) < 2 || !reflect.DeepEqual(events[:2], []string{"recover", "heartbeat"}) {
		t.Fatalf("events %v", events)
	}
}
