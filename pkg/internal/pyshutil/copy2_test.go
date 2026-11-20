package pyshutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCopy2CopiesContentModeAndTimes(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.json"), filepath.Join(dir, "dst.json")
	if err := os.WriteFile(src, []byte("[1]"), 0o640); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2020, 1, 2, 3, 4, 5, 6000, time.UTC)
	if err := os.Chtimes(src, when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Copy2(src, dst); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dst)
	fi, _ := os.Stat(dst)
	if string(data) != "[1]" || !fi.ModTime().Equal(when) {
		t.Fatalf("content %q mtime %v", data, fi.ModTime())
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode())
	}
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	if err := Copy2(src, sub); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(sub, "src.json")); string(data) != "[1]" {
		t.Fatalf("copy into dir %q", data)
	}
	var same *SameFileError
	if err := Copy2(src, src); !errors.As(err, &same) || err.Error() != "'"+src+"' and '"+src+"' are the same file" {
		t.Fatalf("same file %v", err)
	}
	if err := Copy2(filepath.Join(dir, "missing"), dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source %v", err)
	}
}

func TestCopyFileCopiesContentOnlyAndRefusesItself(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "model.gguf"), filepath.Join(dir, "copy.gguf")
	if err := os.WriteFile(src, []byte("GGUF"), 0o640); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(src, when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("older and longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CopyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dst)
	fi, _ := os.Stat(dst)
	if string(data) != "GGUF" || fi.ModTime().Equal(when) {
		t.Fatalf("content %q mtime %v", data, fi.ModTime())
	}
	var same *SameFileError
	if err := CopyFile(src, src); !errors.As(err, &same) {
		t.Fatalf("same file %v", err)
	}
	if err := CopyFile(filepath.Join(dir, "missing"), dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source %v", err)
	}
}

func TestRmtreeRemovesTreesAndRefusesSymlinks(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(root, "a", "b", "f.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "top.txt"), []byte("x"), 0o644)
	outside := filepath.Join(dir, "outside")
	os.Mkdir(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("x"), 0o644)
	if runtime.GOOS != "windows" {
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "rootlink")); err != nil {
			t.Fatal(err)
		}
		if err := Rmtree(filepath.Join(dir, "rootlink")); !errors.Is(err, ErrRmtreeSymlink) || err.Error() != "Cannot call rmtree on a symbolic link" {
			t.Fatalf("symlink root %v", err)
		}
	}
	if err := Rmtree(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root survived %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatalf("symlink target removed %v", err)
	}
	if err := Rmtree(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing %v", err)
	}
}
