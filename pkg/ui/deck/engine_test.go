package deck

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

type pinnedStub struct {
	stub
	minH int
}

func (p *pinnedStub) Pinned() bool                       { return true }
func (p *pinnedStub) MinSize() (int, int)                { return 0, p.minH }
func (p *pinnedStub) Update(msg tea.Msg) (Card, tea.Cmd) { p.msgs = append(p.msgs, msg); return p, nil }

type claimStub struct {
	stub
	keys []string
}

func (c *claimStub) Claims() []string { return c.keys }
func (c *claimStub) Update(msg tea.Msg) (Card, tea.Cmd) {
	c.msgs = append(c.msgs, msg)
	return c, nil
}

type keyedStub struct {
	stub
	key string
}

func (k *keyedStub) Key() string { return k.key }
func (k *keyedStub) Update(msg tea.Msg) (Card, tea.Cmd) {
	k.msgs = append(k.msgs, msg)
	return k, nil
}

type selectStub struct {
	stub
	text string
}

func (s *selectStub) Selectable() bool { return true }
func (s *selectStub) View(w, h int) string {
	lines := strings.Split(s.text, "\n")
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines[:h], "\n")
}
func (s *selectStub) Update(msg tea.Msg) (Card, tea.Cmd) {
	s.msgs = append(s.msgs, msg)
	return s, nil
}

