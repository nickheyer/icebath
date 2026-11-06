//go:build openbsd || netbsd

package proc

import (
	"encoding/binary"
	"errors"
	"math"
	"syscall"

	"golang.org/x/sys/unix"
)

// psutil reads descriptors on this kernel through the kern file sysctl by pid
const OpenFilesAvailable = true

const (
	kernFileByPID = 2
	dtypeVnode    = 1
	vtypeReg      = 1
)

// Regular files pid holds open as psutil kinfo_getfile lists them, the path empty because kinfo_file carries none
func OpenFiles(pid int) ([]OpenFile, error) {
	raw, err := sysctlFiles(pid)
	if err != nil {
		if pidZeroListsNothing && pid == 0 && errors.Is(err, syscall.ESRCH) {
			return []OpenFile{}, nil
		}
		return nil, kinfoError(pid, "kinfo_getfile()", err)
	}
	out := []OpenFile{}
	for len(raw) >= kinfoFileSize {
		rec := raw[:kinfoFileSize]
		raw = raw[kinfoFileSize:]
		if binary.NativeEndian.Uint32(rec[kinfoFTypeOff:]) != dtypeVnode || binary.NativeEndian.Uint32(rec[kinfoVTypeOff:]) != vtypeReg {
			continue
		}
		out = append(out, OpenFile{Path: "", FD: int(int32(binary.NativeEndian.Uint32(rec[kinfoFDOff:])))})
	}
	return out, nil
}

// Raw sysctl reader of x/sys/unix resolving the node name to its mib
var sysctlRaw = unix.SysctlRaw

// Packed kinfo_file records of pid with the record count left to the buffer the size query sized
var sysctlFiles = func(pid int) ([]byte, error) {
	return sysctlRaw(kinfoFileNode, kernFileByPID, pid, kinfoFileSize, math.MaxInt32)
}
