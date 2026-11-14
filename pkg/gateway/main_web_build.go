package gateway

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Source build steps of the dashboard rebuild that live in the self update package, assigned by the command wiring
var WebUISourceBuild struct {
	ProductCurrent func(ctx context.Context, projectRoot, product, out string) bool
	Env            func(ctx context.Context) (map[string]string, error)
	PrepareLaunch  func(ctx context.Context, projectRoot string, env map[string]string) error
	BuildWeb       func(ctx context.Context, projectRoot string, env map[string]string) error
	ExceptionName  func(err error) string
}

// Repo root of a frontend dir, the grandparent of an apps workspace and the parent of web
func webProjectRoot(webDir string) string {
	parent := filepath.Dir(webDir)
	if filepath.Base(parent) == "apps" {
		return filepath.Dir(parent)
	}
	return parent
}

// Directory the web UI build of a frontend dir is published into
func webDistDir(webDir string) string {
	return filepath.Join(webProjectRoot(webDir), "cmd", "icebath", "web_dist")
}

// Whether the dashboard bundle is stale against its sources
func webUIBuildNeeded(ctx context.Context, webDir string) bool {
	return !WebUISourceBuild.ProductCurrent(ctx, webProjectRoot(webDir), "web", webDistDir(webDir))
}

// Historical updater entrypoint that now stops the old updater for a relaunch
func writeWebUIBuildStamp(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired build path that stops the old updater for a relaunch
func runWithIdleTimeout(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired NixOS build environment that stops the old updater for a relaunch
func nixosBuildEnv(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Retired deterministic npm install that stops the old updater for a relaunch
func runNPMInstallDeterministic(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Serializes dashboard rebuilds and checks freshness only after taking the lock, an uncaught build error returned
func BuildWebUI(ctx context.Context, w io.Writer, webDir string, fatal bool) (bool, error) {
	if _, err := os.Stat(filepath.Join(webDir, "package.json")); err != nil {
		return true, nil
	}
	lockPath := filepath.Join(webProjectRoot(webDir), ".web_ui_build.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666)
	if err == nil {
		defer lockFile.Close()
		_, err = pm.LockFD(lockFile, true, pm.WaitForever)
	}
	if err != nil {
		fmt.Fprintf(w, "  ✗ Could not lock the web UI build: %s\n", webBuildErrText(err))
		return false, nil
	}
	return doBuildWebUI(ctx, w, webDir, fatal)
}

// Python exception classes a dashboard build reports instead of raising
var webBuildCaughtErrors = []string{
	"RuntimeError", "NotImplementedError", "RecursionError", "WebappBuildError", "ProductBuildError", "FeatureProbeError",
	"StaleLockRow", "NoCommittedEnvironment", "InstallError", "ResolutionConflict", "BuildFailure", "DownloadError",
	"SubprocessError", "CalledProcessError", "TimeoutExpired",
	"OSError", "FileNotFoundError", "PermissionError", "FileExistsError", "IsADirectoryError", "NotADirectoryError",
	"TimeoutError", "BlockingIOError", "ChildProcessError", "ConnectionError", "BrokenPipeError", "ConnectionAbortedError",
	"ConnectionRefusedError", "ConnectionResetError", "InterruptedError", "ProcessLookupError",
}

// Python str of a build or lock error
func webBuildErrText(err error) string {
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

// Locked rebuild step of the dashboard, swapped by tests that hold the build lock
var doBuildWebUI = doBuildWebUIUnlocked

// Builds stale dashboard sources, a failure never reported as a usable build
func doBuildWebUIUnlocked(ctx context.Context, w io.Writer, webDir string, fatal bool) (bool, error) {
	if _, err := os.Stat(filepath.Join(webDir, "package.json")); err != nil || !webUIBuildNeeded(ctx, webDir) {
		return true, nil
	}
	projectRoot := webProjectRoot(webDir)
	fmt.Fprintln(w, "→ Building web UI...")
	err := func() error {
		env, err := WebUISourceBuild.Env(ctx)
		if err != nil {
			return err
		}
		if err := WebUISourceBuild.PrepareLaunch(ctx, projectRoot, env); err != nil {
			return err
		}
		return WebUISourceBuild.BuildWeb(ctx, projectRoot, env)
	}()
	if err != nil {
		if !pypath.IsOSError(err) && !slices.Contains(webBuildCaughtErrors, WebUISourceBuild.ExceptionName(err)) {
			return false, err
		}
		mark := "⚠"
		if fatal {
			mark = "✗"
		}
		fmt.Fprintf(w, "  %s Web UI build failed: %s\n", mark, webBuildErrText(err))
		return false, nil
	}
	fmt.Fprintln(w, "  ✓ Web UI built")
	return true, nil
}
