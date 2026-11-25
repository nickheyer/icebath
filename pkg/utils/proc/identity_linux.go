package proc

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Reads one procfs file of pid, ErrNoSuchProcess when the pid is gone
func procFile(pid int, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return nil, ErrNoSuchProcess
		}
		return nil, err
	}
	return data, nil
}

// Fields of /proc/pid/stat after the command name, ErrNoSuchProcess when the pid is gone
func statFields(pid int) ([]string, error) {
	data, err := procFile(pid, "stat")
	if err != nil {
		return nil, err
	}
	s := string(data)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return nil, errors.New("malformed /proc stat")
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 20 {
		return nil, errors.New("short /proc stat")
	}
	return fields, nil
}

// Kernel clock ticks per second from the auxiliary vector, 100 when unreadable
var clockTicksPerSecond = sync.OnceValue(func() float64 {
	data, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 100
	}
	word := 8
	if strconv.IntSize == 32 {
		word = 4
	}
	for i := 0; i+2*word <= len(data); i += 2 * word {
		var key, val uint64
		if word == 8 {
			key = binary.LittleEndian.Uint64(data[i:])
			val = binary.LittleEndian.Uint64(data[i+8:])
		} else {
			key = uint64(binary.LittleEndian.Uint32(data[i:]))
			val = uint64(binary.LittleEndian.Uint32(data[i+4:]))
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

// Boot epoch from /proc/stat btime, re read on every call like psutil
func bootTime() (float64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "btime ") {
			return strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 64)
		}
	}
	return 0, errors.New("btime missing from /proc/stat")
}

// Seconds between boot and the start of pid
func startSeconds(pid int) (float64, error) {
	fields, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	ticks, err := strconv.ParseFloat(fields[19], 64)
	if err != nil {
		return 0, err
	}
	return ticks / clockTicksPerSecond(), nil
}

// Seconds between boot and the start of pid, false when unreadable
func BootStart(pid int) (float64, bool) {
	secs, err := startSeconds(pid)
	return secs, err == nil
}

// Creation epoch of pid like psutil create_time
func Birth(pid int) (float64, error) {
	secs, err := startSeconds(pid)
	if err != nil {
		return 0, err
	}
	boot, err := bootTime()
	if err != nil {
		return 0, err
	}
	return secs + boot, nil
}

// Boot relative start of pid, the stamp worker records compare against
func WorkerBirth(pid int) (float64, error) { return startSeconds(pid) }

// Status letter of pid with dead folded to X, the source psutil reads its status from
func State(pid int) (byte, error) {
	fields, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	if fields[0][0] == 'x' {
		return 'X', nil
	}
	return fields[0][0], nil
}

// Whether pid exited and waits to be reaped
func Zombie(pid int) bool {
	fields, err := statFields(pid)
	return err == nil && fields[0] == "Z"
}

// Parent pid of pid, false when unreadable
func PPID(pid int) (int, bool) {
	fields, err := statFields(pid)
	if err != nil {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// Environment of pid parsed from /proc/pid/environ
func Environ(pid int) (map[string]string, error) {
	data, err := procFile(pid, "environ")
	if err != nil {
		return nil, err
	}
	return parseEnvironBlock(string(data)), nil
}

// Argument vector of pid from /proc/pid/cmdline
func Cmdline(pid int) ([]string, error) {
	data, err := procFile(pid, "cmdline")
	if err != nil {
		return nil, err
	}
	return parseCmdline(string(data)), nil
}

// Splits a cmdline file like psutil, falling back to spaces for processes that rewrote their title
func parseCmdline(data string) []string {
	if data == "" {
		return []string{}
	}
	sep := " "
	if strings.HasSuffix(data, "\x00") {
		sep = "\x00"
	}
	data = strings.TrimSuffix(data, sep)
	parts := strings.Split(data, sep)
	if sep == "\x00" && len(parts) == 1 && strings.Contains(data, " ") {
		parts = strings.Split(data, " ")
	}
	return parts
}

// Numeric entries of /proc like psutil pids
func listPIDs() ([]int, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
