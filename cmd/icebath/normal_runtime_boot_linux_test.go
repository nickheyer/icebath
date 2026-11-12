//go:build linux

package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Loopback OpenAI compatible model answering every chat with a fixed acknowledgement of its last user text
type modelPeer struct {
	mu       sync.Mutex
	requests []map[string]any
	server   *httptest.Server
}

func newModelPeer(t *testing.T) *modelPeer {
	t.Helper()
	peer := &modelPeer{}
	peer.server = httptest.NewServer(http.HandlerFunc(peer.serve))
	t.Cleanup(peer.server.Close)
	return peer
}

func (p *modelPeer) chatRequests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *modelPeer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data": [{"id": "local-wire-stub", "object": "model"}]}`)
		return
	}
	var request map[string]any
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var last any = ""
	messages, _ := request["messages"].([]any)
	for i := len(messages) - 1; i >= 0; i-- {
		if m, isDict := messages[i].(map[string]any); isDict && m["role"] == "user" {
			if content, has := m["content"]; has {
				last = content
			}
			break
		}
	}
	lastJSON, _ := json.Marshal(last)
	marker := "MESSAGING_WARM"
	if strings.Contains(string(lastJSON), "WS_SHARED") {
		marker = "WS_SHARED"
	}
	reply := "LOCAL_ACK_" + marker
	if _, isChat := request["messages"]; isChat {
		p.mu.Lock()
		p.requests = append(p.requests, request)
		p.mu.Unlock()
	}
	base := func(extra map[string]any) []byte {
		body := map[string]any{"id": "chatcmpl-local", "model": "local-wire-stub", "created": 1}
		for k, v := range extra {
			body[k] = v
		}
		raw, _ := json.Marshal(body)
		return raw
	}
	if stream, _ := request["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, part := range []struct {
			delta  map[string]any
			finish any
		}{{map[string]any{"role": "assistant", "content": reply}, nil}, {map[string]any{}, "stop"}} {
			chunk := base(map[string]any{"object": "chat.completion.chunk", "choices": []any{
				map[string]any{"index": 0, "delta": part.delta, "finish_reason": part.finish}}})
			_, _ = w.Write(append(append([]byte("data: "), chunk...), '\n', '\n'))
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(base(map[string]any{"object": "chat.completion", "choices": []any{
		map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": reply}, "finish_reason": "stop"}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}))
}

// Result of one verb on the bootstrap control socket of the home, an error for a refused verb
func gatewayControl(h, verb string, params map[string]any) (map[string]any, error) {
	if params == nil {
		params = map[string]any{}
	}
	request, err := json.Marshal(map[string]any{"protocol": 1, "id": 1, "verb": verb, "params": params})
	if err != nil {
		return nil, err
	}
	path := filepath.Join(h, "gateway.sock")
	if pointer, err := os.ReadFile(filepath.Join(h, "gateway.sock.path")); err == nil {
		path = strings.TrimSpace(string(pointer))
	}
	peer, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer peer.Close()
	if err := peer.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return nil, err
	}
	if _, err := peer.Write(append(request, '\n')); err != nil {
		return nil, err
	}
	raw, err := bufio.NewReader(peer).ReadBytes('\n')
	if err != nil && len(raw) == 0 {
		return nil, err
	}
	var reply struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	if !reply.OK {
		return nil, fmt.Errorf("control verb %s refused: %s", verb, raw)
	}
	return reply.Result, nil
}

// Error and crash lines of every log of the home, what a failed call prints beside its reply
func homeFailureLines(h string) string {
	var logged []string
	names, _ := filepath.Glob(filepath.Join(h, "logs", "*.log"))
	for _, name := range names {
		raw, _ := os.ReadFile(name)
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "crash") || strings.Contains(line, " ERROR ") || strings.Contains(line, " WARNING ") {
				logged = append(logged, filepath.Base(name)+": "+line)
			}
		}
	}
	return strings.Join(logged, "\n")
}

// Home of the gateway the running case started, read by the failure output of its calls
var bootHome string

// One JSON RPC call over a session socket, the reply carrying the method as its id
func sessionRPC(t *testing.T, ws *websocket.Conn, method string, params map[string]any) map[string]any {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	if err := ws.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": method, "method": method, "params": params}); err != nil {
		t.Fatal(err)
	}
	if err := ws.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		var reply map[string]any
		if err := ws.ReadJSON(&reply); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if reply["id"] == method {
			return reply
		}
	}
}

