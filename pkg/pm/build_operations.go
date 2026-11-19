//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"io"
	"os"
	"path/filepath"
)

// CheckProjectLockOptions are check_project_lock's keywords, Quiet capturing uv's output
type CheckProjectLockOptions struct {
	Source   string
	Python   string
	Cache    string
	Env      map[string]string
	Offline  bool
	Explicit bool
	Quiet    bool
}

// Refuses a missing or stale lock without rewriting the source or creating a venv
func checkProjectLock(ctx context.Context, opts CheckProjectLockOptions) error {
	if !opts.Offline {
		if err := requireInstallAllowed(ctx, opts.Explicit); err != nil {
			return err
		}
	}
	source := absoluteUnresolved(opts.Source)
	if !isRegularFile(filepath.Join(source, "pyproject.toml")) {
		return NewInstallError("venv", "project manifest is missing: "+source, "")
	}
	var output io.Writer = os.Stderr
	if opts.Quiet {
		output = nil
	}
	environment, err := ManagedEnvironment(ctx, filepath.Join(source, ".venv"), ManagedEnvironmentOptions{Python: opts.Python,
		Cache: opts.Cache, Env: opts.Env, Offline: opts.Offline, Explicit: opts.Explicit, Output: output})
	if err != nil {
		return err
	}
	return environment.CheckLock(ctx, source)
}
