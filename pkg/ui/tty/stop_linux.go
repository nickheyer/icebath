//go:build linux

package tty

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Kernel sigaction record, room for every architecture's layout
type kernelSigaction [8]uint64

func rtSigaction(act, old *kernelSigaction) error {
	if _, _, e := unix.RawSyscall6(unix.SYS_RT_SIGACTION, uintptr(unix.SIGTSTP), uintptr(unsafe.Pointer(act)), uintptr(unsafe.Pointer(old)), sigsetSize, 0, 0); e != 0 {
		return e
	}
	return nil
}

// Sends SIGTSTP to the process group at its default action, the returned call putting the runtime handler back
func stopDefault() (func() error, error) {
	var dfl, saved kernelSigaction
	if err := rtSigaction(&dfl, &saved); err != nil {
		return nil, err
	}
	restore := func() error { return rtSigaction(&saved, nil) }
	if err := unix.Kill(0, unix.SIGTSTP); err != nil {
		return nil, errors.Join(err, restore())
	}
	return restore, nil
}

// Parent process id of a process, read from its proc status
func parentOf(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	s := string(b)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(fields) < 2 {
		return 0, errors.New("tty: unreadable process status of " + strconv.Itoa(pid))
	}
	return strconv.Atoi(fields[1])
}