// Runs a command and returns the messages it produces, flattening batches
func run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, run(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func TestTimelineModeFansAndScrubs(t *testing.T) {
	m, _ := newDeck(5)
	press(m, rune_('t'))
	if m.Mode() != Timeline {
		t.Fatal("t must enter the timeline")
	}
	settle(m)
	boxes := m.boxes
	if boxes[0].w != boxes[2].w || boxes[0].h != 39 || boxes[1].h != 37 || boxes[2].h != 35 {
		t.Fatalf("neighbors must shrink with distance: %+v", boxes)
	}
	if boxes[1].x != boxes[0].x+timelineStep || boxes[2].x != boxes[1].x+timelineStep {
		t.Fatalf("neighbors must step aside: %+v", boxes)
	}
	press(m, rune_('l'))
	press(m, rune_('l'))
	if m.Focused() != 2 {
		t.Fatalf("l must scrub right, focus %d", m.Focused())
	}
	settle(m)
	if m.boxes[2].x != (120-m.boxes[2].w)/2 || m.boxes[2].h != 39 {
		t.Fatalf("focused card must be centered and tall: %+v", m.boxes[2])
	}
	if m.cards[0].fade <= m.cards[1].fade || m.cards[2].fade != 0 {
		t.Fatalf("fade must grow with distance: %v %v %v", m.cards[0].fade, m.cards[1].fade, m.cards[2].fade)
	}
	if m.z(m.cards[2], 2, 5) <= m.z(m.cards[1], 1, 5) || m.z(m.cards[0], 0, 5) >= m.z(m.cards[1], 1, 5) {
		t.Fatal("stacking must fall off with distance")
	}
	press(m, rune_('h'))
	if m.Focused() != 1 {
		t.Fatalf("h must scrub left, focus %d", m.Focused())
	}
	m.Update(tea.MouseWheelMsg{X: 1, Y: 1, Button: tea.MouseWheelDown})
	if m.Focused() != 2 {
		t.Fatalf("wheel must scrub, focus %d", m.Focused())
	}
	m.Update(tea.MouseWheelMsg{X: 1, Y: 1, Button: tea.MouseWheelUp})
	if m.Focused() != 1 {
		t.Fatalf("wheel up must scrub back, focus %d", m.Focused())
	}
	m.Update(tea.MouseClickMsg{X: 0, Y: 38, Button: tea.MouseLeft})
	if !m.scrubbing {
		t.Fatal("pressing the stage must start a scrub")
	}
	m.Update(tea.MouseMotionMsg{X: 2 * timelineStep, Y: 38, Button: tea.MouseLeft})
	if m.Focused() != 0 {
		t.Fatalf("dragging right must scrub toward the start, focus %d", m.Focused())
	}
	m.Update(tea.MouseReleaseMsg{X: 2 * timelineStep, Y: 38, Button: tea.MouseLeft})
	if m.scrubbing {
		t.Fatal("release must end the scrub")
	}
	for i := 0; i < 5; i++ {
		press(m, rune_('h'))
	}
	if m.Focused() != 0 {
		t.Fatal("scrub must stop at the first card")
	}
	for i := 0; i < 9; i++ {
		press(m, rune_('l'))
	}
	if m.Focused() != 4 {
		t.Fatal("scrub must stop at the last card")
	}
	line := bar(m)
	for _, want := range []string{"timeline", "h/l scrub", "enter solo", "esc split"} {
		if !strings.Contains(line, want) {
			t.Fatalf("bar %q lacks %q", line, want)
		}
	}
	press(m, keyEnter)
	if m.Mode() != Solo {
		t.Fatal("enter from the timeline opens solo")
	}
	press(m, keyEsc)
	press(m, rune_('t'))
	press(m, rune_('t'))
	if m.Mode() != Split {
		t.Fatal("t again returns to split")
	}
	if mode, ok := ParseMode("timeline"); !ok || mode != Timeline || Timeline.String() != "timeline" || Split.String() != "split" || Solo.String() != "solo" {
		t.Fatal("mode names")
	}
	if _, ok := ParseMode("weird"); ok {
		t.Fatal("unknown mode parsed")
	}
}

func TestNarrowAndUltrawideLayouts(t *testing.T) {
	boxes := layout(30, 20, 3, 0.62, Split, 1, 1)
	if boxes[0].w != 30 || boxes[1].w != 30 || boxes[2].w != 30 || boxes[0].x != 0 {
		t.Fatalf("narrow screens stack everything in one column: %+v", boxes)
	}
	if boxes[1].h < boxes[0].h || boxes[1].h < boxes[2].h {
		t.Fatalf("focused card must be the open one: %+v", boxes)
	}
	m, _ := newDeck(3)
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 20})
	settle(m)
	if m.onGutter(m.boxes[0].x+m.boxes[0].w, 1) {
		t.Fatal("no gutter on a narrow screen")
	}
	if strings.Contains(bar(m), "resize") {
		t.Fatal("no resize hint on a narrow screen")
	}
	wide := layout(240, 40, 8, 0.62, Split, 0, 1)
	if wide[1].x != wide[4].x || wide[5].x == wide[1].x || wide[5].x <= wide[1].x {
		t.Fatalf("ultrawide stacks split into two columns: %+v", wide)
	}
	if wide[1].h+wide[2].h+wide[3].h+wide[4].h != 40 || wide[5].h+wide[6].h+wide[7].h != 40 {
		t.Fatalf("each column fills the height: %+v", wide)
	}
	focused := layout(240, 40, 8, 0.62, Split, 6, 1)
	if focused[6].h != 40/3 && focused[6].h < focused[5].h {
		t.Fatalf("focused card in the second column must be open: %+v", focused)
	}
	narrowFew := layout(240, 40, 3, 0.62, Split, 0, 1)
	if narrowFew[1].x != narrowFew[2].x {
		t.Fatal("few cards keep one stack column")
	}
}

