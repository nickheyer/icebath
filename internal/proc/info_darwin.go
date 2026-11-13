package proc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// proc_info call numbers, flavors and record sizes of the macOS kernel
const (
	procInfoCallPIDInfo     = 2
	procInfoCallPIDFDInfo   = 3
	procPIDListFDs          = 1
	procPIDTaskInfo         = 4
	procPIDVnodePathInfo    = 9
	procPIDPathInfo         = 11
	procPIDFDVnodePathInfo  = 2
	proxFDTypeVnode         = 1
	procFDInfoSize          = 8
	taskInfoSize            = 96
	vnodePathInfoSize       = 2352
	cdirPathOffset          = 152
	vnodeFDInfoWithPathSize = 1200
	vnodeFDPathOffset       = 176
	pathInfoMaxSize         = 4096
)

// Raw proc_info call returning the byte count or the errno it failed with
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

// Maps a failed proc_info or sysctl read of pid to ErrNotFound, ErrAccessDenied or the system error
func infoError(pid int, op string, errno syscall.Errno) error {
	switch {
	case errno == unix.ESRCH:
		return fmt.Errorf("pid %d: %w", pid, ErrNotFound)
	case errno == unix.EPERM || errno == unix.EACCES:
		return denied(pid, errno)
	case !Exists(pid):
		return fmt.Errorf("pid %d: %w", pid, ErrNotFound)
	case errno != 0:
		return fmt.Errorf("pid %d: %s: %w", pid, op, errno)
	}
	return fmt.Errorf("pid %d: %s returned nothing", pid, op)
}

// Process name, the sixteen byte kernel name widened to the argv zero basename it truncates
func Name(pid int) (string, error) {
	kp, err := kinfo(pid)
	if err != nil {
		return "", err
	}
	short, _, _ := bytes.Cut(kp.Proc.P_comm[:], []byte{0})
	return widenName(pid, string(short))
}

// Executable path of pid
func Exe(pid int) (string, error) {
	buf := make([]byte, pathInfoMaxSize)
	n, errno := procInfo(procInfoCallPIDInfo, pid, procPIDPathInfo, 0, buf)
	if n <= 0 {
		return "", infoError(pid, "proc_pidpath", errno)
	}
	path, _, _ := bytes.Cut(buf[:n], []byte{0})
	return string(path), nil
}

// Argument vector and environment block of pid from the kernel argument area
func procArgs(pid int) ([]string, string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			if errno == unix.EINVAL && Alive(pid) {
				return nil, "", denied(pid, err)
			}
			if errno == unix.EINVAL {
				return nil, "", fmt.Errorf("pid %d: %w", pid, ErrNotFound)
			}
			return nil, "", infoError(pid, "kern.procargs2", errno)
		}
		return nil, "", err
	}
	if len(raw) < 4 {
		return nil, "", fmt.Errorf("pid %d: short argument area", pid)
	}
	argc := int(binary.NativeEndian.Uint32(raw[:4]))
	rest := raw[4:]
	if i := bytes.IndexByte(rest, 0); i >= 0 {
		rest = rest[i:]
	}
	rest = bytes.TrimLeft(rest, "\x00")
	argv := []string{}
	var env strings.Builder
	for i, part := range strings.Split(string(rest), "\x00") {
		if i < argc {
			argv = append(argv, part)
			continue
		}
		if part == "" {
			break
		}
		env.WriteString(part)
		env.WriteByte(0)
	}
	return argv, env.String(), nil
}

// Argument vector of pid
func Cmdline(pid int) ([]string, error) {
	argv, _, err := procArgs(pid)
	return argv, err
}

// Environment pid started with
func Environ(pid int) (map[string]string, error) {
	_, block, err := procArgs(pid)
	if err != nil {
		return nil, err
	}
	return parseEnviron(block, false), nil
}

