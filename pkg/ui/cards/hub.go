package cards

import (
	"cmp"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the row keys every screen of things to manage answers the same way
const HubContext = "hub"

// Keymap context of the keys a hub answers on every view that takes no typing, stepping back and reading again
const HubViewContext = "hub.view"

// Keys shared by every screen of things to manage, registered with the keymap of the app
func RegisterHubKeys(km *keys.Map) {
	for name, doc := range map[string]string{HubContext: "dialog rows", HubViewContext: "dialogs", MenuAreasContext: "the areas of a menu",
		MenuPageContext: "the page of a menu"} {
		if keys.ContextDocs[name] == "" {
			if err := keys.AddContext(name, doc); err != nil {
				panic(err)
			}
		}
	}
	for _, a := range []keys.Action{
		{ID: "hub.use", Context: HubContext, Label: "open", Doc: "Open or use the selected row", Default: []string{"enter"}, Quiet: true},
		{ID: "hub.add", Context: HubContext, Label: "add", Doc: "Add a new item", Default: []string{"alt+n"}, Quiet: true},
		{ID: "hub.edit", Context: HubContext, Label: "edit", Doc: "Edit the selected row", Default: []string{"ctrl+e"}, Quiet: true},
		{ID: "hub.rename", Context: HubContext, Label: "rename", Doc: "Rename the selected row", Default: []string{"f2"}, Quiet: true},
		{ID: "hub.remove", Context: HubContext, Label: "remove", Doc: "Remove the selected row", Default: []string{"ctrl+d"}, Quiet: true},
		{ID: "hub.up", Context: HubContext, Label: "move up", Doc: "Move the selected row up a list whose order matters", Default: []string{"shift+up"},
			Quiet: true},
		{ID: "hub.down", Context: HubContext, Label: "move down", Doc: "Move the selected row down a list whose order matters",
			Default: []string{"shift+down"}, Quiet: true},
		{ID: "hub.back", Context: HubViewContext, Label: "back", Doc: "Go back, or close the dialog", Default: []string{"esc"}, Quiet: true},
		{ID: "hub.refresh", Context: HubViewContext, Label: "refresh", Doc: "Reload the rows", Default: []string{"ctrl+r"}, Quiet: true},
		{ID: "menu.in", Context: MenuAreasContext, Label: "open", Doc: "Give the keys to the page of the area", Default: []string{"right"}, Quiet: true},
		{ID: "menu.out", Context: MenuPageContext, Label: "back", Doc: "Give the keys back to the areas", Default: []string{"left"}, Quiet: true},
	} {
		km.Register(a)
	}
	registerFormKeys(km)
}

// Weight of the line a hub shows under its view
type NoteLevel int

const (
	// What the screen says about itself, cleared after a while
	NoteInfo NoteLevel = iota
	// Work in flight, kept until the next note
	NoteBusy
	// Work that finished, cleared after a while
	NoteDone
	// Work that failed, kept until the next note or action
	NoteFailed
)

// How long a hub keeps a note that only reports, before it clears it
const noteTTL = 6 * time.Second

// Clears the note of a hub once its time is up, unless a later note replaced it
type hubNoteExpired struct {
	hub *Hub
	gen int
}

func (m hubNoteExpired) Addressee() wm.Card { return m.hub }

// Screen of things to manage: views stacked over one root list, the same keys on each, with a note line along the bottom for progress and errors
type Hub struct {
	env   *Env
	key   string
	stack []wm.Card
	note  string
	level NoteLevel
	gen   int
	// Size of the screen the hub floats over, so it opens wide enough to read
	screen func() (int, int)
	// Run once the hub left the screen
	onClose []func() tea.Cmd
	shown   bool
	// Held beneath a palette or another overlay instead of dropped, for a question that waits for its answer
	sticky bool
	w, h   int
	// Menu the hub sits in, as the page of an area or as the hub of its areas, nil for a dialog of its own
	menu *Menu
	// The next view pushed is the job the person came straight to
	job bool
	// Runs once the hub holds depth views, once
	next  func() tea.Cmd
	depth int
}

// Window manager interfaces the hub answers through the view on top
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
} = (*Hub)(nil)

