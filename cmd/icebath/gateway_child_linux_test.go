//go:build linux

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Re-executed test binary of a case, its exit channel and the file its stdout and stderr share
type gatewayChild struct {
	process *exec.Cmd
	exited  chan error
	logPath string
	stopped bool
}

// Case homes of a hermes gateway subprocess, the state home with its config and a separate user home
func gatewayCaseHomes(t *testing.T, config map[string]any) (tmp, h, user string) {
	t.Helper()
	tmp = t.TempDir()
	h, user = filepath.Join(tmp, "state"), filepath.Join(tmp, "user")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(user, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return tmp, h, user
}

// Environment of a hermes gateway subprocess, the inherited path and locale with the case homes and extras
func gatewayChildEnv(h, user string, extra ...string) []string {
	var env []string
	for _, name := range []string{"PATH", "LANG", "TZ", "TIRITH_ENABLED"} {
		if value, set := os.LookupEnv(name); set {
			env = append(env, name+"="+value)
		}
	}
	env = append(env, "HOME="+user, "USERPROFILE="+user, "ICEBATH_HOME="+h)
	return append(env, extra...)
}

// Starts the test binary named icebath with the arguments so its command line reads as a gateway, interrupted at cleanup
func startGatewayChild(t *testing.T, logPath string, env []string, args ...string) *gatewayChild {
	t.Helper()
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	child := &gatewayChild{process: exec.Command(cliProgramBinary(t), args...), exited: make(chan error, 1), logPath: logPath}
	child.process.Env = env
	child.process.Stdout, child.process.Stderr = logFile, logFile
	if err := child.process.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { child.exited <- child.process.Wait() }()
	t.Cleanup(func() {
		if !child.stopped {
			_, _ = child.interrupt(20 * time.Second)
		}
	})
	return child
}

// Merged output of the child so far
func (c *gatewayChild) output() string {
	raw, _ := os.ReadFile(c.logPath)
	return string(raw)
}

// Descriptor of a ready owner, failing at once when the child exits and after the deadline
func (c *gatewayChild) waitReady(t *testing.T, h string, timeout time.Duration) map[string]any {
	t.Helper()
	descriptor := map[string]any{}
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		select {
		case err := <-c.exited:
			c.stopped = true
			t.Fatalf("the gateway exited before it was ready: %v\n%s", err, c.output())
		default:
		}
		if identified, err := gatewayControl(h, "identify", nil); err == nil {
			descriptor = identified
			if descriptor["state"] == "ready" {
				return descriptor
			}
		}
	}
	t.Fatalf("descriptor = %v\n%s", descriptor, c.output())
	return nil
}

// Exit of the child after SIGINT, false with the kill exit when it outlives the timeout
func (c *gatewayChild) interrupt(timeout time.Duration) (bool, error) {
	if c.stopped {
		return true, nil
	}
	c.stopped = true
	_ = c.process.Process.Signal(syscall.SIGINT)
	select {
	case err := <-c.exited:
		return true, err
	case <-time.After(timeout):
		_ = c.process.Process.Kill()
		return false, <-c.exited
	}
}

// Session socket URL of an API origin
func wsOrigin(origin string) string {
	return strings.Replace(origin, "http:", "ws:", 1) + "/api/ws"
}
