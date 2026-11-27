package packages

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
)

// Most interpreters a probe verdict is remembered for, the oldest forgotten first
const probesKept = 32

// Interpreters found usable or not
type probes struct {
	mu    sync.Mutex
	order []string
	known map[string]bool
}

// Names the interpreter goes by on PATH, in the order they are looked for
func PythonNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"python.exe", "python3.exe"}
	}
	return []string{"python3", "python"}
}

// Whether the interpreter is Python 3.8 or newer, a probe that could not run remembered as neither
func (h *Packages) Usable(python string, env map[string]string) bool {
	p := &h.probes
	p.mu.Lock()
	known, ok := p.known[python]
	p.mu.Unlock()
	if ok {
		return known
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", "import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)")
	cmd.Env = envList(env)
	proc.HideWindow(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return false
	}
	if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
		return false
	}
	usable := err == nil
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.known == nil {
		p.known = map[string]bool{}
	}
	if _, seen := p.known[python]; !seen {
		if len(p.order) >= probesKept {
			delete(p.known, p.order[0])
			p.order = p.order[1:]
		}
		p.order = append(p.order, python)
	}
	p.known[python] = usable
	return usable
}

// First Python 3.8 or newer on the search path, probed with the environment
func (h *Packages) OnPath(path string, env map[string]string) string {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		for _, name := range PythonNames() {
			if candidate := filepath.Join(dir, name); fsx.Executable(candidate) && h.Usable(candidate, env) {
				return candidate
			}
		}
	}
	return ""
}

// Python 3.8 or newer that runs what the caller needs, such as "browser_exec runs Python code": the first on the environment's PATH, else
// icebath's own, installed when missing and the profile allows it
func (h *Packages) Python(ctx context.Context, cfg *config.Config, env map[string]string, needs string) (string, error) {
	path := env["PATH"]
	if runtime.GOOS == "windows" {
		for name, value := range env {
			if strings.EqualFold(name, "PATH") {
				path = value
				break
			}
		}
	}
	if found := h.OnPath(path, env); found != "" {
		return found, nil
	}
	if _, err := h.Ensure(ctx, cfg, Python, Ensuring{}); err != nil {
		return "", fmt.Errorf("%s and found no Python 3.8 or newer interpreter on PATH, and installing icebath's own failed: %w", needs, err)
	}
	installed, err := h.Installed(Python, false)
	if err != nil || installed == nil || installed.Binary == "" {
		return "", errors.New(needs + " and found no Python 3.8 or newer interpreter on PATH")
	}
	return installed.Binary, nil
}
