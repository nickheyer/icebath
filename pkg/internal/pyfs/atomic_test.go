package pyfs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWriteTextReplacesThroughSymlinkAndRejectsSurrogates(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "link.json")
	if err := os.WriteFile(real, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := AtomicWriteText(context.Background(), link, "new", ".tmp_"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(real); string(b) != "new" {
		t.Fatalf("real file %q", b)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	err := AtomicWriteText(context.Background(), filepath.Join(dir, "x"), "a\xed\xa0\x80", ".tmp_")
	if _, ok := err.(*EncodeError); !ok {
		t.Fatalf("surrogate write %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("leftover temp files %d", len(entries))
	}
}

func TestMoveIntoExistingDirAndRefusesCollision(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "sub", "f"), []byte("x"), 0o644)
	dst := filepath.Join(dir, "box")
	os.Mkdir(dst, 0o755)
	got, err := Move(src, dst)
	if err != nil || got != filepath.Join(dst, "a") {
		t.Fatalf("%q %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a", "sub", "f")); string(b) != "x" {
		t.Fatal("content lost")
	}
	os.Mkdir(src, 0o755)
	if _, err := Move(src, dst); err == nil || err.Error() != "Destination path '"+filepath.Join(dst, "a")+"' already exists" {
		t.Fatalf("collision %v", err)
	}
	if err := CopyTree(filepath.Join(dst, "a"), filepath.Join(dir, "copy"), true, false, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "copy", "sub", "f")); string(b) != "x" {
		t.Fatal("copytree lost content")
	}
}

func TestAtomicWriteTextModeForcesBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	p := filepath.Join(t.TempDir(), "SKILL.md")
	if err := AtomicWriteTextMode(context.Background(), p, "one", ".tmp_", 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
		t.Fatalf("new file mode %v", st.Mode().Perm())
	}
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteTextMode(context.Background(), p, "two", ".tmp_", 0o640); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	data, _ := os.ReadFile(p)
	if st.Mode().Perm() != 0o640 || string(data) != "two" {
		t.Fatalf("mode %v content %q", st.Mode().Perm(), data)
	}
}
