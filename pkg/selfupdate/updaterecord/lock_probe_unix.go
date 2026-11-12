//go:build !windows

package updaterecord

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Signal zero liveness, permission denied counting as live and a Linux or macOS zombie as dead
func pidRunning(pid int) (bool, error) {
	if pid > math.MaxInt32 {
		return false, errors.New("signed integer is greater than maximum")
	}
	err := syscall.Kill(pid, 0)
	switch {
	case errors.Is(err, syscall.EPERM):
		return true, nil
	case err != nil:
		return false, nil
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		return !proc.Zombie(pid), nil
	}
	return true, nil
}

func asciiSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

// Creation time in unix seconds from proc stat and btime, else from ps lstart read in UTC, nil when unreadable
func stdlibCreateTime(pid int) *float64 {
	if st, err := os.Stat("/proc"); err == nil && st.IsDir() {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return nil
		}
		end := bytes.LastIndexByte(data, ')')
		if end < 0 {
			return nil
		}
		fields := bytes.FieldsFunc(data[min(end+2, len(data)):], asciiSpace)
		if len(fields) < 20 {
			return nil
		}
		startTicks, err := strconv.ParseInt(string(fields[19]), 10, 64)
		if err != nil {
			return nil
		}
		stat, err := os.ReadFile("/proc/stat")
		if err != nil {
			return nil
		}
		for _, line := range bytes.SplitAfter(stat, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("btime ")) {
				continue
			}
			parts := bytes.FieldsFunc(line, asciiSpace)
			if len(parts) < 2 {
				return nil
			}
			btime, err := strconv.ParseInt(string(parts[1]), 10, 64)
			if err != nil {
				return nil
			}
			value := float64(btime) + float64(startTicks)/clockTicks()
			return &value
		}
		return nil
	}
	path := os.Getenv("PATH")
	if path == "" {
		path = "/bin:/usr/bin"
	}
	ps := lookPathIn("ps", path)
	if ps == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ps, "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = []string{"PATH=" + path, "LC_ALL=C", "TZ=UTC0"}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	var exitErr *exec.ExitError
	if ctx.Err() != nil || (err != nil && !errors.As(err, &exitErr)) {
		return nil
	}
	out := pystr.Strip(strings.ToValidUTF8(stdout.String(), "�"))
	if out == "" {
		return nil
	}
	started, err := time.Parse("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(out), " "))
	if err != nil {
		return nil
	}
	value := float64(started.Unix())
	return &value
}

// First executable named name on the given search path, empty when none
func lookPathIn(name, path string) string {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}
