package pm

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Reports a checkout this process can see was deleted, under the data root or in a surviving worktrees dir
func provablyDeleted(root, dataRoot string) bool {
	if _, err := os.Stat(root); err == nil {
		return false
	}
	parent := filepath.Dir(root)
	if isRelativeTo(root, dataRoot) {
		return true
	}
	st, err := os.Stat(parent)
	return filepath.Base(parent) == ".worktrees" && err == nil && st.IsDir()
}

// Newest mtime among what a sync or a launch moves, never the state dir itself
func lastUsed(state string) (float64, error) {
	paths := []string{filepath.Join(state, "inputs"), filepath.Join(state, "facts.json")}
	for _, generations := range []string{filepath.Join(state, "environments"), filepath.Join(state, "pm-runtime", "generations")} {
		if st, err := os.Stat(generations); err == nil && st.IsDir() {
			entries, err := os.ReadDir(generations)
			if err != nil {
				return 0, err
			}
			for _, e := range entries {
				paths = append(paths, filepath.Join(generations, e.Name(), ".leases"))
			}
		}
	}
	newest := 0.0
	for _, path := range paths {
		if st, err := os.Lstat(path); err == nil {
			newest = max(newest, float64(st.ModTime().UnixNano())/1e9)
		}
	}
	return newest, nil
}

// Store wide retention in seconds from the owning data root's config, ok false when reclaiming is off
func retentionSeconds(dataRoot string) (float64, bool, error) {
	tree, err := config.Load(dataRoot, config.NewSecrets(dataRoot).Lookup())
	if err != nil {
		return 0, false, err
	}
	value, found := tree.Lookup("worktrees.install_state_retention_days")
	if !found {
		value = 14
	}
	var days float64
	switch v := value.(type) {
	case int:
		days = float64(v)
	case int64:
		days = float64(v)
	case uint64:
		days = float64(v)
	case float64:
		days = v
	case bool:
		if v {
			days = 1
		}
	default:
		return 0, false, nil
	}
	if days > 0 {
		return days * 86400, true, nil
	}
	return 0, false, nil
}

// Reports a state dir whose recorded checkout is provably gone or missing and unused past retention
func isOrphan(state, dataRoot string, retention float64, retain bool) (bool, error) {
	record := filepath.Join(state, "inputs", ".project-root")
	if st, err := os.Stat(state); err != nil || !st.IsDir() || !isRegularFile(record) {
		return false, nil
	}
	data, err := os.ReadFile(record)
	if err != nil {
		return false, nil
	}
	if err := strictUTF8(trimBOM(data)); err != nil {
		return false, err
	}
	root := pyStrip(string(trimBOM(data)))
	if root == "" {
		return false, nil
	}
	if _, err := os.Stat(root); err == nil {
		return false, nil
	}
	if provablyDeleted(root, dataRoot) {
		return true, nil
	}
	if !retain {
		return false, nil
	}
	used, err := lastUsed(state)
	if err != nil {
		return false, err
	}
	return float64(time.Now().UnixNano())/1e9-used > retention, nil
}

// Sorted state dirs under installs whose checkout is gone or missing and unused past retention
func OrphanInstallStates(installs string) ([]string, error) {
	if st, err := os.Stat(installs); err != nil || !st.IsDir() {
		return nil, nil
	}
	dataRoot := filepath.Dir(resolvePath(installs))
	retention, retain, err := retentionSeconds(filepath.Dir(installs))
	if err != nil {
		return nil, err
	}
	states, err := sortedChildren(installs)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, state := range states {
		orphan, err := isOrphan(state, dataRoot, retention, retain)
		if err != nil {
			return nil, err
		}
		if orphan {
			out = append(out, state)
		}
	}
	return out, nil
}

