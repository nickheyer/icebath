package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupExistingCopiesOnlyRegularFilesBesideTheirOriginals(t *testing.T) {
	dir := t.TempDir()
	env, cfg := filepath.Join(dir, ".env"), filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(env, []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := BackupExisting([]string{env, cfg})
	if err != nil || len(got) != 1 || filepath.Dir(got[0]) != dir || !strings.HasPrefix(filepath.Base(got[0]), ".env") {
		t.Fatalf("%v %v", got, err)
	}
	if data, err := os.ReadFile(got[0]); err != nil || string(data) != "K=v\n" {
		t.Fatalf("%q %v", data, err)
	}
}
