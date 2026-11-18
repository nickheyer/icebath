//go:build darwin

package tty

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Libc sigaction record, its handler, mask and flags
type libcSigaction [4]uint64

var libc_sigaction_trampoline_addr uintptr

//go:cgo_import_dynamic libc_sigaction sigaction "/usr/lib/libSystem.B.dylib"

//go:linkname syscall_rawSyscall syscall.rawSyscall
func syscall_rawSyscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

func sigaction(act, old *libcSigaction) error {
	if _, _, e := syscall_rawSyscall(libc_sigaction_trampoline_addr, uintptr(unix.SIGTSTP), uintptr(unsafe.Pointer(act)), uintptr(unsafe.Pointer(old))); e != 0 {
		return e
	}
	return nil
}

// Sends SIGTSTP to the process group at its default action, the returned call putting the runtime handler back
func stopDefault() (func() error, error) {
	var dfl, saved libcSigaction
	if err := sigaction(&dfl, &saved); err != nil {
		return nil, err
	}
	restore := func() error { return sigaction(&saved, nil) }
	if err := unix.Kill(0, unix.SIGTSTP); err != nil {
		return nil, errors.Join(err, restore())
	}
	return restore, nil
}

// Parent process id of a process from the kernel process table
func parentOf(pid int) (int, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	return int(kp.Eproc.Ppid), nil
}
