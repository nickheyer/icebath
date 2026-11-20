package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func readSpill(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteCreatesFileWithContent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "spill.txt")
	if err := WriteTextExclusive(target, "hello\n", false, false); err != nil {
		t.Fatal(err)
	}
	if got := readSpill(t, target); got != "hello\n" {
		t.Fatalf("content %q", got)
	}
}

func TestPrivateFileIs0600(t *testing.T) {
	target := filepath.Join(t.TempDir(), "spill.txt")
	if err := WriteTextExclusive(target, "secret", true, false); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestPrivateDirIs0700AndTightened(t *testing.T) {
	d := filepath.Join(t.TempDir(), "spills")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSpillDir(d, true); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(d)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestEnsureSpillDirRefusesSymlinkedLeaf(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim-dir")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "spills")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSpillDir(link, false); err == nil {
		t.Fatal("symlinked leaf accepted")
	}
}

func TestRefusesPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "spill.txt")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}
	if err := WriteTextExclusive(target, "attacker-controlled", false, false); err == nil {
		t.Fatal("write through symlink succeeded")
	}
	if got := readSpill(t, victim); got != "original" {
		t.Fatalf("victim %q", got)
	}
}

func TestRefusesDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "spill.txt")
	missing := filepath.Join(dir, "does-not-exist.txt")
	if err := os.Symlink(missing, target); err != nil {
		t.Fatal(err)
	}
	if err := WriteTextExclusive(target, "x", false, false); err == nil {
		t.Fatal("write through dangling symlink succeeded")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("dangling target exists: %v", err)
	}
}

func TestOverwriteRemovesSymlinkNotItsTarget(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "spill.txt")
	if err := os.Symlink(victim, target); err != nil {
		t.Fatal(err)
	}
	if err := WriteTextExclusive(target, "redacted copy", false, true); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("target is still a symlink")
	}
	if got := readSpill(t, target); got != "redacted copy" {
		t.Fatalf("target %q", got)
	}
	if got := readSpill(t, victim); got != "original" {
		t.Fatalf("victim %q", got)
	}
}

func TestOverwriteReplacesRegularFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "spill.txt")
	if err := os.WriteFile(target, []byte("raw"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteTextExclusive(target, "redacted", false, true); err != nil {
		t.Fatal(err)
	}
	if got := readSpill(t, target); got != "redacted" {
		t.Fatalf("target %q", got)
	}
}

func TestOverwriteRefusesDirectory(t *testing.T) {
	target := filepath.Join(t.TempDir(), "spill.txt")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteTextExclusive(target, "x", false, true); err == nil {
		t.Fatal("directory overwritten")
	}
	st, err := os.Lstat(target)
	if err != nil || !st.IsDir() {
		t.Fatalf("directory gone: %v", err)
	}
}

func TestExclusiveCreateFailsOnExistingWithoutOverwrite(t *testing.T) {
	target := filepath.Join(t.TempDir(), "spill.txt")
	if err := os.WriteFile(target, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteTextExclusive(target, "second", false, false); err == nil {
		t.Fatal("existing file overwritten")
	}
	if got := readSpill(t, target); got != "first" {
		t.Fatalf("target %q", got)
	}
}

func TestOpenExclusiveStreamingWrite(t *testing.T) {
	target := filepath.Join(t.TempDir(), "spill.log")
	fh, err := OpenExclusive(target, false, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{"chunk1", "chunk2"} {
		if _, err := fh.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readSpill(t, target); got != "chunk1chunk2" {
		t.Fatalf("target %q", got)
	}
}
