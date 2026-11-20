//go:build darwin

package tui

import (
	"encoding/binary"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// Bytes the system counts the peak resident size of a process in
	maxRSSUnit = 1
	// Call of proc_info that reads facts of one process
	procInfoCallPIDInfo = 2
	// Flavor of proc_info that answers the task information of a process
	procPIDTaskInfo = 4
	// Size of the task information and where its resident size sits
	taskInfoSize, taskInfoResidentOffset = 96, 8
)

// Resident set size of the process in bytes from its task information, zero when the kernel does not answer
func residentBytes() uint64 {
	buf := make([]byte, taskInfoSize)
	n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(os.Getpid()), procPIDTaskInfo, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 || int(n) < taskInfoResidentOffset+8 {
		return 0
	}
	return binary.LittleEndian.Uint64(buf[taskInfoResidentOffset:])
}

// Resident bytes of the process that no file backs, unknown on a system whose task information does not split them out
func anonymousBytes() uint64 { return 0 }

// Open file descriptors of the process, false when the descriptor folder does not answer
func openDescriptors() (int, bool) {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return 0, false
	}
	return len(entries), true
}

// Physical memory of the machine in bytes, zero when the system does not say
func physicalMemory() uint64 {
	size, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return size
}

// Memory limit of a control group, none on a system without control groups
func cgroupMemoryLimit() uint64 { return 0 }

// Memory map totals of the process, empty on a system without procfs
func smapsRollup() string { return "" }
