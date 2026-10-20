package deck

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type stub struct {
	title   string
	msgs    []tea.Msg
	capture bool
	cursor  *tea.Cursor
	status  string
}

func (s *stub) Title() string                      { return s.title }
func (s *stub) Glyph() string                      { return "◈" }
func (s *stub) Init() tea.Cmd                      { return nil }
func (s *stub) Capturing() bool                    { return s.capture }
func (s *stub) Status() string                     { return s.status }
func (s *stub) Cursor() *tea.Cursor                { return s.cursor }
func (s *stub) Update(msg tea.Msg) (Card, tea.Cmd) { s.msgs = append(s.msgs, msg); return s, nil }
func (s *stub) View(w, h int) string {
	return strings.TrimSuffix(strings.Repeat(strings.Repeat("x", w+3)+"\n", h+2), "\n")
}

func (s *stub) lastKey() string {
	for i := len(s.msgs) - 1; i >= 0; i-- {
		if k, ok := s.msgs[i].(tea.KeyPressMsg); ok {
			return k.String()
		}
	}
	return ""
}

func (s *stub) focusTrail() []bool {
	var out []bool
	for _, m := range s.msgs {
		if f, ok := m.(FocusMsg); ok {
			out = append(out, f.Focused)
		}
	}
	return out
}

var clock = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newDeck(n int) (*Model, []*stub) {
	stubs := make([]*stub, n)
	cards := make([]Card, n)
	for i := range stubs {
		stubs[i] = &stub{title: fmt.Sprintf("CARD %d", i)}
		cards[i] = stubs[i]
	}
	m := New(WithCards(cards...))
	m.now = func() time.Time { return clock }
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	settle(m)
	return m, stubs
}

func settle(m *Model) {
	for i := 0; i < 3000 && m.Advance(1.0/60); i++ {
	}
}

func press(m *Model, k tea.KeyPressMsg) tea.Cmd {
	_, cmd := m.Update(k)
	return cmd
}

func rune_(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }

var (
	keyTab      = tea.KeyPressMsg{Code: tea.KeyTab}
	keyShiftTab = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	keyEnter    = tea.KeyPressMsg{Code: tea.KeyEnter}
	keyEsc      = tea.KeyPressMsg{Code: tea.KeyEscape}
	keyCtrlC    = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
)

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestCycleWraps(t *testing.T) {
	m, _ := newDeck(3)
	for _, want := range []int{1, 2, 0} {
		press(m, rune_('j'))
		if m.Focused() != want {
			t.Fatalf("focus %d want %d", m.Focused(), want)
		}
	}
	press(m, rune_('k'))
	if m.Focused() != 2 {
		t.Fatalf("k wraps to %d", m.Focused())
	}
	press(m, keyTab)
	press(m, keyShiftTab)
	if m.Focused() != 2 {
		t.Fatalf("tab pair ends on %d", m.Focused())
	}
}

func TestHopBetweenColumnsRemembersStackCard(t *testing.T) {
	m, _ := newDeck(4)
	press(m, rune_('l'))
	press(m, rune_('j'))
	if m.Focused() != 2 {
		t.Fatalf("focus %d", m.Focused())
	}
	press(m, rune_('h'))
	if m.Focused() != 0 {
		t.Fatalf("h lands on %d", m.Focused())
	}
	press(m, rune_('l'))
	if m.Focused() != 2 {
		t.Fatalf("l forgot the stack card, got %d", m.Focused())
	}
}

func TestEnterPromotesThenOpensSolo(t *testing.T) {
	m, stubs := newDeck(3)
	press(m, rune_('j'))
	press(m, rune_('j'))
	press(m, keyEnter)
	cards := m.Cards()
	if cards[0] != stubs[2] || cards[2] != stubs[0] || m.Focused() != 0 {
		t.Fatalf("swap failed, focus %d", m.Focused())
	}
	if m.Mode() != Split {
		t.Fatal("promoting must not change mode")
	}
	press(m, keyEnter)
	if m.Mode() != Solo {
		t.Fatal("enter on main must open solo")
	}
	press(m, keyEsc)
	if m.Mode() != Split {
		t.Fatal("esc must leave solo")
	}
	settle(m)
	press(m, keyEsc)
	if m.cards[0].rect.X.Vel == 0 {
		t.Fatal("esc with nothing to leave must nudge")
	}
	settle(m)
	if x, _, _, _ := m.cards[0].rect.Cells(); x != 0 {
		t.Fatalf("nudge did not return, x %d", x)
	}
}

func TestCaptureRouting(t *testing.T) {
	m, stubs := newDeck(3)
	stubs[0].capture = true
	press(m, rune_('j'))
	if m.Focused() != 0 || stubs[0].lastKey() != "j" {
		t.Fatalf("captured key escaped, focus %d last %q", m.Focused(), stubs[0].lastKey())
	}
	if isQuit(press(m, rune_('q'))) {
		t.Fatal("q must reach a capturing card")
	}
	if !isQuit(press(m, keyCtrlC)) {
		t.Fatal("ctrl+c must always quit")
	}
	press(m, keyTab)
	if m.Focused() != 1 {
		t.Fatalf("tab must still move focus, got %d", m.Focused())
	}
	press(m, rune_('z'))
	if stubs[1].lastKey() != "z" {
		t.Fatalf("unbound key not forwarded, got %q", stubs[1].lastKey())
	}
	if !isQuit(press(m, rune_('q'))) {
		t.Fatal("q must quit when nothing captures")
	}
}

