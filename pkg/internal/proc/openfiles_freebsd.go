package proc

import (
	"encoding/binary"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// FreeBSD enumerates descriptors through the kern.proc.filedesc sysctl
const OpenFilesAvailable = true

const (
	kfTypeVnode     = 1
	kfVTypeReg      = 1
	kfVnodeTypeOff  = 32
	kfPathOff       = 368
	kfMinRecordSize = 36
)

// Regular files pid holds open through vnode descriptors, read from packed kinfo_file records
func OpenFiles(pid int) ([]OpenFile, error) {
	kp, err := unix.SysctlRaw("kern.proc.pid", pid)
	if err != nil {
		return nil, kinfoError(pid, "sysctl(kinfo_proc)", err)
	}
	if len(kp) == 0 {
		return nil, ErrNoSuchProcess
	}
	raw, err := unix.SysctlRaw("kern.proc.filedesc", pid)
	if err != nil {
		return nil, kinfoError(pid, "kinfo_getfile()", err)
	}
	out := []OpenFile{}
	for len(raw) >= kfMinRecordSize {
		size := int(binary.NativeEndian.Uint32(raw))
		if size < kfMinRecordSize || size > len(raw) {
			return nil, os.NewSyscallError("kinfo_getfile()", syscall.EINVAL)
		}
		rec := raw[:size]
		raw = raw[size:]
		kfType := int32(binary.NativeEndian.Uint32(rec[4:]))
		fd := int(int32(binary.NativeEndian.Uint32(rec[8:])))
		vtype := int32(binary.NativeEndian.Uint32(rec[kfVnodeTypeOff:]))
		if kfType != kfTypeVnode || vtype != kfVTypeReg {
			continue
		}
		path := ""
		if len(rec) > kfPathOff {
			path, _, _ = strings.Cut(string(rec[kfPathOff:]), "\x00")
		}
		out = append(out, OpenFile{Path: path, FD: fd})
	}
	return out, nil
}
