package prompt

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/prompt"
)

const (
	// Longest the first prompt waits for the probe, and those after a probe that ran late
	probeWait     = 10 * time.Second
	probeLateWait = 50 * time.Millisecond
	probeCommand  = 3 * time.Second
)

// One line on the machine's Python toolchain when something about it is worth knowing: probed once, in the background, the first time a
// prompt asks for it
type probe struct {
	mu      sync.Mutex
	started bool
	late    bool
	done    chan struct{}
	text    string
}

// The toolchain line when the environment probe is on, empty when the toolchain is in order or the probe is still running
func (p *probe) line(ctx context.Context, env *prompt.Env) string {
	if !env.Config.GetBool(agentconfig.EnvironmentProbe) {
		return ""
	}
	p.mu.Lock()
	if !p.started {
		p.started, p.done = true, make(chan struct{})
		go func() {
			text := toolchain()
			p.mu.Lock()
			p.text = text
			p.mu.Unlock()
			close(p.done)
		}()
	}
	done, wait := p.done, probeWait
	if p.late {
		wait = probeLateWait
	}
	p.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.text
	case <-timer.C:
		p.mu.Lock()
		first := !p.late
		p.late = true
		p.mu.Unlock()
		if first {
			env.Notify("The Python toolchain check did not finish in time, so this prompt goes without it.")
		}
	case <-ctx.Done():
	}
	return ""
}

// Output of a program on the PATH that exits cleanly, false otherwise
func output(program string, args ...string) (string, bool) {
	if _, err := exec.LookPath(program); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeCommand)
	defer cancel()
	var stdout bytes.Buffer
	command := exec.CommandContext(ctx, program, args...)
	command.Stdout = &stdout
	if command.Run() != nil {
		return "", false
	}
	return strings.TrimSpace(stdout.String()), true
}

func pythonVersion(program string) string {
	version, _ := output(program, "-c", "import sys; print('.'.join(map(str, sys.version_info[:3])))")
	return version
}

// What is off about python3, python, pip and uv: a missing pip, pip bound to another Python, an externally managed Python without uv;
// empty when nothing is
func toolchain() string {
	python3, python := pythonVersion("python3"), pythonVersion("python")
	pip := false
	externallyManaged := false
	if python3 != "" {
		_, pip = output("python3", "-m", "pip", "--version")
		managed, _ := output("python3", "-c", "import os; print('yes' if os.path.exists(os.path.join(os.path.dirname(os.__file__), 'EXTERNALLY-MANAGED')) else 'no')")
		externallyManaged = managed == "yes"
	}
	boundTo := ""
	if version, _ := output("pip", "--version"); strings.HasSuffix(version, ")") {
		if at := strings.Index(version, "(python "); at >= 0 {
			boundTo = strings.TrimSpace(version[at+len("(python ") : len(version)-1])
		}
	}
	_, err := exec.LookPath("uv")
	uv := err == nil
	mismatch := boundTo != "" && python3 != "" && !strings.HasPrefix(python3, boundTo)
	if python3 != "" && pip && !mismatch && (!externallyManaged || uv) {
		return ""
	}
	var bits []string
	switch {
	case python3 == "":
		bits = append(bits, "python3=missing")
	case !pip:
		bits = append(bits, "python3="+python3+" (no pip module)")
	default:
		bits = append(bits, "python3="+python3)
	}
	switch {
	case python != "" && python != python3:
		bits = append(bits, "python="+python)
	case python == "" && python3 != "":
		bits = append(bits, "python=missing (use python3)")
	}
	switch {
	case boundTo != "" && mismatch:
		bits = append(bits, "pip→python"+boundTo+" (mismatch)")
	case boundTo != "" && !pip:
		bits = append(bits, "pip→python"+boundTo)
	case boundTo == "" && !pip:
		bits = append(bits, "pip=missing")
	}
	if externallyManaged {
		bits = append(bits, "PEP 668=yes (use venv or uv)")
	}
	if uv {
		bits = append(bits, "uv=installed")
	}
	return "Python toolchain: " + strings.Join(bits, ", ") + "."
}
