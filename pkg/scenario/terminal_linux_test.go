package scenario

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// The interactive CLI on a terminal answers a typed question, lists its commands and quits with a resume hint
func TestScenarioInteractiveCLIOnATerminal(t *testing.T) {
	s := Start(t, Setup{
		Model: []any{fakellm.Text{Text: "Hello from the terminal session.", ChunkChars: 4}},
		Extra: "display:\n  interface: cli\ntelemetry:\n  shared_metrics:\n    enabled: false\n    send: false\n    offer_version: 2\nupdates:\n  check: false\n",
	})
	tm := s.Terminal()
	tm.Await("fake-model")
	tm.Line("Say hello")
	tm.Await("Hello from the terminal session")
	tm.Line("/help")
	tm.Await("(?i)/quit")
	tm.Line("/quit")
	tm.Await("Resume this session with")
	if code := tm.Wait(); code != 0 {
		s.Failf("the interactive CLI exited %d after /quit\n%s", code, clip(tm.Text()))
	}
	s.SameJSON(s.Rows("state.db", "SELECT role, content FROM messages ORDER BY id"),
		`[{"role":"user","content":"Say hello"},{"role":"assistant","content":"Hello from the terminal session."}]`)
}

// Slash output printed line by line lands under its header above the prompt, every line from the first column
func TestScenarioSlashOutputLandsAboveThePromptOnATerminal(t *testing.T) {
	s := Start(t, Setup{
		Extra: "display:\n  interface: cli\ntelemetry:\n  shared_metrics:\n    enabled: false\n    send: false\n    offer_version: 2\nupdates:\n  check: false\n",
	})
	tm := s.Terminal()
	tm.Await("fake-model")
	tm.Line("/kanban")
	tm.Await("(?m)^\\S+ +/kanban *\\n/kanban — manage the shared task board\\. *\\n *\\nCommon subcommands: *\\n" +
		"  `list` \\(alias `ls`\\)   List tasks on the current board *\\n  `show <id>`           Task details \\+ comments \\+ events *\\n" +
		"  `stats`               Per-status / per-assignee counts *$")
	tm.Await("(?m)^Run `/kanban <subcommand> -h` for arguments\\. Read-only commands are safe while an agent is running\\. *\\n─+ *\\n❯")
	tm.Line("/worktree list")
	tm.Await("(?m)^\\S+ +/worktree list *\\n")
	tm.Line("/quit")
	tm.Await("Goodbye")
	if code := tm.Wait(); code != 0 {
		s.Failf("the interactive CLI exited %d after /quit\n%s", code, clip(tm.Text()))
	}
}
