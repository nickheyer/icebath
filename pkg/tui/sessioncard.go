package tui

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the terminal UI session, ahead of the toolkit session keys
const tuiContext = "tui"

// Rows the queue panel shows before it counts the remainder
const queuePanelRows = 4

// Seconds between spinner frames
const spinnerStep = 0.09

// Session window, the transcript above the queue, the control bars, the status row and the composer
type sessionCard struct {
	app      *app
	composer *cards.Composer
	view     *transcriptView
	width    int
	height   int
	// First content row of the composer, for its cursor and clicks
	composerTop int
	tick        int
	phase       float64
	lines       []string
	// Columns of the live session count on the status row, empty while the bar shows it
	sessionsBox [2]int
	// First content row of the transcript window, below the widgets docked above it
	viewTop int
	// Row of the scrollbar thumb the pointer holds while it drags the bar, minus one otherwise
	barGrab int
	// Pace of the wheel, the rows of one event growing as events follow closely and one row a frame while a modifier is held
	accel     wheelAccel
	precision precisionWheel
	// Whether the terminal is built on xterm.js, which opens the links it shows itself and sends wheel events at its own cadence
	xtermJS bool
	// Whether the pointer holds a selection it started in the prompt, and when and where in the prompt the left button last went down
	promptDrag bool
	pressedAt  time.Time
	pressedOn  int
}

// Window of one app with an empty transcript and a focused prompt
func newSessionCard(a *app) *sessionCard {
	c := &sessionCard{app: a, view: newTranscriptView(a.store.nowMillis), barGrab: -1, xtermJS: os.Getenv("TERM_PROGRAM") == "vscode"}
	c.accel = newWheelAccel(c.xtermJS, scrollSpeedBase(os.LookupEnv))
	c.composer = cards.NewComposer(a.uiTheme(), a.keys, "", a.store.sessionCwd)
	c.composer.Literal = true
	c.composer.Hint = composerHint()
	c.composer.Seed(a.inputs.entries())
	c.composer.OnRemember = a.inputs.push
	c.composer.Apply = a.applyOffer
	c.composer.Sending = func() { a.collapsePaste() }
	return c
}

func (c *sessionCard) Title() string {
	if title := trimJS(c.app.store.ui.SessionTitle); title != "" {
		return title
	}
	return c.untitled()
}

// Name of the session without its title, the short session id when it has one
func (c *sessionCard) untitled() string {
	if sid := c.app.store.ui.Sid; sid != "" {
		return tr(txt.SessionCardTitleOf, sessionLabel(sid))
	}
	return tr(txt.SessionCardTitle)
}

func (c *sessionCard) Glyph() string       { return c.app.uiTheme().Glyphs.Session }
func (c *sessionCard) Key() string         { return "session" }
func (c *sessionCard) Init() tea.Cmd       { return tea.RequestTerminalVersion }
func (c *sessionCard) Closable() bool      { return false }
func (c *sessionCard) Selectable() bool    { return true }
func (c *sessionCard) MinSize() (int, int) { return 40, 10 }

// Keys in force, the stash panel and completion popup first, then the terminal UI keys over the session keys
func (c *sessionCard) Contexts() []string {
	var ctxs []string
	if c.app.stashBrowsing() {
		ctxs = append(ctxs, stashContext)
	}
	if c.composer.Completing() {
		ctxs = append(ctxs, keys.Complete)
	}
	return append(ctxs, tuiContext, keys.Session)
}

// Spinner frames advance while a turn runs and ambient widgets refresh on their interval, the window repainting on either
func (c *sessionCard) Animate(dt float64) bool {
	refreshed := c.app.widgets.refresh(time.Now(), c.app.widgets.ambient)
	if !c.app.store.ui.Busy && len(c.app.store.turn.Tools) == 0 {
		return refreshed
	}
	c.phase += dt
	if c.phase < spinnerStep {
		return refreshed
	}
	c.phase = 0
	c.tick++
	return true
}

