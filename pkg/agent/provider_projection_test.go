package agent

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func projectedRows() []any {
	return []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "acp_s1_t1", "type": "function",
			"function": map[string]any{"name": "acpagent_edit", "arguments": `{"path": "main.py"}`},
		}}},
		map[string]any{"role": "tool", "tool_call_id": "acp_s1_t1", "name": "acpagent_edit", "content": "1 file changed"},
	}
}

func projectionAgent(iters int) *AIAgent {
	return &AIAgent{provider: "acp-agent", itersSinceSkill: iters}
}

func TestProjectedRowsAreAppendedAndTheNudgeTicks(t *testing.T) {
	a := projectionAgent(0)
	messages := []*state.Message{{Role: "user", Content: "edit main.py"}}
	spliced := spliceProviderProjection(a, map[string]any{ProjectedMessagesAttr: projectedRows(), ProviderToolIterationsAttr: 1}, &messages)
	if spliced != 2 {
		t.Fatalf("spliced %d", spliced)
	}
	if messages[1].ToolCalls[0].Function.Name != "acpagent_edit" || messages[2].Content != "1 file changed" {
		t.Fatalf("rows %+v %+v", messages[1], messages[2])
	}
	if a.itersSinceSkill != 1 {
		t.Fatalf("iters %d", a.itersSinceSkill)
	}
}

func TestRowsAreStampedLikeEveryOtherLiveTranscriptAppend(t *testing.T) {
	var messages []*state.Message
	spliceProviderProjection(projectionAgent(0), map[string]any{ProjectedMessagesAttr: projectedRows()}, &messages)
	for _, m := range messages {
		if m.Timestamp == 0 {
			t.Fatalf("unstamped %+v", m)
		}
	}
}

func TestIterationsAccumulateAcrossCalls(t *testing.T) {
	a := projectionAgent(2)
	var messages []*state.Message
	spliceProviderProjection(a, map[string]any{ProviderToolIterationsAttr: 3}, &messages)
	if a.itersSinceSkill != 5 {
		t.Fatalf("iters %d", a.itersSinceSkill)
	}
}

func TestOrdinaryProviderResponseIsANoOp(t *testing.T) {
	a := projectionAgent(1)
	messages := []*state.Message{{Role: "user", Content: "hi"}}
	if n := spliceProviderProjection(a, map[string]any{"choices": []any{}}, &messages); n != 0 {
		t.Fatalf("spliced %d", n)
	}
	if !reflect.DeepEqual(messages, []*state.Message{{Role: "user", Content: "hi"}}) || a.itersSinceSkill != 1 {
		t.Fatalf("changed %+v %d", messages, a.itersSinceSkill)
	}
}

func TestGarbageAttributesCannotBreakTheTurn(t *testing.T) {
	a := projectionAgent(0)
	var messages []*state.Message
	if n := spliceProviderProjection(a, map[string]any{ProjectedMessagesAttr: "not-a-list", ProviderToolIterationsAttr: "lots"}, &messages); n != 0 {
		t.Fatalf("spliced %d", n)
	}
	if len(messages) != 0 || a.itersSinceSkill != 0 {
		t.Fatalf("changed %+v %d", messages, a.itersSinceSkill)
	}
	junk := []any{map[string]any{"role": "tool", "content": "ok"}, "junk", nil}
	if n := spliceProviderProjection(a, map[string]any{ProjectedMessagesAttr: junk}, &messages); n != 1 {
		t.Fatalf("spliced %d", n)
	}
	if len(messages) != 1 || messages[0].Role != "tool" {
		t.Fatalf("rows %+v", messages)
	}
}
