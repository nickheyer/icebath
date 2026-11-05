//go:build windows

package agent

import "golang.org/x/sys/windows"

// Eleven for build 22000 and later, else ten, since the kernel reports 10 for both
func windowsMarketingVersion() string {
	if windows.RtlGetVersion().BuildNumber >= 22000 {
		return "11"
	}
	return "10"
}

// Host label for Windows naming the marketing version
func hostLabel(bool) string { return "Windows (" + windowsMarketingVersion() + ")" }
