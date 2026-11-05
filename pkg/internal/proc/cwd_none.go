//go:build unix && !linux && !darwin && !freebsd && !openbsd && !netbsd && !solaris && !illumos

package proc

import (
	"errors"
	"runtime"
)

// Hosts psutil does not support have no cwd probe
const CwdAvailable = false

// Error psutil raises at import on a platform it does not support
func Cwd(pid int) (string, error) {
	return "", errors.New("platform " + runtime.GOOS + " is not supported")
}