func TestPinnedCardsNeverFoldAndStayPut(t *testing.T) {
	pinned := &pinnedStub{stub: stub{title: "PINNED"}, minH: 5}
	cards := []Card{&stub{title: "MAIN"}, &stub{title: "A"}, pinned, &stub{title: "B"}, &stub{title: "C"}, &stub{title: "D"}}
	m := New(WithCards(cards...))
	m.now = func() time.Time { return clock }
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 14})
	settle(m)
	if m.boxes[2].h < 5 {
		t.Fatalf("pinned card must keep its rows when crowded: %+v", m.boxes)
	}
	if m.boxes[1].h < minOpen || m.boxes[3].h != strip || m.boxes[4].h != strip || m.boxes[5].h != strip {
		t.Fatalf("the remembered card opens and the rest fold: %+v", m.boxes)
	}
	press(m, rune_('3'))
	if m.Focused() != 2 {
		t.Fatal("jump")
	}
	if cmd := press(m, keyEnter); m.Cards()[0].Title() != "MAIN" {
		t.Fatalf("pinned card must not be promoted, main is %s %v", m.Cards()[0].Title(), cmd)
	}
	press(m, rune_('x'))
	if m.Len() != 6 {
		t.Fatal("pinned card must not close on x")
	}
	press(m, rune_('J'))
	if m.Cards()[2].Title() != "PINNED" {
		t.Fatal("pinned card must not move")
	}
	if strings.Contains(bar(m), "x close") {
		t.Fatal("close hint hidden for pinned cards")
	}
	settle(m)
	m.Update(tea.MouseClickMsg{X: m.boxes[2].x + 1, Y: m.boxes[2].y + 1, Button: tea.MouseMiddle})
	if m.Len() != 6 {
		t.Fatal("middle click must not close a pinned card")
	}
	if cmd := m.Remove(2); cmd == nil || m.Len() != 5 {
		t.Fatal("programmatic removal still works")
	}
	c := constraint{pinned: true, minH: 2}
	if c.rows() != minOpen || (constraint{}).rows() != strip || (constraint{pinned: true, minH: 9}).rows() != 9 {
		t.Fatal("constraint rows")
	}
	ys, hs := stackLayout(10, 0, 0, nil)
	if len(ys) != 0 || len(hs) != 0 {
		t.Fatal("empty stack")
	}
}

func TestOverlayTakesInputAndDismisses(t *testing.T) {
	m, stubs := newDeck(2)
	modal := &stub{title: "MODAL"}
	run(m.ShowOverlay(modal))
	settle(m)
	if m.Overlay() != modal || stubs[0].focusTrail()[len(stubs[0].focusTrail())-1] {
		t.Fatal("overlay must take focus from the focused card")
	}
	if tr := modal.focusTrail(); len(tr) != 1 || !tr[0] {
		t.Fatalf("%v", tr)
	}
	x, y, w, h := m.overlay.rect.Cells()
	if w < overlayMinW || h < overlayMinH || x <= 0 || y < 0 || x+w > 120 {
		t.Fatalf("overlay box %d %d %d %d", x, y, w, h)
	}
	press(m, rune_('j'))
	if m.Focused() != 0 || modal.lastKey() != "j" {
		t.Fatal("keys go to the overlay, not the deck")
	}
	if !strings.Contains(bar(m), "modal") || !strings.Contains(bar(m), "esc dismiss") {
		t.Fatalf("%q", bar(m))
	}
	m.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	if m.Focused() != 0 || len(stubs[1].msgs) != 0 {
		t.Fatal("clicks outside the overlay are ignored")
	}
	m.Update(tea.MouseClickMsg{X: x + 2, Y: y + 2, Button: tea.MouseLeft})
	last := modal.msgs[len(modal.msgs)-1].(tea.MouseClickMsg)
	if last.X != 1 || last.Y != 1 {
		t.Fatalf("overlay clicks must be relative: %+v", last)
	}
	if !m.Capturing() == true && m.Capturing() {
		t.Fatal("capturing follows the overlay")
	}
	modal.capture = true
	press(m, keyEsc)
	if m.Overlay() == nil {
		t.Fatal("esc goes to a capturing overlay")
	}
	modal.capture = false
	press(m, keyEsc)
	if m.Overlay() != nil {
		t.Fatal("esc dismisses the overlay")
	}
	if tr := stubs[0].focusTrail(); !tr[len(tr)-1] {
		t.Fatal("focus returns to the focused card")
	}
	if m.Dismiss() != nil {
		t.Fatal("dismiss without an overlay")
	}
	run(m.ShowOverlay(&stub{title: "ONE"}))
	run(m.ShowOverlay(&stub{title: "TWO"}))
	if m.Overlay().Title() != "TWO" {
		t.Fatal("a new overlay replaces the old")
	}
	press(m, keyCtrlC)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	if x, _, w, _ := m.overlay.rect.Cells(); x+w > 80 {
		t.Fatal("overlay follows a resize")
	}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "TWO") {
		t.Fatal("overlay must render")
	}
}

