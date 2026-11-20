package pyshutil

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestCopyTreeCopiesFilesDirectoriesAndLinksIntoANewTree(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "f.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/f.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "dst")
	if err := CopyTree(src, dst, true, false); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(dst, "sub", "f.txt")); err != nil || string(body) != "hello" {
		t.Fatalf("copied file %q %v", body, err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "sub/f.txt" {
		t.Fatalf("link %q %v", target, err)
	}
	if err := CopyTree(src, dst, true, false); err == nil {
		t.Fatal("an existing destination was accepted")
	}
	if err := CopyTree(src, dst, true, true); err == nil {
		t.Fatal("a link already present in the destination is a copytree error")
	}
	if err := os.Remove(filepath.Join(dst, "link")); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(src, dst, true, true); err != nil {
		t.Fatalf("merging into an existing destination failed: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "sub/f.txt" {
		t.Fatalf("merged link %q %v", target, err)
	}
	flat := filepath.Join(t.TempDir(), "flat")
	if err := CopyTree(src, flat, false, false); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(filepath.Join(flat, "link")); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("link not dereferenced: %v %v", st, err)
	}
}

func TestCopyTreeIgnoreSkipsIgnoredNames(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	for _, rel := range []string{"keep.txt", "index.lock", "sub/inner.lock", "sub/data.txt"} {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(t.TempDir(), "dst")
	if err := CopyTreeIgnore(src, dst, false, false, IgnorePatterns("*.lock")); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{"keep.txt": true, "index.lock": false, "sub/inner.lock": false, "sub/data.txt": true} {
		_, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel)))
		if (err == nil) != want {
			t.Fatalf("%s copied=%v want %v", rel, err == nil, want)
		}
	}
	if got := IgnorePatterns("*.lock", "a*")("", []string{"x.lock", "abc", "keep"}); !slices.Equal(got, []string{"x.lock", "abc"}) {
		t.Fatalf("patterns %v", got)
	}
}

func TestCopyTreeCopiesLinkTimesDirectoryModeAndLeavesNothingForAFileSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("link times and directory modes are POSIX facts")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(src, "link")
	if err := os.Symlink("sub/f", link); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2001, 2, 3, 4, 5, 6, 789000000, time.UTC)
	setLinkTimes(t, link, old)
	dirTime := time.Date(2010, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "sub"), dirTime.Add(time.Hour), dirTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "sub"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(src, "sub"), 0o755) })
	dst := filepath.Join(root, "dst")
	if err := CopyTree(src, dst, true, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(dst, "sub"), 0o755) })
	copied, err := os.Lstat(filepath.Join(dst, "link"))
	if err != nil || copied.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link copy %v %v", copied, err)
	}
	if !copied.ModTime().Equal(old) || !fileAtime(copied).Equal(old) {
		t.Fatalf("link times %v and %v, want %v", copied.ModTime(), fileAtime(copied), old)
	}
	sub, err := os.Stat(filepath.Join(dst, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if sub.Mode().Perm() != 0o555 || !sub.ModTime().Equal(dirTime) || !fileAtime(sub).Equal(dirTime.Add(time.Hour)) {
		t.Fatalf("directory copy mode %v mtime %v atime %v", sub.Mode().Perm(), sub.ModTime(), fileAtime(sub))
	}
	file := filepath.Join(root, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "never")
	if err := CopyTree(file, target, true, false); err == nil {
		t.Fatal("copying a file as a tree succeeded")
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("a failed scan of the source still created the destination")
	}
}
