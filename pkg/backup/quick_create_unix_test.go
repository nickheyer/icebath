//go:build !windows

package backup

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestQuickSnapshotTreeIsOwnerOnlyUnderPermissiveUmask(t *testing.T) {
	h := filepath.Join(t.TempDir(), ".icebath")
	writeTree(t, h, map[string]string{"config.yaml": "model: {}\n"})
	c := testConn(t, filepath.Join(h, "state.db"), "")
	mustExec(t, c, "CREATE TABLE sessions (id TEXT PRIMARY KEY)")
	closeQuietly(c)
	old := syscall.Umask(0o022)
	snapID, err := CreateQuickSnapshot(nil, h, nil, nil)
	syscall.Umask(old)
	if err != nil || snapID == "" {
		t.Fatalf("%q %v", snapID, err)
	}
	root := filepath.Join(h, quickSnapshotsDir)
	snapshot := filepath.Join(root, snapID)
	dirs := []string{root, snapshot}
	var files []string
	filepath.Walk(snapshot, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == snapshot {
			return err
		}
		if info.IsDir() {
			dirs = append(dirs, p)
		} else if info.Mode().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) == 0 {
		t.Fatal("no files")
	}
	for _, d := range dirs {
		if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %o", d, st.Mode().Perm())
		}
	}
	for _, f := range files {
		if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o", f, st.Mode().Perm())
		}
	}
}
