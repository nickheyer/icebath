package selfupdate

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Retired dependency hook of an old updater, hands the run to the takeover child without a second install path
func captureActiveLazyFeatures(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired dependency hook of an old updater, hands the run to the takeover child whatever arguments it got
func refreshActiveLazyFeatures(ctx context.Context, args ...any) error {
	return pm.StopForRelaunch(ctx)
}

// Retired dependency hook of an old updater, hands the run to the takeover child without a plugin reinstall
func refreshActiveMemoryProviderDependencies(ctx context.Context) error {
	return pm.StopForRelaunch(ctx)
}

// Retired dependency hook of an old updater, hands the run to the takeover child without a currency answer
func npmLockfileChanged(ctx context.Context, icebathRoot string) error {
	return pm.StopForRelaunch(ctx)
}

// Retired dependency hook of an old updater, hands the run to the takeover child without an npm install
func updateNodeDependencies(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired dependency hook of an old updater, hands the run to the takeover child without a desktop build
func rebuildDesktopAfterUpdate(ctx context.Context, desktopDir string, hadDesktopAppBeforeUpdate bool) error {
	return pm.StopForRelaunch(ctx)
}

// Retired dependency hook of an old updater, hands the run to the takeover child without an ownership preflight
func pathUID(ctx context.Context, path string) error { return pm.StopForRelaunch(ctx) }

// Failure an old updater catches around its quarantine step, carrying the launchers it names
type ShimQuarantineError struct{ FailedShims []string }

func (e *ShimQuarantineError) Error() string {
	return "could not quarantine live shim(s): " + strings.Join(e.FailedShims, ", ")
}

// Builds the quarantine failure over a copy of the launcher names
func newShimQuarantineError(failedShims []string) *ShimQuarantineError {
	return &ShimQuarantineError{FailedShims: append([]string{}, failedShims...)}
}

// Retired main entry of an old updater, hands the run to the takeover child without selecting a build stamp
func desktopStampPath(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired main entry of an old updater, answers nothing so broken imports read as unknown
func detectBrokenLazyRefreshImports(installCmdPrefix []string, env map[string]string) []string {
	return nil
}

// Retired main entry of an old updater, hands the run to the takeover child without preparing an install
func insertPythonPin(ctx context.Context, args []string) error { return pm.StopForRelaunch(ctx) }

// Retired main entry of an old updater, hands the run to the takeover child without targeting live launchers
func interpreterScriptsDir(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired main entry of an old updater, hands the run to the takeover child without selecting dependency retries
func loadInstallableOptionalExtras(ctx context.Context, group string) error {
	return pm.StopForRelaunch(ctx)
}

// Retired main entry of an old updater, hands the run to the takeover child without renaming live executables
func quarantineRunningIcebathExe(ctx context.Context, scriptsDir string, maxAttempts int, failedOut *[]string) error {
	return pm.StopForRelaunch(ctx)
}

// Retired main entry of an old updater, hands the run to the takeover child without repairing imports
func repairBrokenLazyRefreshImports(ctx context.Context, installCmdPrefix, packages []string, env map[string]string) error {
	return pm.StopForRelaunch(ctx)
}

// Retired main entry of an old updater, names no interpreter to probe
func resolveInstallTargetPython(installCmdPrefix []string, env map[string]string) string { return "" }

// Retired main entry of an old updater, renames nothing back
func restoreQuarantinedExes(moved [][2]string) {}

// Retired main entry of an old updater, hands the run to the takeover child without running pip or uv
func runInstallWithHeartbeat(ctx context.Context, cmd []string, env map[string]string, heartbeatIntervalSeconds int) error {
	return pm.StopForRelaunch(ctx)
}

// Retired main entry of an old updater, hands the run to the takeover child without a package only retry
func runPackageOnlyInstall(ctx context.Context, cmd []string, env map[string]string) error {
	return pm.StopForRelaunch(ctx)
}

// Retired main entry of an old updater, hands the run to the takeover child without installing
func runQuarantinedInstall(ctx context.Context, cmd []string, env map[string]string, scriptsDir string, strictQuarantine bool) error {
	return pm.StopForRelaunch(ctx)
}

// Retired main entry of an old updater, hands the run to the takeover child without exposing process scans
func mainSelf(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired main entry of an old updater, hands the run to the takeover child without reinstalling entry points
func verifyConsoleScriptsInstalled(ctx context.Context, installCmdPrefix []string, env map[string]string) error {
	return pm.StopForRelaunch(ctx)
}

// Retired main entry of an old updater, hands the run to the takeover child without reinstalling packages
func verifyCoreDependenciesInstalled(ctx context.Context, installCmdPrefix []string, env map[string]string, group string) error {
	return pm.StopForRelaunch(ctx)
}

// Retired main entry of an old updater, hands the run to the takeover child without an install handoff
func windowsShimInProcessChain(ctx context.Context) error { return pm.StopForRelaunch(ctx) }
