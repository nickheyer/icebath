//go:build linux

package logging

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestManagedModeInitialOpenSetsGroupWritable(t *testing.T) {
	testHome(t)
	path := filepath.Join(t.TempDir(), "managed-open.log")
	prev := isManaged
	isManaged = func(context.Context) bool { return true }
	t.Cleanup(func() { isManaged = prev })
	old := syscall.Umask(0o022)
	err := addRotatingHandler(context.Background(), path, LevelInfo, 1024, 1, plainFormatter(), nil)
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o660 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}
