//go:build unix

package manage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Read only profile copy with 0555 dirs and 0444 files, optionally carrying a gateway.lock
func readOnlyCopy(t *testing.T, h string, lock bool) {
	t.Helper()
	writeFile(t, filepath.Join(h, "config.yaml"), "model: x\n")
	if err := os.MkdirAll(filepath.Join(h, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if lock {
		writeFile(t, filepath.Join(h, "gateway.lock"), "")
	}
	filepath.Walk(h, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			os.Chmod(p, 0o444)
		}
		return nil
	})
	for _, d := range []string{filepath.Join(h, "sub"), h} {
		os.Chmod(d, 0o555)
	}
	t.Cleanup(func() {
		for _, d := range []string{h, filepath.Join(h, "sub")} {
			os.Chmod(d, 0o755)
		}
	})
}

func TestReadOnlyCopyIsUnownedUnlessItsLockIsHeld(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes 0555")
	}
	zero := time.Duration(0)
	for _, lock := range []bool{false, true} {
		h := filepath.Join(t.TempDir(), "profiles", "ro")
		readOnlyCopy(t, h, lock)
		if ok, err := WaitProfileUnowned(h, &zero); err != nil || !ok {
			t.Fatalf("lock=%v: unowned = %v, %v", lock, ok, err)
		}
		if !lock {
			continue
		}
		f, err := os.Open(filepath.Join(h, "gateway.lock"))
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
			t.Fatal(err)
		}
		if ok, err := WaitProfileUnowned(h, &zero); err != nil || ok {
			t.Fatalf("held lock: unowned = %v, %v", ok, err)
		}
		f.Close()
	}
}
