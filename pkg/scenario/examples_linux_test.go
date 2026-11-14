package scenario

import (
	"net/http"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Worked example of SCENARIOS.md, an HTTP exchange with the API server of a running gateway
func TestScenarioExampleHTTPWithTheGateway(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "Hello through the gateway."}}})
	gw := startAPIGateway(s)
	auth := []string{"Authorization", "Bearer " + apiKey}
	s.HTTP("GET", "${API}/v1/models", nil).Code(401)
	reply := s.HTTP("POST", "${API}/v1/chat/completions", map[string]any{"model": "scenario-agent",
		"messages": []any{map[string]any{"role": "user", "content": "Say hello"}}}, auth...).Code(200)
	s.HasJSON(reply.Body, `{"choices": [{"message": {"role": "assistant", "content": "Hello through the gateway."}, "finish_reason": "stop"}]}`)
	gw.Stop()
}

// Worked example of SCENARIOS.md, a JSON-RPC exchange over the WebSocket of a running gateway
func TestScenarioExampleWebSocketWithTheGateway(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "Seen by both clients."}}})
	gw := s.Spawn("gateway", "run")
	gatewayRunning(s)
	w := attachGateway(s)
	created := w.Call("session.create", map[string]any{"request_id": "mine-1", "source": "cli", "cwd": s.Work})
	session, _ := Dig(created, "result", "session_id").(string)
	w.Call("prompt.submit", map[string]any{"session_id": session, "input_id": "mine-input-1", "text": "Say something"})
	done := w.Until("message.complete of the turn", func(m map[string]any) bool { return Dig(m, "params", "type") == "message.complete" })
	s.HasJSON(done, `{"params": {"session_id": "`+session+`", "payload": {"text": "Seen by both clients.", "status": "complete"}}}`)
	s.HasJSON(w.Call("no.such.method", map[string]any{}), `{"error": {"code": -32601}}`)
	gw.Stop()
}

// Worked example of SCENARIOS.md, a loopback peer standing in for a service the product calls
func TestScenarioExampleFakePeerService(t *testing.T) {
	s := Start(t, Setup{})
	server := s.Peer("mcp", http.HandlerFunc(echoMCPServer))
	s.RunStep(Step{Args: []string{"mcp", "add", "fake", "--url", "${PEER_mcp}/mcp"}, Stdin: "n\ny\n"}).Exit(0)
	s.Run("mcp", "test", "fake").Exit(0).Has("Tools discovered: 1")
	listed := 0
	for _, req := range server.Requests() {
		if req.Method == "POST" && req.Path == "/mcp" && strings.Contains(req.Body, `"tools/list"`) {
			listed++
		}
	}
	if listed == 0 {
		s.Failf("the peer never received tools/list, it saw %d requests", len(server.Requests()))
	}
}

// Worked example of SCENARIOS.md, a turn whose scripted model calls two tools
func TestScenarioExampleScriptedToolCall(t *testing.T) {
	s := Start(t, Setup{Model: []any{
		fakellm.ToolCall{Name: "write_file", Args: map[string]any{"path": "notes.txt", "content": "alpha line\nbeta line\n"}},
		fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "cat notes.txt"}},
		fakellm.Text{Text: "Done."},
	}})
	s.Run("chat", "-q", "Write and show the note", "-Q").Exit(0).Has("Done.")
	s.HasJSON(s.ToolResult("write_file"), `{"bytes_written": 21}`)
	s.HasJSON(s.ToolResult("terminal"), `{"exit_code": 0, "error": null}`)
	if s.Read("${WORK}/notes.txt") != "alpha line\nbeta line\n" {
		s.Failf("notes.txt holds %q", s.Read("${WORK}/notes.txt"))
	}
}

// Worked example of SCENARIOS.md, two error paths with their exit codes and words
func TestScenarioExampleErrorPathExitCode(t *testing.T) {
	refusals := make([]any, 12)
	for i := range refusals {
		refusals[i] = fakellm.Error{Status: 401, Message: "invalid api key for scenario"}
	}
	s := Start(t, Setup{Model: refusals})
	s.Run("sessions", "rename", "no-such-session", "x").Exit(1).Has("No session 'no-such-session'. Run: icebath sessions list to find the id.")
	failed := s.Run("chat", "-q", "Say hello", "-Q").Exit(1)
	if !strings.Contains(failed.Stdout, "Provider said: ") || !strings.Contains(failed.Stdout, "invalid api key for scenario") {
		s.Failf("the failure explanation is missing\n%s", failed.Text())
	}
}
