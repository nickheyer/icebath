package wm

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Text of the chip pieces in order
func chipText(c chip) string {
	var sb strings.Builder
	for _, p := range c.parts {
		sb.WriteString(p.text)
	}
	return sb.String()
}

// Three workspaces with a working, a blocked and a finished lane, a waiting approval and five dials
func lanes(t *testing.T, w int) *Model {
	t.Helper()
	m := still(t, w, 16)
	ws1, _ := m.AddWorkspace("1", newStub("a"))
	ws2, _ := m.AddWorkspace("2", newStub("b"))
	ws3, _ := m.AddWorkspace("3", newStub("c"))
	m.SetBar(func() Status {
		return Status{
			Lanes: map[*Workspace]Lane{
				ws1: {Name: "howdy partner", Signal: Working, Detail: "Bash 14.2s"},
				ws2: {Name: "Sup freddie, im demoing my app", Signal: Blocked, Detail: "Bash"},
				ws3: {Name: "port\nscan", Signal: Fresh},
			},
			Notice: Notice{Text: "approve Bash rm -rf build ∷ 2 Sup freddie", Signal: Blocked, More: 1, Action: "attention.next"},
			Dials:  []Segment{{Text: "$0.42", Style: Dim}, {Text: "⟁ 2", Style: Cool}, {Text: "ctx ▰▰▱▱▱▱ 38%", Style: Dim}, {Text: "⟡ freddie", Style: Cool}, {Text: "bypass", Style: Bad, Action: "mode.set"}},
		}
	})
	m.View()
	return m
}

func TestLanesCarryNameSignalAndDetail(t *testing.T) {
	m := lanes(t, 180)
	spin := m.theme.Glyphs.Spinner[int(m.phase*spinRate)%len(m.theme.Glyphs.Spinner)]
	if got := chipText(m.chips[0]); got != " 1 howdy partner "+spin+" Bash 14.2s ✕ " {
		t.Fatalf("working lane %q", got)
	}
	if got := chipText(m.chips[1]); got != " 2 Sup freddie, im d⋯ ⍰ Bash " {
		t.Fatalf("blocked lane cuts its name %q", got)
	}
	if got := chipText(m.chips[2]); got != " 3 port scan ✓ " {
		t.Fatalf("finished lane folds whitespace %q", got)
	}
	if m.chipParts(0, Lane{Signal: Failed}, 0)[1].text != " ✘" || len(m.chipParts(0, Lane{}, 0)) != 3 {
		t.Fatal("failed lanes carry the fail mark and idle lanes no mark")
	}
	if got := m.chipParts(1, Lane{Name: "howdy partner", Signal: Fresh, Detail: "x"}, 1); len(got) != 4 || got[1].text != " howdy p⋯" || got[2].role != chipMark {
		t.Fatalf("the middle level keeps a short name and drops the detail %v", got)
	}
	if got := m.chipParts(1, Lane{Name: "howdy", Signal: Fresh}, 2); len(got) != 3 || got[1].role != chipMark {
		t.Fatalf("the last level keeps the number and the mark %v", got)
	}
	for _, s := range []Signal{Idle, Working, Fresh, Failed, Blocked} {
		if m.signalColor(s) == nil {
			t.Fatal("signal color")
		}
	}
	for _, s := range []SegmentStyle{Plain, Dim, Cool, Warm, Hot, Good, Bad} {
		if m.styleColor(s) == nil {
			t.Fatal("dial color")
		}
	}
}

func TestBarGivesWayInOrder(t *testing.T) {
	level := func(m *Model) int {
		switch {
		case len(m.chips[0].parts) > 0 && m.chips[0].parts[len(m.chips[0].parts)-3].role == chipDetail:
			return 0
		case m.chips[0].parts[1].role == chipName:
			return 1
		}
		return 2
	}
	cases := []struct {
		w, level, dials int
		field           bool
	}{{180, 0, 5, true}, {160, 1, 5, true}, {130, 1, 2, true}, {110, 1, 1, true}, {90, 2, 1, true}, {50, 2, 1, true}, {34, 2, 1, false}, {20, 2, 0, false}}
	for _, c := range cases {
		m := lanes(t, c.w)
		_, fw := m.fieldBox()
		if level(m) != c.level || len(m.dials) != c.dials || (fw > 0) != c.field {
			t.Fatalf("at %d level %d dials %d field %d", c.w, level(m), len(m.dials), fw)
		}
		if c.dials > 0 && m.dials[len(m.dials)-1].seg.Text != "bypass" {
			t.Fatalf("the mode dial stays last at %d", c.w)
		}
		if c.dials > 0 && m.dials[len(m.dials)-1].x+m.dials[len(m.dials)-1].w != c.w-m.gapOutX {
			t.Fatalf("dials end at the margin at %d", c.w)
		}
		if fx, fw := m.fieldBox(); fw > 0 && (fx-1 < m.chipsEnd()+len(newChip)+barGap || (len(m.dials) > 0 && fx+fw >= m.dials[0].x-barGap)) {
			t.Fatalf("the field keeps clear of the chips and dials at %d", c.w)
		}
		if row := screenRow(m, 0); ansi.StringWidth(row) != c.w {
			t.Fatalf("the bar fills the row at %d %q", c.w, row)
		}
	}
}

