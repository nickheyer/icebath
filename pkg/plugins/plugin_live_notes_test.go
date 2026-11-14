package plugins

import (
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

const (
	liveNote    = "[System note: plugin blender was installed and is live in this chat now.]"
	oldLiveNote = "[System note: plugin blender (an older install) is live.]"
	liveOutput  = `<untrusted_tool_result>{"results": ["a web page about Blender scripting"]}</untrusted_tool_result>`
)

// test_a_plugin_installed_during_a_tool_is_told_in_its_result_and_not_again_next_turn at the module seam
func TestLiveNoteRidesTheToolResultOnce(t *testing.T) {
	notes := &PluginLiveNotes{}
	QueuePluginNotice(notes, "blender", oldLiveNote)
	QueuePluginNotice(notes, "blender", liveNote)
	row := &state.Message{Role: "tool", ToolCallID: "c1", Content: liveOutput}
	AttachToToolMessage(notes, row)
	content := row.Content.(string)
	if !strings.HasSuffix(content, "</untrusted_tool_result>\n\n"+liveNote) || strings.Contains(content, oldLiveNote) {
		t.Fatalf("tool row %q", content)
	}
	if next := PopPluginNotices(notes); next != "" {
		t.Fatalf("told twice %q", next)
	}
	empty := &state.Message{Role: "tool", ToolCallID: "c2", Content: "x"}
	AttachToToolMessage(notes, empty)
	if empty.Content != "x" {
		t.Fatalf("nothing queued must leave the row %v", empty.Content)
	}
}

// test_a_notice_no_tool_result_carried_rides_the_next_user_message_once plus plugin order and list content
func TestLiveNoteRidesTheNextUserMessageOnce(t *testing.T) {
	notes := &PluginLiveNotes{}
	QueuePluginNotice(notes, "blender", liveNote)
	QueuePluginNotice(notes, "lsp:a.go", "lsp note")
	QueuePluginNotice(notes, "blender", "newer")
	if got := PopPluginNotices(notes); got != "lsp note\n\nnewer" {
		t.Fatalf("first turn %q", got)
	}
	if got := PopPluginNotices(notes); got != "" {
		t.Fatalf("second turn %q", got)
	}
	QueuePluginNotice(notes, "blender", liveNote)
	row := &state.Message{Role: "tool", ToolCallID: "c1", Content: []any{map[string]any{"type": "text", "text": "out"}}}
	AttachToToolMessage(notes, row)
	parts := row.Content.([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["text"] != liveNote {
		t.Fatalf("list content %v", parts)
	}
	none := &state.Message{Role: "tool", ToolCallID: "c3"}
	QueuePluginNotice(notes, "blender", liveNote)
	AttachToToolMessage(notes, none)
	if none.Content != "\n\n"+liveNote {
		t.Fatalf("empty content %q", none.Content)
	}
}

// test_the_notice_survives_the_turn_budget_spilling_its_tool_result at the module seam
func TestLiveNoteSurvivesTheTurnBudgetSpill(t *testing.T) {
	notes := &PluginLiveNotes{}
	QueuePluginNotice(notes, "blender", oldLiveNote)
	QueuePluginNotice(notes, "blender", liveNote)
	big := strings.Repeat("x", 20000)
	spilled := &state.Message{Role: "tool", ToolCallID: "c1", Content: big}
	kept := &state.Message{Role: "tool", ToolCallID: "c2", Content: "short"}
	AttachToToolMessage(notes, spilled)
	QueuePluginNotice(notes, "other", "other note")
	AttachToToolMessage(notes, kept)
	spilled.Content = "[preview] " + big[:100]
	KeepAfterTurnBudget(notes, []*state.Message{spilled, kept})
	if got := spilled.Content.(string); strings.Contains(got, big) || !strings.HasSuffix(got, liveNote) {
		t.Fatalf("spilled row %q", got)
	}
	if got := kept.Content.(string); got != "short\n\nother note" {
		t.Fatalf("kept row %q", got)
	}
	KeepAfterTurnBudget(notes, []*state.Message{spilled})
	if strings.Count(spilled.Content.(string), liveNote) != 1 {
		t.Fatal("in batch notes must clear after one budget pass")
	}
}

// The install thread queues while the agent thread delivers
func TestLiveNotesQueueAcrossGoroutines(t *testing.T) {
	notes := &PluginLiveNotes{}
	var wg sync.WaitGroup
	got := make(chan string, 64)
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); QueuePluginNotice(notes, "blender", liveNote) }()
		go func() { defer wg.Done(); got <- PopPluginNotices(notes) }()
	}
	wg.Wait()
	close(got)
	told := 0
	for s := range got {
		told += strings.Count(s, liveNote)
	}
	told += strings.Count(PopPluginNotices(notes), liveNote)
	if told < 1 || told > 32 {
		t.Fatalf("told %d", told)
	}
}
