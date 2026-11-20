//go:build openbsd || netbsd

package proc

import (
	"encoding/binary"
	"errors"
	"syscall"
	"testing"
)

func TestOpenFilesKeepsRegularVnodesWithEmptyPaths(t *testing.T) {
	record := func(ftype, vtype uint32, fd int32) []byte {
		rec := make([]byte, kinfoFileSize)
		binary.NativeEndian.PutUint32(rec[kinfoFTypeOff:], ftype)
		binary.NativeEndian.PutUint32(rec[kinfoVTypeOff:], vtype)
		binary.NativeEndian.PutUint32(rec[kinfoFDOff:], uint32(fd))
		return rec
	}
	var raw []byte
	raw = append(raw, record(dtypeVnode, vtypeReg, 3)...)
	raw = append(raw, record(2, vtypeReg, 4)...)
	raw = append(raw, record(dtypeVnode, 2, 5)...)
	raw = append(raw, record(dtypeVnode, vtypeReg, 7)...)
	saved := sysctlFiles
	t.Cleanup(func() { sysctlFiles = saved })
	sysctlFiles = func(int) ([]byte, error) { return raw, nil }
	got, err := OpenFiles(42)
	if err != nil || len(got) != 2 || got[0] != (OpenFile{FD: 3}) || got[1] != (OpenFile{FD: 7}) {
		t.Fatalf("OpenFiles = %v, %v", got, err)
	}
	for errno, want := range map[syscall.Errno]error{syscall.ESRCH: ErrNoSuchProcess, syscall.EPERM: ErrAccessDenied} {
		sysctlFiles = func(int) ([]byte, error) { return nil, errno }
		if _, err := OpenFiles(42); !errors.Is(err, want) {
			t.Fatalf("%v mapped to %v", errno, err)
		}
	}
	sysctlFiles = func(int) ([]byte, error) { return nil, syscall.ESRCH }
	if got, err := OpenFiles(0); pidZeroListsNothing && (err != nil || len(got) != 0) {
		t.Fatalf("pid 0 = %v, %v", got, err)
	}
}
