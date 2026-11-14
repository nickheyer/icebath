package pyfs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCopyTreeKeepsLinksAppliesIgnoreAndMergesExisting(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	for path, body := range map[string]string{"a.txt": "a", "sub/b.txt": "b", "skip/c.txt": "c", "sub/x.tmp": "x"} {
		p := filepath.Join(src, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	ignore := func(dir string, names []string) map[string]bool {
		out := map[string]bool{}
		for _, n := range names {
			if n == "skip" || strings.HasSuffix(n, ".tmp") {
				out[n] = true
			}
		}
		return out
	}
	dst := filepath.Join(tmp, "dst")
	if err := CopyTree(src, dst, true, false, ignore); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "a.txt" {
		t.Fatalf("link %q %v", target, err)
	}
	if st, err := os.Stat(filepath.Join(dst, "sub", "b.txt")); err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("b.txt %v %v", st, err)
	}
	for _, gone := range []string{"skip", "sub/x.tmp"} {
		if _, err := os.Lstat(filepath.Join(dst, gone)); err == nil {
			t.Fatalf("%s copied", gone)
		}
	}
	if err := CopyTree(src, dst, true, false, ignore); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing dst accepted %v", err)
	}
	if err := os.Remove(filepath.Join(dst, "link")); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(src, dst, true, true, ignore); err != nil {
		t.Fatalf("dirs_exist_ok merge %v", err)
	}
}

func TestCopyTreeGathersFailures(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "missing"), filepath.Join(src, "dangling")); err != nil {
		t.Fatal(err)
	}
	err := CopyTree(src, filepath.Join(tmp, "dst"), false, false, nil)
	var tree *CopyTreeError
	if !errors.As(err, &tree) || len(tree.Failures) != 1 || !strings.Contains(tree.Error(), "[Errno 2] No such file or directory") {
		t.Fatalf("%v", err)
	}
}

func TestCopyTreeAbortableStopsAtTheIgnoreError(t *testing.T) {
	src := t.TempDir()
	for _, d := range []string{"a/deep", "b"} {
		if err := os.MkdirAll(filepath.Join(src, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "a", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("refused")
	var seen []string
	dst := filepath.Join(t.TempDir(), "out")
	err := CopyTreeAbortable(src, dst, false, false, func(dir string, names []string) (map[string]bool, error) {
		seen = append(seen, filepath.Base(dir))
		if filepath.Base(dir) == "deep" {
			return nil, stop
		}
		return map[string]bool{}, nil
	})
	if err != stop {
		t.Fatalf("err %v, want the ignore error unchanged", err)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "b")); !os.IsNotExist(statErr) {
		t.Fatalf("copy went on after the abort, seen %v", seen)
	}
	if err := CopyTreeAbortable(src, filepath.Join(t.TempDir(), "ok"), false, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCopy2KeepsTheAccessTimeApartFromTheModificationTime(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.txt"), filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	accessed, modified := time.Now().Add(time.Hour).Truncate(time.Second), time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(src, accessed, modified); err != nil {
		t.Fatal(err)
	}
	if err := Copy2(src, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil || !fi.ModTime().Equal(modified) {
		t.Fatalf("modification time %v %v", fi, err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if got := identityAccessTime(t, dst); !got.Equal(accessed) {
		t.Fatalf("access time %v, the source had %v", got, accessed)
	}
	if data, _ := os.ReadFile(dst); string(data) != "payload" {
		t.Fatalf("content %q", data)
	}
	if err := Copy2(src, src); err == nil {
		t.Fatal("copying a file onto itself is refused")
	}
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	if err := Copy2(src, sub); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(sub, "src.txt")); string(data) != "payload" {
		t.Fatalf("copy into a directory %q", data)
	}
}
