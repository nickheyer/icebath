package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
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
}

// Window of one app with an empty transcript and a focused prompt
func newSessionCard(a *app) *sessionCard {
	c := &sessionCard{app: a, view: newTranscriptView(a.store.nowMillis)}
	c.composer = cards.NewComposer(a.uiTheme(), a.keys, "", a.store.sessionCwd)
	c.composer.Literal = true
	c.composer.Hint = composerHint()
	c.composer.Seed(a.inputs.entries())
	c.composer.OnRemember = a.inputs.push
	c.composer.Apply = a.applyOffer
	return c
}

func (c *sessionCard) Title() string {
	if title := trimJS(c.app.store.ui.SessionTitle); title != "" {
		return title
	}
	if sid := c.app.store.ui.Sid; sid != "" {
		return "session " + sessionLabel(sid)
	}
	return "session"
}

func (c *sessionCard) Glyph() string       { return c.app.uiTheme().Glyphs.Session }
func (c *sessionCard) Key() string         { return "session" }
func (c *sessionCard) Init() tea.Cmd       { return nil }
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
		return c, c.action(m.ID)
	case tea.KeyPressMsg:
		cmd := c.composer.Update(m)
		return c, tea.Batch(c.app.typed(cmd), c.app.syncTokens())
	case tea.PasteMsg:
		return c, c.app.pasted(m.Content)
	case tea.MouseWheelMsg:
		switch m.Button {
		case tea.MouseWheelUp:
			c.view.scrollBy(-3)
		case tea.MouseWheelDown:
			c.view.scrollBy(3)
		}
		c.app.turn.boostStreamingForScroll()
	case tea.MouseClickMsg:
		if m.Button == tea.MouseLeft && m.Y >= c.composerTop {
			if cmd, ok := c.composer.Click(m.X, m.Y-c.composerTop); ok {
				return c, cmd
			}
		}
	case keys.ChangedMsg:
		c.composer.Rebind()
	}
	return c, nil
}

// Carries out a keymap action, the transcript and session ones here and the prompt ones in the composer
func (c *sessionCard) action(id string) tea.Cmd {
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
	case "mode.cycle":
		return c.app.cycleBusyMode()
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
	look := c.app.themeLook + "|" + ui.DetailsMode + fmt.Sprint(ui.Sections, ui.DetailsModeCommandOverride, ui.Timestamps) + "|" + expand
	return renderCtx{t: ui.Theme, ut: c.app.uiTheme(), width: width, details: details, timestamps: ui.Timestamps, look: look, focus: ui.FocusView, compact: ui.Compact, expandKey: expand}
}

// Queued inputs waiting behind the turn, the one being edited marked
func (c *sessionCard) queueRows(width int, rc renderCtx) []string {
	shown, err := c.app.queue.queuedDisplay()
	if err != nil || len(shown) == 0 {
		return nil
	}
	rows := []string{toneStyle(rc.t.Color.Label).Render(fmt.Sprintf("  queued %d", len(shown))) + toneStyle(rc.t.Color.Muted).Render("  ctrl+k drops the newest · /queue edit <n>")}
	for i, text := range shown {
		if i == queuePanelRows {
			rows = append(rows, toneStyle(rc.t.Color.Muted).Render(fmt.Sprintf("    … %d more", len(shown)-i)))
			break
		}
		mark := fmt.Sprintf("  %d ", i+1)
		tone := rc.t.Color.Muted
		if c.app.queueEdit != nil && *c.app.queueEdit == i {
			mark, tone = "  ✎ ", rc.t.Color.Accent
		}
		rows = append(rows, toneStyle(tone).Render(mark+truncateEnd(strings.ReplaceAll(text, "\n", " "), max(width-6, 8))))
	}
	return rows
}

