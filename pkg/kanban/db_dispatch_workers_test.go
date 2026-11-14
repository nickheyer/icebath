package kanban

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	testEpochMu     sync.Mutex
	testEpochCached *string
)

// Real host process facts in the gateway status shape, the test binary cannot link pkg/gateway
func installTestGatewayStatusProbes() {
	GatewayStatusProbes = GatewayStatusProbeSet{
		PidExists:          proc.Exists,
		ProcessStartTime:   testProcessStartTime,
		StartTimesAgree:    testStartTimesAgree,
		InstantiationEpoch: testInstantiationEpoch,
	}
}

// Start tick from proc stat field 22, else process birth in centiseconds, nil when unreadable
func testProcessStartTime(pid int) any {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if end := strings.LastIndexByte(string(data), ')'); end >= 0 {
			if fields := strings.Fields(string(data)[end+1:]); len(fields) > 19 {
				if n, err := strconv.ParseInt(fields[19], 10, 64); err == nil {
					return n
				}
			}
		}
	}
	birth, err := proc.Birth(pid)
	if err != nil {
		return nil
	}
	return int64(pyint.Round(birth*100, 0))
}

// Whether every recorded fingerprint is positive and within a millisecond of current
func testStartTimesAgree(current any, recorded ...any) (bool, error) {
	cur, err := pyint.Float(current)
	if err != nil || !(cur > 0) {
		return false, err
	}
	for _, r := range recorded {
		f, err := pyint.Float(r)
		if err != nil {
			return false, err
		}
		if !(f > 0 && math.Abs(f-cur) <= 0.001) {
			return false, nil
		}
	}
	return true, nil
}

// Cached boot id and PID 1 start pair identifying this host instantiation
func testInstantiationEpoch() string {
	testEpochMu.Lock()
	defer testEpochMu.Unlock()
	if testEpochCached != nil {
		return *testEpochCached
	}
	bootID, pid1 := "", ""
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		bootID = pystr.Strip(string(b))
	}
	if b, err := os.ReadFile("/proc/1/stat"); err == nil {
		if i := strings.LastIndex(string(b), ")"); i >= 0 {
			if fields := strings.Fields(string(b)[i+1:]); len(fields) > 19 {
				pid1 = fields[19]
			}
		}
	}
	epoch := ""
	if bootID != "" || pid1 != "" {
		epoch = bootID + ":" + pid1
	}
	testEpochCached = &epoch
	return epoch
}

// Drops the cached instantiation epoch
func testInstantiationEpochCacheClear() {
	testEpochMu.Lock()
	testEpochCached = nil
	testEpochMu.Unlock()
}

type signalRecord struct {
	pid int
	sig syscall.Signal
}

// Signal hook appending every delivery instead of signalling
func recordSignals(into *[]signalRecord) SignalFunc {
	return func(pid int, sig syscall.Signal) error {
		*into = append(*into, signalRecord{pid, sig})
		return nil
	}
}

