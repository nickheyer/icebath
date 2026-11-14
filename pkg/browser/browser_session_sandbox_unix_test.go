//go:build linux

package browser

import (
	"bufio"
	"bytes"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	extraChildModes["sandbox-chromium-standin"] = runSandboxChromiumStandin
	extraChildModes["sigterm-recorder"] = runSigtermRecorder
}

// Stand in Chromium publishing a DevTools port and recording a delivered SIGTERM beside its profile
func runSandboxChromiumStandin() int {
	udd := ""
	for _, a := range os.Args {
		if v, ok := strings.CutPrefix(a, "--user-data-dir="); ok {
			udd = v
		}
	}
	marker := filepath.Join(udd, "sigterm")
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 2
	}
	tmp := filepath.Join(udd, "DevToolsActivePort.tmp")
	port := ln.Addr().(*net.TCPAddr).Port
	if os.WriteFile(tmp, []byte(strconv.Itoa(port)+"\n/devtools/browser/stub\n"), 0o644) != nil || os.Rename(tmp, filepath.Join(udd, "DevToolsActivePort")) != nil {
		return 2
	}
	go func() {
		<-terms
		_ = os.WriteFile(marker, []byte("1"), 0o644)
	}()
	time.Sleep(120 * time.Second)
	return 0
}

// Unrelated job that records a delivered SIGTERM to the marker its env names
func runSigtermRecorder() int {
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	marker := os.Getenv("ICEBATH_SIGTERM_MARKER")
	go func() {
		<-terms
		_ = os.WriteFile(marker, []byte("1"), 0o644)
	}()
	os.Stdout.WriteString("ready\n")
	time.Sleep(120 * time.Second)
	return 0
}

type localSandbox struct{ tools.Environment }

// Terminal sandbox whose execs run argv on this host
func localSandboxFixture(t *testing.T) (*tools.Env, string) {
	t.Helper()
	env := testEnv(t)
	tmp := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	standin := filepath.Join(tmp, "chromium")
	script := "#!/bin/sh\n" + childModeEnv + "=sandbox-chromium-standin exec " + self + " \"$@\"\n"
	if err := os.WriteFile(standin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sandbox := &localSandbox{}
	pin(t, &sandboxRunIn, func(_ tools.Environment, argv []string, childEnv map[string]string, _ string, timeoutS float64) (*tools.RunInResult, error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = os.Environ()
		for k, v := range childEnv {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return nil, err
		}
		return &tools.RunInResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ReturnCode: cmd.ProcessState.ExitCode()}, nil
	})
	pin(t, &sandboxForwardPort, func(_ tools.Environment, port int, _ string) (int, error) { return port, nil })
	pin(t, &sandboxForwardFailures, func(tools.Environment, int) int { return 0 })
	pin(t, &sandboxTempDir, func(tools.Environment) string { return tmp })
	pin(t, &sandboxTerminalEnv, func(*tools.Env, bool) (tools.Environment, error) { return sandbox, nil })
	pin(t, &sandboxPublishedEnv, func(*tools.Env) (map[string]string, error) { return map[string]string{"DISPLAY": ":5"}, nil })
	pin(t, &sandboxUserFor, func(tools.Environment) (string, error) { return "", nil })
	pin(t, &sandboxProfileDir, func(tools.Environment) (string, error) { return filepath.Join(tmp, "profile"), nil })
	pin(t, &sandboxChromiumExe, func(tools.Environment) (string, error) { return standin, nil })
	pin(t, &sandboxLaunched, map[string]sandboxLaunch{})
	pin(t, &sandboxConfirmed, map[string]sandboxConfirmation{})
	t.Cleanup(func() {
		for _, l := range sandboxLaunched {
			killIfOurs(l.pid, tmp)
		}
	})
	return env, tmp
}

// SIGKILLs the stand in Chromium when its argv names this test's directory
func killIfOurs(pid int, tmp string) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err == nil && bytes.Contains(raw, []byte(tmp)) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func waitForFile(path string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestStopSignalsTheChromiumItLaunched(t *testing.T) {
	env, tmp := localSandboxFixture(t)
	info, err := createSandboxSession(env, "task-A")
	if err != nil {
		t.Fatal(err)
	}
	pid := sandboxLaunched[info["launcher_key"].(string)].pid
	defer killIfOurs(pid, tmp)
	stopSandboxBrowser(env, info)
	if !waitForFile(filepath.Join(tmp, "profile", "sigterm"), 5*time.Second) {
		t.Fatal("the launched Chromium was not signalled")
	}
}

func TestStopLeavesAloneAPidThatNowNamesAnotherProcess(t *testing.T) {
	env, tmp := localSandboxFixture(t)
	logs := &bytes.Buffer{}
	env.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	info, err := createSandboxSession(env, "task-A")
	if err != nil {
		t.Fatal(err)
	}
	key := info["launcher_key"].(string)
	killIfOurs(sandboxLaunched[key].pid, tmp)
	self, _ := os.Executable()
	marker := filepath.Join(tmp, "job-sigterm")
	job := exec.Command(self)
	job.Env = append(os.Environ(), childModeEnv+"=sigterm-recorder", "ICEBATH_SIGTERM_MARKER="+marker)
	out, _ := job.StdoutPipe()
	if err := job.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = job.Process.Kill(); _ = job.Wait() }()
	if line, _ := bufio.NewReader(out).ReadString('\n'); strings.TrimSpace(line) != "ready" {
		t.Fatalf("job said %q", line)
	}
	launched := sandboxLaunched[key]
	launched.pid = job.Process.Pid
	sandboxLaunched[key] = launched
	stopSandboxBrowser(env, info)
	if waitForFile(marker, time.Second) {
		t.Fatal("an unrelated process on a reused pid was signalled")
	}
	if !strings.Contains(logs.String(), strconv.Itoa(job.Process.Pid)) {
		t.Fatalf("log %s", logs.String())
	}
}
