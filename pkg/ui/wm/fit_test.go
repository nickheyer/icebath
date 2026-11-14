package wm

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// Card of a fixed least size for layout tests
type sized struct {
	name string
	w, h int
}

func (s sized) Title() string                  { return s.name }
func (s sized) Key() string                    { return s.name }
func (s sized) Init() tea.Cmd                  { return nil }
func (s sized) Update(tea.Msg) (Card, tea.Cmd) { return s, nil }
func (s sized) View(int, int) string           { return "" }
func (s sized) MinSize() (int, int)            { return s.w, s.h }

type floater struct{ sized }

func (floater) Floating() bool { return true }

// Opens the cards in the workspace one after another without animation
func fill(m *Model, ws *Workspace, cards ...Card) {
	for _, c := range cards {
		m.Open(ws, c)
	}
}

func TestFitsMasterStack(t *testing.T) {
	m := New(WithMotion(0))
	m.SetSize(120, 40)
	ws, _ := m.AddWorkspace("w")
	session := func(name string) Card { return sized{name: name, w: 30, h: 8} }
	if !m.Fits(ws, session("a")) {
		t.Fatal("an empty workspace refused a card")
	}
	fill(m, ws, session("a"))
	n := 1
	for ; n < 20 && m.Fits(ws, session(string(rune('a'+n)))); n++ {
		fill(m, ws, session(string(rune('a'+n))))
	}
	if n < 3 || n >= 20 {
		t.Fatalf("a 120 by 40 stage took %d sessions", n)
	}
	for _, e := range ws.live() {
		if c := m.contentRect(e.box); c.W < 30 || c.H < 8 {
			t.Fatalf("tile %s squeezed to %dx%d", e.key(), c.W, c.H)
		}
	}
	if !m.Fits(ws, ws.live()[0].card) {
		t.Fatal("a card already open does not fit where it is")
	}
	if !m.Fits(ws, floater{sized{name: "f", w: 30, h: 8}}) {
		t.Fatal("a float was refused")
	}
	if !m.Fits(ws, sized{name: "z"}) {
		t.Fatal("a card of no least size was refused")
	}
	m.SetSize(0, 0)
	if !m.Fits(ws, session("zz")) {
		t.Fatal("an unsized stage refused a card")
	}
	m.SetSize(120, 40)
	if m.Fits(ws, session("zz")) {
		t.Fatal("a full workspace took one more")
	}
	m.SetSize(400, 200)
	if !m.Fits(ws, session("zz")) {
		t.Fatal("a grown stage still refused a card")
	}
	if m.Fits(nil, session("zz")) || m.Fits(ws, nil) {
		t.Fatal("nil fitted")
	}
}

func TestFitsDwindle(t *testing.T) {
	m := New(WithMotion(0))
	m.SetSize(100, 30)
	ws, _ := m.AddWorkspace("w")
	m.SetLayout(Dwindle)
	wide := sized{name: "a", w: 40, h: 8}
	fill(m, ws, wide)
	if !m.Fits(ws, sized{name: "b", w: 40, h: 8}) {
		t.Fatal("a second tile beside a 40 column one does not fit in 96 columns")
	}
	fill(m, ws, sized{name: "b", w: 40, h: 8})
	if m.Fits(ws, sized{name: "c", w: 40, h: 20}) {
		t.Fatal("a tall tile fit under a half height cut")
	}
	if len(ws.live()) != 2 || ws.tree.leaves(nil)[0].card.(sized).name != "a" {
		t.Fatalf("the fit check changed the workspace: %d tiles", len(ws.live()))
	}
	if ws.Empty() {
		t.Fatal("a workspace with tiles reads empty")
	}
	for _, e := range ws.live() {
		m.Close(e.card)
	}
	if !ws.Empty() {
		t.Fatal("a workspace with every tile closed reads full")
	}
}