// Working directory of pid from its current directory vnode
func Cwd(pid int) (string, error) {
	buf := make([]byte, vnodePathInfoSize)
	n, errno := procInfo(procInfoCallPIDInfo, pid, procPIDVnodePathInfo, 0, buf)
	if n <= 0 {
		return "", infoError(pid, "proc_pidinfo vnode path", errno)
	}
	path, _, _ := bytes.Cut(buf[cdirPathOffset:], []byte{0})
	return string(path), nil
}

// Real uid of pid
func RealUID(pid int) (int, error) {
	kp, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	return int(kp.Eproc.Pcred.P_ruid), nil
}

// Timebase frequency converting task info CPU ticks to nanoseconds
var timebase = sync.OnceValues(func() (uint64, error) {
	raw, err := unix.SysctlRaw("hw.tbfrequency")
	if err != nil {
		return 0, err
	}
	switch len(raw) {
	case 4:
		return uint64(binary.NativeEndian.Uint32(raw)), nil
	case 8:
		return binary.NativeEndian.Uint64(raw), nil
	}
	return 0, fmt.Errorf("hw.tbfrequency has %d bytes", len(raw))
})

// Descriptor table of pid as proc_fdinfo records, grown until the kernel leaves room to spare
func listFDs(pid int) ([]byte, error) {
	for {
		size, errno := procInfo(procInfoCallPIDInfo, pid, procPIDListFDs, 0, nil)
		if size <= 0 {
			return nil, infoError(pid, "proc_pidinfo fd list size", errno)
		}
		buf := make([]byte, size+procFDInfoSize*16)
		n, errno := procInfo(procInfoCallPIDInfo, pid, procPIDListFDs, 0, buf)
		if n <= 0 {
			return nil, infoError(pid, "proc_pidinfo fd list", errno)
		}
		if n+procFDInfoSize < len(buf) {
			return buf[:n-n%procFDInfoSize], nil
		}
	}
}

// Memory, thread, CPU and descriptor use of pid from its task info and descriptor table
func StatsOf(pid int) (Stats, error) {
	buf := make([]byte, taskInfoSize)
	n, errno := procInfo(procInfoCallPIDInfo, pid, procPIDTaskInfo, 0, buf)
	if n <= 0 {
		return Stats{}, infoError(pid, "proc_pidinfo task info", errno)
	}
	freq, err := timebase()
	if err != nil {
		return Stats{}, err
	}
	fds, err := listFDs(pid)
	if err != nil {
		return Stats{}, err
	}
	ns := func(off int) time.Duration {
		return time.Duration(float64(binary.NativeEndian.Uint64(buf[off:])) * 1e9 / float64(freq))
	}
	return Stats{
		VMS:         binary.NativeEndian.Uint64(buf[0:]),
		RSS:         binary.NativeEndian.Uint64(buf[8:]),
		User:        ns(16),
		System:      ns(24),
		Threads:     int(int32(binary.NativeEndian.Uint32(buf[84:]))),
		Descriptors: len(fds) / procFDInfoSize,
	}, nil
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
	for off := 0; off+procFDInfoSize <= len(fds); off += procFDInfoSize {
		fd := int(int32(binary.NativeEndian.Uint32(fds[off:])))
		if binary.NativeEndian.Uint32(fds[off+4:]) != proxFDTypeVnode {
			continue
		}
		n, errno := procInfo(procInfoCallPIDFDInfo, pid, procPIDFDVnodePathInfo, uint64(fd), info)
		if n < vnodeFDInfoWithPathSize {
			if errno == unix.ENOENT || errno == unix.EBADF {
				continue
			}
			return nil, infoError(pid, "proc_pidfdinfo vnode path", errno)
		}
		path, _, _ := bytes.Cut(info[vnodeFDPathOffset:], []byte{0})
		regular, err := regularFile(string(path))
		if err != nil {
			return nil, denied(pid, err)
		}
		if regular {
			out = append(out, OpenFile{Path: string(path), FD: fd})
		}
	}
	return out, nil
}