// Keeps the frame clock running while an ambient widget refreshes on an interval
func (c *sessionCard) Awake() bool { return c.app.widgets.refreshing(c.app.widgets.ambient) }

func (c *sessionCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	switch m := m.(type) {
	case wm.FocusMsg:
		return c, c.composer.SetFocus(m.Focused)
	case wm.ActionMsg:
		if m.ID != "session.stop" {
			c.app.rewindPrimed = ""
		}
		return c, c.action(m.ID)
	case tea.KeyPressMsg:
		c.app.rewindPrimed = ""
		cmd := c.composer.Update(m)
		return c, tea.Batch(c.app.typed(cmd), c.app.syncTokens())
	case tea.PasteMsg:
		c.app.rewindPrimed = ""
		return c, c.app.pasted(m.Content)
	case tea.MouseWheelMsg:
		c.wheel(m)
	case tea.MouseClickMsg:
		if m.Y >= c.composerTop {
			return c, c.promptPress(m)
		}
		if m.Button == tea.MouseLeft && c.Grabs(m.X, m.Y) {
			size, start, _ := c.view.thumb()
			row := m.Y - c.viewTop
			if c.barGrab = size / 2; row >= start && row < start+size {
				c.barGrab = row - start
			}
			c.view.scrollThumb(row - c.barGrab)
		}
	case tea.MouseMotionMsg:
		switch {
		case m.Button != tea.MouseLeft:
		case c.barGrab >= 0:
			c.view.scrollThumb(min(max(m.Y-c.viewTop, 0), c.view.height-1) - c.barGrab)
		case c.promptDrag:
			c.composer.Drag(m.X, m.Y-c.composerTop)
		}
	case tea.MouseReleaseMsg:
		c.barGrab = -1
		if c.promptDrag {
			return c, c.promptRelease()
		}
	case wm.TapMsg:
		return c, c.tap(m)
	case tea.TerminalVersionMsg:
		if strings.HasPrefix(m.Name, "xterm.js") && !c.xtermJS {
			c.xtermJS = true
			c.accel = newWheelAccel(true, scrollSpeedBase(os.LookupEnv))
		}
	case keys.ChangedMsg:
		c.composer.Rebind()
	}
	return c, nil
}

// Reports whether a press at the cell takes hold of the transcript scrollbar or of the text of the prompt, the drag then moving the thumb or selecting
// in the prompt instead of sweeping text
func (c *sessionCard) Grabs(x, y int) bool {
	if y >= c.composerTop {
		return c.composer.Holds(x, y-c.composerTop)
	}
	return c.view.barShown() && x == c.view.width-1 && y >= c.viewTop && y < c.viewTop+c.view.height
}

// Longest two presses on one place of the prompt lie apart to select the whole prompt
const promptDoubleClick = 500 * time.Millisecond

// Press on the prompt. The send button sends. On the text the right button copies what is selected or, with nothing selected, pastes the clipboard;
// the left button puts the cursor there and starts a selection the drag extends, a second press on the same place selecting the whole prompt
func (c *sessionCard) promptPress(m tea.MouseClickMsg) tea.Cmd {
	y := m.Y - c.composerTop
	if m.Button == tea.MouseLeft {
		if cmd, sent := c.composer.Click(m.X, y); sent {
			return cmd
		}
	}
	if !c.composer.Holds(m.X, y) {
		return nil
	}
	c.app.rewindPrimed = ""
	switch m.Button {
	case tea.MouseRight:
		if _, selected := c.composer.Selection(); selected {
			return c.composer.Action("composer.copy")
		}
		return c.app.pasteFromClipboard()
	case tea.MouseLeft:
		c.composer.Press(m.X, y)
		now, on := c.app.store.now(), c.composer.Offset()
		again := on == c.pressedOn && now.Sub(c.pressedAt) < promptDoubleClick
		c.pressedAt, c.pressedOn, c.promptDrag = now, on, true
		if again {
			c.composer.SelectAll()
		}
	}
	return nil
}

