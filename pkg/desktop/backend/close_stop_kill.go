package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// RuntimeLock is a gateway lock with the pids it names and whether a live process is known to hold it
type RuntimeLock struct {
	Path       string
	HolderPIDs []int
	Held       bool
}

// TaskkillFailure is a tree kill that failed for one pid
type TaskkillFailure struct {
	PID   int
	Error string
}

// LockError is an unheld lock whose removal failed
type LockError struct {
	Path  string
	Error string
}

// CloseStopKillDeps are the existing tree kill, the liveness probe and the lock removal
type CloseStopKillDeps struct {
	KillTree   func(pid int) error
	IsPidAlive func(pid int) bool
	ClearLock  func(path string) error
}

// CloseStopKillResult is what a Windows close or stop left behind
type CloseStopKillResult struct {
	TaskkillFailures []TaskkillFailure
	RemainingPIDs    []int
	ClearedLocks     []string
	RetainedLocks    []string
	LockErrors       []LockError
	LiveFailure      bool
}

// Whether a lock is held explicitly or by a live positive holder pid
func lockIsHeld(lock RuntimeLock, alive func(int) bool) bool {
	if lock.Held {
		return true
	}
	for _, pid := range lock.HolderPIDs {
		if pid > 0 && alive(pid) {
			return true
		}
	}
	return false
}

// Tree kills the owned pids, inventories the survivors and clears only locks no live holder owns
func FinishWindowsCloseStop(ownedPIDs []int, locks []RuntimeLock, deps CloseStopKillDeps) CloseStopKillResult {
	var pids []int
	for _, pid := range ownedPIDs {
		if pid > 0 {
			pids = append(pids, pid)
		}
	}
	result := CloseStopKillResult{TaskkillFailures: []TaskkillFailure{}, RemainingPIDs: []int{}, ClearedLocks: []string{}, RetainedLocks: []string{}, LockErrors: []LockError{}}
	for _, pid := range pids {
		if err := deps.KillTree(pid); err != nil {
			result.TaskkillFailures = append(result.TaskkillFailures, TaskkillFailure{PID: pid, Error: err.Error()})
		}
	}
	alive := map[int]bool{}
	for _, pid := range pids {
		if deps.IsPidAlive(pid) {
			result.RemainingPIDs = append(result.RemainingPIDs, pid)
			alive[pid] = true
		}
	}
	for _, lock := range locks {
		if lockIsHeld(lock, func(pid int) bool { return alive[pid] || deps.IsPidAlive(pid) }) {
			result.RetainedLocks = append(result.RetainedLocks, lock.Path)
			continue
		}
		if err := deps.ClearLock(lock.Path); err != nil {
			result.RetainedLocks = append(result.RetainedLocks, lock.Path)
			result.LockErrors = append(result.LockErrors, LockError{Path: lock.Path, Error: err.Error()})
			continue
		}
		result.ClearedLocks = append(result.ClearedLocks, lock.Path)
	}
	failed := map[int]bool{}
	for _, failure := range result.TaskkillFailures {
		failed[failure.PID] = true
	}
	result.LiveFailure = len(result.RemainingPIDs) > 0
	for _, pid := range pids {
		if failed[pid] && deps.IsPidAlive(pid) {
			result.LiveFailure = true
		}
	}
	return result
}

// Single line naming the failed kills, surviving pids and retained locks of a close or stop
func CloseStopFailureMessage(result CloseStopKillResult) string {
	parts := []string{"Windows close/stop did not finish cleanly."}
	kills := make([]string, len(result.TaskkillFailures))
	for i, failure := range result.TaskkillFailures {
		kills[i] = fmt.Sprintf("PID %d: %s", failure.PID, failure.Error)
	}
	if text := strings.Join(kills, "; "); text != "" {
		parts = append(parts, text)
	}
	if len(result.RemainingPIDs) > 0 {
		remaining := make([]string, len(result.RemainingPIDs))
		for i, pid := range result.RemainingPIDs {
			remaining[i] = strconv.Itoa(pid)
		}
		parts = append(parts, "still running: "+strings.Join(remaining, ", "))
	}
	if len(result.RetainedLocks) > 0 {
		parts = append(parts, "locks still held: "+strings.Join(result.RetainedLocks, ", "))
	}
	return strings.Join(parts, " ")
}

