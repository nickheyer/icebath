package env

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

const (
	probeWaitTimeout  = 10 * time.Second
	probePeekTimeout  = 50 * time.Millisecond
	probeRunTimeout   = 3 * time.Second
	probeRunWaitDelay = 500 * time.Millisecond
)

type toolchainProbe struct {
	mu       sync.Mutex
	started  bool
	timedOut bool
	done     chan struct{}
	line     string
}

func newToolchainProbe() *toolchainProbe {
	return &toolchainProbe{done: make(chan struct{})}
}

func (p *toolchainProbe) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return
	}
	p.started = true
	go func() {
		line := buildProbeLine()
		p.mu.Lock()
		p.line = line
		p.mu.Unlock()
		close(p.done)
	}()
}

func (p *toolchainProbe) result() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.line
}

func (m *Manager) probeBackend(ctx context.Context) string {
	v, err := m.settings.TerminalEnv(ctx, "TERMINAL_ENV")
	if err != nil {
		slog.Debug("terminal backend resolution failed", "err", err)
		return "local"
	}
	if v == "" {
		v = "local"
	}
	return strings.ToLower(strings.TrimSpace(v))
}

func (m *Manager) ProbeLine(ctx context.Context) string {
	if m.settings.IsRemoteBackend(ctx, m.probeBackend(ctx)) {
		return ""
	}
	p := m.probe
	select {
	case <-p.done:
		return p.result()
	default:
	}
	p.start()
	p.mu.Lock()
	wait := probeWaitTimeout
	if p.timedOut {
		wait = probePeekTimeout
	}
	p.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.result()
	case <-timer.C:
	}
	p.mu.Lock()
	first := !p.timedOut
	p.timedOut = true
	p.mu.Unlock()
	if first {
		slog.Warn(fmt.Sprintf("env_probe did not finish within %.0fs; building the system prompt without the Python toolchain line", probeWaitTimeout.Seconds()))
	}
	return ""
}

func (m *Manager) WarmProbe() {
	m.probe.start()
}

func probeRun(argv ...string) (int, string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), probeRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = probeRunWaitDelay
	proc.Hide(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return -1, "", "timeout"
	}
	if err != nil && cmd.ProcessState == nil {
		return -1, "", "not found"
	}
	return proc.ExitOf(cmd.ProcessState).Status(), strings.TrimSpace(decodeLossy(stdout.Bytes())), strings.TrimSpace(decodeLossy(stderr.Bytes()))
}

func decodeLossy(b []byte) string {
	return strings.ToValidUTF8(string(b), "�")
}

func probeOut(binary string, args ...string) (string, bool) {
	if _, err := exec.LookPath(binary); err != nil {
		return "", false
	}
	code, out, _ := probeRun(append([]string{binary}, args...)...)
	return out, code == 0
}

var probeVersionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

func probeInterpreterVersion(binary string) string {
	if _, err := exec.LookPath(binary); err != nil {
		return ""
	}
	code, out, errOut := probeRun(binary, "--version")
	if code != 0 {
		return ""
	}
	for _, text := range []string{out, errOut} {
		if m := probeVersionRe.FindString(text); m != "" {
			return m
		}
	}
	return ""
}

func probeHasPipModule(binary string) bool {
	_, ok := probeOut(binary, "-m", "pip", "--version")
	return ok
}

var probeStdlibRe = regexp.MustCompile(`(?m)^\s*stdlib = "([^"]*)"\s*$`)

func probeExternallyManaged(binary string) bool {
	out, ok := probeOut(binary, "-m", "sysconfig")
	if !ok {
		return false
	}
	m := probeStdlibRe.FindStringSubmatch(out)
	if m == nil {
		return false
	}
	_, err := os.Stat(filepath.Join(m[1], "EXTERNALLY-MANAGED"))
	return err == nil
}

func probePipBoundVersion() string {
	out, ok := probeOut("pip", "--version")
	if !ok {
		return ""
	}
	if i := strings.LastIndex(out, "(python "); i >= 0 && strings.HasSuffix(out, ")") {
		return strings.TrimSpace(out[i+len("(python ") : len(out)-1])
	}
	return ""
}

func buildProbeLine() string {
	py3 := probeInterpreterVersion("python3")
	py := probeInterpreterVersion("python")
	py3Pip := py3 != "" && probeHasPipModule("python3")
	pipBound := probePipBoundVersion()
	pep668 := py3 != "" && probeExternallyManaged("python3")
	_, uvErr := exec.LookPath("uv")
	hasUV := uvErr == nil
	mismatch := pipBound != "" && py3 != "" && !strings.HasPrefix(py3, pipBound)
	if py3 != "" && py3Pip && !mismatch && (!pep668 || hasUV) {
		return ""
	}
	var bits []string
	if py3 != "" {
		bit := "python3=" + py3
		if !py3Pip {
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
	if pipBound != "" {
		if mismatch {
			bits = append(bits, "pip→python"+pipBound+" (mismatch)")
		} else if !py3Pip {
			bits = append(bits, "pip→python"+pipBound)
		}
	} else if !py3Pip {
		bits = append(bits, "pip=missing")
	}
	if pep668 {
		bits = append(bits, "PEP 668=yes (use venv or uv)")
	}
	if hasUV {
		bits = append(bits, "uv=installed")
	}
	return "Python toolchain: " + strings.Join(bits, ", ") + "."
}
