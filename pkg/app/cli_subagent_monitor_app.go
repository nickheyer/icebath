package app

import (
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Message repainting the monitor
type monitorRepaintMsg struct{}

// Full height subagent and process monitor, header, roster or tail, steer input, notice and footer
type monitorApp struct {
	monitor *subagentMonitor
	shell   *ptApplication
	roster  *ptBuffer
	tail    *ptBuffer
	steer   *ptBuffer
	style   *ptMergedStyle
	in      io.Reader
	out     io.Writer
	program *tea.Program
	done    atomic.Bool

	mu        sync.Mutex
	focus     string
	steering  bool
	confirm   bool
	notice    string
	target    any
	cols      int
	rows      int
	rosterTop int
	tailTop   int
	tailTop2  int
	steerLeft int
}

// Builds the monitor application over the terminal streams with the skin style overrides
func buildMonitorApplication(m *subagentMonitor, in io.Reader, out io.Writer) *monitorApp {
	c := m.cli
	d := &styleDict{values: map[string]string{}}
	d.update(config.GetPromptToolkitStyleOverrides(c.ctx))
	user, err := ptStyleFromDict(d)
	if err != nil {
		panic(err)
	}
	a := &monitorApp{monitor: m, style: newPTMergedStyle(user), in: in, out: out, focus: "roster", cols: 80, rows: 24}
	if f, ok := out.(*os.File); ok {
		if w, h, err := xterm.GetSize(f.Fd()); err == nil && w > 0 && h > 0 {
			a.cols, a.rows = w, h
		}
	}
	readOnly := func() bool { return true }
	a.roster, a.tail, a.steer = newPTBuffer(nil), newPTBuffer(nil), newPTBuffer(nil)
	a.roster.readOnly, a.tail.readOnly = readOnly, readOnly
	a.shell = newPTApplication(in, out)
	a.shell.setBuffer(a.roster)
	a.shell.keyBindings = a.bindings()
	return a
}

// Moves focus to the roster, the tail or the steer input
func (a *monitorApp) focusOn(target string) {
	a.focus = target
	switch target {
	case "tail":
		a.shell.setBuffer(a.tail)
	case "steer":
		a.shell.setBuffer(a.steer)
	default:
		a.shell.setBuffer(a.roster)
	}
}

// Key bindings of the monitor, roster navigation, detail, steer, stop and close
func (a *monitorApp) bindings() *ptKeyBindings {
	m := a.monitor
	kb := &ptKeyBindings{}
	normal := func() bool { return !a.steering && !a.confirm }
	listing := func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return normal() && !m.detail
	}
	steering := func() bool { return a.steering }
	confirming := func() bool { return a.confirm }
	kb.add(func(*ptEvent) { m.selectBy(-1) }, ptBind{filter: listing}, "up")
	kb.add(func(*ptEvent) { m.selectBy(1) }, ptBind{filter: listing}, "down")
	kb.add(func(*ptEvent) {
		if row, _ := m.selected(); row != nil {
			m.mu.Lock()
			m.detail = true
			m.mu.Unlock()
			m.pollSoon()
			a.updateTail()
			a.focusOn("tail")
		}
	}, ptBind{filter: listing}, "enter")
	kb.add(func(*ptEvent) {
		if row, isProcess := m.selected(); row != nil && !isProcess {
			a.steering = true
			m.mu.Lock()
			a.target = m.selectedID
			m.mu.Unlock()
			a.focusOn("steer")
		}
	}, ptBind{filter: normal}, "s")
	kb.add(func(*ptEvent) {
		text := a.steer.text()
		if strings.TrimSpace(text) == "" {
			return
		}
		a.runControl("steer", text, a.target)
		_ = a.steer.setDocument("", 0, true)
		a.steering = false
		a.focusBack()
	}, ptBind{filter: steering}, "enter")
	kb.add(func(*ptEvent) {
		if row, _ := m.selected(); row != nil {
			a.confirm = true
			m.mu.Lock()
			a.target = m.selectedID
			m.mu.Unlock()
		}
	}, ptBind{filter: normal}, "x")
	kb.add(func(*ptEvent) {
		a.runControl("stop", nil, a.target)
		a.confirm = false
	}, ptBind{filter: confirming}, "y")
	kb.add(func(*ptEvent) {
		m.mu.Lock()
		detail := m.detail
		m.mu.Unlock()
		switch {
		case a.steering || a.confirm:
			a.steering, a.confirm = false, false
			a.focusBack()
		case detail:
			m.mu.Lock()
			m.detail = false
			m.mu.Unlock()
			a.focusOn("roster")
		default:
			a.exit()
		}
	}, ptBind{eager: true}, "escape")
	for _, key := range []string{"q", "f6", "c-t"} {
		kb.add(func(*ptEvent) { a.exit() }, ptBind{filter: normal}, key)
	}
	kb.add(func(*ptEvent) { a.exit() }, ptBind{}, "c-c")
	return kb
}

