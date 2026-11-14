//go:build !windows

package googlemeet

import "syscall"

func umask(m int) int { return syscall.Umask(m) }
