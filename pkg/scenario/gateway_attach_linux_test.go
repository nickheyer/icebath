package scenario

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Waits until the gateway of the home reports the running state in gateway_state.json
func gatewayRunning(s *S) {
	s.t.Helper()
	s.Eventually("gateway_state.json reporting running", 3*time.Minute, func() bool {
		var state map[string]any
		raw, err := os.ReadFile(s.Path("gateway_state.json"))
		return err == nil && json.Unmarshal(raw, &state) == nil && state["gateway_state"] == "running"
	})
}

// Attaches one more client to the running gateway over its WebSocket and returns it past the gateway.ready event
func attachGateway(s *S) *Socket {
	s.t.Helper()
	s.WaitFile("gateway.sock")
	var owner map[string]any
	s.Eventually("the gateway owner reporting ready", 3*time.Minute, func() bool {
		owner = controlVerb(s, "identify", nil)
		return owner["state"] == "ready"
	})
	grant := controlVerb(s, "session-ticket", map[string]any{"profile_id": s.Home, "instance_id": owner["instance_id"], "purpose": "interactive"})
	ticket, _ := grant["ticket"].(string)
	origin, _ := owner["api_origin"].(string)
	w := s.Dial("ws"+strings.TrimPrefix(origin, "http")+"/api/ws", "Sec-WebSocket-Protocol", "icebath-gateway-v1, icebath-gateway-ticket."+ticket)
	if ready := w.Next(); Dig(ready, "params", "type") != "gateway.ready" {
		s.Failf("the first WebSocket message is %v, want the gateway.ready event", ready)
	}
	return w
}

// A second client attached to the same gateway lists the session the first client created and ran a turn in
func TestScenarioTUIGatewayTwoClientsShareASession(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "Seen by both clients."}}})
	gw := s.Spawn("gateway", "run")
	gatewayRunning(s)
	first, second := attachGateway(s), attachGateway(s)
	created := first.Call("session.create", map[string]any{"request_id": "two-clients-1", "source": "cli", "cwd": s.Work})
	session, _ := Dig(created, "result", "session_id").(string)
	if session == "" {
		s.Failf("session.create returned no session id: %v", created)
	}
	first.Call("prompt.submit", map[string]any{"session_id": session, "input_id": "two-clients-input-1", "text": "Say something"})
	done := first.Until("message.complete of the turn", func(m map[string]any) bool { return Dig(m, "params", "type") == "message.complete" })
	s.HasJSON(done, `{"params": {"session_id": "`+session+`", "payload": {"text": "Seen by both clients.", "status": "complete"}}}`)
	s.HasJSON(second.Call("session.list", map[string]any{}), `{"result": {"sessions": [{"id": "`+session+`", "message_count": 2}]}}`)
	s.HasJSON(second.Call("ping", map[string]any{}), `{"result": {"pong": true}}`)
	gw.Stop()
}
