package pyplatform

import "runtime"

// Python platform.system of this host, empty when the OS does not say
func System() string { return system() }

// Python platform.machine of this host, empty when the OS does not say
func Machine() string { return machine() }

// Kernel release the way platform.release reads it, the Windows product release on Windows
func Release() string { return release() }

var goosSystems = map[string]string{"linux": "Linux", "darwin": "Darwin", "windows": "Windows", "freebsd": "FreeBSD",
	"openbsd": "OpenBSD", "netbsd": "NetBSD", "dragonfly": "DragonFly", "solaris": "SunOS", "android": "Linux"}

func goosSystem() string { return goosSystems[runtime.GOOS] }
