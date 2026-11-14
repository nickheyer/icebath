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