func TestFocusMessages(t *testing.T) {
	m, stubs := newDeck(2)
	press(m, rune_('j'))
	if got := stubs[0].focusTrail(); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("card 0 trail %v", got)
	}
	if got := stubs[1].focusTrail(); len(got) != 1 || !got[0] {
		t.Fatalf("card 1 trail %v", got)
	}
}

func TestMouseFocusDoubleClickAndWheel(t *testing.T) {
	m, stubs := newDeck(3)
	x, y, w, h := m.cards[1].rect.Cells()
	cx, cy := x+w/2, y+h/2
	m.Update(tea.MouseClickMsg{X: cx, Y: cy, Button: tea.MouseLeft})
	if m.Focused() != 1 {
		t.Fatalf("click focus %d", m.Focused())
	}
	click, ok := stubs[1].msgs[len(stubs[1].msgs)-2].(tea.MouseClickMsg)
	if !ok || click.X != cx-x-1 || click.Y != cy-y-1 {
		t.Fatalf("click not translated %+v", stubs[1].msgs[len(stubs[1].msgs)-2])
	}
	clock = clock.Add(100 * time.Millisecond)
	m.Update(tea.MouseClickMsg{X: cx, Y: cy, Button: tea.MouseLeft})
	if m.Cards()[0] != stubs[1] || m.Focused() != 0 {
		t.Fatal("double click must promote")
	}
	settle(m)
	m.Update(tea.MouseWheelMsg{X: 5, Y: 5, Button: tea.MouseWheelDown})
	if _, ok := stubs[1].msgs[len(stubs[1].msgs)-1].(tea.MouseWheelMsg); !ok {
		t.Fatal("wheel must reach the card under the pointer")
	}
	m.Update(tea.MouseMotionMsg{X: cx, Y: cy})
	if m.hover != 1 {
		t.Fatalf("hover %d", m.hover)
	}
}

func TestGutterDragChangesRatio(t *testing.T) {
	m, _ := newDeck(3)
	gx := m.boxes[0].x + m.boxes[0].w
	m.Update(tea.MouseMotionMsg{X: gx, Y: 3})
	if !m.gutterHot {
		t.Fatal("gutter must light up under the pointer")
	}
	m.Update(tea.MouseClickMsg{X: gx, Y: 3, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 48, Y: 3, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 48, Y: 3, Button: tea.MouseLeft})
	if m.Ratio() != 0.4 || m.dragging {
		t.Fatalf("ratio %v dragging %v", m.Ratio(), m.dragging)
	}
	settle(m)
	if m.boxes[0].w != 48 {
		t.Fatalf("main width %d", m.boxes[0].w)
	}
}

func TestRatioKeysAndLimits(t *testing.T) {
	m, _ := newDeck(2)
	press(m, tea.KeyPressMsg{Code: 'l', Text: "L", Mod: tea.ModShift})
	if m.Ratio() != 0.67 {
		t.Fatalf("ratio %v", m.Ratio())
	}
	for range 10 {
		press(m, tea.KeyPressMsg{Code: 'l', Text: "L", Mod: tea.ModShift})
	}
	if m.Ratio() != maxRatio || m.cards[0].rect.X.Vel == 0 {
		t.Fatalf("limit must clamp and nudge, ratio %v", m.Ratio())
	}
}

func TestStackReorder(t *testing.T) {
	m, stubs := newDeck(4)
	press(m, rune_('j'))
	press(m, tea.KeyPressMsg{Code: 'j', Text: "J", Mod: tea.ModShift})
	if m.Cards()[2] != stubs[1] || m.Focused() != 2 {
		t.Fatalf("J did not move the card down, focus %d", m.Focused())
	}
	press(m, tea.KeyPressMsg{Code: 'k', Text: "K", Mod: tea.ModShift})
	if m.Cards()[1] != stubs[1] || m.Focused() != 1 {
		t.Fatal("K did not move the card back up")
	}
	press(m, rune_('h'))
	press(m, tea.KeyPressMsg{Code: 'j', Text: "J", Mod: tea.ModShift})
	if m.Cards()[0] != stubs[0] {
		t.Fatal("main card must not reorder")
	}
}

