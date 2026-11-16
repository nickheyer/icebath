package app

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
)

// Screen row of the first line holding each text, in the order given
func screenRowsOf(t *testing.T, h *ptHarness, texts ...string) []int {
	t.Helper()
	screen := h.screen()
	rows := make([]int, len(texts))
	for i, text := range texts {
		rows[i] = -1
		for y, row := range screen {
			if strings.Contains(row, text) {
				rows[i] = y
				break
			}
		}
		if rows[i] < 0 {
			t.Fatalf("%q is not on the screen:\n%s", text, strings.Join(screen, "\n"))
		}
	}
	return rows
}

func TestKanbanHelpDrawsItsMarkdownBoldOnlyWhereStylesShow(t *testing.T) {
	styled := &cliui.Console{StdoutTTY: func() bool { return true }, Getenv: func(k string) (string, bool) {
		return map[string]string{"TERM": "xterm-256color"}[k], k == "TERM"
	}}
	restore := SetStreams(styled, stderrWriter())
	if got := terminalBold("**/kanban** — board"); got != boldANSI+"/kanban"+resetANSI+" — board" {
		t.Fatalf("styled console drew %q", got)
	}
	restore()
	plain := &cliui.Console{StdoutTTY: func() bool { return false }}
	defer SetStreams(plain, stderrWriter())()
	if got := terminalBold("**/kanban** — board"); got != "**/kanban** — board" {
		t.Fatalf("plain console drew %q", got)
	}
}