// Busy mark in the style the user picked, a face, an emoji phase, the braille spinner or plain strokes
func (c *sessionCard) indicator(rc renderCtx) string {
	switch c.app.store.ui.IndicatorStyle {
	case "ascii":
		return toneStyle(rc.t.Color.Accent).Render([]string{"|", "/", "-", `\`}[c.tick%4])
	case "emoji":
		return []string{"🌑", "🌒", "🌓", "🌔", "🌕", "🌖", "🌗", "🌘"}[(c.tick/2)%8]
	case "unicode":
		return render.Spinner(c.tick, rc.ut)
	}
	if kaomoji := faces(); len(kaomoji) > 0 {
		return toneStyle(rc.t.Color.Accent).Render(kaomoji[(c.tick/12)%len(kaomoji)])
	}
	return render.Spinner(c.tick, rc.ut)
}

// Status row, the spinner and status of the turn at the left and the notice or hint at the right
func (c *sessionCard) statusRow(width int, rc renderCtx) string {
	ui := c.app.store.ui
	left := "  "
	status := trimJS(ui.Status)
	if ui.Busy {
		left += c.indicator(rc) + " "
		if status == "running…" && len(c.app.store.turn.Tools) == 0 {
			status = spinnerVerbs[(c.tick/45)%len(spinnerVerbs)] + "…"
		}
		if started := c.app.turnStartedAt; !started.IsZero() {
			status += " · " + render.Elapsed(c.app.store.now().Sub(started).Truncate(time.Second))
		}
	}
	tone := rc.t.Color.Muted
	if ui.GatewayConnected != nil && !*ui.GatewayConnected {
		tone = rc.t.Color.Warn
	}
	left += toneStyle(tone).Render(status)
	right := ""
	switch {
	case ui.Notice != nil:
		noticeTone := rc.t.Color.Accent
		switch ui.Notice.Level {
		case "warn", "warning":
			noticeTone = rc.t.Color.Warn
		case "error":
			noticeTone = rc.t.Color.Error
		}
		right = toneStyle(noticeTone).Render(ui.Notice.Text)
	case c.app.store.voicePartial != "":
		right = toneStyle(rc.t.Color.Accent).Render("● " + c.app.store.voicePartial)
	case c.app.voiceRecording:
		right = toneStyle(rc.t.Color.Error).Render("● recording")
	case c.app.voiceProcessing:
		right = toneStyle(rc.t.Color.Accent).Render("● transcribing…")
	case len(imageAttachments(c.composer.Value(), c.app.tokens)) > 0:
		right = toneStyle(rc.t.Color.Accent).Render(countNoun(len(imageAttachments(c.composer.Value(), c.app.tokens)), "image") + " attached")
	case ui.BusyInputMode != "" && ui.Busy:
		right = toneStyle(rc.t.Color.Muted).Render("enter: " + ui.BusyInputMode)
	}
	if ui.StatusBar == "bottom" {
		var dials []string
		if model := infoText(infoOf(ui.Info).Model); model != "" {
			dials = append(dials, model)
		}
		if text, _, has := contextDial(ui.Usage); has {
			dials = append(dials, text)
		}
		if cwd := c.app.store.sessionCwd(); cwd != "" {
			dials = append(dials, fmtCwdBranch(cwd, c.app.git.current(), 28))
		}
		if pin := c.app.stashIndicator(); pin != "" {
			dials = append(dials, pin)
		}
		right = strings.TrimSpace(right + "  " + toneStyle(rc.t.Color.Muted).Render(strings.Join(dials, " │ ")))
	}
	if extra := c.composer.Status(); extra != "" {
		right = strings.TrimSpace(right + "  " + toneStyle(rc.t.Color.Muted).Render(extra))
	}
	room := width - ansi.StringWidth(left) - 1
	if room < 4 {
		return render.Fit(left, width, rc.ut)
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
	chrome = append(chrome, c.statusRow(w, rc))
	composerRows := min(c.composer.Height(), max(h-len(chrome)-3, 2))
	bodyRows := max(h-len(chrome)-composerRows, 1)
	top := c.app.widgets.dock("dock-top", w)
	bottom := c.app.widgets.dock("dock-bottom", w)
	bodyRows = max(bodyRows-len(top)-len(bottom), 1)
	pet := c.app.pet.place(w, bodyRows, len(top))
	wrap := rc
	wrap.width = scrollbarRoom(w - pet.gutter)
	c.lines = c.view.rows(c.app.history, c.app.store, c.app.turn.visibleStream(), wrap, c.tick)
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
	if c.app.store.ui.Busy {
		return append([]wm.Hint{{Action: "composer.send", Label: c.app.store.ui.BusyInputMode}, {Action: "interrupt"}, {Action: "mode.cycle", Label: "busy mode"}}, edits...)
	}
	return append([]wm.Hint{{Action: "composer.send"}, {Action: "tui.editor"}, {Action: "palette"}}, edits...)
}
