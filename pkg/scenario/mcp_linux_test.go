package scenario

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Answers the streamable HTTP transport of an MCP server that offers one echo tool
func echoMCPServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var call struct {
		ID     any            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if call.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch call.Method {
	case "initialize":
		version, _ := call.Params["protocolVersion"].(string)
		result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "scenario-echo", "version": "1.0.0"}}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Returns the text it was given",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}}}}}
	case "tools/call":
		args, _ := call.Params["arguments"].(map[string]any)
		text, _ := args["text"].(string)
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo: " + text}}, "isError": false}
	case "ping":
		result = map[string]any{}
	default:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
}

// An MCP server is added, listed and tested, its tool is found and called by the model in a turn, and it is removed
func TestScenarioMCPAddAndToolCall(t *testing.T) {
	s := Start(t, Setup{Model: []any{
		fakellm.ToolCall{Name: "tool_search", Args: map[string]any{"queries": []any{"echo"}}},
		fakellm.ToolCall{Name: "tool_call", Args: map[string]any{"calls": []any{
			map[string]any{"name": "mcp__fake__echo", "arguments": map[string]any{"text": "ping from the model"}}}}},
		fakellm.Text{Text: "The server echoed it."},
	}})
	server := s.Peer("mcp", http.HandlerFunc(echoMCPServer))
	s.Run("mcp", "list").Exit(0).Has("No MCP servers configured.", "icebath mcp add <name> --url <endpoint>")

	added := s.RunStep(Step{Args: []string{"mcp", "add", "fake", "--url", "${PEER_mcp}/mcp"}, Stdin: "n\ny\n"}).Exit(0)
	added.Has("Does this server require authentication? [Y/n]:", "Connecting to 'fake'...", "Connected! Found 1 tool(s) from 'fake':",
		"Returns the text it was given", "Enable all 1 tools? [Y/n/select]:", "Saved 'fake' to ", "/config.yaml (1/1 tools enabled)",
		"Start a new session to use these tools, or run /reload-mcp to load them into open sessions.")
	s.HasJSON(s.YAML("config.yaml"), `{"mcp_servers": {"fake": {"url": "${PEER_mcp}/mcp", "enabled": true}}}`)
	s.Run("mcp", "list").Exit(0).Has("fake", "${PEER_mcp}/mcp")
	s.Run("mcp", "test", "fake").Exit(0).Has("Testing 'fake'...", "Transport: HTTP → ${PEER_mcp}/mcp", "Auth: none", "Connected (", "Tools discovered: 1")

	s.RunStep(Step{Args: []string{"chat", "-q", "Echo something through the server", "-Q"}, Timeout: 3 * time.Minute}).Exit(0).Has("The server echoed it.")
	if found := s.ToolResult("tool_search"); !strings.Contains(found, "mcp__fake__echo") {
		s.Failf("tool_search for echo did not find mcp__fake__echo: %s", clip(found))
	}
	if got := s.ToolResult("tool_call"); !strings.Contains(got, "echo: ping from the model") {
		s.Failf("the tool result the model received is %s", clip(got))
	}
	called := false
	for _, req := range server.Requests() {
		var call map[string]any
		if json.Unmarshal([]byte(req.Body), &call) == nil && call["method"] == "tools/call" {
			called = true
			s.HasJSON(call, `{"params": {"name": "echo", "arguments": {"text": "ping from the model"}}}`)
		}
	}
	if !called {
		s.Failf("the server never received tools/call")
	}

	s.RunStep(Step{Args: []string{"mcp", "remove", "fake"}, Stdin: "y\n"}).Exit(0).Has("Removed 'fake' from config")
	if servers, _ := Dig(s.YAML("config.yaml"), "mcp_servers").(map[string]any); servers["fake"] != nil {
		s.Failf("config.yaml still holds the removed server: %v", servers)
	}
	s.Run("mcp", "list").Exit(0).Has("No MCP servers configured.")
}