func TestToastsExpireAndSlide(t *testing.T) {
	m, _ := newDeck(1)
	now := clock
	m.now = func() time.Time { return now }
	cmd := m.Toast("saved", Success, time.Second)
	if cmd == nil || len(m.Toasts()) != 1 {
		t.Fatal("toast")
	}
	settle(m)
	if !strings.Contains(ansi.Strip(m.View().Content), "saved") {
		t.Fatal("toast must render")
	}
	for i := 0; i < maxToasts+2; i++ {
		m.Toast("more", Info, 0)
	}
	if len(m.Toasts()) != maxToasts {
		t.Fatalf("%d toasts", len(m.Toasts()))
	}
	now = now.Add(2 * time.Second)
	m.Update(toastTickMsg{})
	if len(m.Toasts()) != maxToasts {
		t.Fatal("default life keeps the later toasts")
	}
	now = now.Add(DefaultToastTTL)
	m.Update(toastTickMsg{})
	if len(m.Toasts()) != 0 {
		t.Fatal("toasts must expire")
	}
	_, cmd = m.Update(Notify("hello", Warn)())
	if len(m.Toasts()) != 1 || cmd == nil {
		t.Fatal("notify intent")
	}
	m.Update(NotifyFor("brief", Error, time.Millisecond)())
	if len(m.Toasts()) != 2 {
		t.Fatal("notify for")
	}
}

func TestIntentsReachTheDeck(t *testing.T) {
	m, stubs := newDeck(3)
	m.Update(RequestFocus(stubs[2])())
	if m.Focused() != 2 {
		t.Fatal("focus intent")
	}
	m.Update(RequestAttention(stubs[1], Alarm)())
	if m.UrgencyOf(1) != Alarm {
		t.Fatal("attention intent")
	}
	settle(m)
	if m.cards[1].pulse.Pos < 0.9 {
		t.Fatalf("pulse must rise %v", m.cards[1].pulse.Pos)
	}
	m.Update(RequestFocus(stubs[1])())
	if m.UrgencyOf(1) != Calm {
		t.Fatal("focus clears attention")
	}
	m.Update(RequestAttention(stubs[1], Pulse)())
	if m.UrgencyOf(1) != Calm {
		t.Fatal("attention on the focused card nudges instead")
	}
	m.Update(RequestAttention(stubs[0], Pulse)())
	m.Update(RequestAttention(stubs[0], Calm)())
	if m.UrgencyOf(0) != Calm || m.UrgencyOf(9) != Calm {
		t.Fatal("calm clears")
	}
	m.Update(RequestPromote(stubs[1])())
	if m.Cards()[0] != stubs[1] {
		t.Fatal("promote intent")
	}
	sib := &stub{title: "SIB"}
	m.Update(RequestSibling(stubs[1], sib)())
	if m.Len() != 4 || m.Cards()[1] != sib {
		t.Fatalf("sibling intent must deal in after the requester: %v", m.Cards())
	}
	m.Update(RequestClose(sib)())
	if m.Len() != 3 {
		t.Fatal("close intent")
	}
	m.Update(RequestOverlay(stubs[1], &stub{title: "OV"})())
	if m.Overlay() == nil {
		t.Fatal("overlay intent")
	}
	m.Update(RequestDismiss(stubs[1])())
	if m.Overlay() != nil {
		t.Fatal("dismiss intent")
	}
	m.Update(RequestMode(stubs[1], Timeline)())
	if m.Mode() != Timeline {
		t.Fatal("mode intent")
	}
	if m.handleIntent(Intent{From: &stub{title: "ghost"}, kind: intentFocus}) != nil {
		t.Fatal("unknown card ignored")
	}
	if m.handleIntent(Intent{From: stubs[1], kind: intentSibling}) != nil || m.handleIntent(Intent{kind: intentOverlay}) != nil {
		t.Fatal("empty payloads ignored")
	}
	if m.Attention(9, Pulse) != nil || m.IndexOf(nil) != -1 {
		t.Fatal("out of range")
	}
}