// Focus after a steer or confirm ends, the tail in detail and the roster otherwise
func (a *monitorApp) focusBack() {
	a.monitor.mu.Lock()
	detail := a.monitor.detail
	a.monitor.mu.Unlock()
	if detail {
		a.focusOn("tail")
	} else {
		a.focusOn("roster")
	}
}

// Runs a steer or stop off the UI loop, its answer landing in the notice line
func (a *monitorApp) runControl(action string, message any, target any) {
	a.mu.Lock()
	a.notice = a.monitor.cli.t("cli.subagents.control_pending", nil)
	a.mu.Unlock()
	go func() {
		result, err := func() (result map[string]any, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = errorFromPanic(r)
				}
			}()
			return a.monitor.control(action, message, target)
		}()
		if err != nil {
			result = map[string]any{"error": err.Error()}
		}
		var notice string
		switch {
		case pyrepr.Truthy(result["error"]):
			notice = pyrepr.Str(result["error"])
		case pyrepr.Truthy(result["note"]):
			notice = pyrepr.Str(result["note"])
		default:
			notice = pyrepr.Str(result)
		}
		a.mu.Lock()
		a.notice = notice
		a.mu.Unlock()
		a.invalidate()
	}()
}

// Error of a recovered panic value
func errorFromPanic(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return &panicError{text: errorText(r)}
}

// Panic value carried as an error
type panicError struct{ text string }

func (e *panicError) Error() string { return e.text }

// Replaces the tail text when the selection's transcript or output changed, following the end
func (a *monitorApp) updateTail() {
	m := a.monitor
	row, isProcess := m.selected()
	var text string
	switch {
	case row == nil:
		text = m.cli.t("cli.subagents.entry_gone", nil)
	case isProcess:
		text = processTail(m.cli.ctx, m.cli.processRegistry(), pyrepr.Str(row["id"]))
	default:
		text = subagentTail(m, row)
	}
	current := a.tail.text()
	if text == current {
		return
	}
	position := a.tail.cursor
	following := position == len([]rune(current))
	cursor := min(position, len([]rune(text)))
	if following {
		cursor = len([]rune(text))
	}
	_ = a.tail.setDocument(text, cursor, true)
}

// Repaints the monitor from any goroutine
func (a *monitorApp) invalidate() {
	if a.done.Load() || a.program == nil {
		return
	}
	go a.program.Send(monitorRepaintMsg{})
}

// Ends the monitor application
func (a *monitorApp) exit() {
	if a.done.Swap(true) {
		return
	}
	if a.program != nil {
		go a.program.Quit()
	}
}

// Runs the monitor full screen until it exits
func (a *monitorApp) run() {
	p := tea.NewProgram(&monitorModel{app: a}, tea.WithInput(guardedInput(a.in)), tea.WithOutput(a.out), tea.WithoutSignalHandler(),
		tea.WithWindowSize(a.cols, a.rows))
	a.program = p
	if a.done.Load() {
		return
	}
	_, _ = p.Run()
	a.done.Store(true)
}

// monitorModel adapts the monitor to the bubbletea model contract
type monitorModel struct{ app *monitorApp }

func (m *monitorModel) Init() tea.Cmd { return nil }

// Feeds keys through the monitor bindings and tracks the terminal size
func (m *monitorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	a := m.app
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.mu.Lock()
		a.cols, a.rows = msg.Width, msg.Height
		a.mu.Unlock()
	case tea.KeyPressMsg:
		a.shell.feedKeys(ptKeyPressesForEvent(msg.Key())...)
	case tea.PasteMsg:
		a.shell.feedKeys(ptKeyPress{Key: ptKeyBracketedPaste, Data: msg.Content})
	}
	if a.done.Load() {
		return m, tea.Quit
	}
	return m, nil
}

// Paints the monitor, exiting first when a prompt arrived and refreshing the open tail
func (m *monitorModel) View() tea.View {
	a := m.app
	var v tea.View
	v.AltScreen = true
	if modalPromptActive(a.monitor.cli) && !a.done.Load() {
		a.exit()
	} else {
		a.monitor.mu.Lock()
		detail := a.monitor.detail
		a.monitor.mu.Unlock()
		if detail {
			a.updateTail()
		}
	}
	lines, cx, cy := a.paint()
	v.SetContent(strings.Join(lines, "\n"))
	if cy >= 0 {
		v.Cursor = tea.NewCursor(cx, cy)
	}
	return v
}

