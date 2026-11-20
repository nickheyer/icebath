package proc

import (
	"encoding/binary"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// FreeBSD reads every process cwd from its descriptor table
const CwdAvailable = true

const kfFDTypeCwd = -1

// Working directory of pid from the KF_FD_TYPE_CWD record of kern.proc.filedesc
func Cwd(pid int) (string, error) {
	raw, err := unix.SysctlRaw("kern.proc.filedesc", pid)
	if err != nil {
		return "", kinfoError(pid, "kinfo_getfile()", err)
	}
	for len(raw) >= kfMinRecordSize {
		size := int(binary.NativeEndian.Uint32(raw))
		if size < kfMinRecordSize || size > len(raw) {
			return "", os.NewSyscallError("kinfo_getfile()", syscall.EINVAL)
		}
		rec := raw[:size]
		raw = raw[size:]
		if int32(binary.NativeEndian.Uint32(rec[8:])) == kfFDTypeCwd && len(rec) > kfPathOff {
			path, _, _ := strings.Cut(string(rec[kfPathOff:]), "\x00")
			return path, nil
		}
	}
	return "", ErrAccessDenied
}