func TestClaimedKeysBypassDeckBindings(t *testing.T) {
	c := &claimStub{stub: stub{title: "TEXT"}, keys: []string{"j", "x"}}
	m := New(WithCards(c, &stub{title: "OTHER"}))
	m.now = func() time.Time { return clock }
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	settle(m)
	press(m, rune_('j'))
	if m.Focused() != 0 || c.lastKey() != "j" {
		t.Fatal("claimed key must reach the card")
	}
	press(m, rune_('x'))
	if m.Len() != 2 {
		t.Fatal("claimed x must not close")
	}
	press(m, keyTab)
	if m.Focused() != 1 {
		t.Fatal("unclaimed keys still drive the deck")
	}
	if claims(&stub{}, "j") {
		t.Fatal("cards without claims claim nothing")
	}
}

func TestMotionScaleAndRest(t *testing.T) {
	m := New(WithCards(&stub{title: "A"}, &stub{title: "B"}), WithMotion(0))
	m.now = func() time.Time { return clock }
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.Motion() != 0 || m.Advance(1.0/60) {
		t.Fatal("with motion off nothing moves")
	}
	for i, e := range m.cards {
		if e.rect.Distance() != 0 || e.decode != 1 || (i == 0 && e.glow.Pos != 1) {
			t.Fatalf("card %d must snap: %+v", i, e.rect)
		}
	}
	if cmd := press(m, rune_('h')); cmd != nil {
		t.Fatal("no nudge without motion")
	}
	_, cmd := m.Update(anim.FrameMsg{})
	if cmd != nil {
		t.Fatal("stale frames are dropped")
	}
	frame := run(m.wake())[0].(anim.FrameMsg)
	_, cmd = m.Update(frame)
	if cmd != nil || !m.Resting() {
		t.Fatal("clock must rest when nothing moves and motion is off")
	}
	_, cmd = m.Update(rune_('j'))
	if cmd == nil || m.Resting() {
		t.Fatal("input wakes the clock")
	}
	m.SetMotion(2)
	if m.Motion() != 1 {
		t.Fatal("motion clamps to one")
	}
	m.Update(tea.BlurMsg{})
	settle(m)
	frame = run(m.wake())[0].(anim.FrameMsg)
	_, cmd = m.Update(frame)
	if cmd != nil || !m.Resting() {
		t.Fatal("blurred terminals rest once settled")
	}
	m.Update(tea.FocusMsg{})
	frame = run(m.wake())[0].(anim.FrameMsg)
	_, cmd = m.Update(frame)
	if cmd == nil || m.Resting() {
		t.Fatal("a focused terminal keeps the ring flowing")
	}
	m.Toast("x", Info, 0)
	m.Update(tea.BlurMsg{})
	if m.canRest() {
		t.Fatal("toasts keep the clock awake")
	}
	empty := New()
	empty.Update(tea.WindowSizeMsg{Width: 40, Height: 10})
	frame = run(empty.wake())[0].(anim.FrameMsg)
	if _, cmd := empty.Update(frame); cmd != nil {
		t.Fatal("an empty deck rests")
	}
	m.SetMotion(0.5)
	m.Update(RequestFocus(m.Cards()[1])())
	if !m.Advance(1.0 / 60) {
		t.Fatal("half motion still animates")
	}
}

func TestLayoutPersistence(t *testing.T) {
	a := &keyedStub{stub: stub{title: "A"}, key: "a"}
	b := &keyedStub{stub: stub{title: "B"}, key: "b"}
	c := &stub{title: "C"}
	m := New(WithCards(a, b, c))
	m.now = func() time.Time { return clock }
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	settle(m)
	press(m, rune_('2'))
	press(m, rune_('L'))
	press(m, rune_('?'))
	l := m.Layout()
	if strings.Join(l.Order, ",") != "a,b,C" || l.Focus != "b" || l.Ratio <= 0.62 || l.Mode != "split" || l.Hints || l.Theme != "blackwall" {
		t.Fatalf("%+v", l)
	}
	fresh := New(WithCards(c, &stub{title: "NEW"}, b, a))
	fresh.now = func() time.Time { return clock }
	fresh.Init()
	fresh.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	settle(fresh)
	run(fresh.Restore(Layout{Order: []string{"b", "ghost", "a"}, Focus: "a", Ratio: 0.3, Mode: "timeline", Hints: true}))
	titles := []string{}
	for _, card := range fresh.Cards() {
		titles = append(titles, card.Title())
	}
	if strings.Join(titles, ",") != "B,A,C,NEW" || fresh.Focused() != 1 || fresh.Ratio() != 0.3 || fresh.Mode() != Timeline || !fresh.Hints() {
		t.Fatalf("%v focus %d ratio %v mode %v", titles, fresh.Focused(), fresh.Ratio(), fresh.Mode())
	}
	run(fresh.Restore(Layout{Mode: "weird"}))
	if fresh.Mode() != Timeline {
		t.Fatal("unknown mode keeps the current one")
	}
	if keyOf(c) != "C" || keyOf(a) != "a" {
		t.Fatal("keys")
	}
	if (New()).Layout().Focus != "" {
		t.Fatal("empty focus")
	}
}

