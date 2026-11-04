package tools

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// One deterministic system prompt line about the local Python toolchain

const envProbeWaitTimeout = 10 * time.Second

var envProbeRemoteBackends = map[string]bool{
	"docker": true, "modal": true, "daytona": true, "ssh": true, "managed_modal": true, "vercel_sandbox": true,
}

type envProbeState struct {
	mu            sync.Mutex
	done          chan struct{}
	line          string
	started       bool
	gen           int
	waitTimedOut  bool
	lookPath      func(string) (string, error)
	runCommand    func(cmd []string, timeout time.Duration) (int, string, string)
	backendRemote func(backend string) bool
}

var envProbe = newEnvProbeState()

func newEnvProbeState() *envProbeState {
	return &envProbeState{done: make(chan struct{}), lookPath: exec.LookPath, runCommand: envProbeRun,
		backendRemote: pluginBackendIsRemote}
}

// Whether a plugin registered terminal backend is remote
func pluginBackendIsRemote(backend string) bool {
	if backend == "" || envProbeRemoteBackends[backend] || backend == "local" {
		return false
	}
	provider := GetTerminalBackend(backend)
	return provider != nil && provider.IsRemote()
}

// Runs a short subprocess returning the exit code and trimmed output, failures as -1
func envProbeRun(cmd []string, timeout time.Duration) (int, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	var out, errBuf bytes.Buffer
	c.Stdout, c.Stderr = &out, &errBuf
	c.Stdin = nil
	err := c.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return -1, "", "timeout"
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), strings.TrimSpace(out.String()), strings.TrimSpace(errBuf.String())
		}
		if _, ok := err.(*exec.Error); ok {
			return -1, "", "not found"
		}
		return -1, "", "oserror: " + err.Error()
	}
	return 0, strings.TrimSpace(out.String()), strings.TrimSpace(errBuf.String())
}

// Stdout of the binary when it is on PATH and exits zero
func (p *envProbeState) pyOut(binary string, args ...string) (string, bool) {
	if _, err := p.lookPath(binary); err != nil {
		return "", false
	}
	rc, out, _ := p.runCommand(append([]string{binary}, args...), 3*time.Second)
	if rc != 0 {
		return "", false
	}
	return out, true
}

func (p *envProbeState) pythonVersionOf(binary string) string {
	out, ok := p.pyOut(binary, "-c", "import sys; print('.'.join(map(str, sys.version_info[:3])))")
	if !ok {
		return ""
	}
	return out
}

func (p *envProbeState) hasPipModule(binary string) bool {
	_, ok := p.pyOut(binary, "-m", "pip", "--version")
	return ok
}

func (p *envProbeState) detectPEP668(binary string) bool {
	out, _ := p.pyOut(binary, "-c", "import os; print('yes' if os.path.exists(os.path.join(os.path.dirname(os.__file__), 'EXTERNALLY-MANAGED')) else 'no')")
	return strings.TrimSpace(out) == "yes"
}

// Python version the pip on PATH is bound to from the trailing parenthesis of pip --version
func (p *envProbeState) pipPythonVersion() string {
	out, _ := p.pyOut("pip", "--version")
	idx := strings.Index(out, "(python ")
	if idx < 0 || !strings.HasSuffix(out, ")") {
		return ""
	}
	return strings.TrimSpace(out[idx+len("(python ") : len(out)-1])
}

// Scope aware terminal backend name with local when unresolvable
func resolveTerminalBackend(env *Env) string {
	policy, err := terminalPolicy(env)
	if err != nil {
		slog.Debug("terminal backend resolution failed", "error", err)
		return "local"
	}
	backend := strings.ToLower(strings.TrimSpace(policy.Get("TERMINAL_ENV", "local")))
	if backend == "" {
		return "local"
	}
	return backend
}

// Builds the one liner, empty when nothing notable is detected
func (p *envProbeState) buildProbeLine() string {
	py3 := p.pythonVersionOf("python3")
	py := p.pythonVersionOf("python")
	py3HasPip := py3 != "" && p.hasPipModule("python3")
	pipBoundTo := p.pipPythonVersion()
	py3PEP668 := py3 != "" && p.detectPEP668("python3")
	_, uvErr := p.lookPath("uv")
	hasUV := uvErr == nil
	mismatch := pipBoundTo != "" && py3 != "" && !strings.HasPrefix(py3, pipBoundTo)
	if py3 != "" && py3HasPip && !mismatch && (!py3PEP668 || hasUV) {
		return ""
	}
	var bits []string
	if py3 != "" {
		bit := "python3=" + py3
		if !py3HasPip {
			bit += " (no pip module)"
		}
		bits = append(bits, bit)
	} else {
		bits = append(bits, "python3=missing")
	}
	if py != "" && py != py3 {
		bits = append(bits, "python="+py)
	} else if py == "" && py3 != "" {
		bits = append(bits, "python=missing (use python3)")
	}
	if pipBoundTo != "" {
		if mismatch {
			bits = append(bits, "pip→python"+pipBoundTo+" (mismatch)")
		} else if !py3HasPip {
			bits = append(bits, "pip→python"+pipBoundTo)
		}
	} else if !py3HasPip {
		bits = append(bits, "pip=missing")
	}
	if py3PEP668 {
		bits = append(bits, "PEP 668=yes (use venv or uv)")
	}
	if hasUV {
		bits = append(bits, "uv=installed")
	}
	return "Python toolchain: " + strings.Join(bits, ", ") + "."
}

func (p *envProbeState) worker(gen int, done chan struct{}) {
	line := p.buildProbeLine()
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != p.gen {
		return
	}
	p.line = line
	close(done)
}

// Starts the single probe worker when it is neither running nor finished
func (p *envProbeState) ensureStarted() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return
	}
	p.started = true
	go p.worker(p.gen, p.done)
}

// EnvironmentProbeLine returns the cached toolchain line, empty for remote backends or a clean host
func EnvironmentProbeLine(env *Env, forceRefresh bool) string {
	if forceRefresh {
		ResetEnvironmentProbe()
	}
	backend := resolveTerminalBackend(env)
	if envProbeRemoteBackends[backend] || envProbe.backendRemote(backend) {
		return ""
	}
	p := envProbe
	p.mu.Lock()
	done := p.done
	timedOut := p.waitTimedOut
	p.mu.Unlock()
	select {
	case <-done:
		return p.cachedLine()
	default:
	}
	p.ensureStarted()
	wait := envProbeWaitTimeout
	if timedOut {
		wait = 50 * time.Millisecond
	}
	select {
	case <-done:
		return p.cachedLine()
	case <-time.After(wait):
		p.mu.Lock()
		first := !p.waitTimedOut
		p.waitTimedOut = true
		p.mu.Unlock()
		if first {
			toolsLogger(env).Warn("env_probe did not finish in time; building the system prompt without the Python toolchain line",
				"seconds", envProbeWaitTimeout.Seconds())
		}
		return ""
	}
}

func (p *envProbeState) cachedLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.line
}

// WarmEnvironmentProbeAsync starts the probe in the background ahead of the first prompt build
func WarmEnvironmentProbeAsync() {
	envProbe.ensureStarted()
}

// ResetEnvironmentProbe clears the cache so the next read probes again
func ResetEnvironmentProbe() {
	p := envProbe
	p.mu.Lock()
	defer p.mu.Unlock()
	p.line = ""
	p.done = make(chan struct{})
	p.started = false
	p.gen++
	p.waitTimedOut = false
}
