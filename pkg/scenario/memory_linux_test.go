package scenario

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// The memory tool stores notes in MEMORY.md and USER.md, later sessions read them in the system prompt
func TestScenarioMemoryToolWritesAndNextSessionReads(t *testing.T) {
	s := Start(t, Setup{Model: []any{
		fakellm.ToolCall{Name: "memory", Args: map[string]any{"action": "add", "target": "memory", "content": "The deploy key lives in vault slot 7"}},
		fakellm.ToolCall{Name: "memory", Args: map[string]any{"action": "add", "target": "user", "content": "Prefers answers under three lines"}},
		fakellm.Text{Text: "Both notes are saved."},
		fakellm.Text{Text: "Second session answer."},
		fakellm.ToolCall{Name: "memory", Args: map[string]any{"action": "replace", "target": "memory", "old_text": "vault slot 7", "content": "vault slot 8"}},
		fakellm.ToolCall{Name: "memory", Args: map[string]any{"action": "patch", "target": "memory", "old_text": "vault slot 7", "content": "vault slot 9"}},
		fakellm.Text{Text: "Updated."},
	}})
	s.Run("memory", "status").Exit(0).Has("Memory status", "Built-in (MEMORY.md / USER.md):", "Memory injection:", "User profile:", "Memory tool:", "(none — built-in only)")

	s.Run("chat", "-q", "Remember where the deploy key is and how I like answers", "-Q").Exit(0).Has("Both notes are saved.")
	s.HasJSON(s.ToolResult("memory"), `{"success": true}`)
	if text := s.Read("memories/MEMORY.md"); !strings.Contains(text, "The deploy key lives in vault slot 7") {
		s.Failf("memories/MEMORY.md lacks the note\n%s", text)
	}
	if text := s.Read("memories/USER.md"); !strings.Contains(text, "Prefers answers under three lines") {
		s.Failf("memories/USER.md lacks the note\n%s", text)
	}

	s.Run("chat", "-q", "What do you know about me", "-Q").Exit(0).Has("Second session answer.")
	system, _ := Dig(s.LastMessages(), 0, "content").(string)
	if !strings.Contains(system, "The deploy key lives in vault slot 7") || !strings.Contains(system, "Prefers answers under three lines") {
		s.Failf("the system prompt of the next session lacks the stored notes")
	}

	s.Run("chat", "-q", "The key moved to slot 9", "-Q").Exit(0).Has("Updated.")
	var results []string
	for _, m := range s.LastMessages() {
		if Dig(m, "role") == "tool" {
			text, _ := Dig(m, "content").(string)
			results = append(results, text)
		}
	}
	if len(results) != 2 {
		s.Failf("the third turn carried %d tool results, want 2", len(results))
	}
	s.HasJSON(results[0], `{"success": false}`)
	if !strings.Contains(results[0], "'replace' is not available; use 'patch'.") || !strings.Contains(results[0], "Nothing was written.") {
		s.Failf("the retired replace action answered %s", results[0])
	}
	s.HasJSON(results[1], `{"success": true}`)
	if text := s.Read("memories/MEMORY.md"); !strings.Contains(text, "The deploy key lives in vault slot 9") || strings.Contains(text, "vault slot 7") {
		s.Failf("memories/MEMORY.md after the patch holds\n%s", text)
	}
}