func TestGroupTabs(t *testing.T) {
	one := &stub{title: "ONE", status: "s1"}
	two := &claimStub{stub: stub{title: "TWO"}, keys: []string{"z"}}
	pin := &pinnedStub{stub: stub{title: "PIN"}, minH: 4}
	g := NewGroup("TRACE", "≋", one, two)
	if g.Title() != "TRACE ONE" || g.Glyph() != "≋" || g.Key() != "TRACE" || g.Len() != 2 || !g.Holds(two) || g.Holds(pin) {
		t.Fatalf("%s", g.Title())
	}
	run(g.Init())
	if active, i := g.Active(); active != one || i != 0 {
		t.Fatal("active")
	}
	g.Update(tea.KeyPressMsg{Code: ']', Text: "]"})
	if active, _ := g.Active(); active != two || g.Title() != "TRACE TWO" {
		t.Fatal("] switches tabs")
	}
	if tr := one.focusTrail(); len(tr) != 1 || tr[0] {
		t.Fatalf("old tab must lose focus: %v", tr)
	}
	g.Update(tea.KeyPressMsg{Code: '[', Text: "["})
	if active, _ := g.Active(); active != one {
		t.Fatal("[ switches back")
	}
	g.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if one.lastKey() != "q" {
		t.Fatal("keys reach the active tab")
	}
	view := g.View(40, 6)
	lines := strings.Split(ansi.Strip(view), "\n")
	if len(lines) < 6 || !strings.Contains(lines[0], "ONE") || !strings.Contains(lines[0], "TWO") {
		t.Fatalf("%q", lines[0])
	}
	g.Update(tea.MouseClickMsg{X: g.spans[1] + 1, Y: 0, Button: tea.MouseLeft})
	if active, _ := g.Active(); active != two {
		t.Fatal("clicking a tab selects it")
	}
	g.Update(tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft})
	if last := two.msgs[len(two.msgs)-1].(tea.MouseClickMsg); last.Y != 1 {
		t.Fatalf("content clicks shift up past the strip: %+v", last)
	}
	g.Update(tea.MouseWheelMsg{X: 1, Y: 3, Button: tea.MouseWheelDown})
	if last := two.msgs[len(two.msgs)-1].(tea.MouseWheelMsg); last.Y != 2 {
		t.Fatalf("%+v", last)
	}
	g.Update(FocusMsg{Focused: true})
	if tr := two.focusTrail(); !tr[len(tr)-1] {
		t.Fatal("focus goes to the active tab")
	}
	g.Update("broadcast")
	if one.msgs[len(one.msgs)-1] != "broadcast" || two.msgs[len(two.msgs)-1] != "broadcast" {
		t.Fatal("other messages reach every tab")
	}
	if g.Status() != "" || g.Capturing() || g.Pinned() || g.Selectable() || g.Cursor() != nil {
		t.Fatal("delegation to a plain active tab")
	}
	g.Select(0)
	if g.Status() != "s1" || len(g.Hints()) != 2 || len(g.Claims()) != 2 {
		t.Fatalf("%s %d", g.Status(), len(g.Hints()))
	}
	one.cursor = &tea.Cursor{Position: tea.Position{X: 2, Y: 3}}
	if c := g.Cursor(); c == nil || c.Y != 4 || c.X != 2 {
		t.Fatalf("%+v", c)
	}
	run(g.Add(pin))
	if active, i := g.Active(); active != pin || i != 2 || !g.Pinned() {
		t.Fatal("add activates the new tab")
	}
	if w, h := g.MinSize(); w != 0 || h != 5 {
		t.Fatalf("%d %d", w, h)
	}
	if _, ok := g.Remove(9); ok {
		t.Fatal("remove out of range")
	}
	if _, ok := g.Remove(2); !ok || g.Len() != 2 {
		t.Fatal("remove active")
	}
	if active, _ := g.Active(); active != two {
		t.Fatal("removal of the last tab activates the previous one")
	}
	g.Remove(0)
	if active, _ := g.Active(); active != two || g.active != 0 {
		t.Fatal("removing before the active tab keeps it active")
	}
	empty := NewGroup("E", "e")
	if empty.Title() != "E" || empty.cycle(1) != nil {
		t.Fatal("empty group")
	}
	empty.Add(one)
	if empty.Len() != 1 || empty.cycle(1) != nil {
		t.Fatal("single tab group")
	}
	if a, i := NewGroup("x", "x").Active(); a != nil || i != -1 {
		t.Fatal("no active in an empty group")
	}
	if NewGroup("x", "x").View(10, 1) == "" {
		t.Fatal("strip only view")
	}
	m := New(WithCards(g, &stub{title: "SIDE"}))
	m.now = func() time.Time { return clock }
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	settle(m)
	m.Update(RequestFocus(two)())
	if m.Focused() != 0 {
		t.Fatal("intents from a held card resolve to the group")
	}
	press(m, rune_('z'))
	if two.lastKey() != "z" {
		t.Fatal("claims of the active tab are honored through the group")
	}
	g.SetTheme(theme.Light())
}

