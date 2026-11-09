package pyshutil

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
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
