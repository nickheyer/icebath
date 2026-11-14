package scenario

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Each tool call of a turn is dispatched once with the hook rewritten arguments and reported with its call ids
func TestScenarioRelayToolPathDispatchesFinalArgumentsOnce(t *testing.T) {
	s := Start(t, Setup{
		Model: []any{
			fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "echo original >> relay.txt"}},
			fakellm.ToolCall{Name: "read_file", Args: map[string]any{"path": "absent-relay-file.txt"}},
			fakellm.Text{Text: "Both calls answered."},
		},
		Extra: "hooks_auto_accept: true\nhooks:\n" +
			"  pre_tool_call:\n    - matcher: \"terminal\"\n      command: \"bash ${ICEBATH_HOME}/agent-hooks/rewrite.sh\"\n" +
			"  post_tool_call:\n    - command: \"bash ${ICEBATH_HOME}/agent-hooks/after.sh\"\n",
		Home: map[string]string{
			"agent-hooks/rewrite.sh": "cat > /dev/null\necho '{\"action\": \"modify\", \"args\": {\"command\": \"echo rewritten >> relay.txt\"}}'\n",
			"agent-hooks/after.sh":   "payload=$(cat)\nprintf '%s\\n' \"$payload\" >> \"$ICEBATH_HOME/agent-hooks/after.jsonl\"\n",
		},
	})
	r := s.Run("chat", "--accept-hooks", "-q", "Run both calls", "-Q").Exit(0).Has("Both calls answered.")
	id := r.Match(`(?m)^session_id: (\S+)$`)[1]

	if got := s.Read("${WORK}/relay.txt"); got != "rewritten\n" {
		s.Failf("relay.txt holds %q, the rewritten command must have run exactly once", got)
	}
	s.HasJSON(s.ToolResult("terminal"), `{"exit_code": 0, "error": null}`)
	missing := s.ToolResult("read_file")
	if !strings.Contains(missing, "absent-relay-file.txt") || !strings.Contains(missing, `"error"`) {
		s.Failf("the failed read_file call answered %q", missing)
	}

	after := strings.Split(strings.TrimSpace(s.Read("agent-hooks/after.jsonl")), "\n")
	if len(after) != 2 {
		s.Failf("the post_tool_call hook saw %d payloads, want one per call", len(after))
	}
	s.HasJSON(after[0], `{"hook_event_name": "post_tool_call", "tool_name": "terminal", "tool_input": {"command": "echo rewritten >> relay.txt"},
		"session_id": "`+id+`", "profile": "default", "extra": {"tool_call_id": "call_fake_1", "status": "ok", "error_type": null, "error_message": null}}`)
	s.HasJSON(after[1], `{"hook_event_name": "post_tool_call", "tool_name": "read_file", "tool_input": {"path": "absent-relay-file.txt"},
		"session_id": "`+id+`", "extra": {"tool_call_id": "call_fake_2", "status": "error", "error_type": "tool_error"}}`)
	for _, line := range after {
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			s.Failf("a post_tool_call payload is not JSON: %v\n%s", err, line)
		}
		if turn, _ := Dig(payload, "extra", "turn_id").(string); turn == "" {
			s.Failf("a post_tool_call payload carries no turn id: %s", line)
		}
	}
	for _, log := range []string{"logs/agent.log", "logs/errors.log"} {
		if s.Exists(log) && strings.Contains(s.Read(log), "tool execution callback invoked more than once") {
			s.Failf("%s reports a second dispatch of one tool call", log)
		}
	}
}
