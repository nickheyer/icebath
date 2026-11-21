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
	// An atime past the ctime and within the day survives the readlink on relatime and noatime mounts
	fresh := time.Unix(time.Now().Add(time.Hour).Unix(), 123456789)
	ts := []unix.Timespec{unix.NsecToTimespec(fresh.UnixNano()), unix.NsecToTimespec(old.UnixNano())}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, link, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := copyTreeLike(src, dst, true, nil); err != nil {
		t.Fatal(err)
	}
	// shutil.copystat lstats a kept link after readlink, so a strictatime mount hands on the atime that read stamped
	srcLink, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fileAccessTime(link, srcLink)
	if err != nil {
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
	if !copied.ModTime().Equal(old) || !atime.Equal(want) {
		t.Fatalf("link mtime %v atime %v, want %v and %v", copied.ModTime(), atime, old, want)
	}
}

func TestCopyTreeLikeGivesAFileTheTimesStatedBeforeItsContentWasRead(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(src, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A day old atime is restamped by the read on relatime and strictatime mounts, the os.DirEntry stat copytree hands copy2 predating it
	atime, mtime := time.Unix(1000000000, 500000000), time.Unix(1100000000, 250000000)
	if err := os.Chtimes(file, atime, mtime); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := copyTreeLike(src, dst, false, nil); err != nil {
		t.Fatal(err)
	}
	copied, err := os.Stat(filepath.Join(dst, "f"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := fileAccessTime(filepath.Join(dst, "f"), copied)
	if err != nil {
		t.Fatal(err)
	}
	if !copied.ModTime().Equal(mtime) || !got.Equal(atime) {
		t.Fatalf("file times %v and %v, want %v and %v", copied.ModTime(), got, mtime, atime)
	}
}