func TestRemoveAndAdd(t *testing.T) {
	m, stubs := newDeck(3)
	press(m, rune_('j'))
	press(m, rune_('x'))
	if m.Len() != 2 || len(m.leaving) != 1 || m.Focused() != 1 || m.Cards()[1] != stubs[2] {
		t.Fatalf("len %d leaving %d focus %d", m.Len(), len(m.leaving), m.Focused())
	}
	if trail := stubs[2].focusTrail(); len(trail) != 1 || !trail[0] {
		t.Fatalf("next card must gain focus, trail %v", trail)
	}
	settle(m)
	if len(m.leaving) != 0 {
		t.Fatal("departed card must be dropped once off screen")
	}
	fresh := &stub{title: "FRESH"}
	m.Add(fresh)
	if x, _, _, _ := m.cards[2].rect.Cells(); x < 120 {
		t.Fatalf("new card must start off screen, x %d", x)
	}
	settle(m)
	if x, y, w, h := m.cards[2].rect.Cells(); x != m.boxes[2].x || y != m.boxes[2].y || w != m.boxes[2].w || h != m.boxes[2].h {
		t.Fatalf("new card did not land, %d %d %d %d want %+v", x, y, w, h, m.boxes[2])
	}
	for m.Len() > 0 {
		press(m, rune_('x'))
	}
	settle(m)
	v := m.View()
	if lines := strings.Split(v.Content, "\n"); len(lines) != 40 {
		t.Fatalf("empty deck renders %d lines", len(lines))
	}
	if !isQuit(press(m, rune_('q'))) {
		t.Fatal("q must quit an empty deck")
	}
}

func TestViewKeepsScreenShape(t *testing.T) {
	m, stubs := newDeck(5)
	stubs[0].status = "7 lines"
	check := func(w, h int) {
		t.Helper()
		v := m.View()
		lines := strings.Split(v.Content, "\n")
		if h == 0 {
			if v.Content != "" {
				t.Fatalf("%dx%d rendered content", w, h)
			}
			return
		}
		if len(lines) != h {
			t.Fatalf("%dx%d rendered %d lines", w, h, len(lines))
		}
		for i, l := range lines {
			if got := ansi.StringWidth(l); got != w {
				t.Fatalf("%dx%d line %d is %d wide", w, h, i, got)
			}
		}
	}
	check(120, 40)
	plain := ansi.Strip(m.View().Content)
	if !strings.Contains(plain, "CARD 0") || !strings.Contains(plain, "7 lines") {
		t.Fatalf("title or status missing:\n%s", plain)
	}
	press(m, rune_('j'))
	press(m, keyEnter)
	for range 4 {
		m.Advance(1.0 / 60)
		check(120, 40)
	}
	for _, size := range [][2]int{{30, 8}, {12, 5}, {5, 3}, {2, 2}, {1, 1}, {0, 0}, {80, 24}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for range 3 {
			m.Advance(1.0 / 60)
			check(size[0], size[1])
		}
		settle(m)
		check(size[0], size[1])
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	press(m, keyEnter)
	for range 20 {
		m.Advance(1.0 / 60)
		check(100, 30)
	}
}

func TestCursorTranslation(t *testing.T) {
	m, stubs := newDeck(2)
	stubs[0].cursor = tea.NewCursor(2, 1)
	v := m.View()
	if v.Cursor == nil || v.Cursor.X != 3 || v.Cursor.Y != 2 {
		t.Fatalf("cursor %+v", v.Cursor)
	}
	stubs[0].cursor = tea.NewCursor(500, 1)
	if m.View().Cursor != nil {
		t.Fatal("cursor outside the card must be hidden")
	}
	press(m, rune_('j'))
	if m.View().Cursor != nil {
		t.Fatal("unfocused card must not own the cursor")
	}
}

func TestDealInStaggersFromTheEdge(t *testing.T) {
	stubs := []Card{&stub{title: "A"}, &stub{title: "B"}, &stub{title: "C"}}
	m := New(WithCards(stubs...))
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	for i, e := range m.cards {
		if x, _, _, _ := e.rect.Cells(); x != 102 {
			t.Fatalf("card %d starts at %d", i, x)
		}
		if e.delay != float64(i)*dealStagger {
			t.Fatalf("card %d delay %v", i, e.delay)
		}
	}
	for range 6 {
		m.Advance(1.0 / 60)
	}
	x0, _, _, _ := m.cards[0].rect.Cells()
	x2, _, _, _ := m.cards[2].rect.Cells()
	if x0 >= 102 || x2 != 102 {
		t.Fatalf("stagger broken, x0 %d x2 %d", x0, x2)
	}
	settle(m)
	for i, e := range m.cards {
		if x, y, w, h := e.rect.Cells(); x != m.boxes[i].x || y != m.boxes[i].y || w != m.boxes[i].w || h != m.boxes[i].h {
			t.Fatalf("card %d did not land", i)
		}
	}
}

func TestBroadcastReachesEveryCard(t *testing.T) {
	type ping struct{}
	m, stubs := newDeck(2)
	press(m, rune_('j'))
	press(m, rune_('x'))
	m.Update(ping{})
	for i, s := range stubs {
		if _, ok := s.msgs[len(s.msgs)-1].(ping); !ok {
			t.Fatalf("card %d missed the broadcast", i)
		}
	}
}

func BenchmarkView(b *testing.B) {
	m, _ := newDeck(8)
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	settle(m)
	b.ReportAllocs()
	for b.Loop() {
		m.Advance(1.0 / 60)
		m.View()
	}
}
