package scenario

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// One turn writes, reads, patches and searches a file and runs a command, each result reaching the model
func TestScenarioToolCallsFilesAndTerminal(t *testing.T) {
	s := Start(t, Setup{Model: []any{
		fakellm.ToolCall{Name: "write_file", Args: map[string]any{"path": "notes.txt", "content": "alpha line\nbeta line\n"}},
		fakellm.ToolCall{Name: "read_file", Args: map[string]any{"path": "notes.txt"}},
		fakellm.ToolCall{Name: "patch", Args: map[string]any{"path": "notes.txt", "old_string": "beta line", "new_string": "gamma line"}},
		fakellm.ToolCall{Name: "search_files", Args: map[string]any{"pattern": "gamma", "path": "."}},
		fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "echo terminal-says-$((6*7)) && cat notes.txt"}},
		fakellm.ToolCall{Name: "read_file", Args: map[string]any{"path": "absent.txt"}},
		fakellm.Text{Text: "All five tools ran."},
	}})
	r := s.Run("chat", "-q", "Exercise the file tools", "-Q").Exit(0)
	r.Has("notes.txt", "+alpha line", "+beta line", "-beta line", "+gamma line")
	if !strings.HasSuffix(r.Stdout, "All five tools ran.\n") {
		s.Failf("standard output ends with the final answer, got %q", r.Stdout)
	}
	if got := s.Read("${WORK}/notes.txt"); got != "alpha line\ngamma line\n" {
		s.Failf("notes.txt after write and patch holds %q", got)
	}
	var results []map[string]any
	for _, m := range s.LastMessages() {
		if Dig(m, "role") == "tool" {
			content, _ := Dig(m, "content").(string)
			parsed, ok := s.value(content).(map[string]any)
			if !ok {
				s.Failf("tool result is not a JSON object: %s", content)
			}
			results = append(results, parsed)
		}
	}
	if len(results) != 6 {
		s.Failf("the last request carries %d tool results, want 6", len(results))
	}
	s.HasJSON(results[0], `{"bytes_written": 21}`)
	s.HasJSON(results[1], `{"total_lines": 2, "content": "1|alpha line\n2|beta line"}`)
	s.HasJSON(results[2], `{"success": true}`)
	diff, _ := results[2]["diff"].(string)
	if !strings.Contains(diff, "-beta line") || !strings.Contains(diff, "+gamma line") {
		s.Failf("patch result diff lacks the removed and added line: %q", diff)
	}
	s.HasJSON(results[3], `{"total_count": 1, "matches": [{"line": 2, "content": "gamma line"}]}`)
	if path, _ := Dig(results[3], "matches", 0, "path").(string); !strings.HasSuffix(path, "notes.txt") {
		s.Failf("search_files match does not name notes.txt: %v", results[3])
	}
	s.HasJSON(results[4], `{"exit_code": 0, "error": null}`)
	if out, _ := results[4]["output"].(string); !strings.Contains(out, "terminal-says-42") || !strings.Contains(out, "gamma line") {
		s.Failf("terminal output lacks the command output: %q", out)
	}
	if msg, _ := results[5]["error"].(string); !strings.Contains(msg, "absent.txt") {
		s.Failf("reading a missing file reports an error naming it, got %v", results[5])
	}
}
