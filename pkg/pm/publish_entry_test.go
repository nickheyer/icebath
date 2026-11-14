package pm

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Writes an entry tree holding one tool binary with the given bytes
func pubWriteEntry(t *testing.T, path string, contents []byte) {
	t.Helper()
	writeFile(t, filepath.Join(path, "bin", "tool"), contents, 0o644)
}

// Refuses only the rename of the live entry onto the previous entry and skips the held retries
func pubHoldEntryRename(t *testing.T, entry, previous string) {
	t.Helper()
	peSwap(t, &heldRetryDelays, []time.Duration{})
	peSwap(t, &renameEntry, func(src, dst string) error {
		if src == entry && dst == previous {
			return &fs.PathError{Op: "rename", Path: src, Err: errors.Join(errors.New("live entry is held"), syscall.EACCES)}
		}
		return os.Rename(src, dst)
	})
}

func TestPublishKeepsAByteIdenticalHeldEntry(t *testing.T) {
	tmp := t.TempDir()
	store := NewStore(filepath.Join(tmp, "store"))
	entry, previous, staged := store.Entry("tool-1"), store.Entry(".previous-tool-1"), filepath.Join(tmp, "staged")
	pubWriteEntry(t, entry, []byte("tool"))
	pubWriteEntry(t, staged, []byte("tool"))
	pubHoldEntryRename(t, entry, previous)
	p := &ieStagePackage{BasePackage{Name: "tool"}}
	if err := publishEntry(p, store, staged, entry, previous, "target", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(entry, "bin", "tool")); err != nil || string(data) != "tool" {
		t.Fatalf("live entry %q %v", data, err)
	}
	if lexists(previous) {
		t.Fatal("previous entry left behind")
	}
}

func TestPublishRaisesWhenAHeldEntryDiffersFromStagedBytes(t *testing.T) {
	tmp := t.TempDir()
	store := NewStore(filepath.Join(tmp, "store"))
	entry, previous, staged := store.Entry("tool-1"), store.Entry(".previous-tool-1"), filepath.Join(tmp, "staged")
	pubWriteEntry(t, entry, []byte("tool"))
	pubWriteEntry(t, staged, []byte("replacement"))
	pubHoldEntryRename(t, entry, previous)
	p := &ieStagePackage{BasePackage{Name: "tool"}}
	err := publishEntry(p, store, staged, entry, previous, "target", func() error { return nil })
	if !isAccessError(err) || !errors.Is(err, syscall.EACCES) || !strings.Contains(err.Error(), "live entry is held") {
		t.Fatalf("publication error %v", err)
	}
}
