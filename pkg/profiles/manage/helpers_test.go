package manage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Temp HOME and default profile home with the state guard bypassed since its real root follows HOME, returning HOME
func manageEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	def := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, def)
	t.Setenv("ICEBATH_STATE_DB_GUARD_BYPASS", "1")
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
