package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

// In memory user PATH standing in for the registry value, counting writes
type mwbRegistry struct {
	entries []string
	kind    uint32
	writes  int
}

func mwbInstall(t *testing.T, windows bool, initial []string) *mwbRegistry {
	t.Helper()
	state := &mwbRegistry{entries: append([]string{}, initial...), kind: 2}
	vsSwap(t, &migrateIsWindows, func() bool { return windows })
	vsSwap(t, &migrateReadUserPath, func() ([]string, uint32, error) { return append([]string{}, state.entries...), state.kind, nil })
	vsSwap(t, &migrateWriteUserPath, func(entries []string, kind uint32) error {
		state.entries, state.kind = append([]string{}, entries...), kind
		state.writes++
		return nil
	})
	return state
}

func TestMigrationRefusesEmptyStoreWithoutChangingPath(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "icebath")
	root := filepath.Join(h, "icebath-agent")
	if err := os.MkdirAll(filepath.Join(root, "venv", "Scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	store := filepath.Join(tmp, "empty-store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_RUNTIME_DIR", store)
	state := mwbInstall(t, true, []string{filepath.Join(root, "bin"), `C:\Windows\system32`})
	if migrateWindowsBinPath(root) || state.writes != 0 {
		t.Fatal("the PATH moved onto launchers that cannot start", state.writes)
	}
}

func TestMigrationSkipsSourceCheckouts(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "icebath-home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_RUNTIME_DIR", filepath.Join(tmp, "empty-store"))
	root := filepath.Join(tmp, "src", "icebath-agent")
	if err := os.MkdirAll(filepath.Join(root, "venv", "Scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := mwbInstall(t, true, []string{`C:\Windows\system32`})
	if migrateWindowsBinPath(root) || state.writes != 0 {
		t.Fatal("a source checkout was migrated")
	}
}

func TestMigrationNoopOnPosix(t *testing.T) {
	mwbInstall(t, false, nil)
	if migrateWindowsBinPath(filepath.Join(t.TempDir(), "icebath-agent")) {
		t.Fatal("the migration ran off Windows")
	}
}

func TestNormalizeWindowsPathEquivalences(t *testing.T) {
	if normalizeWindowsPath(`C:\Users\Me\AppData\Local\icebath\bin`) != normalizeWindowsPath("c:/users/me/appdata/local/ICEBATH/BIN/") {
		t.Fatal("equivalent Windows paths compared unequal")
	}
}
