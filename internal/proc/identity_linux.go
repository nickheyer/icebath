package proc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// Kernel clock ticks per second from the auxiliary vector, the C library default of 100 when the vector omits it
var clockTicks = sync.OnceValue(func() float64 {
	data, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 100
	}
	word := int(unsafe.Sizeof(uintptr(0)))
	for i := 0; i+2*word <= len(data); i += 2 * word {
		var key, val uint64
		if word == 8 {
			key, val = binary.NativeEndian.Uint64(data[i:]), binary.NativeEndian.Uint64(data[i+8:])
		} else {
			key, val = uint64(binary.NativeEndian.Uint32(data[i:])), uint64(binary.NativeEndian.Uint32(data[i+4:]))
		}
		if key == 17 && val > 0 {
			return float64(val)
		}
		if key == 0 {
			break
		}
	}
	return 100
})

func procPath(pid int, name string) string { return "/proc/" + itoa(pid) + "/" + name }

// Maps a procfs failure for pid to ErrNotFound or ErrAccessDenied
func procError(pid int, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ESRCH):
		return fmt.Errorf("pid %d: %w", pid, ErrNotFound)
	case errors.Is(err, fs.ErrPermission):
		return denied(pid, err)
	}
	return err
}

// Reads one procfs file of pid
func readProc(pid int, name string) ([]byte, error) {
	data, err := os.ReadFile(procPath(pid, name))
	if err != nil {
		return nil, procError(pid, err)
	}
	return data, nil
}

// Fields of a stat line after the command name, which may hold spaces and parentheses
func parseStat(pid int, data []byte) ([]string, error) {
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return nil, fmt.Errorf("pid %d: malformed stat line", pid)
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 22 {
		return nil, fmt.Errorf("pid %d: short stat line", pid)
	}
	return fields, nil
}

func statFields(pid int) ([]string, error) {
	data, err := readProc(pid, "stat")
	if err != nil {
		return nil, err
	}
	return parseStat(pid, data)
}

// Kernel start time of pid in clock ticks since boot
func startTicks(pid int) (uint64, error) {
	fields, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

// Creation time of pid in seconds since the epoch, the boot time read afresh plus the kernel start ticks
func Started(pid int) (float64, error) {
	ticks, err := startTicks(pid)
	if err != nil {
		return 0, err
	}
	boot, err := BootTime()
	if err != nil {
		return 0, err
	}
	return boot + float64(ticks)/clockTicks(), nil
}

// Seconds from boot to the start of pid, a value no wall clock step moves
func SinceBoot(pid int) (float64, error) {
	ticks, err := startTicks(pid)
	if err != nil {
		return 0, err
	}
	return float64(ticks) / clockTicks(), nil
}

// Start stamp of pid that never moves with wall clock steps, the seconds since boot on Linux
func Birth(pid int) (float64, error) { return SinceBoot(pid) }

// Integer start fingerprint pid and lock records store, the raw kernel start ticks on Linux
func Fingerprint(pid int) (int64, error) {
	ticks, err := startTicks(pid)
	if err != nil {
		return 0, err
	}
	return int64(ticks), nil
}

// Instant the system booted in seconds since the epoch, from the btime line of /proc/stat
func BootTime() (float64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "btime"); ok {
			return strconv.ParseFloat(strings.TrimSpace(rest), 64)
		}
	}
	return 0, errors.New("/proc/stat has no btime line")
}

// Kernel identifier of the running boot
func BootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errors.New("boot_id is empty")
	}
	return id, nil
}

// State of a stat line letter
func statusLetter(c byte) Status {
	switch c {
	case 'R':
		return Running
	case 'S':
		return Sleeping
	case 'D':
		return DiskSleep
	case 'T':
		return Stopped
	case 't':
		return TracingStop
	case 'Z':
		return Zombie
	case 'X', 'x':
		return Dead
	case 'K':
		return WakeKill
	case 'W':
		return Waking
	case 'P':
		return Parked
	case 'I':
		return Idle
	}
	return 0
}

// Scheduling state of pid
func StatusOf(pid int) (Status, error) {
	fields, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	return statusLetter(fields[0][0]), nil
}

// Parent pid of pid as the kernel reports it now
func ParentOf(pid int) (int, error) {
	fields, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(fields[1])
}

// Whether pid leads a thread group, a thread id answering a signal probe without being a process
func groupLeader(pid int) bool {
	data, err := os.ReadFile(procPath(pid, "status"))
	if err != nil {
		return true
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "Tgid:"); ok {
			tgid, err := strconv.Atoi(strings.TrimSpace(rest))
			return err != nil || tgid == pid
		}
	}
	return true
}

// Pid zero is never a listed process on Linux
func zeroExists() bool { return false }

// Whether pid is present and has not exited into a zombie
func notExited(pid int) bool {
	st, err := StatusOf(pid)
	return err == nil && !st.Exited()
}
