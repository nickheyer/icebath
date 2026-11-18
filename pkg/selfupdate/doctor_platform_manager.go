//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
)

// Binds the managed interpreter TCC anchor row of the doctor
func init() { doctorTCCAnchorCheck = doctorCheckMacosTCCAnchor }

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
