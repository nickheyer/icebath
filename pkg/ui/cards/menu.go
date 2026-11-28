package cards

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of a menu: the keys its areas answer, and those the page of an area answers
const (
	MenuAreasContext = "menu.areas"
	MenuPageContext  = "menu.page"
)

// Fewest content columns of a wide menu, a dialog of 100 columns, which draws its areas down the left beside the page in view
const menuWide = 96

// Columns and rows a menu takes on a screen with room to spare around it
const menuWidth, menuHeight = 128, 36

// A menu a caller describes: the areas to pick from and the page of each
type MenuSpec struct {
	Key string
	// The areas: a list to pick from, drawn down the left of a wide menu, and whole when the menu is not wide or is searched
	Areas *List
	// Id of the area on the row of Areas in hand, empty for a row that is no area, such as something a search found
	Area func() string
	// Page of an area, asked for the first time the area comes into view, nil for an area with none
	Page func(id string) (*Hub, tea.Cmd)
	// Columns the areas take beside a page: the longest name of an area or heading and its margins
	Rail func() int
}

// Dialog of areas, each with a page of its own: the areas down the left of a wide menu beside the page of the one in hand, one pane at a
// time in a narrow menu or while a search is typed. Moving through the areas shows each page at once, enter or → gives the page the
// keys, and esc or ← gives them back
type Menu struct {
	env    *Env
	spec   MenuSpec
	screen func() (int, int)
	// Hub of the areas list, where a view opened on something a search found stacks over the rows found
	areas *Hub
	// Pages built so far by the id of their area, nil for an area with none
	pages map[string]*Hub
	// Area whose page is in view, and an area to put the cursor of the areas on once their rows hold it
	view, want string
	// Whether the page in view has the keys
	inside bool
	// The person came straight to one job and did nothing else in the menu since
	direct bool
	shown  bool
	w, h   int
	// Columns the areas and the rule beside them took at the last paint, none while one pane filled the menu
	split int
}

// Window manager interfaces the menu answers through the pane that has the keys
var _ interface {
	wm.Card
	wm.Keyed
	wm.Glypher
	wm.Statuser
	wm.Cursorer
	wm.Hinter
	wm.Contexter
	wm.Raw
	wm.Selectable
	wm.Sized
	wm.Backer
	wm.Grabber
	wm.Answerer
	wm.Holder
} = (*Menu)(nil)

// Builds a menu over its areas, sized from the screen it floats over, the keys on the areas
func NewMenu(env *Env, spec MenuSpec, screen func() (int, int)) *Menu {
	m := &Menu{env: env, spec: spec, screen: screen, pages: map[string]*Hub{}}
	m.areas = NewHub(env, spec.Key, spec.Areas, screen)
	m.areas.menu = m
	return m
}

// Hub of the areas list, where a view opened on something a search found is stacked over the rows found
func (m *Menu) Home() *Hub { return m.areas }

// Whether the areas are drawn down the left now: the menu is wide and nothing is searched. The areas list reads it to choose its columns
func (m *Menu) Rail() bool {
	return m.wide() && m.areas.Depth() == 0 && m.spec.Areas.Query() == ""
}

// Whether the menu is wide enough to show its areas beside a page, judged by the size it opens at before its first paint
func (m *Menu) wide() bool {
	w := m.w
	if w == 0 {
		w, _ = m.MinSize()
	}
	return w >= menuWide
}

// Brings the page of the area into view, building it when it was not; inside gives the page the keys, else the areas keep them
func (m *Menu) Go(id string, inside bool) tea.Cmd {
	cmd := m.areas.PopTo(m.spec.Areas)
	if l := m.spec.Areas; l.Query() != "" {
		before := l.Query()
		l.bar.Close()
		l.refresh()
		cmd = tea.Batch(cmd, l.searchedSince(before))
	}
	m.want = ""
	if !m.point(id) {
		m.want = id
	}
	m.direct = false
	built := m.show(id)
	m.inside = inside && m.page() != nil
	return tea.Batch(cmd, built)
}

