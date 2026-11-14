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

func TestPathlibStatReportsOnlyTheErrorsPathlibRaises(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, err := PathlibStat(file); err != nil || fi == nil || !fi.Mode().IsRegular() {
		t.Fatalf("file stat %v %v", fi, err)
	}
	if fi, err := PathlibStat(dir); err != nil || fi == nil || !fi.IsDir() {
		t.Fatalf("dir stat %v %v", fi, err)
	}
	for _, p := range []string{filepath.Join(dir, "missing"), filepath.Join(file, "child"), dir + "/a\x00b"} {
		if fi, err := PathlibStat(p); fi != nil || err != nil {
			t.Fatalf("%q: got %v %v want nil nil", p, fi, err)
		}
	}
	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if fi, err := PathlibStat(loop); fi != nil || err != nil {
		t.Fatalf("symlink loop: got %v %v", fi, err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	if _, err := PathlibStat(filepath.Join(locked, "x")); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("permission denied stat error %v", err)
	}
}
