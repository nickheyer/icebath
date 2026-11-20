package home

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Entry whose ro subdir refuses unlink of its blob, the residue a root owned container write leaves
func undeletableEntry(t *testing.T, root, name string) (string, string) {
	t.Helper()
	entry := filepath.Join(root, name)
	ro := filepath.Join(entry, "ro")
	writeText(t, filepath.Join(entry, "deletable.txt"), "x")
	writeText(t, filepath.Join(ro, "blob"), "left by a container")
	age(t, 48, filepath.Join(ro, "blob"), filepath.Join(entry, "deletable.txt"), ro, entry)
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	return entry, ro
}

// Records every directory under root the prune lists, with the reap step disabled
func walkSpy(t *testing.T, root string) *[]string {
	var walked []string
	swap(t, &scratchReadDir, func(dir string) ([]fs.DirEntry, error) {
		if strings.HasPrefix(dir, root+string(os.PathSeparator)) {
			walked = append(walked, dir)
		}
		return readDirUnsorted(dir)
	})
	swap(t, &reapStep, func(string, *[]string, *pruneAudit) (int, error) { return 0, nil })
	return &walked
}

// Log lines naming entry
func linesFor(t *testing.T, logFile, entry string) []string {
	data, _ := os.ReadFile(logFile)
	var out []string
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.Contains(ln, pathRepr(entry)) {
			out = append(out, ln)
		}
	}
	return out
}

func walkedUnder(walked []string, entry string) bool {
	for _, p := range walked {
		if p == entry || strings.HasPrefix(p, entry+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func mustPruneIdle(t *testing.T, root, logFile string, want int) {
	t.Helper()
	got, err := PruneIdleEntries(root, 24, nil, logFile)
	if err != nil || got != want {
		t.Fatalf("PruneIdleEntries = %d, %v, want %d", got, err, want)
	}
}

// Skips where root deletes through read only dirs or stuck tracking does not exist
func requireStuckTracking(t *testing.T) {
	if !stuckTrackingSupported() || os.Getuid() == 0 {
		t.Skip("needs a non-root POSIX user, root deletes through a read-only dir")
	}
}

func TestUndeletableOrUnreadableEntryLoggedOnceThenSkippedWithoutWalk(t *testing.T) {
	requireStuckTracking(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "scratch")
	entry, ro := undeletableEntry(t, root, "lane")
	sealed := filepath.Join(root, "sealed")
	locked := filepath.Join(sealed, "locked")
	writeText(t, filepath.Join(locked, "blob"), "unreadable")
	age(t, 48, filepath.Join(locked, "blob"), locked, sealed)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	logFile := filepath.Join(tmp, "logs", "scratch-prune.log")
	walked := walkSpy(t, root)
	mustPruneIdle(t, root, logFile, 0)
	if !walkedUnder(*walked, entry) || !walkedUnder(*walked, sealed) {
		t.Fatalf("first pass must walk both entries: %v", *walked)
	}
	*walked = nil
	later := scratchNow() + 48*3600
	swap(t, &scratchNow, func() float64 { return later })
	mustPruneIdle(t, root, logFile, 0)
	for _, e := range []string{entry, sealed} {
		if walkedUnder(*walked, e) {
			t.Fatalf("a known-stuck entry must not be re-walked every pass: %s", e)
		}
		lines := linesFor(t, logFile, e)
		if len(lines) != 1 || !strings.Contains(lines[0], "sudo rm -rf "+e) {
			t.Fatalf("%q", lines)
		}
	}
	if _, err := os.Stat(filepath.Join(ro, "blob")); err != nil {
		t.Fatal(err)
	}
}

func TestFixedExpiredOrOtherUIDEntryIsJudgedAgain(t *testing.T) {
	requireStuckTracking(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "scratch")
	entry, ro := undeletableEntry(t, root, "lane")
	other, otherRO := undeletableEntry(t, root, "other")
	logFile := filepath.Join(tmp, "logs", "scratch-prune.log")
	walked := walkSpy(t, root)
	now := scratchNow()
	mustPruneIdle(t, root, logFile, 0)
	before, _ := entryFingerprint(entry)
	if err := os.Chmod(ro, 0o700); err != nil {
		t.Fatal(err)
	}
	if after, _ := entryFingerprint(entry); after[2] != before[2] {
		t.Fatal("chmod of a nested dir moved the entry ctime")
	}
	*walked = nil
	swap(t, &scratchNow, func() float64 { return now + 48*3600 })
	mustPruneIdle(t, root, logFile, 1)
	if _, err := os.Stat(entry); err == nil || walkedUnder(*walked, other) {
		t.Fatal("fixed entry kept or stuck entry walked")
	}
	scratchNow = func() float64 { return now + 9*24*3600 }
	mustPruneIdle(t, root, logFile, 0)
	if !walkedUnder(*walked, other) || len(linesFor(t, logFile, other)) != 2 {
		t.Fatal("expired record was not judged again")
	}
	stateFile := filepath.Join(root, StuckStateName)
	data, _ := os.ReadFile(stateFile)
	v, err := pyjson.Loads(string(data))
	if err != nil {
		t.Fatal(err)
	}
	rec := v.(*pyjson.Object).Vals["other"].(*pyjson.Object)
	rec.Vals["uid"] = rec.Vals["uid"].(int64) + 1
	text, _ := pyjson.Dumps(v)
	writeText(t, stateFile, text)
	*walked = nil
	mustPruneIdle(t, root, logFile, 0)
	if !walkedUnder(*walked, other) {
		t.Fatal("record made by another uid was trusted")
	}
	if err := os.Chmod(otherRO, 0o700); err != nil {
		t.Fatal(err)
	}
	scratchNow = func() float64 { return now + 11*24*3600 }
	mustPruneIdle(t, root, logFile, 1)
	if _, err := os.Stat(stateFile); err == nil {
		t.Fatal("empty stuck state file kept")
	}
}

// Directory entry whose stat always fails
type unreadableChild struct{}

func (unreadableChild) Name() string      { return "result.txt" }
func (unreadableChild) IsDir() bool       { return false }
func (unreadableChild) Type() fs.FileMode { return 0 }
func (unreadableChild) Info() (fs.FileInfo, error) {
	return nil, &fs.PathError{Op: "stat", Path: "result.txt", Err: fs.ErrInvalid}
}

// Installs a failing probe of entry for failed once active reports true
func failProbe(t *testing.T, failed, entry string, active func() bool) {
	if failed == "root-stat" {
		swap(t, &scratchLstat, func(p string) (fs.FileInfo, error) {
			if active() && p == entry {
				return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrPermission}
			}
			return os.Lstat(p)
		})
		return
	}
	swap(t, &scratchReadDir, func(dir string) ([]fs.DirEntry, error) {
		if active() && dir == entry {
			if failed == "directory-scan" {
				return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
			}
			return []fs.DirEntry{unreadableChild{}}, nil
		}
		return readDirUnsorted(dir)
	})
}

// Scratch root with one idle lane holding one aged result file
func scanFailureEntry(t *testing.T) (string, string, string, string) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "scratch")
	entry := filepath.Join(root, "lane")
	result := filepath.Join(entry, "result.txt")
	writeText(t, result, "keep until activity can be checked")
	age(t, 48, result, entry)
	return tmp, root, entry, result
}