// Text of fragments rendered in their styles over the container style, padded to the width
func (a *monitorApp) styledLine(frags []styledFragment, width int) string {
	var b strings.Builder
	used := 0
	for _, f := range frags {
		style := "class:subagent-dock"
		if f.Style != "" {
			style += " " + f.Style
		}
		b.WriteString(a.style.render(style).Render(f.Text))
		used += cellWidth(f.Text)
	}
	if used < width {
		b.WriteString(a.style.render("class:subagent-dock").Render(strings.Repeat(" ", width-used)))
	}
	return b.String()
}

// Rendered rows of the whole screen with the cursor cell of the focused control
func (a *monitorApp) paint() ([]string, int, int) {
	m := a.monitor
	a.mu.Lock()
	cols, rows, notice, steering, confirm := a.cols, a.rows, a.notice, a.steering, a.confirm
	a.mu.Unlock()
	m.mu.Lock()
	detail := m.detail
	row := m.selectedLocked()
	title := m.titleLocked() + " · " + m.countsLocked(false)
	if detail && row != nil {
		subject := pyrepr.Or(row["goal"], pyrepr.Or(row["command"], ""))
		title += " · " + pyrepr.Str(dockRowKey(row)) + " · " + pyrepr.Str(subject)
	}
	isProcess := row != nil && row["kind"] == "process"
	m.mu.Unlock()
	lines := []string{a.styledLine([]styledFragment{{Style: "class:subagent-dock.heading", Text: dockClip(title, cols)}}, cols)}
	bodyHeight := max(0, rows-3)
	if steering {
		bodyHeight = max(0, bodyHeight-1)
	}
	cx, cy := -1, -1
	if detail {
		body, x, y := a.paintTail(cols, bodyHeight)
		lines = append(lines, body...)
		if a.focus == "tail" && y >= 0 {
			cx, cy = x, 1+y
		}
	} else {
		body, y := a.paintRoster(cols, bodyHeight)
		lines = append(lines, body...)
		if a.focus == "roster" && y >= 0 {
			cx, cy = 0, 1+y
		}
	}
	if steering {
		line, x := a.paintSteer(cols)
		if a.focus == "steer" {
			cx, cy = x, len(lines)
		}
		lines = append(lines, line)
	}
	lines = append(lines, a.styledLine([]styledFragment{{Text: dockClip(notice, cols)}}, cols))
	lines = append(lines, a.styledLine([]styledFragment{{Text: a.footer(cols, confirm, steering, detail, isProcess)}}, cols))
	return lines, cx, cy
}

// Footer hint for the current mode, compact below sixty columns
func (a *monitorApp) footer(cols int, confirm, steering, detail, process bool) string {
	t := a.monitor.cli.t
	narrow := cols < 60
	if confirm {
		if narrow {
			return t("cli.subagents.footer_confirm_stop_compact", nil)
		}
		noun := t("cli.subagents.noun_subagent", nil)
		if process {
			noun = t("cli.subagents.noun_process", nil)
		}
		return t("cli.subagents.footer_confirm_stop", map[string]any{"noun": noun})
	}
	if steering {
		if narrow {
			return t("cli.subagents.footer_steering_compact", nil)
		}
		return t("cli.subagents.footer_steering", nil)
	}
	steer := ""
	if !process {
		steer = t("cli.subagents.footer_steer_key", nil)
	}
	if narrow {
		if detail {
			return t("cli.subagents.footer_detail_compact", map[string]any{"steer": steer})
		}
		return t("cli.subagents.footer_roster_compact", nil)
	}
	steer = ""
	if !process {
		steer = t("cli.subagents.footer_steer_key_long", nil)
	}
	if detail {
		return t("cli.subagents.footer_detail", map[string]any{"steer": steer})
	}
	return t("cli.subagents.footer_roster", map[string]any{"steer": steer})
}

