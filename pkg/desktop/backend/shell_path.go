package backend

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	loginPathStart          = "__HERMES_LOGIN_PATH_START__"
	loginPathEnd            = "__HERMES_LOGIN_PATH_END__"
	loginPathProbeCommand   = "printf '%s' \"" + loginPathStart + "${PATH}" + loginPathEnd + "\""
	loginPathAttemptTimeout = 5 * time.Second
	loginPathKillGrace      = time.Second
	loginPathMaxBuffer      = 1024 * 1024
)

// LoginShellProbe runs shell with args under timeout and answers what it printed on stdout
type LoginShellProbe func(shell string, args []string, timeout time.Duration) string

// LoginShellPathResult is the outcome of merging the login shell PATH into the environment
type LoginShellPathResult struct {
	Applied bool   `json:"applied"`
	Reason  string `json:"reason,omitempty"`
	Path    string `json:"path,omitempty"`
}

// Shell the PATH probe runs, SHELL when set, zsh on macOS and bash elsewhere
func LoginShellExecutable(env map[string]string, platform string) string {
	if shell := strings.TrimSpace(env["SHELL"]); shell != "" {
		return shell
	}
	if platform == "darwin" {
		return "/bin/zsh"
	}
	return "/bin/bash"
}

// PATH between the last sentinel pair of the probe output, empty when the pair is incomplete
func ExtractSentinelPath(stdout string) string {
	start := strings.LastIndex(stdout, loginPathStart)
	if start == -1 {
		return ""
	}
	valueStart := start + len(loginPathStart)
	end := strings.Index(stdout[valueStart:], loginPathEnd)
	if end == -1 {
		return ""
	}
	return strings.TrimFunc(stdout[valueStart:valueStart+end], func(r rune) bool { return unicode.IsSpace(r) || r == 0xFEFF })
}

// Login shell entries first, then the current entries, each kept once
func MergeLoginShellPath(loginPath, currentPath, delimiter string) string {
	return AppendUniquePathEntries([][]string{SplitPathList(loginPath, delimiter), SplitPathList(currentPath, delimiter)}, delimiter)
}

type probeOutput struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	over func()
}

func (p *probeOutput) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if room := loginPathMaxBuffer - p.buf.Len(); len(b) > room {
		p.buf.Write(b[:max(0, room)])
		p.over()
		return len(b), nil
	}
	return p.buf.Write(b)
}

func (p *probeOutput) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// Runs the probe with stdin closed, SIGTERM at the timeout and SIGKILL one second later keeping what it printed
func RunLoginShellProbe(shell string, args []string, timeout time.Duration) string {
	cmd := exec.Command(shell, args...)
	out := &probeOutput{}
	out.over = func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	cmd.Stdout = out
	if err := cmd.Start(); err != nil {
		return ""
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
		return out.String()
	case <-time.After(timeout):
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-exited:
	case <-time.After(loginPathKillGrace):
		_ = cmd.Process.Kill()
	}
	return out.String()
}

// PATH an interactive login shell reports, then a plain login shell, empty on Windows or when neither answers
func CaptureLoginShellPath(env map[string]string, platform string, probe LoginShellProbe, timeout time.Duration) string {
	if platform == "win32" {
		return ""
	}
	shell := LoginShellExecutable(env, platform)
	for _, flag := range []string{"-ilc", "-lc"} {
		if captured := ExtractSentinelPath(probe(shell, []string{flag, loginPathProbeCommand}, timeout)); captured != "" {
			return captured
		}
	}
	return ""
}

// Merges the login shell PATH ahead of the inherited PATH and stores it with setenv
func ApplyLoginShellPath(env map[string]string, platform string, probe LoginShellProbe, timeout time.Duration, setenv func(key, value string) error) LoginShellPathResult {
	if platform == "win32" {
		return LoginShellPathResult{Reason: "win32"}
	}
	loginPath := CaptureLoginShellPath(env, platform, probe, timeout)
	if loginPath == "" {
		return LoginShellPathResult{Reason: "unresolved"}
	}
	key := PathEnvKey(env, platform)
	merged := MergeLoginShellPath(loginPath, env[key], DelimiterForPlatform(platform))
	if merged == "" || merged == env[key] {
		return LoginShellPathResult{Reason: "unchanged", Path: merged}
	}
	if err := setenv(key, merged); err != nil {
		return LoginShellPathResult{Reason: err.Error()}
	}
	return LoginShellPathResult{Applied: true, Path: merged}
}

// ProcessEnv is the environment of this process as a map
func ProcessEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			out[k] = v
		}
	}
	return out
}

var loginShellPath struct {
	once   sync.Once
	result LoginShellPathResult
}

// Merges the login shell PATH into this process once, every caller sharing the first outcome
func EnsureLoginShellPath() LoginShellPathResult {
	loginShellPath.once.Do(func() {
		loginShellPath.result = ApplyLoginShellPath(ProcessEnv(), electronPlatformName(), RunLoginShellProbe, loginPathAttemptTimeout, os.Setenv)
	})
	return loginShellPath.result
}
