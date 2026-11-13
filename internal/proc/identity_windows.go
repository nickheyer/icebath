package proc

import (
	"errors"
	"fmt"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Opens pid with access, mapping a missing pid to ErrNotFound and a refusal to ErrAccessDenied
func openProcess(pid int, access uint32) (windows.Handle, error) {
	if pid < 0 {
		return 0, fmt.Errorf("pid %d: %w", pid, ErrNotFound)
	}
	h, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		switch {
		case errors.Is(err, windows.ERROR_INVALID_PARAMETER):
			return 0, fmt.Errorf("pid %d: %w", pid, ErrNotFound)
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return 0, denied(pid, err)
		}
		return 0, fmt.Errorf("pid %d: %w", pid, err)
	}
	return h, nil
}

// Whether the process behind h has exited, a handle kept open after exit included
func handleExited(h windows.Handle) bool {
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev != uint32(windows.WAIT_TIMEOUT)
}

// Creation time of the process behind h, the native list answering when the times are refused
func handleStart(h windows.Handle, pid int) (float64, error) {
	if handleExited(h) {
		return 0, fmt.Errorf("pid %d: %w", pid, ErrNotFound)
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return listedStart(pid)
		}
		return 0, fmt.Errorf("pid %d: %w", pid, err)
	}
	return filetimeSeconds(int64(creation.HighDateTime)<<32 | int64(creation.LowDateTime)), nil
}

// Creation time of pid from the native system process list
func listedStart(pid int) (float64, error) {
	p, err := systemProcess(pid)
	if err != nil {
		return 0, err
	}
	return filetimeSeconds(p.create), nil
}

// Creation time of pid in seconds since the epoch, read from the native list when the process refuses a handle
func Started(pid int) (float64, error) {
	h, err := openProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE)
	if errors.Is(err, ErrAccessDenied) {
		return listedStart(pid)
	}
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	return handleStart(h, pid)
}

// Boot relative starts exist only on Linux, errors.ErrUnsupported here
func SinceBoot(pid int) (float64, error) {
	return 0, fmt.Errorf("start since boot on Windows: %w", errors.ErrUnsupported)
}

// Start stamp of pid that never moves with wall clock steps, the creation time recorded once on Windows
func Birth(pid int) (float64, error) { return Started(pid) }

// Integer start fingerprint pid and lock records store, the creation time in centiseconds outside Linux
func Fingerprint(pid int) (int64, error) {
	started, err := Started(pid)
	if err != nil {
		return 0, err
	}
	return int64(math.RoundToEven(started * 100)), nil
}

// SYSTEM_TIMEOFDAY_INFORMATION, whose first field is the boot instant
type timeOfDay struct {
	BootTime, CurrentTime, TimeZoneBias int64
	TimeZoneID, Reserved                uint32
	BootTimeBias, SleepTimeBias         uint64
}

// Instant the system booted in seconds since the epoch, as the kernel keeps it
func BootTime() (float64, error) {
	var tod timeOfDay
	var ret uint32
	if err := windows.NtQuerySystemInformation(windows.SystemTimeOfDayInformation, unsafe.Pointer(&tod), uint32(unsafe.Sizeof(tod)), &ret); err != nil {
		return 0, fmt.Errorf("system time of day: %w", err)
	}
	return filetimeSeconds(tod.BootTime), nil
}

// Boot identifiers scope Linux boot relative starts, errors.ErrUnsupported here
func BootID() (string, error) {
	return "", fmt.Errorf("boot id on Windows: %w", errors.ErrUnsupported)
}

// Scheduling state of pid, stopped when every thread is suspended
func StatusOf(pid int) (Status, error) {
	p, err := systemProcess(pid)
	if err != nil {
		return 0, err
	}
	if p.suspended {
		return Stopped, nil
	}
	return Running, nil
}

// Pid of the process that created pid, which Windows keeps even after that parent exits
func ParentOf(pid int) (int, error) {
	p, err := systemProcess(pid)
	if err != nil {
		return 0, err
	}
	return p.ppid, nil
}

// Windows collects exited processes itself, so a running incarnation has never become a zombie
func notExited(int) bool { return true }
