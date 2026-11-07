package pyfs

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWriteBytesForcesModeAndKeepsExistingMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "vault.key")
	mode := os.FileMode(0o600)
	payload := []byte("key\r\nbytes\x00\xff")
	if err := AtomicWriteBytes(path, payload, "", &mode, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("content %q %v", got, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
		os.Chmod(path, 0o640)
		if err := AtomicWriteBytes(path, []byte("next"), ".x_", nil, false); err != nil {
			t.Fatal(err)
		}
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
			t.Fatalf("existing mode not kept %v", st.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp file left behind %v", entries)
	}
}
