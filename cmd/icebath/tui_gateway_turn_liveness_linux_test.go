//go:build linux

package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	fakellm "github.com/nickheyer/icebath/pkg/testfakes"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Liveness budget of one chaos scenario, the fakes faulting for an hour against deadlines of seconds
const (
	chaosStaleTimeoutS                 = 3
	chaosRequestTimeoutS               = 5
	chaosAPIMaxRetries                 = 2
	chaosMaxTurns                      = 8
	chaosToolTimeoutS                  = 3
	chaosTurnDeadline                  = 90 * time.Second
	chaosMaxProviderCallsPerFailedTurn = 16
	chaosInterruptDeadline             = 15 * time.Second
	chaosLongTimeoutS                  = 600
	chaosProcessReap                   = 15 * time.Second
	chaosPlainModel                    = "fake-model"
	chaosReasoningModel                = "deepseek-r1"
)

// Bounds of the stdio gateway scenarios, the heartbeat the dispatcher must keep answering during a wedged turn
const (
	chaosHeartbeatMax      = 3 * time.Second
	chaosHeartbeatInterval = 150 * time.Millisecond
	chaosMinHeartbeats     = 5
	chaosReadyTimeout      = 60 * time.Second
	chaosSettleTimeout     = 30 * time.Second
	chaosExitTimeout       = 30 * time.Second
	chaosParallelCalls     = 8
	chaosHungCommand       = "sleep 3600"
	chaosWarmupDeadline    = 300 * time.Second
)

var (
	chaosMark  = regexp.MustCompile(`\[\[chaos:(?P<fault>[a-z0-9_]+):(?P<nonce>[0-9a-f]+)\]\]`)
	chaosProbe = regexp.MustCompile(`\[\[probe:(?P<nonce>[0-9a-f]+)\]\]`)
)

// Random lowercase hex of n characters
func chaosHex(n int) string {
	raw := make([]byte, (n+1)/2)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)[:n]
}

// Tag every descendant of one scenario inherits so the orphan scan finds it after reparenting
func newChaosTag() string { return "chaos" + chaosHex(12) }

// Settings of a chaos home, every liveness knob short and explicit
type chaosSettings struct {
	model          string
	staleTimeout   float64
	requestTimeout float64
}

// config.yaml text of a chaos home
func chaosConfig(baseURL string, s chaosSettings) string {
	number := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	return "model:\n" +
		"  provider: custom\n" +
		"  base_url: " + baseURL + "\n" +
		"  default: " + s.model + "\n" +
		"  context_length: 128000\n" +
		"agent:\n" +
		"  api_max_retries: " + strconv.Itoa(chaosAPIMaxRetries) + "\n" +
		"  auto_recovery_cycles: 0\n" +
		"  max_turns: " + strconv.Itoa(chaosMaxTurns) + "\n" +
		"providers:\n" +
		"  custom:\n" +
		"    request_timeout_seconds: " + number(s.requestTimeout) + "\n" +
		"    stale_timeout_seconds: " + number(s.staleTimeout) + "\n" +
		"compression:\n" +
		"  enabled: false\n" +
		"memory:\n" +
		"  memory_enabled: false\n" +
		"  user_profile_enabled: false\n" +
		"updates:\n" +
		"  check: false\n"
}

// Fake HOME under root and the state home inside it, routed to the fake provider
func writeChaosHome(t *testing.T, root, baseURL string, s chaosSettings) (string, string) {
	t.Helper()
	userHome := filepath.Join(root, "home")
	stateHome := filepath.Join(userHome, ".icebath")
	if err := os.MkdirAll(stateHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateHome, "config.yaml"), []byte(chaosConfig(baseURL, s)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateHome, ".env"), []byte("OPENAI_API_KEY=sk-fake-chaos\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return userHome, stateHome
}

// Child environment without real credentials, the homes faked and the scenario tag set
func hermeticChaosEnv(userHome, stateHome, tag string) map[string]string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasSuffix(key, "_API_KEY") || strings.HasSuffix(key, "_TOKEN") || strings.HasSuffix(key, "_SECRET") ||
			strings.HasPrefix(key, "ICEBATH_") || strings.HasPrefix(key, "OPENROUTER") || strings.HasPrefix(key, "ANTHROPIC") ||
			strings.HasPrefix(key, "OPENAI") || strings.HasPrefix(key, "NOUS_") {
			continue
		}
		env[key] = value
	}
	for key, value := range map[string]string{"HOME": userHome, "ICEBATH_HOME": stateHome, "OPENAI_API_KEY": "sk-fake-chaos",
		"CHAOS_TAG": tag, "ICEBATH_STATE_DB_GUARD_BYPASS": "1", "TZ": "UTC", "NO_COLOR": "1"} {
		env[key] = value
	}
	return env
}