func TestUnreadableSubtreeIsPreservedUntilItCanBeScanned(t *testing.T) {
	for _, failed := range []string{"root-stat", "directory-scan", "child-stat"} {
		t.Run(failed, func(t *testing.T) {
			_, root, entry, result := scanFailureEntry(t)
			swap(t, &reapStep, func(string, *[]string, *pruneAudit) (int, error) { return 0, nil })
			probing := true
			failProbe(t, failed, entry, func() bool { return probing })
			mustPruneIdle(t, root, "", 0)
			if data, _ := os.ReadFile(result); string(data) != "keep until activity can be checked" {
				t.Fatal("unscannable entry damaged")
			}
			probing = false
			mustPruneIdle(t, root, "", 1)
			if _, err := os.Stat(entry); err == nil {
				t.Fatal("scannable idle entry kept")
			}
		})
	}
}

func TestPostSelectionScanFailureKeepsEntryWithHonestAudit(t *testing.T) {
	for _, failed := range []string{"root-stat", "directory-scan", "child-stat"} {
		t.Run(failed, func(t *testing.T) {
			tmp, root, entry, result := scanFailureEntry(t)
			logFile := filepath.Join(tmp, "logs", "scratch-prune.log")
			before, _ := os.Stat(result)
			selected := false
			swap(t, &reapStep, func(string, *[]string, *pruneAudit) (int, error) { selected = true; return 0, nil })
			failProbe(t, failed, entry, func() bool { return selected })
			mustPruneIdle(t, root, logFile, 0)
			after, err := os.Stat(result)
			if err != nil || !after.ModTime().Equal(before.ModTime()) {
				t.Fatal("entry touched by the failed re-scan")
			}
			data, _ := os.ReadFile(logFile)
			log := string(data)
			if strings.Contains(log, "touched since selection") || !strings.Contains(log, "kept "+pathRepr(entry)+" ") || !strings.Contains(log, "activity re-scan failed, left untouched") {
				t.Fatal(log)
			}
			selected = false
			swap(t, &reapStep, func(string, *[]string, *pruneAudit) (int, error) { return 0, nil })
			mustPruneIdle(t, root, logFile, 1)
			if _, err := os.Stat(entry); err == nil {
				t.Fatal("scannable idle entry kept")
			}
		})
	}
}
