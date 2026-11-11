package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Registry access and platform of the PATH migration, replaced by hermes tests
var (
	migrateIsWindows     = func() bool { return runtime.GOOS == "windows" }
	migrateReadUserPath  = readUserPathRaw
	migrateWriteUserPath = writeUserPathRaw
	migrateOwnsUserPath  = pm.OwnsUserPath
)

// One time move of the launchers onto the home bin dir of the User PATH, true when that layout is in place
func migrateWindowsBinPath(root string) bool {
	if !migrateIsWindows() {
		return false
	}
	base := home.Root()
	if base == "" {
		return false
	}
	if normalizeWindowsPath(filepath.Dir(root)) != normalizeWindowsPath(base) {
		return false
	}
	windows := true
	EnsureWindowsBinLaunchers(root, &windows, []string{})
	homeBin := filepath.Join(base, "bin")
	for _, name := range pm.WindowsBinLaunchers() {
		if !isRegular(filepath.Join(homeBin, name+".exe")) && !isRegular(filepath.Join(homeBin, name+".cmd")) {
			return false
		}
	}
	if !migrateOwnsUserPath(base) {
		return false
	}
	entries, kind, err := migrateReadUserPath()
	if err != nil {
		return false
	}
	legacy := map[string]bool{
		normalizeWindowsPath(filepath.Join(root, "bin")):                       true,
		normalizeWindowsPath(pm.VenvBinDir(filepath.Join(root, "venv"), true)): true,
	}
	homeBinKey := normalizeWindowsPath(homeBin)
	key := func(entry string) string { return normalizeWindowsPath(os.ExpandEnv(windowsExpandVars(entry))) }
	kept := []string{}
	haveHomeBin := false
	for _, entry := range entries {
		if legacy[key(entry)] {
			continue
		}
		kept = append(kept, entry)
		haveHomeBin = haveHomeBin || key(entry) == homeBinKey
	}
	if !haveHomeBin {
		kept = append([]string{homeBin}, kept...)
	}
	if !slices.Equal(kept, entries) {
		if err := migrateWriteUserPath(kept, kind); err != nil {
			return false
		}
		fmt.Fprintf(os.Stderr, "  ✓ icebath launchers now resolve from %s (legacy PATH entries removed)\n", homeBin)
	}
	return true
}