// Release of a press on the prompt, the selection ending where the pointer is and, on a Mac, going to the clipboard
func (c *sessionCard) promptRelease() tea.Cmd {
	c.promptDrag = false
	c.composer.Release()
	if _, selected := c.composer.Selection(); selected && isMac {
		return c.composer.Action("composer.copy")
	}
	return nil
}

// Scrolls the transcript by a wheel event, one row a frame while Alt or Ctrl is held and otherwise by rows that grow as the events follow closely
func (c *sessionCard) wheel(m tea.MouseWheelMsg) {
	dir := 0
	switch m.Button {
	case tea.MouseWheelUp:
		dir = -1
	case tea.MouseWheelDown:
		dir = 1
	default:
		return
	}
	now := c.app.store.nowMillis()
	precise, entered, rows := c.precision.step(dir, m.Mod.Contains(tea.ModAlt) || m.Mod.Contains(tea.ModCtrl), now)
	switch {
	case !precise:
		rows = c.accel.step(dir, now)
	case entered:
		c.accel = newWheelAccel(c.xtermJS, scrollSpeedBase(os.LookupEnv))
	}
	if rows != 0 {
		c.view.scrollBy(dir * rows)
	}
	c.app.turn.boostStreamingForScroll()
}

// Carries out a press and release on one cell, the session switcher opened from the live session count of the status row, the part of the transcript block under it opened or closed, else the address under it opened unless the terminal opens the links it shows itself
func (c *sessionCard) tap(m wm.TapMsg) tea.Cmd {
	if m.Y == c.composerTop-1 && m.X >= c.sessionsBox[0] && m.X < c.sessionsBox[1] {
		return c.app.wm.Fire("open.sessions")
	}
	if m.X < 0 || m.X >= c.view.width {
		return nil
	}
	row, shown := c.view.rowAt(m.Y - c.viewTop)
	if !shown {
		return nil
	}
	if id, part, found := c.view.partAt(row); found {
		return c.app.clickPart(id, part, row, m.Mod)
	}
	if m.Link != "" && !c.xtermJS {
		openExternalURL(m.Link)
	}
	return nil
}

// Keys that leave the prompt at its edges, up and down stepping through the queued inputs and enter on an empty prompt acting on the turn or the queue,
// false when the key stays with the prompt
func (c *sessionCard) promptEdge(id string) (tea.Cmd, bool) {
	switch {
	case id == "composer.up" && c.composer.AtTop():
		return c.app.cycleQueue(1)
	case id == "composer.down" && c.composer.AtBottom():
		return c.app.cycleQueue(-1)
	case id == "composer.send":
		return c.app.emptyEnter()
	}
	return nil, false
}

// Carries out a keymap action, the transcript and session ones here and the prompt ones in the composer
func (c *sessionCard) action(id string) tea.Cmd {
	if cmd, taken := c.promptEdge(id); taken {
		return cmd
	}
	page := max(c.view.height-2, 1)
	switch id {
	case "session.scroll.up":
		c.view.scrollBy(-page)
	case "session.scroll.down":
		c.view.scrollBy(page)
	case "session.scroll.top":
		c.view.scrollTop()
	case "session.scroll.end":
		c.view.scrollToBottom()
	case "tui.scroll.line.up":
		c.view.scrollBy(-1)
	case "tui.scroll.line.down":
		c.view.scrollBy(1)
	case "session.stop":
		return c.app.escape()
	case "session.expand":
		return c.app.setDetails("expanded")
	case "session.collapse":
		return c.app.setDetails("collapsed")
	case "session.thinking":
		return c.app.toggleThinking()
	case "tui.paste.expand":
		if cmd, taken := c.app.togglePaste(); taken {
			return cmd
		}
		c.app.toggleSentPastes()
	case "composer.attachments.clear":
		return c.app.clearAttachments()
	default:
		if cmd, handled := c.app.tuiAction(id); handled {
			return cmd
		}
		before := c.composer.Value()
		cmd := c.composer.Action(id)
		if (id == "composer.send" || id == "complete.accept") && trimJS(before) != "" && c.composer.Value() == "" {
			return c.app.typed(cmd)
		}
		return tea.Batch(c.app.typed(cmd), c.app.syncTokens())
	}
	return nil
}