// Builds a hub over its root view, sized from the screen it floats over
func NewHub(env *Env, key string, root wm.Card, screen func() (int, int)) *Hub {
	return &Hub{env: env, key: key, stack: []wm.Card{root}, screen: screen}
}

// Keeps the hub as a question: another overlay holds it beneath instead of dropping it, and only an answer or esc closes it
func (h *Hub) Stick() { h.sticky = true }

// Whether the hub waits beneath other overlays
func (h *Hub) Sticky() bool { return h.sticky }

// Runs fn once the hub leaves the screen, after the ones added before
func (h *Hub) OnClose(fn func() tea.Cmd) { h.onClose = append(h.onClose, fn) }

// View on top, the one taking the keys
func (h *Hub) Top() wm.Card { return h.stack[len(h.stack)-1] }

// Root view of the hub
func (h *Hub) Root() wm.Card { return h.stack[0] }

// Reports whether the view is stacked on the hub, or is a step of a view stacked on it
func (h *Hub) Holds(c wm.Card) bool { return h.holder(c) != nil }

// View stacked on the hub that is c or holds it, nil for none
func (h *Hub) holder(c wm.Card) wm.Card {
	for _, view := range h.stack {
		if view == c {
			return view
		}
		if steps, ok := view.(*Steps); ok && slices.Contains(steps.steps, c) {
			return view
		}
	}
	return nil
}

// Number of views stacked over the root
func (h *Hub) Depth() int { return len(h.stack) - 1 }

// Reports whether the hub is on screen: a dialog of its own while the window manager shows it, the page of an area while its menu shows
// and the page is the one in view
func (h *Hub) Shown() bool {
	if h.menu != nil {
		return h.menu.shows(h)
	}
	return h.shown
}

// Puts a view on top, the one under it waiting until it closes
func (h *Hub) Push(c wm.Card) tea.Cmd {
	h.opened()
	cmds := []tea.Cmd{h.send(h.Top(), wm.FocusMsg{})}
	h.stack = append(h.stack, c)
	cmds = append(cmds, c.Init(), h.send(c, wm.FocusMsg{Focused: true}))
	if next := h.next; next != nil && len(h.stack) >= h.depth {
		h.next = nil
		cmds = append(cmds, next())
	}
	return tea.Batch(cmds...)
}

// Runs then once the hub holds depth views, its first counted, at once when it does already: such as to put a control over the page a
// screen pushes, with the pages above it inside the area, once it read what they show
func (h *Hub) At(depth int, then func() tea.Cmd) tea.Cmd {
	if len(h.stack) >= depth {
		return then()
	}
	h.next, h.depth = then, depth
	return nil
}

// A job of one pick is done once the value picked is kept: in a menu the person came straight into for it and did nothing else in since,
// the menu closes; nothing else closes, the picker having closed its own list
func (h *Hub) Picked() tea.Cmd {
	if h.menu != nil && h.menu.direct {
		return h.Leave()
	}
	return nil
}

// Another view opened in the menu the hub sits in, so the job the person came straight to is no longer all they did there; the view of
// that job itself, when it is the first pushed since Direct, leaves it so
func (h *Hub) opened() {
	switch {
	case h.job:
		h.job = false
	case h.menu != nil:
		h.menu.direct = false
	}
}

// View the person works in on top: the step in view of a job in steps, else the view on top itself
func (h *Hub) face() wm.Card {
	if steps, ok := h.Top().(*Steps); ok && len(steps.steps) > 0 {
		return steps.Step()
	}
	return h.Top()
}

// The job the person came for is done. In a menu they came straight into for this job and did nothing else in since, the menu closes;
// otherwise the view closes, nothing closing for no view. A dialog of its own closes itself for no view or its first
func (h *Hub) Finish(view wm.Card) tea.Cmd {
	switch {
	case h.menu != nil && h.menu.direct:
		return h.Leave()
	case h.menu == nil && (view == nil || view == h.Root()):
		return h.Leave()
	case view == nil:
		return nil
	}
	return h.Close(view)
}

