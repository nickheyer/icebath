package proc

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Thread count of pid from the NtQuerySystemInformation process list like psutil num_threads
func NumThreads(pid int) (int, error) {
	size := uint32(512 << 10)
	for range 8 {
		buf := make([]byte, size)
		var ret uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), size, &ret)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			size = max(ret+64<<10, size*2)
			continue
		}
		if err != nil {
			return 0, err
		}
		for off := 0; off+int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{})) <= len(buf); {
			info := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
			if int(info.UniqueProcessID) == pid {
				return int(info.NumberOfThreads), nil
			}
			if info.NextEntryOffset == 0 {
				break
			}
			off += int(info.NextEntryOffset)
		}
		return 0, ErrNoSuchProcess
	}
	return 0, fmt.Errorf("system process list kept outgrowing %d bytes", size)
}
