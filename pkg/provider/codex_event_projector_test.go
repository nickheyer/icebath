package provider

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func codexNote(t *testing.T, text string) any {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatalf("fixture %s: %v", text, err)
	}
	return v
}

func codexCompleted(t *testing.T, item string) any {
	return codexNote(t, `{"method": "item/completed", "params": {"item": `+item+`}}`)
}

const codexCommandExecCompleted = `{"type": "commandExecution", "id": "f8a75c66-a89e-4fd7-8bcf-2d58e664fa9e",
 "command": "/bin/bash -lc 'echo hello && ls /tmp | head -3'", "cwd": "/tmp", "processId": null,
 "source": "userShell", "status": "completed", "commandActions": [{"type": "listFiles", "command": "ls /tmp", "path": "tmp"}],
 "aggregatedOutput": "hello\naa_lang.json\n", "exitCode": 0, "durationMs": 10}`

func TestCodexProjectionInvariants(t *testing.T) {
	p := NewCodexEventProjector()
	for _, n := range []string{
		`{"method": "item/agentMessage/delta", "params": {"delta": "hel"}}`,
		`{"method": "item/started", "params": {"item": {"type": "agentMessage", "id": "a"}}}`,
		`{"method": "turn/started", "params": {}}`, `{"method": "turn/completed", "params": {}}`, `{"method": "weird/thing"}`,
	} {
		if r := p.Project(codexNote(t, n)); len(r.Messages) != 0 || r.IsToolIteration || r.FinalText != nil {
			t.Fatalf("%s materialized %+v", n, r)
		}
	}
}

