package agent

import (
	"context"
	"maps"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestRepairOwnerSkipsListContentDirectly(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	apiMessages := []map[string]any{
		{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "hi"}}},
		{"role": "assistant", "content": ""},
		{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1"}}},
		{"role": "user", "content": "trailing turn keeps the above non-final"},
	}
	out := repairEmptyNonFinalMessages(context.Background(), "", apiMessages)
	if !reflect.DeepEqual(out[0]["content"], []any{map[string]any{"type": "text", "text": "hi"}}) {
		t.Fatalf("list content changed: %v", out[0]["content"])
	}
	if out[1]["content"] != InterruptedPlaceholder || out[2]["content"] != "" {
		t.Fatalf("repair gave %v and %v", out[1]["content"], out[2]["content"])
	}
	if apiMessages[1]["content"] != "" {
		t.Fatal("repair must copy on write")
	}
}

func TestEmptyNonFinalSanitizerStillRepairsUnmarkedEmptyAssistant(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	rows := []map[string]any{
		{"role": "user", "content": "start"},
		{"role": "assistant", "content": "", "display_kind": "hidden"},
		{"role": "user", "content": "correction"},
	}
	healed := repairEmptyNonFinalMessages(context.Background(), "", rows)
	if healed[1]["role"] != "assistant" || healed[1]["content"] != InterruptedPlaceholder {
		t.Fatalf("assistant not healed: %v", healed[1])
	}
	if rows[1]["content"] != "" {
		t.Fatal("the durable list must not be mutated")
	}
}

func TestPersistedSteerRowIsNeverMergedWithTheNextPrompt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	steer := SteerUserRow("focus on error handling")
	before := maps.Clone(steer)
	rows := []map[string]any{steer, {"role": "user", "content": "next question"}}
	repairs, err := state.RepairMessageSequence(&rows)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(steer, before) || repairs != 0 {
		t.Fatalf("steer row changed %v or repairs %d", steer, repairs)
	}
	if len(rows) != 2 || rows[0]["role"] != "user" || rows[1]["role"] != "user" || rows[1]["content"] != "next question" {
		t.Fatalf("merged %v", rows)
	}
}
