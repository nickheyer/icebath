//go:build windows

package prompt

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// Name of the host with its Windows version, 11 from build 22000 on
func hostLabel(bool) string {
	v := windows.RtlGetVersion()
	version := fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	switch {
	case v.MajorVersion == 10 && v.BuildNumber >= 22000:
		version = fmt.Sprintf("11, build %d", v.BuildNumber)
	case v.MajorVersion == 10:
		version = fmt.Sprintf("10, build %d", v.BuildNumber)
	}
	return "Windows (" + version + ")"
}
