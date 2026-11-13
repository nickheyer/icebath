package scenario

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Asks the private control socket of the gateway one verb and returns its result object
func controlVerb(s *S, verb string, params map[string]any) map[string]any {
	s.t.Helper()
	req := map[string]any{"protocol": 1, "id": 1, "verb": verb}
	if params != nil {
		req["params"] = params
	}
	line, _ := json.Marshal(req)
	answer := s.value(s.UnixLine("gateway.sock", string(line)))
	result, ok := Dig(answer, "result").(map[string]any)
	if Dig(answer, "ok") != true || Dig(answer, "protocol") != float64(1) || Dig(answer, "id") != float64(1) || !ok {
		s.Failf("control verb %s answered %v", verb, answer)
	}
	return result
}

// A peer finds the gateway through its control socket, attaches by ticket and drives a turn over JSON-RPC
func TestScenarioTUIGatewayWebSocketTurn(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "Hello over the socket.", ChunkChars: 6}}})
	gw := s.Spawn("gateway", "run")
	s.WaitFile("gateway.sock")
	var owner map[string]any
	s.Eventually("the gateway owner reporting ready", waitLimit, func() bool {
		owner = controlVerb(s, "identify", nil)
		return owner["state"] == "ready"
	})
	s.HasJSON(owner, `{"kind": "icebath-gateway", "protocol": 1, "runtime_protocol": 1, "profile": "default", "hermes_home": "${ICEBATH_HOME}",
		"served_profiles": [{"home": "${ICEBATH_HOME}", "profile_id": "${ICEBATH_HOME}"}]}`)
	origin, _ := owner["api_origin"].(string)
	if !strings.HasPrefix(origin, "http://127.0.0.1:") {
		s.Failf("the owner publishes the API origin %q, want a loopback address", origin)
	}
	for _, capability := range []string{"session-authority-v1", "durable-admission-v1", "event-replay-v1"} {
		found := false
		for _, have := range owner["capabilities"].([]any) {
			found = found || have == capability
		}
		if !found {
			s.Failf("the owner lacks capability %s: %v", capability, owner["capabilities"])
		}
	}
	binding := map[string]any{"profile_id": s.Home, "instance_id": owner["instance_id"], "purpose": "interactive"}
	grant := controlVerb(s, "session-ticket", binding)
	ticket, _ := grant["ticket"].(string)
	if ticket == "" || grant["instance_id"] != owner["instance_id"] || grant["profile_id"] != s.Home {
		s.Failf("session-ticket granted %v", grant)
	}
	refused := s.value(s.UnixLine("gateway.sock", `{"protocol":1,"id":1,"verb":"session-ticket","params":{"profile_id":"${ICEBATH_HOME}","instance_id":"0000","purpose":"interactive"}}`))
	s.HasJSON(refused, `{"ok": false, "error": "PermissionError: stale runtime instance"}`)

	url := "ws" + strings.TrimPrefix(origin, "http") + "/api/ws"
	_, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Sec-WebSocket-Protocol": {"icebath-gateway-v1, icebath-gateway-ticket.not-a-ticket"}})
	if err == nil || resp == nil || resp.StatusCode != 403 {
		s.Failf("a WebSocket upgrade with a made up ticket was not refused with 403: %v %v", err, resp)
	}
	w := s.Dial(url, "Sec-WebSocket-Protocol", "icebath-gateway-v1, icebath-gateway-ticket."+ticket)
	if w.Subprotocol() != "icebath-gateway-v1" {
		s.Failf("the owner selected subprotocol %q", w.Subprotocol())
	}
	s.HasJSON(w.Next(), `{"jsonrpc": "2.0", "method": "event", "params": {"type": "gateway.ready"}}`)
	s.HasJSON(w.Call("ping", map[string]any{}), `{"result": {"pong": true}}`)
	s.HasJSON(w.Call("no.such.method", map[string]any{}), `{"error": {"code": -32601,
		"message": "unknown method: no.such.method — not available on this Icebath backend"}}`)

	described := w.Call("runtime.describe", map[string]any{})
	s.HasJSON(described, `{"result": {"profile_id": "${ICEBATH_HOME}", "session_create": {"sources": ["acp", "cli", "gui", "oneshot", "tool", "tui"]}}}`)
	parameters, _ := json.Marshal(Dig(described, "result", "session_create", "parameters"))
	for _, parameter := range []string{"cwd", "model", "provider", "title", "toolsets", "skills", "request_id", "source", "yolo"} {
		if !strings.Contains(string(parameters), `"`+parameter+`"`) {
			s.Failf("session.create does not accept %s: %s", parameter, parameters)
		}
	}
	created := w.Call("session.create", map[string]any{"request_id": "scenario-request-1", "source": "cli", "cwd": s.Work})
	s.HasJSON(created, `{"result": {"info": {"model": "fake-model", "provider": "custom", "cwd": "${WORK}", "profile_name": "default"}}}`)
	session, _ := Dig(created, "result", "session_id").(string)
	if session == "" {
		s.Failf("session.create returned no session id: %v", created)
	}

	receipt := w.Call("prompt.submit", map[string]any{"session_id": session, "input_id": "scenario-input-1", "text": "Say hello"})
	s.HasJSON(receipt, `{"result": {"status": "queued", "ref": {"session_id": "`+session+`", "profile_id": "${ICEBATH_HOME}"}}}`)
	admission, _ := Dig(receipt, "result", "admission_id").(string)
	done := w.Until("message.complete of the turn", func(m map[string]any) bool { return Dig(m, "params", "type") == "message.complete" })
	s.HasJSON(done, `{"params": {"session_id": "`+session+`", "admission_id": "`+admission+`",
		"payload": {"text": "Hello over the socket.", "outcome": "completed", "status": "complete"}}}`)
	var streamed strings.Builder
	for _, ev := range w.Events {
		if Dig(ev, "params", "type") == "message.delta" && Dig(ev, "params", "admission_id") == admission {
			piece, _ := Dig(ev, "params", "payload", "text").(string)
			streamed.WriteString(piece)
		}
	}
	if streamed.String() != "Hello over the socket." {
		s.Failf("message.delta events carried %q", streamed.String())
	}
	if Dig(s.LastMessages(), -1, "content") != "Say hello" {
		s.Failf("the model did not receive the submitted text")
	}
	s.HasJSON(w.Call("session.list", map[string]any{}), `{"result": {"sessions": [{"id": "`+session+`", "message_count": 2, "model": "fake-model"}]}}`)
	s.SameJSON(s.Rows("state.db", "SELECT role, content FROM messages WHERE session_id = ? ORDER BY id", session),
		`[{"role":"user","content":"Say hello"},{"role":"assistant","content":"Hello over the socket."}]`)
	gw.Stop()
}
