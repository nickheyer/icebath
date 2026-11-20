//go:build unix

package manage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestExportDirSymlinkIsRejected(t *testing.T) {
	tmp := t.TempDir()
	def := filepath.Join(tmp, ".icebath")
	elsewhere := filepath.Join(tmp, "elsewhere")
	for _, d := range []string{def, elsewhere} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(elsewhere, filepath.Join(def, "profile-exports")); err != nil {
		t.Fatal(err)
	}
	exportHomes(t, def, tmp)
	_, err := GetProfileExportPath("default", "")
	var ve *profiles.ValueError
	if !errors.As(err, &ve) || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("got %v", err)
	}
}

func TestExportRemovesAReadOnlyStage(t *testing.T) {
	tmp := manageEnv(t)
	scratch := filepath.Join(tmp, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", scratch)
	dir := namedProfile(t, tmp, "nixy")
	locked := filepath.Join(dir, "skills", "store")
	writeFile(t, filepath.Join(locked, "SKILL.md"), "---\nname: store\n---\n")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	if _, err := ExportProfile("nixy", filepath.Join(tmp, "nixy.tar.gz"), nil); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadDir(scratch)
	if err != nil || len(left) != 0 {
		t.Fatalf("export left %v behind (%v)", left, err)
	}
}
