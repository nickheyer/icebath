package manage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Temp user home and default profile home like the hermes profile_env Path.home patch, returning the user home
func manageEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Cleanup(home.PatchUserHome(tmp))
	def := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, def)
	profiles.ResetFileCache()
	profiles.ResetStandaloneMemo()
	home.ResetKeyCache()
	resetSkillCountCache()
	return tmp
}

// Named profile directory with a config.yaml identity marker
func namedProfile(t *testing.T, tmp, name string) string {
	t.Helper()
	dir := filepath.Join(tmp, ".icebath", "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resetSkillCountCache() {
	skillCountRefreshes.Wait()
	skillCountMu.Lock()
	defer skillCountMu.Unlock()
	clear(skillCountCache)
	clear(skillCountNextCheck)
	clear(skillCountScanLocks)
}