// Says the person came straight to one job on the hub, as a command that names one job does: the job of the next view pushed, which may
// come once something is read, or of the view on top when none is. In a menu, Finish then closes the menu unless they do something else
// there first. A dialog of its own is unchanged
func (h *Hub) Direct() {
	if h.menu != nil {
		h.menu.Direct()
		h.job = true
	}
}

// Closes the dialog the hub is in: the hub itself as a dialog of its own, else the menu it sits in. A note it shows that says what was done
// is said again as a toast, since the dialog goes with it
func (h *Hub) Leave() tea.Cmd {
	var dialog wm.Card = h
	if h.menu != nil {
		dialog = h.menu
	}
	return tea.Batch(h.toast(), wm.RequestDismissOf(dialog))
}

// The note under the view as a toast, nothing for none or for work still in flight
func (h *Hub) toast() tea.Cmd {
	if h.note == "" {
		return nil
	}
	switch h.level {
	case NoteDone:
		return wm.Notify(h.note, wm.Success)
	case NoteFailed:
		return wm.Notify(h.note, wm.Error)
	case NoteInfo:
		return wm.Notify(h.note, wm.Info)
	}
	return nil
}

// View of a hub that is told when it leaves the hub for good, by its way back, by the code that closes it or with the hub itself, so
// it stops what it started
type Closer interface {
	Closed() tea.Cmd
}

// Tells a view that leaves the hub so, when it asks to be told
func closed(c wm.Card) tea.Cmd {
	if closer, ok := c.(Closer); ok {
		return closer.Closed()
	}
	return nil
}

// Closes the view on top and gives the keys back to the one under it, the root staying
func (h *Hub) Pop() tea.Cmd {
	if len(h.stack) == 1 {
		return nil
	}
	top := h.Top()
	h.stack = h.stack[:len(h.stack)-1]
	return tea.Batch(h.send(top, wm.FocusMsg{}), closed(top), h.send(h.Top(), wm.FocusMsg{Focused: true}))
}

// Closes every view down to c, c staying on top, nothing when c is not stacked
func (h *Hub) PopTo(c wm.Card) tea.Cmd {
	i := slices.Index(h.stack, c)
	if i < 0 || i == len(h.stack)-1 {
		return nil
	}
	var cmds []tea.Cmd
	for len(h.stack) > i+1 {
		cmds = append(cmds, h.Pop())
	}
	return tea.Batch(cmds...)
}

// Puts c in the place of the view on top, such as another tab of the root, the views keeping their order
func (h *Hub) Swap(c wm.Card) tea.Cmd {
	h.opened()
	top := h.Top()
	h.stack[len(h.stack)-1] = c
	return tea.Batch(h.send(top, wm.FocusMsg{}), closed(top), c.Init(), h.send(c, wm.FocusMsg{Focused: true}))
}

// Closes c wherever it is stacked, the root staying
func (h *Hub) Close(c wm.Card) tea.Cmd {
	i := slices.Index(h.stack, c)
	switch {
	case i <= 0:
		return nil
	case i == len(h.stack)-1:
		return h.Pop()
	}
	h.stack = slices.Delete(h.stack, i, i+1)
	return tea.Batch(h.send(c, wm.FocusMsg{}), closed(c))
}

// Shows a note under the view, a report clearing itself after a while
func (h *Hub) Say(text string, level NoteLevel) tea.Cmd {
	h.gen++
	h.note, h.level = text, level
	if level != NoteInfo && level != NoteDone {
		return nil
	}
	gen := h.gen
	return tea.Tick(noteTTL, func(time.Time) tea.Msg { return hubNoteExpired{hub: h, gen: gen} })
}

// Shows work in flight under the view
func (h *Hub) Busy(text string) { h.Say(text, NoteBusy) }

