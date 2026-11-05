package pm

import "context"

// Stops an old updater that reloads constants until it relaunches
func reloadIcebathConstants(ctx context.Context) error { return stopForRelaunch(ctx, false) }

// Stops an old updater asking for uv until it relaunches
func EnsureUV(ctx context.Context) error { return stopForRelaunch(ctx, false) }

// Stops an old updater refreshing its managed uv until it relaunches
func UpdateManagedUV(ctx context.Context) error { return stopForRelaunch(ctx, false) }

// Stops an old updater resolving uv until it relaunches, never enabling its pip fallback
func ResolveUV(ctx context.Context) error { return stopForRelaunch(ctx, false) }

// Stops an old updater preparing a managed Python child until it relaunches
func ManagedPythonEnv(ctx context.Context, projectRoot, installDir string, baseEnv map[string]string) error {
	return stopForRelaunch(ctx, false)
}

// Stops an old updater rebuilding a venv until it relaunches, never claiming a rebuild
func RebuildVenv(ctx context.Context, uvBin, venvDir, pythonVersion string) error {
	return stopForRelaunch(ctx, false)
}
