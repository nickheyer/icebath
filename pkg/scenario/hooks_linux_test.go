package scenario

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Shell hooks from config.yaml block one tool call, see every call as JSON and add context to the model request
func TestScenarioShellHooksBlockObserveAndInject(t *testing.T) {
	s := Start(t, Setup{
		Model: []any{
			fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "echo forbidden-word > hooked.txt"}},
			fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "echo allowed-word > hooked.txt"}},
			fakellm.Text{Text: "One call was blocked."},
		},
		Extra: "hooks_auto_accept: true\nhooks:\n" +
			"  pre_tool_call:\n    - matcher: \"terminal\"\n      command: \"bash ${ICEBATH_HOME}/agent-hooks/guard.sh\"\n" +
			"  pre_llm_call:\n    - command: \"bash ${ICEBATH_HOME}/agent-hooks/context.sh\"\n",
		Home: map[string]string{
			"agent-hooks/guard.sh": "payload=$(cat)\nprintf '%s\\n' \"$payload\" >> \"$ICEBATH_HOME/agent-hooks/seen.jsonl\"\n" +
				"case \"$payload\" in *forbidden-word*) echo '{\"decision\": \"block\", \"reason\": \"Forbidden by the scenario hook\"}' ;; esac\n",
			"agent-hooks/context.sh": "cat > /dev/null\necho '{\"context\": \"Today is Scenarioday\"}'\n",
		},
	})
	s.Run("hooks", "list").Exit(0).Has("pre_tool_call", "pre_llm_call", "guard.sh", "context.sh")
	s.Run("chat", "--accept-hooks", "-q", "Write the word", "-Q").Exit(0).Has("One call was blocked.")

	var results []string
	for _, m := range s.LastMessages() {
		if Dig(m, "role") == "tool" {
			text, _ := Dig(m, "content").(string)
			results = append(results, text)
		}
	}
	if len(results) != 2 || !strings.Contains(results[0], "Forbidden by the scenario hook") {
		s.Failf("the blocked call did not carry the hook reason to the model: %v", results)
	}
	s.HasJSON(results[1], `{"exit_code": 0}`)
	if got := s.Read("${WORK}/hooked.txt"); got != "allowed-word\n" {
		s.Failf("hooked.txt holds %q, the blocked command must not have run", got)
	}
	seen := strings.Split(strings.TrimSpace(s.Read("agent-hooks/seen.jsonl")), "\n")
	if len(seen) != 2 {
		s.Failf("the pre_tool_call hook saw %d payloads, want 2", len(seen))
	}
	s.HasJSON(seen[0], `{"hook_event_name": "pre_tool_call", "tool_name": "terminal", "tool_input": {"command": "echo forbidden-word > hooked.txt"},
		"profile": "default", "extra": {"tool_call_id": "call_fake_1"}}`)
	prompted := false
	for _, m := range s.Model.MainRequests()[0]["messages"].([]any) {
		text, _ := Dig(m, "content").(string)
		prompted = prompted || (Dig(m, "role") == "user" && strings.Contains(text, "Today is Scenarioday") && strings.Contains(text, "Write the word"))
	}
	if !prompted {
		s.Failf("the pre_llm_call context did not reach the model with the user message")
	}
}
