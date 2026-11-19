//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Binds the staged tool runtime row and the managed interpreter TCC anchor row of the doctor
func init() {
	doctorStagedRuntimeRow = doctorReportStagedRuntime
	doctorTCCAnchorCheck = doctorCheckMacosTCCAnchor
}

// Selected dependency venv of the package manager for this install
var doctorSelectedVenvDir = func(ctx context.Context) (string, error) { return pm.SelectedVenv(ctx, pm.RepoRoot()) }

// Provisioned runtime venv of PM, empty when nothing is staged or PM cannot say
var doctorStagedVenvDir = func(ctx context.Context) (staged string) {
	defer func() {
		if recover() != nil {
			staged = ""
		}
	}()
	venvDir, err := doctorSelectedVenvDir(ctx)
	if err != nil || venvDir == "" {
		return ""
	}
	if info, err := os.Stat(filepath.Join(venvDir, "pyvenv.cfg")); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return venvDir
}

// Names the tool runtime venv the package manager staged, silent when none is
func doctorReportStagedRuntime(ctx context.Context, d *Doctor) {
	if staged := doctorStagedVenvDir(ctx); staged != "" {
		doctorCheckOK(d.W, "Runtime venv staged ("+staged+")", "(this process runs outside it)")
	}
}

// Reports the interpreter TCC anchor and installs it with fix, silent off macOS and for unmanaged interpreters
func doctorCheckMacosTCCAnchor(ctx context.Context, d *Doctor, shouldFix bool) error {
	return doctorWarnOnError(d.W, "macOS TCC anchor check failed", "", nil, func() error {
		status, detail, err := macosTCCAnchorState(ctx, d.ProjectRoot)
		if err != nil {
			return err
		}
		switch status {
		case "skip":
			return nil
		case "active":
			doctorCheckOK(d.W, "macOS TCC anchor active", "("+detail+")")
			return nil
		}
		if shouldFix {
			anchored, err := ensureMacosTCCAnchor(ctx, d.ProjectRoot)
			if err != nil {
				return err
			}
			if anchored != "" {
				doctorCheckOK(d.W, "macOS TCC anchor installed", "("+anchored+")")
				return nil
			}
		}
		if status == "missing" {
			doctorCheckWarn(d.W, "macOS TCC anchor missing", "("+detail+")")
		} else {
			doctorCheckWarn(d.W, "macOS TCC anchor stale", "("+detail+")")
		}
		return nil
	})
}