func TestCodexCommandExecutionProjection(t *testing.T) {
	r := NewCodexEventProjector().Project(codexCompleted(t, codexCommandExecCompleted))
	if len(r.Messages) != 2 || !r.IsToolIteration {
		t.Fatalf("result %+v", r)
	}
	call := r.Messages[0]["tool_calls"].([]any)[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if r.Messages[0]["role"] != "assistant" || r.Messages[0]["content"] != nil || fn["name"] != "exec_command" {
		t.Fatalf("assistant %+v", r.Messages[0])
	}
	if fn["arguments"] != `{"command": "/bin/bash -lc 'echo hello && ls /tmp | head -3'", "cwd": "/tmp"}` {
		t.Fatalf("arguments %s", fn["arguments"])
	}
	if call["id"] != "codex_exec_f8a75c66-a89e-4fd7-8bcf-2d58e664fa9e" || r.Messages[1]["tool_call_id"] != call["id"] || r.Messages[1]["role"] != "tool" {
		t.Fatalf("correlation %+v", r.Messages)
	}
	if r.Messages[1]["content"] != `{"exit_code": 0, "output": "hello\naa_lang.json\n"}` {
		t.Fatalf("content %q", r.Messages[1]["content"])
	}
	unknown := NewCodexEventProjector().Project(codexCompleted(t, `{"type": "commandExecution", "id": "i", "command": "sleep 100", "cwd": "/tmp", "status": "failed", "aggregatedOutput": "[Command interrupted]\n"}`))
	if unknown.Messages[1]["content"] != "[Command interrupted]\n" {
		t.Fatalf("unknown exit keeps the legacy text shape, got %q", unknown.Messages[1]["content"])
	}
}

func TestCodexAgentMessageAndReasoningProjection(t *testing.T) {
	p := NewCodexEventProjector()
	r := p.Project(codexCompleted(t, `{"type": "agentMessage", "id": "x", "text": "hi there"}`))
	if r.FinalText == nil || *r.FinalText != "hi there" || r.IsToolIteration || !reflect.DeepEqual(r.Messages, []map[string]any{{"role": "assistant", "content": "hi there"}}) {
		t.Fatalf("agent message %+v", r)
	}
	if r1 := p.Project(codexCompleted(t, `{"type": "reasoning", "id": "r1", "summary": ["thinking..."], "content": ["step 1", "step 2"]}`)); len(r1.Messages) != 0 {
		t.Fatalf("reasoning alone produced %+v", r1.Messages)
	}
	first := p.Project(codexCompleted(t, `{"type": "agentMessage", "id": "a1", "text": "ok"}`)).Messages[0]
	if first["reasoning"] != "thinking...\nstep 1\nstep 2" {
		t.Fatalf("reasoning %q", first["reasoning"])
	}
	second := p.Project(codexCompleted(t, `{"type": "agentMessage", "id": "b", "text": "second"}`)).Messages[0]
	if _, ok := second["reasoning"]; ok {
		t.Fatal("reasoning attached twice")
	}
}

func TestCodexFileChangeMcpWebSearchProjection(t *testing.T) {
	fc := NewCodexEventProjector().Project(codexCompleted(t, `{"type": "fileChange", "id": "fc1", "status": "applied",
	 "changes": [{"kind": {"type": "add"}, "path": "/tmp/new.py", "diff": "+x"}, {"kind": {"type": "update"}, "path": "/tmp/old.py"}]}`))
	fn := fc.Messages[0]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "apply_patch" || fn["arguments"] != `{"changes": [{"kind": "add", "path": "/tmp/new.py"}, {"kind": "update", "path": "/tmp/old.py"}]}` {
		t.Fatalf("file change %+v", fn)
	}
	if fc.Messages[1]["content"] != "apply_patch status=applied, 2 change(s)" {
		t.Fatalf("file change content %q", fc.Messages[1]["content"])
	}
	mcp := NewCodexEventProjector().Project(codexCompleted(t, `{"type": "mcpToolCall", "id": "m1", "server": "obsidian", "tool": "search_notes",
	 "status": "completed", "arguments": {"query": "icebath"}, "result": {"content": [{"text": "found"}]}, "error": null}`))
	call := mcp.Messages[0]["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["name"] != "mcp.obsidian.search_notes" || call["id"] != "codex_mcp__obsidian__search_notes_m1" || mcp.Messages[1]["content"] != `{"content": [{"text": "found"}]}` {
		t.Fatalf("mcp %+v", mcp.Messages)
	}
	failed := NewCodexEventProjector().Project(codexCompleted(t, `{"type": "mcpToolCall", "id": "m2", "server": "x", "tool": "y", "status": "failed", "arguments": {}, "result": null, "error": {"code": -1, "message": "no"}}`))
	if failed.Messages[1]["content"] != `[error] {"code": -1, "message": "no"}` {
		t.Fatalf("mcp error %q", failed.Messages[1]["content"])
	}
	item := codexNote(t, `{"type": "webSearch", "id": "ws-1", "query": "Icebath docs"}`)
	ws := NewCodexEventProjector().Project(codexNote(t, `{"method": "item/completed", "params": {"item": {"type": "webSearch", "id": "ws-1", "query": "Icebath docs"}}}`))
	wsCall := ws.Messages[0]["tool_calls"].([]any)[0].(map[string]any)
	liveName := codexItemToToolName(item)
	if !ws.IsToolIteration || wsCall["function"].(map[string]any)["name"] != liveName || wsCall["id"] != ws.Messages[1]["tool_call_id"] || wsCall["id"] != codexStableCallID(item, liveName) {
		t.Fatalf("web search card id %+v", ws.Messages)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(wsCall["function"].(map[string]any)["arguments"].(string)), &args); err != nil || !reflect.DeepEqual(args, codexItemToArgs(item)) {
		t.Fatalf("web search args %v %v", args, err)
	}
	if ws.Messages[1]["content"] != `{"provider": "codex"}` {
		t.Fatalf("web search content %q", ws.Messages[1]["content"])
	}
	dyn := NewCodexEventProjector().Project(codexCompleted(t, `{"type": "dynamicToolCall", "id": "d1", "tool": "lookup", "arguments": "raw", "success": false}`))
	if dyn.Messages[0]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"arguments": "raw"}` || dyn.Messages[1]["content"] != "success=False" {
		t.Fatalf("dynamic %+v", dyn.Messages)
	}
}

func TestCodexUserAndOpaqueProjection(t *testing.T) {
	u := NewCodexEventProjector().Project(codexCompleted(t, `{"type": "userMessage", "id": "u1", "content": [{"type": "text", "text": "hello"}, {"type": "image", "url": "http://x/y"}, {"type": "text", "text": "world"}]}`))
	if u.Messages[0]["role"] != "user" || u.Messages[0]["content"] != "hello\nworld" {
		t.Fatalf("user %+v", u.Messages)
	}
	o := NewCodexEventProjector().Project(codexCompleted(t, `{"type": "plan", "id": "p1", "text": "do the thing"}`))
	if len(o.Messages) != 1 || o.Messages[0]["role"] != "assistant" || o.Messages[0]["content"] != `[codex plan] {"type": "plan", "id": "p1", "text": "do the thing"}` {
		t.Fatalf("opaque %+v", o.Messages)
	}
	if _, ok := o.Messages[0]["tool_calls"]; ok {
		t.Fatal("opaque item fabricated tool calls")
	}
}

func TestCodexProjectorHelpers(t *testing.T) {
	if codexDeterministicCallID("exec", "abc") != codexDeterministicCallID("exec", "abc") || codexDeterministicCallID("exec", "abc") == codexDeterministicCallID("exec", "xyz") {
		t.Fatal("call id not stable and distinct")
	}
	if a := codexDeterministicCallID("exec", ""); a != codexDeterministicCallID("exec", "") || !strings.HasPrefix(a, "codex_exec_") || len(a) != len("codex_exec_")+16 {
		t.Fatalf("missing id %q", a)
	}
	ordered := pyjson.NewObject()
	ordered.Set("b", 1)
	ordered.Set("a", 2)
	if codexFormatToolArgs(ordered) != codexFormatToolArgs(map[string]any{"a": 2, "b": 1}) || codexFormatToolArgs(ordered) != `{"a": 2, "b": 1}` {
		t.Fatalf("args not sorted %s", codexFormatToolArgs(ordered))
	}
}

func TestCodexToolItemsEmitAssistantThenTool(t *testing.T) {
	for _, item := range []string{
		`{"type": "commandExecution", "id": "c1", "command": "x", "cwd": "/", "status": "completed", "aggregatedOutput": "", "exitCode": 0}`,
		`{"type": "fileChange", "id": "f1", "status": "applied", "changes": []}`,
		`{"type": "mcpToolCall", "id": "m1", "server": "s", "tool": "t", "arguments": {}, "result": null, "error": null}`,
		`{"type": "dynamicToolCall", "id": "d1", "tool": "t", "arguments": {}, "contentItems": [], "success": true}`,
		`{"type": "webSearch", "id": "w1", "query": "q", "status": "completed"}`,
	} {
		msgs := NewCodexEventProjector().Project(codexCompleted(t, item)).Messages
		if len(msgs) != 2 || msgs[0]["role"] != "assistant" || msgs[1]["role"] != "tool" {
			t.Fatalf("%s gave %+v", item, msgs)
		}
	}
}