// Look every block is painted with at the current width
func (c *sessionCard) renderCtx(width int) renderCtx {
	ui := c.app.store.ui
	details := detailsCtx{CommandOverride: ui.DetailsModeCommandOverride, DetailsMode: ui.DetailsMode, Sections: ui.Sections}
	expand := c.app.keys.Display("session.expand")
	look := c.app.themeLook + "|" + ui.DetailsMode + fmt.Sprint(ui.Sections, ui.DetailsModeCommandOverride, ui.Timestamps, ui.InlineImages) + "|" + expand
	return renderCtx{t: ui.Theme, ut: c.app.uiTheme(), width: width, details: details, timestamps: ui.Timestamps, look: look, focus: ui.FocusView, compact: ui.Compact,
		expandKey: expand, images: c.app.images, inlineImages: ui.InlineImages}
}

// Rows of the queue panel, from the first shown to one past the last, the window following the input held for editing
func queueWindow(count int, edit *int) (start, end int) {
	if edit != nil {
		start = max(0, min(*edit-1, count-queuePanelRows))
	}
	return start, min(count, start+queuePanelRows)
}

// Queued inputs waiting behind the turn, the one held for editing marked and kept in view
func (c *sessionCard) queueRows(width int, rc renderCtx) []string {
	shown, err := c.app.queue.queuedDisplay()
	if err != nil || len(shown) == 0 {
		return nil
	}
	muted, edit := toneStyle(rc.t.Color.Muted), c.app.queueEdit
	header := toneStyle(rc.t.Color.Label).Render("  " + tr(txt.ChatBitsQueuedHeader, len(shown)))
	if edit != nil {
		header += muted.Render(tr(txt.ChatBitsQueuedEditing, *edit+1))
	}
	rows := []string{header}
	start, end := queueWindow(len(shown), edit)
	if start > 0 {
		rows = append(rows, muted.Render("    …"))
	}
	for i := start; i < end; i++ {
		mark, tone := fmt.Sprintf("  %d ", i+1), rc.t.Color.Muted
		if edit != nil && *edit == i {
			mark, tone = "  ✎ ", rc.t.Color.Accent
		}
		rows = append(rows, toneStyle(tone).Render(mark+truncateEnd(strings.ReplaceAll(shown[i], "\n", " "), max(width-6, 8))))
	}
	if end < len(shown) {
		rows = append(rows, muted.Render("    "+tr(txt.ChatBitsQueuedMore, len(shown)-end)))
	}
	return rows
}

const (
	// Hotkeys the quick help card lists under the common commands
	quickHelpHotkeys = 8
	// Rows the quick help card leaves to the status row, the prompt and the end of the transcript
	quickHelpReserve = 6
)

