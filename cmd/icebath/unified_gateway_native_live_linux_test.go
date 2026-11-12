//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Python str of a decoded JSON value, the text the native model peer acknowledges
func pyStrOfJSON(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	}
	return pyReprOfJSON(value)
}

// Python repr of a decoded JSON value, dict keys in sorted order
func pyReprOfJSON(value any) string {
	switch v := value.(type) {
	case string:
		quoted, _ := json.Marshal(v)
		inner := strings.ReplaceAll(string(quoted[1:len(quoted)-1]), `\"`, `"`)
		return "'" + strings.ReplaceAll(inner, "'", `\'`) + "'"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = pyReprOfJSON(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = pyReprOfJSON(key) + ": " + pyReprOfJSON(v[key])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyStrOfJSON(value)
}

// One chat request the native model peer received
type nativeModelRequest struct {
	Model           any
	Text            string
	Auth            string
	TitleGeneration bool
}

// Loopback model acknowledging the last user text, a BLOCK_ text held until release, the Model peer of the native live cases
type nativeModel struct {
	mu       sync.Mutex
	requests []nativeModelRequest
	blocked  chan struct{}
	release  chan struct{}
	once     sync.Once
	server   *httptest.Server
}

func newNativeModel(t *testing.T) *nativeModel {
	t.Helper()
	m := &nativeModel{blocked: make(chan struct{}), release: make(chan struct{})}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(func() {
		m.releaseAll()
		m.server.Close()
	})
	return m
}

// Releases every held request, the release event set
func (m *nativeModel) releaseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.release:
	default:
		close(m.release)
	}
}

// Clears both events for the next blocked text
func (m *nativeModel) clearEvents() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blocked, m.release = make(chan struct{}), make(chan struct{})
}

// Whether a BLOCK_ text arrived within the timeout
func (m *nativeModel) waitBlocked(timeout time.Duration) bool {
	m.mu.Lock()
	blocked := m.blocked
	m.mu.Unlock()
	select {
	case <-blocked:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Texts of the received chat requests in arrival order
func (m *nativeModel) texts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.requests))
	for i, r := range m.requests {
		out[i] = r.Text
	}
	return out
}