// Searches the areas for the words, as typing them into the areas does, the keys on the areas
func (m *Menu) Search(words string) tea.Cmd {
	cmd := m.areas.PopTo(m.spec.Areas)
	m.out()
	l := m.spec.Areas
	before := l.Query()
	l.bar.Close()
	l.Filter(words)
	return tea.Batch(cmd, l.searchedSince(before))
}

// Forgets the page of the area, so it is built again the next time it comes into view, such as to open it with another argument
func (m *Menu) Drop(id string) tea.Cmd {
	page, built := m.pages[id]
	if !built {
		return nil
	}
	delete(m.pages, id)
	if m.view == id {
		m.view, m.inside = "", false
	}
	if page == nil {
		return nil
	}
	// The page leaves the screen for good, so what its views started stops
	return m.send(page, wm.FocusMsg{})
}

// Page of the area as built, nil before it was
func (m *Menu) Page(id string) *Hub { return m.pages[id] }

// Says the person came straight to one job; cleared the moment they do anything else in the menu (move in the areas, open another view)
func (m *Menu) Direct() { m.direct = true }

// Page in view, nil for none
func (m *Menu) page() *Hub { return m.pages[m.view] }

// Pane that has the keys: the page in view once given them, else the areas
func (m *Menu) keyed() *Hub {
	if page := m.page(); m.inside && page != nil {
		return page
	}
	return m.areas
}

// Whether the hub is on screen: the hub of the areas while the menu shows, a page while it is also the one in view
func (m *Menu) shows(h *Hub) bool {
	return m.shown && (h == m.areas || h == m.page())
}

// Way out of the first view of a hub of the menu: from a page the keys go back to the areas, from the areas the menu closes
func (m *Menu) home(h *Hub) tea.Cmd {
	if h == m.areas {
		return wm.RequestDismissOf(m)
	}
	m.out()
	return nil
}

// Gives the page in view the keys
func (m *Menu) in() {
	if m.page() != nil {
		m.inside, m.direct = true, false
	}
}

// Gives the areas the keys
func (m *Menu) out() { m.inside, m.direct = false, false }

// Puts the page of the area in view, built and told it shows the first time it comes into view
func (m *Menu) show(id string) tea.Cmd {
	m.view = id
	if _, built := m.pages[id]; built || m.spec.Page == nil {
		return nil
	}
	page, cmd := m.spec.Page(id)
	m.pages[id] = page
	if page == nil {
		return cmd
	}
	page.menu = m
	cmds := []tea.Cmd{cmd, page.Init()}
	if m.shown {
		cmds = append(cmds, m.send(page, wm.FocusMsg{Focused: true}))
	}
	return tea.Batch(cmds...)
}

// Puts the cursor of the areas on the row of the area, reporting whether a row holds it
func (m *Menu) point(id string) bool {
	if m.spec.Area() == id {
		return true
	}
	l := m.spec.Areas
	at := l.sel
	for i := range l.data {
		if l.selectRow(i) && m.spec.Area() == id {
			return true
		}
	}
	l.sel = at
	return false
}

// Shows the page of the area in hand as the cursor moves through the areas, or puts the cursor on the area asked for once the rows hold it
func (m *Menu) follow() tea.Cmd {
	if m.want != "" {
		if m.point(m.want) || len(m.spec.Areas.data) > 0 {
			m.want = ""
		}
		return nil
	}
	if m.inside || m.areas.Depth() > 0 {
		return nil
	}
	id := m.spec.Area()
	if id == "" || id == m.view {
		return nil
	}
	m.direct = false
	return m.show(id)
}

// Message handed to a hub of the menu
func (m *Menu) send(h *Hub, msg tea.Msg) tea.Cmd {
	_, cmd := h.Update(msg)
	return cmd
}

