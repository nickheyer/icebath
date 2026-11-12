//go:build linux

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Session token the serve children of these cases accept on the socket upgrade
const serveTurnToken = "serve-turn-session-token"

// State and user homes of a child whose model is the loopback peer
func serveTurnHomes(t *testing.T, peer *modelPeer) (string, string) {
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
		"model":     map[string]any{"provider": "custom", "default": "local-wire-stub", "base_url": peer.server.URL + "/v1"},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	return h, user
}

// Environment of a re-executed binary run as the program under the two homes
func serveTurnEnv(h, user string, peer *modelPeer) []string {
	var env []string
	for _, name := range []string{"PATH", "LANG", "TZ", "TIRITH_ENABLED"} {
		if value, set := os.LookupEnv(name); set {
			env = append(env, name+"="+value)
		}
	}
	return append(env, "HOME="+user, "USERPROFILE="+user, "ICEBATH_HOME="+h, "OPENAI_API_KEY=loopback-only",
		"OPENAI_BASE_URL="+peer.server.URL+"/v1", "ICEBATH_DASHBOARD_SESSION_TOKEN="+serveTurnToken, runMainEnv+"=1")
}

// A running serve child with its output files and exit channel
type serveChild struct {
	process *exec.Cmd
	exited  chan error
	stopped bool
	stdout  string
	stderr  string
	home    string
	port    int
}

// Output of the child and the failure lines of its home logs
func (c *serveChild) logs() string {
	out, _ := os.ReadFile(c.stdout)
	errText, _ := os.ReadFile(c.stderr)
	return "--- stdout\n" + string(out) + "--- stderr\n" + string(errText) + "--- home logs\n" + homeFailureLines(c.home)
}

// Boots the binary as serve on an ephemeral port and waits for the backend ready line
func startServeChild(t *testing.T, h, user string, peer *modelPeer) *serveChild {
	t.Helper()
	dir := t.TempDir()
	child := &serveChild{process: exec.Command(os.Args[0], "serve", "--port", "0"), exited: make(chan error, 1),
		stdout: filepath.Join(dir, "stdout.log"), stderr: filepath.Join(dir, "stderr.log"), home: h}
	child.process.Env = serveTurnEnv(h, user, peer)
	out, err := os.Create(child.stdout)
	if err != nil {
		t.Fatal(err)
	}
	errOut, err := os.Create(child.stderr)
	if err != nil {
		t.Fatal(err)
	}
	child.process.Stdout, child.process.Stderr = out, errOut
	if err := child.process.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		child.exited <- child.process.Wait()
		out.Close()
		errOut.Close()
	}()
	t.Cleanup(func() {
		if child.stopped {
			return
		}
		_ = child.process.Process.Signal(syscall.SIGTERM)
		select {
		case <-child.exited:
		case <-time.After(15 * time.Second):
			_ = child.process.Process.Kill()
			<-child.exited
		}
	})
	for deadline := time.Now().Add(60 * time.Second); child.port == 0; time.Sleep(50 * time.Millisecond) {
		select {
		case err := <-child.exited:
			child.stopped = true
			t.Fatalf("serve exited before it was ready: %v\n%s", err, child.logs())
		default:
		}
		raw, _ := os.ReadFile(child.stdout)
		for _, line := range strings.Split(string(raw), "\n") {
			if rest, found := strings.CutPrefix(line, "ICEBATH_BACKEND_READY port="); found {
				child.port, _ = strconv.Atoi(strings.TrimSpace(rest))
			}
		}
		if child.port == 0 && time.Now().After(deadline) {
			t.Fatalf("no ready line\n%s", child.logs())
		}
	}
	return child
}

