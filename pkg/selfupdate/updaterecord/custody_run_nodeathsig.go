//go:build !linux && !windows

package updaterecord

import "syscall"

func deathSignalAttr() *syscall.SysProcAttr { return nil }
