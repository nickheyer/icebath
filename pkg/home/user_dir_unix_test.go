//go:build !windows

package home

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestMakeUserDirFollowsPathMkdir(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	tmp := t.TempDir()
	deep := filepath.Join(tmp, "a", "b", "c")
	if _, err := MakeUserDir(deep, 0o700, false, false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing parent without parents = %v", err)
	}
	got, err := MakeUserDir(deep+"/", 0o700, true, false)
	if err != nil || got != deep {
		t.Fatalf("MakeUserDir = %q, %v", got, err)
	}
	if st, _ := os.Stat(deep); st.Mode().Perm() != 0o700 {
		t.Fatalf("leaf mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(tmp, "a")); st.Mode().Perm() != 0o755 {
		t.Fatalf("parent mode %v, want the default mode under umask", st.Mode().Perm())
	}
	if _, err := MakeUserDir(deep, 0o700, true, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing leaf without exist_ok = %v", err)
	}
	if _, err := MakeUserDir(deep, 0o700, true, true); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(tmp, "file")
	os.WriteFile(file, nil, 0o644)
	if _, err := MakeUserDir(file, 0o700, true, true); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing file with exist_ok = %v", err)
	}
}
