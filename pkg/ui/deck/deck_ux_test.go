package deck

import (
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type counter struct {
	stub
	views int
}

func (c *counter) View(w, h int) string { c.views++; return c.stub.View(w, h) }
func (c *counter) Update(msg tea.Msg) (Card, tea.Cmd) {
	c.msgs = append(c.msgs, msg)
	return c, nil
}

func bar(m *Model) string {
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	return lines[len(lines)-1]
}

func column(line, text string) int {
	i := strings.Index(line, text)
	if i < 0 {
		return -1
	}
	return utf8.RuneCountInString(line[:i])
}

func TestHintBarReservesBottomRow(t *testing.T) {
	m, _ := newDeck(3)
	if !m.Hints() || m.boxes[0].h != 39 {
		t.Fatalf("hints %v main height %d", m.Hints(), m.boxes[0].h)
	}
	line := bar(m)
	for _, want := range []string{"CARD 0 1/3", "enter solo", "l stack", "tab next", "L/H resize", "1-9 jump", "x close", "? hide", "q quit"} {
		if !strings.Contains(line, want) {
			t.Fatalf("bar %q lacks %q", line, want)
		}
	}
	press(m, rune_('?'))
	if m.Hints() || m.boxes[0].h != 40 {
		t.Fatalf("? must hide hints and reclaim the row, hints %v height %d", m.Hints(), m.boxes[0].h)
	}
	settle(m)
	if strings.Contains(bar(m), "quit") {
		t.Fatal("hidden bar still rendered")
	}
	press(m, rune_('j'))
	press(m, rune_('?'))
	line = bar(m)
	for _, want := range []string{"CARD 1 2/3", "enter promote", "h main", "J/K move"} {
		if !strings.Contains(line, want) {
			t.Fatalf("stack bar %q lacks %q", line, want)
		}
	}
}

func TestHintsFollowCaptureAndSolo(t *testing.T) {
	m, stubs := newDeck(2)
	stubs[0].capture = true
	line := bar(m)
	if !strings.Contains(line, "capturing") || !strings.Contains(line, "tab next") || !strings.Contains(line, "ctrl+c quit") || strings.Contains(line, "q quit") {
		t.Fatalf("capture bar %q", line)
	}
	stubs[0].capture = false
	press(m, keyEnter)
	line = bar(m)
	if !strings.Contains(line, "solo") || !strings.Contains(line, "esc split") {
		t.Fatalf("solo bar %q", line)
	}
	m.Update(tea.MouseMotionMsg{X: 118, Y: 1})
	if !strings.Contains(bar(m), "CARD 1 2/2") {
		t.Fatalf("hovered chip must be named, bar %q", bar(m))
	}
}

func TestJumpKeys(t *testing.T) {
	m, _ := newDeck(3)
	press(m, rune_('3'))
	if m.Focused() != 2 {
		t.Fatalf("3 focuses %d", m.Focused())
	}
	press(m, rune_('1'))
	if m.Focused() != 0 {
		t.Fatalf("1 focuses %d", m.Focused())
	}
	settle(m)
	press(m, rune_('9'))
	if m.Focused() != 0 || m.cards[0].rect.X.Vel == 0 {
		t.Fatalf("out of range digit must nudge, focus %d", m.Focused())
	}
}

func TestOrdinalsShowInTitles(t *testing.T) {
	m, _ := newDeck(3)
	plain := ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "1 ◈ CARD 0") || !strings.Contains(plain, "3 ◈ CARD 2") {
		t.Fatalf("ordinals missing:\n%s", plain)
	}
	press(m, rune_('?'))
	settle(m)
	plain = ansi.Strip(m.View().Content)
	if strings.Contains(plain, "1 ◈ CARD 0") || !strings.Contains(plain, "◈ CARD 0") {
		t.Fatalf("ordinals must hide with the bar:\n%s", plain)
	}
}

func TestResizeSnapsPlacedCards(t *testing.T) {
	m, _ := newDeck(3)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	for i, e := range m.cards {
		if x, y, w, h := e.rect.Cells(); (box{x, y, w, h}) != m.boxes[i] {
			t.Fatalf("card %d lags the resize at %d %d %d %d want %+v", i, x, y, w, h, m.boxes[i])
		}
	}
}

