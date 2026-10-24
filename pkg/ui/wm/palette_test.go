package wm

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

func TestFuzzyScoresAndHits(t *testing.T) {
	cases := []struct {
		query, text string
		hits        []int
		match       bool
	}{{"", "anything", nil, true}, {"wz", "Window: zoom", []int{0, 8}, true}, {"zoom", "Window: zoom", []int{8, 9, 10, 11}, true}, {"xq", "Window: zoom", nil, false}, {"WZ", "window zoom", []int{0, 7}, true}}
	for _, c := range cases {
		score, hits := fuzzy(c.query, c.text)
		if (score >= 0) != c.match {
			t.Fatalf("%q in %q scored %d", c.query, c.text, score)
		}
		if len(hits) != len(c.hits) {
			t.Fatalf("%q in %q hits %v", c.query, c.text, hits)
		}
		for i := range hits {
			if hits[i] != c.hits[i] {
				t.Fatalf("%q in %q hits %v", c.query, c.text, hits)
			}
		}
	}
	adjacent, _ := fuzzy("zo", "zoom")
	spread, _ := fuzzy("zo", "zip code")
	if adjacent <= spread {
		t.Fatal("adjacent hits score higher")
	}
}

func TestPaletteFiltersMovesAndPicks(t *testing.T) {
	var ran []string
	item := func(title string) Item {
		return Item{ID: title, Title: title, Run: func() tea.Cmd { ran = append(ran, title); return nil }}
	}
	p := NewPalette(theme.Blackwall(), []Item{item("Window: zoom"), item("Window: float"), item("Theme"), {ID: "detail", Title: "Quiet", Detail: "mission"}})
	if p.Rows() != 4 || len(p.Matches()) != 4 {
		t.Fatalf("rows %d matches %d", p.Rows(), len(p.Matches()))
	}
	p.Update(key('f'))
	p.Update(key('l'))
	if m := p.Matches(); len(m) != 1 || m[0].Title != "Window: float" || p.Query() != "fl" {
		t.Fatalf("%v", m)
	}
	p.Update(ActionMsg{ID: "palette.delete.back"})
	p.Update(ActionMsg{ID: "palette.delete.back"})
	p.Update(key('m'))
	p.Update(key('i'))
	if m := p.Matches(); len(m) != 1 || m[0].ID != "detail" {
		t.Fatalf("detail match %v", m)
	}
	p.Update(ActionMsg{ID: "palette.clear"})
	if p.Query() != "" || len(p.Matches()) != 4 {
		t.Fatal("clear empties the query")
	}
	p.Update(ActionMsg{ID: "palette.down"})
	p.Update(ActionMsg{ID: "palette.down"})
	p.Update(ActionMsg{ID: "palette.up"})
	if p.sel != 1 {
		t.Fatalf("sel %d", p.sel)
	}
	p.Update(ActionMsg{ID: "palette.page.down"})
	if p.sel != 3 {
		t.Fatalf("page down clamps %d", p.sel)
	}
	p.Update(ActionMsg{ID: "palette.page.up"})
	p.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	_, cmd := p.Update(ActionMsg{ID: "palette.pick"})
	msgs := collect(cmd, 50*time.Millisecond)
	if len(ran) != 1 || ran[0] != "Window: float" {
		t.Fatalf("%v", ran)
	}
	dismissed := false
	for _, msg := range msgs {
		if in, ok := msg.(Intent); ok && in.kind == intentDismiss {
			dismissed = true
		}
	}
	if !dismissed {
		t.Fatal("enter dismisses")
	}
	p.View(40, 10)
	_, cmd = p.Update(tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft})
	collect(cmd, 50*time.Millisecond)
	if len(ran) != 2 || ran[1] != "Theme" {
		t.Fatalf("click picks a row %v", ran)
	}
	p.Update(tea.PasteMsg{Content: "zo"})
	if p.Query() != "zo" || p.Matches()[0].Title != "Window: zoom" {
		t.Fatal("paste filters")
	}
	p.Update(ActionMsg{ID: "palette.left"})
	p.Update(key('x'))
	p.Update(ActionMsg{ID: "palette.home"})
	p.Update(ActionMsg{ID: "palette.delete.forward"})
	p.Update(ActionMsg{ID: "palette.end"})
	if p.Query() != "xo" || p.cursor != 2 {
		t.Fatalf("cursor edits %q %d", p.Query(), p.cursor)
	}
	p.Update(ActionMsg{ID: "palette.right"})
	p.Update(ActionMsg{ID: "palette.delete.word"})
	if p.Query() != "" {
		t.Fatal("delete word empties it")
	}
	if p.Contexts()[0] != "palette" {
		t.Fatal("context")
	}
	p.Update(key('q'))
	p.Update(key('q'))
	if len(p.Matches()) != 0 || p.Rows() != 1 || p.Tally() != "0/4" {
		t.Fatal("no match")
	}
	if _, caret := p.Input(20, theme.Blackwall().Surface); caret != 2 {
		t.Fatalf("caret follows the query %d", caret)
	}
}

