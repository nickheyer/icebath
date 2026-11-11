package agent

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const loopbackOwnerEnv = "ICEBATH_TEST_LOOPBACK_OWNER"

// What a loopback owner child serves, its WS behaviour and how many identifies answer ready before draining
type loopbackOwnerSpec struct {
	Home            string `json:"home"`
	Mode            string `json:"mode"`
	ReadyIdentifies int64  `json:"ready_identifies"`
}

// A separate owner process on loopback, its control socket and WS endpoint real, its events logged under the home
type loopbackOwner struct {
	home   string
	origin string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
}

// Starts an owner child for the home and waits until its control socket and WS listener serve
func startLoopbackOwner(t *testing.T, home, mode string, readyIdentifies int64) *loopbackOwner {
	t.Helper()
	spec, err := json.Marshal(loopbackOwnerSpec{Home: home, Mode: mode, ReadyIdentifies: readyIdentifies})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), loopbackOwnerEnv+"="+string(spec))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	owner := &loopbackOwner{home: home, cmd: cmd, stdin: stdin}
	t.Cleanup(owner.stop)
	ready := filepath.Join(home, "owner-ready")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(ready); err == nil && len(data) > 0 {
			owner.origin = string(data)
			return owner
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("loopback owner never became ready")
	return nil
}

// Ends the owner child by closing its stdin
func (o *loopbackOwner) stop() {
	_ = o.stdin.Close()
	done := make(chan struct{})
	go func() { _ = o.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = o.cmd.Process.Kill()
		<-done
	}
}

// Events the owner child logged, one decoded line each
func (o *loopbackOwner) events(t *testing.T) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(o.home, "owner-events.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		out = append(out, event)
	}
	return out
}

// Events of one kind the owner child logged
func (o *loopbackOwner) eventsOf(t *testing.T, kind string) []map[string]any {
	var out []map[string]any
	for _, event := range o.events(t) {
		if event["kind"] == kind {
			out = append(out, event)
		}
	}
	return out
}

// Runs the loopback owner when the test binary was started as one
func dispatchLoopbackOwnerChild() {
	raw := os.Getenv(loopbackOwnerEnv)
	if raw == "" {
		return
	}
	var spec loopbackOwnerSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		panic(err)
	}
	os.Exit(runLoopbackOwner(spec))
}

// Serves the owner's WS endpoint and control socket until stdin closes
func runLoopbackOwner(spec loopbackOwnerSpec) int {
	var logMu sync.Mutex
	logEvent := func(kind string, fields map[string]any) {
		fields["kind"] = kind
		line, _ := json.Marshal(fields)
		logMu.Lock()
		defer logMu.Unlock()
		f, err := os.OpenFile(filepath.Join(spec.Home, "owner-events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		_, _ = f.Write(append(line, '\n'))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	origin := "http://" + listener.Addr().String()
	upgrader := websocket.Upgrader{Subprotocols: []string{gateway.CanonicalGatewayProtocol}, CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ws", func(w http.ResponseWriter, r *http.Request) {
		protocols := websocket.Subprotocols(r)
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		logEvent("connect", map[string]any{"protocols": protocols})
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var request map[string]any
			if err := json.Unmarshal(data, &request); err != nil {
				return
			}
			logEvent("request", map[string]any{"method": request["method"]})
			result := map[string]any{"ok": true}
			if spec.Mode == "method" {
				result = map[string]any{"method": request["method"]}
			}
			reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": result})
			if err := ws.WriteMessage(websocket.TextMessage, reply); err != nil {
				return
			}
			if spec.Mode == "reply-close" {
				_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				return
			}
		}
	})
	go func() { _ = http.Serve(listener, mux) }()
	home, err := gateway.CanonicalHome(spec.Home)
	if err != nil {
		panic(err)
	}
	var identifies atomic.Int64
	server, err := gateway.NewGatewayControlServer(home, map[string]gateway.ControlVerbHandler{
		"identify": func(map[string]any) (any, error) {
			state := "ready"
			if n := identifies.Add(1); spec.ReadyIdentifies > 0 && n > spec.ReadyIdentifies {
				state = "draining"
			}
			payload := gateway.BuildIdentifyPayload()
			for _, kv := range [][2]any{{"runtime_protocol", int64(1)}, {"state", state}, {"supervisor", "external"},
				{"capabilities", []any{"session-authority-v1"}}, {"authority_epoch", int64(1)}, {"instance_id", "owner-instance"},
				{"api_origin", origin}, {"served_profiles", []any{pyjson.ObjectOf("home", home, "profile_id", home)}}} {
				payload.Set(kv[0].(string), kv[1])
			}
			return payload, nil
		},
	})
	if err != nil {
		panic(err)
	}
	var tickets atomic.Int64
	server.AddPrivateHandlers(map[string]gateway.ControlPrivateHandler{
		"session-ticket": func(params map[string]any, _ string) (any, error) {
			ticket := "ticket-" + strings.Repeat("x", int(tickets.Add(1)))
			logEvent("ticket", map[string]any{"purpose": params["purpose"], "ticket": ticket})
			return pyjson.ObjectOf("ticket", ticket, "profile_id", home, "instance_id", "owner-instance", "runtime_protocol", int64(1)), nil
		},
	})
	if !server.Start() {
		return 1
	}
	defer server.Stop()
	if err := os.WriteFile(filepath.Join(spec.Home, "owner-ready"), []byte(origin), 0o600); err != nil {
		panic(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}
