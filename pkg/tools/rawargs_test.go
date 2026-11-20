package tools

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestCallRefCarriesTheEmittedArgumentText(t *testing.T) {
	raw := `{"scene": "castle", "notes": "y"}`
	p := &ParsedCall{ToolCall: state.ToolCall{ID: "c1", Function: state.ToolCallFunction{Name: "mcp__srv__paint", Arguments: raw}},
		Name: "mcp__srv__paint", Args: map[string]any{"scene": "castle", "notes": "y"}}
	if got := p.Ref("t").RawArgs; got != raw {
		t.Fatalf("direct call raw %q", got)
	}
	inner := map[string]any{"zeta": "1", "alpha": "2"}
	for _, wrapper := range []string{
		`{"name": "mcp__srv__paint", "arguments": {"zeta": "1", "alpha": "2"}}`,
		`{"name": "mcp__srv__paint", "arguments": "{\"zeta\": \"1\", \"alpha\": \"2\"}"}`,
		`{"calls": [{"name": "mcp__srv__paint", "arguments": {"zeta": "1", "alpha": "2"}}]}`,
	} {
		p := &ParsedCall{ToolCall: state.ToolCall{ID: "c2", Function: state.ToolCallFunction{Name: ToolCallName, Arguments: wrapper}},
			Name: "mcp__srv__paint", Args: inner}
		if got := p.Ref("t").RawArgs; got != `{"zeta": "1", "alpha": "2"}` {
			t.Fatalf("bridged call raw %q from %s", got, wrapper)
		}
	}
	if got := rawArgsFor([]byte(`{"other": 1}`), inner); got != "" {
		t.Fatalf("absent args %q", got)
	}
}
