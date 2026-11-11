//go:build unix

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Device id of a path's filesystem
func deviceOf(t *testing.T, path string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return uint64(st.Dev)
}

// A writable directory on a filesystem other than the one holding path, removed at cleanup
func otherFilesystemDir(t *testing.T, path string) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"/dev/shm", cwd} {
		if info, err := os.Stat(base); err != nil || !info.IsDir() || deviceOf(t, base) == deviceOf(t, path) {
			continue
		}
		dir, err := os.MkdirTemp(base, ".exdev-")
		if err != nil {
			continue
		}
		t.Cleanup(func() {
			_ = os.Chmod(dir, 0o700)
			_ = os.RemoveAll(dir)
		})
		return dir
	}
	t.Fatal("no second filesystem beside the temp dir to rename across")
	return ""
}

func TestOutboxJournalSurvivesACrossDeviceRename(t *testing.T) {
	store := mustStore(t, cannedCall(func(string, *pyjson.Object) (any, error) { return pyjson.NewObject(), nil }),
		pyjson.ObjectOf("session_id", "s"), filepath.Join(t.TempDir(), "private"), 0)
	defer store.closeOwner()
	elsewhere := filepath.Join(otherFilesystemDir(t, filepath.Dir(store.path)), "ro")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(elsewhere, "journal.json")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(elsewhere, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.path); err != nil {
		t.Fatal(err)
	}
	if err := store.QueueTokenCounts("s", state.TokenCountDelta{InputTokens: 1}); err != nil {
		t.Fatalf("journal write across devices: %v", err)
	}
	if f := store.Failure(); f != nil {
		t.Fatalf("store failed %q", *f)
	}
	if scope := readJournal(t, store).GetOr("scope", nil); !pyjson.Equal(scope, pyjson.ObjectOf("session_id", "s")) {
		t.Fatalf("scope %v", scope)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode %v %v", info, err)
	}
	for _, dir := range []string{filepath.Dir(store.path), elsewhere} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".pending-") {
				t.Fatalf("leftover %s in %s", e.Name(), dir)
			}
		}
	}
}