// Shows why something failed under the view
func (h *Hub) Fail(text string) tea.Cmd { return h.Say(text, NoteFailed) }

// Shows that something finished under the view
func (h *Hub) Done(text string) tea.Cmd { return h.Say(text, NoteDone) }

// Clears the note under the view
func (h *Hub) Quiet() {
	h.gen++
	h.note = ""
}

// Reports whether work the hub started is still in flight
func (h *Hub) Waiting() bool { return h.note != "" && h.level == NoteBusy }

// Message handed to a view of the hub, keeping the card it answers with in its place
func (h *Hub) send(c wm.Card, msg tea.Msg) tea.Cmd {
	next, cmd := c.Update(msg)
	if i := slices.Index(h.stack, c); i >= 0 && next != nil {
		h.stack[i] = next
	}
	return cmd
}

// Fewest cells the trail of a hub keeps before its middle folds, and what the frame around the title takes of the width
const trailLeast, trailChrome = 24, 28

// Title of the first view, and from a view opened over it the mark that steps back and the whole trail to that view, each view named
// once. A trail too long for the width folds its middle and keeps the first and the last name
func (h *Hub) Title() string {
	names := h.names(nil)
	if len(names) == 0 {
		return ""
	}
	if len(h.stack) == 1 {
		return names[0]
	}
	return trail(h.env.Theme.Glyphs, names, h.w)
}

// Names after those given of every view stacked on the hub, each named once
func (h *Hub) names(names []string) []string {
	for _, view := range h.stack {
		if title := view.Title(); title != "" && (len(names) == 0 || names[len(names)-1] != title) {
			names = append(names, title)
		}
	}
	return names
}

// The mark that steps back and the names of a trail from the first view to the one in hand. A trail too long for the width folds its
// middle and keeps the first and the last name; before the first paint the width is not known and nothing folds
func trail(g theme.Glyphs, names []string, w int) string {
	names = slices.Clone(names)
	sep := " " + g.Forward + " "
	said := func() string { return g.Back + " " + strings.Join(names, sep) }
	room := max(w-trailChrome, trailLeast)
	for w > 0 && len(names) > 2 && ansi.StringWidth(said()) > room {
		if names[1] != g.Ellipsis {
			names[1] = g.Ellipsis
			continue
		}
		if len(names) == 3 {
			break
		}
		names = slices.Delete(names, 2, 3)
	}
	return said()
}

func (h *Hub) Glyph() string {
	if g, ok := h.Root().(wm.Glypher); ok {
		return g.Glyph()
	}
	return ""
}

func (h *Hub) Key() string { return h.key }

func (h *Hub) Init() tea.Cmd { return h.Root().Init() }

func (h *Hub) Selectable() bool { return true }

// Columns and rows a hub takes on a screen with room to spare around it
const hubWidth, hubHeight = 104, 30

// Least the screen must show around a hub for it to float; with less it covers the screen, since a sliver of what lies beneath only reads as noise
const hubMarginX, hubMarginY = 12, 3

// View of a hub that knows how many rows it fills at a width, so the hub opens no taller than what it holds
type Fitted interface {
	Rows(w int) int
}

// Fewest rows a hub opens with, room for an empty view to say so
const hubLeast = 7

// Opens at a readable width in the middle of a large screen and over the whole of a small one, as tall as the tallest of its views needs
func (h *Hub) MinSize() (int, int) {
	sw, sh := 120, 40
	if h.screen != nil {
		sw, sh = h.screen()
	}
	w := hubWidth
	if sw-w < 2*hubMarginX {
		w = sw
	}
	ht := hubLeast
	for _, view := range h.stack {
		rows := hubHeight
		if f, ok := view.(Fitted); ok {
			rows = f.Rows(cmp.Or(h.w, w))
		}
		ht = max(ht, min(rows, hubHeight))
	}
	// A row for the note of the hub, so one arriving moves nothing
	if ht++; sh-ht < 2*hubMarginY {
		ht = sh
	}
	return w, ht
}

