package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Deferred and skill tools find an earlier session, keep a task list, and create, list and read a skill
func TestScenarioToolCallsSearchTodoAndSkills(t *testing.T) {
	const skill = "---\nname: scenario-notes\ndescription: Notes kept for the scenario run.\n---\n\n# Scenario Notes\n\nKeep notes short.\n"
	deferred := func(name string, args map[string]any) fakellm.ToolCall {
		return fakellm.ToolCall{Name: "tool_call", Args: map[string]any{"calls": []any{map[string]any{"name": name, "arguments": args}}}}
	}
	s := Start(t, Setup{Model: []any{
		fakellm.Text{Text: "Stored the word."},
		deferred("session_search", map[string]any{"query": "pineapple"}),
		deferred("todo_list", map[string]any{"todos": []any{map[string]any{"id": "1", "content": "Write tests", "status": "pending"}}}),
		fakellm.ToolCall{Name: "skill_manage", Args: map[string]any{"operations": []any{map[string]any{"action": "create", "name": "scenario-notes", "content": skill}}}},
		fakellm.ToolCall{Name: "skills_list", Args: map[string]any{}},
		fakellm.ToolCall{Name: "skill_view", Args: map[string]any{"name": "scenario-notes"}},
		fakellm.Text{Text: "Done."},
	}})
	first := s.Run("chat", "-q", "The launch code word is pineapple", "-Q").Exit(0).Match(`(?m)^session_id: (\S+)$`)[1]
	s.Run("chat", "-q", "Find the code word, plan the work and write a skill", "-Q").Exit(0).Has("Done.")

	var results []string
	for _, m := range s.LastMessages() {
		if Dig(m, "role") == "tool" {
			text, _ := Dig(m, "content").(string)
			results = append(results, text)
		}
	}
	if len(results) != 5 {
		s.Failf("the second turn carried %d tool results, want 5", len(results))
	}
	if !strings.Contains(results[0], first) || !strings.Contains(results[0], "pineapple") {
		s.Failf("session_search did not return the earlier session %s with its text: %s", first, clip(results[0]))
	}
	if !strings.Contains(results[1], "Write tests") || !strings.Contains(results[1], "pending") {
		s.Failf("todo_list did not return the stored item: %s", clip(results[1]))
	}
	var stored []string
	filepath.WalkDir(filepath.Join(s.Home, "skills"), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, "scenario-notes/SKILL.md") {
			stored = append(stored, path)
		}
		return nil
	})
	if len(stored) != 1 || s.Read(stored[0]) != skill {
		s.Failf("skill_manage create stored %v, want one scenario-notes/SKILL.md with the given text\n%s", stored, clip(results[2]))
	}
	if !strings.Contains(results[3], "scenario-notes") {
		s.Failf("skills_list lacks the created skill: %s", clip(results[3]))
	}
	if !strings.Contains(results[4], "Keep notes short.") {
		s.Failf("skill_view did not return the skill text: %s", clip(results[4]))
	}
	s.Run("skills", "list").Exit(0).Has("scenario-notes")
}
