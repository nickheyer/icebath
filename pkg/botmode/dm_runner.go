package botmode

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Argv word that runs this binary as the Bot Mode DM delivery runner
const DMRunnerEntry = "__icebath_bot_mode_dm__"

// Command a delivery runs the CLI through, the published launcher, then the binary's sibling, then PATH
func IcebathCLI() string {
	name := "icebath"
	if runtime.GOOS == "windows" {
		name = "icebath.exe"
	}
	candidates := []string{filepath.Join(pm.RepoRoot(), ".icebath", "bin", name)}
	if executable, err := os.Executable(); err == nil && executable != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), name))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	if found, ok := pyshutil.Which("icebath", nil); ok {
		return found
	}
	return "icebath"
}
