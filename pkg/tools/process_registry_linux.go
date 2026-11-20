//go:build linux

package tools

import "golang.org/x/sys/unix"

// Physical memory of the host in bytes
func physicalMemoryBytes() (int64, bool) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0, false
	}
	return int64(info.Totalram) * int64(info.Unit), true
}