func TestFieldRaisesTheNoticeAndServesIt(t *testing.T) {
	m := lanes(t, 160)
	row := screenRow(m, 0)
	if !strings.Contains(row, "▐ ⍰ approve Bash rm -rf build ∷ 2 Sup fredd⋯ +1  alt+i ▌") {
		t.Fatalf("%q", row)
	}
	served := 0
	m.SetCommands([]Command{{ID: "attention.next", Title: "next", Run: func() tea.Cmd { served++; return nil }}})
	fx, fw := m.fieldBox()
	m.Update(click(fx+fw/2, 0, tea.MouseLeft))
	if served != 1 || m.Overlay() != nil {
		t.Fatal("a click on the notice serves it")
	}
	calm := m.fieldBg()
	m.Update(move(fx, 0, tea.MouseNone))
	if m.hot.kind != barField || m.fieldBg() == calm {
		t.Fatal("the field rises under the pointer")
	}
	idle := still(t, 100, 16)
	idle.AddWorkspace("1", newStub("a"))
	row = screenRow(idle, 0)
	fx, fw = idle.fieldBox()
	if !strings.Contains(row, "▐ ❯ palette (alt+space) ▌") || fx != (100-fw)/2 {
		t.Fatalf("an idle field is centered and leads into the commands %q", row)
	}
	idle.Update(click(fx+1, 0, tea.MouseLeft))
	if _, ok := idle.Overlay().(*Palette); !ok {
		t.Fatal("a click on the idle field opens the palette")
	}
	plain := lanes(t, 160)
	plain.SetTheme(theme.Plain())
	if row := screenRow(plain, 0); !strings.Contains(row, "? approve") || plain.fieldBg() != plain.theme.Surface {
		t.Fatalf("plain field %q", row)
	}
}

func TestDialsFireTheirActions(t *testing.T) {
	m := lanes(t, 180)
	fired := 0
	m.SetCommands([]Command{{ID: "mode.set", Title: "Mode", Run: func() tea.Cmd { fired++; return nil }}})
	mode, cost := m.dials[4], m.dials[0]
	m.Update(click(mode.x, 0, tea.MouseLeft))
	m.Update(click(cost.x, 0, tea.MouseLeft))
	if fired != 1 || m.Overlay() != nil {
		t.Fatalf("only a dial with an action fires %d", fired)
	}
	m.Update(move(mode.x+1, 0, tea.MouseNone))
	m.View()
	if m.hot != (barSpot{kind: barDial, i: 4}) {
		t.Fatalf("hover on a dial %v", m.hot)
	}
	if c := m.buf.CellAt(mode.x, 0); c == nil || c.Style.Bg == m.theme.Bg {
		t.Fatal("a dial with an action rises under the pointer")
	}
	m.Update(move(cost.x, 0, tea.MouseNone))
	m.View()
	if c := m.buf.CellAt(cost.x, 0); c == nil || c.Style.Bg != m.theme.Bg {
		t.Fatal("a dial without an action stays flat")
	}
}

func TestPaletteTypesIntoTheField(t *testing.T) {
	m := still(t, 100, 30)
	m.AddWorkspace("1", newStub("a"))
	m.OpenPalette()
	m.Update(key('w'))
	m.Update(key('o'))
	v := m.View()
	fx, fw := m.fieldBox()
	ox, ow := m.openField()
	if fx != ox || fw != ow || ow != 60 {
		t.Fatalf("the field opens wide and centered %d %d", fx, fw)
	}
	e := m.overlay
	if e.box.X != fx-1 || e.box.Y != 0 || e.box.W != fw+2 {
		t.Fatalf("the sheet hangs from the field %v", e.box)
	}
	row := screenRow(m, 0)
	p := m.Overlay().(*Palette)
	if !strings.Contains(row, "❯ wo") || !strings.Contains(row, p.Tally()+" ▌") {
		t.Fatalf("%q", row)
	}
	if v.Cursor == nil || v.Cursor.Y != 0 || v.Cursor.X != fx+1+2+2 {
		t.Fatalf("the caret sits after the query %v", v.Cursor)
	}
	if lines := strings.Split(ansi.Strip(v.Content), "\n"); strings.Contains(lines[2]+lines[3], "❯") {
		t.Fatal("the sheet lists matches without its own input line")
	}
	if c := m.buf.CellAt(fx+fw/2, 0); c == nil || c.Style.Bg != m.theme.Surface {
		t.Fatal("the backdrop spares the field")
	}
	if c := m.buf.CellAt(0, 0); c == nil || c.Style.Bg == m.theme.Bg {
		t.Fatal("the backdrop dims the bar beside the field")
	}
	m.Update(click(fx+2, 0, tea.MouseLeft))
	if m.Overlay() == nil {
		t.Fatal("a click on the field keeps the palette")
	}
	m.Update(click(1, 0, tea.MouseLeft))
	if m.Overlay() != nil {
		t.Fatal("a click on the bar beside the field dismisses the palette")
	}
	var got string
	m.ShowOverlay(NewPrompt(m.theme, "title", strings.Repeat("x", 80), func(text string) tea.Cmd { got = text; return nil }))
	v = m.View()
	row = screenRow(m, 0)
	if m.overlay.box.W != 0 || !strings.Contains(row, "enter confirm ∷ esc cancel ▌") || strings.Contains(screenRow(m, 2), "x") {
		t.Fatalf("a prompt lives in the field alone %q", row)
	}
	if fx, fw := m.fieldBox(); v.Cursor == nil || v.Cursor.X <= fx || v.Cursor.X >= fx+fw {
		t.Fatalf("a long query scrolls so the caret stays inside %v", v.Cursor)
	}
	_, cmd := m.Update(key(tea.KeyEnter))
	pump(m, cmd)
	if m.Overlay() != nil || got != strings.Repeat("x", 80) {
		t.Fatalf("enter hands the text over and closes the prompt %q", got)
	}
}

