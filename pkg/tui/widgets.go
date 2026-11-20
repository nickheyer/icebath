package tui

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/txt"
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
	Every time.Duration
	// Rows the window of a modal widget asks for at a width, six tenths of the screen when the widget has no say
	Rows func(state any, cols int) int
	// Step a modal widget takes back at the close key before its window closes, false once it has none left
	Back   func(ctx context.Context, state any) (next any, stepped bool, err error)
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

// Plugin that failed to load at a scan, with the reason
type WidgetLoadFailure struct {
	Plugin  string
	Message string
}

// Widgets the plugins of the profile register and the plugins that failed to load, after a scan of the plugins that is forced anew when asked or
// else runs only the first time, assigned where the plugin manager is wired
var ScanPluginWidgets func(ctx context.Context, force bool) ([]Widget, []WidgetLoadFailure)

// Plugin widgets were scanned, the composer learns their commands and the transcript what came, went or failed
type widgetsLoadedMsg struct {
	widgets  []Widget
	failures []WidgetLoadFailure
	// Told the ids of the plugin widgets and the failures once the transcript has them, nil for a scan nobody asked for
	report func(loaded []string, failures []WidgetLoadFailure)
}

// Key the widget host travels under in the context of a widget's input call
type widgetHostKey struct{}

// Widget a running widget asked to open in its place, with the state it handed over
type widgetOpening struct {
	id    string
	state any
}

// Opens the widget of an id in place of the modal one whose input function is running, with a state that needs no argument to parse,
// false outside such a call and for an id no widget has
func OpenWidget(ctx context.Context, id string, state any) bool {
	h, _ := ctx.Value(widgetHostKey{}).(*widgetHost)
	if h == nil || !h.has(id) {
		return false
	}
	h.opening = &widgetOpening{id: id, state: state}
	return true
}

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

// Widget on screen with the state it holds, when it last refreshed and whether a tick of it runs
type activeWidget struct {
	widget  Widget
	state   any
	gen     int
	last    time.Time
	ticking bool
}

// Refresh of a widget that came due on a frame, its tick run on the update loop like every other widget call
type widgetTickedMsg struct {
	active *activeWidget
	gen    int
}

// Widgets of the app, the modal one over the session and the ambient ones in their zones
type widgetHost struct {
	app     *app
	modal   *widgetCard
	ambient []*activeWidget
	gen     int
	// Set once the plugins were loaded beside the UI, so listing them never waits on a plugin start
	loaded bool
	// Ids of the plugin widgets the last scan found, what the next scan is compared with
	known map[string]bool
	// Widget the input function of the modal one asked to open, taken once that function returned
	opening *widgetOpening
}

// Loads the plugins of the profile beside the UI and reports when their widgets can be listed
func (h *widgetHost) warm() tea.Cmd { return h.scan(false, nil) }

// Scans the plugins again beside the UI, the transcript told what came, went or failed and the report what the scan found
func (h *widgetHost) rescan(report func(loaded []string, failures []WidgetLoadFailure)) tea.Cmd {
	if report == nil {
		report = func([]string, []WidgetLoadFailure) {}
	}
	return h.scan(true, report)
}

// Reads the plugin widgets beside the UI, through the scan hook when one is wired and else as the plugins stand
func (h *widgetHost) scan(force bool, report func(loaded []string, failures []WidgetLoadFailure)) tea.Cmd {
	if ScanPluginWidgets == nil && PluginWidgets == nil {
		if report != nil {
			report(nil, nil)
		}
		return nil
	}
	return func() tea.Msg {
		m := widgetsLoadedMsg{report: report}
		if ScanPluginWidgets != nil {
			m.widgets, m.failures = ScanPluginWidgets(h.app.ctx, force)
		} else {
			m.widgets = PluginWidgets(h.app.ctx)
		}
		return m
	}
}

