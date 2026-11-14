//go:build unix

package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestUntrackedAutostashCannotSplitLiveDatabaseLockInode(t *testing.T) {
	repo := usFlatInstallRepo(t)
	lockPath := filepath.Join(repo, "state.db.quarantine.lock")
	first, err := os.OpenFile(lockPath, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := unix.Flock(int(first.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	firstStat, err := first.Stat()
	if err != nil {
		t.Fatal(err)
	}
	ugWrite(t, filepath.Join(repo, "app.py"), "print('changed')\n")

	stashRef, err := stashLocalChangesIfNeeded(context.Background(), &bytes.Buffer{}, []string{"git"}, repo, nil, nil, false)
	if err != nil || stashRef == nil || *stashRef == "" {
		t.Fatalf("stash ref = %v, err = %v", stashRef, err)
	}

	after, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstStat, after) {
		t.Fatal("the held lock file moved to another inode")
	}
	second, err := os.OpenFile(lockPath, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := unix.Flock(int(second.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("second exclusive lock: %v", err)
	}
}