// Every key as typed while the view on top asks for it
func (h *Hub) Raw() bool {
	r, ok := h.Top().(wm.Raw)
	return ok && r.Raw()
}

// Keymap contexts of the view on top, with going back and reloading while it takes no typing, or while the typing it takes only filters a
// list to pick from; a key being recorded, a question or a cell being edited keeps them out
func (h *Hub) Contexts() []string {
	var out []string
	if c, ok := h.Top().(wm.Contexter); ok {
		out = c.Contexts()
	}
	// A form answers esc itself and may take any letter, so the hub's own keys stay out of it
	_, form := h.face().(*Form)
	list, listed := h.face().(*List)
	filtering := listed && list.Picks() && slices.Contains(out, keys.Search)
	if !form && (filtering || !takesTyping(out)) {
		out = append(slices.Clone(out), HubViewContext)
	}
	return out
}

// Whether the view on top has a use for the action, so a key it leaves unused goes on to the next action bound to it
func (h *Hub) Answers(action string) bool {
	a, ok := h.Top().(wm.Answerer)
	return !ok || a.Answers(action)
}

// Whether a view with these contexts takes typed text, so letter keys must reach it
func takesTyping(ctxs []string) bool {
	for _, c := range ctxs {
		switch c {
		case keys.Edit, keys.Search, keys.Confirm, keys.Capture, keys.Text, FormTextContext:
			return true
		}
	}
	return false
}

// Buttons of the view on top, the way out last: cancel on a form, back from a view opened over another and close from the first, back from
// the first view of the page of an area too, which gives the keys back to the areas. A view that asks a question or records a key names
// its own way out
func (h *Hub) Hints() []wm.Hint {
	var hints []wm.Hint
	if c, ok := h.Top().(wm.Hinter); ok {
		hints = c.Hints()
	}
	out, label := "hub.back", "back"
	if len(h.stack) == 1 && (h.menu == nil || h.menu.areas == h) {
		label = "close"
	}
	switch top := h.face().(type) {
	case *Form:
		out, label = "form.cancel", "cancel"
	case *Page:
		if top.ask != nil {
			return hints
		}
	case *List:
		if top.capture != nil || top.ask != nil || top.edit != nil {
			return hints
		}
		// Esc in a filter clears what was typed before it leaves
		if top.pick || top.bar.Active() {
			out = "search.close"
			if top.Query() != "" || !top.pick {
				label = "clear search"
			}
		}
	}
	if slices.ContainsFunc(hints, func(hint wm.Hint) bool { return hint.Action == out }) {
		return hints
	}
	return append(slices.Clone(hints), wm.Hint{Action: out, Label: label})
}

func (h *Hub) Status() string {
	if s, ok := h.Top().(wm.Statuser); ok {
		return s.Status()
	}
	return ""
}

func (h *Hub) Cursor() *tea.Cursor {
	if c, ok := h.Top().(wm.Cursorer); ok {
		return c.Cursor()
	}
	return nil
}

func (h *Hub) Grabs(x, y int) bool {
	g, ok := h.Top().(wm.Grabber)
	return ok && g.Grabs(x, y)
}

// Esc on the hub while its view takes typing and has no step back of its own: a job in steps goes back a step, a stacked view closes, the
// root lets the hub close
func (h *Hub) Back() bool {
	if steps, ok := h.Top().(*Steps); ok {
		if cmd, stepped := steps.back(); stepped {
			h.env.run(cmd)
			return true
		}
	}
	if len(h.stack) == 1 {
		return false
	}
	h.env.run(h.Pop())
	return true
}

// One step back: the view on top steps back first, a filter clears, then the view on top goes away
func (h *Hub) back() tea.Cmd {
	if b, ok := h.Top().(wm.Backer); ok && b.Back() {
		return nil
	}
	if l, ok := h.face().(*List); ok && l.bar.Shown() && !l.pick {
		before := l.Query()
		l.bar.Close()
		l.refresh()
		return l.searchedSince(before)
	}
	return h.away()
}