// Takes a finished scan: widgets that came or went and plugins that failed are told in the transcript, since a widget registered in silence
// cannot be told from one that failed, and the widgets on screen follow their plugins
func (h *widgetHost) onScanned(m widgetsLoadedMsg) tea.Cmd {
	h.loaded = true
	builtinMu.Lock()
	found, defs := map[string]bool{}, map[string]Widget{}
	for _, w := range m.widgets {
		if _, builtin := builtinWidgets[w.ID]; !builtin && w.ID != "" && w.Render != nil {
			found[w.ID], defs[w.ID] = true, w
		}
	}
	builtinMu.Unlock()
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var lines []string
	for _, id := range ids {
		if !h.known[id] {
			lines = append(lines, tr(txt.SessionMainWidgetLive, id))
		}
	}
	var gone []string
	for id := range h.known {
		if !found[id] {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, id := range gone {
		lines = append(lines, tr(txt.SessionMainWidgetRemoved, id))
	}
	for _, failure := range m.failures {
		lines = append(lines, tr(txt.SessionMainWidgetFailedToLoad, failure.Plugin, failure.Message))
	}
	closing := h.follow(defs)
	h.known = found
	// The scan of the start finishes before the first session stands, whose start would take the lines with it
	return tea.Batch(closing, h.app.onceSessionStands(sessionWaitTries, func() {
		for _, line := range lines {
			h.app.sys(line)
		}
		if m.report != nil {
			m.report(ids, m.failures)
		}
	}))
}

// Points the plugin widgets on screen at the functions their plugins registered at the last scan and closes those whose plugin no longer provides them
func (h *widgetHost) follow(defs map[string]Widget) tea.Cmd {
	kept := h.ambient[:0]
	for _, active := range h.ambient {
		def, still := defs[active.widget.ID]
		switch {
		case still:
			active.widget = def
		case h.known[active.widget.ID]:
			continue
		}
		kept = append(kept, active)
	}
	h.ambient = kept
	if h.modal == nil {
		return nil
	}
	active := h.modal.active
	if def, still := defs[active.widget.ID]; still {
		active.widget = def
	} else if h.known[active.widget.ID] {
		return wm.RequestDismissOf(h.modal)
	}
	return nil
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
	return h.show(w, state)
}

// Puts a widget on screen with its first state, an ambient one in its zone and a modal one in the window over the session, in place of any modal one up
func (h *widgetHost) show(w Widget, state any) tea.Cmd {
	h.gen++
	active := &activeWidget{widget: w, state: state, gen: h.gen, last: time.Now()}
	if w.Mode == "ambient" {
		h.ambient = append(h.ambient, active)
		return nil
	}
	h.modal = &widgetCard{host: h, active: active}
	return h.app.wm.ShowOverlay(h.modal)
}

// Opens the widget the input function of the modal one asked for, an ambient one of the same id replaced
func (h *widgetHost) open() tea.Cmd {
	opening := h.opening
	h.opening = nil
	if opening == nil {
		return nil
	}
	w, ok := h.find(opening.id)
	if !ok {
		return nil
	}
	h.ambient = slices.DeleteFunc(h.ambient, func(active *activeWidget) bool { return active.widget.ID == w.ID })
	return h.show(w, opening.state)
}

// Refreshes each widget whose interval passed on this frame, its tick queued for the loop, reporting whether one draws anew
func (h *widgetHost) refresh(now time.Time, actives []*activeWidget) bool {
	redraw := false
	for _, active := range actives {
		w := active.widget
		if w.Every <= 0 || now.Sub(active.last) < w.Every {
			continue
		}
		active.last = now
		redraw = true
		if w.Tick == nil || active.ticking || h.app.post == nil {
			continue
		}
		active.ticking = true
		h.app.post(widgetTickedMsg{active: active, gen: active.gen})
	}
	return redraw
}

// Ambient widgets that refresh on an interval keep the frame clock running
func (h *widgetHost) refreshing(actives []*activeWidget) bool {
	for _, active := range actives {
		if active.widget.Every > 0 {
			return true
		}
	}
	return false
}

// Runs the tick of a widget still shown and draws it anew, a failing widget refreshing no more
func (h *widgetHost) onTicked(m widgetTickedMsg) {
	m.active.ticking = false
	if m.active.gen != m.gen || m.active.widget.Tick == nil {
		return
	}
	next, err := m.active.widget.Tick(h.app.ctx, m.active.state)
	if err != nil {
		h.app.sys("⚠ /" + m.active.widget.ID + ": " + err.Error())
		m.active.widget.Every = 0
		return
	}
	m.active.state = next
	h.app.wm.Repaint()
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
	return framedBox(t, h.app.uiTheme().Border, " /"+active.widget.ID+" ", inner[:min(len(inner), max(rows-2, 1))], width)
}

// Frame of a total width around rows in the border tone, drawn with the border of the toolkit theme, the title cut into the top edge
func framedBox(t theme, b lipgloss.Border, title string, inner []string, width int) []string {
	border := toneStyle(t.Color.Border)
	title = ansi.Truncate(title, max(width-4, 0), "")
	out := []string{border.Render(b.TopLeft + b.Top + title + strings.Repeat(b.Top, max(width-4-ansi.StringWidth(title), 0)) + b.Top + b.TopRight)}
	for _, line := range inner {
		pad := max(width-4-ansi.StringWidth(line), 0)
		out = append(out, border.Render(b.Left+" ")+line+strings.Repeat(" ", pad)+border.Render(" "+b.Right))
	}
	return append(out, border.Render(b.BottomLeft+strings.Repeat(b.Bottom, width-2)+b.BottomRight))
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

// Refreshes the modal widget on the frame clock when its interval passed
func (c *widgetCard) Animate(float64) bool {
	return c.host.refresh(time.Now(), []*activeWidget{c.active})
}

// Keeps the frame clock running while the modal widget refreshes on an interval
func (c *widgetCard) Awake() bool { return c.active.widget.Every > 0 }

func (c *widgetCard) MinSize() (int, int) {
	w, h := c.host.app.wm.Size()
	width := c.active.widget.Width
	if width <= 0 {
		width = w * 7 / 10
	}
	width = min(width, max(w-8, 20))
	if c.active.widget.Rows != nil {
		return width, max(c.active.widget.Rows(c.active.state, width), 1)
	}
	return width, max(h*6/10, 8)
}

func (c *widgetCard) View(w, h int) string {
	rows := c.host.paint(c.active, w, h)
	return strings.Join(rows[:min(len(rows), h)], "\n")
}

// Keys the window manager hands an overlay as actions and not as key presses, by the action each arrives as, so a widget sees the arrows,
// the enter key and the editing keys it would otherwise never hear of
var widgetActionKeys = map[string]string{
	"palette.up": "up", "palette.down": "down", "palette.left": "left", "palette.right": "right", "palette.pick": "enter",
	"palette.page.up": "pgup", "palette.page.down": "pgdown", "palette.home": "home", "palette.end": "end",
	"palette.delete.back": "backspace", "palette.delete.forward": "delete", "palette.clear": "ctrl+u", "palette.delete.word": "ctrl+w",
}

// Takes the close key for a widget that has a step of its own behind it, a zoom to leave or a mode to end, the window closing when it has none
func (c *widgetCard) Back() bool {
	if c.active.widget.Back == nil {
		return false
	}
	next, stepped, err := c.active.widget.Back(c.host.app.ctx, c.active.state)
	if err != nil {
		c.host.app.sys("⚠ /" + c.active.widget.ID + ": " + err.Error())
		return false
	}
	if stepped {
		c.active.state = next
	}
	return stepped
}

func (c *widgetCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	var in WidgetInput
	switch m := m.(type) {
	case tea.KeyPressMsg:
		in = WidgetInput{Text: m.Text, Key: m.String(), Ctrl: m.Mod&tea.ModCtrl != 0, Alt: m.Mod&tea.ModAlt != 0, Shift: m.Mod&tea.ModShift != 0}
	case wm.ActionMsg:
		key, mapped := widgetActionKeys[m.ID]
		if !mapped {
			return c, nil
		}
		in = WidgetInput{Key: key}
	default:
		return c, nil
	}
	if c.active.widget.Input == nil {
		if in.Key == "esc" || in.Text == "q" || in.Key == "ctrl+c" {
			return c, wm.RequestDismissOf(c)
		}
		return c, nil
	}
	next, open, err := c.active.widget.Input(context.WithValue(c.host.app.ctx, widgetHostKey{}, c.host), c.active.state, in)
	if err != nil {
		c.host.opening = nil
		c.host.app.sys("⚠ /" + c.active.widget.ID + ": " + err.Error())
		return c, wm.RequestDismissOf(c)
	}
	// A widget opened from the input takes the window, so the one that asked neither stays nor dismisses what replaced it
	if c.host.opening != nil {
		return c, c.host.open()
	}
	if !open {
		return c, wm.RequestDismissOf(c)
	}
	c.active.state = next
	return c, nil
}
