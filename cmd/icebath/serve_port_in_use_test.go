package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/dashboard"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Port a re-executed test binary serves a headless backend on, the start_server call of the hermes subprocess
const startServerProbeEnv = "ICEBATH_TEST_START_SERVER_PORT"

// Runs a headless server start on the port and returns the status the process exits with
func runStartServerProbe(rawPort string) int {
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	err = dashboard.StartServer(context.Background(), gateway.DashboardServerOptions{Host: "127.0.0.1", Port: port,
		Headless: true, Stderr: os.Stderr})
	var exit *gateway.DashboardExit
	if errors.As(err, &exit) {
		if exit.Msg != "" {
			fmt.Fprintln(os.Stderr, exit.Msg)
		}
		return exit.Code
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// Real headless server start in a child process under a temp home, stderr merged into stdout or discarded
func spawnServe(t *testing.T, port int, mergeStderr bool) (*exec.Cmd, io.Reader) {
	t.Helper()
	h := filepath.Join(t.TempDir(), "icebath_home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "ICEBATH_DESKTOP", "ICEBATH_PARENT_PID", "ICEBATH_PARENT_START_MARKER", "ICEBATH_PARENT_NONCE", "ICEBATH_HOME":
		default:
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "ICEBATH_HOME="+h, "ICEBATH_SERVE_HEADLESS=1", startServerProbeEnv+"="+strconv.Itoa(port))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if mergeStderr {
		cmd.Stderr = cmd.Stdout
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, stdout
}

// Collects output lines until one carries the token, the process ends or the timeout passes
func readUntil(out io.Reader, token string, timeout time.Duration) (bool, []string) {
	var mu sync.Mutex
	var lines []string
	hit := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text() + "\n"
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
			if strings.Contains(line, token) {
				close(hit)
				return
			}
		}
	}()
	found := false
	select {
	case <-hit:
		found = true
	case <-time.After(timeout):
	}
	mu.Lock()
	defer mu.Unlock()
	return found, append([]string(nil), lines...)
}

// Terminates the child and waits up to thirty seconds before killing it
func terminateServe(cmd *exec.Cmd) {
	if runtime.GOOS == "windows" {
		_ = cmd.Process.Kill()
	} else {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// Loopback port that was free a moment ago
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestConflictEmitsSentinelAndExit75(t *testing.T) {
	holder, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	port := holder.Addr().(*net.TCPAddr).Port
	cmd, stdout := spawnServe(t, port, true)
	var buf bytes.Buffer
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, stdout)
		close(copied)
	}()
	waited := make(chan error, 1)
	go func() {
		<-copied
		waited <- cmd.Wait()
	}()
	select {
	case err = <-waited:
	case <-time.After(180 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("serve did not exit on a port conflict")
	}
	out := buf.String()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 75 {
		t.Fatalf("exit = %v\n%s", err, out)
	}
	if !strings.Contains(out, fmt.Sprintf("BACKEND_PORT_IN_USE port=%d", port)) {
		t.Errorf("no port in use sentinel\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("Port %d", port)) {
		t.Errorf("no human hint line\n%s", out)
	}
	if strings.Contains(out, "ICEBATH_BACKEND_READY") {
		t.Errorf("claimed ready\n%s", out)
	}
	if n := strings.Count(out, "BACKEND_PORT_IN_USE"); n != 1 {
		t.Errorf("sentinel lines = %d\n%s", n, out)
	}
}

func TestEphemeralPortZeroUnaffected(t *testing.T) {
	cmd, stdout := spawnServe(t, 0, true)
	defer terminateServe(cmd)
	ready, lines := readUntil(stdout, "ICEBATH_BACKEND_READY", 120*time.Second)
	out := strings.Join(lines, "")
	if !ready {
		t.Fatalf("no READY sentinel; output:\n%s", out)
	}
	announced := 0
	for _, line := range lines {
		if strings.Contains(line, "ICEBATH_BACKEND_READY") {
			_, raw, _ := strings.Cut(strings.TrimSpace(line), "port=")
			var err error
			if announced, err = strconv.Atoi(raw); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if announced <= 0 {
		t.Errorf("announced port = %d", announced)
	}
	if strings.Contains(out, "BACKEND_PORT_IN_USE") {
		t.Errorf("port in use reported\n%s", out)
	}
}

func TestReadySentinelArrivesOnStdoutNotStderr(t *testing.T) {
	port := freeLoopbackPort(t)
	cmd, stdout := spawnServe(t, port, false)
	defer terminateServe(cmd)
	ready, lines := readUntil(stdout, "ICEBATH_BACKEND_READY", 120*time.Second)
	out := strings.Join(lines, "")
	if !ready {
		t.Fatalf("READY sentinel not on stdout (desktop boot would time out); stdout:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("ICEBATH_BACKEND_READY port=%d", port)) {
		t.Errorf("stdout:\n%s", out)
	}
}

func TestHeadlessServeAnnouncesBothReadyTokens(t *testing.T) {
	port := freeLoopbackPort(t)
	cmd, stdout := spawnServe(t, port, true)
	defer terminateServe(cmd)
	legacy := fmt.Sprintf("ICEBATH_DASHBOARD_READY port=%d", port)
	backend := fmt.Sprintf("ICEBATH_BACKEND_READY port=%d", port)
	ready, lines := readUntil(stdout, legacy, 120*time.Second)
	out := strings.Join(lines, "")
	if !ready {
		t.Fatalf("legacy token missing (a stale packaged Desktop would time out); output:\n%s", out)
	}
	if !strings.Contains(out, backend) {
		t.Fatalf("neutral token missing\n%s", out)
	}
	if strings.Index(out, backend) >= strings.Index(out, legacy) {
		t.Errorf("neutral token after the legacy one\n%s", out)
	}
	if n := strings.Count(out, "ICEBATH_BACKEND_READY port="); n != 1 {
		t.Errorf("neutral tokens = %d", n)
	}
	if n := strings.Count(out, "ICEBATH_DASHBOARD_READY port="); n != 1 {
		t.Errorf("legacy tokens = %d", n)
	}
}
