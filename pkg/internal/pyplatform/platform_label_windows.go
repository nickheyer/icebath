//go:build windows

package pyplatform

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Major, minor and build numbers of Windows joined with dots
func version() string {
	info := windows.RtlGetVersion()
	return fmt.Sprintf("%d.%d.%d", info.MajorVersion, info.MinorVersion, info.BuildNumber)
}

// DNS host name of the computer, empty when Windows refuses
func node() string {
	name, _ := os.Hostname()
	return name
}

// Service pack label of Windows, the minor part only when it is not zero
func servicePack() string {
	info := windows.RtlGetVersion()
	if info.ServicePackMinor != 0 {
		return fmt.Sprintf("SP%d.%d", info.ServicePackMajor, info.ServicePackMinor)
	}
	return fmt.Sprintf("SP%d", info.ServicePackMajor)
}

// Platform string of a Windows host from its release name, version and service pack
func platformLabel() string {
	return joinPlatform(system(), release(), version(), servicePack())
}
