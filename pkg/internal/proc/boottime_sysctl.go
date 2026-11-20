//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package proc

import "golang.org/x/sys/unix"

// Boot epoch in seconds like psutil boot_time, read from the kern.boottime sysctl
func BootTime() (float64, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return 0, err
	}
	return float64(tv.Sec) + float64(tv.Usec)/1e6, nil
}