// Replaces a package seam for one test
func patch[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

// Scratch board connection with the crash grace pinned to zero
func fingerprintBoard(t *testing.T) state.SQLiteConn {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	t.Setenv("ICEBATH_KANBAN_CRASH_GRACE_SECONDS", "0")
	conn, err := Connect(context.Background(), filepath.Join(tmp, "kanban.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Claimed running task with a published worker pid, its claim and run start an hour in the past
func claimedRunning(t *testing.T, conn state.SQLiteConn, pid int, startedAt any, maxRuntime *int64) string {
	t.Helper()
	ctx := context.Background()
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "job", Assignee: sp("worker"), MaxRuntimeSeconds: maxRuntime})
	if _, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := setWorkerPid(ctx, conn, tid, pid, nil, nil); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Unix() - 3600
	err := WriteTxn(ctx, conn, false, func() error {
		if _, err := conn.ExecContext(ctx, "UPDATE tasks SET worker_started_at = ?, started_at = ?, claim_expires = ? WHERE id = ?", startedAt, old, old, tid); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "UPDATE task_runs SET started_at = ? WHERE id = (SELECT current_run_id FROM tasks WHERE id = ?)", old, tid)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tid
}

func mustEnforce(t *testing.T, conn state.SQLiteConn, fn SignalFunc) []string {
	t.Helper()
	out, err := EnforceMaxRuntime(context.Background(), conn, fn)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustRelease(t *testing.T, conn state.SQLiteConn, fn SignalFunc) int {
	t.Helper()
	n, err := ReleaseStaleClaims(context.Background(), conn, fn, nil)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecycledPidIsReclaimedWithoutBeingSignalled(t *testing.T) {
	conn := fingerprintBoard(t)
	var killed []signalRecord
	stranger := int64(1)
	tid := claimedRunning(t, conn, os.Getpid(), stranger, i64ptr(1))
	if workerAlive(os.Getpid(), stranger) {
		t.Fatal("a stranger fingerprint reads as our live worker")
	}
	if !slices.Contains(mustEnforce(t, conn, recordSignals(&killed)), tid) || len(killed) != 0 {
		t.Fatalf("runtime enforcement signalled a recycled pid %v", killed)
	}
	if task := mustTask(t, conn, tid); task.Status != "ready" || task.WorkerPID != nil {
		t.Fatalf("task %s pid %v", task.Status, task.WorkerPID)
	}
	tid2 := claimedRunning(t, conn, os.Getpid(), stranger, nil)
	if n := mustRelease(t, conn, recordSignals(&killed)); n != 1 || len(killed) != 0 {
		t.Fatalf("released %d signalled %v", n, killed)
	}
	if task := mustTask(t, conn, tid2); task.Status != "ready" {
		t.Fatal("stale claim kept", task.Status)
	}
}

func TestMatchingFingerprintKeepsTheLiveWorker(t *testing.T) {
	conn := fingerprintBoard(t)
	patch(t, &dispatchSleep, func(time.Duration) {})
	var killed []signalRecord
	own := testProcessStartTime(os.Getpid())
	tid := claimedRunning(t, conn, os.Getpid(), own, nil)
	if !workerAlive(os.Getpid(), testProcessStartTime(os.Getpid())) {
		t.Fatal("our own fingerprint reads as dead")
	}
	if n := mustRelease(t, conn, nil); n != 0 {
		t.Fatal("live worker reclaimed", n)
	}
	if task := mustTask(t, conn, tid); task.Status != "running" {
		t.Fatal("status", task.Status)
	}
	events, err := ListEvents(context.Background(), conn, tid)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(e *Event) bool { return e.Kind == "claim_extended" }) {
		t.Fatal("no claim_extended event")
	}
	mustExec(t, conn, "UPDATE tasks SET max_runtime_seconds = 1 WHERE id = '"+tid+"'")
	mustEnforce(t, conn, recordSignals(&killed))
	if len(killed) == 0 || killed[0] != (signalRecord{os.Getpid(), syscall.SIGTERM}) {
		t.Fatalf("timeout path signals %v", killed)
	}
}

func TestSamePidAndStartTickOnAnotherBootIsForeign(t *testing.T) {
	conn := fingerprintBoard(t)
	var killed []signalRecord
	live := processFingerprint(os.Getpid())
	if live == nil {
		t.Fatal("no fingerprint for this process")
	}
	_, tick, _ := strings.Cut(*live, "|")
	if tick != fmt.Sprint(testProcessStartTime(os.Getpid())) {
		t.Fatalf("fingerprint %s does not carry the start tick", *live)
	}
	tid := claimedRunning(t, conn, os.Getpid(), *live, i64ptr(1))
	if !workerAlive(os.Getpid(), *live) {
		t.Fatal("the live fingerprint reads as dead")
	}
	otherBoot := "deadbeef-boot:1|" + tick
	mustExec(t, conn, "UPDATE tasks SET worker_started_at = '"+otherBoot+"' WHERE id = '"+tid+"'")
	if workerAlive(os.Getpid(), otherBoot) {
		t.Fatal("another boot's fingerprint reads as our worker")
	}
	if !slices.Contains(mustEnforce(t, conn, recordSignals(&killed)), tid) || len(killed) != 0 {
		t.Fatalf("foreign worker signalled %v", killed)
	}
	if task := mustTask(t, conn, tid); task.Status != "ready" || task.WorkerPID != nil {
		t.Fatalf("task %s pid %v", task.Status, task.WorkerPID)
	}
	testInstantiationEpochCacheClear()
	if again := processFingerprint(os.Getpid()); again == nil || *again != *live {
		t.Fatal("fingerprint unstable within a boot")
	}
}

func TestUnverifiedFingerprintCaptureNeverAuthorizesASignal(t *testing.T) {
	conn := fingerprintBoard(t)
	ctx := context.Background()
	var killed []signalRecord
	sig := recordSignals(&killed)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "job", Assignee: sp("worker"), MaxRuntimeSeconds: i64ptr(1)})
	if _, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	real := GatewayStatusProbes.ProcessStartTime
	GatewayStatusProbes.ProcessStartTime = func(int) any { return nil }
	err := setWorkerPid(ctx, conn, tid, os.Getpid(), nil, nil)
	GatewayStatusProbes.ProcessStartTime = real
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRows(t, conn, "SELECT worker_started_at FROM tasks WHERE id = ?", tid)[0].Get("worker_started_at"); got != UnverifiedWorkerFingerprint {
		t.Fatal("failed capture stored", got)
	}
	old := time.Now().Unix() - 3600
	mustExec(t, conn, fmt.Sprintf("UPDATE tasks SET started_at = %d, claim_expires = %d WHERE id = '%s'", old, old, tid),
		fmt.Sprintf("UPDATE task_runs SET started_at = %d WHERE id = (SELECT current_run_id FROM tasks WHERE id = '%s')", old, tid))
	if got := mustEnforce(t, conn, sig); len(got) != 0 {
		t.Fatal("unverified worker timed out", got)
	}
	if n := mustRelease(t, conn, sig); n != 0 || len(killed) != 0 {
		t.Fatalf("released %d signalled %v", n, killed)
	}
	if task := mustTask(t, conn, tid); task.Status != "running" {
		t.Fatal("claim not held", task.Status)
	}
	ok, err := ReclaimTask(ctx, conn, tid, sp("operator"), sig)
	if err != nil || !ok || len(killed) != 0 {
		t.Fatalf("operator reclaim %v %v signalled %v", ok, err, killed)
	}
	tid2 := mustCreate(t, conn, CreateTaskOptions{Title: "job2", Assignee: sp("worker")})
	if _, err := ClaimTask(ctx, conn, tid2, nil, nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, fmt.Sprintf("UPDATE tasks SET worker_pid = %d, worker_started_at = '%s', claim_expires = %d WHERE id = '%s'",
		os.Getpid(), UnverifiedWorkerFingerprint, old, tid2))
	patch(t, &pidAlive, func(any) bool { return false })
	if n := mustRelease(t, conn, sig); n != 1 || len(killed) != 0 || mustTask(t, conn, tid2).Status != "ready" {
		t.Fatalf("dead unverified worker released %d signalled %v", n, killed)
	}
}

func TestMaxRuntimeTerminatesOverrunWorker(t *testing.T) {
	setupKanbanHome(t)
	t.Setenv("ICEBATH_KANBAN_CRASH_GRACE_SECONDS", "0")
	ctx := context.Background()
	var killed []signalRecord
	patch(t, &pidAlive, func(any) bool { return false })
	conn := mustConnect(t, "", nil)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "long job", Assignee: sp("worker"), MaxRuntimeSeconds: i64ptr(1)})
	if _, err := ClaimTask(ctx, conn, tid, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := setWorkerPid(ctx, conn, tid, os.Getpid(), nil, nil); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Unix() - 30
	mustExec(t, conn, fmt.Sprintf("UPDATE tasks SET started_at = %d WHERE id = '%s'", old, tid),
		fmt.Sprintf("UPDATE task_runs SET started_at = %d WHERE id = (SELECT current_run_id FROM tasks WHERE id = '%s')", old, tid))
	if !slices.Contains(mustEnforce(t, conn, recordSignals(&killed)), tid) || len(killed) == 0 || killed[0].pid != os.Getpid() {
		t.Fatalf("overrun worker not terminated %v", killed)
	}
	task := mustTask(t, conn, tid)
	if task.Status != "ready" || task.WorkerPID != nil || task.LastHeartbeatAt != nil {
		t.Fatalf("timed-out task should reset to ready, got %s", task.Status)
	}
	events, err := ListEvents(ctx, conn, tid)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(events, func(e *Event) bool { return e.Kind == "timed_out" })
	if i < 0 {
		t.Fatal("no timed_out event")
	}
	ep, _ := events[i].Payload.(*pyjson.Object)
	limit, elapsed := objGet(ep, "limit_seconds"), objGet(ep, "elapsed_seconds")
	if fmt.Sprint(limit) != "1" {
		t.Fatal("limit_seconds", limit)
	}
	if e, err := pyIntOf(elapsed); err != nil || e < 30 {
		t.Fatal("elapsed_seconds", elapsed)
	}
}

func TestPidAliveDetectsZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	setupKanbanHome(t)
	cmd := exec.Command("sleep", "3600")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer cmd.Wait()
	defer cmd.Process.Kill()
	if !pidAlive(pid) {
		t.Fatal("live non-zombie reads as dead")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	stateLine := ""
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "State:") {
			stateLine = l
			break
		}
	}
	if !strings.Contains(stateLine, "Z") {
		t.Fatalf("expected zombie, got %q", stateLine)
	}
	if pidAlive(pid) {
		t.Fatal("zombie reads as alive")
	}
}

