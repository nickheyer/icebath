package proc

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin enumerates descriptors through proc_info
const OpenFilesAvailable = true

const (
	procInfoCallPIDInfo     = 2
	procInfoCallPIDFDInfo   = 3
	procPIDListFDs          = 1
	procPIDFDVnodePathInfo  = 2
	proxFDTypeVnode         = 1
	procFDInfoSize          = 8
	vnodeFDInfoWithPathSize = 1200
	vipPathOffset           = 176
)

// Raw __proc_info call returning the byte count or the errno it failed with
func procInfo(call, pid, flavor int, arg uint64, buf []byte) (int, syscall.Errno) {
	var p unsafe.Pointer
	if len(buf) > 0 {
		p = unsafe.Pointer(&buf[0])
	}
	r, _, e := unix.Syscall6(unix.SYS_PROC_INFO, uintptr(call), uintptr(pid), uintptr(flavor), uintptr(arg), uintptr(p), uintptr(len(buf)))
	if e != 0 {
		return 0, e
	}
	return int(r), 0
}

// psutil raise_for_pid, mapping errno to the process error psutil wrap_exceptions raises
func raiseForPID(pid int, name string, errno syscall.Errno) error {
	switch {
	case errno == syscall.ESRCH:
		return ErrNoSuchProcess
	case errno == syscall.EPERM || errno == syscall.EACCES:
		return ErrAccessDenied
	case errno != 0:
		return os.NewSyscallError(name, errno)
	case unix.Kill(pid, 0) == syscall.ESRCH:
		return ErrNoSuchProcess
	}
	return errors.New(name + " syscall failed")
}

// Descriptor table of pid as proc_fdinfo records, grown until the kernel leaves room to spare
func listFDs(pid int) ([]byte, error) {
	for {
		size, errno := procInfo(procInfoCallPIDInfo, pid, procPIDListFDs, 0, nil)
		if size <= 0 {
			return nil, raiseForPID(pid, "proc_pidinfo(PROC_PIDLISTFDS) 1/2", errno)
		}
		buf := make([]byte, size+procFDInfoSize*16)
		n, errno := procInfo(procInfoCallPIDInfo, pid, procPIDListFDs, 0, buf)
		if n <= 0 {
			return nil, raiseForPID(pid, "proc_pidinfo(PROC_PIDLISTFDS) 2/2", errno)
		}
		if n+procFDInfoSize < len(buf) {
			return buf[:n-n%procFDInfoSize], nil
		}
	}
}

// Regular files pid holds open through vnode descriptors, empty for the kernel task
func OpenFiles(pid int) ([]OpenFile, error) {
	if pid == 0 {
		return []OpenFile{}, nil
	}
	fds, err := listFDs(pid)
	if err != nil {
		return nil, err
	}
	out := []OpenFile{}
	info := make([]byte, vnodeFDInfoWithPathSize)
	for off := 0; off < len(fds); off += procFDInfoSize {
		fd := int(*(*int32)(unsafe.Pointer(&fds[off])))
		if *(*uint32)(unsafe.Pointer(&fds[off+4])) != proxFDTypeVnode {
			continue
		}
		n, errno := procInfo(procInfoCallPIDFDInfo, pid, procPIDFDVnodePathInfo, uint64(fd), info)
		if n <= 0 || n < vnodeFDInfoWithPathSize {
			if errno == syscall.ENOENT || errno == syscall.EBADF {
				continue
			}
			return nil, raiseForPID(pid, "proc_pidinfo(PROC_PIDFDVNODEPATHINFO)", errno)
		}
		raw := info[vipPathOffset:]
		path, _, _ := strings.Cut(string(raw), "\x00")
		regular, err := isFileStrict(path)
		if err != nil {
			return nil, ErrAccessDenied
		}
		if regular {
			out = append(out, OpenFile{Path: path, FD: fd})
		}
	}
	return out, nil
}