// Holder pid a gateway lock names as bare digits, JSON pid or JSON number, held when the file cannot be read
func holderPIDsFromLockFile(lockPath string) RuntimeLock {
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		switch hardening.NodeCode(err) {
		case "EBUSY", "EPERM", "EACCES":
			return RuntimeLock{Path: lockPath, HolderPIDs: []int{}, Held: true}
		}
		return RuntimeLock{Path: lockPath, HolderPIDs: []int{}}
	}
	text := strings.TrimSpace(utf8Lossy(raw))
	if text == "" {
		return RuntimeLock{Path: lockPath, HolderPIDs: []int{}}
	}
	pid, ok := jsNumberInteger(text)
	if !ok {
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) != nil {
			return RuntimeLock{Path: lockPath, HolderPIDs: []int{}}
		}
		if obj, isObj := parsed.(map[string]any); isObj {
			parsed = obj["pid"]
		}
		pid, ok = jsonNumberInteger(parsed)
	}
	if ok && pid > 0 {
		return RuntimeLock{Path: lockPath, HolderPIDs: []int{pid}}
	}
	return RuntimeLock{Path: lockPath, HolderPIDs: []int{}}
}

var (
	jsDecimalNumber = regexp.MustCompile(`^[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?$`)
	jsRadixNumber   = regexp.MustCompile(`^0([xXoObB])([0-9a-fA-F]+)$`)
)

// Number(text) of trimmed text as JavaScript reads it, false unless it is a finite integer
func jsNumberInteger(text string) (int, bool) {
	f, ok := jsNumberFloat(text)
	if !ok || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
}

// Number(value) of a decoded JSON pid when it is an integer
func jsonNumberInteger(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) <= math.MaxInt32 {
			return int(x), true
		}
	case string:
		if t := strings.TrimSpace(x); t != "" {
			return jsNumberInteger(t)
		}
		return 0, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	}
	return 0, false
}

// Gateway locks of the root home and every profile home, each read for its holder
func collectCloseStopLocks(icebathHome string) []RuntimeLock {
	roots := []string{icebathHome}
	if entries, err := os.ReadDir(filepath.Join(icebathHome, "profiles")); err == nil {
		for _, entry := range entries {
			roots = append(roots, filepath.Join(icebathHome, "profiles", entry.Name()))
		}
	}
	var locks []RuntimeLock
	for _, root := range roots {
		lockPath := filepath.Join(root, "gateway.lock")
		if info, err := os.Stat(lockPath); err != nil || !info.Mode().IsRegular() {
			continue
		}
		locks = append(locks, holderPIDsFromLockFile(lockPath))
	}
	return locks
}

// Tree kills the still running owned children on Windows and clears unheld locks, returning the live failure
func (s *Service) windowsCloseStopOwnedBackends(owned []int) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	result := FinishWindowsCloseStop(owned, collectCloseStopLocks(s.opts.IcebathHome), CloseStopKillDeps{
		KillTree:   TaskkillTree,
		IsPidAlive: IsPidAlive,
		ClearLock: func(lockPath string) error {
			if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return hardening.NodeErr(err)
			}
			return nil
		},
	})
	for _, failure := range result.TaskkillFailures {
		s.Log(fmt.Sprintf("[close-stop] taskkill PID %d failed: %s", failure.PID, failure.Error))
	}
	for _, failure := range result.LockErrors {
		s.Log(fmt.Sprintf("[close-stop] could not clear unheld lock %s: %s", failure.Path, failure.Error))
	}
	if len(result.ClearedLocks) > 0 {
		s.Log("[close-stop] cleared unheld lock(s): " + strings.Join(result.ClearedLocks, ", "))
	}
	if result.LiveFailure {
		return errors.New(CloseStopFailureMessage(result))
	}
	return nil
}