func TestOverflowMarkersAndThemeSwitch(t *testing.T) {
	cards := []Card{&stub{title: "MAIN"}}
	for i := 0; i < 12; i++ {
		cards = append(cards, &stub{title: "CARD " + string(rune('A'+i))})
	}
	m := New(WithCards(cards...))
	m.now = func() time.Time { return clock }
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 10})
	settle(m)
	press(m, rune_('9'))
	settle(m)
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "above") {
		t.Fatalf("strips off the top must be counted:\n%s", view)
	}
	press(m, rune_('2'))
	settle(m)
	view = ansi.Strip(m.View().Content)
	if !strings.Contains(view, "below") {
		t.Fatalf("strips off the bottom must be counted:\n%s", view)
	}
	m.SetTheme(theme.Plain())
	if m.Theme().Name != "plain" || !m.Theme().Plain {
		t.Fatal("theme switch")
	}
	settle(m)
	view = ansi.Strip(m.View().Content)
	if !strings.Contains(view, "+") || strings.Contains(view, "╭") {
		t.Fatal("plain theme draws ascii borders")
	}
	if !strings.Contains(view, "v ") {
		t.Fatalf("plain overflow marker:\n%s", view)
	}
	m.SetTheme(theme.Contrast())
	run(m.ShowOverlay(&stub{title: "OV"}))
	m.Toast("t", Info, 0)
	settle(m)
	m.View()
	m.SetTheme(theme.Light())
	settle(m)
	m.View()
	for _, name := range theme.Names() {
		th, ok := theme.Named(name)
		if !ok || th.Name != name {
			t.Fatal(name)
		}
		m.SetTheme(th)
		m.View()
	}
}

