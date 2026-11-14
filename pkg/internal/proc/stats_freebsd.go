package proc

import (
	"encoding/binary"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Descriptor counts are read through the kern.proc.filedesc sysctl
const NumFDsAvailable = true

// Count of descriptors pid holds open, the kinfo_file records of its descriptor table
func NumFDs(pid int) (int, error) {
	raw, err := unix.SysctlRaw("kern.proc.filedesc", pid)
	if err != nil {
		return 0, kinfoError(pid, "kinfo_getfile()", err)
	}
	count := 0
	for len(raw) >= kfMinRecordSize {
		size := int(binary.NativeEndian.Uint32(raw))
		if size < kfMinRecordSize || size > len(raw) {
			return 0, os.NewSyscallError("kinfo_getfile()", syscall.EINVAL)
		}
		raw = raw[size:]
		count++
	}
	return count, nil
}