// Opens the session socket with the session token and routes its frames
func (c *serveChild) dial(t *testing.T) *rpcStream {
	t.Helper()
	url := "ws://127.0.0.1:" + strconv.Itoa(c.port) + "/api/ws?token=" + serveTurnToken
	ws, response, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).Dial(url, nil)
	if err != nil {
		refusal := ""
		if response != nil {
			body, _ := io.ReadAll(response.Body)
			refusal = response.Status + " " + string(body)
		}
		t.Fatalf("%v %s\n%s", err, refusal, c.logs())
	}
	t.Cleanup(func() { ws.Close() })
	read := func() (map[string]any, error) {
		var frame map[string]any
		err := ws.ReadJSON(&frame)
		return frame, err
	}
	return newRPCStream(read, func(frame map[string]any) error { return ws.WriteJSON(frame) }, c.logs)
}

// Sends SIGINT and fails unless the child exits with status 0
func (c *serveChild) interrupt(t *testing.T) {
	t.Helper()
	if err := c.process.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-c.exited:
		c.stopped = true
		if err != nil {
			t.Fatalf("serve exit = %v\n%s", err, c.logs())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("serve did not stop on SIGINT\n%s", c.logs())
	}
}

// The turn of one session over the socket, the transcript seen again and the session in the listing
func driveServedTurn(t *testing.T, first, second *rpcStream) {
	t.Helper()
	first.waitEvent(t, 0, 30*time.Second, func(event map[string]any) bool { return event["type"] == "gateway.ready" })
	created := first.result(t, "session.create", map[string]any{"cols": 100})
	sid, _ := created["session_id"].(string)
	stored, _ := created["stored_session_id"].(string)
	if sid == "" || stored == "" {
		t.Fatalf("session.create = %v", created)
	}
	start := first.eventCount()
	submitted := first.result(t, "prompt.submit", map[string]any{"session_id": sid, "text": "WS_SHARED please"})
	if submitted["status"] != "streaming" {
		t.Fatalf("prompt.submit = %v", submitted)
	}
	completed := first.waitEvent(t, start, 60*time.Second, eventOf("message.complete", sid))
	payload, _ := completed["payload"].(map[string]any)
	if text, _ := payload["text"].(string); !strings.Contains(text, "LOCAL_ACK_WS_SHARED") {
		t.Fatalf("message.complete = %v", completed)
	}
	history := first.result(t, "session.history", map[string]any{"session_id": sid})
	transcript := jsonText(history["messages"])
	if !strings.Contains(transcript, "WS_SHARED please") || !strings.Contains(transcript, "LOCAL_ACK_WS_SHARED") {
		t.Fatalf("session.history = %v", history)
	}
	listing := first.result(t, "session.list", nil)
	rows, _ := listing["sessions"].([]any)
	if !slices.ContainsFunc(rows, func(raw any) bool {
		row, isDict := raw.(map[string]any)
		return isDict && row["id"] == stored
	}) {
		t.Fatalf("session.list = %v", listing)
	}
	second.waitEvent(t, 0, 30*time.Second, func(event map[string]any) bool { return event["type"] == "gateway.ready" })
	resumed := second.result(t, "session.resume", map[string]any{"session_id": stored, "cols": 100})
	if resumed["session_id"] != sid || !strings.Contains(jsonText(resumed["messages"]), "LOCAL_ACK_WS_SHARED") {
		t.Fatalf("session.resume on a second socket = %v", resumed)
	}
}

func TestServeRunsATurnOverTheSessionSocket(t *testing.T) {
	peer := newModelPeer(t)
	h, user := serveTurnHomes(t, peer)
	child := startServeChild(t, h, user, peer)
	response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(child.port) + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	decodeErr := json.NewDecoder(response.Body).Decode(&status)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil {
		t.Fatalf("/api/status = %d %v\n%s", response.StatusCode, decodeErr, child.logs())
	}
	driveServedTurn(t, child.dial(t), child.dial(t))
	if got := peer.chatRequests(); got != 1 {
		t.Fatalf("model requests = %d", got)
	}
	child.interrupt(t)
}
