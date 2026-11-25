//go:build unix && !linux && !darwin

package proc

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// One ps row of pid as the requested output columns, ErrNoSuchProcess when the pid is gone
func psRow(pid int, columns string) (string, error) {
	out, err := exec.Command("ps", "-o", columns, "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", ErrNoSuchProcess
		}
		return "", err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", ErrNoSuchProcess
	}
	return text, nil
}

// Creation epoch of pid from the ps start column
func Birth(pid int) (float64, error) {
	text, err := psRow(pid, "lstart=")
	if err != nil {
		return 0, err
	}
	for _, layout := range []string{"Mon Jan _2 15:04:05 2006", "Mon _2 Jan 15:04:05 2006", "Mon Jan _2 15:04:05 MST 2006"} {
		if t, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return float64(t.Unix()), nil
		}
	}
	return 0, errors.New("unparsed ps lstart " + text)
}

// Start stamp worker records compare against, the creation epoch on BSD
func WorkerBirth(pid int) (float64, error) { return Birth(pid) }

// Boot relative start, which ps does not expose
func BootStart(pid int) (float64, bool) { return 0, false }

// Status letter of pid from the first character of the ps stat column
func State(pid int) (byte, error) {
	text, err := psRow(pid, "stat=")
	if err != nil {
		return 0, err
	}
	return text[0], nil
}

// Whether pid exited and waits to be reaped
func Zombie(pid int) bool {
	text, err := psRow(pid, "stat=")
	return err == nil && strings.HasPrefix(text, "Z")
}

// Parent pid of pid, false when unreadable
func PPID(pid int) (int, bool) {
	text, err := psRow(pid, "ppid=")
	if err != nil {
		return 0, false
	}
	ppid, err := strconv.Atoi(strings.Fields(text)[0])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// Environment of pid from a mounted procfs, else from procstat
func Environ(pid int) (map[string]string, error) {
	if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ"); err == nil {
		return parseEnvironBlock(string(data)), nil
	}
	raw, err := exec.Command("procstat", "-e", strconv.Itoa(pid)).Output()
	if err != nil {
		if _, perr := psRow(pid, "pid="); perr != nil {
			return nil, perr
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		return nil, ErrNoSuchProcess
	}
	out := map[string]string{}
	fields := strings.Fields(lines[len(lines)-1])
	for _, kv := range fields[min(2, len(fields)):] {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}

// Argument vector of pid from the ps args column
func Cmdline(pid int) ([]string, error) {
	text, err := psRow(pid, "args=")
	if err != nil {
		return nil, err
	}
	return strings.Fields(text), nil
}

// Every pid in the ps listing like psutil pids
func listPIDs() ([]int, error) {
	t, err := snapshot()
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(t))
	for _, p := range t {
		pids = append(pids, p.PID)
	}
	return pids, nil
}