func TestToastsExpireWithTheClock(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := New(WithMotion(0), WithClock(func() time.Time { return now }))
	m.SetSize(100, 30)
	m.AddWorkspace("1", newStub("a"))
	if m.Toast("", Info, 0) != nil {
		t.Fatal("empty toasts are dropped")
	}
	m.Toast("one", Info, 0)
	m.Toast("two", Error, time.Second)
	if toasts := m.Toasts(); len(toasts) != 2 || toasts[0] != "one" {
		t.Fatalf("%v", toasts)
	}
	now = now.Add(2 * time.Second)
	m.Update(toastTickMsg{})
	if toasts := m.Toasts(); len(toasts) != 1 || toasts[0] != "one" {
		t.Fatalf("%v", toasts)
	}
	now = now.Add(DefaultToastTTL)
	m.Update(toastTickMsg{})
	if len(m.Toasts()) != 0 {
		t.Fatal("expired")
	}
	for i := 0; i < 8; i++ {
		m.Toast("many", Warn, 0)
	}
	if len(m.Toasts()) != 5 {
		t.Fatal("toasts are capped")
	}
	m.SetMotion(1)
	m.Toast("slide", Success, 0)
	if !m.stepToasts(1.0 / 60) {
		t.Fatal("toasts slide in")
	}
	m.SetSize(10, 5)
	m.View()
}

func TestLayoutPersistence(t *testing.T) {
	m, _, _, _ := threeTiles(t)
	f := newStub("dialog")
	f.float = true
	m.OpenHere(f)
	l := m.Layout()
	if len(l.Workspaces) != 1 || l.Ratio != defaultRatio || l.Theme != "blackwall" || l.Motion == nil || *l.Motion != 0 {
		t.Fatalf("%+v", l)
	}
	ws := l.Workspaces[0]
	if ws.ID != "ws1" || len(ws.Tiles) != 3 || ws.Tiles[1].Key != "beta" || ws.Tiles[2].Key != "gamma" || len(ws.Floats) != 1 || ws.Floats[0].Key != "dialog" || ws.Focus != "dialog" {
		t.Fatalf("%+v", ws)
	}
	half := 0.5
	m.Restore(Layout{Ratio: 0.4, Motion: &half})
	if m.Ratio() != 0.4 || m.Motion() != 0 {
		t.Fatal("restore applies the ratio but keeps motion fixed by the option")
	}
	fresh := New()
	fresh.SetSize(100, 30)
	fresh.AddWorkspace("1", newStub("a"))
	fresh.Restore(Layout{Ratio: 0.45, Motion: &half})
	if fresh.Ratio() != 0.45 || fresh.Motion() != 0.5 {
		t.Fatal("restore applies both when nothing fixed them")
	}
	fixed := New(WithRatio(0.7))
	fixed.SetSize(100, 30)
	fixed.AddWorkspace("1", newStub("a"))
	fixed.Restore(Layout{Ratio: 0.45})
	if fixed.Ratio() != 0.7 {
		t.Fatal("the ratio option wins")
	}
	fixed.Restore(Layout{})
	if fixed.Ratio() != 0.7 || fixed.Motion() != 1 {
		t.Fatal("an empty layout changes nothing")
	}
}

func TestThemeSwitchRepaints(t *testing.T) {
	m, a, _, _ := threeTiles(t)
	m.View()
	if m.Active().holds(a).dirty {
		t.Fatal("painted")
	}
	m.OpenPalette()
	m.SetTheme(plainTheme())
	if m.Theme().Name != "plain" || !m.Active().holds(a).dirty {
		t.Fatal("theme switch marks cards dirty")
	}
	if p := m.Overlay().(*Palette); p.theme.Name != "plain" {
		t.Fatal("the palette follows the theme")
	}
	m.View()
}
