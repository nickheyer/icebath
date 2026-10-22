package wm

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/keys"
)

func TestAppCommandsTakePrecedenceAndGate(t *testing.T) {
	m, a, b, _ := threeTiles(t)
	ws := m.Active()
	m.Focus(b)
	fired, shadowed := 0, 0
	ready := true
	m.Keys().Register(keys.Action{ID: "custom", Label: "custom", Doc: "fires a test", Default: []string{"alt+c"}})
	m.SetCommands([]Command{
		{ID: "custom", Title: "Custom", When: func() bool { return ready }, Run: func() tea.Cmd { fired++; return nil }},
		{ID: "window.close", Title: "Shadowed close", Run: func() tea.Cmd { shadowed++; return nil }},
	})
	m.Update(key('w', tea.ModAlt))
	if shadowed != 1 || ws.find("beta") == nil {
		t.Fatal("the app command wins over the builtin of the same id")
	}
	m.Update(key('c', tea.ModAlt))
	if fired != 1 {
		t.Fatal("a registered action fires the app command")
	}
	ready = false
	m.Update(key('c', tea.ModAlt))
	if fired != 1 {
		t.Fatal("a command that is not ready does not fire")
	}
	if keys := received[tea.KeyPressMsg](b); len(keys) != 1 || keys[0].String() != "alt+c" {
		t.Fatalf("the key then reaches the card %v", keys)
	}
	if m.Run("custom") != nil || fired != 1 {
		t.Fatal("run by id respects readiness")
	}
	ready = true
	m.Run("custom")
	if fired != 2 {
		t.Fatal("run by id")
	}
	if m.Run("nope") != nil {
		t.Fatal("unknown id")
	}
	m.Fire("custom")
	if fired != 3 {
		t.Fatal("fire runs a command")
	}
	ready = false
	m.Fire("custom")
	if fired != 3 {
		t.Fatal("fire respects readiness")
	}
	all := m.Commands()
	if all[0].ID != "custom" || len(all) < 30 {
		t.Fatalf("%d commands, first %s", len(all), all[0].ID)
	}
	if (Command{}).Ready() != true {
		t.Fatal("nil when is ready")
	}
	_ = a
}

// Stub living in the list context that records the actions fired at it
type listed struct {
	*stub
	actions []string
}

func (l *listed) Contexts() []string { return []string{"sessions", "list", "panel"} }

func (l *listed) Update(msg tea.Msg) (Card, tea.Cmd) {
	if a, ok := msg.(ActionMsg); ok {
		l.actions = append(l.actions, a.ID)
		return l, nil
	}
	l.stub.Update(msg)
	return l, nil
}

func (l *listed) Hints() []Hint {
	return []Hint{{Action: "list.down", Pair: "list.up", Label: "navigate"}, {Action: "sessions.fork"}, {Action: "layout.dwindle"}}
}

func TestCardActionsFireInTheirContexts(t *testing.T) {
	m := still(t, 100, 30)
	m.AddWorkspace("1", newStub("master"))
	l := &listed{stub: newStub("list")}
	m.OpenHere(l)
	m.Update(key('j'))
	m.Update(key(tea.KeyDown))
	m.Update(key('f'))
	m.Update(key('G'))
	if got := strings.Join(l.actions, " "); got != "list.down list.down sessions.fork list.bottom" {
		t.Fatalf("actions %q", got)
	}
	if n := len(received[tea.KeyPressMsg](l.stub)); n != 0 {
		t.Fatalf("bound keys never arrive as text, %d did", n)
	}
	m.Update(key('z'))
	if n := len(received[tea.KeyPressMsg](l.stub)); n != 1 {
		t.Fatal("unbound keys arrive as text")
	}
	m.Update(key('q'))
	if m.Active().find("list") != nil {
		t.Fatal("q closes a panel")
	}
	m.OpenHere(l)
	m.Fire("sessions.delete")
	if l.actions[len(l.actions)-1] != "sessions.delete" {
		t.Fatal("fire reaches the focused card")
	}
	m.Keys().Unbind("sessions.fork")
	ea := m.Active().holds(l)
	bottom := ea.box.Y + ea.box.H - 1 + barHeight
	row := screenRow(m, bottom)
	if !strings.Contains(row, "j/k navigate") || strings.Contains(row, "fork") || !strings.Contains(row, "q close") || strings.Contains(row, "dwindle") {
		t.Fatalf("legend from the keymap %q", row)
	}
	m.Keys().Reset("sessions.fork")
	m.View()
	var span hintSpan
	for _, h := range ea.hints {
		if h.id == "sessions.fork" {
			span = h
		}
	}
	if span.x1 == 0 {
		t.Fatalf("fork span recorded %v", ea.hints)
	}
	m.Update(click(span.x0, bottom, tea.MouseLeft))
	if l.actions[len(l.actions)-1] != "sessions.fork" {
		t.Fatalf("clicking a legend entry fires it %v", l.actions)
	}
	m.Update(move(span.x0, bottom, tea.MouseNone))
	if !m.hoverGrip {
		t.Fatal("legend entries show a grip")
	}
	m.OpenPalette()
	p := m.Overlay().(*Palette)
	found := false
	for _, it := range p.Matches() {
		if it.ID == "sessions.fork" && it.Keys == "f" && strings.Contains(it.Title, "list: fork") {
			found = true
		}
		if it.ID == "list.down" {
			t.Fatal("quiet actions stay out of the palette")
		}
	}
	if !found {
		t.Fatal("the palette lists the actions of the focused card with their keys")
	}
	m.Update(key('j', tea.ModAlt))
	if m.Overlay() == nil {
		t.Fatal("global binds without the overlay flag do not fire over an overlay")
	}
	m.Update(key(tea.KeyEscape))
	if m.Overlay() != nil {
		t.Fatal("esc closes the palette")
	}
	m.SetKeys(nil)
	if m.Keys() == nil {
		t.Fatal("a nil keymap is ignored")
	}
}

