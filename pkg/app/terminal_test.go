//go:build linux || darwin

package app

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Runs a command and every command it leads to through the model until the view shows the text or time runs out
func driveUntil(t *testing.T, m *Model, cmd tea.Cmd, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 && time.Now().Before(deadline) {
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		msg := next()
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		if msg == nil {
			continue
		}
		_, cmd := m.update(msg)
		if strings.Contains(m.View().Content, text) {
			return
		}
		queue = append(queue, cmd)
	}
	t.Fatalf("view never showed %q:\n%s", text, m.View().Content)
}

func TestTerminalRunsLineBesideSession(t *testing.T) {
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	project := t.TempDir()
	m := started(t, project, "")
	m.update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if len(m.terms) != 0 {
		t.Fatal("terminals open before any was asked for")
	}
	cmd := m.runLine(m.current(), "echo terminal-ok")
	driveUntil(t, m, cmd, "terminal-ok")
	if len(m.terms) != 1 || !m.wm.Has("terminal:1") {
		t.Fatalf("terminals %v", m.terms)
	}
	term := m.terms[1]
	if ws := m.wm.WorkspaceOf(term); ws != m.workspaceOf(m.current()) {
		t.Fatal("terminal opened away from the session")
	}
	saved := m.layoutState()
	var keys []string
	for _, tile := range saved.Workspaces[0].Tiles {
		keys = append(keys, tile.Key)
	}
	if strings.Join(keys, " ") != "session:@1 terminal:1" {
		t.Fatalf("saved tiles %v", keys)
	}
	if _, ok := saved.Workspaces[0].Cards["terminal:1"]; !ok {
		t.Fatalf("terminal state missing from %v", saved.Workspaces[0].Cards)
	}
	again := m.runLine(m.current(), "echo second-line")
	driveUntil(t, m, again, "second-line")
	if len(m.terms) != 1 {
		t.Fatal("second line opened another terminal")
	}
	if c := m.cardFor("terminal:7"); c == nil || c.(*cards.Terminal).ID() != 7 || m.termSeq < 7 {
		t.Fatalf("restored key: %v seq %d", c, m.termSeq)
	}
	m.update(cards.SettingChangedMsg{Key: "terminal_scrollback", Value: "50"})
	m.wm.Close(term)
	if m.wm.Has("terminal:1") {
		t.Fatal("terminal still open after close")
	}
}