// The view on top goes away: a job in steps goes back a step, a stacked view closes, the first view of the page of an area gives the keys
// back to the areas, and the first of any other hub closes the dialog
func (h *Hub) away() tea.Cmd {
	if steps, ok := h.Top().(*Steps); ok {
		if cmd, stepped := steps.back(); stepped {
			return cmd
		}
	}
	switch {
	case len(h.stack) > 1:
		return h.Pop()
	case h.menu != nil:
		return h.menu.home(h)
	}
	return wm.RequestDismissOf(h)
}

func (h *Hub) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case hubNoteExpired:
		if msg.gen == h.gen {
			h.note = ""
		}
		return h, nil
	case wm.Addressed:
		// A message for one of the views goes to it, wherever it is stacked
		if view := h.holder(msg.Addressee()); view != nil {
			return h, h.send(view, msg)
		}
		return h, nil
	case wm.FocusMsg:
		// A hub is an overlay the window manager drops when it loses the focus, so losing it means leaving the screen
		h.shown = msg.Focused
		cmds := []tea.Cmd{h.send(h.Top(), msg)}
		if !msg.Focused {
			for _, view := range h.stack {
				cmds = append(cmds, closed(view))
			}
			for _, fn := range h.onClose {
				cmds = append(cmds, fn())
			}
		}
		return h, tea.Batch(cmds...)
	case wm.ActionMsg:
		switch msg.ID {
		case "hub.back":
			return h, h.back()
		case "form.cancel":
			if f, ok := h.face().(*Form); ok && f.spec.Cancel == nil {
				return h, h.back()
			}
		case "search.close":
			// Esc on a list to pick from clears what was typed first, then goes back, closing the hub from its first view
			if l, ok := h.face().(*List); ok && l.pick {
				if before := l.Query(); before != "" {
					l.bar.Close()
					l.refresh()
					return h, l.searchedSince(before)
				}
				return h, h.away()
			}
		}
		if h.level == NoteFailed {
			h.Quiet()
		}
	case tea.MouseMsg:
		if msg.Mouse().Y >= h.bodyRows() {
			return h, nil
		}
	}
	return h, h.send(h.Top(), msg)
}

// Whether the view the person works in asks a question along its bottom
func (h *Hub) asking() bool {
	switch face := h.face().(type) {
	case *List:
		return face.ask != nil
	case *Page:
		return face.ask != nil
	}
	return false
}

// Rows the view on top paints, the last ones kept for the note while there is one
func (h *Hub) bodyRows() int {
	return max(h.h-len(h.noteLines(h.w)), 0)
}

func (h *Hub) View(w, ht int) string {
	h.w, h.h = w, ht
	// A question asked along the bottom of the view replaces the note, which no longer says anything of what is in hand
	if h.note != "" && h.asking() {
		h.Quiet()
	}
	body := h.bodyRows()
	lines := strings.Split(h.Top().View(w, body), "\n")
	lines = lines[:min(len(lines), body)]
	for len(lines) < body {
		lines = append(lines, "")
	}
	return strings.Join(append(lines, h.noteLines(w)...), "\n")
}

// Note along the bottom in the colour of its weight, wrapped so it is read whole, on no more lines than leave the view half its rows
func (h *Hub) noteLines(w int) []string {
	if h.note == "" {
		return nil
	}
	t := h.env.Theme
	paint := render.Dim
	switch h.level {
	case NoteBusy:
		paint = render.Warm
	case NoteDone:
		paint = render.Good
	case NoteFailed:
		paint = render.Bad
	}
	parts := wrapWords(h.note, max(w-2, 8))
	parts = parts[:min(len(parts), max(h.h/2, 1))]
	lines := make([]string, len(parts))
	for i, part := range parts {
		lines[i] = render.Fit(" "+paint(part, t), w, t)
	}
	return lines
}

// Text padded with spaces to n cells
func padRight(s string, n int) string {
	if w := ansi.StringWidth(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}
