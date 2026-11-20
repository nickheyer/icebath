package tuiserver

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestALaterTurnOverAnUnchangedPrefixReportsACompleteReceipt(t *testing.T) {
	hz := newTurnHarness(t, []any{fakellm.Text{Text: "one"}, fakellm.Text{Text: "two"}, fakellm.Text{Text: "three"}}, "")
	for _, text := range []string{"first", "second", "third"} {
		hz.turn(t, text)
	}
	completes := hz.events("message.complete")
	want := []map[string]any{
		{"row_ids": []any{1, 2}, "complete": true, "user_row_id": 1, "user_row_ids": []any{1}, "final_assistant_row_id": 2},
		{"row_ids": []any{3, 4}, "complete": true, "user_row_id": 3, "user_row_ids": []any{3}, "final_assistant_row_id": 4},
		{"row_ids": []any{5, 6}, "complete": true, "user_row_id": 5, "user_row_ids": []any{5}, "final_assistant_row_id": 6},
	}
	if len(completes) != len(want) {
		t.Fatalf("completions %v", completes)
	}
	for i, payload := range completes {
		if !pyjson.Equal(payload["persisted_turn"], want[i]) {
			t.Errorf("turn %d receipt %v", i+1, payload["persisted_turn"])
		}
	}
}