func TestPaletteViewLightsHitsAndKeys(t *testing.T) {
	p := NewPalette(theme.Blackwall(), []Item{{ID: "a", Title: "Window: zoom", Keys: "alt+f"}, {ID: "b", Title: "Theme", Detail: "pick a palette"}})
	p.Placeholder = "type"
	bg := theme.Blackwall().Surface
	view := ansi.Strip(p.View(40, 10))
	lines := strings.Split(view, "\n")
	if input, _ := p.Input(20, bg); len(lines) != 2 || ansi.Strip(input) != "type" || !strings.Contains(lines[0], "alt+f") || !strings.Contains(lines[1], "pick a palette") {
		t.Fatalf("%q", view)
	}
	p.Update(key('z'))
	view = ansi.Strip(p.View(40, 10))
	if input, _ := p.Input(20, bg); ansi.Strip(input) != "z" || !strings.Contains(view, "z") || strings.Contains(view, "type") {
		t.Fatalf("%q", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > 40 {
			t.Fatalf("too wide %q", line)
		}
	}
	empty := NewPalette(theme.Blackwall(), nil)
	if empty.View(20, 5) != "" || empty.Rows() != 0 {
		t.Fatal("a prompt keeps no sheet")
	}
}

func TestPaletteWheelScrollsUnderAStillPointer(t *testing.T) {
	items := make([]Item, 40)
	for i := range items {
		items[i] = Item{ID: strconv.Itoa(i), Title: "item " + strconv.Itoa(i)}
	}
	p := NewPalette(theme.Blackwall(), items)
	p.View(40, 10)
	p.Update(tea.MouseMotionMsg{X: 3, Y: 3})
	for range 15 {
		p.Update(tea.MouseWheelMsg{X: 3, Y: 3, Button: tea.MouseWheelDown})
	}
	if p.top != 15 || p.sel != 18 || p.highlightRow() != 3 {
		t.Fatalf("the list scrolls under a highlight that stays with the pointer, top %d sel %d", p.top, p.sel)
	}
	p.Update(tea.MouseMotionMsg{X: 4, Y: 3})
	lines := strings.Split(ansi.Strip(p.View(40, 10)), "\n")
	if p.top != 15 || p.sel != 18 || !strings.Contains(lines[0], "item 15") || !strings.Contains(lines[3], "item 18") {
		t.Fatalf("a nudge of the pointer after scrolling leaves the list where it is, top %d sel %d", p.top, p.sel)
	}
	p.Update(ActionMsg{ID: "palette.up"})
	if p.top != 15 || p.sel != 17 {
		t.Fatal("moving inside the view leaves the list still")
	}
	for range 3 {
		p.Update(ActionMsg{ID: "palette.up"})
	}
	if p.top != 14 || p.sel != 14 {
		t.Fatalf("moving past the top scrolls one row, top %d sel %d", p.top, p.sel)
	}
	for range 40 {
		p.Update(tea.MouseWheelMsg{X: 3, Y: 3, Button: tea.MouseWheelDown})
	}
	if p.top != 30 || p.sel != 33 {
		t.Fatalf("the wheel stops at the last page, top %d sel %d", p.top, p.sel)
	}
	for range 40 {
		p.Update(tea.MouseWheelMsg{X: 3, Y: -1, Button: tea.MouseWheelUp})
	}
	if p.top != 0 || p.sel != 9 {
		t.Fatalf("off the rows the wheel keeps the selection in view, top %d sel %d", p.top, p.sel)
	}
	p.Update(key('3'))
	if p.top != 0 || len(p.Matches()) != 13 {
		t.Fatalf("filtering keeps the list inside its matches, top %d", p.top)
	}
	for range 5 {
		p.Update(tea.MouseWheelMsg{X: 3, Y: 0, Button: tea.MouseWheelDown})
	}
	if p.top != 3 || p.sel != 3 {
		t.Fatalf("a short list scrolls only as far as its last page, top %d sel %d", p.top, p.sel)
	}
}

func TestPromptHandsTextOver(t *testing.T) {
	var got string
	p := NewPrompt(theme.Blackwall(), "name", "init", func(text string) tea.Cmd { got = text; return nil })
	if p.Rows() != 0 || p.Query() != "init" || p.cursor != 4 {
		t.Fatal("prompt setup")
	}
	p.Update(key('!'))
	_, cmd := p.Update(ActionMsg{ID: "palette.pick"})
	collect(cmd, 50*time.Millisecond)
	if got != "init!" {
		t.Fatalf("%q", got)
	}
	bare := NewPalette(theme.Blackwall(), nil)
	_, cmd = bare.Update(ActionMsg{ID: "palette.pick"})
	msgs := collect(cmd, 50*time.Millisecond)
	if len(msgs) != 1 {
		t.Fatal("a prompt without a handler only dismisses")
	}
	quiet := NewPalette(theme.Blackwall(), []Item{{ID: "x", Title: "x"}})
	_, cmd = quiet.Update(ActionMsg{ID: "palette.pick"})
	if len(collect(cmd, 50*time.Millisecond)) != 1 {
		t.Fatal("an item without run only dismisses")
	}
	if p.Title() != "" || p.Key() != "palette" || p.Init() != nil {
		t.Fatal("identity")
	}
}

func TestTabsCycleClickAndShift(t *testing.T) {
	a, b, c := newStub("a"), newStub("b"), newStub("c")
	cur := sized{newStub("cur"), 10, 3}
	g := NewTabs(theme.Blackwall(), "CONFIG", "⚙", a, b, c, cur)
	if g.Title() != "CONFIG" || g.Glyph() != "⚙" || g.Key() != "CONFIG" || g.Len() != 4 {
		t.Fatal("identity")
	}
	g.Init()
	if a.inits != 1 || b.inits != 1 {
		t.Fatal("init reaches every tab")
	}
	g.Update(ActionMsg{ID: "tabs.next"})
	if active, i := g.Active(); active != b || i != 1 || !b.focused || a.focused {
		t.Fatal("tab cycles forward")
	}
	g.Update(ActionMsg{ID: "tabs.prev"})
	g.Update(ActionMsg{ID: "tabs.prev"})
	if _, i := g.Active(); i != 3 {
		t.Fatalf("shift+tab wraps %d", i)
	}
	g.Update(ActionMsg{ID: "tabs.next"})
	if _, i := g.Active(); i != 0 {
		t.Fatal("tab wraps")
	}
	if ctx := g.Contexts(); len(ctx) != 1 || ctx[0] != "tabs" {
		t.Fatalf("contexts %v", ctx)
	}
	g.Update(key('x'))
	if keys := received[tea.KeyPressMsg](a); len(keys) != 1 {
		t.Fatal("keys reach the active tab")
	}
	strip := ansi.Strip(strings.Split(g.View(60, 5), "\n")[0])
	if !strings.Contains(strip, "a") || !strings.Contains(strip, "c") {
		t.Fatalf("%q", strip)
	}
	g.Update(tea.MouseClickMsg{X: g.spans[2] + 1, Y: 0, Button: tea.MouseLeft})
	if _, i := g.Active(); i != 2 {
		t.Fatal("click on the strip selects")
	}
	g.Select(3)
	narrow := ansi.Strip(strings.Split(g.View(14, 5), "\n")[0])
	if !strings.HasPrefix(narrow, "❮ ") || strings.HasPrefix(narrow, "❮  ") || !strings.Contains(narrow, "cur") || g.first == 0 {
		t.Fatalf("a narrow strip scrolls to keep the active tab in view %q", narrow)
	}
	g.Update(tea.MouseClickMsg{X: g.spans[0] + 1, Y: 0, Button: tea.MouseLeft})
	if _, i := g.Active(); i != g.first {
		t.Fatal("clicks on a scrolled strip pick the tab drawn there")
	}
	g.SetTheme(theme.Plain())
	if plain := ansi.Strip(strings.Split(g.View(14, 5), "\n")[0]); !strings.HasPrefix(plain, "< ") || strings.HasPrefix(plain, "<  ") {
		t.Fatalf("the plain theme marks the scroll in ascii %q", plain)
	}
	g.SetTheme(theme.Blackwall())
	g.Select(0)
	if wide := ansi.Strip(strings.Split(g.View(60, 5), "\n")[0]); strings.HasPrefix(wide, "❮") || g.first != 0 {
		t.Fatalf("the strip scrolls back once the active tab is at the left %q", wide)
	}
	g.Select(2)
	g.Update(tea.MouseClickMsg{X: 4, Y: 1, Button: tea.MouseLeft})
	if clicks := received[tea.MouseClickMsg](c); len(clicks) != 0 {
		t.Fatalf("the padding under the strip takes no clicks %v", clicks)
	}
	g.Update(tea.MouseClickMsg{X: 4, Y: 3, Button: tea.MouseLeft})
	if clicks := received[tea.MouseClickMsg](c); len(clicks) != 1 || clicks[0].Y != 1 || clicks[0].X != 4 {
		t.Fatalf("rows shift up past the strip and its padding %v", clicks)
	}
	g.Update(tea.MouseWheelMsg{X: 1, Y: 2, Button: tea.MouseWheelDown})
	if wh := received[tea.MouseWheelMsg](c); len(wh) != 1 || wh[0].Y != 0 {
		t.Fatal("wheel shifts")
	}
	g.Update(tea.MouseClickMsg{X: 200, Y: 0, Button: tea.MouseLeft})
	if _, i := g.Active(); i != 2 {
		t.Fatal("click past the strip changes nothing")
	}
	if !g.Holds(c) || g.Holds(newStub("z")) || len(g.Cards()) != 4 {
		t.Fatal("holds")
	}
	g.Select(3)
	if g.Cursor() == nil || g.Cursor().Y != 4 || g.Cursor().X != 1 {
		t.Fatalf("cursor shifts down %v", g.Cursor())
	}
	if lines := strings.Split(g.View(60, 6), "\n"); len(lines) < 3 || strings.TrimSpace(ansi.Strip(lines[1])) != "" {
		t.Fatalf("a blank row pads the content from the strip %q", lines)
	}
	if w, h := g.MinSize(); w != 10 || h != 5 {
		t.Fatalf("min size %d %d", w, h)
	}
	if g.Selectable() || g.Status() != "" {
		t.Fatal("quiet tab")
	}
	g.Select(0)
	a.sel = true
	if !g.Selectable() {
		t.Fatal("selectable follows the active tab")
	}
	g.Update(FocusMsg{Focused: true})
	if !a.focused {
		t.Fatal("focus reaches the active tab")
	}
	type note struct{}
	g.Update(note{})
	if len(received[note](b)) != 1 {
		t.Fatal("other messages reach every tab")
	}
	g.SetTheme(theme.Light())
	if lines := strings.Split(g.View(60, 1), "\n"); len(lines) != 1 {
		t.Fatal("one row shows only the strip")
	}
	empty := NewTabs(theme.Blackwall(), "E", "e")
	if active, i := empty.Active(); active != nil || i != -1 || empty.Cursor() != nil || empty.View(10, 3) == "" {
		t.Fatal("empty tabs")
	}
	empty.Update(key('x'))
}

func TestTabsInsideTheManager(t *testing.T) {
	m := still(t, 100, 30)
	a, b := newStub("a"), newStub("b")
	g := NewTabs(theme.Blackwall(), "GROUP", "g", a, b)
	m.AddWorkspace("1", newStub("master"))
	m.OpenHere(g)
	if m.Focused() != g || m.entryOf(b) == nil {
		t.Fatal("a tab group holds its cards")
	}
	m.Focus(b)
	if m.Focused() != g {
		t.Fatal("focusing a held card focuses the group")
	}
	m.Update(key(tea.KeyTab))
	if _, i := g.Active(); i != 1 {
		t.Fatal("tab reaches the group through the keymap")
	}
	m.Update(key(tea.KeyTab, tea.ModShift))
	if _, i := g.Active(); i != 0 {
		t.Fatal("shift+tab goes back")
	}
	m.Update(key('x'))
	if keys := received[tea.KeyPressMsg](a); len(keys) != 1 {
		t.Fatal("unbound keys reach the active tab")
	}
}
