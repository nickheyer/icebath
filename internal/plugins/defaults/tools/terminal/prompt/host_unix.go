//go:build !windows

package prompt

import (
	"os"
	"regexp"
	"runtime"

	"golang.org/x/sys/unix"
)

var productVersion = regexp.MustCompile(`<key>ProductVersion</key>\s*<string>([^<]*)</string>`)

// Name of the host: WSL, macOS with its version, else the kernel and its release
func hostLabel(wsl bool) string {
	var name unix.Utsname
	system, release := "", ""
	if unix.Uname(&name) == nil {
		system, release = unix.ByteSliceToString(name.Sysname[:]), unix.ByteSliceToString(name.Release[:])
	}
	switch {
	case wsl:
		return "WSL (Windows Subsystem for Linux)"
	case runtime.GOOS == "darwin":
		version := release
		if data, err := os.ReadFile("/System/Library/CoreServices/SystemVersion.plist"); err == nil {
			if m := productVersion.FindSubmatch(data); m != nil {
				version = string(m[1])
			}
		}
		return "macOS (" + version + ")"
	}
	return system + " (" + release + ")"
}
