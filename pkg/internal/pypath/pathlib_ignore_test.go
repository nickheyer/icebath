package pypath

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPathlibIgnoredError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, missing := os.Stat(filepath.Join(dir, "missing"))
	_, notDir := os.Stat(filepath.Join(file, "child"))
	for _, c := range []struct {
		err  error
		want bool
	}{
		{missing, true},
		{notDir, true},
		{&fs.PathError{Op: "stat", Path: file, Err: syscall.EACCES}, false},
		{&fs.PathError{Op: "stat", Path: file, Err: syscall.EPERM}, false},
		{errors.New("plain"), false},
		{nil, false},
	} {
		if got := PathlibIgnoredError(c.err); got != c.want {
			t.Errorf("%v: got %v want %v", c.err, got, c.want)
		}
	}
}
