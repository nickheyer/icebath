package home

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"
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

// Creates and secures the scratch dir of h or the process home and starts its hourly detached prune
func EnsureScratchDir(h string) (string, error) { return scratchDir(h, true) }

// Scratch dir of h created and permissioned, the mkdir error returned beside the path
func scratchDir(h string, prune bool) (string, error) {
	base := h
	if base == "" {
		base = Process()
	}
	scratch := ScratchDir(base)
	err := os.MkdirAll(scratch, 0o777)
	if err == nil && runtime.GOOS != "windows" {
		ApplySecureDirPolicy(scratch, base)
	}
	if prune {
		pruneScratchDirOnce(scratch)
	}
	return scratch, err
}

// Starts at most one detached prune per process and per hour across processes through the stamp file
func pruneScratchDirOnce(scratch string) {
	if scratchPrunedOnce.Swap(true) {
		return
	}
	stamp := filepath.Join(scratch, scratchPruneStamp)
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
		root, _ = scratchDir("", false)
	}
	logFile := filepath.Join(filepath.Dir(filepath.Dir(root)), "logs", "scratch-prune.log")
	return PruneIdleEntries(root, maxIdleHours, map[string]bool{scratchPruneStamp: true}, logFile)
}

// Total apparent bytes of every non directory under the scratch dir, zero when unreadable
func ScratchDirUsageBytes(scratch string) int64 {
	root := scratch
	if root == "" {
		root, _ = scratchDir("", false)
	}
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if target, err := os.Stat(path); err == nil && target.IsDir() {
				return nil
			}
		}
		if fi, err := os.Lstat(path); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}
