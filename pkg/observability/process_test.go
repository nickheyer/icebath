package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// importFailure is a raised ImportError
type importFailure struct{}

func (importFailure) Error() string { return "x" }
func (importFailure) PyExceptionMRO() []string {
	return []string{"ImportError", "Exception", "BaseException"}
}

func TestDeadMarkersReportKilledOrCrashAndLiveOnesWait(t *testing.T) {
	m := captureMarks(t)
	dir := processMarkersDir(m.home)
	mustMkdir(t, dir)
	dead := deadPID(t)
	killed := filepath.Join(dir, fmt.Sprintf("cli-%d.json", dead))
	writeJSON(t, killed, map[string]any{"kind": "cli", "pid": dead, "start_time": nil, "state": "running"})
	crashed := filepath.Join(dir, "tui-2.json")
	writeJSON(t, crashed, map[string]any{"kind": "tui", "pid": deadPID(t), "start_time": nil, "state": "crash", "crash_class": "import_error"})
	parent := os.Getppid()
	live := filepath.Join(dir, fmt.Sprintf("gateway-%d.json", parent))
	writeJSON(t, live, map[string]any{"kind": "gateway", "pid": parent, "start_time": nil, "state": "running"})

	ctx := context.Background()
	reportDeadMarkers(ctx, m.home, filepath.Join(dir, "self.json"))
	reportDeadMarkers(ctx, m.home, filepath.Join(dir, "self.json"))

	var rows []string
	for _, r := range m.snapshot() {
		rows = append(rows, r.Data["exit_kind"]+":"+r.Data["crash_class"]+":"+r.Data["process_kind"])
	}
	sort.Strings(rows)
	if fmt.Sprint(rows) != "[crash:import_error:tui killed:none:cli]" {
		t.Fatalf("rows %v", rows)
	}
	if !exists(live) || exists(killed) || exists(crashed) {
		t.Fatalf("live %v killed %v crashed %v", exists(live), exists(killed), exists(crashed))
	}
	assertValid(t, m.snapshot())
}

func TestTurnWatchdogAbortCountsOncePerTurnInTheTurnProfile(t *testing.T) {
	m := captureMarks(t)
	st := &TurnExitState{}
	RecordWatchdogTurnAbort(context.Background(), st)
	ArmTurn(context.Background(), st)
	t.Setenv(home.EnvHome, filepath.Join(t.TempDir(), "other"))
	var seen []string
	recordProcessMark = func(ctx context.Context, mark string, data map[string]string) {
		m.add(Mark{Name: mark, Data: data})
		seen = append(seen, home.Dir(ctx))
	}
	RecordWatchdogTurnAbort(context.Background(), st)
	RecordWatchdogTurnAbort(context.Background(), st)
	rows := m.snapshot()
	if len(rows) != 1 || rows[0].Data["exit_kind"] != "watchdog" {
		t.Fatalf("rows %v", rows)
	}
	if len(seen) != 1 || seen[0] != m.home {
		t.Fatalf("seen %v want %s", seen, m.home)
	}
	assertValid(t, rows)
}

func TestBeginProcessMarksThenStampsCleanAndCrash(t *testing.T) {
	m := captureMarks(t)
	resetProcessState(t)
	spawnReporter = func(func()) {}
	BeginProcess(context.Background(), "cli")
	marker := filepath.Join(processMarkersDir(m.home), fmt.Sprintf("cli-%d.json", os.Getpid()))
	if readState(t, marker)["state"] != "running" {
		t.Fatalf("marker %v", readState(t, marker))
	}
	func() {
		defer func() { _ = recover() }()
		defer CrashGuard()
		panic(importFailure{})
	}()
	EndProcess()
	record := readState(t, marker)
	if record["state"] != "crash" || record["crash_class"] != "import_error" {
		t.Fatalf("record %v", record)
	}
}

func TestOptedOutStartPurgesMarkersLeftWhileOptedIn(t *testing.T) {
	m := captureMarks(t)
	left := []string{
		filepath.Join(processMarkersDir(m.home), "cli-1.json"),
		filepath.Join(setupMarkersDir(m.home), "tui-1-1.json"),
		filepath.Join(m.home, "telemetry", "shared_metrics", OnboardingLatchDirname, "provider.completed"),
	}
	for _, marker := range left {
		mustMkdir(t, filepath.Dir(marker))
		writeJSON(t, marker, map[string]any{"pid": 1, "started_at": 0, "surface": "tui", "provider": "xai", "state": "running"})
	}
	resetProcessState(t)
	m.setPolicy(false)
	BeginProcess(context.Background(), "cli")
	for _, marker := range left {
		if exists(filepath.Dir(marker)) {
			t.Fatalf("%s survived", filepath.Dir(marker))
		}
	}
}