func TestGutterDragTracksPointer(t *testing.T) {
	m, _ := newDeck(3)
	gx := m.boxes[0].x + m.boxes[0].w
	m.Update(tea.MouseMotionMsg{X: gx + 1, Y: 3})
	if !m.gutterHot || m.hover != -1 {
		t.Fatalf("gutter zone must span the border columns, hot %v hover %d", m.gutterHot, m.hover)
	}
	m.Update(tea.MouseClickMsg{X: gx + 1, Y: 3, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 60, Y: 3, Button: tea.MouseLeft})
	if _, _, w, _ := m.cards[0].rect.Cells(); w != 60 {
		t.Fatalf("main card must follow the pointer without easing, width %d", w)
	}
	m.Update(tea.MouseReleaseMsg{X: 60, Y: 3, Button: tea.MouseLeft})
	if m.dragging || m.Ratio() != 0.5 {
		t.Fatalf("dragging %v ratio %v", m.dragging, m.Ratio())
	}
}

func TestDragReordersStack(t *testing.T) {
	m, stubs := newDeck(4)
	b1, b2 := m.boxes[1], m.boxes[2]
	hx := b1.x + 5
	m.Update(tea.MouseClickMsg{X: hx, Y: b1.y, Button: tea.MouseLeft})
	if m.Focused() != 1 || m.grab == nil {
		t.Fatalf("press on the frame must focus and grab, focus %d grab %v", m.Focused(), m.grab)
	}
	py := b2.y + b2.h/2
	m.Update(tea.MouseMotionMsg{X: hx, Y: py, Button: tea.MouseLeft})
	if m.Cards()[2] != stubs[1] || m.Focused() != 2 || !m.cards[2].held {
		t.Fatalf("drag did not swap, focus %d held %v", m.Focused(), m.cards[2].held)
	}
	if _, y, _, _ := m.cards[2].rect.Cells(); y != py {
		t.Fatalf("held card must ride the pointer, y %d want %d", y, py)
	}
	m.Update(tea.MouseReleaseMsg{X: hx, Y: py, Button: tea.MouseLeft})
	if m.grab != nil || m.cards[2].held {
		t.Fatal("release must let go")
	}
	settle(m)
	if x, y, w, h := m.cards[2].rect.Cells(); (box{x, y, w, h}) != m.boxes[2] {
		t.Fatalf("dropped card did not settle, %d %d %d %d want %+v", x, y, w, h, m.boxes[2])
	}
}

func TestDragOntoMainPromotes(t *testing.T) {
	m, stubs := newDeck(3)
	b1 := m.boxes[1]
	m.Update(tea.MouseClickMsg{X: b1.x + 3, Y: b1.y, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 20, Y: 10, Button: tea.MouseLeft})
	if m.drop != 0 || m.hover != 0 {
		t.Fatalf("main must light up as the drop target, drop %d hover %d", m.drop, m.hover)
	}
	m.Update(tea.MouseReleaseMsg{X: 20, Y: 10, Button: tea.MouseLeft})
	if m.Cards()[0] != stubs[1] || m.Focused() != 0 || m.drop != -1 || m.grab != nil {
		t.Fatalf("drop on main must promote, focus %d drop %d", m.Focused(), m.drop)
	}
	settle(m)
	b1 = m.boxes[1]
	m.Update(tea.MouseClickMsg{X: b1.x + b1.w/2, Y: b1.y + b1.h/2, Button: tea.MouseLeft})
	if m.grab != nil {
		t.Fatal("content press must stay with the card")
	}
}

func TestMiddleClickCloses(t *testing.T) {
	m, stubs := newDeck(3)
	b := m.boxes[2]
	m.Update(tea.MouseClickMsg{X: b.x + b.w/2, Y: b.y + b.h/2, Button: tea.MouseMiddle})
	if m.Len() != 2 || m.Cards()[1] != stubs[1] || len(m.leaving) != 1 {
		t.Fatalf("len %d leaving %d", m.Len(), len(m.leaving))
	}
}

