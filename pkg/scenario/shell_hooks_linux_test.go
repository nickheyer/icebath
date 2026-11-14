package scenario

import (
	"strings"
	"testing"
)

// hooks list names the configured hook and hooks test fires it with quoted arguments kept whole
func TestScenarioShellHooksListAndTest(t *testing.T) {
	script := "cat > \"$ICEBATH_HOME/hook-payload.json\"\nprintf '%s|%s' \"$1\" \"$2\" > \"$ICEBATH_HOME/hook-args.txt\"\n" +
		"echo '{\"decision\": \"block\", \"reason\": \"scenario says no\"}'\n"
	s := Start(t, Setup{
		Extra: "hooks:\n  pre_tool_call:\n    - matcher: terminal\n      command: \"sh ${ICEBATH_HOME}/agent-hooks/block.sh --tag 'two words'\"\n      timeout: 10\n" +
			"  post_tool_call:\n    - command: \"sh ${ICEBATH_HOME}/agent-hooks/block.sh 'never closed\"\n",
		Home: map[string]string{"agent-hooks/block.sh": script},
	})
	listed := s.Run("hooks", "list").Exit(0)
	listed.Has("Configured shell hooks (2 total):", "[pre_tool_call]", "[post_tool_call]",
		"sh "+s.Path("agent-hooks/block.sh")+" --tag 'two words'", "matcher=", "timeout=10s", "✗ not allowlisted")

	fired := s.Run("hooks", "test", "pre_tool_call", "--for-tool", "terminal").Exit(0)
	fired.Has("Firing 1 hook(s) for event 'pre_tool_call':", "→ sh "+s.Path("agent-hooks/block.sh")+" --tag 'two words'", "exit=0",
		`stdout: {"decision": "block", "reason": "scenario says no"}`, `parsed (Icebath wire shape): {"action": "block", "message": "scenario says no"}`)
	if got := s.Read("hook-args.txt"); got != "--tag|two words" {
		s.Failf("the hook received the arguments %q, want the quoted pair kept as one argument", got)
	}
	payload := s.value(s.Read("hook-payload.json"))
	if Dig(payload, "hook_event_name") != "pre_tool_call" || Dig(payload, "tool_name") != "terminal" {
		s.Failf("the hook read the payload %v, want the pre_tool_call event of the terminal tool", payload)
	}

	s.Run("hooks", "test", "pre_tool_call", "--for-tool", "read_file").Exit(0).
		Has("No shell hooks configured for event: pre_tool_call", "(with matcher filter --for-tool=read_file)").Lacks("Firing")
	broken := s.Run("hooks", "test", "post_tool_call").Exit(0)
	broken.Has("Firing 1 hook(s) for event 'post_tool_call':", "✗ error: command ", "cannot be parsed: No closing quotation",
		"parsed: <none — hook contributed nothing to the dispatcher>")
	unknown := s.Run("hooks", "test", "no_such_event").Exit(0)
	unknown.Has("Unknown event: 'no_such_event'", "Valid events: ")
	if !strings.Contains(unknown.Text(), "pre_tool_call") {
		s.Failf("the valid events do not name pre_tool_call: %s", unknown.Text())
	}
}
