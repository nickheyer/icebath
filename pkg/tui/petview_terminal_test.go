//go:build !icebath_core && !icebath_noextras

package tui

import (
	"os"
	"testing"
)

func TestPetTerminalGraphicsNamesTheClientsOwnTerminal(t *testing.T) {
	for _, k := range []string{"TMUX", "STY", "KITTY_WINDOW_ID", "GHOSTTY_RESOURCES_DIR", "TERM_PROGRAM", "WEZTERM_PANE"} {
		t.Setenv(k, "")
	}
	t.Setenv("TERM", "xterm-256color")
	if got := terminalGraphicsPick(os.Getenv); got != "unicode" {
		t.Fatalf("a plain xterm paints %s", got)
	}
	t.Setenv("TERM", "xterm-kitty")
	if got := terminalGraphicsPick(os.Getenv); got != "kitty" {
		t.Fatalf("kitty paints %s", got)
	}
	for _, env := range [][2]string{{"TMUX", "/tmp/tmux-1000/default,1,0"}, {"STY", "1234.pts-0.host"}, {"WEZTERM_PANE", "3"}, {"TERM_PROGRAM", "vscode"}} {
		t.Setenv(env[0], env[1])
		if got := terminalGraphicsPick(os.Getenv); got != "unicode" {
			t.Fatalf("%s=%s still paints %s", env[0], env[1], got)
		}
		t.Setenv(env[0], "")
	}
}
