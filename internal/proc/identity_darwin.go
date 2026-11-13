package proc

import (
	"errors"
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// Kernel p_stat values
const (
	darwinIdle   = 1
	darwinRun    = 2
	darwinSleep  = 3
	darwinStop   = 4
	darwinZombie = 5
)

// Kernel record of pid
func kinfo(pid int) (*unix.KinfoProc, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.EIO) || errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("pid %d: %w", pid, ErrNotFound)
		}
		return nil, err
	}
	if kp.Proc.P_pid == 0 && pid != 0 {
		return nil, fmt.Errorf("pid %d: %w", pid, ErrNotFound)
	}
	return kp, nil
}

// Creation time of pid in seconds since the epoch, the kernel's own unadjusted start
func Started(pid int) (float64, error) {
	kp, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	return float64(kp.Proc.P_starttime.Sec) + float64(kp.Proc.P_starttime.Usec)/1e6, nil
}

// Boot relative starts exist only on Linux, errors.ErrUnsupported here
func SinceBoot(pid int) (float64, error) {
	return 0, fmt.Errorf("start since boot on macOS: %w", errors.ErrUnsupported)
}

// Start stamp of pid that never moves with wall clock steps, the kernel start time recorded once on macOS
func Birth(pid int) (float64, error) { return Started(pid) }

// Integer start fingerprint pid and lock records store, the creation time in centiseconds outside Linux
func Fingerprint(pid int) (int64, error) {
	started, err := Started(pid)
	if err != nil {
		return 0, err
	}
	return int64(math.RoundToEven(started * 100)), nil
}

// Instant the system booted in seconds since the epoch
func BootTime() (float64, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return 0, err
	}
	return float64(tv.Sec) + float64(tv.Usec)/1e6, nil
}

// Boot identifiers scope Linux boot relative starts, errors.ErrUnsupported here
func BootID() (string, error) {
	return "", fmt.Errorf("boot id on macOS: %w", errors.ErrUnsupported)
}

// State of a kernel p_stat value
func darwinStatus(stat int8) Status {
	switch stat {
	case darwinIdle:
		return Idle
	case darwinRun:
		return Running
	case darwinSleep:
		return Sleeping
	case darwinStop:
		return Stopped
	case darwinZombie:
		return Zombie
	}
	return 0
}

// Scheduling state of pid
func StatusOf(pid int) (Status, error) {
	kp, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	return darwinStatus(kp.Proc.P_stat), nil
}

// Parent pid of pid as the kernel reports it now
func ParentOf(pid int) (int, error) {
	kp, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	return int(kp.Eproc.Ppid), nil
}

// Every pid a signal probe finds is a process on macOS
func groupLeader(int) bool { return true }

// Pid zero is the kernel task, which macOS lists
func zeroExists() bool { return true }

// Whether pid is present and has not exited into a zombie
func notExited(pid int) bool {
	st, err := StatusOf(pid)
	return err == nil && !st.Exited()
}
