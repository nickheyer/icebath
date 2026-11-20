//go:build windows

package tools

import (
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Windows has no process groups, the lookup fails as os.getpgid is absent there
func getpgid(int) (int, error) { return 0, syscall.EWINDOWS }

// Windows has no process groups, the signal fails as os.killpg is absent there
func killpg(int, syscall.Signal) error { return syscall.EWINDOWS }

// PidExistsStdlib reports pid alive through OpenProcess and a zero wait, access denied meaning alive
func PidExistsStdlib(pid int) (bool, error) { return proc.Exists(pid), nil }
