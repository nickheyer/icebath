//go:build darwin

package state

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// libproc entry points over the __proc_info system call they wrap, zero on failure as libproc returns
type systemLibproc struct{}

// Raw __proc_info call returning the byte count, zero when the kernel refuses
func procInfoCall(call, pid, flavor int, arg uint64, buf []byte) int {
	var p unsafe.Pointer
	if len(buf) > 0 {
		p = unsafe.Pointer(&buf[0])
	}
	r, _, e := unix.Syscall6(unix.SYS_PROC_INFO, uintptr(call), uintptr(pid), uintptr(flavor), uintptr(arg), uintptr(p), uintptr(len(buf)))
	if e != 0 || int32(r) < 0 {
		return 0
	}
	return int(int32(r))
}

func (systemLibproc) ProcListPIDs(typ, typeinfo uint32, buf []byte) int {
	return procInfoCall(1, int(typ), int(typeinfo), 0, buf)
}

func (systemLibproc) ProcPIDInfo(pid, flavor int, arg uint64, buf []byte) int {
	return procInfoCall(2, pid, flavor, arg, buf)
}

func (systemLibproc) ProcPIDFDInfo(pid, fd, flavor int, buf []byte) int {
	return procInfoCall(3, pid, flavor, uint64(fd), buf)
}

// libproc of this process, always resolvable on macOS
func loadDarwinLibproc() (darwinLibprocAPI, error) { return systemLibproc{}, nil }
