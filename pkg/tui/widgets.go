package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Widget the terminal UI shows from its slash command, built in or provided by a plugin
type Widget struct {
	ID    string
	Help  string
	Usage string
	// Modal takes the keys in a window over the session, ambient sits in a zone beside the transcript
	Mode string
	// One of top-left, top-right, bottom-left, bottom-right, dock-top, dock-bottom
	Zone  string
	Width int
	// Interval at which Tick advances the state of an animated widget, zero for a still one
	Every  time.Duration
	Open   func(ctx context.Context, arg string) (state any, ok bool, err error)
	Input  func(ctx context.Context, state any, in WidgetInput) (next any, open bool, err error)
	Tick   func(ctx context.Context, state any) (next any, err error)
	Render func(ctx context.Context, frame WidgetFrame) (WidgetView, error)
}

// One key press or text input delivered to a modal widget
type WidgetInput struct {
	Text  string
	Key   string
	Ctrl  bool
	Alt   bool
	Shift bool
}

// Size and state a widget renders for
type WidgetFrame struct {
	Cols  int
	Rows  int
	State any
}

// Rendered lines of a widget
type WidgetView struct{ Lines []WidgetLine }

// One rendered line
type WidgetLine struct{ Spans []WidgetSpan }

// Run of text in one theme role and style
type WidgetSpan struct {
	Text      string
	Role      string
	Bold      bool
	Dim       bool
	Italic    bool
	Underline bool
	Inverse   bool
}

// Widgets the plugins of the profile registered, read each time the list is asked for, assigned where the plugin manager is wired
var PluginWidgets func(ctx context.Context) []Widget

// Plugin widgets were read for the first time, the composer learns their commands
type widgetsLoadedMsg struct{}

var (
	builtinMu      sync.Mutex
	builtinWidgets = map[string]Widget{}
)

// Adds a widget compiled into the binary, refusing one without an id or a render function
func RegisterWidget(w Widget) error {
	if w.ID == "" || w.Render == nil {
		return fmt.Errorf("widget %q needs an id and a render function", w.ID)
	}
	builtinMu.Lock()
	defer builtinMu.Unlock()
	if _, taken := builtinWidgets[w.ID]; taken {
		return fmt.Errorf("widget %s is already registered", w.ID)
	}
	builtinWidgets[w.ID] = w
	return nil
}

// Widget on screen with the state it holds
type activeWidget struct {
	widget Widget
	state  any
	gen    int
}

// Tick of one animated widget
type widgetTickMsg struct {
	id  string
	gen int
}

// Widgets of the app, the modal one over the session and the ambient ones in their zones
type widgetHost struct {
	app     *app
	modal   *widgetCard
	ambient []*activeWidget
	gen     int
	// Set once the plugins were loaded beside the UI, so listing them never waits on a plugin start
	loaded bool
}

// Loads the plugins of the profile beside the UI and reports when their widgets can be listed
func (h *widgetHost) warm() tea.Cmd {
	if PluginWidgets == nil {
		return nil
	}
	return func() tea.Msg {
		PluginWidgets(h.app.ctx)
		return widgetsLoadedMsg{}
	}
}

func newWidgetHost(a *app) *widgetHost { return &widgetHost{app: a} }