// Result of a call that must succeed
func sessionResult(t *testing.T, ws *websocket.Conn, method string, params map[string]any) map[string]any {
	t.Helper()
	reply := sessionRPC(t, ws, method, params)
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: %v\n%s", method, reply, homeFailureLines(bootHome))
	}
	return result
}

// Fails unless the call is refused with the expected error message
func sessionRefusal(t *testing.T, ws *websocket.Conn, method, expected string, params map[string]any) {
	t.Helper()
	reply := sessionRPC(t, ws, method, params)
	refusal, _ := reply["error"].(map[string]any)
	if refusal["message"] != expected {
		t.Fatalf("%s: %v, want error %s", method, reply, expected)
	}
}

// Session socket of the owner opened with a fresh ticket from its control socket
func dialSession(t *testing.T, h, url string, binding map[string]any) (*websocket.Conn, []string) {
	t.Helper()
	grant, err := gatewayControl(h, "session-ticket", binding)
	if err != nil {
		t.Fatal(err)
	}
	ticket, _ := grant["ticket"].(string)
	protocols := []string{gateway.CanonicalGatewayProtocol, gateway.GatewayWSTicketPrefix + ticket}
	ws, response, err := (&websocket.Dialer{Subprotocols: protocols, HandshakeTimeout: 10 * time.Second}).Dial(url, nil)
	if err != nil {
		refusal := ""
		if response != nil {
			body, _ := io.ReadAll(response.Body)
			refusal = fmt.Sprintf(" (%s %s)", response.Status, body)
		}
		t.Fatalf("%v%s\n%s", err, refusal, homeFailureLines(h))
	}
	t.Cleanup(func() { ws.Close() })
	return ws, protocols
}

// Fails unless the upgrade is refused with an HTTP status
func requireUpgradeRefused(t *testing.T, url string, protocols []string) {
	t.Helper()
	ws, response, err := (&websocket.Dialer{Subprotocols: protocols, HandshakeTimeout: 10 * time.Second}).Dial(url, nil)
	if err == nil {
		ws.Close()
		t.Fatal("the upgrade was accepted")
	}
	if !errors.Is(err, websocket.ErrBadHandshake) || response == nil {
		t.Fatalf("upgrade error = %v", err)
	}
}

