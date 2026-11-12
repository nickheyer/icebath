package connections

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Creation time of a file from its Win32 attribute data
func fileBirthtime(path string) (time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, data.CreationTime.Nanoseconds()), true
}

// Kernel version as major.minor.build, the form Node os.release reports on Windows
func osRelease() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
}

// User preferred UI languages in priority order
func preferredUILanguages() []string {
	langs, err := windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME)
	if err != nil {
		return nil
	}
	return langs
}
