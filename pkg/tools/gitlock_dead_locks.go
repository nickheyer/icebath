package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Drops index.lock once the killed update run behind it is proven gone, whatever the lock's age
func ReleaseDeadIndexLock(ctx context.Context, repoRoot string) (bool, error) {
	released, err := releaseKilledUpdateLocks(ctx, repoRoot, func(gitDir string) []string {
		return []string{filepath.Join(gitDir, "index.lock")}
	})
	return len(released) > 0, err
}

// Drops the shallow and ref locks a killed update's fetch left, returning the removed paths
func ReleaseDeadFetchLocks(ctx context.Context, repoRoot string) ([]string, error) {
	return releaseKilledUpdateLocks(ctx, repoRoot, func(gitDir string) []string {
		return append([]string{filepath.Join(gitDir, "shallow.lock")}, RefLockFiles(gitDir)...)
	})
}

// Releases the candidate locks a killed update owns while no git of any form works in the checkout
func releaseKilledUpdateLocks(ctx context.Context, root string, candidates func(gitDir string) []string) ([]string, error) {
	gitDir, err := pm.CheckoutGitDir(root)
	if err != nil {
		return nil, err
	}
	if pathExists(filepath.Join(gitDir, pm.InterruptedPullMarker)) {
		return []string{}, nil
	}
	var locks []string
	for _, lock := range candidates(gitDir) {
		if pathExists(lock) && killedUpdateOwns(ctx, lock) {
			locks = append(locks, lock)
		}
	}
	if len(locks) == 0 {
		return []string{}, nil
	}
	if gitlockWindows {
		works, err := windowsGitInCheckout(root)
		if err != nil {
			return nil, err
		}
		if works == nil || *works {
			return []string{}, nil
		}
	}
	released := []string{}
	for _, lock := range locks {
		ok, err := pm.ReleaseDeadLock(lock, root, true)
		if err != nil {
			return released, err
		}
		if !ok {
			break
		}
		released = append(released, lock)
	}
	return released, nil
}

// Epoch seconds of an ISO timestamp, a naive value read as local time, false when it does not parse
func receiptTime(value any) (float64, bool) {
	t, aware, err := pydatetime.ParseISOFormat(pyrepr.Str(value))
	if err != nil {
		return 0, false
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	}
	return pydatetime.Timestamp(t), true
}

// Reports a receipt outcome of running or interrupted
func receiptUnfinished(record *pyjson.Object) bool {
	outcome := record.Vals["outcome"]
	return pyrepr.Equal(outcome, "running") || pyrepr.Equal(outcome, "interrupted")
}

// Reports whether the last update run that started before lock was written died unfinished with its processes gone
func killedUpdateOwns(ctx context.Context, lock string) bool {
	info, err := os.Stat(lock)
	if err != nil {
		return false
	}
	written := statMtime(info)
	var records []*pyjson.Object
	for _, directory := range updaterecord.ReceiptDirs(ctx) {
		entries, err := os.ReadDir(directory)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !fnmatch.Fnmatch(entry.Name(), "update_*.json") {
				continue
			}
			loaded, err := updaterecord.LoadReceiptFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				continue
			}
			if record, ok := loaded.(*pyjson.Object); ok {
				records = append(records, record)
			}
		}
	}
	if latest := updaterecord.LatestReceiptObject(ctx); latest != nil {
		records = append(records, latest)
	}
	type run struct {
		started float64
		record  *pyjson.Object
	}
	runs := map[string]run{}
	var order []string
	for _, record := range records {
		started, ok := receiptTime(record.Vals["started_at"])
		if !ok || started > written {
			continue
		}
		key := fmt.Sprintf("%p", record)
		if id := record.Vals["update_id"]; pyrepr.Truthy(id) {
			key = pyrepr.Str(id)
		}
		held, seen := runs[key]
		if !seen {
			order = append(order, key)
		}
		if !seen || (receiptUnfinished(held.record) && !receiptUnfinished(record)) {
			runs[key] = run{started, record}
		}
	}
	if len(runs) == 0 {
		return false
	}
	last := runs[order[0]]
	for _, key := range order[1:] {
		if runs[key].started > last.started {
			last = runs[key]
		}
	}
	return receiptUnfinished(last.record) && !updaterecord.OwnerAlive(last.record)
}

// Process facts the Windows checkout scan reads, cwd empty and environ nil when unreadable
type gitlockProcess struct {
	PID       int
	Name      string
	Cwd       string
	Cmdline   []string
	Environ   map[string]string
	IsRunning func() bool
}

// Process table reader of the Windows checkout scan, swapped by tests
var gitlockProcessIter = gitlockProcesses

// Lists every process with the facts psutil process_iter yields, a vanished process skipped and unreadable facts absent
func gitlockProcesses() ([]gitlockProcess, error) {
	pids, err := proc.PIDs()
	if err != nil {
		return nil, err
	}
	out := make([]gitlockProcess, 0, len(pids))
	for _, pid := range pids {
		name, err := proc.Name(pid)
		if err != nil && !proc.Exists(pid) {
			continue
		}
		cwd, _ := proc.Cwd(pid)
		cmdline, _ := proc.Cmdline(pid)
		environ, _ := proc.Environ(pid)
		out = append(out, gitlockProcess{PID: pid, Name: name, Cwd: cwd, Cmdline: cmdline, Environ: environ, IsRunning: func() bool { return proc.Exists(pid) }})
	}
	return out, nil
}

// Whether any git works in root by cwd, path argument or GIT_DIR style variable, nil when the scan cannot tell
func windowsGitInCheckout(root string) (*bool, error) {
	gitDir, err := pm.CheckoutGitDir(root)
	if err != nil {
		return nil, err
	}
	places := pm.CheckoutPlaces(root, gitDir)
	procs, err := gitlockProcessIter()
	if err != nil {
		return nil, nil
	}
	self := os.Getpid()
	for _, p := range procs {
		if p.PID == self {
			continue
		}
		args := make([]string, 0, len(p.Cmdline))
		for _, a := range p.Cmdline {
			if a != "" {
				args = append(args, a)
			}
		}
		if pm.GitProgram(p.Name) == "" && !(len(args) > 0 && pm.GitProgram(args[0]) != "") {
			continue
		}
		named := args
		if len(named) == 0 {
			named = []string{p.Name}
		}
		if pm.GitNeverLocks(pm.GitSubcommandOf(named)) {
			continue
		}
		working := args
		if len(working) == 0 {
			working = []string{"git"}
			if p.Name != "" {
				working = []string{p.Name}
			}
		}
		if pm.GitWorksIn(working, p.Cwd, p.Environ, places) {
			yes := true
			return &yes, nil
		}
		if p.Cwd == "" && p.IsRunning() {
			return nil, nil
		}
	}
	no := false
	return &no, nil
}
