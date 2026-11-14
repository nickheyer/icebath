//go:build windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// Stages the replacement source the way the pytest helper does
func writeReplaceSource(t *testing.T, dir, content string) string {
	t.Helper()
	tmp := filepath.Join(dir, ".src.tmp")
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return tmp
}

func TestWindowsRealHeldHandleReportsAccessDenied(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := writeReplaceSource(t, dir, "new")
	held, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	renameErr := os.Rename(tmp, target)
	held.Close()
	if renameErr == nil {
		t.Fatal("rename onto a held target succeeded")
	}
	var errno syscall.Errno
	if !errors.As(renameErr, &errno) || (errno != 5 && errno != 32 && errno != 33) {
		t.Fatalf("held target rename reported %v", renameErr)
	}
	if !pyfs.IsContendedReplaceError(renameErr) {
		t.Fatalf("%v not classified as a contended replace", renameErr)
	}
}
