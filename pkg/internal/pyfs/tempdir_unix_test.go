//go:build unix

package pyfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveTempDirResetsReadOnlyEntries(t *testing.T) {
	tmp, err := os.MkdirTemp(t.TempDir(), "tmp")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(outside, 0o700) })
	locked := filepath.Join(tmp, "stage", "locked")
	if err := os.MkdirAll(filepath.Join(locked, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(locked, "file"), filepath.Join(locked, "deep", "secret")} {
		if err := os.WriteFile(f, []byte("x"), 0o000); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(locked, "link")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(locked, "deep"), locked} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveTempDir(tmp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tmp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("temporary directory survived: %v", err)
	}
	st, err := os.Stat(outside)
	if err != nil || st.Mode().Perm() != 0o500 {
		t.Fatalf("link target changed: %v %v", st, err)
	}
}

func TestRemoveTempDirOfAMissingDirectory(t *testing.T) {
	if err := RemoveTempDir(filepath.Join(t.TempDir(), "gone")); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTempDirRefusesASymlinkedRoot(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTempDir(link); !errors.Is(err, ErrRmTreeSymlink) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("link target removed: %v", err)
	}
}

func TestCloseTempDirReplacesTheBlockResult(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	blockErr := errors.New("block")
	err := blockErr
	CloseTempDir(link, &err)
	if !errors.Is(err, ErrRmTreeSymlink) {
		t.Fatalf("got %v", err)
	}
	err = blockErr
	CloseTempDir(filepath.Join(t.TempDir(), "gone"), &err)
	if err != blockErr {
		t.Fatalf("a clean cleanup replaced the block error: %v", err)
	}
}
