package tools

import (
	"sync"
	"testing"
)

func TestToolResultTextMatchesJSONDumpsOfHermes(t *testing.T) {
	store := NewTodoStore()
	todoOut := todoTool(store, []any{
		map[string]any{"id": "1", "content": "\u00e9", "status": "pending"},
		map[string]any{"id": "2", "content": "b", "status": "completed", "parent": "1"},
	}, false)
	zero := 0
	exited := &ProcessSession{Command: "c \u00e9", OutputBuffer: "out", ExitCode: &zero, CompletionReason: "exited"}
	turnEnv := &Env{Values: &sync.Map{}}
	turnEnv.Values.Store(nativeTurnImagesKey, map[string]bool{visionImageKey(turnEnv, "https://x.test/a.png"): true})
	pages := []*WebPage{
		{URL: "u", Title: "t", Content: "c \u00e9"},
		{URL: "v", Error: "e", BlockedByPolicy: map[string]any{"host": "h", "rule": "r", "source": "s"}},
	}
	block := ToolGuardrailDecision{Action: "block", Code: "loop", Message: "stop", ToolName: "t", Count: 3}
	signed := block
	signed.Signature = &ToolCallSignature{ToolName: "t", ArgsHash: "h"}
	cases := []struct{ name, got, want string }{
		{"tool_error_extra", toolErrorJSON("bad \u00e9", Dict("success", false)), "{\"error\": \"bad \u00e9\", \"success\": false}"},
		{"validation_error", validationError("m", "a.b[0]", "type", Dict("type", "object")), "{\"error\": \"m\", \"path\": \"a.b[0]\", \"constraint\": \"type\", \"parameters\": {\"type\": \"object\"}, \"hint\": \"Retry tool_call with 'arguments' matching the parameters schema above.\"}"},
		{"project_fail_ascii", projectFail("\u00e9"), "{\"success\": false, \"error\": \"\\u00e9\"}"},
		{"blocked", blockedJSON("\u00e9", "blocked"), "{\"output\": \"\", \"exit_code\": 1, \"error\": \"\u00e9\", \"status\": \"blocked\"}"},
		{"terminal_error", TerminalErrorJSON("boom", "error"), "{\"output\": \"\", \"exit_code\": -1, \"error\": \"boom\", \"status\": \"error\"}"},
		{"guardrail_signed", ToolguardSyntheticResult(signed), "{\"error\": \"stop\", \"guardrail\": {\"action\": \"block\", \"code\": \"loop\", \"message\": \"stop\", \"tool_name\": \"t\", \"count\": 3, \"signature\": {\"tool_name\": \"t\", \"args_hash\": \"h\"}}}"},
		{"guardrail_unsigned", ToolguardSyntheticResult(block), "{\"error\": \"stop\", \"guardrail\": {\"action\": \"block\", \"code\": \"loop\", \"message\": \"stop\", \"tool_name\": \"t\", \"count\": 3}}"},
		{"process_not_found", JSON(notFoundResult("p1")), "{\"status\": \"not_found\", \"error\": \"No process with ID p1\"}"},
		{"process_exit_snapshot", JSON(exitSnapshot(exited, "exited")), "{\"status\": \"exited\", \"command\": \"c \u00e9\", \"exit_code\": 0, \"completion_reason\": \"exited\", \"termination_source\": \"\", \"output\": \"out\"}"},
		{"todo", todoOut, "{\"todos\": [{\"id\": \"1\", \"content\": \"\u00e9\", \"status\": \"pending\"}, {\"id\": \"2\", \"content\": \"b\", \"status\": \"completed\", \"parent\": \"1\"}], \"revision\": 1, \"summary\": {\"total\": 2, \"pending\": 1, \"in_progress\": 0, \"completed\": 1, \"cancelled\": 0}}"},
		{"extract_results_indent", jsonIndent(Dict("results", trimResults(pages))), "{\n  \"results\": [\n    {\n      \"url\": \"u\",\n      \"title\": \"t\",\n      \"content\": \"c \u00e9\",\n      \"error\": null\n    },\n    {\n      \"url\": \"v\",\n      \"title\": \"\",\n      \"content\": \"\",\n      \"error\": \"e\",\n      \"blocked_by_policy\": {\n        \"host\": \"h\",\n        \"rule\": \"r\",\n        \"source\": \"s\"\n      }\n    }\n  ]\n}"},
		{"vision_duplicate_ascii", nativeTurnDuplicate(turnEnv, "https://x.test/a.png", nil), "{\"success\": true, \"already_in_context\": true, \"message\": \"This image is already attached natively to the current user message \\u2014 you can see it now. Answer with your built-in vision; pass a `region` to zoom into part of it.\"}"},
		{"multi_select", JSON([]string{"a", "\u00e9"}), "[\"a\", \"\u00e9\"]"},
		{"close_terminal_unavailable", JSON((&ProcessRegistry{}).RequestCloseTerminal(nil, "p1")), "{\"status\": \"error\", \"error\": \"close_terminal is only available in the icebath desktop app.\"}"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s\n got %s\nwant %s", c.name, c.got, c.want)
		}
	}
}