// Every widget by id, the built in ones and those of plugins, a built in id winning
func (h *widgetHost) list() []Widget {
	builtinMu.Lock()
	byID := make(map[string]Widget, len(builtinWidgets))
	for id, w := range builtinWidgets {
		byID[id] = w
	}
	builtinMu.Unlock()
	if PluginWidgets != nil && h.loaded {
		for _, w := range PluginWidgets(h.app.ctx) {
			if _, taken := byID[w.ID]; !taken && w.ID != "" && w.Render != nil {
				byID[w.ID] = w
			}
		}
	}
	out := make([]Widget, 0, len(byID))
	for _, w := range byID {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (h *widgetHost) find(id string) (Widget, bool) {
	for _, w := range h.list() {
		if w.ID == strings.ToLower(id) {
			return w, true
		}
	}
	return Widget{}, false
}

func (h *widgetHost) has(id string) bool {
	_, ok := h.find(id)
	return ok
}

// Opens a widget from its command, an ambient one already showing closed by its bare command
func (h *widgetHost) launch(id, arg string) tea.Cmd {
	w, ok := h.find(id)
	if !ok {
		h.app.sys("unknown widget: " + id)
		return nil
	}
	ambient := w.Mode == "ambient"
	if ambient {
		for i, active := range h.ambient {
			if active.widget.ID == w.ID {
				h.ambient = append(h.ambient[:i], h.ambient[i+1:]...)
				if trimJS(arg) == "" {
					return nil
				}
				break
			}
		}
	}
	var state any
	if w.Open != nil {
		opened, accepted, err := w.Open(h.app.ctx, arg)
		if err != nil {
			h.app.sys("⚠ /" + w.ID + ": " + err.Error())
			return nil
		}
		if !accepted {
			usage := w.Usage
			if usage == "" {
				usage = "usage: /" + w.ID
			}
			h.app.sys(usage)
			return nil
		}
		state = opened
	}
	h.gen++
	active := &activeWidget{widget: w, state: state, gen: h.gen}
	if ambient {
		h.ambient = append(h.ambient, active)
		return h.schedule(active)
	}
	h.modal = &widgetCard{host: h, active: active}
	return tea.Batch(h.app.wm.ShowOverlay(h.modal), h.schedule(active))
}

// Next tick of an animated widget
func (h *widgetHost) schedule(active *activeWidget) tea.Cmd {
	if active.widget.Every <= 0 || active.widget.Tick == nil {
		return nil
	}
	id, gen := active.widget.ID, active.gen
	return tea.Tick(active.widget.Every, func(time.Time) tea.Msg { return widgetTickMsg{id: id, gen: gen} })
}

// Advances the widget a tick names, dropped once the widget closed
func (h *widgetHost) onTick(m widgetTickMsg) tea.Cmd {
	actives := append([]*activeWidget{}, h.ambient...)
	if h.modal != nil && h.app.wm.Overlay() == wm.Card(h.modal) {
		actives = append(actives, h.modal.active)
	}
	for _, active := range actives {
		if active.widget.ID != m.id || active.gen != m.gen {
			continue
		}
		next, err := active.widget.Tick(h.app.ctx, active.state)
		if err != nil {
			h.app.sys("⚠ /" + active.widget.ID + ": " + err.Error())
			return nil
		}
		active.state = next
		return h.schedule(active)
	}
	return nil
}

// Styled rows of a widget view at a width, each span painted in the tone of its role
func (h *widgetHost) paint(active *activeWidget, cols, rows int) []string {
	t := h.app.store.ui.Theme
	view, err := active.widget.Render(h.app.ctx, WidgetFrame{Cols: cols, Rows: rows, State: active.state})
	if err != nil {
		return []string{toneStyle(t.Color.Error).Render(truncateEnd("⚠ /"+active.widget.ID+": "+err.Error(), cols))}
	}
	out := make([]string, 0, len(view.Lines))
	for _, line := range view.Lines {
		var b strings.Builder
		for _, span := range line.Spans {
			st := lipgloss.NewStyle().Bold(span.Bold).Faint(span.Dim).Italic(span.Italic).Underline(span.Underline).Reverse(span.Inverse)
			if tone := roleTone(t, span.Role); tone != "" {
				st = st.Foreground(toneColor(tone))
			}
			b.WriteString(st.Render(span.Text))
		}
		out = append(out, ansi.Truncate(b.String(), cols, "…"))
	}
	return out
}

// Tone of a widget span role in the session theme
func roleTone(t theme, role string) string {
	if role == "" {
		return t.Color.Text
	}
	if slot := t.Color.slot(role); slot != nil {
		return *slot
	}
	switch role {
	case "dim", "muted":
		return t.Color.Muted
	case "good":
		return t.Color.Ok
	case "bad":
		return t.Color.Error
	}
	return t.Color.Text
}

// Framed box of an ambient widget
func (h *widgetHost) box(active *activeWidget, cols, rows int) []string {
	t := h.app.store.ui.Theme
	width := active.widget.Width
	if width <= 0 {
		width = 44
	}
	width = min(width, max(cols-2, 12))
	inner := h.paint(active, width-4, max(rows-2, 1))
	border := toneStyle(t.Color.Border)
	title := " /" + active.widget.ID + " "
	out := []string{border.Render("╭─" + title + strings.Repeat("─", max(width-4-ansi.StringWidth(title), 0)) + "─╮")}
	for _, line := range inner[:min(len(inner), max(rows-2, 1))] {
		pad := max(width-4-ansi.StringWidth(line), 0)
		out = append(out, border.Render("│ ")+line+strings.Repeat(" ", pad)+border.Render(" │"))
	}
	return append(out, border.Render("╰"+strings.Repeat("─", width-2)+"╯"))
}

// Ambient widgets docked in a row zone, side by side at the right
func (h *widgetHost) dock(zone string, cols int) []string {
	var boxes [][]string
	for _, active := range h.ambient {
		if widgetZone(active.widget) == zone {
			boxes = append(boxes, h.box(active, cols, 12))
		}
	}
	if len(boxes) == 0 {
		return nil
	}
	height := 0
	for _, b := range boxes {
		height = max(height, len(b))
	}
	rows := make([]string, height)
	for _, b := range boxes {
		width := ansi.StringWidth(b[0])
		for i := range rows {
			cell := strings.Repeat(" ", width)
			if i < len(b) {
				cell = b[i]
			}
			rows[i] += cell + " "
		}
	}
	for i := range rows {
		rows[i] = strings.Repeat(" ", max(cols-ansi.StringWidth(rows[i])-1, 0)) + rows[i]
	}
	return rows
}

// Zone an ambient widget sits in, the top right when it names none
func widgetZone(w Widget) string {
	if w.Zone == "" {
		return "top-right"
	}
	return w.Zone
}

// Lays the corner widgets over the transcript rows, each corner a stack of boxes
func (h *widgetHost) overlayCorners(body []string, cols int) []string {
	if len(h.ambient) == 0 {
		return body
	}
	stacks := map[string][]string{}
	for _, active := range h.ambient {
		zone := widgetZone(active.widget)
		if strings.HasPrefix(zone, "dock-") {
			continue
		}
		stacks[zone] = append(stacks[zone], h.box(active, cols, max(len(body)/2, 4))...)
	}
	for zone, stack := range stacks {
		stack = stack[:min(len(stack), len(body))]
		first := 0
		if strings.HasPrefix(zone, "bottom-") {
			first = len(body) - len(stack)
		}
		for i, row := range stack {
			width := ansi.StringWidth(row)
			line := body[first+i]
			if strings.HasSuffix(zone, "-left") {
				rest := ""
				if ansi.StringWidth(line) > width {
					rest = ansi.TruncateLeft(line, width, "")
				}
				body[first+i] = row + rest
				continue
			}
			keep := max(cols-width-1, 0)
			left := ansi.Truncate(line, keep, "")
			body[first+i] = left + strings.Repeat(" ", max(keep-ansi.StringWidth(left), 0)) + row
		}
	}
	return body
}

// Window of the modal widget, every key going to its input function
type widgetCard struct {
	host   *widgetHost
	active *activeWidget
}

func (c *widgetCard) Title() string  { return "/" + c.active.widget.ID }
func (c *widgetCard) Key() string    { return "widget:" + c.active.widget.ID }
func (c *widgetCard) Init() tea.Cmd  { return nil }
func (c *widgetCard) Raw() bool      { return true }
func (c *widgetCard) Status() string { return c.active.widget.Help }

func (c *widgetCard) MinSize() (int, int) {
	w, h := c.host.app.wm.Size()
	width := c.active.widget.Width
	if width <= 0 {
		width = w * 7 / 10
	}
	return min(width, max(w-8, 20)), max(h*6/10, 8)
}

func (c *widgetCard) View(w, h int) string {
	rows := c.host.paint(c.active, w, h)
	return strings.Join(rows[:min(len(rows), h)], "\n")
}

func (c *widgetCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	key, isKey := m.(tea.KeyPressMsg)
	if !isKey {
		return c, nil
	}
	in := WidgetInput{Text: key.Text, Key: key.String(), Ctrl: key.Mod&tea.ModCtrl != 0, Alt: key.Mod&tea.ModAlt != 0, Shift: key.Mod&tea.ModShift != 0}
	if c.active.widget.Input == nil {
		if in.Key == "esc" || in.Text == "q" || in.Key == "ctrl+c" {
			return c, wm.RequestDismissOf(c)
		}
		return c, nil
	}
	next, open, err := c.active.widget.Input(c.host.app.ctx, c.active.state, in)
	if err != nil {
		c.host.app.sys("⚠ /" + c.active.widget.ID + ": " + err.Error())
		return c, wm.RequestDismissOf(c)
	}
	if !open {
		return c, wm.RequestDismissOf(c)
	}
	c.active.state = next
	return c, nil
}
