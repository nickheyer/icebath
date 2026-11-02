package app

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ext"
	_ "github.com/nickheyer/icebath/pkg/ext/sprint"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Arena, board and settings panels of the sprint extension open through their slash commands and render in the app
func TestSprintPanelsInApp(t *testing.T) {
	if err := ext.Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	m := started(t, t.TempDir(), "")
	m.wm.SetMotion(0)
	m.wm.SetSize(160, 44)
	for _, name := range []string{"arena", "sprints", "sprint-settings"} {
		if cmd := m.command(nil, name, ""); cmd == nil {
			t.Fatalf("/%s gave no command", name)
		}
		if !m.wm.Has(name) {
			t.Fatalf("/%s opened no panel", name)
		}
	}
	plain := ansi.Strip(m.View().Content)
	for _, want := range []string{"ARENA", "SPRINTS", "SPRINT SETTINGS"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("view lacks %q", want)
		}
	}
	arena, ws := m.wm.Find("arena")
	if arena == nil || ws == nil {
		t.Fatal("arena is not open")
	}
	if _, ok := arena.(wm.Reporter); !ok {
		t.Fatal("arena does not report to the bar")
	}
	for i := len(ws.Cards()) - 1; i >= 0; i-- {
		if c := ws.Cards()[i]; c != arena {
			m.wm.Close(c)
		}
	}
	for _, d := range m.desksIn(ws) {
		m.wm.Close(d.session)
		m.sessionClosed(d.live.Session.ID())
	}
	st := m.status()
	if st.Lanes[ws].Name != "arena" {
		t.Fatalf("lane of the arena workspace is %+v", st.Lanes[ws])
	}
	live, err := m.app.Create()
	if err != nil {
		t.Fatal(err)
	}
	if cmd := m.submit(cards.SubmitMsg{Session: live.Session.ID(), Text: "hi"}); cmd == nil {
		t.Fatal("submit to a session without a card gave nothing back")
	}
	if cmd := m.submit(cards.SubmitMsg{Session: "nobody", Text: "hi"}); cmd == nil {
		t.Fatal("submit to an unknown session gave nothing back")
	}
	if cmd := m.toggleThinking(live.Session.ID()); cmd == nil {
		t.Fatal("thinking toggle on a session without a card gave nothing back")
	}
	if live.ExpandsThinking() == live.Endpoint.Profile.ExpandsThinking() {
		t.Fatal("thinking did not flip")
	}
}
