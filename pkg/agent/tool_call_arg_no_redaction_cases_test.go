package agent

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestPgpasswordPreservedVerbatim(t *testing.T) {
	a := kwargsTestAgent(t, "custom", "http://127.0.0.1:9/v1", "test-model", "chat_completions")
	key := home.Key(home.Dir(t.Context()))
	tools.NoteConfigPolicy(key, true)
	args := `{"command": "PGPASSWORD='honchorulez' psql -h 127.0.0.1"}`
	msg := &state.Message{Role: "assistant", Content: "ok",
		ToolCalls: []state.ToolCall{{ID: "call_1", Type: "function", Function: state.ToolCallFunction{Name: "terminal", Arguments: args}}}}
	row := a.buildAssistantMessage(&provider.Response{Message: msg}, "tool_calls")
	calls, _ := row["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool calls %v", row["tool_calls"])
	}
	call, _ := pyjson.Dict(calls[0])
	fn, _ := pyjson.Dict(call["function"])
	got, _ := fn["arguments"].(string)
	if got != args {
		t.Fatalf("arguments %q want %q", got, args)
	}
	if !strings.Contains(got, "honchorulez") || strings.Contains(got, "***") {
		t.Fatalf("credential was masked in the replayed tool call: %q", got)
	}
}
