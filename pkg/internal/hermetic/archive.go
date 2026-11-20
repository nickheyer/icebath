package hermetic

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/tarzst"
)

// Unpacks a testdata zstd tar into a fresh directory of the test and returns that directory
func UnpackTarZst(t testing.TB, archive string) string {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tarzst.Extract(data, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Short directory of the test for unix sockets, whose whole path the kernel caps near a hundred bytes
func ShortDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
