//go:build !windows

package pyfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func lockDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
}

func TestMoveMatchesShutilMove(t *testing.T) {
	root := t.TempDir()
	j := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	t.Run("file_into_dir", func(t *testing.T) {
		os.MkdirAll(j("c1", "box"), 0o755)
		mustWrite(t, j("c1", "f.txt"), "x", 0o644)
		got, err := Move(j("c1", "f.txt"), j("c1", "box"))
		if err != nil || got != j("c1", "box", "f.txt") {
			t.Fatalf("%q %v", got, err)
		}
	})
	t.Run("dir_trailing_slash_then_collision", func(t *testing.T) {
		os.MkdirAll(j("c2", "a", "sub"), 0o755)
		os.MkdirAll(j("c2", "box"), 0o755)
		got, err := Move(j("c2", "a")+"/", j("c2", "box"))
		if err != nil || got != j("c2", "box", "a") {
			t.Fatalf("%q %v", got, err)
		}
		os.MkdirAll(j("c2", "a"), 0o755)
		_, err = Move(j("c2", "a"), j("c2", "box"))
		var me *MoveError
		if !errors.As(err, &me) || me.Msg != "Destination path '"+j("c2", "box", "a")+"' already exists" {
			t.Fatalf("collision %v", err)
		}
	})
	t.Run("dangling_link_target", func(t *testing.T) {
		os.MkdirAll(j("c3", "box"), 0o755)
		mustWrite(t, j("c3", "f"), "y", 0o644)
		if err := os.Symlink(j("c3", "nowhere"), j("c3", "box", "f")); err != nil {
			t.Fatal(err)
		}
		got, err := Move(j("c3", "f"), j("c3", "box"))
		if err != nil || got != j("c3", "box", "f") {
			t.Fatalf("%q %v", got, err)
		}
		if st, err := os.Lstat(j("c3", "box", "f")); err != nil || st.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("link survived %v %v", st, err)
		}
		if b, _ := os.ReadFile(j("c3", "box", "f")); string(b) != "y" {
			t.Fatalf("content %q", b)
		}
	})
	t.Run("same_dir", func(t *testing.T) {
		os.MkdirAll(j("c4", "d"), 0o755)
		if got, err := Move(j("c4", "d"), j("c4", "d")); err != nil || got != "" {
			t.Fatalf("%q %v", got, err)
		}
	})
	t.Run("into_itself", func(t *testing.T) {
		os.MkdirAll(j("c5", "d", "inner"), 0o755)
		_, err := Move(j("c5", "d"), j("c5", "d", "inner", "x"))
		var me *MoveError
		if !errors.As(err, &me) || me.Msg != "Cannot move a directory '"+j("c5", "d")+"' into itself '"+j("c5", "d", "inner", "x")+"'." {
			t.Fatalf("%v", err)
		}
	})
	t.Run("missing_src", func(t *testing.T) {
		_, err := Move(j("c6", "none"), j("c6x"))
		if err == nil || OSErrorText(err) != "[Errno 2] No such file or directory: '"+j("c6", "none")+"'" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("copy2_fallback", func(t *testing.T) {
		os.MkdirAll(j("c7", "out"), 0o755)
		src := j("c7", "locked", "f.bin")
		mustWrite(t, src, "data", 0o640)
		atime, mtime := time.Unix(1000000000, 500000000), time.Unix(1100000000, 250000000)
		if err := os.Chtimes(src, atime, mtime); err != nil {
			t.Fatal(err)
		}
		lockDir(t, j("c7", "locked"))
		_, err := Move(src, j("c7", "out", "g.bin"))
		if !errors.Is(err, fs.ErrPermission) || OSErrorText(err) != "[Errno 13] Permission denied: '"+src+"'" {
			t.Fatalf("%v", err)
		}
		st, err := os.Stat(j("c7", "out", "g.bin"))
		if err != nil || st.Mode().Perm() != 0o640 || st.ModTime().UnixNano() != 1100000000250000000 || identityAccessTime(t, j("c7", "out", "g.bin")).UnixNano() != 1000000000500000000 {
			t.Fatalf("copied stat %v %v", st, err)
		}
		if b, _ := os.ReadFile(j("c7", "out", "g.bin")); string(b) != "data" {
			t.Fatalf("content %q", b)
		}
		if _, err := os.Stat(src); err != nil {
			t.Fatalf("source gone %v", err)
		}
	})
	t.Run("copytree_fallback", func(t *testing.T) {
		os.MkdirAll(j("c8", "out"), 0o755)
		file := j("c8", "locked", "t", "sub", "f")
		mustWrite(t, file, "z", 0o600)
		os.Chtimes(file, time.Unix(1000000000, 500000000), time.Unix(1100000000, 250000000))
		if err := os.Symlink("sub/f", j("c8", "locked", "t", "link")); err != nil {
			t.Fatal(err)
		}
		linkBefore, _ := os.Lstat(j("c8", "locked", "t", "link"))
		os.Chtimes(j("c8", "locked", "t", "sub"), time.Unix(1200000000, 0), time.Unix(1300000000, 0))
		os.Chtimes(j("c8", "locked", "t"), time.Unix(1200000000, 0), time.Unix(1400000000, 0))
		lockDir(t, j("c8", "locked"))
		_, err := Move(j("c8", "locked", "t"), j("c8", "out", "t2"))
		if !errors.Is(err, fs.ErrPermission) || OSErrorText(err) != "[Errno 13] Permission denied: '"+j("c8", "locked", "t")+"'" {
			t.Fatalf("%v", err)
		}
		top, _ := os.Stat(j("c8", "out", "t2"))
		sub, _ := os.Stat(j("c8", "out", "t2", "sub"))
		leaf, _ := os.Stat(j("c8", "out", "t2", "sub", "f"))
		link, _ := os.Lstat(j("c8", "out", "t2", "link"))
		if top == nil || sub == nil || leaf == nil || link == nil {
			t.Fatal("copied tree is missing entries")
		}
		if top.Mode().Perm() != 0o755 || top.ModTime().UnixNano() != 1400000000000000000 || sub.ModTime().UnixNano() != 1300000000000000000 {
			t.Fatalf("dir stat %v %v", top.ModTime(), sub.ModTime())
		}
		if leaf.Mode().Perm() != 0o600 || leaf.ModTime().UnixNano() != 1100000000250000000 || identityAccessTime(t, j("c8", "out", "t2", "sub", "f")).UnixNano() != 1000000000500000000 {
			t.Fatalf("leaf stat %v", leaf)
		}
		if target, _ := os.Readlink(j("c8", "out", "t2", "link")); target != "sub/f" || !link.ModTime().Equal(linkBefore.ModTime()) {
			t.Fatalf("link %q %v %v", target, link.ModTime(), linkBefore.ModTime())
		}
		left, _ := os.ReadDir(j("c8", "locked", "t"))
		if _, err := os.Stat(j("c8", "locked", "t")); err != nil || len(left) != 0 {
			t.Fatalf("source tree %v %v", left, err)
		}
	})
	t.Run("dir_onto_file", func(t *testing.T) {
		os.MkdirAll(j("c9", "d"), 0o755)
		mustWrite(t, j("c9", "file"), "f", 0o644)
		_, err := Move(j("c9", "d"), j("c9", "file"))
		if !errors.Is(err, fs.ErrExist) || OSErrorText(err) != "[Errno 17] File exists: '"+j("c9", "file")+"'" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("symlink_fallback", func(t *testing.T) {
		os.MkdirAll(j("c10", "locked"), 0o755)
		os.MkdirAll(j("c10", "out"), 0o755)
		if err := os.Symlink("target", j("c10", "locked", "l")); err != nil {
			t.Fatal(err)
		}
		lockDir(t, j("c10", "locked"))
		_, err := Move(j("c10", "locked", "l"), j("c10", "out", "l2"))
		if !errors.Is(err, fs.ErrPermission) || OSErrorText(err) != "[Errno 13] Permission denied: '"+j("c10", "locked", "l")+"'" {
			t.Fatalf("%v", err)
		}
		if target, _ := os.Readlink(j("c10", "out", "l2")); target != "target" {
			t.Fatalf("link %q", target)
		}
		if _, err := os.Lstat(j("c10", "locked", "l")); err != nil {
			t.Fatalf("source link gone %v", err)
		}
	})
}
