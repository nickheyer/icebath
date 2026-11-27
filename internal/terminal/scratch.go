package terminal

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Entries of the scratch folder idle this long are pruned
const scratchIdle = 24 * time.Hour

// Stamp of the scratch folder's last prune, pruned at most once an hour
const scratchStamp = ".last_prune"

// Scratch folder of the profile in its folder dir: where TMPDIR points commands, entries idle for a day pruned at most once an hour;
// empty when it can't be made
func Scratch(dir string) string {
	scratch := filepath.Join(dir, "cache", "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return ""
	}
	stamp := filepath.Join(scratch, scratchStamp)
	if st, err := os.Stat(stamp); err == nil && time.Since(st.ModTime()) < time.Hour {
		return scratch
	}
	if touch(stamp) == nil {
		go PruneIdle(scratch, scratchIdle, map[string]bool{scratchStamp: true})
	}
	return scratch
}

func touch(path string) error {
	now := time.Now()
	if os.Chtimes(path, now, now) == nil {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// Removes the entries of root nothing was written under for idle, sparing those named in skip and those a running process works in,
// answering how many went
func PruneIdle(root string, idle time.Duration, skip map[string]bool) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-idle)
	busy := workingDirs()
	removed := 0
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if skip[entry.Name()] || touchedSince(path, cutoff) || inside(busy, path) {
			continue
		}
		if os.RemoveAll(path) == nil {
			removed++
		}
	}
	return removed
}

// Whether path or anything under it was written at or after cutoff, an unreadable one counting as written
func touchedSince(path string, cutoff time.Time) bool {
	st, err := os.Lstat(path)
	if err != nil || !st.ModTime().Before(cutoff) {
		return true
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return false
	}
	for stack := []string{path}; len(stack) > 0; {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := os.ReadDir(current)
		if err != nil {
			return true
		}
		for _, entry := range entries {
			full := filepath.Join(current, entry.Name())
			st, err := os.Lstat(full)
			if err != nil {
				return true
			}
			if !st.ModTime().Before(cutoff) && !(linkCount(st) > 1 && st.Mode().IsRegular()) {
				return true
			}
			if st.IsDir() && st.Mode()&os.ModeSymlink == 0 {
				stack = append(stack, full)
			}
		}
	}
	return false
}

// Working folders of the running processes this host lets icebath see
func workingDirs() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd")); err == nil {
			dirs = append(dirs, cwd)
		}
	}
	return dirs
}

// Whether any of the folders is path or under it
func inside(dirs []string, path string) bool {
	for _, dir := range dirs {
		if dir == path || strings.HasPrefix(dir, path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Background job files a sandbox keeps in threes, judged by the newest of them
var jobFiles = regexp.MustCompile(`^(icebath_bg_[A-Za-z0-9_-]+)\.(log|pid|exit)$`)

// Removes what sat idle in a terminal temp folder for idle, a background job's log, pid and exit file judged together, answering
// how many went
func PruneTemp(root string, idle time.Duration) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-idle)
	newest := map[string]time.Time{}
	for _, entry := range entries {
		if m := jobFiles.FindStringSubmatch(entry.Name()); m != nil {
			if st, err := os.Lstat(filepath.Join(root, entry.Name())); err == nil && st.ModTime().After(newest[m[1]]) {
				newest[m[1]] = st.ModTime()
			}
		}
	}
	removed := 0
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if m := jobFiles.FindStringSubmatch(entry.Name()); m != nil {
			if !newest[m[1]].Before(cutoff) {
				continue
			}
		} else if touchedSince(path, cutoff) {
			continue
		}
		os.RemoveAll(path)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			removed++
		}
	}
	return removed
}
