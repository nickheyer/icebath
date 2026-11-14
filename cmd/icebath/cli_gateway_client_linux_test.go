//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Two fresh homes and the environment of the binary re-executed as the program against the loopback model
func cliGatewayHomes(t *testing.T, peer *modelPeer) (string, []string) {
	t.Helper()
	tmp := t.TempDir()
	h, user := filepath.Join(tmp, "state"), filepath.Join(tmp, "user")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(user, 0o755); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{
		"gateway":   map[string]any{"multiplex_profiles": false},
		"model":     map[string]any{"provider": "custom", "default": "local-wire-stub", "base_url": peer.server.URL + "/v1"},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, name := range []string{"PATH", "LANG", "TZ", "TIRITH_ENABLED"} {
		if value, set := os.LookupEnv(name); set {
			env = append(env, name+"="+value)
		}
	}
	return h, append(env, "HOME="+user, "USERPROFILE="+user, "ICEBATH_HOME="+h, "OPENAI_API_KEY=loopback-only",
		"OPENAI_BASE_URL="+peer.server.URL+"/v1", runMainEnv+"=1")
}

// Test binary under the program's own name, so the gateway it launches carries a command line gateway stop recognizes
func cliProgramBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "icebath")
	if err := os.Link(os.Args[0], bin); err == nil {
		return bin
	}
	raw, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// Runs the binary as the program with the given arguments and returns its exit code, stdout and stderr
func runCLIProgram(t *testing.T, bin string, env []string, timeout time.Duration, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, bin, args...)
	command.Env = env
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	command.Stdin = nil
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("%v did not finish in %s\nstdout:\n%s\nstderr:\n%s", args, timeout, stdout.String(), stderr.String())
	}
	code := 0
	if exit, isExit := err.(*exec.ExitError); isExit {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

// Stops the gateway a case started through the same binary and waits until its pid file and socket are gone
func stopCLIGateway(t *testing.T, bin, h string, env []string) {
	t.Helper()
	code, stdout, stderr := runCLIProgram(t, bin, env, 60*time.Second, "gateway", "stop")
	if code != 0 {
		t.Errorf("gateway stop = %d\n%s\n%s", code, stdout, stderr)
	}
	var identified map[string]any
	var pidErr error
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var err error
		if identified, err = gatewayControl(h, "identify", nil); err != nil {
			if _, pidErr = os.Stat(filepath.Join(h, "gateway.pid")); os.IsNotExist(pidErr) {
				return
			}
		}
	}
	t.Errorf("the gateway still owns %s after gateway stop: identify %v, pid file %v\n%s\n%s\n%s", h, identified, pidErr, stdout, stderr, homeFailureLines(h))
}

var printedSessionLine = regexp.MustCompile(`(?m)^Session:\s+(\S+)\s*$`)

func TestClassicChatQueryRunsInTheGatewayItLaunches(t *testing.T) {
	peer := newModelPeer(t)
	h, env := cliGatewayHomes(t, peer)
	bin := cliProgramBinary(t)
	bootHome = h
	t.Cleanup(func() { stopCLIGateway(t, bin, h, env) })
	code, stdout, stderr := runCLIProgram(t, bin, env, 120*time.Second, "chat", "-q", "WS_SHARED please")
	if code != 0 || !strings.Contains(stdout, "LOCAL_ACK_WS_SHARED") {
		t.Fatalf("chat -q = %d\nstdout:\n%s\nstderr:\n%s\n%s", code, stdout, stderr, homeFailureLines(h))
	}
	printed := printedSessionLine.FindStringSubmatch(stdout)
	if printed == nil {
		t.Fatalf("no session line in\n%s", stdout)
	}
	sid := printed[1]
	descriptor, err := gatewayControl(h, "identify", nil)
	if err != nil || descriptor["state"] != "ready" {
		t.Fatalf("identify after chat -q = %v, %v\n%s", descriptor, err, homeFailureLines(h))
	}
	origin, _ := descriptor["api_origin"].(string)
	url := strings.Replace(origin, "http:", "ws:", 1) + "/api/ws"
	resolved, err := filepath.EvalSymlinks(h)
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := dialSession(t, h, url, map[string]any{"profile_id": resolved, "instance_id": descriptor["instance_id"], "purpose": "interactive"})
	var source string
	var ended sql.NullString
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(h, "state.db")+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		err = db.QueryRowContext(context.Background(), "SELECT source, end_reason FROM sessions WHERE id = ?", sid).Scan(&source, &ended)
		db.Close()
		if err != nil {
			t.Fatalf("gateway row of %s: %v", sid, err)
		}
		if ended.String == "cli_close" || !time.Now().Before(deadline) {
			break
		}
	}
	if source != "oneshot" || ended.String != "cli_close" {
		t.Fatalf("gateway row of %s = (%q, %v), want the one shot row the viewer closed", sid, source, ended)
	}
	snapshot := sessionResult(t, ws, "session.resume", map[string]any{"session_id": sid})
	if snapshot["stored_session_id"] != sid {
		t.Fatalf("session.resume %s answered %v", sid, snapshot["stored_session_id"])
	}
	messages, _ := snapshot["messages"].([]any)
	var user, assistant bool
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		content, _ := json.Marshal(message["content"])
		user = user || (message["role"] == "user" && strings.Contains(string(content), "WS_SHARED please"))
		assistant = assistant || (message["role"] == "assistant" && strings.Contains(string(content), "LOCAL_ACK_WS_SHARED"))
	}
	if !user || !assistant {
		t.Fatalf("the turn is not in the gateway transcript of %s: %v", sid, snapshot["messages"])
	}
	if got := peer.chatRequests(); got != 1 {
		t.Fatalf("model requests = %d", got)
	}
}