func (m *nativeModel) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Unsupported method", http.StatusNotImplemented)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var text any = ""
	if messages, _ := body["messages"].([]any); len(messages) > 0 {
		for i := len(messages) - 1; i >= 0; i-- {
			if message, isDict := messages[i].(map[string]any); isDict && message["role"] == "user" {
				text = message["content"]
				if text == nil {
					text = ""
				}
				break
			}
		}
		first, _ := messages[0].(map[string]any)
		head := pyStrOfJSON(first["content"])
		if first["content"] == nil {
			head = ""
		}
		textString, isString := text.(string)
		m.mu.Lock()
		m.requests = append(m.requests, nativeModelRequest{Model: body["model"], Text: pyStrOfJSON(text),
			Auth: r.Header.Get("Authorization"), TitleGeneration: strings.HasPrefix(head, "You name chat sessions")})
		blocked, release := m.blocked, m.release
		if isString && strings.HasPrefix(textString, "BLOCK_") {
			select {
			case <-blocked:
			default:
				close(blocked)
			}
		}
		m.mu.Unlock()
		if isString && strings.HasPrefix(textString, "BLOCK_") {
			select {
			case <-release:
			case <-time.After(60 * time.Second):
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": "NATIVE_ACK_" + pyStrOfJSON(text)}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": "stop"}
	frame := map[string]any{"id": "native", "model": body["model"], "choices": []any{choice},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}
	kind := "application/json"
	var payload []byte
	if stream, _ := body["stream"].(bool); stream {
		choice["delta"] = message
		delete(choice, "message")
		raw, _ := json.Marshal(frame)
		payload = []byte("data: " + string(raw) + "\n\ndata: [DONE]\n\n")
		kind = "text/event-stream"
	} else {
		payload, _ = json.Marshal(frame)
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
	_, _ = w.Write(payload)
}

// Disposable home, workspace and child environment around one native model, the harness fixture
type nativeHarness struct {
	home, work, url, audit string
	env                    []string
	peer                   *nativeModel
}

func newNativeHarness(t *testing.T) *nativeHarness {
	t.Helper()
	peer := newNativeModel(t)
	tmp := t.TempDir()
	hn := &nativeHarness{home: filepath.Join(tmp, "state"), work: filepath.Join(tmp, "work"), peer: peer,
		url: peer.server.URL + "/v1", audit: filepath.Join(tmp, "audit.jsonl")}
	user := filepath.Join(tmp, "user")
	if err := os.Mkdir(hn.home, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{user, hn.work} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeJSONConfig(t, filepath.Join(hn.home, "config.yaml"), map[string]any{
		"gateway":           map[string]any{"multiplex_profiles": false},
		"model":             map[string]any{"provider": "custom", "default": "native-model", "base_url": hn.url},
		"auxiliary":         map[string]any{"title_generation": map[string]any{"enabled": false}},
		"platform_toolsets": map[string]any{"cli": []any{}},
	})
	for _, name := range []string{"PATH", "LANG", "TZ"} {
		if value, set := os.LookupEnv(name); set {
			hn.env = append(hn.env, name+"="+value)
		}
	}
	hn.env = append(hn.env, "HOME="+user, "USERPROFILE="+user, "ICEBATH_HOME="+hn.home,
		testSiteEnv+"="+nativeAuditSite, nativeAuditEnv+"="+hn.audit,
		"OPENAI_API_KEY=loopback-only", "OPENAI_BASE_URL="+hn.url)
	return hn
}

// Ready owner started through the fixture mode, failing with its log and gateway log when it never gets ready
func (hn *nativeHarness) daemon(t *testing.T, logName, mode string, extra ...string) (*gatewayChild, map[string]any) {
	t.Helper()
	env := append(append([]string{}, hn.env...), extra...)
	if mode != "" {
		env = append(env, childModeEnv+"="+mode)
	}
	child := startGatewayChild(t, filepath.Join(filepath.Dir(hn.home), logName), env, gateway.GatewayRunModuleEntry)
	descriptor := map[string]any{}
	for deadline := time.Now().Add(180 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		select {
		case err := <-child.exited:
			child.stopped = true
			child.exited <- err
			deadline = time.Now()
			continue
		default:
		}
		if identified, err := gatewayControl(hn.home, "identify", nil); err == nil {
			descriptor = identified
			if descriptor["state"] == "ready" {
				return child, descriptor
			}
		}
	}
	gatewayLog, _ := os.ReadFile(filepath.Join(hn.home, "logs", "gateway.log"))
	stacks, _ := os.ReadFile(filepath.Join(hn.home, "logs", "stacks.txt"))
	t.Fatalf("descriptor = %v\n%s\n%s\n%s", descriptor, child.output(), tail(string(gatewayLog), 6000), stacks)
	return nil, nil
}

// Last n bytes of text
func tail(text string, n int) string {
	if len(text) > n {
		return text[len(text)-n:]
	}
	return text
}

// Session socket of the first served profile opened with a fresh ticket of the purpose
func (hn *nativeHarness) websocket(t *testing.T, descriptor map[string]any) *websocket.Conn {
	t.Helper()
	served, _ := descriptor["served_profiles"].([]any)
	first, _ := served[0].(map[string]any)
	grant, err := gatewayControl(hn.home, "session-ticket", map[string]any{"profile_id": first["profile_id"],
		"instance_id": descriptor["instance_id"], "purpose": "interactive"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, _ := grant["ticket"].(string)
	origin, _ := descriptor["api_origin"].(string)
	ws, _, err := (&websocket.Dialer{Subprotocols: []string{gateway.CanonicalGatewayProtocol, gateway.GatewayWSTicketPrefix + ticket},
		HandshakeTimeout: 20 * time.Second}).Dial(strings.Replace(origin, "http:", "ws:", 1)+"/api/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

// Rows of a read only query of the home state database
func (hn *nativeHarness) query(t *testing.T, query string, args ...any) [][]any {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(hn.home, "state.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, _ := rows.Columns()
	var out [][]any
	for rows.Next() {
		row := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range row {
			pointers[i] = &row[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

// Status of every admission by request id
func (hn *nativeHarness) admissions(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, row := range hn.query(t, "SELECT request_id, status FROM session_admissions") {
		out[fmt.Sprint(row[0])] = fmt.Sprint(row[1])
	}
	return out
}

// Waits for the admission to reach the status, failing with every status at the deadline
func (hn *nativeHarness) waitStatus(t *testing.T, requestID, status string, timeout time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if hn.admissions(t)[requestID] == status {
			return
		}
	}
	t.Fatalf("%s never reached %s: %v", requestID, status, hn.admissions(t))
}

// Exit of the owner after the gateway stop marker naming its pid, failing when it outlives ninety seconds
func (hn *nativeHarness) plannedStop(t *testing.T, owner *gatewayChild, descriptor map[string]any) error {
	t.Helper()
	pid := int(descriptor["pid"].(float64))
	marker, _ := json.Marshal(map[string]any{"target_pid": pid, "target_start_time": gateway.GetProcessStartTime(pid),
		"stopper_pid": os.Getpid(), "written_at": time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00")})
	markerPath := filepath.Join(hn.home, ".gateway-planned-stop.json")
	if err := os.WriteFile(markerPath, marker, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-owner.exited:
		owner.stopped = true
		return err
	case <-time.After(90 * time.Second):
		_, markerLeft := os.Stat(markerPath)
		gatewayLog, _ := os.ReadFile(filepath.Join(hn.home, "logs", "gateway.log"))
		t.Fatalf("planned stop did not exit within 90s %s marker present %v\n%s", marker, markerLeft == nil, tail(string(gatewayLog), 6000))
		return nil
	}
}

// Texts of the message.complete events read from the socket until n arrived
func messageCompletes(t *testing.T, ws *websocket.Conn, n int, timeout time.Duration) []string {
	t.Helper()
	if err := ws.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for len(texts) < n {
		var frame map[string]any
		if err := ws.ReadJSON(&frame); err != nil {
			t.Fatalf("after %v: %v", texts, err)
		}
		params, _ := frame["params"].(map[string]any)
		if frame["method"] == "event" && params["type"] == "message.complete" {
			payload, _ := params["payload"].(map[string]any)
			text, _ := payload["text"].(string)
			texts = append(texts, text)
		}
	}
	return texts
}

// Whether a TCP connect to the loopback port succeeds
func portOpen(port string) bool {
	peer, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 2*time.Second)
	if err != nil {
		return false
	}
	peer.Close()
	return true
}

// Exit code of a process error, zero for none
func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestNativeGatewayRuntimeLive(t *testing.T) {
	hn := newNativeHarness(t)
	h, peer := hn.home, hn.peer
	owner, descriptor := hn.daemon(t, "first.log", stackDumpDaemonMode, "UGW_STACK_DUMP_AFTER=20")
	receipt := map[string]any{"os": "linux", "first_pid": owner.process.Process.Pid}
	origin, _ := descriptor["api_origin"].(string)

	if ws, response, err := (&websocket.Dialer{HandshakeTimeout: 20 * time.Second}).Dial(strings.Replace(origin, "http:", "ws:", 1)+"/api/ws", nil); err == nil {
		ws.Close()
		t.Fatal("an unauthenticated socket was accepted")
	} else if !errors.Is(err, websocket.ErrBadHandshake) || response == nil {
		t.Fatalf("unauthenticated dial = %v", err)
	}
	capabilities, _ := descriptor["capabilities"].([]any)
	if epoch, _ := descriptor["authority_epoch"].(float64); !slices.Contains(capabilities, any("session-authority-v1")) || epoch <= 0 {
		t.Fatalf("descriptor = %v", descriptor)
	}
	ws := hn.websocket(t, descriptor)
	if ws.Subprotocol() != gateway.CanonicalGatewayProtocol {
		t.Fatalf("subprotocol = %q", ws.Subprotocol())
	}
	created := sessionResult(t, ws, "session.create", map[string]any{"request_id": "native-create", "source": "cli", "cwd": h, "toolsets": []any{}})
	sid, _ := created["session_id"].(string)
	initial := sessionResult(t, ws, "session.resume", map[string]any{"session_id": sid})
	if admitted := sessionResult(t, ws, "prompt.submit", map[string]any{"session_id": sid, "input_id": "native-turn", "text": "NATIVE_TURN"}); admitted["status"] != "queued" {
		t.Fatalf("prompt.submit = %v", admitted)
	}
	if texts := messageCompletes(t, ws, 1, 60*time.Second); !strings.Contains(texts[0], "NATIVE_ACK_NATIVE_TURN") {
		t.Fatalf("message.complete = %v", texts)
	}
	if status := hn.admissions(t)["native-turn"]; status != "terminal" {
		t.Fatalf("native-turn = %s", status)
	}
	replay := sessionResult(t, ws, "session.events.since", map[string]any{"session_id": sid,
		"replay_epoch": initial["replay_epoch"], "last_sequence": initial["last_sequence"]})
	completes := 0
	events, _ := replay["events"].([]any)
	for _, raw := range events {
		if event, isDict := raw.(map[string]any); isDict && event["type"] == "message.complete" {
			completes++
		}
	}
	if completes != 1 {
		t.Fatalf("replay = %v", replay)
	}
	ws.Close()

	first, second := hn.websocket(t, descriptor), hn.websocket(t, descriptor)
	sessionResult(t, second, "session.resume", map[string]any{"session_id": sid})
	sessionResult(t, first, "session.resume", map[string]any{"session_id": sid})
	if head := sessionResult(t, first, "prompt.submit", map[string]any{"session_id": sid, "input_id": "fifo-head", "text": "BLOCK_FIFO_HEAD"}); head["status"] != "queued" {
		t.Fatalf("head = %v", head)
	}
	if !peer.waitBlocked(30 * time.Second) {
		t.Fatal("the head never reached the model")
	}
	if tailAdmission := sessionResult(t, second, "prompt.submit", map[string]any{"session_id": sid, "input_id": "fifo-tail", "text": "FIFO_TAIL"}); tailAdmission["status"] != "queued" {
		t.Fatalf("tail = %v", tailAdmission)
	}
	if rows := hn.admissions(t); rows["fifo-head"] != "started" || rows["fifo-tail"] != "queued" {
		t.Fatalf("admissions = %v", rows)
	}
	peer.releaseAll()
	if completed := messageCompletes(t, second, 2, 60*time.Second); !reflect.DeepEqual(completed, []string{"NATIVE_ACK_BLOCK_FIFO_HEAD", "NATIVE_ACK_FIFO_TAIL"}) {
		t.Fatalf("completed = %v", completed)
	}
	first.Close()
	second.Close()
	texts := peer.texts()
	if slices.Index(texts, "BLOCK_FIFO_HEAD") > slices.Index(texts, "FIFO_TAIL") || slices.Index(texts, "FIFO_TAIL") < 0 {
		t.Fatalf("texts = %v", texts)
	}
	peer.clearEvents()

	ws = hn.websocket(t, descriptor)
	before := sessionResult(t, ws, "session.resume", map[string]any{"session_id": sid})
	branch := map[string]any{"session_id": sid, "request_id": "native-branch", "operation": "branch", "payload": map[string]any{},
		"expected_revision": before["revision"], "expected_generation": before["execution_generation"]}
	branched := sessionResult(t, ws, "session.mutate", branch)
	child, _ := branched["branched_session_id"].(string)
	if child == sid || child == "" {
		t.Fatalf("branched = %v", branched)
	}
	if retried := sessionResult(t, ws, "session.mutate", branch); !reflect.DeepEqual(retried, branched) {
		t.Fatalf("branch retry must replay its receipt: %v, want %v", retried, branched)
	}
	staleBranch := map[string]any{}
	for k, v := range branch {
		staleBranch[k] = v
	}
	staleBranch["request_id"] = "stale-branch"
	sessionRefusal(t, ws, "session.mutate", "revision_conflict", staleBranch)
	childSnapshot := sessionResult(t, ws, "session.resume", map[string]any{"session_id": child})
	childMessages, _ := childSnapshot["messages"].([]any)
	beforeMessages, _ := before["messages"].([]any)
	if len(childMessages) != len(beforeMessages) || len(beforeMessages) == 0 {
		t.Fatalf("child messages %d, parent %d", len(childMessages), len(beforeMessages))
	}
	deletion := map[string]any{"session_id": child, "request_id": "native-delete", "operation": "delete", "payload": map[string]any{},
		"expected_revision": childSnapshot["revision"], "expected_generation": childSnapshot["execution_generation"]}
	deleted := sessionResult(t, ws, "session.mutate", deletion)
	if ids, _ := deleted["deleted_ids"].([]any); !slices.Contains(ids, any(child)) {
		t.Fatalf("deleted = %v", deleted)
	}
	if retried := sessionResult(t, ws, "session.mutate", deletion); !reflect.DeepEqual(retried, deleted) {
		t.Fatalf("delete retry must replay its receipt: %v, want %v", retried, deleted)
	}
	if gone := sessionRPC(t, ws, "session.resume", map[string]any{"session_id": child}); gone["error"] == nil {
		t.Fatalf("resume of the deleted child = %v", gone)
	}
	if rows := hn.query(t, "SELECT 1 FROM sessions WHERE id=?", child); len(rows) != 0 {
		t.Fatalf("child rows = %v", rows)
	}
	if rows := hn.query(t, "SELECT 1 FROM sessions WHERE id=?", sid); len(rows) != 1 {
		t.Fatalf("parent rows = %v", rows)
	}
	ws.Close()
	receipt["branch_deleted"] = child

	if _, release, err := gateway.ExclusiveMaintenance(context.Background(), []string{h}, ""); err == nil {
		release()
		t.Fatal("maintenance reserved the home a live owner holds")
	} else {
		var conflict *gateway.OwnershipConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("maintenance = %v", err)
		}
	}
	secondLog := filepath.Join(filepath.Dir(h), "second.log")
	secondOwner := startGatewayChild(t, secondLog, hn.env, gateway.GatewayRunModuleEntry)
	var code int
	select {
	case err := <-secondOwner.exited:
		secondOwner.stopped = true
		code = exitCode(err)
	case <-time.After(120 * time.Second):
		t.Fatal("second gateway did not refuse while the first owns gateway.lock")
	}
	text := secondOwner.output()
	rawErrors, err := os.ReadFile(filepath.Join(h, "logs", "errors.log"))
	if err != nil {
		t.Fatal(err)
	}
	both := text + string(rawErrors)
	attached := code == 0 && strings.Contains(text, "already serves")
	refused := code != 0 && (strings.Contains(both, "already running") || strings.Contains(both, "Cannot reserve gateway profiles") ||
		strings.Contains(both, "Another gateway already owns this host"))
	if !attached && !refused {
		t.Fatalf("second owner exit %d\n%s\n%s", code, text, tail(string(rawErrors), 2000))
	}
	select {
	case err := <-owner.exited:
		owner.exited <- err
		t.Fatalf("the first owner exited: %v", err)
	default:
	}
	if identified, err := gatewayControl(h, "identify", nil); err != nil || identified["instance_id"] != descriptor["instance_id"] {
		t.Fatalf("identify after the second owner = %v, %v", identified, err)
	}
	receipt["second_owner_exit"] = code
	parsed, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	port := parsed.Port()
	if err := hn.plannedStop(t, owner, descriptor); err != nil {
		firstLog, _ := os.ReadFile(filepath.Join(filepath.Dir(h), "first.log"))
		t.Fatalf("planned stop exit = %v\n%s", err, firstLog)
	}
	for _, name := range []string{"gateway.pid", "gateway.sock"} {
		if _, err := os.Stat(filepath.Join(h, name)); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", name, err)
		}
	}
	if _, err := gatewayControl(h, "identify", nil); err == nil {
		t.Fatal("the control channel still answers")
	}
	if portOpen(port) {
		t.Fatalf("port %s still accepts", port)
	}
	_, release, err := gateway.ExclusiveMaintenance(context.Background(), []string{h}, "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	receipt["lock_released"] = true
	raw, _ := json.Marshal(receipt)
	t.Log(string(raw))
}

// Ends the owner without cleanup through the start time guarded force kill and waits for it
func hardKill(t *testing.T, owner *gatewayChild) {
	t.Helper()
	pid := owner.process.Process.Pid
	if err := gateway.TerminatePid(pid, true, gateway.GetProcessStartTime(pid)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-owner.exited:
		owner.stopped = true
	case <-time.After(30 * time.Second):
		t.Fatal("the killed owner did not exit")
	}
}

func TestNativeOwnerHardKillRecoversQueuedAdmissionOnce(t *testing.T) {
	hn := newNativeHarness(t)
	h, peer := hn.home, hn.peer
	var pids []int
	var epochs []float64
	sessions := map[string]string{}
	owner, descriptor := hn.daemon(t, "first.log", apiRecoveryDaemonMode)
	pids, epochs = append(pids, owner.process.Process.Pid), append(epochs, descriptor["authority_epoch"].(float64))
	ws := hn.websocket(t, descriptor)
	for _, name := range []string{"blocked", "safe"} {
		created := sessionResult(t, ws, "session.create", map[string]any{"request_id": name, "source": "cli", "cwd": h, "toolsets": []any{}})
		sessions[name], _ = created["session_id"].(string)
	}
	if warm := sessionResult(t, ws, "prompt.submit", map[string]any{"session_id": sessions["safe"], "input_id": "warm", "text": "WARM"}); warm["status"] != "queued" {
		t.Fatalf("warm = %v", warm)
	}
	hn.waitStatus(t, "warm", "terminal", 60*time.Second)
	sessionResult(t, ws, "prompt.submit", map[string]any{"session_id": sessions["blocked"], "input_id": "started", "text": "BLOCK_STARTED"})
	if !peer.waitBlocked(30 * time.Second) {
		t.Fatal("BLOCK_STARTED never reached the model")
	}
	sessionResult(t, ws, "prompt.submit", map[string]any{"session_id": sessions["blocked"], "input_id": "follower", "text": "NEVER_REPLAY"})
	if queued := sessionResult(t, ws, "prompt.submit", map[string]any{"session_id": sessions["safe"], "input_id": "queued", "text": "SAFE_QUEUE"}); queued["status"] != "queued" {
		t.Fatalf("queued = %v", queued)
	}
	hn.waitStatus(t, "queued", "queued", 20*time.Second)
	ws.Close()
	hardKill(t, owner)
	peer.releaseAll()
	for _, name := range []string{"restart-1.log", "restart-2.log"} {
		owner, descriptor := hn.daemon(t, name, stackDumpDaemonMode, "UGW_STACK_DUMP_AFTER=20")
		pids, epochs = append(pids, owner.process.Process.Pid), append(epochs, descriptor["authority_epoch"].(float64))
		hn.waitStatus(t, "queued", "terminal", 60*time.Second)
		if rows := hn.admissions(t); rows["started"] != "unknown" || rows["follower"] != "queued" {
			t.Fatalf("admissions after %s = %v", name, rows)
		}
		ws := hn.websocket(t, descriptor)
		resumed := sessionResult(t, ws, "session.resume", map[string]any{"session_id": sessions["safe"]})
		history, _ := json.Marshal(resumed["messages"])
		if !strings.Contains(string(history), "NATIVE_ACK_WARM") || !strings.Contains(string(history), "NATIVE_ACK_SAFE_QUEUE") {
			t.Fatalf("safe history = %s", history)
		}
		paused := sessionResult(t, ws, "session.resume", map[string]any{"session_id": sessions["blocked"]})
		pending, _ := paused["pending"].([]any)
		if !slices.ContainsFunc(pending, func(raw any) bool {
			row, isDict := raw.(map[string]any)
			return isDict && row["status"] == "unknown"
		}) {
			t.Fatalf("blocked pending = %v", paused["pending"])
		}
		ws.Close()
		if err := hn.plannedStop(t, owner, descriptor); err != nil {
			t.Fatalf("planned stop of %s = %v", name, err)
		}
	}
	texts := peer.texts()
	count := func(text string) int {
		n := 0
		for _, got := range texts {
			if got == text {
				n++
			}
		}
		return n
	}
	if count("SAFE_QUEUE") != 1 || count("BLOCK_STARTED") != 1 || count("NEVER_REPLAY") != 0 {
		t.Fatalf("inference texts = %v", texts)
	}
	distinct := map[int]bool{}
	for _, pid := range pids {
		distinct[pid] = true
	}
	if !slices.IsSorted(epochs) || len(slices.Compact(slices.Clone(epochs))) != len(epochs) || len(distinct) != 3 {
		t.Fatalf("pids %v epochs %v", pids, epochs)
	}
	raw, _ := json.Marshal(map[string]any{"pids": pids, "epochs": epochs, "inference_texts": texts, "rows": hn.admissions(t)})
	t.Log(string(raw))
}

// Tails of every log of the home, what a failed worker case reports beside its exit
func homeLogTails(h string) string {
	var out []string
	names, _ := filepath.Glob(filepath.Join(h, "logs", "*.log"))
	for _, name := range names {
		raw, _ := os.ReadFile(name)
		out = append(out, "== "+filepath.Base(name)+"\n"+tail(string(raw), 4000))
	}
	return strings.Join(out, "\n")
}

func TestNativeSafeModeCLIExecutesInManagedWorker(t *testing.T) {
	hn := newNativeHarness(t)
	h, peer := hn.home, hn.peer
	owner, descriptor := hn.daemon(t, "worker.log", stackDumpDaemonMode, "UGW_STACK_DUMP_AFTER=20")
	gatewayPID := int(descriptor["pid"].(float64))
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	cli := exec.CommandContext(ctx, os.Args[0], "chat", "--safe-mode", "--provider", "custom", "--base-url", hn.url,
		"--model", "safe-fixture", "--api-key", "fixture", "-Q", "-q", "SAFE_PROBE_NATIVE")
	cli.Dir = hn.work
	cli.Env = append(append([]string{}, hn.env...), runMainEnv+"=1")
	var stdout, stderr strings.Builder
	cli.Stdout, cli.Stderr = &stdout, &stderr
	runErr := cli.Run()
	out, errText := stdout.String(), stderr.String()
	records, err := nativeAuditRecords(hn.audit)
	if err != nil {
		t.Fatal(err)
	}
	isWorker := func(r nativeAuditEntry) bool {
		return r.Kind == "start" && len(r.Argv) > 0 && r.Argv[len(r.Argv)-1] == gateway.ManagedWorkerEntry
	}
	if runErr != nil {
		var starts []nativeAuditEntry
		for _, r := range records {
			if isWorker(r) {
				starts = append(starts, r)
			}
		}
		t.Fatalf("safe-mode CLI = %v %v\n%s\n%s\nowner %d gateway %d worker starts %+v\nworker executions %v\n%s", runErr, ctx.Err(),
			out, errText, owner.process.Process.Pid, gatewayPID, starts, hn.query(t, "SELECT execution_id, status FROM worker_executions"), homeLogTails(h))
	}
	if !strings.Contains(out+errText, "NATIVE_ACK_SAFE_PROBE_NATIVE") {
		t.Fatalf("CLI output lacks the reply\n%s\n%s", out, errText)
	}
	match := regexp.MustCompile(`Session: (\S+)`).FindStringSubmatch(errText)
	if match == nil {
		t.Fatalf("no session line on stderr\n%s", errText)
	}
	sid := match[1]
	if rows := hn.query(t, "SELECT status FROM session_admissions WHERE target_session_id=?", sid); len(rows) != 1 || fmt.Sprint(rows[0][0]) != "terminal" {
		t.Fatalf("admissions = %v", rows)
	}
	if rows := hn.query(t, "SELECT status FROM worker_executions WHERE session_id=?", sid); len(rows) != 1 || fmt.Sprint(rows[0][0]) != "terminal" {
		t.Fatalf("worker executions = %v", rows)
	}
	rows := hn.query(t, "SELECT value FROM state_meta WHERE key=?", "gateway.local_policy.v1:"+sid)
	if len(rows) == 0 {
		t.Fatal("no local policy row")
	}
	var policy struct {
		Policy map[string]any `json:"policy"`
	}
	if err := json.Unmarshal([]byte(fmt.Sprint(rows[0][0])), &policy); err != nil || policy.Policy["safe_mode"] != true || policy.Policy["ignore_user_config"] != true {
		t.Fatalf("policy = %v, %v", rows[0][0], err)
	}
	workers := map[int]bool{}
	for _, r := range records {
		if !isWorker(r) {
			continue
		}
		descends := r.PPID == gatewayPID
		for _, raw := range r.Ancestors {
			if pair, isList := raw.([]any); isList && len(pair) > 0 && pair[0] == float64(gatewayPID) {
				descends = true
			}
		}
		if descends {
			workers[r.PID] = true
		}
	}
	if len(workers) != 1 || workers[gatewayPID] {
		t.Fatalf("workers %v of gateway %d in %+v", workers, gatewayPID, records)
	}
	var worker int
	for pid := range workers {
		worker = pid
	}
	if !slices.ContainsFunc(records, func(r nativeAuditEntry) bool {
		return r.Kind == "sqlite" && r.PID == gatewayPID && strings.Contains(r.Target, "state.db")
	}) {
		t.Fatalf("the audit never saw the gateway open state.db: %+v", records)
	}
	var writable []string
	for _, r := range records {
		if r.Kind == "sqlite" && r.PID == worker && strings.Contains(r.Target, "state.db") && !strings.Contains(r.Target, "mode=ro") {
			writable = append(writable, r.Target)
		}
	}
	if len(writable) != 0 {
		t.Fatalf("the worker opened canonical sqlite writable: %v", writable)
	}
	peer.mu.Lock()
	var safe []nativeModelRequest
	for _, r := range peer.requests {
		if r.Text == "SAFE_PROBE_NATIVE" && !r.TitleGeneration {
			safe = append(safe, r)
		}
	}
	all := append([]nativeModelRequest{}, peer.requests...)
	peer.mu.Unlock()
	if len(safe) != 1 || safe[0].Model != "safe-fixture" || safe[0].Auth != "Bearer fixture" {
		t.Fatalf("model requests = %+v", all)
	}
	if err := hn.plannedStop(t, owner, descriptor); err != nil {
		t.Fatalf("planned stop = %v", err)
	}
	raw, _ := json.Marshal(map[string]any{"worker_pid": worker, "gateway_pid": gatewayPID, "session": sid})
	t.Log(string(raw))
}
