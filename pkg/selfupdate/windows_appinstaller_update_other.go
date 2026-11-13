//go:build !windows

package selfupdate

import (
	"fmt"
	"runtime"
)

// The Package projection lives in the Windows Runtime, which only a Windows build reaches
func loadAppInstallerProjection() (appInstallerProjection, error) {
	return nil, fmt.Errorf("the Windows Runtime is unavailable on %s", runtime.GOOS)
}