// Card of the common commands and the first hotkeys, shown while the draft is one question mark and cut to the rows the window can spare
func (c *sessionCard) quickHelpRows(width, room int, rc renderCtx) []string {
	if c.composer.Value() != "?" || room < 3 {
		return nil
	}
	t := rc.t
	commands := [][2]string{{"/help", tr(txt.HelpCommandsHelp)}, {"/clear", tr(txt.HelpCommandsClear)}, {"/resume", tr(txt.HelpCommandsResume)},
		{"/details", tr(txt.HelpCommandsDetails)}, {"/copy", tr(txt.HelpCommandsCopy)}, {"/quit", tr(txt.HelpCommandsQuit)}}
	hotkeys := c.app.hotkeyRows()
	hotkeys = hotkeys[:min(len(hotkeys), quickHelpHotkeys)]
	title, tail := tr(txt.HelpQuickHelp), tr(txt.HelpQuickHelpTail)
	label, inner := 0, ansi.StringWidth(title+tail)
	for _, row := range slices.Concat(commands, hotkeys) {
		label = max(label, ansi.StringWidth(row[0]))
	}
	for _, row := range slices.Concat(commands, hotkeys) {
		inner = max(inner, label+2+ansi.StringWidth(row[1]))
	}
	inner = max(12, min(inner, width-6))
	pair := func(row [2]string) string {
		key := row[0] + strings.Repeat(" ", label-ansi.StringWidth(row[0])+2)
		return toneStyle(t.Color.Label).Render(key) + toneStyle(t.Color.Muted).Render(truncateEnd(row[1], max(inner-ansi.StringWidth(key), 1)))
	}
	head := toneStyle(t.Color.Accent).Bold(true)
	lines := []string{toneStyle(t.Color.Primary).Bold(true).Render(title) + toneStyle(t.Color.Muted).Render(truncateEnd(tail, max(inner-ansi.StringWidth(title), 1))),
		"", head.Render(tr(txt.HelpCommonCommands))}
	for _, row := range commands {
		lines = append(lines, pair(row))
	}
	lines = append(lines, "", head.Render(tr(txt.HelpHotkeys)))
	for _, row := range hotkeys {
		lines = append(lines, pair(row))
	}
	lines = lines[:min(len(lines), room-2)]
	border := toneStyle(t.Color.Primary)
	rows := []string{"  " + border.Render("╭"+strings.Repeat("─", inner+2)+"╮")}
	for _, line := range lines {
		rows = append(rows, "  "+border.Render("│")+" "+line+strings.Repeat(" ", max(inner-ansi.StringWidth(line), 0))+" "+border.Render("│"))
	}
	return append(rows, "  "+border.Render("╰"+strings.Repeat("─", inner+2)+"╯"))
}