func TestHintClickRunsAction(t *testing.T) {
	m, _ := newDeck(3)
	line := bar(m)
	qx := column(line, "q quit")
	if qx < 0 {
		t.Fatalf("bar %q", line)
	}
	m.Update(tea.MouseMotionMsg{X: qx, Y: 39})
	if m.hintHot < 0 {
		t.Fatal("hovered hint must light up")
	}
	_, cmd := m.Update(tea.MouseClickMsg{X: qx, Y: 39, Button: tea.MouseLeft})
	if !isQuit(cmd) {
		t.Fatal("clicking quit must quit")
	}
	tx := column(line, "tab next")
	m.Update(tea.MouseClickMsg{X: tx, Y: 39, Button: tea.MouseLeft})
	if m.Focused() != 1 {
		t.Fatalf("clicking next focuses %d", m.Focused())
	}
	jx := column(line, "1-9 jump")
	m.Update(tea.MouseMotionMsg{X: jx, Y: 39})
	if m.hintHot >= 0 {
		t.Fatal("a hint without an action must not light up")
	}
}

func TestContentCachesUntilCardChanges(t *testing.T) {
	c := &counter{stub: stub{title: "C"}}
	m := New(WithCards(c))
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	settle(m)
	m.View()
	n := c.views
	m.Advance(1.0 / 60)
	m.View()
	if c.views != n {
		t.Fatalf("idle frame re-rendered the card, %d views", c.views-n)
	}
	m.Update(struct{}{})
	m.View()
	if c.views != n+1 {
		t.Fatalf("a message must refresh the card, %d views", c.views-n)
	}
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	m.View()
	if c.views != n+2 {
		t.Fatalf("a resize must refresh the card, %d views", c.views-n)
	}
}

func TestFoldedCardsShowTitleAndStatus(t *testing.T) {
	m, stubs := newDeck(10)
	stubs[5].status = "47%"
	if _, _, _, h := m.cards[5].rect.Cells(); h != 1 {
		t.Fatalf("unfocused stack card must fold to a strip, h %d", h)
	}
	if _, _, _, h := m.cards[1].rect.Cells(); h != 31 {
		t.Fatalf("remembered stack card must open, h %d", h)
	}
	plain := ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "6 ◈ CARD 5") || !strings.Contains(plain, "47%") {
		t.Fatalf("strip must carry title and status:\n%s", plain)
	}
	press(m, rune_('6'))
	settle(m)
	h1 := m.cards[1].rect.H.Pos
	if _, _, _, h := m.cards[5].rect.Cells(); h != 31 || h1 != 1 {
		t.Fatalf("focus must open the card and fold the old one, %d and %v", h, h1)
	}
}

func TestEmptyDeckShowsLabel(t *testing.T) {
	m := New()
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	plain := ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "no cards") {
		t.Fatalf("empty deck:\n%s", plain)
	}
}

type hinted struct{ stub }

func (h *hinted) Update(msg tea.Msg) (Card, tea.Cmd) {
	h.msgs = append(h.msgs, msg)
	return h, nil
}

func (h *hinted) Hints() []key.Binding {
	return []key.Binding{key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdn", "scroll"))}
}

func TestCardHintsLeadTheBar(t *testing.T) {
	h := &hinted{stub{title: "LOG"}}
	m := New(WithCards(h, &stub{title: "B"}))
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	settle(m)
	line := bar(m)
	px, ex := column(line, "pgdn scroll"), column(line, "enter solo")
	if px < 0 || ex < 0 || px > ex {
		t.Fatalf("card hints must lead the bar %q", line)
	}
	m.Update(tea.MouseMotionMsg{X: px, Y: 39})
	if m.hintHot >= 0 {
		t.Fatal("card hints are not clickable")
	}
	press(m, keyTab)
	if strings.Contains(bar(m), "pgdn") {
		t.Fatalf("card hints must follow focus %q", bar(m))
	}
}

func TestWithHintsStartsHidden(t *testing.T) {
	m := New(WithHints(false), WithCards(&stub{title: "A"}, &stub{title: "B"}))
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	settle(m)
	if m.Hints() || m.boxes[0].h != 40 || strings.Contains(bar(m), "quit") {
		t.Fatalf("hints %v height %d bar %q", m.Hints(), m.boxes[0].h, bar(m))
	}
	press(m, rune_('?'))
	if !m.Hints() || m.boxes[0].h != 39 {
		t.Fatalf("? must show hints, hints %v height %d", m.Hints(), m.boxes[0].h)
	}
}