// Roster rows scrolled to keep the selection visible, with the selection's screen row
func (a *monitorApp) paintRoster(cols, height int) ([]string, int) {
	m := a.monitor
	t := m.cli.t
	m.mu.Lock()
	roster := m.rosterLocked()
	selectedID := m.selectedID
	m.mu.Unlock()
	var content [][]styledFragment
	cursor := 0
	for i, row := range roster {
		selected := dockRowKey(row) == selectedID
		if selected {
			cursor = i
		}
		var prefix, activity, subject string
		if row["kind"] == "process" {
			prefix = processGlyph(row) + " " + processActivity(m.cli.ctx, row) + " · "
			subject = pyrepr.Str(row["command"])
		} else {
			status := pyrepr.Str(pyrepr.Or(row["status"], t("cli.subagents.starting", nil)))
			prefix = pyrepr.Str(row["elapsed"]) + "s · " + status + " · "
			if pyrepr.Truthy(row["last_tool"]) {
				activity = " · " + t("cli.subagents.last_tool", map[string]any{"tool": row["last_tool"]})
			}
			subject = pyrepr.Str(pyrepr.Or(row["goal"], row["subagent_id"]))
		}
		goalWidth := max(0, cols-2-cellWidth(prefix+activity))
		mark := " "
		if selected {
			mark = "❯"
		}
		text := mark + " " + dockClip(prefix+dockClip(subject, goalWidth)+activity, max(0, cols-2))
		text += strings.Repeat(" ", max(0, cols-cellWidth(text)))
		style := ""
		if selected {
			style = "class:subagent-dock.selected"
		}
		content = append(content, []styledFragment{{Style: style, Text: text}})
	}
	if len(roster) == 0 {
		content = [][]styledFragment{{{Text: t("cli.subagents.roster_empty", nil)}}}
	} else {
		content = append(content, nil)
	}
	a.rosterTop = ptDoScroll(a.rosterTop, cursor, height, len(content))
	out := make([]string, 0, height)
	for i := a.rosterTop; i < len(content) && len(out) < height; i++ {
		out = append(out, a.styledLine(clipFragments(content[i], cols), cols))
	}
	for len(out) < height {
		out = append(out, a.styledLine(nil, cols))
	}
	return out, cursor - a.rosterTop
}

// Fragments cut to the width in cells
func clipFragments(frags []styledFragment, width int) []styledFragment {
	var out []styledFragment
	used := 0
	for _, f := range frags {
		var b strings.Builder
		for _, r := range f.Text {
			w := max(0, cellWidth(string(r)))
			if used+w > width {
				break
			}
			b.WriteRune(r)
			used += w
		}
		out = append(out, styledFragment{Style: f.Style, Text: b.String()})
	}
	return out
}

// Scroll offset keeping the cursor line inside the window without line wrapping
func ptDoScroll(current, cursor, windowSize, contentSize int) int {
	if current < 0 {
		current = 0
	}
	if current > contentSize-windowSize {
		current = max(0, contentSize-windowSize)
	}
	if current > cursor {
		current = max(0, cursor)
	}
	if current < cursor+1-windowSize {
		current = cursor + 1 - windowSize
	}
	return current
}

// Wrapped rows of one logical line at the width, at least one row
func monitorWrapRows(line string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	var rows []string
	var cur strings.Builder
	used := 0
	for _, r := range line {
		w := max(0, cellWidth(string(r)))
		if used+w > width && used > 0 {
			rows = append(rows, cur.String())
			cur.Reset()
			used = 0
		}
		cur.WriteRune(r)
		used += w
	}
	return append(rows, cur.String())
}

// Tail rows with line wrapping and the scrollbar margin, scrolled to the cursor, with the cursor cell
func (a *monitorApp) paintTail(cols, height int) ([]string, int, int) {
	width := max(0, cols-1)
	text := a.tail.text()
	logical := strings.Split(text, "\n")
	runes := []rune(text)
	cursor := min(a.tail.cursor, len(runes))
	before := string(runes[:cursor])
	cursorLine := strings.Count(before, "\n")
	cursorCol := []rune(before[strings.LastIndex(before, "\n")+1:])
	heights := make([]int, len(logical))
	for i, l := range logical {
		heights[i] = ptLineHeight(cellWidth(l), width)
	}
	a.scrollWrapped(heights, cursorLine, ptLineHeight(cellWidth(string(cursorCol)), width), height, width)
	var rows []string
	cx, cy := -1, -1
	for i := a.tailTop; i < len(logical) && len(rows) < height; i++ {
		wrapped := monitorWrapRows(logical[i], width)
		start := 0
		if i == a.tailTop {
			start = min(a.tailTop2, len(wrapped)-1)
		}
		if i == cursorLine {
			cw := monitorWrapRows(string(cursorCol), width)
			rowInLine := len(cw) - 1
			col := cellWidth(cw[len(cw)-1])
			if col >= width && width > 0 {
				rowInLine, col = rowInLine+1, 0
			}
			if rowInLine >= start {
				cy, cx = len(rows)+rowInLine-start, col
			}
		}
		for _, r := range wrapped[start:] {
			if len(rows) >= height {
				break
			}
			rows = append(rows, r)
		}
	}
	if cy >= height {
		cx, cy = -1, -1
	}
	visibleLines := 0
	for i, used := a.tailTop, 0; i < len(logical) && used < height; i++ {
		used += heights[i]
		visibleLines++
	}
	bar := a.scrollbar(height, len(logical), visibleLines, a.tailTop)
	out := make([]string, height)
	for i := 0; i < height; i++ {
		body := ""
		if i < len(rows) {
			body = rows[i]
		}
		frags := []styledFragment{{Text: body}, {Text: strings.Repeat(" ", max(0, width-cellWidth(body)))}}
		if i < len(bar) {
			frags = append(frags, bar[i])
		}
		out[i] = a.styledLine(frags, cols)
	}
	return out, cx, cy
}