// The handshake of the ordinary launch, one shared turn seen through fresh sockets and a mutation retried after reconnect
func ordinaryHandshake(t *testing.T, h string, descriptor map[string]any) {
	t.Helper()
	bootHome = h
	origin, _ := descriptor["api_origin"].(string)
	url := strings.Replace(origin, "http:", "ws:", 1) + "/api/ws"
	requireUpgradeRefused(t, url, nil)
	resolved, err := filepath.EvalSymlinks(h)
	if err != nil {
		t.Fatal(err)
	}
	binding := map[string]any{"profile_id": resolved, "instance_id": descriptor["instance_id"], "purpose": "interactive"}
	ws, protocols := dialSession(t, h, url, binding)
	if err := ws.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "session.resume",
		"params": map[string]any{"session_id": "not-registered"}}); err != nil {
		t.Fatal(err)
	}
	for {
		if err := ws.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var reply map[string]any
		if err := ws.ReadJSON(&reply); err != nil {
			t.Fatal(err)
		}
		if reply["id"] == float64(7) {
			refusal, _ := reply["error"].(map[string]any)
			if refusal["message"] != "not_found" {
				t.Fatalf("resume of an unregistered session: %v", reply)
			}
			break
		}
	}
	if got := ws.Subprotocol(); got != gateway.CanonicalGatewayProtocol {
		t.Fatalf("subprotocol = %q", got)
	}
	status := sessionResult(t, ws, "setup.status", nil)
	if status["provider_configured"] != true {
		t.Fatalf("setup.status = %v", status)
	}
	readiness := sessionResult(t, ws, "setup.runtime_check", map[string]any{"provider": "custom"})
	if readiness["ok"] != true || readiness["provider"] != "custom" {
		t.Fatalf("setup.runtime_check = %v", readiness)
	}
	for _, method := range []string{"setup.status", "setup.runtime_check"} {
		sessionRefusal(t, ws, method, "profile_mismatch", map[string]any{"profile": "foreign-profile"})
	}
	created := sessionResult(t, ws, "session.create", map[string]any{"request_id": "ordinary-launch", "source": "cli"})
	sid, _ := created["session_id"].(string)
	if sid == "" || sid != created["stored_session_id"] {
		t.Fatalf("session.create = %v", created)
	}
	initial := sessionResult(t, ws, "session.resume", map[string]any{"session_id": sid})
	admitted := sessionResult(t, ws, "prompt.submit", map[string]any{"session_id": sid, "input_id": "ordinary-input", "text": "WS_SHARED"})
	if admitted["status"] != "queued" {
		t.Fatalf("prompt.submit = %v", admitted)
	}
	ws.Close()

	viewer, _ := dialSession(t, h, url, binding)
	var snapshot map[string]any
	for deadline := time.Now().Add(25 * time.Second); ; {
		snapshot = sessionResult(t, viewer, "session.resume", map[string]any{"session_id": sid})
		messages, _ := json.Marshal(snapshot["messages"])
		if strings.Contains(string(messages), "LOCAL_ACK_WS_SHARED") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reply never reached the transcript: %v\n%s", snapshot["messages"], homeFailureLines(h))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if snapshot["stored_session_id"] != sid {
		t.Fatalf("stored_session_id = %v", snapshot["stored_session_id"])
	}
	var completed []map[string]any
	var replay map[string]any
	for deadline := time.Now().Add(25 * time.Second); len(completed) == 0; {
		replay = sessionResult(t, viewer, "session.events.since", map[string]any{"session_id": sid,
			"replay_epoch": initial["replay_epoch"], "last_sequence": initial["last_sequence"]})
		if replay["snapshot_required"] == true {
			t.Fatalf("replay needs a snapshot: %v", replay)
		}
		events, _ := replay["events"].([]any)
		for _, raw := range events {
			if event, isDict := raw.(map[string]any); isDict && event["type"] == "message.complete" {
				completed = append(completed, event)
			}
		}
		if len(completed) == 0 {
			if time.Now().After(deadline) {
				var types []any
				for _, raw := range events {
					if event, isDict := raw.(map[string]any); isDict {
						types = append(types, event["type"])
					}
				}
				t.Fatalf("the turn never completed, replayed %v\n%s", types, homeFailureLines(h))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if len(completed) != 1 {
		t.Fatalf("completed = %v", replay)
	}
	payload, _ := completed[0]["payload"].(map[string]any)
	if text, _ := payload["text"].(string); !strings.Contains(text, "LOCAL_ACK_WS_SHARED") {
		t.Fatalf("message.complete = %v", completed[0])
	}
	snapshot = sessionResult(t, viewer, "session.resume", map[string]any{"session_id": sid})
	if seq, last := completed[0]["seq"].(float64), snapshot["last_sequence"].(float64); seq > last {
		t.Fatalf("seq %v after last_sequence %v", seq, last)
	}
	edit := map[string]any{"session_id": sid, "request_id": "ordinary-rename", "expected_revision": snapshot["revision"],
		"operation": "rename", "payload": map[string]any{"title": "Shared metadata receipt"}}
	renamed := sessionResult(t, viewer, "session.mutate", edit)
	if renamed["title"] != "Shared metadata receipt" {
		t.Fatalf("session.mutate = %v", renamed)
	}
	viewer.Close()

	again, _ := dialSession(t, h, url, binding)
	if retried := sessionResult(t, again, "session.mutate", edit); !reflect.DeepEqual(retried, renamed) {
		t.Fatalf("retried mutation = %v, want %v", retried, renamed)
	}
	stale := map[string]any{}
	for k, v := range edit {
		stale[k] = v
	}
	stale["request_id"], stale["payload"] = "stale-rename", map[string]any{"title": "Stale overwrite"}
	sessionRefusal(t, again, "session.mutate", "revision_conflict", stale)
	listing := sessionResult(t, again, "session.list", nil)
	rows, _ := listing["sessions"].([]any)
	index := slices.IndexFunc(rows, func(raw any) bool {
		row, isDict := raw.(map[string]any)
		return isDict && row["session_id"] == sid
	})
	if index < 0 || rows[index].(map[string]any)["title"] != renamed["title"] {
		t.Fatalf("session.list = %v", listing)
	}
	again.Close()
	requireUpgradeRefused(t, url, protocols)
}

func TestNormalEntrypointEarnsAuthenticatedAuthorityReadiness(t *testing.T) {
	peer := newModelPeer(t)
	tmp := t.TempDir()
	h, user := filepath.Join(tmp, "state"), filepath.Join(tmp, "user")
	if err := os.Mkdir(h, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(user, 0o755); err != nil {
		t.Fatal(err)
	}
	modelURL := peer.server.URL + "/v1"
	config, err := json.Marshal(map[string]any{
		"gateway":   map[string]any{"multiplex_profiles": false},
		"model":     map[string]any{"provider": "custom", "default": "local-wire-stub", "base_url": modelURL},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(tmp, "gateway.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	process := exec.Command(os.Args[0], "gateway", "run")
	for _, name := range []string{"PATH", "LANG", "TZ", "TIRITH_ENABLED"} {
		if value, set := os.LookupEnv(name); set {
			process.Env = append(process.Env, name+"="+value)
		}
	}
	process.Env = append(process.Env, "HOME="+user, "USERPROFILE="+user, "ICEBATH_HOME="+h,
		"OPENAI_API_KEY=loopback-only", "OPENAI_BASE_URL="+modelURL, runMainEnv+"=1")
	process.Stdout, process.Stderr = logFile, logFile
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- process.Wait() }()
	stopped := false
	defer func() {
		if stopped {
			return
		}
		_ = process.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = process.Process.Kill()
			<-exited
		}
	}()
	output := func() string {
		raw, _ := os.ReadFile(logPath)
		return string(raw)
	}
	descriptor := map[string]any{}
	for deadline := time.Now().Add(35 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		select {
		case err := <-exited:
			stopped = true
			t.Fatalf("the gateway exited before it was ready: %v\n%s", err, output())
		default:
		}
		if identified, err := gatewayControl(h, "identify", nil); err == nil {
			descriptor = identified
			if descriptor["state"] == "ready" {
				break
			}
		}
	}
	if descriptor["state"] != "ready" {
		t.Fatalf("descriptor = %v\n%s", descriptor, output())
	}
	epoch, _ := descriptor["authority_epoch"].(float64)
	if epoch <= 0 {
		t.Fatalf("authority_epoch = %v", descriptor["authority_epoch"])
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h, "state.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var storedEpoch int64
	var storedInstance string
	err = db.QueryRowContext(context.Background(), "SELECT epoch, instance_id FROM runtime_epoch").Scan(&storedEpoch, &storedInstance)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if float64(storedEpoch) != epoch || storedInstance != descriptor["instance_id"] {
		t.Fatalf("runtime_epoch = (%d, %s), descriptor = %v", storedEpoch, storedInstance, descriptor)
	}
	served := []any{map[string]any{"profile_id": h, "home": h}}
	if !reflect.DeepEqual(descriptor["served_profiles"], served) {
		t.Fatalf("served_profiles = %v, want %v", descriptor["served_profiles"], served)
	}
	capabilities, _ := descriptor["capabilities"].([]any)
	if !slices.Contains(capabilities, any("session-authority-v1")) {
		t.Fatalf("capabilities = %v", capabilities)
	}
	ordinaryHandshake(t, h, descriptor)
	if got := peer.chatRequests(); got != 1 {
		t.Fatalf("model requests = %d", got)
	}
	if err := process.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		stopped = true
		if err != nil {
			t.Fatalf("gateway exit = %v\n%s", err, output())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the gateway did not stop on SIGINT\n%s", output())
	}
	for _, name := range []string{"gateway.pid", "gateway.sock"} {
		if _, err := os.Stat(filepath.Join(h, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", name, err)
		}
	}
}