// Message handed to the pane that has the keys, the page following the cursor of the areas
func (m *Menu) toKeyed(msg tea.Msg) tea.Cmd {
	cmd := m.send(m.keyed(), msg)
	return tea.Batch(cmd, m.follow())
}

// Hub of the menu that is c or holds it, nil for none
func (m *Menu) holder(c wm.Card) *Hub {
	if wm.Card(m.areas) == c || m.areas.Holds(c) {
		return m.areas
	}
	for _, page := range m.pages {
		if page != nil && (wm.Card(page) == c || page.Holds(c)) {
			return page
		}
	}
	return nil
}

// Reports whether the card is one of the menu's hubs or a view stacked on one
func (m *Menu) Holds(c wm.Card) bool { return m.holder(c) != nil }

// Coming on screen tells the areas and every page built that they show; leaving it tells them all they closed
func (m *Menu) focus(on bool) tea.Cmd {
	m.shown = on
	cmds := []tea.Cmd{m.send(m.areas, wm.FocusMsg{Focused: on})}
	for _, page := range m.pages {
		// A page built while the menu showed was told so then
		if page != nil && (!on || !page.shown) {
			cmds = append(cmds, m.send(page, wm.FocusMsg{Focused: on}))
		}
	}
	return tea.Batch(append(cmds, m.follow())...)
}

func (m *Menu) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.FocusMsg:
		return m, m.focus(msg.Focused)
	case wm.ActionMsg:
		switch msg.ID {
		case "menu.in":
			m.in()
			return m, nil
		case "menu.out":
			m.out()
			return m, nil
		}
		return m, m.toKeyed(msg)
	case tea.KeyPressMsg, tea.KeyReleaseMsg, tea.PasteMsg, tea.ClipboardMsg:
		return m, m.toKeyed(msg)
	case tea.MouseMsg:
		return m, m.mouse(msg)
	case wm.Addressed:
		if hub := m.holder(msg.Addressee()); hub != nil {
			return m, tea.Batch(m.send(hub, msg), m.follow())
		}
		return m, nil
	}
	// Anything else is news for every part of the menu
	cmds := []tea.Cmd{m.send(m.areas, msg)}
	for _, page := range m.pages {
		if page != nil {
			cmds = append(cmds, m.send(page, msg))
		}
	}
	return m, tea.Batch(append(cmds, m.follow())...)
}

// Hands the pointer to the pane under it, moved to that pane's own columns. A click in the pane that does not have the keys gives it the
// keys first; the pointer doing anything else there is left alone
func (m *Menu) mouse(msg tea.MouseMsg) tea.Cmd {
	if m.split == 0 {
		return m.toKeyed(msg)
	}
	mo := msg.Mouse()
	target := m.areas
	switch rail := m.split - 1; {
	case mo.X == rail:
		return nil
	case mo.X > rail:
		target = m.page()
		mo.X -= m.split
	}
	if target == nil {
		return nil
	}
	if target != m.keyed() {
		if _, click := msg.(tea.MouseClickMsg); !click {
			return nil
		}
		if target == m.areas {
			m.out()
		} else {
			m.in()
		}
	}
	return tea.Batch(m.send(target, relocated(msg, mo)), m.follow())
}

// Mouse message of the same kind at another position
func relocated(msg tea.MouseMsg, mo tea.Mouse) tea.Msg {
	switch msg.(type) {
	case tea.MouseClickMsg:
		return tea.MouseClickMsg(mo)
	case tea.MouseReleaseMsg:
		return tea.MouseReleaseMsg(mo)
	case tea.MouseWheelMsg:
		return tea.MouseWheelMsg(mo)
	}
	return tea.MouseMotionMsg(mo)
}

// The title of the areas while they have the keys, and from a page the mark that steps back and the trail from the areas through every view
// of the page, each named once and the middle folded when the trail is too long
func (m *Menu) Title() string {
	page := m.page()
	if !m.inside || page == nil {
		return m.areas.Title()
	}
	return trail(m.env.Theme.Glyphs, page.names([]string{m.spec.Areas.Title()}), m.w)
}