// Working directory of the gateway child under the scenario root
func chaosGatewayCwd(t *testing.T, root string) string {
	t.Helper()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	return work
}

// Gateway child environment, local terminal in yolo mode with only the system directories on PATH
func envForChaosGateway(t *testing.T, base map[string]string, work string) []string {
	t.Helper()
	env := map[string]string{}
	for key, value := range base {
		env[key] = value
	}
	env["TERMINAL_ENV"] = "local"
	env["ICEBATH_YOLO_MODE"] = "1"
	env["ICEBATH_STATE_DB_GUARD_BYPASS"] = "1"
	env["PWD"] = work
	tmp := filepath.Join(filepath.Dir(work), "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	env["TMPDIR"] = tmp
	env["TERMINAL_CWD"] = work
	env["PATH"] = strings.Join([]string{"/usr/local/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator))
	for _, key := range []string{"DISPLAY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK"} {
		delete(env, key)
	}
	env[runMainEnv] = "1"
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	slices.Sort(out)
	return out
}

// Pids whose environment carries the scenario tag, zombies and the excluded pids left out
func taggedPIDs(tag string, exclude ...int) []int {
	needle := []byte("CHAOS_TAG=" + tag)
	skip := append([]int{os.Getpid()}, exclude...)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var found []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || slices.Contains(skip, pid) {
			continue
		}
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		if cut := bytes.LastIndexByte(stat, ')'); cut >= 0 {
			if fields := bytes.Fields(stat[cut+1:]); len(fields) > 0 && string(fields[0]) == "Z" {
				continue
			}
		}
		if slices.ContainsFunc(bytes.Split(environ, []byte{0}), func(item []byte) bool { return bytes.Equal(item, needle) }) {
			found = append(found, pid)
		}
	}
	return found
}

// Command lines of the given pids for a failure message
func describePIDs(pids []int) []string {
	var out []string
	for _, pid := range pids {
		cmd := "?"
		if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
			cmd = strings.ToValidUTF8(string(bytes.ReplaceAll(raw, []byte{0}, []byte{' '})), "�")
		}
		out = append(out, fmt.Sprintf("%d: %s", pid, cmd[:min(len(cmd), 160)]))
	}
	return out
}

// Polls until no tagged process remains and returns the survivors at the deadline
func waitNoTagged(tag string, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	for {
		left := taggedPIDs(tag)
		if len(left) == 0 || !time.Now().Before(deadline) {
			return left
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Kills every process still carrying the scenario tag
func killTagged(tag string) {
	for _, pid := range taggedPIDs(tag) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// Tool call ids of an assistant message, marker strings standing in for unparseable or id less calls
func chaosCallIDs(msg map[string]any) []string {
	raw := msg["tool_calls"]
	if text, isText := raw.(string); isText {
		if text == "" {
			return nil
		}
		var decoded any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return []string{"<unparseable tool_calls>"}
		}
		raw = decoded
	}
	calls, _ := raw.([]any)
	var ids []string
	for _, item := range calls {
		call, isDict := item.(map[string]any)
		if !isDict {
			continue
		}
		id, _ := call["id"].(string)
		if id == "" {
			id, _ = call["call_id"].(string)
		}
		if id == "" {
			id = "<missing id>"
		}
		ids = append(ids, id)
	}
	return ids
}

// Tool call ids with no tool message in the block right after their assistant message
func unansweredToolCalls(messages []map[string]any) []string {
	missing := []string{}
	for i, msg := range messages {
		if msg["role"] != "assistant" {
			continue
		}
		ids := chaosCallIDs(msg)
		if len(ids) == 0 {
			continue
		}
		answered := map[string]bool{}
		for _, follow := range messages[i+1:] {
			if follow["role"] != "tool" {
				break
			}
			id, _ := follow["tool_call_id"].(string)
			answered[id] = true
		}
		for _, id := range ids {
			if !answered[id] {
				missing = append(missing, id)
			}
		}
	}
	return missing
}

// Tool result text by tool call id
func toolResultsByID(messages []map[string]any) map[string]string {
	out := map[string]string{}
	for _, msg := range messages {
		if msg["role"] != "tool" {
			continue
		}
		content, isText := msg["content"].(string)
		if !isText {
			raw, _ := json.Marshal(msg["content"])
			content = string(raw)
		}
		id, _ := msg["tool_call_id"].(string)
		out[id] = content
	}
	return out
}

// Active message rows of a session in a real state.db, oldest first and OpenAI shaped
func persistedMessages(t *testing.T, stateDB, sessionID string) []map[string]any {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+stateDB+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if sessionID == "" {
		err := db.QueryRow("SELECT session_id FROM messages ORDER BY id DESC LIMIT 1").Scan(&sessionID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Query("SELECT role, content, tool_call_id, tool_calls FROM messages WHERE session_id = ? AND active = 1 ORDER BY id", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var role string
		var content, toolCallID, toolCalls sql.NullString
		if err := rows.Scan(&role, &content, &toolCallID, &toolCalls); err != nil {
			t.Fatal(err)
		}
		msg := map[string]any{"role": role, "content": nil}
		if content.Valid {
			msg["content"] = content.String
		}
		if toolCallID.String != "" {
			msg["tool_call_id"] = toolCallID.String
		}
		if toolCalls.String != "" {
			msg["tool_calls"] = toolCalls.String
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Integrity check verdict of a state.db
func integrityOK(t *testing.T, stateDB string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+stateDB+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var verdict string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&verdict); err != nil {
		t.Fatal(err)
	}
	return verdict
}

// Plain text of message content, list parts joined by spaces
func chaosTextOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, part := range c {
			if dict, isDict := part.(map[string]any); isDict {
				text := dict["text"]
				if text == nil {
					text = ""
				}
				parts = append(parts, fmt.Sprint(text))
			} else {
				parts = append(parts, fmt.Sprint(part))
			}
		}
		return strings.Join(parts, " ")
	case nil:
		return ""
	}
	return fmt.Sprint(content)
}

// Newest user message carrying a scenario or probe marker, so a fault applies to the whole turn
func decidingUserText(messages []any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, _ := messages[i].(map[string]any)
		if msg["role"] != "user" {
			continue
		}
		text := chaosTextOf(msg["content"])
		if chaosProbe.MatchString(text) || chaosMark.MatchString(text) {
			return text
		}
	}
	return ""
}

// Token one parallel call echoes
func chaosToken(nonce string, i int) string { return fmt.Sprintf("tok%sn%d", nonce, i) }

// Responder answering a tool result with text and anything else with the first response
func toolsThenText(first func(nonce string) any) func(messages []any, nonce string) any {
	return func(messages []any, nonce string) any {
		if len(messages) > 0 {
			if last, _ := messages[len(messages)-1].(map[string]any); last["role"] == "tool" {
				return fakellm.Text{Text: "tools settled " + nonce}
			}
		}
		return first(nonce)
	}
}

// One assistant turn of eight parallel terminal calls, each echoing its own token
func parallelBatch(nonce string) any {
	var calls []fakellm.Call
	for i := range chaosParallelCalls {
		calls = append(calls, fakellm.Call{Name: "terminal", Args: map[string]any{"command": "echo " + chaosToken(nonce, i)}})
	}
	return fakellm.ToolCall{Name: calls[0].Name, Args: calls[0].Args, Parallel: calls[1:]}
}

// Fault each scenario marker selects, every one lasting an hour
var chaosFaults = map[string]func(messages []any, nonce string) any{
	"hang": func([]any, string) any { return fakellm.Hang{Seconds: 3600} },
	"stall": func([]any, string) any {
		return fakellm.StallMidStream{Text: "partial answer that stalls", AfterChars: 8, Seconds: 3600}
	},
	"err500": func([]any, string) any { return fakellm.Error{Status: 500, Message: "scripted failure"} },
	"err429": func([]any, string) any { return fakellm.Error{Status: 429, Message: "scripted failure"} },
	"drop": func([]any, string) any {
		return fakellm.DropMidStream{Text: "partial answer that never finishes", AfterChars: 12}
	},
	"parallel": toolsThenText(parallelBatch),
	"toolhang": toolsThenText(func(string) any {
		return fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": chaosHungCommand, "timeout": chaosToolTimeoutS}}
	}),
	"toolhang_long": toolsThenText(func(string) any {
		return fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": chaosHungCommand, "timeout": chaosLongTimeoutS}}
	}),
}

// Response of one provider request, keyed by the marker of its deciding user message
func chaosResponder(record *fakellm.Request) any {
	messages, _ := record.Body["messages"].([]any)
	deciding := decidingUserText(messages)
	if probe := chaosProbe.FindStringSubmatch(deciding); probe != nil {
		return fakellm.Text{Text: "alive " + probe[chaosProbe.SubexpIndex("nonce")]}
	}
	if mark := chaosMark.FindStringSubmatch(deciding); mark != nil {
		return chaosFaults[mark[chaosMark.SubexpIndex("fault")]](messages, mark[chaosMark.SubexpIndex("nonce")])
	}
	return fakellm.Text{Text: "unscripted"}
}

// Main request bodies whose deciding user text carries the needle
func mainRequestsFor(srv *fakellm.Server, needle string) []map[string]any {
	var out []map[string]any
	for _, record := range srv.Requests() {
		messages, _ := record.Body["messages"].([]any)
		if record.Kind == "main" && strings.Contains(decidingUserText(messages), needle) {
			out = append(out, record.Body)
		}
	}
	return out
}

// One chaos scenario, the fault its prompt selects and what is done once it is live
type chaosScenario struct {
	id                 string
	fault              string
	model              string
	longTimeouts       bool
	longRequestTimeout bool
	waitFor            string
	action             string
	reapsHungTool      bool
}

var chaosScenarios = []chaosScenario{
	{id: "provider_hang", fault: "hang"},
	{id: "provider_stall_reasoning_model", fault: "stall", model: chaosReasoningModel, longRequestTimeout: true},
	{id: "provider_500_forever", fault: "err500"},
	{id: "provider_429_forever", fault: "err429"},
	{id: "provider_drop_mid_stream", fault: "drop"},
	{id: "parallel_8_terminal_calls", fault: "parallel"},
	{id: "tool_hang_past_timeout", fault: "toolhang", reapsHungTool: true},
	{id: "interrupt_during_provider_hang", fault: "hang", longTimeouts: true, waitFor: "provider", action: "interrupt"},
	{id: "interrupt_during_hung_tool", fault: "toolhang_long", longTimeouts: true, waitFor: "tool", action: "interrupt", reapsHungTool: true},
	{id: "stdin_eof_during_provider_hang", fault: "hang", longTimeouts: true, waitFor: "provider", action: "eof"},
	{id: "stdin_eof_during_hung_tool", fault: "toolhang_long", longTimeouts: true, waitFor: "tool", action: "eof"},
	{id: "sigterm_during_hung_tool", fault: "toolhang_long", longTimeouts: true, waitFor: "tool", action: "sigterm"},
	{id: "sigterm_during_provider_hang", fault: "hang", longTimeouts: true, waitFor: "provider", action: "sigterm"},
}

// Whether a scenario's action ends the gateway mid turn
func chaosExitAction(action string) bool { return action == "eof" || action == "sigterm" }

// Pushed event of a kind for a session
func chaosIs(event map[string]any, kind, sid string) bool {
	return event["type"] == kind && event["session_id"] == sid
}

// Tagged pids running the hung tool command
func hungToolPIDs(tag string) []int {
	var out []int
	for _, pid := range taggedPIDs(tag) {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		if strings.Contains(string(bytes.ReplaceAll(raw, []byte{0}, []byte{' '})), chaosHungCommand) {
			out = append(out, pid)
		}
	}
	return out
}

// Polls a predicate every 50 ms until it holds or the timeout passes, its last answer the result
func chaosWaitUntil(pred func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return pred()
}

// Checks that every one of the eight parallel calls got the result carrying its own token
func assertParallelResults(t *testing.T, messages []map[string]any, nonce, where string) {
	t.Helper()
	results := toolResultsByID(messages)
	seen := 0
	for _, msg := range messages {
		if msg["role"] != "assistant" || msg["tool_calls"] == nil {
			continue
		}
		raw := msg["tool_calls"]
		if text, isText := raw.(string); isText {
			var decoded any
			if err := json.Unmarshal([]byte(text), &decoded); err != nil {
				t.Fatalf("%s: tool_calls %q: %v", where, text, err)
			}
			raw = decoded
		}
		calls, _ := raw.([]any)
		if len(calls) == 0 {
			continue
		}
		for _, item := range calls {
			call := item.(map[string]any)
			function := call["function"].(map[string]any)
			var args map[string]any
			if err := json.Unmarshal([]byte(function["arguments"].(string)), &args); err != nil {
				t.Fatalf("%s: arguments %v: %v", where, function["arguments"], err)
			}
			words := strings.Fields(args["command"].(string))
			token := words[len(words)-1]
			if !strings.HasPrefix(token, "tok"+nonce) {
				t.Fatalf("%s: %v", where, args)
			}
			id, _ := call["id"].(string)
			body := results[id]
			if !strings.Contains(body, token) || strings.Contains(body, "Result unavailable") {
				t.Logf("%s: tool_call %s full result %s", where, id, body)
				t.Fatalf("%s: tool_call %s (%s) got %q", where, id, token, body[:min(len(body), 200)])
			}
			seen++
		}
	}
	if seen != chaosParallelCalls {
		t.Fatalf("%s: %d tool calls, expected %d", where, seen, chaosParallelCalls)
	}
}

// A running stdio gateway child of one chaos scenario with its frame stream
type chaosGateway struct {
	process *exec.Cmd
	stdin   io.WriteCloser
	exited  chan struct{}
	waitErr error
	stderr  string
	stream  *rpcStream
}

// Tail of the gateway stderr for a failure message
func (g *chaosGateway) tail() string {
	raw, _ := os.ReadFile(g.stderr)
	if len(raw) > 4000 {
		raw = raw[len(raw)-4000:]
	}
	if len(raw) == 0 {
		return ""
	}
	return "\n--- gateway stderr tail ---\n" + string(raw)
}

// Boots the binary as the stdio gateway entry under the scenario environment
func startChaosGateway(t *testing.T, env []string, cwd, stderrPath string) *chaosGateway {
	t.Helper()
	g := &chaosGateway{process: exec.Command(os.Args[0], tuigateway.TUIGatewayEntry), exited: make(chan struct{}), stderr: stderrPath}
	g.process.Env = env
	g.process.Dir = cwd
	errOut, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	g.process.Stderr = errOut
	if g.stdin, err = g.process.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := g.process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := g.process.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	drained := make(chan struct{})
	read := func() (map[string]any, error) {
		for lines.Scan() {
			var frame map[string]any
			if json.Unmarshal(lines.Bytes(), &frame) == nil {
				return frame, nil
			}
		}
		close(drained)
		if err := lines.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	go func() {
		<-drained
		g.waitErr = g.process.Wait()
		errOut.Close()
		close(g.exited)
	}()
	var writeMu sync.Mutex
	send := func(frame map[string]any) error {
		raw, err := json.Marshal(frame)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err = g.stdin.Write(append(raw, '\n'))
		return err
	}
	g.stream = newRPCStream(read, send, g.tail)
	return g
}

// Reply of one request or an error naming why none came, safe off the case goroutine
func (s *rpcStream) tryCall(method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	s.mu.Lock()
	s.ids++
	id := fmt.Sprintf("r%d", s.ids)
	s.mu.Unlock()
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		reply, done := s.replies[id]
		ended, wake := s.ended, s.wake
		delete(s.replies, id)
		s.mu.Unlock()
		if done {
			if refusal, failed := reply["error"]; failed {
				return nil, fmt.Errorf("%s -> %v", method, refusal)
			}
			return reply, nil
		}
		if ended != nil {
			return nil, fmt.Errorf("gateway exited before rpc id=%s: %v", id, ended)
		}
		select {
		case <-wake:
		case <-deadline:
			return nil, fmt.Errorf("no response to rpc id=%s within %v", id, timeout)
		}
	}
}

// Result of a request that must succeed
func (g *chaosGateway) call(t *testing.T, method string, params map[string]any, timeout time.Duration) map[string]any {
	t.Helper()
	reply, err := g.stream.tryCall(method, params, timeout)
	if err != nil {
		t.Fatalf("%v%s", err, g.tail())
	}
	result, _ := reply["result"].(map[string]any)
	return result
}

// First event at index start or later matching, with its index, nil at the deadline or when the gateway ended
func (g *chaosGateway) findEvent(start int, timeout time.Duration, match func(map[string]any) bool) (map[string]any, int) {
	deadline := time.After(timeout)
	for seen := start; ; {
		g.stream.mu.Lock()
		events, ended, wake := g.stream.events, g.stream.ended, g.stream.wake
		g.stream.mu.Unlock()
		for ; seen < len(events); seen++ {
			if match(events[seen]) {
				return events[seen], seen
			}
		}
		if ended != nil {
			return nil, -1
		}
		select {
		case <-wake:
		case <-deadline:
			return nil, -1
		}
	}
}

// Event types of a session since index start
func (g *chaosGateway) eventTypesSince(start int, sid string) []string {
	g.stream.mu.Lock()
	defer g.stream.mu.Unlock()
	seen := map[string]bool{}
	for _, event := range g.stream.events[min(start, len(g.stream.events)):] {
		if event["session_id"] == sid {
			seen[fmt.Sprint(event["type"])] = true
		}
	}
	var out []string
	for kind := range seen {
		out = append(out, kind)
	}
	slices.Sort(out)
	return out
}

// Waits for the gateway to exit, false at the deadline
func (g *chaosGateway) waitExit(timeout time.Duration) bool {
	select {
	case <-g.exited:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Closes stdin, the goodbye of the TUI parent, and waits for the gateway to exit on its own
func (g *chaosGateway) closeStdinAndWait(timeout time.Duration) bool {
	_ = g.stdin.Close()
	return g.waitExit(timeout)
}

// Kills the gateway when it still runs
func (g *chaosGateway) kill() {
	select {
	case <-g.exited:
		return
	default:
	}
	_ = g.process.Process.Kill()
	g.waitExit(10 * time.Second)
}

// Cheap RPCs issued round robin on their own goroutine, each round trip latency recorded
type chaosHeartbeat struct {
	gw            *chaosGateway
	calls         []chaosHeartbeatCall
	mu            sync.Mutex
	latencies     []time.Duration
	failures      []string
	inflightSince time.Time
	stop          chan struct{}
	done          chan struct{}
}

type chaosHeartbeatCall struct {
	method string
	params map[string]any
}

// Starts issuing the calls every interval
func startChaosHeartbeat(gw *chaosGateway, calls []chaosHeartbeatCall, interval, perCallTimeout time.Duration) *chaosHeartbeat {
	hb := &chaosHeartbeat{gw: gw, calls: calls, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(hb.done)
		for n := 0; ; n++ {
			select {
			case <-hb.stop:
				return
			default:
			}
			call := hb.calls[n%len(hb.calls)]
			hb.mu.Lock()
			hb.inflightSince = time.Now()
			hb.mu.Unlock()
			started := time.Now()
			_, err := gw.stream.tryCall(call.method, call.params, perCallTimeout)
			hb.mu.Lock()
			hb.inflightSince = time.Time{}
			if err != nil {
				hb.failures = append(hb.failures, err.Error()[:min(len(err.Error()), 300)])
				if len(hb.failures) > 3 {
					hb.mu.Unlock()
					return
				}
			} else {
				hb.latencies = append(hb.latencies, time.Since(started))
			}
			hb.mu.Unlock()
			select {
			case <-hb.stop:
				return
			case <-time.After(interval):
			}
		}
	}()
	return hb
}

// Number of answered heartbeats so far
func (hb *chaosHeartbeat) count() int {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	return len(hb.latencies)
}

// Stops the heartbeat, a call still unanswered counting with its age so far
func (hb *chaosHeartbeat) close() {
	close(hb.stop)
	select {
	case <-hb.done:
	case <-time.After(5 * time.Second):
		hb.mu.Lock()
		if !hb.inflightSince.IsZero() {
			hb.latencies = append(hb.latencies, time.Since(hb.inflightSince))
		}
		hb.mu.Unlock()
	}
}

// Fails unless the dispatcher answered every heartbeat in time during the wedged turn
func assertChaosHeartbeat(t *testing.T, scn chaosScenario, hb *chaosHeartbeat, turn time.Duration) {
	t.Helper()
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if len(hb.failures) > 0 {
		t.Fatalf("%s: heartbeat RPC failed during the turn: %v", scn.id, hb.failures)
	}
	var worst time.Duration
	for _, latency := range hb.latencies {
		worst = max(worst, latency)
	}
	if worst >= chaosHeartbeatMax {
		t.Fatalf("%s: dispatcher blocked %v during the wedged turn", scn.id, worst)
	}
	if len(hb.latencies) < chaosMinHeartbeats && turn >= 2*time.Second {
		t.Fatalf("%s: only %d heartbeats answered in a %v turn", scn.id, len(hb.latencies), turn)
	}
}

// Waits for the settled session.info after a turn's message.complete
func waitChaosSettled(t *testing.T, gw *chaosGateway, sid string, doneIndex int, what string) {
	t.Helper()
	settled, _ := gw.findEvent(doneIndex+1, chaosSettleTimeout, func(e map[string]any) bool {
		payload, _ := e["payload"].(map[string]any)
		running, has := payload["running"]
		return chaosIs(e, "session.info", sid) && has && running == false
	})
	if settled == nil {
		t.Fatalf("%s: session never settled (running stays true)%s", what, gw.tail())
	}
}

// Submits a probe prompt to an idle session, which must start at once and be answered
func chaosAnsweredTurn(t *testing.T, gw *chaosGateway, sid, nonce, what string, deadline time.Duration) time.Duration {
	t.Helper()
	start := gw.stream.eventCount()
	began := time.Now()
	reply := gw.call(t, "prompt.submit", map[string]any{"session_id": sid, "text": "are you there [[probe:" + nonce + "]]"}, 60*time.Second)
	if reply["status"] != "streaming" {
		t.Fatalf("%s: session busy instead of starting the turn: %v", what, reply)
	}
	done, index := gw.findEvent(start, deadline, func(e map[string]any) bool { return chaosIs(e, "message.complete", sid) })
	if done == nil {
		t.Fatalf("%s: prompt never completed within %v%s", what, deadline, gw.tail())
	}
	payload, _ := done["payload"].(map[string]any)
	if payload["status"] != "complete" || !strings.Contains(chaosTextOf(payload["text"]), "alive "+nonce) {
		t.Fatalf("%s: %v", what, payload)
	}
	waitChaosSettled(t, gw, sid, index, what)
	return time.Since(began)
}

// The client leaves or the supervisor stops the gateway mid wedge, the tool leftovers checked last
func chaosExitMidTurn(t *testing.T, scn chaosScenario, gw *chaosGateway, hb *chaosHeartbeat, tag, stateDB, stored string) {
	t.Helper()
	assertChaosHeartbeat(t, scn, hb, chaosLongTimeoutS*time.Second)
	exited := false
	if scn.action == "sigterm" {
		_ = gw.process.Process.Signal(syscall.SIGTERM)
		exited = gw.waitExit(chaosExitTimeout)
	} else {
		exited = gw.closeStdinAndWait(chaosExitTimeout)
	}
	if !exited {
		t.Fatalf("%s: gateway still alive %v after %s mid-turn%s", scn.id, chaosExitTimeout, scn.action, gw.tail())
	}
	if verdict := integrityOK(t, stateDB); verdict != "ok" {
		t.Fatalf("%s: integrity_check %s", scn.id, verdict)
	}
	var leftovers []string
	if dangling := unansweredToolCalls(persistedMessages(t, stateDB, stored)); len(dangling) > 0 {
		leftovers = append(leftovers, fmt.Sprintf("state.db keeps tool_call(s) %v with no result (next resume sends them)", dangling))
	}
	if survivors := waitNoTagged(tag, chaosProcessReap); len(survivors) > 0 {
		leftovers = append(leftovers, fmt.Sprintf("orphans: %v", describePIDs(survivors)))
	}
	if len(leftovers) > 0 {
		t.Fatalf("ToolOutlivedGateway: %s after %s mid-turn: %s", scn.id, scn.action, strings.Join(leftovers, "; "))
	}
}

// Kills tagged descendants before the gateway, then the gateway and anything still tagged
func reapChaosGateway(gw *chaosGateway, tag string) {
	for _, pid := range taggedPIDs(tag, gw.process.Process.Pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	gw.kill()
	killTagged(tag)
}

// Runs one scenario against its own gateway and checks every liveness invariant
func runChaosScenario(t *testing.T, scn chaosScenario, srv *fakellm.Server, root string) {
	nonce := chaosHex(10)
	marker := "[[chaos:" + scn.fault + ":" + nonce + "]]"
	probeMarker := "[[probe:" + nonce + "]]"
	settings := chaosSettings{model: scn.model, staleTimeout: chaosStaleTimeoutS, requestTimeout: chaosRequestTimeoutS}
	if settings.model == "" {
		settings.model = chaosPlainModel
	}
	if scn.longTimeouts {
		settings.requestTimeout, settings.staleTimeout = chaosLongTimeoutS, chaosLongTimeoutS
	} else if scn.longRequestTimeout {
		settings.requestTimeout = chaosLongTimeoutS
	}
	userHome, stateHome := writeChaosHome(t, root, srv.BaseURL(), settings)
	tag := newChaosTag()
	work := chaosGatewayCwd(t, root)
	env := envForChaosGateway(t, hermeticChaosEnv(userHome, stateHome, tag), work)
	gw := startChaosGateway(t, env, work, filepath.Join(root, "gateway-stderr.log"))
	defer reapChaosGateway(gw, tag)
	if ready, _ := gw.findEvent(0, chaosReadyTimeout, func(e map[string]any) bool { return e["type"] == "gateway.ready" }); ready == nil {
		t.Fatalf("gateway never became ready%s", gw.tail())
	}
	created := gw.call(t, "session.create", map[string]any{"cols": 100}, 60*time.Second)
	sid, _ := created["session_id"].(string)
	stored, _ := created["stored_session_id"].(string)
	warm := chaosAnsweredTurn(t, gw, sid, chaosHex(10), scn.id+" warm-up", chaosWarmupDeadline)
	t.Logf("%s warm_s=%.2f", scn.id, warm.Seconds())

	start := gw.stream.eventCount()
	hb := startChaosHeartbeat(gw, []chaosHeartbeatCall{{"ping", map[string]any{}}, {"session.status", map[string]any{"session_id": sid}}},
		chaosHeartbeatInterval, chaosLongTimeoutS*time.Second)
	began := time.Now()
	submitted := gw.call(t, "prompt.submit", map[string]any{"session_id": sid, "text": "please work " + marker}, 60*time.Second)
	if submitted["status"] != "streaming" {
		hb.close()
		t.Fatalf("%s: %v", scn.id, submitted)
	}
	switch scn.waitFor {
	case "provider":
		if !chaosWaitUntil(func() bool { return len(mainRequestsFor(srv, marker)) > 0 }, chaosTurnDeadline) {
			hb.close()
			t.Fatalf("provider never saw the turn%s", gw.tail())
		}
	case "tool":
		if started, _ := gw.findEvent(start, chaosTurnDeadline, func(e map[string]any) bool { return chaosIs(e, "tool.start", sid) }); started == nil {
			hb.close()
			t.Fatalf("hung tool never started%s", gw.tail())
		}
		if !chaosWaitUntil(func() bool { return len(hungToolPIDs(tag)) > 0 }, chaosTurnDeadline) {
			hb.close()
			t.Fatal("hung tool process never spawned")
		}
	}
	deadline := chaosTurnDeadline
	if scn.action != "" {
		chaosWaitUntil(func() bool { return hb.count() >= chaosMinHeartbeats }, chaosTurnDeadline)
	}
	if scn.action == "interrupt" {
		interruptedAt := time.Now()
		reply := gw.call(t, "session.interrupt", map[string]any{"session_id": sid}, chaosInterruptDeadline)
		if reply["status"] != "interrupted" {
			hb.close()
			t.Fatalf("%s: interrupt answered %v", scn.id, reply)
		}
		began, deadline = interruptedAt, chaosInterruptDeadline
	}
	var done map[string]any
	doneIndex := -1
	if !chaosExitAction(scn.action) {
		done, doneIndex = gw.findEvent(start, deadline, func(e map[string]any) bool { return chaosIs(e, "message.complete", sid) })
	}
	turn := time.Since(began)
	hb.close()
	stateDB := filepath.Join(stateHome, "state.db")
	if chaosExitAction(scn.action) {
		chaosExitMidTurn(t, scn, gw, hb, tag, stateDB, stored)
		return
	}
	if done == nil {
		t.Fatalf("%s: no terminal message.complete within %v (events: %v)%s", scn.id, deadline, gw.eventTypesSince(start, sid), gw.tail())
	}
	payload, _ := done["payload"].(map[string]any)
	if scn.action == "interrupt" && payload["status"] != "interrupted" {
		t.Fatalf("%s: %v", scn.id, done)
	}
	waitChaosSettled(t, gw, sid, doneIndex, scn.id)
	calls := len(mainRequestsFor(srv, marker))
	if calls < 1 || calls > chaosMaxProviderCallsPerFailedTurn {
		t.Fatalf("%s: %d provider calls", scn.id, calls)
	}
	assertChaosHeartbeat(t, scn, hb, turn)
	if scn.reapsHungTool && !chaosWaitUntil(func() bool { return len(hungToolPIDs(tag)) == 0 }, chaosProcessReap) {
		t.Fatalf("%s: hung tool survived its turn: %v", scn.id, describePIDs(hungToolPIDs(tag)))
	}
	probe := chaosAnsweredTurn(t, gw, sid, nonce, scn.id+" after the fault", chaosTurnDeadline)
	t.Logf("%s turn_s=%.2f status=%v provider_calls=%d probe_s=%.2f", scn.id, turn.Seconds(), payload["status"], calls, probe.Seconds())
	probeRequests := mainRequestsFor(srv, probeMarker)
	if len(probeRequests) == 0 {
		t.Fatalf("%s: follow-up never reached the provider", scn.id)
	}
	sentRaw, _ := probeRequests[len(probeRequests)-1]["messages"].([]any)
	sent := make([]map[string]any, 0, len(sentRaw))
	carried := false
	for _, item := range sentRaw {
		msg, _ := item.(map[string]any)
		sent = append(sent, msg)
		carried = carried || (msg["role"] == "user" && strings.Contains(chaosTextOf(msg["content"]), marker))
	}
	if !carried {
		t.Fatalf("%s: follow-up ran without the faulted turn in its history (not the same session)", scn.id)
	}
	if missing := unansweredToolCalls(sent); len(missing) != 0 {
		t.Fatalf("%s: model was sent unanswered tool_calls %v", scn.id, missing)
	}
	persisted := persistedMessages(t, stateDB, stored)
	if missing := unansweredToolCalls(persisted); len(missing) != 0 {
		t.Fatalf("%s: state.db has unanswered tool_calls %v", scn.id, missing)
	}
	if scn.fault == "parallel" {
		assertParallelResults(t, persisted, nonce, "state.db")
		assertParallelResults(t, sent, nonce, "provider request")
	}
	if !gw.closeStdinAndWait(chaosExitTimeout) {
		t.Fatalf("%s: gateway ignored stdin EOF for %v%s", scn.id, chaosExitTimeout, gw.tail())
	}
	if survivors := waitNoTagged(tag, chaosProcessReap); len(survivors) > 0 {
		t.Fatalf("%s: orphans after exit: %v", scn.id, describePIDs(survivors))
	}
	if verdict := integrityOK(t, stateDB); verdict != "ok" {
		t.Fatalf("%s: integrity_check %s", scn.id, verdict)
	}
}

func TestTuiGatewayTurnStaysLiveUnderFault(t *testing.T) {
	srv, err := fakellm.NewFakeLLMResponder(chaosResponder, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	for _, scn := range chaosScenarios {
		t.Run(scn.id, func(t *testing.T) {
			t.Parallel()
			runChaosScenario(t, scn, srv, t.TempDir())
		})
	}
}
