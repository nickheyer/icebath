package pyfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageAndReplaceThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	os.Mkdir(realDir, 0o700)
	real := filepath.Join(realDir, "jobs.json")
	os.WriteFile(real, []byte("old"), 0o600)
	link := filepath.Join(dir, "jobs.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	tmp, err := MkstempBeside(link, ".jobs_", ".tmp")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(tmp.Name()) != realDir || !strings.HasPrefix(filepath.Base(tmp.Name()), ".jobs_") || !strings.HasSuffix(tmp.Name(), ".tmp") {
		t.Fatalf("staged at %s", tmp.Name())
	}
	tmp.WriteString("new")
	tmp.Close()
	published, err := AtomicReplace(tmp.Name(), link)
	if err != nil || published != real {
		t.Fatalf("published %s %v", published, err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	if b, _ := os.ReadFile(link); string(b) != "new" {
		t.Fatalf("content %q", b)
	}
	FsyncDirectory(realDir)
}
