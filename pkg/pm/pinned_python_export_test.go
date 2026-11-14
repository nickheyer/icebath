package pm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedPythonVersionExportReadsTheLockPin(t *testing.T) {
	version, ok := NewLockfile(LockfilePath()).Version("python")
	parts := strings.SplitN(version, ".", 3)
	major, minor, err := PinnedPythonVersion()
	if !ok || len(parts) < 2 || err != nil || major != parts[0] || minor != parts[1] {
		t.Fatalf("lock %q %v, export %q %q %v", version, ok, major, minor, err)
	}
	empty := filepath.Join(t.TempDir(), "lock.json")
	if err := os.WriteFile(empty, []byte(`{"schema": 1, "packages": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := lockfilePath
	lockfilePath = func() string { return empty }
	t.Cleanup(func() { lockfilePath = previous })
	var pyErr *PyError
	if _, _, err := PinnedPythonVersion(); !errors.As(err, &pyErr) || pyErr.Class != "RuntimeError" || pyErr.Message != "the lock pins no python version: ''" {
		t.Fatalf("no pin %v", err)
	}
}