// Busy mark in the style the user picked, the braille wheel, an emoji phase or plain strokes
func (c *sessionCard) indicator(rc renderCtx) string {
	switch c.app.store.ui.IndicatorStyle {
	case "ascii":
		return toneStyle(rc.t.Color.Accent).Render([]string{"|", "/", "-", `\`}[c.tick%4])
	case "emoji":
		return []string{"🌑", "🌒", "🌓", "🌔", "🌕", "🌖", "🌗", "🌘"}[(c.tick/2)%8]
	}
	return render.Spinner(c.tick, rc.ut)
}

// Columns a status fact takes before its text on the status row
const factGap = 3

// Status row, the spinner, the status of the turn and its facts at the left and the notice or hint, the facts of the session and the affection heart at the right
func (c *sessionCard) statusRow(width int, rc renderCtx) string {
	ui := c.app.store.ui
	t := rc.t
	muted := toneStyle(t.Color.Muted)
	c.sessionsBox = [2]int{}
	left := "  "
	raw := trimJS(ui.Status)
	status := displayStatus(raw)
	if ui.Busy {
		left += c.indicator(rc) + " "
		switch {
		case ui.Compacting:
			status = tr(txt.StatusCompacting) + "…"
		case raw == "running…" && len(c.app.store.turn.Tools) == 0:
			status = spinnerVerbs[(c.tick/45)%len(spinnerVerbs)] + "…"
		}
		if started := c.app.turnStartedAt; !started.IsZero() {
			status += " · " + render.Elapsed(c.app.store.now().Sub(started).Truncate(time.Second))
		}
		if ui.SpinnerTokenFlow {
			if flow := formatTokenFlow(c.app.turn.flowTokens(), "↓"); flow != "" {
				status += " · " + flow
			}
		}
	}
	tone := t.Color.Muted
	if ui.GatewayConnected != nil && !*ui.GatewayConnected {
		tone = t.Color.Warn
	}
	left += toneStyle(tone).Render(status)
	shown := ui.StatusBar != "off"
	if shown && ui.FocusView {
		left += muted.Render(" · ") + toneStyle(t.Color.Warn).Render(tr(txt.StatusFocus))
	}
	right, capturing := "", false
	switch {
	case ui.Notice != nil:
		noticeTone := t.Color.Accent
		switch ui.Notice.Level {
		case "warn", "warning":
			noticeTone = t.Color.Warn
		case "error":
			noticeTone = t.Color.Error
		case "success":
			noticeTone = t.Color.StatusGood
		}
		right = toneStyle(noticeTone).Render(ui.Notice.Text)
	case c.app.store.voicePartial != "":
		right, capturing = toneStyle(t.Color.Accent).Render("● "+c.app.store.voicePartial), true
	case c.app.voiceRecording:
		right, capturing = toneStyle(t.Color.Error).Render(tr(txt.ComposerRecording)), true
	case c.app.voiceProcessing:
		right, capturing = toneStyle(t.Color.Accent).Render(tr(txt.ComposerTranscribing)), true
	case len(imageAttachments(c.composer.Value(), c.app.tokens)) > 0:
		right = toneStyle(t.Color.Accent).Render(trCount(txt.ComposerImagesAttachedOne, txt.ComposerImagesAttachedOther, len(imageAttachments(c.composer.Value(), c.app.tokens))))
	case ui.BusyInputMode != "" && ui.Busy:
		right = muted.Render(tr(txt.ComposerEnterMode, ui.BusyInputMode))
	}
	tail := ""
	if extra := c.composer.Status(); extra != "" {
		tail = muted.Render(extra)
	}
	if heart := c.app.goodVibesHeart(t); heart != "" {
		tail = strings.TrimSpace(tail + " " + heart)
	}
	room := width - ansi.StringWidth(left) - 1
	for _, fixed := range []string{right, tail} {
		if fixed != "" {
			room -= ansi.StringWidth(fixed) + 2
		}
	}
	cols, _ := c.app.wm.Size()
	facts := append(c.app.turnFacts(cols), c.app.sessionFacts(cols)...)
	if ui.StatusBar == "bottom" {
		facts = append(facts, c.app.barFacts(cols, rowFolderCols)...)
	}
	slices.SortStableFunc(facts, func(a, b statusFact) int { return a.rank - b.rank })
	paint := func(f statusFact) string { return toneStyle(factTone(f.style, t)).Faint(f.faint).Render(f.text) }
	var aside []statusFact
	for _, f := range facts {
		need := factGap + ansi.StringWidth(f.text)
		if need > room || (capturing && f.rank == rankVoice) {
			continue
		}
		room -= need
		if f.turn {
			left += muted.Render(" · ") + paint(f)
		} else {
			aside = append(aside, f)
		}
	}
	if hud, style := spawnHUD(c.app.store.turn.Subagents, c.app.store.delegation); shown && hud != "" && room > factGap {
		left += muted.Render(" · ") + toneStyle(factTone(style, t)).Render(truncateEnd(hud, room-factGap))
	}
	sessionsAt, sessionsWide := -1, 0
	for i, f := range aside {
		switch {
		case i > 0:
			right += muted.Render(" │ ")
		case right != "":
			right += "  "
		}
		if f.action == "open.sessions" {
			sessionsAt, sessionsWide = ansi.StringWidth(right), ansi.StringWidth(f.text)
		}
		right += paint(f)
	}
	if tail != "" && right != "" {
		right += "  "
	}
	right += tail
	room = width - ansi.StringWidth(left) - 1
	if room < 4 {
		return render.Fit(left, width, rc.ut)
	}
	if wide := ansi.StringWidth(right); sessionsAt >= 0 && wide <= room {
		c.sessionsBox = [2]int{width - wide + sessionsAt, width - wide + sessionsAt + sessionsWide}
	}
	right = truncateEnd(right, room)
	return left + strings.Repeat(" ", max(width-ansi.StringWidth(left)-ansi.StringWidth(right), 1)) + right
}

func (c *sessionCard) View(w, h int) string {
	c.width, c.height = w, h
	rc := c.renderCtx(w)
	c.composer.Hint = c.app.composerPlaceholder()
	c.composer.Fit(w)
	var chrome []string
	chrome = append(chrome, c.queueRows(w, rc)...)
	if goal := goalBarView(max(w-2, 8), w, c.app.store.currentGoalLine(), rc.t); goal != "" {
		chrome = append(chrome, "  "+goal)
	}
	if loop := loopBarView(max(w-2, 8), w, c.app.store.currentLoopLine(), rc.t); loop != "" {
		chrome = append(chrome, "  "+loop)
	}
	for _, row := range c.app.dockRows(w, rc) {
		chrome = append(chrome, row)
	}
	chrome = append(chrome, c.app.stashPanelRows(w, rc)...)
	chrome = append(chrome, c.app.rewindHintRows(rc)...)
	chrome = append(chrome, c.app.pasteHintRows(rc)...)
	chrome = append(chrome, c.quickHelpRows(w, h-len(chrome)-quickHelpReserve, rc)...)
	chrome = append(chrome, c.statusRow(w, rc))
	composerRows := min(c.composer.Height(), max(h-len(chrome)-3, 2))
	bodyRows := max(h-len(chrome)-composerRows, 1)
	top := c.app.widgets.dock("dock-top", w)
	bottom := c.app.widgets.dock("dock-bottom", w)
	bodyRows = max(bodyRows-len(top)-len(bottom), 1)
	pet := c.app.pet.place(w, bodyRows, len(top))
	wrap := rc
	wrap.width = scrollbarRoom(w - pet.gutter)
	c.lines, c.viewTop = c.view.rows(c.app.history, c.app.store, c.app.turn.visibleStream(), wrap, c.tick), len(top)
	rows := pet.over(strings.Split(c.view.view(c.lines, w-pet.gutter, bodyRows-pet.band, rc), "\n"), w)
	body := strings.Join(c.app.widgets.overlayCorners(rows, w), "\n")
	if len(top) > 0 {
		body = strings.Join(top, "\n") + "\n" + body
	}
	if len(bottom) > 0 {
		body += "\n" + strings.Join(bottom, "\n")
	}
	bodyRows += len(top) + len(bottom)
	c.composerTop = bodyRows + len(chrome)
	parts := []string{body}
	parts = append(parts, chrome...)
	parts = append(parts, c.composer.View(w, composerRows))
	return strings.Join(parts, "\n")
}

// Screen cell the content starts at, where the pet image is placed from
func (c *sessionCard) Origin(x, y int) { c.app.pet.origin(x, y) }

// Box of a pet image the terminal places under the cells
func (c *sessionCard) Holes() []wm.Box { return c.app.pet.holes() }

// Cursor of the prompt, moved below the transcript and chrome
func (c *sessionCard) Cursor() *tea.Cursor {
	cur := c.composer.Cursor()
	if cur == nil {
		return nil
	}
	cur.Y += c.composerTop
	return cur
}

// Trailer of the window frame, the model and context use of the session
func (c *sessionCard) Status() string {
	return c.app.frameStatus()
}

// Legend of the keys that matter right now
func (c *sessionCard) Hints() []wm.Hint {
	edits := []wm.Hint{{Action: "composer.undo"}, {Action: "composer.redo", All: true}, {Action: "tui.suspend"}}
	lead := []wm.Hint{{Action: "composer.send"}, {Action: "tui.editor"}, {Action: "palette"}}
	if c.app.store.ui.Busy {
		lead = []wm.Hint{{Action: "composer.send", Label: c.app.store.ui.BusyInputMode}, {Action: "interrupt"}}
	}
	var queued []wm.Hint
	switch {
	case c.app.queueEdit != nil:
		queued = []wm.Hint{{Action: "tui.queue.delete"}, {Action: "session.stop", Label: tr(txt.HintsLabelCancel)}}
	case c.app.queueHeadWaits():
		queued = []wm.Hint{{Action: "tui.queue.send"}}
	case c.app.hasQueued():
		queued = []wm.Hint{{Action: "composer.up", Label: tr(txt.HintsLabelEditQueued)}, {Action: "tui.queue.drop"}}
	}
	return slices.Concat(lead, queued, edits)
}
