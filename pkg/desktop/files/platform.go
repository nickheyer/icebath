package files

import "runtime"

// Node process.platform name of the host
func electronPlatformName() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}