func (m *Menu) Glyph() string    { return m.areas.Glyph() }
func (m *Menu) Key() string      { return m.spec.Key }
func (m *Menu) Init() tea.Cmd    { return m.areas.Init() }
func (m *Menu) Selectable() bool { return true }

// Opens up to its full size in the middle of a large screen and over the whole of a small one, with the same room to spare a hub needs
func (m *Menu) MinSize() (int, int) {
	sw, sh := 120, 40
	if m.screen != nil {
		sw, sh = m.screen()
	}
	w, h := menuWidth, menuHeight
	if sw-w < 2*hubMarginX {
		w = sw
	}
	if sh-h < 2*hubMarginY {
		h = sh
	}
	return w, h
}

// Keymap contexts of the pane that has the keys, after the menu's own for that pane
func (m *Menu) Contexts() []string {
	own := MenuAreasContext
	if m.keyed() != m.areas {
		own = MenuPageContext
	}
	return append([]string{own}, m.keyed().Contexts()...)
}

// Whether the menu has a use for the action: → only on an area with a page, nothing typed into the search, ← only from the first view of a
// page that is a list with nothing typed into it and no question, key or cell in hand. Anything else as the pane with the keys answers it
func (m *Menu) Answers(action string) bool {
	switch action {
	case "menu.in":
		id := m.spec.Area()
		return m.keyed() == m.areas && m.areas.Depth() == 0 && m.spec.Areas.Query() == "" && id != "" && m.pages[id] != nil
	case "menu.out":
		page := m.page()
		if !m.inside || page == nil || page.Depth() > 0 {
			return false
		}
		l, ok := page.Top().(*List)
		return ok && l.idle()
	}
	return m.keyed().Answers(action)
}

func (m *Menu) Hints() []wm.Hint    { return m.keyed().Hints() }
func (m *Menu) Raw() bool           { return m.keyed().Raw() }
func (m *Menu) Status() string      { return m.keyed().Status() }
func (m *Menu) Grabs(x, y int) bool { return m.keyed().Grabs(x-m.offset(), y) }

// Columns the pane that has the keys starts at
func (m *Menu) offset() int {
	if m.keyed() == m.areas {
		return 0
	}
	return m.split
}

func (m *Menu) Cursor() *tea.Cursor {
	c := m.keyed().Cursor()
	if c != nil {
		c.X += m.offset()
	}
	return c
}

// Esc while the pane with the keys takes typing and has no step back of its own: a view of the pane closes, the first view of a page
// gives the keys back to the areas, and the areas let the menu close
func (m *Menu) Back() bool {
	pane := m.keyed()
	if pane.Back() {
		return true
	}
	if pane != m.areas {
		m.out()
		return true
	}
	return false
}

// The areas down the left of a wide menu that is not searched, a rule, then the page in view; else the pane that has the keys alone. The
// row in hand is lit where the keys are and only marked in the other pane
func (m *Menu) View(w, h int) string {
	m.w, m.h = w, h
	page := m.page()
	m.spec.Areas.Rest(m.inside && page != nil)
	if page != nil {
		if l, ok := page.Top().(*List); ok {
			l.Rest(!m.inside)
		}
	}
	m.split = 0
	if !m.Rail() {
		return m.keyed().View(w, h)
	}
	t := m.env.Theme
	rail := min(max(m.spec.Rail(), 1), max(w-2, 1))
	m.split = rail + 1
	left := strings.Split(m.areas.View(rail, h), "\n")
	var right []string
	if page != nil {
		right = strings.Split(page.View(w-m.split, h), "\n")
	}
	bar := "│"
	if t.Plain {
		bar = "|"
	}
	rule := render.Dim(bar, t)
	lines := make([]string, h)
	for i := range lines {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		lines[i] = render.Fit(l, rail, t) + rule + render.Fit(r, w-m.split, t)
	}
	return strings.Join(lines, "\n")
}
