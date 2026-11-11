package selfupdate

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Directories under root that each hold a version.txt with their marker
func uzLiveTree(t *testing.T, root string, names map[string]string) {
	t.Helper()
	for name, marker := range names {
		ugWrite(t, filepath.Join(root, name, "version.txt"), marker)
	}
}

// Stages every named entry of fresh beside its target in root
func uzStageAll(t *testing.T, root, fresh string, names ...string) []stagedEntry {
	t.Helper()
	var staged []stagedEntry
	for _, name := range names {
		staging, err := stageReplacement(&bytes.Buffer{}, filepath.Join(fresh, name), filepath.Join(root, name), nil)
		if err != nil {
			t.Fatal(err)
		}
		staged = append(staged, stagedEntry{staging, filepath.Join(root, name)})
	}
	return staged
}

// Names in dir that carry the update's own suffixes
func uzLitter(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var litter []string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "icebath-update") {
			litter = append(litter, entry.Name())
		}
	}
	return litter
}

func uzLiveAndNew(t *testing.T) (live, fresh string) {
	t.Helper()
	tmp := t.TempDir()
	return filepath.Join(tmp, "live"), filepath.Join(tmp, "new")
}

func TestStagingTouchesNothingLive(t *testing.T) {
	live, fresh := uzLiveAndNew(t)
	uzLiveTree(t, live, map[string]string{"agent": "old", "tools": "old"})
	uzLiveTree(t, fresh, map[string]string{"agent": "new", "tools": "new"})
	uzStageAll(t, live, fresh, "agent", "tools")
	for _, name := range []string{"agent", "tools"} {
		if got := ugRead(t, filepath.Join(live, name, "version.txt")); got != "old" {
			t.Fatalf("%s = %q", name, got)
		}
	}
}

func TestCommitSwapsEveryEntry(t *testing.T) {
	live, fresh := uzLiveAndNew(t)
	uzLiveTree(t, live, map[string]string{"agent": "old", "tools": "old"})
	uzLiveTree(t, fresh, map[string]string{"agent": "new", "tools": "new"})
	if err := commitStagedReplacements(uzStageAll(t, live, fresh, "agent", "tools"), nil, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent", "tools"} {
		if got := ugRead(t, filepath.Join(live, name, "version.txt")); got != "new" {
			t.Fatalf("%s = %q", name, got)
		}
	}
	if litter := uzLitter(t, live); len(litter) != 0 {
		t.Fatalf("litter: %v", litter)
	}
}

func TestFailedSwapRollsBackEveryEarlierSwap(t *testing.T) {
	live, fresh := uzLiveAndNew(t)
	uzLiveTree(t, live, map[string]string{"agent": "old", "tools": "old"})
	uzLiveTree(t, fresh, map[string]string{"agent": "new", "tools": "new"})
	staged := uzStageAll(t, live, fresh, "agent", "tools")
	real, calls := zipRename, 0
	zipRename = func(src, dst string) error {
		calls++
		if calls == 4 {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EACCES}
		}
		return real(src, dst)
	}
	err := commitStagedReplacements(staged, nil, "", nil)
	zipRename = real
	if err == nil || !isUpdateOSError(err) {
		t.Fatalf("err = %v, want an OSError", err)
	}
	for _, name := range []string{"agent", "tools"} {
		if got := ugRead(t, filepath.Join(live, name, "version.txt")); got != "old" {
			t.Fatalf("mixed-version tree after rollback: %s = %q", name, got)
		}
	}
}

func TestCommitHandlesEntriesAbsentFromTheInstall(t *testing.T) {
	live, fresh := uzLiveAndNew(t)
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}
	uzLiveTree(t, fresh, map[string]string{"brand_new": "new"})
	if err := commitStagedReplacements(uzStageAll(t, live, fresh, "brand_new"), nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := ugRead(t, filepath.Join(live, "brand_new", "version.txt")); got != "new" {
		t.Fatalf("brand_new = %q", got)
	}
}