// Child paths of a directory sorted by name
func sortedChildren(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// Takes a state's install lock without waiting, a nil release while another process holds it or it cannot open
var installLock = func(state string) func() {
	f, err := LockPath(filepath.Join(state, ".install.lock"), 0, false)
	if err != nil || f == nil {
		return nil
	}
	return func() { f.Close() }
}

// Removes a tree of a reclaimed state ignoring failures
var removeStateTree = removeTreeQuiet

// Unlinks one file of a reclaimed state
var unlinkStateFile = os.Remove

// Reports whether a dependency environment or PM runtime generation lease is held
var leased = func(state string) (bool, error) {
	for _, generations := range []string{filepath.Join(state, "environments"), filepath.Join(state, "pm-runtime", "generations")} {
		st, err := os.Stat(generations)
		if err != nil || !st.IsDir() {
			continue
		}
		entries, err := os.ReadDir(generations)
		if err != nil {
			return false, err
		}
		for _, e := range entries {
			generation := filepath.Join(generations, e.Name())
			if gst, err := os.Stat(generation); err != nil || !gst.IsDir() {
				continue
			}
			if lst, err := os.Stat(filepath.Join(generation, ".leases")); err != nil || !lst.IsDir() {
				continue
			}
			held, err := LeasesHeld(generation)
			if err != nil {
				return false, err
			}
			if held {
				return true, nil
			}
		}
	}
	return false, nil
}

var keptUntilLast = map[string]bool{"inputs": true, ".install.lock": true}

// Removes everything but the record and the held lock, true when nothing else is left
func removeContents(state string) (bool, error) {
	entries, err := os.ReadDir(state)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if keptUntilLast[e.Name()] {
			continue
		}
		child := filepath.Join(state, e.Name())
		if st, err := os.Stat(child); err == nil && st.IsDir() && e.Type()&fs.ModeSymlink == 0 {
			removeStateTree(child)
		} else {
			_ = unlinkStateFile(child)
		}
	}
	entries, err = os.ReadDir(state)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !keptUntilLast[e.Name()] {
			return false, nil
		}
	}
	return true, nil
}

// Removes every unheld orphan state dir under installs, rechecking each under its lock, and returns them
func CollectOrphanInstallStates(installs string) ([]string, error) {
	if st, err := os.Stat(installs); err != nil || !st.IsDir() {
		return nil, nil
	}
	dataRoot := filepath.Dir(resolvePath(installs))
	retention, retain, err := retentionSeconds(filepath.Dir(installs))
	if err != nil {
		return nil, err
	}
	states, err := sortedChildren(installs)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for _, state := range states {
		orphan, err := isOrphan(state, dataRoot, retention, retain)
		if err != nil {
			return nil, err
		}
		if orphan {
			candidates = append(candidates, state)
		}
	}
	var removed []string
	for _, state := range candidates {
		emptied, err := collectOneState(state, dataRoot, retention, retain)
		if err != nil {
			return nil, err
		}
		if !emptied {
			continue
		}
		if runtime.GOOS != "windows" || unlinkStateFile(filepath.Join(state, ".install.lock")) == nil {
			_ = syscallRmdir(state)
		}
		if _, err := os.Stat(state); errors.Is(err, fs.ErrNotExist) {
			removed = append(removed, state)
		}
	}
	return removed, nil
}

// Empties one orphan under its install lock, the record going last and the POSIX lock unlinked while held
func collectOneState(state, dataRoot string, retention float64, retain bool) (bool, error) {
	release := installLock(state)
	if release == nil {
		slog.Debug("orphan install state " + filepath.Base(state) + " is held or back in use; skipped")
		return false, nil
	}
	defer release()
	orphan, err := isOrphan(state, dataRoot, retention, retain)
	if err != nil {
		return false, err
	}
	if !orphan {
		slog.Debug("orphan install state " + filepath.Base(state) + " is held or back in use; skipped")
		return false, nil
	}
	held, err := leased(state)
	if err != nil {
		return false, err
	}
	if held {
		slog.Debug("orphan install state " + filepath.Base(state) + " is held or back in use; skipped")
		return false, nil
	}
	emptied, err := removeContents(state)
	if err != nil {
		return false, err
	}
	if !emptied {
		slog.Debug("orphan install state " + filepath.Base(state) + " only partly removed; retried next pass")
		return false, nil
	}
	removeStateTree(filepath.Join(state, "inputs"))
	if runtime.GOOS != "windows" {
		_ = unlinkStateFile(filepath.Join(state, ".install.lock"))
	}
	return true, nil
}

// Removes an empty directory without recursing
func syscallRmdir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return &fs.PathError{Op: "rmdir", Path: path, Err: errors.New("not a directory")}
	}
	return os.Remove(path)
}
