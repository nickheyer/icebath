//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package pm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCopyTreeLikeCopiesTheTimesOfAKeptSymlink(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(src, "link")
	if err := os.Symlink("f", link); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2001, 2, 3, 4, 5, 6, 789000000, time.UTC)
	ts := []unix.Timespec{unix.NsecToTimespec(old.Add(time.Minute).UnixNano()), unix.NsecToTimespec(old.UnixNano())}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, link, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := copyTreeLike(src, dst, true, nil); err != nil {
		t.Fatal(err)
	}
	copied, err := os.Lstat(filepath.Join(dst, "link"))
	if err != nil || copied.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link copy %v %v", copied, err)
	}
	if target, _ := os.Readlink(filepath.Join(dst, "link")); target != "f" {
		t.Fatalf("link target %q", target)
	}
	atime, err := fileAccessTime(filepath.Join(dst, "link"), copied)
	if err != nil {
		t.Fatal(err)
	}
	if !copied.ModTime().Equal(old) || !atime.Equal(old.Add(time.Minute)) {
		t.Fatalf("link mtime %v atime %v, want %v and %v", copied.ModTime(), atime, old, old.Add(time.Minute))
	}
}
