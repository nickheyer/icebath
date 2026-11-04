//go:build linux

package tools

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

// Linux process facts read from procfs

// Fields of /proc/pid/stat after the command name, errNoSuchProcess when the pid is gone
func procStatFields(pid int) ([]string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return nil, errNoSuchProcess
		}
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
func procBootTime() (float64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "btime ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 64)
			if err != nil {
				return 0, err
			}
			return v, nil
		}
	}
	return 0, errors.New("btime missing from /proc/stat")
}

func (osProcessProber) BootStart(pid int) (float64, bool) {
	fields, err := procStatFields(pid)
	if err != nil {
		return 0, false
	}
	ticks, err := strconv.ParseFloat(fields[19], 64)
	if err != nil {
		return 0, false
	}
	return ticks / clockTicksPerSecond(), true
}

func (p osProcessProber) Birth(pid int) (float64, error) {
	fields, err := procStatFields(pid)
	if err != nil {
		return 0, err
	}
	ticks, err := strconv.ParseFloat(fields[19], 64)
	if err != nil {
		return 0, err
	}
	boot, err := procBootTime()
	if err != nil {
		return 0, err
	}
	return ticks/clockTicksPerSecond() + boot, nil
}

func (p osProcessProber) WorkerBirth(pid int) (float64, error) {
	fields, err := procStatFields(pid)
	if err != nil {
		return 0, err
	}
	ticks, err := strconv.ParseFloat(fields[19], 64)
	if err != nil {
		return 0, err
	}
	return ticks / clockTicksPerSecond(), nil
}

func (osProcessProber) Zombie(pid int) bool {
	fields, err := procStatFields(pid)
	return err == nil && fields[0] == "Z"
}

func (osProcessProber) PPID(pid int) (int, bool) {
	fields, err := procStatFields(pid)
	if err != nil {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// NUL separated file of a process split into entries, errNoSuchProcess when the pid is gone
func procNulFile(pid int, name string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return nil, errNoSuchProcess
		}
		return nil, err
	}
	parts := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	if len(parts) == 1 && parts[0] == "" {
		return []string{}, nil
	}
	return parts, nil
}

func (osProcessProber) Environ(pid int) (map[string]string, error) {
	parts, err := procNulFile(pid, "environ")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, kv := range parts {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}

func (osProcessProber) Cmdline(pid int) ([]string, error) {
	return procNulFile(pid, "cmdline")
}

func (osProcessProber) Children(pid int, recursive bool) []int {
	table := localProcessTable()
	kids := map[int][]int{}
	for child, parent := range table {
		kids[parent] = append(kids[parent], child)
	}
	if !recursive {
		return kids[pid]
	}
	seen := map[int]bool{pid: true}
	out := []int{}
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, k := range kids[cur] {
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
			queue = append(queue, k)
		}
	}
	return out
}
