package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestSlashExportUsesTheSameManagedDestination(t *testing.T) {
	rec := recordPrints(t)
	tmp := t.TempDir()
	defaultHome := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(defaultHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, defaultHome)
	t.Cleanup(home.PatchUserHome(tmp))
	profiles.ResetFileCache()
	home.ResetKeyCache()
	t.Chdir(tmp)

	bareCLI().handleExportCommand("/export")

	archives, err := filepath.Glob(filepath.Join(defaultHome, "profile-exports", "*.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 {
		t.Fatalf("archives under profile-exports = %q, printed %q", archives, rec.plain())
	}
	if base := filepath.Base(archives[0]); !strings.HasPrefix(base, "default-") {
		t.Fatalf("exported profile archive %s is not the default profile's", base)
	}
	if _, err := os.Stat(filepath.Join(tmp, "default.tar.gz")); !os.IsNotExist(err) {
		t.Fatalf("archive written to the cwd: %v", err)
	}
}
