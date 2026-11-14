package scenario

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// A one shot turn blocks a dangerous command nobody can approve and --yolo runs it
func TestScenarioApprovalsSingleQueryDenyAndYolo(t *testing.T) {
	remove := fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "rm -rf victim"}}
	s := Start(t, Setup{
		Model: []any{remove, fakellm.Text{Text: "Blocked."}, remove, fakellm.Text{Text: "Removed."}},
		Work:  map[string]string{"victim/keep.txt": "precious\n"},
	})
	s.Run("chat", "-q", "Delete the victim directory", "-Q").Exit(0).Has("Blocked.")
	blocked := s.ToolResult("terminal")
	if !s.Exists("${WORK}/victim/keep.txt") {
		s.Failf("a one shot turn ran rm -rf without approval, tool result %s", clip(blocked))
	}
	if !strings.Contains(blocked, "BLOCKED") {
		s.Failf("the model was not told the command was blocked: %s", clip(blocked))
	}

	s.Run("chat", "--yolo", "-q", "Delete the victim directory", "-Q").Exit(0).Has("Removed.")
	if s.Exists("${WORK}/victim") {
		s.Failf("--yolo did not run the command, tool result %s", clip(s.ToolResult("terminal")))
	}
	s.HasJSON(s.ToolResult("terminal"), `{"exit_code": 0}`)
}

// A command matching an approvals.deny pattern never runs, with or without --yolo
func TestScenarioApprovalsDenyListBlocksUnderYolo(t *testing.T) {
	write := fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "echo wipe-marker > guarded.txt"}}
	s := Start(t, Setup{
		Model: []any{write, fakellm.Text{Text: "Denied."}, write, fakellm.Text{Text: "Denied again."}},
		Extra: "approvals:\n  deny:\n    - \"*wipe-marker*\"\n",
		Work:  map[string]string{"guarded.txt": "intact\n"},
	})
	s.SameJSON(s.Run("config", "get", "approvals.deny", "--json").Exit(0).Stdout, `["*wipe-marker*"]`)
	for _, flags := range [][]string{{"-q"}, {"--yolo", "-q"}} {
		s.Run(append(append([]string{"chat"}, flags...), "Overwrite the guarded file", "-Q")...).Exit(0).Has("Denied")
		if got := s.Read("${WORK}/guarded.txt"); got != "intact\n" {
			s.Failf("a command matching approvals.deny ran with %v, guarded.txt holds %q, tool result %s", flags, got, clip(s.ToolResult("terminal")))
		}
		if denied := s.ToolResult("terminal"); !strings.Contains(denied, "BLOCKED") {
			s.Failf("the model was not told the deny pattern blocked the command with %v: %s", flags, clip(denied))
		}
	}
}
