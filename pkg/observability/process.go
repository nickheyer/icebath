package observability

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const (
	// MarkerDirname is the per profile directory of process exit markers
	MarkerDirname   = "process_markers"
	reportingSuffix = ".reporting"
	// ExitFlushTimeout bounds the exit flush so a wedged flush costs its pending rows, never the exit
	ExitFlushTimeout = 3 * time.Second
	drainMargin      = 100 * time.Millisecond
)

// Hooks pkg/gateway installs for its status module
var (
	// ProcessStartTimeHook returns the canonical start time of a pid, gateway.status get_process_start_time
	ProcessStartTimeHook func(pid int) any
	// RuntimeStatusPidIsLiveHook reports whether a status record's pid is the live process it names
	RuntimeStatusPidIsLiveHook func(record *pyjson.Object) bool
)

// processState is this process's marker, its kind, path and current record
type processState struct {
	mu     sync.Mutex
	kind   string
	path   string
	record *pyjson.Object
}

var procState processState

// Starts the dead marker reporter off the caller's goroutine
var spawnReporter = func(run func()) { go run() }

// Python exception classes in crash class order, most specific first
var crashClassKinds = [...][2]string{{"MemoryError", "memory_error"}, {"ImportError", "import_error"}, {"OSError", "os_error"}, {"RuntimeError", "runtime_error"}}

// Crash class of a recovered panic value by its exception class chain, Go OS and runtime errors by their type
func CrashClass(v any) string {
	err, ok := v.(error)
	if !ok {
		return "other"
	}
	mro := pyExceptionMRO(err)
	for _, kind := range crashClassKinds {
		if slices.Contains(mro, kind[0]) {
			return kind[1]
		}
	}
	var rtErr runtime.Error
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var sysErr *os.SyscallError
	var errno syscall.Errno
	switch {
	case errors.As(err, &pathErr), errors.As(err, &linkErr), errors.As(err, &sysErr), errors.As(err, &errno):
		return "os_error"
	case errors.As(err, &rtErr):
		return "runtime_error"
	}
	return "other"
}

// Exit marker directory of a profile home
func processMarkersDir(h string) string {
	return filepath.Join(h, "telemetry", "shared_metrics", MarkerDirname)
}

// Writes a marker through a sibling temporary file and an atomic replace
func writeMarker(path string, record *pyjson.Object) error {
	text, err := pyjson.Dumps(record)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Copy of a marker record with one key set, keeping key order
func withKey(record *pyjson.Object, key string, value any) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range record.Keys {
		out.Set(k, record.Vals[k])
	}
	out.Set(key, value)
	return out
}

// Restamps this process's running marker with how it is ending, a no op when unmarked
func StampExit(exitKind, crash string) {
	procState.mu.Lock()
	defer procState.mu.Unlock()
	record := procState.record
	if record == nil || procState.path == "" {
		return
	}
	if state, _ := record.Get("state"); state != "running" {
		return
	}
	record = withKey(withKey(record, "state", exitKind), "crash_class", crash)
	procState.record = record
	if err := writeMarker(procState.path, record); err != nil {
		slog.Debug("Process exit marker not stamped", "error", err)
	}
}

// Joins pending feature disabled recorders then stamps a clean exit, the hook every entrypoint defers
func EndProcess() {
	featureDisabledWG.Wait()
	drainDeferringStoresAtExit(context.Background())
	StampExit("clean", "none")
}

// Stamps a crash with its class for a panic leaving the entrypoint, then lets the panic continue
func CrashGuard() {
	if r := recover(); r != nil {
		StampExit("crash", CrashClass(r))
		panic(r)
	}
}

// Runs the shared metrics exit flush before a hard exit within the timeout, true when it finished in time
func FlushBeforeHardExit(ctx context.Context, timeout time.Duration) bool {
	if timeout < 0 {
		timeout = 0
	}
	deadline := time.Now().Add(timeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				slog.Debug("Shared-metrics exit flush failed", "error", r)
			}
		}()
		shutdownRuntimes(ctx)
		drainDeferringStores(ctx, deadline.Add(-drainMargin))
	}()
	select {
	case <-done:
		return true
	case <-time.After(time.Until(deadline)):
		slog.Warn(fmt.Sprintf("Shared-metrics exit flush still running after %.1fs; exiting without it", timeout.Seconds()))
		return false
	}
}

// Drops parked update and installer receipts so an opt out never leaves rows a later opt in would count
func PurgePendingReceipts(ctx context.Context, h string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Pending shared-metrics receipts not purged", "error", r)
		}
	}()
	purgePendingUpdates(h)
	purgePendingInstalls(ctx, h)
}

// Marks this process as a running kind and reports dead predecessors once per process
func BeginProcess(ctx context.Context, kind string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Process exit marker not started", "error", r)
		}
	}()
	procState.mu.Lock()
	if procState.kind != "" {
		procState.mu.Unlock()
		return
	}
	procState.kind = kind
	procState.mu.Unlock()
	h := home.Dir(ctx)
	if !collectionOn(ctx) {
		PurgePendingReceipts(ctx, h)
		latches := filepath.Join(h, "telemetry", "shared_metrics", OnboardingLatchDirname)
		for _, dir := range []string{processMarkersDir(h), setupMarkersDir(h), latches} {
			_ = os.RemoveAll(dir)
		}
		return
	}
	dir := processMarkersDir(h)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Debug("Process exit marker not started", "error", err)
		return
	}
	pid := os.Getpid()
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.json", kind, pid))
	record := pyjson.NewObject()
	record.Set("kind", kind)
	record.Set("pid", int64(pid))
	record.Set("start_time", ProcessStartTimeHook(pid))
	record.Set("state", "running")
	procState.mu.Lock()
	err := writeMarker(path, record)
	if err == nil {
		procState.path, procState.record = path, record
	}
	procState.mu.Unlock()
	if err != nil {
		slog.Debug("Process exit marker not started", "error", err)
		return
	}
	reportCtx := context.WithoutCancel(home.WithOverride(ctx, h))
	spawnReporter(func() { reportDeadMarkers(reportCtx, h, path) })
}

