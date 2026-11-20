//go:build !windows

package tools

import (
	"errors"
	"math"
	"syscall"
)

// Process group id of pid
func getpgid(pid int) (int, error) { return syscall.Getpgid(pid) }

// Signals every member of process group pgid
func killpg(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }

// PidExistsStdlib reports pid alive without signalling it, a zombie dead, another user's process alive and pid zero alive
func PidExistsStdlib(pid int) (bool, error) {
	zombie, err := posixIsZombie(pid)
	if err != nil {
		return false, err
	}
	if zombie {
		return false, nil
	}
	if pid > math.MaxInt32 {
		return false, errors.New("signed integer is greater than maximum")
	}
	if pid < math.MinInt32 {
		return false, errors.New("signed integer is less than minimum")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return errors.Is(err, syscall.EPERM), nil
	}
	return true, nil
}