func TestStagingSetsAsideLeftoversItCannotProveAreItsOwn(t *testing.T) {
	live, fresh := uzLiveAndNew(t)
	uzLiveTree(t, live, map[string]string{"agent": "old"})
	uzLiveTree(t, fresh, map[string]string{"agent": "new"})
	stale := filepath.Join(live, "agent") + zipStagingSuffix
	ugWrite(t, filepath.Join(stale, "junk.txt"), "from a previous crash")
	if err := commitStagedReplacements(uzStageAll(t, live, fresh, "agent"), nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := ugRead(t, filepath.Join(live, "agent", "version.txt")); got != "new" {
		t.Fatalf("agent = %q", got)
	}
	if statExists(filepath.Join(live, "agent", "junk.txt")) {
		t.Fatal("the leftover was staged over the entry")
	}
	kept, err := filepath.Glob(filepath.Join(live, "agent"+zipStagingSuffix+".icebath-update-kept", "*"))
	if err != nil || len(kept) != 1 || ugRead(t, kept[0]) != "from a previous crash" {
		t.Fatalf("kept aside = %v (%v)", kept, err)
	}
}

func TestTopLevelFilesAreSwappedAtomically(t *testing.T) {
	live, fresh := uzLiveAndNew(t)
	ugWrite(t, filepath.Join(live, "run_agent.py"), "old")
	ugWrite(t, filepath.Join(fresh, "run_agent.py"), "new")
	if err := commitStagedReplacements(uzStageAll(t, live, fresh, "run_agent.py"), nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := ugRead(t, filepath.Join(live, "run_agent.py")); got != "new" {
		t.Fatalf("run_agent.py = %q", got)
	}
	if litter := uzLitter(t, live); len(litter) != 0 {
		t.Fatalf("litter: %v", litter)
	}
}

func TestFileSwapFailureRestoresTheOriginalFile(t *testing.T) {
	live, fresh := uzLiveAndNew(t)
	for _, name := range []string{"cli.py", "run_agent.py"} {
		ugWrite(t, filepath.Join(live, name), "old")
		ugWrite(t, filepath.Join(fresh, name), "new")
	}
	staged := uzStageAll(t, live, fresh, "cli.py", "run_agent.py")
	real, calls := zipReplace, 0
	zipReplace = func(src, dst string) error {
		calls++
		if calls == 2 {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EACCES}
		}
		return real(src, dst)
	}
	err := commitStagedReplacements(staged, nil, "", nil)
	zipReplace = real
	if err == nil || !isUpdateOSError(err) {
		t.Fatalf("err = %v, want an OSError", err)
	}
	for _, name := range []string{"cli.py", "run_agent.py"} {
		if got := ugRead(t, filepath.Join(live, name)); got != "old" {
			t.Fatalf("mixed or corrupt root modules after rollback: %s = %q", name, got)
		}
	}
}

func TestFailedStagingLeavesNoOrphanedCopies(t *testing.T) {
	live, fresh := uzLiveAndNew(t)
	uzLiveTree(t, live, map[string]string{"agent": "old", "tools": "old", "gateway": "old"})
	uzLiveTree(t, fresh, map[string]string{"agent": "new", "tools": "new", "gateway": "new"})
	real, calls := zipCopyTree, 0
	zipCopyTree = func(src, dst string, symlinks, dirsExistOK bool) error {
		calls++
		if calls == 3 {
			return &fs.PathError{Op: "write", Path: dst, Err: syscall.ENOSPC}
		}
		return real(src, dst, symlinks, dirsExistOK)
	}
	var staged []stagedEntry
	var failure error
	for _, name := range []string{"agent", "tools", "gateway"} {
		staging, err := stageReplacement(&bytes.Buffer{}, filepath.Join(fresh, name), filepath.Join(live, name), nil)
		if err != nil {
			discardStaged(staged)
			failure = err
			break
		}
		staged = append(staged, stagedEntry{staging, filepath.Join(live, name)})
	}
	zipCopyTree = real
	if failure == nil || !errors.Is(failure, syscall.ENOSPC) {
		t.Fatalf("failure = %v, want the disk full error", failure)
	}
	if litter := uzLitter(t, live); len(litter) != 0 {
		t.Fatalf("orphaned staging copies: %v", litter)
	}
}

func TestStagingRestoresBackupWhenDstIsMissing(t *testing.T) {
	live, fresh := uzLiveAndNew(t)
	uzLiveTree(t, fresh, map[string]string{"agent": "new"})
	backup := filepath.Join(live, "agent"+zipOldSuffix)
	ugWrite(t, filepath.Join(backup, "version.txt"), "old")
	real := zipCopyTree
	zipCopyTree = func(_, dst string, _, _ bool) error {
		return &fs.PathError{Op: "write", Path: dst, Err: syscall.ENOSPC}
	}
	_, err := stageReplacement(&bytes.Buffer{}, filepath.Join(fresh, "agent"), filepath.Join(live, "agent"), nil)
	zipCopyTree = real
	if err == nil || !isUpdateOSError(err) {
		t.Fatalf("err = %v, want an OSError", err)
	}
	if got := ugRead(t, filepath.Join(live, "agent", "version.txt")); got != "old" {
		t.Fatalf("agent = %q", got)
	}
	if statExists(backup) {
		t.Fatal("the backup is still there")
	}
	if err := commitStagedReplacements(uzStageAll(t, live, fresh, "agent"), nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := ugRead(t, filepath.Join(live, "agent", "version.txt")); got != "new" {
		t.Fatalf("agent = %q", got)
	}
	if litter := uzLitter(t, live); len(litter) != 0 {
		t.Fatalf("litter: %v", litter)
	}
}

func TestStagingRestoresADanglingSymlinkBackupInsteadOfDeletingIt(t *testing.T) {
	live, fresh := uzLiveAndNew(t)
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}
	ugWrite(t, filepath.Join(fresh, "alpha"), "NEW")
	backup := filepath.Join(live, "alpha"+zipOldSuffix)
	if err := os.Symlink("gone.txt", backup); err != nil {
		t.Skip("symlinks need privileges here")
	}
	if _, err := stageReplacement(&bytes.Buffer{}, filepath.Join(fresh, "alpha"), filepath.Join(live, "alpha"), nil); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(live, "alpha"))
	if err != nil || target != "gone.txt" {
		t.Fatalf("alpha -> %q (%v)", target, err)
	}
	if lexists(backup) {
		t.Fatal("the backup is still there")
	}
}
