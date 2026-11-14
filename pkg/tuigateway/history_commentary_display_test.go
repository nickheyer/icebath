package tuigateway

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Assistant row with a public commentary item, a private analysis item and a tool call
func commentaryRow(text string) map[string]any {
	return map[string]any{
		"role": "assistant", "content": "",
		"reasoning":         "Private summary.\n\n" + text,
		"reasoning_content": "Private summary.\n\n" + text,
		"codex_message_items": []any{
			map[string]any{"type": "message", "role": "assistant", "phase": "commentary",
				"content": []any{map[string]any{"type": "output_text", "text": text}}},
			map[string]any{"type": "message", "role": "assistant", "phase": "analysis",
				"content": []any{map[string]any{"type": "output_text", "text": "private analysis"}}},
		},
		"tool_calls": []any{map[string]any{"id": "call-1", "type": "function",
			"function": map[string]any{"name": "terminal", "arguments": "{}"}}},
		"timestamp": int64(1),
	}
}

func TestGatewayHistoryUsesSameDisplayProjection(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	row := commentaryRow("I will inspect the files.")
	rows, err := HistoryToMessages(context.Background(), []any{row}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	projected, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatal("projected row is not a dict")
	}
	if !reflect.DeepEqual(projected["display_commentary"], []any{"I will inspect the files."}) {
		t.Fatalf("display_commentary = %#v", projected["display_commentary"])
	}
	if projected["display_reasoning"] != "Private summary." {
		t.Fatalf("display_reasoning = %#v", projected["display_reasoning"])
	}
	if !reflect.DeepEqual(projected["codex_message_items"], row["codex_message_items"]) {
		t.Fatal("codex_message_items changed")
	}
	if !strings.HasSuffix(row["reasoning"].(string), "I will inspect the files.") {
		t.Fatal("source reasoning changed")
	}
}
