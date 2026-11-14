package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Sessions made by two turns are listed, renamed, exported, resumed by id and by title, and deleted
func TestScenarioSessionsListRenameExportResumeDelete(t *testing.T) {
	s := Start(t, Setup{Model: []any{
		fakellm.Text{Text: "first answer"}, fakellm.Text{Text: "second answer"},
		fakellm.Text{Text: "third answer"}, fakellm.Text{Text: "fourth answer"},
	}})
	a := s.Run("chat", "-q", "alpha question", "-Q").Exit(0).Match(`(?m)^session_id: (\S+)$`)[1]
	b := s.Run("chat", "-q", "beta question", "-Q").Exit(0).Match(`(?m)^session_id: (\S+)$`)[1]
	if a == b {
		s.Failf("two separate one shot turns share the session id %s", a)
	}
	if n := len(s.LastMessages()); n != 2 {
		s.Failf("the second one shot turn sent %d messages, want the system prompt and its own question", n)
	}
	s.Run("sessions", "list").Exit(0).Has("Last Active", "ID", a, b)

	s.Run("sessions", "rename", a, "My", "Title").Exit(0).Has("Session '" + a + "' renamed to: My Title")
	s.SameJSON(s.Rows("state.db", "SELECT title FROM sessions WHERE id = ?", a), `[{"title":"My Title"}]`)
	s.Run("sessions", "list").Exit(0).Has("Title", "My Title")
	s.Run("sessions", "rename", a, " ").Exit(1).Has("Error: title cannot be empty or whitespace-only.")
	s.Run("sessions", "rename", "no-such-session", "x").Exit(1).Has("No session 'no-such-session'. Run: icebath sessions list to find the id.")

	exported := s.Run("sessions", "export", "--session-id", a, "-").Exit(0)
	s.HasJSON(strings.SplitN(exported.Stdout, "\n", 2)[0], `{"id": "`+a+`", "title": "My Title", "messages": [
		{"role": "user", "content": "alpha question"}, {"role": "assistant", "content": "first answer"}]}`)
	out := filepath.Join(s.Work, "exports")
	s.Run("sessions", "export", "--session-id", a, "--format", "md", out).Exit(0).Has("Exported 1 session (2 messages) to " + out + "/")
	files, _ := filepath.Glob(filepath.Join(out, "*.md"))
	if len(files) != 1 {
		s.Failf("markdown export wrote %d files into %s, want 1", len(files), out)
	}
	if md, _ := os.ReadFile(files[0]); !strings.Contains(string(md), "alpha question") || !strings.Contains(string(md), "first answer") {
		s.Failf("markdown export lacks the conversation\n%s", clip(string(md)))
	}
	s.Run("sessions", "export", "--format", "jsonl").Exit(1).Has("JSONL export requires an output path (use - for stdout).")

	resumed := s.Run("chat", "--resume", a, "-q", "gamma question", "-Q").Exit(0)
	resumed.Has("↻ Resumed session "+a, "(1 user message, 2 total messages)", "session_id: "+a)
	if resumed.Stdout != "third answer\n" {
		s.Failf("a resumed quiet turn prints only the answer on standard output, got %q", resumed.Stdout)
	}
	s.HasJSON(s.LastMessages()[1:], `[{"role": "user", "content": "alpha question"}, {"role": "assistant", "content": "first answer"},
		{"role": "user", "content": "gamma question"}]`)
	byTitle := s.Run("chat", "--continue", "My Title", "-q", "delta question", "-Q").Exit(0)
	byTitle.Has("session_id: "+a, "(2 user messages, 4 total messages)")
	if got := len(s.Rows("state.db", "SELECT id FROM messages WHERE session_id = ?", a)); got != 6 {
		s.Failf("the resumed session stores %d messages, want 6", got)
	}

	s.Run("sessions", "delete", a).Exit(1).Has("Delete session '"+a+"' and all its messages? [y/N]",
		"No answer on a non-interactive stdin; nothing was changed. Pass --yes to run it unattended.")
	s.RunStep(Step{Args: []string{"sessions", "delete", a}, Stdin: "n\n"}).Exit(0).Has("Cancelled.")
	s.Run("sessions", "delete", a, "--yes").Exit(0).Has("Deleted session '" + a + "'.")
	s.Run("sessions", "delete", a, "--yes").Exit(1).Has("No session '" + a + "'. Run: icebath sessions list to find the id.")
	s.Run("sessions", "list").Exit(0).Has(b).Lacks(a)
	if left := s.Rows("state.db", "SELECT id FROM messages WHERE session_id = ?", a); len(left) != 0 {
		s.Failf("%d messages of the deleted session remain", len(left))
	}
}
