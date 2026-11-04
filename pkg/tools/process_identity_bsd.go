//go:build unix && !linux && !darwin

package tools

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// BSD process facts read through ps and procstat

// One ps row of pid as the requested output columns, errNoSuchProcess when the pid is gone
func psRow(pid int, columns string) (string, error) {
	out, err := exec.Command("ps", "-o", columns, "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", errNoSuchProcess
		}
		return "", err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", errNoSuchProcess
	}
	return text, nil
}

func (osProcessProber) Birth(pid int) (float64, error) {
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

func (p osProcessProber) WorkerBirth(pid int) (float64, error) { return p.Birth(pid) }

func (osProcessProber) BootStart(pid int) (float64, bool) { return 0, false }

func (osProcessProber) Zombie(pid int) bool {
	text, err := psRow(pid, "stat=")
	return err == nil && strings.HasPrefix(text, "Z")
}

func (osProcessProber) PPID(pid int) (int, bool) {
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

func (osProcessProber) Environ(pid int) (map[string]string, error) {
	out := map[string]string{}
	if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ"); err == nil {
		for _, kv := range strings.Split(string(data), "\x00") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				out[k] = v
			}
		}
		return out, nil
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
		return nil, errNoSuchProcess
	}
	fields := strings.Fields(lines[len(lines)-1])
	for _, kv := range fields[2:] {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}

func (osProcessProber) Cmdline(pid int) ([]string, error) {
	text, err := psRow(pid, "args=")
	if err != nil {
		return nil, err
	}
	return strings.Fields(text), nil
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