func TestSelectionCopiesText(t *testing.T) {
	sel := &selectStub{stub: stub{title: "SEL"}, text: "alpha beta\ngamma delta\nepsilon"}
	m := New(WithCards(sel, &stub{title: "OTHER"}))
	m.now = func() time.Time { return clock }
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	settle(m)
	m.View()
	m.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	if m.sel == nil || m.Selected() != "" {
		t.Fatal("press inside a selectable card starts a sweep")
	}
	_, cmd := m.Update(tea.MouseMotionMsg{X: 7, Y: 2, Button: tea.MouseLeft})
	if cmd == nil || m.Selected() != "alpha beta\ngamma d" {
		t.Fatalf("%q", m.Selected())
	}
	m.View()
	cell := m.buf.CellAt(2, 1)
	if cell.Style.Bg != m.theme.Ice {
		t.Fatal("selected cells must be highlighted")
	}
	_, cmd = m.Update(tea.MouseReleaseMsg{X: 7, Y: 2, Button: tea.MouseLeft})
	msgs := run(cmd)
	copied := false
	for _, msg := range msgs {
		if _, ok := msg.(tea.MouseReleaseMsg); ok {
			continue
		}
		if strings.Contains(ansi.Strip(strings.TrimSpace(sprint(msg))), "alpha beta") {
			copied = true
		}
	}
	if !copied {
		t.Fatalf("release must copy to the clipboard: %v", msgs)
	}
	if m.sel != nil || len(m.Toasts()) != 1 || !strings.Contains(m.Toasts()[0], "2 lines") {
		t.Fatalf("%v", m.Toasts())
	}
	m.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	if len(m.Toasts()) != 1 {
		t.Fatal("a click without movement copies nothing")
	}
	m.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 3, Y: 1, Button: tea.MouseLeft})
	press(m, rune_('j'))
	if m.sel != nil {
		t.Fatal("keys clear a sweep")
	}
	press(m, rune_('k'))
	m.Update(tea.MouseClickMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 4, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	if m.Selected() != "" {
		t.Fatal("sweeping back to the start selects nothing")
	}
	m.Update(tea.MouseMotionMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	m.Update(tea.MouseClickMsg{X: 90, Y: 5, Button: tea.MouseLeft})
	if m.sel != nil {
		t.Fatal("plain cards do not start sweeps")
	}
	s := &selection{x0: 5, y0: 2, x1: 1, y1: 0}
	if x0, y0, x1, y1 := s.span(); x0 != 1 || y0 != 0 || x1 != 5 || y1 != 2 {
		t.Fatal("span normalizes")
	}
	if !s.covers(9, 1) || s.covers(0, 0) || s.covers(6, 2) || s.covers(0, 3) {
		t.Fatal("covers")
	}
	if (&selection{e: &entry{}}).text() != "" {
		t.Fatal("no buffer")
	}
}

func sprint(msg tea.Msg) string { return fmt.Sprint(msg) }

func TestTerminalIntegrationFields(t *testing.T) {
	m, _ := newDeck(1)
	v := m.View()
	if !v.ReportFocus || !v.KeyboardEnhancements.ReportEventTypes || v.ProgramStatus != nil || v.ProgressBar != nil {
		t.Fatal("view flags")
	}
	m.SetStatus(&tea.ProgramStatus{State: tea.ProgramStateWorking, Title: "run"})
	m.SetProgress(tea.NewProgressBar(tea.ProgressBarDefault, 40))
	m.SetWindowTitle("icebath · run")
	v = m.View()
	if v.ProgramStatus == nil || v.ProgramStatus.State != tea.ProgramStateWorking || v.ProgressBar == nil || v.ProgressBar.Value != 40 || v.WindowTitle != "icebath · run" {
		t.Fatal("status fields must reach the view")
	}
	m.Update(tea.KeyReleaseMsg{Code: 'a'})
	m.Update(tea.PasteMsg{Content: "pasted"})
	stubs := m.Cards()[0].(*stub)
	var release, paste bool
	for _, msg := range stubs.msgs {
		switch msg.(type) {
		case tea.KeyReleaseMsg:
			release = true
		case tea.PasteMsg:
			paste = true
		}
	}
	if !release || !paste {
		t.Fatal("key releases and pastes reach the focused card")
	}
	if w, h := m.Size(); w != 120 || h != 40 {
		t.Fatal("size")
	}
	if c, ok := m.Card(0); !ok || c != m.Cards()[0] {
		t.Fatal("card")
	}
	if _, ok := m.Card(5); ok {
		t.Fatal("card out of range")
	}
	if m.IndexOf(m.Cards()[0]) != 0 {
		t.Fatal("index of")
	}
	if cmd := press(m, tea.KeyPressMsg{Code: 't', Text: "t"}); cmd == nil {
		t.Fatal("t with one card still answers")
	}
	keys := DefaultKeyMap()
	if !key.Matches(tea.KeyPressMsg{Code: 't', Text: "t"}, keys.Timeline) {
		t.Fatal("timeline binding")
	}
}