// Stub that waits for a key the way the keybind editor does
type grabber struct {
	*listed
	waiting bool
}

func (g *grabber) Raw() bool { return g.waiting }

func (g *grabber) Contexts() []string {
	if g.waiting {
		return []string{keys.Capture}
	}
	return g.listed.Contexts()
}

func (g *grabber) Update(msg tea.Msg) (Card, tea.Cmd) {
	g.listed.Update(msg)
	return g, nil
}

func TestRawCardsTakeGlobalChords(t *testing.T) {
	m := still(t, 100, 30)
	m.AddWorkspace("1", newStub("master"))
	g := &grabber{listed: &listed{stub: newStub("grab")}, waiting: true}
	m.OpenHere(g)
	tabs := NewTabs(m.Theme(), "tabs", "t", g)
	m.Update(key('j', tea.ModAlt))
	m.Update(key(tea.KeySpace, tea.ModAlt))
	if m.Overlay() != nil || m.Focused() != g {
		t.Fatal("global chords stay with a raw card")
	}
	if got := received[tea.KeyPressMsg](g.stub); len(got) != 2 || got[0].String() != "alt+j" {
		t.Fatalf("the raw card receives the chords as typed %v", got)
	}
	m.Update(key(tea.KeyEscape))
	if len(g.actions) != 1 || g.actions[0] != "capture.cancel" {
		t.Fatalf("its own context still fires %v", g.actions)
	}
	if !tabs.Raw() || slices.Contains(tabs.Contexts(), keys.Tabs) {
		t.Fatal("tabs take keys raw while the active tab does, the strip keys included")
	}
	g.waiting = false
	if !slices.Contains(tabs.Contexts(), keys.Tabs) {
		t.Fatal("the strip keys come back once the tab stops waiting")
	}
	m.Update(key('h', tea.ModAlt))
	if m.Focused() == g {
		t.Fatal("global chords fire again once the card stops waiting")
	}
}

func TestPickerStaysOpenWhenPickedFromThePalette(t *testing.T) {
	m, _, _, _ := threeTiles(t)
	m.SetCommands([]Command{{ID: "theme.set", Title: "Theme…", Run: func() tea.Cmd {
		return m.ShowOverlay(NewPalette(m.theme, []Item{{ID: "light", Title: "light"}}))
	}}})
	m.OpenPalette()
	p := m.Overlay().(*Palette)
	for _, r := range "theme" {
		m.Update(key(r))
	}
	if ms := p.Matches(); len(ms) == 0 || ms[0].ID != "theme.set" {
		t.Fatalf("%v", p.Matches())
	}
	_, cmd := m.Update(key(tea.KeyEnter))
	pump(m, cmd)
	next, ok := m.Overlay().(*Palette)
	if !ok || next == p || len(next.Matches()) != 1 || next.Matches()[0].ID != "light" {
		t.Fatal("the picker a palette command opened survives the palette's own dismissal")
	}
	m.Update(RequestDismissOf(p)())
	if m.Overlay() != next {
		t.Fatal("a dismissal from the gone palette is ignored")
	}
	m.Update(RequestDismiss()())
	if m.Overlay() != nil {
		t.Fatal("an untargeted dismissal closes whatever is up")
	}
}

func TestPaletteListsRecentFirst(t *testing.T) {
	m, _, _, _ := threeTiles(t)
	m.Run("window.zoom")
	m.Run("window.float")
	m.OpenPalette()
	p, ok := m.Overlay().(*Palette)
	if !ok || !p.commands {
		t.Fatal("palette overlay")
	}
	items := p.Matches()
	if items[0].ID != "window.float" || items[1].ID != "window.zoom" {
		t.Fatalf("%s %s", items[0].ID, items[1].ID)
	}
	for _, it := range items {
		if it.ID == "workspace.next" {
			t.Fatal("commands that are not ready are left out")
		}
	}
	m.OpenPalette()
	if m.Overlay() != nil {
		t.Fatal("opening again closes")
	}
	for i := 0; i < 20; i++ {
		m.remember("window.zoom")
		m.remember("window.float")
	}
	if len(m.recent) > 12 {
		t.Fatal("recent list is bounded")
	}
	m.AddWorkspace("2", newStub("z"))
	m.OpenPalette()
	p = m.Overlay().(*Palette)
	found := false
	for _, it := range p.Matches() {
		if it.ID == "workspace.2" {
			found = true
		}
	}
	if !found {
		t.Fatal("workspace commands appear once there are workspaces")
	}
	was := m.Active().holds(m.Focused()).floating
	_, cmd := m.Update(key(tea.KeyEnter))
	if m.Active().holds(m.Focused()).floating == was {
		t.Fatal("enter runs the first item")
	}
	pump(m, cmd)
	if m.Overlay() != nil {
		t.Fatal("enter dismisses")
	}
}
