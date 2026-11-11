package selfupdate

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"

	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
)

// Sets the access and modification times of st on the open file itself through libc futimens
func utimeHandle(out *os.File, st os.FileInfo) error {
	futimens, err := purego.Dlsym(purego.RTLD_DEFAULT, "futimens")
	if err != nil {
		return err
	}
	times := [2]unix.Timespec{unix.NsecToTimespec(pyshutil.FileAtime(st).UnixNano()), unix.NsecToTimespec(st.ModTime().UnixNano())}
	status, _, errno := purego.SyscallN(futimens, out.Fd(), uintptr(unsafe.Pointer(&times)))
	runtime.KeepAlive(&times)
	if int32(status) != 0 {
		return &os.PathError{Op: "futimens", Path: out.Name(), Err: syscall.Errno(errno)}
	}
	return nil
}