// Vertical scroll of a wrapping window keeping the cursor visible, the prompt_toolkit wrapping scroll
func (a *monitorApp) scrollWrapped(heights []int, cursorLine, textBeforeHeight, height, width int) {
	if width <= 0 {
		a.tailTop, a.tailTop2 = cursorLine, 0
		return
	}
	lineHeight := heights[cursorLine]
	if lineHeight > height {
		a.tailTop = cursorLine
		a.tailTop2 = min(textBeforeHeight-1, lineHeight-height, a.tailTop2)
		a.tailTop2 = max(0, textBeforeHeight-height, a.tailTop2)
		return
	}
	a.tailTop2 = 0
	minScroll := 0
	used, prev := 0, cursorLine
	for l := cursorLine; l >= 0; l-- {
		used += heights[l]
		if used > height {
			minScroll = prev
			break
		}
		prev = l
	}
	maxScroll := cursorLine
	topmost, prev3 := len(heights)-1, len(heights)-1
	used = 0
	for l := len(heights) - 1; l >= 0; l-- {
		used += heights[l]
		if used > height {
			topmost = prev3
			break
		}
		prev3 = l
		topmost = prev3
	}
	a.tailTop = max(a.tailTop, min(topmost, minScroll))
	a.tailTop = min(a.tailTop, maxScroll)
}

// Scrollbar margin cells with arrows, the button spanning the visible fraction
func (a *monitorApp) scrollbar(windowHeight, contentHeight, displayedLines, verticalScroll int) []styledFragment {
	if contentHeight == 0 {
		return nil
	}
	height := windowHeight - 2
	fractionVisible := float64(displayedLines) / float64(contentHeight)
	fractionAbove := float64(verticalScroll) / float64(contentHeight)
	barHeight := int(min(float64(height), max(1, float64(height)*fractionVisible)))
	barTop := int(float64(height) * fractionAbove)
	isButton := func(row int) bool { return barTop <= row && row <= barTop+barHeight }
	out := []styledFragment{{Style: "class:scrollbar.arrow", Text: "^"}}
	for i := 0; i < height; i++ {
		switch {
		case isButton(i) && !isButton(i+1):
			out = append(out, styledFragment{Style: "class:scrollbar.button,scrollbar.end", Text: " "})
		case isButton(i):
			out = append(out, styledFragment{Style: "class:scrollbar.button", Text: " "})
		case isButton(i + 1):
			out = append(out, styledFragment{Style: "class:scrollbar.background,scrollbar.start", Text: " "})
		default:
			out = append(out, styledFragment{Style: "class:scrollbar.background", Text: " "})
		}
	}
	return append(out, styledFragment{Style: "class:scrollbar.arrow", Text: "v"})
}

// Steer input row with its prompt, scrolled to the cursor, and the cursor column
func (a *monitorApp) paintSteer(cols int) (string, int) {
	prompt := a.monitor.cli.t("cli.subagents.steer_prompt", nil)
	text := []rune(a.steer.text())
	cursor := min(a.steer.cursor, len(text))
	full := prompt + string(text)
	cursorX := cellWidth(prompt) + cellWidth(string(text[:cursor]))
	a.steerLeft = ptDoScroll(a.steerLeft, cursorX, cols, cellWidth(full)+1)
	var visible strings.Builder
	col := 0
	for _, r := range full {
		w := max(0, cellWidth(string(r)))
		if col >= a.steerLeft && col+w <= a.steerLeft+cols {
			visible.WriteRune(r)
		}
		col += w
	}
	promptCells := max(0, cellWidth(prompt)-a.steerLeft)
	shown := visible.String()
	pr := []rune(shown)
	split := 0
	for w := 0; split < len(pr) && w < promptCells; split++ {
		w += cellWidth(string(pr[split]))
	}
	frags := []styledFragment{{Style: "class:text-area.prompt", Text: string(pr[:split])}, {Text: string(pr[split:])}}
	return a.styledLine(frags, cols), cursorX - a.steerLeft
}
