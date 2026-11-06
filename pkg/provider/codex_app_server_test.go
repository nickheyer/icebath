//go:build !windows

package provider

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Installs the gateway owned env blocklist sources the subprocess env scrubber requires
func installCodexEnvSources(t *testing.T) {
	t.Helper()
	prevC, prevR := tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv
	if tools.ConfigEnvTableKeys == nil {
		tools.ConfigEnvTableKeys = func() []string { return nil }
	}
	if tools.RegisteredAdapterSecretEnv == nil {
		tools.RegisteredAdapterSecretEnv = func(env *tools.Env) map[string]bool { return map[string]bool{} }
	}
	t.Cleanup(func() { tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv = prevC, prevR })
}

// Writes an executable shell script standing in for the codex CLI
func fakeCodexBin(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func startFakeCodex(t *testing.T, body string) *CodexAppServerClient {
	t.Helper()
	installCodexEnvSources(t)
	c, err := NewCodexAppServerClient(CodexAppServerOptions{CodexBin: fakeCodexBin(t, body)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(3 * time.Second) })
	return c
}

func codexPendingCount(c *CodexAppServerClient) int {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return len(c.pending)
}

func TestCodexCloseFailsInFlightRequestImmediately(t *testing.T) {
	c := startFakeCodex(t, "cat > /dev/null")
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := c.Request("turn/start", nil, 10*time.Second)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	c.Close(3 * time.Second)
	err := <-done
	var transport *CodexAppServerTransportError
	if !errors.As(err, &transport) || !strings.Contains(err.Error(), "clos") || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
	if codexPendingCount(c) != 0 {
		t.Fatal("pending slot left behind")
	}
}

func TestCodexWriteFailureRaisesTransportErrorAndDropsPending(t *testing.T) {
	c := startFakeCodex(t, "exec 0<&-; exit 0")
	select {
	case <-c.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("fake codex did not exit")
	}
	_, err := c.Request("turn/start", nil, time.Second)
	var transport *CodexAppServerTransportError
	var appErr *CodexAppServerError
	if !errors.As(err, &transport) || !errors.As(err, &appErr) {
		t.Fatalf("err %T %v", err, err)
	}
	if codexPendingCount(c) != 0 {
		t.Fatal("pending slot left behind")
	}
}

func TestCodexStdoutEOFFailsInFlightRequestPromptly(t *testing.T) {
	c := startFakeCodex(t, "read line; exit 0")
	start := time.Now()
	_, err := c.Request("turn/start", nil, 10*time.Second)
	var transport *CodexAppServerTransportError
	if !errors.As(err, &transport) || !strings.Contains(err.Error(), "stdout closed") || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
	if codexPendingCount(c) != 0 {
		t.Fatal("pending slot left behind")
	}
	c.failPendingRequests("codex app-server client is closing")
}

func TestCodexJSONRPCRoundTripRoutesRepliesQueuesAndStderr(t *testing.T) {
	c := startFakeCodex(t, `read line
echo '{"id": 1, "result": {"userAgent": "fake/0.0.0"}}'
read line
echo '{"method": "turn/started", "params": {"threadId": "t"}}'
echo '{"id": "s1", "method": "item/commandExecution/requestApproval", "params": {}}'
echo 'garbage'
echo 'boom on stderr' >&2
read line
echo '{"id": 2, "error": {"code": -32601, "message": "nope"}}'
cat > /dev/null`)
	result, err := c.Initialize("icebath", "Icebath Agent", "dev", nil, 5*time.Second)
	if err != nil || cxGet(result, "userAgent") != "fake/0.0.0" {
		t.Fatalf("initialize %v %v", result, err)
	}
	if _, err := c.Initialize("icebath", "Icebath Agent", "dev", nil, time.Second); err == nil || err.Error() != "already initialized" {
		t.Fatalf("second initialize %v", err)
	}
	note := c.TakeNotification(5 * time.Second)
	if cxGet(note, "method") != "turn/started" {
		t.Fatalf("notification %v", note)
	}
	if req := c.TakeServerRequest(5 * time.Second); cxGet(req, "id") != "s1" {
		t.Fatalf("server request %v", req)
	}
	_, err = c.Request("bogus", nil, 5*time.Second)
	var appErr *CodexAppServerError
	if !errors.As(err, &appErr) || err.Error() != "codex app-server error -32601: nope" {
		t.Fatalf("error reply %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		tail := c.StderrTail(20)
		if slices.Contains(tail, "<non-json on stdout> b'garbage'") && slices.Contains(tail, "boom on stderr") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stderr tail %q", c.StderrTail(20))
}

func TestCodexRequestTimeoutMessage(t *testing.T) {
	c := startFakeCodex(t, "cat > /dev/null")
	_, err := c.Request("turn/start", nil, 100*time.Millisecond)
	if err == nil || err.Error() != "codex app-server method 'turn/start' timed out after 0.1s" || !codexIsTimeout(err) {
		t.Fatalf("timeout %v", err)
	}
}

func TestCodexCheckBinary(t *testing.T) {
	ok, msg, err := CheckCodexBinary("/nonexistent/codex", CodexMinVersion)
	if ok || err != nil || msg != "codex CLI not found at '/nonexistent/codex'. Install with: npm i -g @openai/codex" {
		t.Fatalf("missing %v %q %v", ok, msg, err)
	}
	if ok, msg, _ := CheckCodexBinary(fakeCodexBin(t, "echo 'codex-cli 0.130.0'"), CodexMinVersion); !ok || msg != "0.130.0" {
		t.Fatalf("current %v %q", ok, msg)
	}
	if ok, msg, _ := CheckCodexBinary(fakeCodexBin(t, "echo 'codex-cli 0.100.2'"), CodexMinVersion); ok || msg != "codex 0.100.2 is older than required 0.125.0. Run: npm i -g @openai/codex" {
		t.Fatalf("old %v %q", ok, msg)
	}
	if ok, msg, _ := CheckCodexBinary(fakeCodexBin(t, "echo ' bad flag ' >&2; exit 3"), CodexMinVersion); ok || msg != "codex --version exited 3: bad flag" {
		t.Fatalf("exit %v %q", ok, msg)
	}
	if ok, msg, _ := CheckCodexBinary(fakeCodexBin(t, "echo 'codex dev'"), CodexMinVersion); ok || msg != `could not parse codex version from: 'codex dev\n'` {
		t.Fatalf("unparsable %v %q", ok, msg)
	}
	if v, ok := ParseCodexVersion("codex-cli 0.130.0 (abc)"); !ok || v != [3]int{0, 130, 0} {
		t.Fatalf("parse %v %v", v, ok)
	}
}

func TestCodexSpawnEnvAndKanbanWorkerOverrides(t *testing.T) {
	installCodexEnvSources(t)
	t.Setenv("RUST_LOG", "")
	os.Unsetenv("RUST_LOG")
	t.Setenv("ICEBATH_KANBAN_TASK", "")
	os.Unsetenv("ICEBATH_KANBAN_TASK")
	argv, env := codexAppServerSpawn(CodexAppServerOptions{CodexBin: "codex", CodexHome: "/tmp/codex-home", ExtraArgs: []string{"--x"}})
	if env["CODEX_HOME"] != "/tmp/codex-home" || env["RUST_LOG"] != "warn" || !slices.Equal(argv, []string{"codex", "app-server", "--x"}) {
		t.Fatalf("plain spawn %v %v %v", argv, env["CODEX_HOME"], env["RUST_LOG"])
	}
	t.Setenv("ICEBATH_KANBAN_TASK", "task-1")
	t.Setenv("ICEBATH_KANBAN_DB", "/boards/main/kanban.db")
	t.Setenv("ICEBATH_NON_DISPATCHER_OWNED_CONTEXT", "")
	t.Setenv("ICEBATH_DELEGATED_CHILD_CONTEXT", "")
	argv, _ = codexAppServerSpawn(CodexAppServerOptions{CodexBin: "codex"})
	joined := strings.Join(argv, "\n")
	for _, want := range []string{
		`mcp_servers.icebath-tools.env.ICEBATH_KANBAN_TASK="task-1"`,
		`mcp_servers.icebath-tools.env.ICEBATH_KANBAN_DB="/boards/main/kanban.db"`,
		`mcp_servers.icebath-tools.env.ICEBATH_DELEGATED_CHILD_CONTEXT=""`,
		`sandbox_mode="workspace-write"`,
		`sandbox_workspace_write.writable_roots=["/boards/main"]`,
		"sandbox_workspace_write.network_access=false",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("owned worker argv lacks %s: %v", want, argv)
		}
	}
	t.Setenv("ICEBATH_NON_DISPATCHER_OWNED_CONTEXT", "1")
	if argv, _ = codexAppServerSpawn(CodexAppServerOptions{CodexBin: "codex"}); len(argv) != 2 {
		t.Fatalf("non dispatcher owned worker got overrides %v", argv)
	}
}

func TestCodexCloseReapsIndependentDescendantProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	c := startFakeCodex(t, "setsid sleep 300 & echo $! > "+pidFile+"\ncat > /dev/null")
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("descendant never started")
	}
	time.Sleep(100 * time.Millisecond)
	c.Close(3 * time.Second)
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	t.Fatalf("descendant %d survived close", pid)
}

func TestCodexCloseEscalatesToTreeKillWhenRootIgnoresSigterm(t *testing.T) {
	c := startFakeCodex(t, "trap '' TERM\nwhile :; do sleep 1; done")
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	c.Close(200 * time.Millisecond)
	if c.IsAlive() || time.Since(start) > 3*time.Second {
		t.Fatalf("root survived close after %s", time.Since(start))
	}
}