// Kind this process marked itself with, other before BeginProcess
func CurrentProcessKind() string {
	procState.mu.Lock()
	defer procState.mu.Unlock()
	if procState.kind == "" {
		return "other"
	}
	return procState.kind
}

// Status record view of a pid alone
func pidRecord(pid int64) *pyjson.Object {
	r := pyjson.NewObject()
	r.Set("pid", pid)
	return r
}

// Reports whether a pid passes the gateway's canonical liveness check
func pidRecordIsLive(record *pyjson.Object) bool {
	return RuntimeStatusPidIsLiveHook(record)
}

// Reports whether the reporter whose pid a claimed file names is still alive
func claimerAlive(path string) bool {
	parts := strings.Split(filepath.Base(path), ".")
	if len(parts) < 2 {
		return false
	}
	claimer, err := strconv.ParseInt(strings.TrimSpace(parts[len(parts)-2]), 10, 64)
	if err != nil {
		return false
	}
	return pidRecordIsLive(pidRecord(claimer))
}

// Reads a JSON record file with a BOM tolerant decode, false when unreadable or not an object
func readRecord(path string) (*pyjson.Object, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	text := strings.TrimPrefix(string(raw), "\xef\xbb\xbf")
	if !utf8.ValidString(text) {
		return nil, false
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return nil, false
	}
	obj, ok := decoded.(*pyjson.Object)
	return obj, ok
}

// Renames a reportable marker so exactly one reporter owns it, empty when not reportable
func claim(path string) string {
	reclaim := strings.HasSuffix(filepath.Base(path), reportingSuffix)
	if reclaim && claimerAlive(path) {
		return ""
	}
	record, ok := readRecord(path)
	if !ok {
		return ""
	}
	if !reclaim {
		if state, _ := record.Get("state"); state == "running" && pidRecordIsLive(record) {
			return ""
		}
	}
	base := strings.SplitN(filepath.Base(path), ".json", 2)[0]
	claimed := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s.json.%d%s", base, os.Getpid(), reportingSuffix))
	if err := os.Rename(path, claimed); err != nil {
		return ""
	}
	return claimed
}

// Bounded exit row of one dead process marker
func ProcessExitFields(record *pyjson.Object) map[string]string {
	state, _ := record.Get("state")
	exitKind, isText := state.(string)
	if state == "running" || !isText || !ProcessExitKinds.Has(exitKind) {
		exitKind = "killed"
	}
	var crash any = "none"
	if exitKind == "crash" {
		crash, _ = record.Get("crash_class")
	}
	kind, _ := record.Get("kind")
	out := map[string]string{"crash_class": "other", "exit_kind": exitKind, "process_kind": "other"}
	if inSet(crash, CrashClasses) {
		out["crash_class"] = crash.(string)
	}
	if inSet(kind, ProcessKinds) {
		out["process_kind"] = kind.(string)
	}
	return out
}

// Deletes a claimed file once its row is saved, otherwise hands it back for the next start
func SettleClaim(claimed, original string, saved bool) {
	var err error
	if saved {
		err = os.Remove(claimed)
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	} else {
		err = os.Rename(claimed, original)
	}
	if err != nil {
		slog.Debug("Claimed shared-metrics file not settled", "error", err)
	}
}

// Reports every dead marker of the profile, then pending updates, abandoned setups and pending installs
func reportDeadMarkers(ctx context.Context, h, own string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Dead process markers not reported", "error", r)
		}
	}()
	entries, err := os.ReadDir(processMarkersDir(h))
	if err != nil {
		slog.Debug("Dead process markers not reported", "error", err)
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(processMarkersDir(h), name)
		if path == own || strings.HasPrefix(name, ".") || !strings.Contains(name, ".json") {
			continue
		}
		claimed := claim(path)
		if claimed == "" {
			continue
		}
		var rows []Mark
		if record, ok := readRecord(claimed); ok {
			rows = []Mark{{Name: ProcessExitMark, Data: ProcessExitFields(record)}}
		}
		SettleClaim(claimed, path, EmitSaved(ctx, rows) == len(rows))
	}
	reportPendingUpdates(ctx)
	ReportAbandonedSetups(ctx, h)
	reportPendingInstalls(ctx, h)
}

// TurnExitState is the per agent turn latch both turn watchdogs share
type TurnExitState struct {
	mu           sync.Mutex
	home         string
	abortCounted bool
}

// Arms a turn with its owning home and a fresh once per turn watchdog latch
func ArmTurn(ctx context.Context, st *TurnExitState) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.home = home.Dir(ctx)
	st.abortCounted = false
}

// Records one watchdog exit row per killed turn in the turn's own profile, nothing for an unarmed turn
func RecordWatchdogTurnAbort(ctx context.Context, st *TurnExitState) {
	st.mu.Lock()
	h := st.home
	if h == "" || st.abortCounted {
		st.mu.Unlock()
		return
	}
	st.abortCounted = true
	st.mu.Unlock()
	emit(home.WithOverride(ctx, h), ProcessExitMark, func() map[string]string {
		return map[string]string{"crash_class": "none", "exit_kind": "watchdog", "process_kind": CurrentProcessKind()}
	})
}
