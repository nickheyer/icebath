package scenario

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Asserts the last terminal call was refused by the named approvals.deny pattern and told the model so
func approvalsDenyBlocked(s *S, pattern, when string) {
	result := s.ToolResult("terminal")
	s.HasJSON(result, `{"exit_code": -1, "status": "blocked"}`)
	want := "BLOCKED: this command matches the user-defined deny rule '" + pattern + "' (approvals.deny in config.yaml)"
	if !strings.Contains(result, want) || !strings.Contains(result, "not even with --yolo, /yolo, or approvals.mode=off") {
		s.Failf("%s: the model was not told which deny rule blocked the command: %s", when, clip(result))
	}
}

// approvals.deny lets other commands run and blocks a match under yolo, after a config edit and with mode off
func TestScenarioApprovalsDenyAllowedDeniedYoloAndModeOff(t *testing.T) {
	call := func(command string) fakellm.ToolCall {
		return fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": command}}
	}
	s := Start(t, Setup{
		Model: []any{
			call("echo allowed > allowed.txt"), fakellm.Text{Text: "Allowed ran."},
			call("echo wipe-marker > guarded.txt"), fakellm.Text{Text: "Denied."},
			call("echo wipe-marker > guarded.txt"), fakellm.Text{Text: "Denied under yolo."},
			call("echo yolo-ok > allowed.txt"), fakellm.Text{Text: "Allowed under yolo."},
			call("echo late-marker > late.txt"), fakellm.Text{Text: "Late rule denied."},
			call("rm -rf victim"), fakellm.Text{Text: "Removed with mode off."},
			call("echo wipe-marker > guarded.txt"), fakellm.Text{Text: "Denied with mode off."},
		},
		Extra: "approvals:\n  deny:\n    - \"*wipe-marker*\"\n",
		Work:  map[string]string{"guarded.txt": "intact\n", "victim/keep.txt": "precious\n"},
	})

	s.Run("chat", "-q", "Write the allowed file", "-Q").Exit(0).Has("Allowed ran.")
	s.HasJSON(s.ToolResult("terminal"), `{"exit_code": 0, "error": null}`)
	if got := s.Read("${WORK}/allowed.txt"); got != "allowed\n" {
		s.Failf("a command matching no deny pattern did not run, allowed.txt holds %q", got)
	}

	s.Run("chat", "-q", "Overwrite the guarded file", "-Q").Exit(0).Has("Denied.")
	approvalsDenyBlocked(s, "*wipe-marker*", "plain turn")
	s.Run("chat", "--yolo", "-q", "Overwrite the guarded file", "-Q").Exit(0).Has("Denied under yolo.")
	approvalsDenyBlocked(s, "*wipe-marker*", "yolo turn")
	if got := s.Read("${WORK}/guarded.txt"); got != "intact\n" {
		s.Failf("a denied command ran, guarded.txt holds %q", got)
	}

	s.Run("chat", "--yolo", "-q", "Write the allowed file again", "-Q").Exit(0).Has("Allowed under yolo.")
	if got := s.Read("${WORK}/allowed.txt"); got != "yolo-ok\n" {
		s.Failf("a command matching no deny pattern did not run under yolo, allowed.txt holds %q", got)
	}

	edited := strings.Replace(s.Read("config.yaml"), "    - \"*wipe-marker*\"\n", "    - \"*wipe-marker*\"\n    - \"*late-marker*\"\n", 1)
	if !strings.Contains(edited, "late-marker") {
		s.Failf("config.yaml holds no deny list to extend:\n%s", s.Read("config.yaml"))
	}
	s.Write("config.yaml", edited)
	s.Run("chat", "--yolo", "-q", "Write the late file", "-Q").Exit(0).Has("Late rule denied.")
	approvalsDenyBlocked(s, "*late-marker*", "rule added after the first turns")
	if s.Exists("${WORK}/late.txt") {
		s.Failf("a command matching a deny pattern added to config.yaml ran, late.txt holds %q", s.Read("${WORK}/late.txt"))
	}

	s.Run("config", "set", "approvals.mode", "off").Exit(0)
	s.SameJSON(s.Run("config", "get", "approvals.deny", "--json").Exit(0).Stdout, `["*wipe-marker*", "*late-marker*"]`)
	s.Run("chat", "-q", "Delete the victim directory", "-Q").Exit(0).Has("Removed with mode off.")
	if s.Exists("${WORK}/victim") {
		s.Failf("approvals.mode off did not run a flagged command, tool result %s", clip(s.ToolResult("terminal")))
	}
	s.Run("chat", "-q", "Overwrite the guarded file", "-Q").Exit(0).Has("Denied with mode off.")
	approvalsDenyBlocked(s, "*wipe-marker*", "mode off turn")
	if got := s.Read("${WORK}/guarded.txt"); got != "intact\n" {
		s.Failf("a denied command ran with approvals.mode off, guarded.txt holds %q", got)
	}
}
