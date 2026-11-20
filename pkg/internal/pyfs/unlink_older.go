package pyfs

import (
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
)

// Deletes regular files matching the pattern whose mtime is more than the age before now and returns the count
func UnlinkFilesOlderThan(dir, pattern string, maxAgeSeconds float64, now *float64) int {
	var current float64
	if now != nil {
		current = *now
	} else {
		current = float64(time.Now().UnixNano()) / 1e9
	}
	cutoff := current - maxAgeSeconds
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !fnmatch.Fnmatch(e.Name(), pattern) {
			continue
		}
		if unlinkIfOlder(filepath.Join(dir, e.Name()), cutoff) {
			removed++
		}
	}
	return removed
}

// Removes one regular file whose mtime precedes the cutoff, any OS error skipping it
func unlinkIfOlder(path string, cutoff float64) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if float64(st.ModTime().UnixNano())/1e9 >= cutoff {
		return false
	}
	return unlinkSweepEntry(path) == nil
}

// Unlink of one swept file, swapped by tests
var unlinkSweepEntry = os.Remove
