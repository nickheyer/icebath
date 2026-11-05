//go:build !windows

package agent

import (
	"os"
	"regexp"
	"runtime"

	"golang.org/x/sys/unix"
)

// Kernel name and release like platform.system and platform.release
func hostSystemRelease() (string, string) {
	var name unix.Utsname
	if unix.Uname(&name) != nil {
		return "", ""
	}
	return unix.ByteSliceToString(name.Sysname[:]), unix.ByteSliceToString(name.Release[:])
}

var macProductVersionRe = regexp.MustCompile(`<key>ProductVersion</key>\s*<string>([^<]*)</string>`)

// macOS product version from SystemVersion.plist, empty when unreadable like platform.mac_ver
func macOSVersion() string {
	data, err := os.ReadFile("/System/Library/CoreServices/SystemVersion.plist")
	if err != nil {
		return ""
	}
	if m := macProductVersionRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

// Host label for a unix host, WSL first, then macOS with its product version, then kernel and release
func hostLabel(isWSL bool) string {
	system, release := hostSystemRelease()
	switch {
	case isWSL:
		return "WSL (Windows Subsystem for Linux)"
	case runtime.GOOS == "darwin":
		version := macOSVersion()
		if version == "" {
			version = release
		}
		return "macOS (" + version + ")"
	}
	return system + " (" + release + ")"
}
