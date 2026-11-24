package hostutil

import (
	"runtime"
	"strings"
	"sync"
)

var (
	osName   = sync.OnceValue(readOSName)
	machine  = sync.OnceValue(readMachine)
	release  = sync.OnceValue(readRelease)
	platform = sync.OnceValue(readPlatform)
)

// Name the operating system gives itself, for people and headers: the kernel name uname reports on Unix (Linux, Darwin, FreeBSD), Windows on Windows
func OSName() string { return osName() }

// Hardware name the operating system reports for this process: x86_64, aarch64 or arm64 on Unix, AMD64, ARM64 or x86 on Windows
func Machine() string { return machine() }

// Kernel release on Unix, the Windows product release such as 10, 11 or 2022Server on Windows, empty when the system does not say
func Release() string { return release() }

// One-line label of this platform safe in file names and headers, such as Linux-6.12.1-x86_64-with-glibc2.40 or macOS-15.1-arm64-64bit-Mach-O
func Platform() string { return platform() }

// Operating system name of a build target when the kernel cannot be asked
func goosName() string {
	switch runtime.GOOS {
	case "linux", "android":
		return "Linux"
	case "darwin", "ios":
		return "Darwin"
	case "windows":
		return "Windows"
	case "freebsd":
		return "FreeBSD"
	case "openbsd":
		return "OpenBSD"
	case "netbsd":
		return "NetBSD"
	case "dragonfly":
		return "DragonFly"
	case "solaris", "illumos":
		return "SunOS"
	case "aix":
		return "AIX"
	case "plan9":
		return "Plan9"
	}
	return runtime.GOOS
}

// Non-empty parts joined with dashes, spaces, slashes and punctuation turned into file name safe characters, unknown parts dropped
func joinLabel(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	replacer := strings.NewReplacer(" ", "_", "/", "-", "\\", "-", ":", "-", ";", "-", "\"", "-", "(", "-", ")", "-")
	label := strings.ReplaceAll(replacer.Replace(strings.Join(kept, "-")), "unknown", "")
	for strings.Contains(label, "--") {
		label = strings.ReplaceAll(label, "--", "-")
	}
	return strings.Trim(label, "-")
}
