package home

import (
	"context"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

const (
	// Env var holding the scratch dir icebath itself exported into the temp vars
	ScratchDirMarkerEnv = "ICEBATH_SCRATCH_DIR"
	// Hours without a write anywhere in a scratch entry before the prune removes it
	ScratchMaxIdleHours = 24
	scratchPruneStamp   = ".last_prune"
	scratchPruneEvery   = 3600 * time.Second
)

// Temp vars pointed at the scratch dir for every icebath process and child
var ScratchTmpEnvVars = []string{"TMPDIR", "TMP", "TEMP"}

// Once per process latch and the detached prune launcher, swapped by tests that record spawns
var (
	scratchPrunedOnce atomic.Bool
	spawnPrune        = spawnPruneChild
)

// Records detached prune launches instead of starting them with the once latch rearmed, returning the record and its restore
func CaptureScratchPruneSpawns() (*[]string, func()) {
	var spawned []string
	prev := spawnPrune
	spawnPrune = func(scratch string) { spawned = append(spawned, scratch) }
	scratchPrunedOnce.Store(false)
	return &spawned, func() { spawnPrune = prev; scratchPrunedOnce.Store(false) }
}

// Rearms the once per process prune latch the way a fresh process starts
func ResetScratchPruneLatch() { scratchPrunedOnce.Store(false) }

// Creates and secures the scratch dir of h or the active home and starts its hourly detached prune
func EnsureScratchDir(h string) string { return scratchDir(h, true) }

// Scratch dir of h, created and permissioned where the filesystem allows, a creation failure ignored
func scratchDir(h string, prune bool) string {
	base := h
	if base == "" {
		base = Dir(context.Background())
	}
	scratch := ScratchDir(base)
	if pathMkdir(scratch, 0o777, true, true) == nil && runtime.GOOS != "windows" {
		ApplySecureDirPolicy(scratch, base)
	}
	if prune {
		pruneScratchDirOnce(scratch)
	}
	return scratch
}

// Starts at most one detached prune per process and per hour across processes through the stamp file
func pruneScratchDirOnce(scratch string) {
	if scratchPrunedOnce.Swap(true) {
		return
	}
	stamp := pypath.PathJoin(scratch, scratchPruneStamp)
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < scratchPruneEvery {
		return
	}
	if touchFile(stamp) != nil {
		return
	}
	spawnPrune(scratch)
}

// Path.touch with exist_ok, mtime set to now or the file created
func touchFile(path string) error {
	now := time.Now()
	if os.Chtimes(path, now, now) == nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	return f.Close()
}

// Removes scratch entries idle for maxIdleHours after reaping what runs in them, logging to logs/scratch-prune.log
func PruneScratchDir(scratch string, maxIdleHours float64) (int, error) {
	root := scratch
	if root == "" {
		root = scratchDir("", false)
	}
	logFile := pypath.PathJoin(pypath.PathParent(pypath.PathParent(root)), "logs", "scratch-prune.log")
	return PruneIdleEntries(root, maxIdleHours, map[string]bool{scratchPruneStamp: true}, logFile)
}

// Total lstat bytes of every non directory os.walk lists under the scratch dir, zero when unreadable
func ScratchDirUsageBytes(scratch string) int64 {
	root := scratch
	if root == "" {
		root = scratchDir("", false)
	}
	var total int64
	stack := []string{root}
	for len(stack) > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		f, err := os.Open(top)
		if err != nil {
			continue
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			continue
		}
		var dirs []string
		for _, e := range entries {
			p := pypath.Join(top, e.Name())
			isDir := e.IsDir()
			if e.Type()&os.ModeSymlink != 0 {
				st, serr := os.Stat(p)
				isDir = serr == nil && st.IsDir()
			}
			if isDir {
				dirs = append(dirs, p)
				continue
			}
			if fi, err := os.Lstat(p); err == nil {
				total += fi.Size()
			}
		}
		for i := len(dirs) - 1; i >= 0; i-- {
			if st, err := os.Lstat(dirs[i]); err == nil && st.Mode()&os.ModeSymlink != 0 {
				continue
			}
			stack = append(stack, dirs[i])
		}
	}
	return total
}
