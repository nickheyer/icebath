//go:build windows

package gatewaycli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestSeedSuperviseSkeletonFailsWhereWindowsLacksChownAndMkfifo(t *testing.T) {
	svc := t.TempDir()
	if err := seedSuperviseSkeleton(svc); err == nil || err.Error() != "module 'os' has no attribute 'chown'" {
		t.Fatalf("fresh slot %v", err)
	}
	if st, err := os.Stat(filepath.Join(svc, "event")); err != nil || !st.IsDir() {
		t.Fatalf("event dir before the chown %v", err)
	}
	if err := os.MkdirAll(filepath.Join(svc, "supervise", "event"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := seedSuperviseSkeleton(svc); err == nil || err.Error() != "module 'os' has no attribute 'mkfifo'" {
		t.Fatalf("seeded slot %v", err)
	}
	if _, err := os.Lstat(filepath.Join(svc, "supervise", "control")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("control entry %v", err)
	}
}
