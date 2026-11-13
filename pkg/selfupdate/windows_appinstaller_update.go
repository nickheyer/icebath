package selfupdate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Hidden first argument that runs the App Installer update check the desktop spawns for its MSIX package
const WindowsAppInstallerUpdateEntry = "__icebath_windows_appinstaller_update__"

// HRESULT 0x80073D54, the process has no package identity, the one failure naming a non packaged run
const noPackageIdentityHRESULT = -2147009196

// Member names of PackageUpdateAvailability as the Windows Runtime projection spells them
var packageUpdateAvailabilityNames = []string{"UNKNOWN", "NO_UPDATES", "AVAILABLE", "REQUIRED", "ERROR"}

const (
	packageUpdateNoUpdates int32 = 1
	packageUpdateAvailable int32 = 2
	packageUpdateRequired  int32 = 3
)

// Windows Runtime Package statics the check reads the running package from
type appInstallerProjection interface {
	Current() (appInstallerPackage, error)
	Close()
}

// Running MSIX package the check asks for its App Installer source and its update availability
type appInstallerPackage interface {
	AppInstallerSourceURI() (uri string, registered bool, err error)
	CheckUpdateAvailability() (int32, error)
}

// Runs the App Installer check for the hidden entry argv names and exits with its status
func DispatchWindowsAppInstallerUpdateEntry(exit func(int)) {
	if len(os.Args) < 2 || os.Args[1] != WindowsAppInstallerUpdateEntry {
		return
	}
	exit(windowsAppInstallerUpdateMain(os.Stdout))
}

// Prints the App Installer check as one JSON line, exit 2 for an update, 0 for none, else 1
func windowsAppInstallerUpdateMain(stdout io.Writer) int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	report := func(code int, pairs ...any) int {
		text, err := pyjson.Dumps(pyjson.ObjectOf(pairs...))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if _, err := fmt.Fprintln(stdout, text); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return code
	}
	projection, err := loadAppInstallerProjection()
	if err != nil {
		return report(1, "available", nil, "error", "winrt import failed: "+err.Error())
	}
	defer projection.Close()
	pkg, err := projection.Current()
	if err != nil {
		var winErr interface{ WinError() int32 }
		if errors.As(err, &winErr) && winErr.WinError() == noPackageIdentityHRESULT {
			return report(0, "available", false, "reason", "not-packaged")
		}
		return report(1, "available", nil, "error", "package identity failed: "+err.Error())
	}
	checkFailed := func(err error) int {
		return report(1, "available", nil, "error", "check failed: "+err.Error())
	}
	sourceURI, registered, err := pkg.AppInstallerSourceURI()
	if err != nil {
		return checkFailed(err)
	}
	if !registered {
		return report(1, "available", nil, "reason", "no-app-installer-source",
			"error", "No App Installer update source is registered. Install Icebath through its .appinstaller file to enable updates.")
	}
	raw, err := pkg.CheckUpdateAvailability()
	if err != nil {
		return checkFailed(err)
	}
	if raw < 0 || int(raw) >= len(packageUpdateAvailabilityNames) {
		return checkFailed(fmt.Errorf("%d is not a valid PackageUpdateAvailability", raw))
	}
	name := packageUpdateAvailabilityNames[raw]
	switch raw {
	case packageUpdateNoUpdates:
		return report(0, "available", false, "availability", name, "source_uri", sourceURI)
	case packageUpdateAvailable, packageUpdateRequired:
		return report(2, "available", true, "availability", name, "source_uri", sourceURI)
	}
	return report(1, "available", nil, "error", "availability unknown: "+name)
}
