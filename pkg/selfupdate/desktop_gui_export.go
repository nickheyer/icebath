package selfupdate

import (
	"context"
	"io"
)

// Whether the desktop app needs a rebuild before launch
func DesktopBuildNeeded(ctx context.Context, desktopDir, projectRoot string, sourceMode bool) bool {
	return desktopBuildNeeded(ctx, desktopDir, projectRoot, sourceMode)
}

// Unpacked Electron executable of this platform under the release dir, false when none exists
func DesktopPackagedExecutable(desktopDir string) (string, bool) {
	return desktopPackagedExecutable(desktopDir)
}

// True when a local desktop renderer build is present
func DesktopDistExists(desktopDir string) bool { return desktopDistExists(desktopDir) }

// Electron package directory the checkout workspace resolves
func ElectronDir(projectRoot string) string { return electronDir(projectRoot) }

// App a packaged launch opens, the refreshed installed bundle on macOS, else the release build
func InstalledDesktopLaunchTarget(ctx context.Context, desktopDir, packagedExecutable string) string {
	return installedDesktopLaunchTarget(ctx, desktopDir, packagedExecutable)
}

// Re-signs the packaged macOS app with the configured or pinned identity, true when it is relaunchable
func DesktopMacosRelaunchableFixup(desktopDir string) bool {
	return desktopMacosRelaunchableFixup(desktopDir, nil, "")
}

// Python exception class an error stands for
func ExceptionName(err error) string { return exceptionName(err) }

// Routes the desktop build messages to w and returns the restore of the previous writer
func SetDesktopOut(w io.Writer) func() {
	prev := desktopOut
	desktopOut = w
	return func() { desktopOut = prev }
}

// Shipped Webapp bundle the environment names, empty when none is packaged
func PackagedWebappDist() string { return packagedWebappDist() }