func TestReclaimTaskResetsRunningToReady(t *testing.T) {
	setupKanbanHome(t)
	t.Setenv("ICEBATH_KANBAN_CRASH_GRACE_SECONDS", "0")
	ctx := context.Background()
	conn := mustConnect(t, "", nil)
	tid := mustCreate(t, conn, CreateTaskOptions{Title: "stuck", Assignee: sp("broken")})
	host, _, _ := strings.Cut(claimerID(), ":")
	lock := host + ":0123456789abcdef"
	future := time.Now().Unix() + 3600
	var killed []syscall.Signal
	alive := true
	signal := func(pid int, sig syscall.Signal) error {
		killed = append(killed, sig)
		if sig == syscall.SIGTERM {
			alive = false
		}
		return nil
	}
	patch(t, &pidAlive, func(any) bool { return alive })
	mustExec(t, conn, fmt.Sprintf("UPDATE tasks SET status='running', claim_lock='%s', claim_expires=%d, worker_pid=12345 WHERE id='%s'", lock, future, tid),
		fmt.Sprintf("INSERT INTO task_runs (task_id, status, claim_lock, claim_expires, worker_pid, started_at) VALUES ('%s', 'running', '%s', %d, 12345, %d)", tid, lock, future, time.Now().Unix()),
		fmt.Sprintf("UPDATE tasks SET current_run_id=(SELECT MAX(id) FROM task_runs) WHERE id='%s'", tid))
	if n := mustRelease(t, conn, nil); n != 0 {
		t.Fatal("unexpired claim released", n)
	}
	ok, err := ReclaimTask(ctx, conn, tid, sp("test reason"), signal)
	if err != nil || !ok {
		t.Fatal("reclaim", ok, err)
	}
	row := mustRows(t, conn, "SELECT status, claim_lock, worker_pid FROM tasks WHERE id=?", tid)[0]
	if row.Get("status") != "ready" || row.Get("claim_lock") != nil || row.Get("worker_pid") != nil {
		t.Fatal("claim not released", row.Get("status"), row.Get("claim_lock"), row.Get("worker_pid"))
	}
	evs := mustRows(t, conn, "SELECT payload FROM task_events WHERE task_id=? AND kind='reclaimed'", tid)
	if len(evs) != 1 {
		t.Fatal("reclaimed events", len(evs))
	}
	p := jsonDict(evs[0].Get("payload"))
	if objGet(p, "manual") != true || objGet(p, "reason") != "test reason" || objGet(p, "termination_attempted") != true || objGet(p, "terminated") != true {
		t.Fatal("reclaimed payload", p)
	}
}
