package proc

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// One entry of the native system process list, which answers for processes OpenProcess refuses
type sysProc struct {
	pid, ppid    int
	create       int64
	name         string
	threads      int
	handles      int
	rss, vms     uint64
	user, kernel int64
	suspended    bool
}

// SYSTEM_THREAD_INFORMATION entries that follow each process entry
type systemThreadInformation struct {
	KernelTime, UserTime, CreateTime int64
	WaitTime                         uint32
	StartAddress                     uintptr
	UniqueProcess, UniqueThread      uintptr
	Priority, BasePriority           int32
	ContextSwitches                  uint32
	ThreadState, WaitReason          uint32
}

// KTHREAD_STATE Waiting together with KWAIT_REASON Suspended
const (
	threadWaiting   = 5
	waitSuspended   = 5
	epochFiletime   = 116444736000000000
	systemListStart = 1 << 20
)

// Every process the kernel lists, read in one NtQuerySystemInformation call
func systemProcesses() ([]sysProc, error) {
	size := uint32(systemListStart)
	for range 12 {
		buf := make([]byte, size)
		var ret uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), size, &ret)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			size = max(ret+ret/4, size*2)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("system process list: %w", err)
		}
		return parseSystemProcesses(buf), nil
	}
	return nil, fmt.Errorf("system process list kept outgrowing %d bytes", size)
}

// Decodes the packed process entries and their thread arrays
func parseSystemProcesses(buf []byte) []sysProc {
	const procSize = int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{}))
	const threadSize = int(unsafe.Sizeof(systemThreadInformation{}))
	var out []sysProc
	for off := 0; off+procSize <= len(buf); {
		info := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
		p := sysProc{
			pid: int(info.UniqueProcessID), ppid: int(info.InheritedFromUniqueProcessID),
			create: info.CreateTime, name: info.ImageName.String(), threads: int(info.NumberOfThreads),
			handles: int(info.HandleCount), rss: uint64(info.WorkingSetSize), vms: uint64(info.PagefileUsage),
			user: info.UserTime, kernel: info.KernelTime, suspended: true,
		}
		for i := range p.threads {
			at := off + procSize + i*threadSize
			if at+threadSize > len(buf) {
				p.suspended = false
				break
			}
			t := (*systemThreadInformation)(unsafe.Pointer(&buf[at]))
			if t.ThreadState != threadWaiting || t.WaitReason != waitSuspended {
				p.suspended = false
				break
			}
		}
		out = append(out, p)
		if info.NextEntryOffset == 0 {
			break
		}
		off += int(info.NextEntryOffset)
	}
	return out
}

// Native list entry of pid
func systemProcess(pid int) (sysProc, error) {
	all, err := systemProcesses()
	if err != nil {
		return sysProc{}, err
	}
	for _, p := range all {
		if p.pid == pid {
			return p, nil
		}
	}
	return sysProc{}, fmt.Errorf("pid %d: %w", pid, ErrNotFound)
}

// Seconds since the epoch of a FILETIME count
func filetimeSeconds(ft int64) float64 { return float64(ft-epochFiletime) / 1e7 }