func TestAMarkerTheStoreDidNotSaveIsKeptForTheNextStart(t *testing.T) {
	m := captureMarks(t)
	dir := processMarkersDir(m.home)
	mustMkdir(t, dir)
	marker := filepath.Join(dir, "gateway-2.json")
	writeJSON(t, marker, map[string]any{"kind": "gateway", "pid": deadPID(t), "start_time": nil, "state": "clean"})
	m.setSaves(false)
	reportDeadMarkers(context.Background(), m.home, filepath.Join(dir, "self.json"))
	if !exists(marker) || len(m.snapshot()) != 0 {
		t.Fatalf("marker kept %v rows %v", exists(marker), m.snapshot())
	}
	m.setSaves(true)
	reportDeadMarkers(context.Background(), m.home, filepath.Join(dir, "self.json"))
	rows := m.snapshot()
	if exists(marker) || len(rows) != 1 || rows[0].Data["exit_kind"] != "clean" {
		t.Fatalf("marker %v rows %v", exists(marker), rows)
	}
}

func TestProcessExitFieldsBoundUnknownValues(t *testing.T) {
	record := pyjson.NewObject()
	record.Set("kind", "mystery")
	record.Set("state", "crash")
	record.Set("crash_class", "SegfaultError")
	got := ProcessExitFields(record)
	if got["exit_kind"] != "crash" || got["crash_class"] != "other" || got["process_kind"] != "other" {
		t.Fatalf("fields %v", got)
	}
	if !CounterDimensionsAreValid(ProcessExitMetric, anyDims(got)) {
		t.Fatalf("invalid %v", got)
	}
}

// markCapture is the runtime boundary double every process and setup test records through
type markCapture struct {
	mu    sync.Mutex
	rows  []Mark
	on    bool
	saves bool
	home  string
}

func (m *markCapture) add(rows ...Mark) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, rows...)
}

func (m *markCapture) snapshot() []Mark {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Mark(nil), m.rows...)
}

func (m *markCapture) setPolicy(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.on = on
}

func (m *markCapture) setSaves(saves bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves = saves
}

// Captures decision marks at the runtime boundary with collection on and the store saving
func captureMarks(t *testing.T) *markCapture {
	t.Helper()
	m := &markCapture{on: true, saves: true, home: filepath.Join(t.TempDir(), "home")}
	t.Setenv(home.EnvHome, m.home)
	saved := []any{relayEnabled, recordProcessMark, recordProcessMarksSaved, collectionOn, reportPendingUpdates,
		reportPendingInstalls, purgePendingUpdates, purgePendingInstalls, spawnReporter, RuntimeStatusPidIsLiveHook}
	t.Cleanup(func() {
		relayEnabled = saved[0].(func(context.Context) bool)
		recordProcessMark = saved[1].(func(context.Context, string, map[string]string))
		recordProcessMarksSaved = saved[2].(func(context.Context, []Mark) int)
		collectionOn = saved[3].(func(context.Context) bool)
		reportPendingUpdates = saved[4].(func(context.Context))
		reportPendingInstalls = saved[5].(func(context.Context, string))
		purgePendingUpdates = saved[6].(func(string))
		purgePendingInstalls = saved[7].(func(context.Context, string))
		spawnReporter = saved[8].(func(func()))
		RuntimeStatusPidIsLiveHook = saved[9].(func(*pyjson.Object) bool)
	})
	policy := func(context.Context) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.on
	}
	relayEnabled, collectionOn = policy, policy
	recordProcessMark = func(_ context.Context, mark string, data map[string]string) { m.add(Mark{Name: mark, Data: data}) }
	recordProcessMarksSaved = func(_ context.Context, rows []Mark) int {
		m.mu.Lock()
		saves := m.saves
		m.mu.Unlock()
		if !saves {
			return 0
		}
		m.add(rows...)
		return len(rows)
	}
	reportPendingUpdates = func(context.Context) {}
	reportPendingInstalls = func(context.Context, string) {}
	purgePendingUpdates = func(string) {}
	purgePendingInstalls = func(context.Context, string) {}
	RuntimeStatusPidIsLiveHook = pidAlive
	return m
}

// Liveness of a status record's pid through the portable process probe the gateway check uses
func pidAlive(record *pyjson.Object) bool {
	v, _ := record.Get("pid")
	n, ok := pyInt(v)
	if !ok || !n.IsInt64() || n.Int64() <= 0 {
		return false
	}
	return proc.Exists(int(n.Int64()))
}

// Clears this process's marker state for a fresh BeginProcess
func resetProcessState(t *testing.T) {
	t.Helper()
	procState.mu.Lock()
	procState.kind, procState.path, procState.record = "", "", nil
	procState.mu.Unlock()
	t.Cleanup(func() {
		procState.mu.Lock()
		procState.kind, procState.path, procState.record = "", "", nil
		procState.mu.Unlock()
	})
}

// A pid whose process has exited and been reaped
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.ProcessState.Pid()
}

func assertValid(t *testing.T, rows []Mark) {
	t.Helper()
	for _, r := range rows {
		if !CounterDimensionsAreValid(decisionMarkMetrics[r.Name], anyDims(r.Data)) {
			t.Fatalf("invalid row %s %v", r.Name, r.Data)
		}
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readState(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
