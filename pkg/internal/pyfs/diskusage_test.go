package pyfs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDiskUsageReportsConsistentVolumeSizes(t *testing.T) {
	total, used, free, err := DiskUsage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || used > total || free > total {
		t.Fatalf("disk_usage total=%d used=%d free=%d", total, used, free)
	}
	if _, _, _, err := DiskUsage(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("disk_usage of a missing path must fail like os.statvfs")
	}
}

func TestAccessMatchesOSAccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !Access(path, 4|2) {
		t.Fatal("owner read write file must be accessible")
	}
	if Access(filepath.Join(dir, "missing"), 0) {
		t.Fatal("missing path is never accessible")
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits")
	}
	if Access(path, 2) {
		t.Fatal("read only file must refuse W_OK")
	}
	if !Access(path, 4) {
		t.Fatal("read only file still grants R_OK")
	}
}
