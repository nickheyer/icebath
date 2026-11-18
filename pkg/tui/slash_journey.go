package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Play frames and rate, partial reveal frames, the chart row cap and the size watch period
const (
	journeyPlayFrames   = 42
	journeyPlayFPS      = 12
	journeyRevealFrames = 101
	journeyMaxChartRows = 8
	journeyWatchEvery   = 200 * time.Millisecond
)

// Show options of a /journey run as the journey parser reads them
type journeyOptions struct {
	reveal        float64
	play          bool
	fps           int
	width, height int
	color         bool
}

// One learned skill or memory row of the timeline tree
type journeyNode = tuigateway.LearningNodeRow

// One time slice of the timeline with its counts and nodes
type journeyBucket = tuigateway.LearningBucketRow

// Row of the flattened tree, a slice header, a node under it, or a gap between slices
type journeyRow struct {
	kind   string
	bucket *journeyBucket
	node   *journeyNode
	last   bool
}

// Window of the learning journey, the pre-rendered timeline frames over the tree of slices and their nodes
type journeyCard struct {
	a       *app
	opts    journeyOptions
	data    *tuigateway.LearningFramesResult
	frames  []tuigateway.LearningFrame
	buckets []journeyBucket
	rows    []journeyRow
	err     string
	loaded  bool
	cursor  int
	item    bool
	scroll  int
	frame   int
	gen     int
	confirm bool
	notice  string
	busy    bool
	viewW   int
	viewH   int
	seen    [2]int
	asked   [2]int
}

// Opens the journey window, its first learning.frames request leaving once the window has a size
func (a *app) openJourney(opts journeyOptions) tea.Cmd {
	card := &journeyCard{a: a, opts: opts}
	return tea.Batch(a.wm.ShowOverlay(card), card.watch())
}

// Chart columns and rows a window of a size asks learning.frames for, the parser overrides first
func (c *journeyCard) chartSize(w, h int) (int, int) {
	cols := max(40, w-1)
	if c.opts.width != 0 {
		cols = max(24, max(40, c.opts.width)-2)
	}
	rows := max(5, min(journeyMaxChartRows, int(math.Floor(float64(max(16, h))*0.32))))
	if c.opts.height != 0 {
		rows = max(6, max(10, c.opts.height)-10-len(c.summary()))
	}
	return cols, rows
}

// Frames learning.frames renders for the run, the build up for a play and the reveal step for a partial reveal
func (c *journeyCard) frameCount() int {
	switch {
	case c.opts.play:
		return journeyPlayFrames
	case c.opts.reveal == 0 || c.opts.reveal == 1:
		return 2
	}
	return journeyRevealFrames
}

// Refetches the timeline once the window size settles on a chart it was not asked for, while the window shows
func (c *journeyCard) watch() tea.Cmd {
	return tea.Tick(journeyWatchEvery, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			if c.a.wm.Overlay() != wm.Card(c) && c.viewW != 0 {
				return nil
			}
			var load tea.Cmd
			size := [2]int{c.viewW, c.viewH}
			if cols, rows := c.chartSize(c.viewW, c.viewH); c.viewW != 0 && size == c.seen && c.asked != [2]int{cols, rows} {
				load = c.refetch()
			}
			c.seen = size
			return tea.Batch(load, c.watch())
		}}
	})
}

// Asks for the timeline again at the chart size the window needs now
func (c *journeyCard) refetch() tea.Cmd {
	cols, rows := c.chartSize(c.viewW, c.viewH)
	c.asked = [2]int{cols, rows}
	return c.load(cols, rows)
}

// Asks the gateway for the timeline frames at a chart size and hands them to the card
func (c *journeyCard) load(cols, rows int) tea.Cmd {
	c.gen++
	gen := c.gen
	c.data, c.loaded, c.err = nil, false, ""
	params := tuigateway.LearningFramesParams{Cols: int64(cols), Frames: int64(c.frameCount()), Rows: int64(rows)}
	return call(c.a.gw, tuigateway.LearningFrames, params, func(data *tuigateway.LearningFramesResult, err error) tea.Cmd {
		return c.loadedFrames(gen, data, err)
	})
}

// Takes fresh frames, the cursor on the newest row and the timeline view, and starts the animation when asked
func (c *journeyCard) loadedFrames(gen int, data *tuigateway.LearningFramesResult, err error) tea.Cmd {
	if gen != c.gen {
		return nil
	}
	c.loaded = true
	if err != nil {
		c.err = rpcErrorMessage(err)
		return nil
	}
	c.data, c.frames, c.buckets = data, data.Frames, data.Buckets
	c.rows = nil
	for b := range c.buckets {
		bucket := &c.buckets[b]
		if b > 0 {
			c.rows = append(c.rows, journeyRow{kind: "gap"})
		}
		c.rows = append(c.rows, journeyRow{kind: "slice", bucket: bucket})
		for n := range bucket.Nodes {
			c.rows = append(c.rows, journeyRow{kind: "node", bucket: bucket, node: &bucket.Nodes[n], last: n == len(bucket.Nodes)-1})
		}
	}
	c.cursor, c.item, c.scroll = max(0, len(c.rows)-1), false, 0
	c.frame = max(0, len(c.frames)-1)
	switch {
	case c.opts.play:
		c.frame = 0
		return c.tick(gen, 0)
	case c.opts.reveal == 0:
		c.frame = 0
	case c.opts.reveal != 1:
		c.frame = min(len(c.frames)-1, int(math.RoundToEven(c.opts.reveal*float64(journeyRevealFrames-1))))
	}
	if _, rows := c.chartSize(c.viewW, c.viewH); c.opts.height != 0 && c.asked[1] != rows {
		return c.refetch()
	}
	return nil
}

// Next frame of the build up animation, the last one resting on the full timeline
func (c *journeyCard) tick(gen, frame int) tea.Cmd {
	delay := time.Duration(float64(time.Second) / float64(max(1, min(60, c.opts.fps))))
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			if gen != c.gen {
				return nil
			}
			c.frame = min(frame, len(c.frames)-1)
			if frame+1 < len(c.frames) {
				return c.tick(gen, frame+1)
			}
			return nil
		}}
	})
}

// Summary lines of the timeline answer, none before it arrived
func (c *journeyCard) summary() []string {
	if c.data == nil {
		return nil
	}
	return c.data.Summary
}

func (c *journeyCard) Title() string  { return "journey" }
func (c *journeyCard) Key() string    { return "tui:journey" }
func (c *journeyCard) Init() tea.Cmd  { return nil }
func (c *journeyCard) Raw() bool      { return true }
func (c *journeyCard) Status() string { return tr("journey.subtitle") }

func (c *journeyCard) MinSize() (int, int) {
	w, h := c.a.wm.Size()
	return max(min(w-4, 120), 40), max(h-4, 16)
}

// Painted text in a hex color, plain when color is off or the hex is empty
func (c *journeyCard) ink(text, hex string, bold bool) string {
	if !c.opts.color || hex == "" {
		if bold && c.opts.color {
			return lipgloss.NewStyle().Bold(true).Render(text)
		}
		return text
	}
	return lipgloss.NewStyle().Foreground(toneColor(hex)).Bold(bold).Render(text)
}

// Starmap palette of the session theme the chart and the tree are inked with
func (c *journeyCard) palette() starmapPalette {
	t := c.a.store.ui.Theme
	return deriveStarmapPalette(t.Color.Primary, t.Color.Text)
}

// Ink of a render style at an alpha, empty for a style the palette does not know
func (c *journeyCard) styleInk(p starmapPalette, style string, alpha float64) string {
	hex, _ := p.fadeInk(style, alpha)
	return hex
}

// Chart rows of the frame shown, trajectory rows left out and the oldest past the cap dropped
func (c *journeyCard) chart(chartCap int) ([]string, *tuigateway.LearningFrame) {
	if c.frame >= len(c.frames) {
		return nil, nil
	}
	frame := &c.frames[c.frame]
	p := c.palette()
	var out []string
	for _, row := range frame.Grid {
		var b, plain strings.Builder
		for _, run := range row {
			hex := c.styleInk(p, run.Style, run.Alpha)
			if run.Hex != "" {
				hex = p.fadeHex(run.Hex, run.Alpha)
			}
			plain.WriteString(run.Text)
			b.WriteString(c.ink(run.Text, hex, false))
		}
		if strings.HasPrefix(strings.TrimLeftFunc(plain.String(), isJSSpace), "trajectory") {
			continue
		}
		out = append(out, b.String())
	}
	if len(out) > chartCap {
		out = out[len(out)-chartCap:]
	}
	return out, frame
}

// Header, legends, chart, axis and playhead footer of the timeline answer at the chart width it was asked for
func (c *journeyCard) header(chartCap int) []string {
	t := c.a.store.ui.Theme
	p := c.palette()
	lines := []string{c.ink("✦ "+tr("journey.title"), t.Color.Primary, true) + c.ink(" "+tr("journey.subtitle"), t.Color.Muted, false)}
	var b strings.Builder
	for i, item := range c.data.Legend {
		if i > 0 {
			b.WriteString("   ")
		}
		style := item.Style
		if style == "" {
			style = "dim"
		}
		b.WriteString(c.ink(item.Glyph+" ", c.styleInk(p, style, 1), false))
		b.WriteString(c.ink(item.Label, t.Color.Muted, false))
	}
	lines = append(lines, b.String())
	if categories := c.data.Categories; len(categories) > 0 {
		var cb strings.Builder
		for i, item := range categories {
			if i > 0 {
				cb.WriteString("  ")
			}
			hex := t.Color.Muted
			if item.Color != "" {
				hex = p.fadeHex(item.Color, 1)
			}
			cb.WriteString(c.ink(item.Glyph+" ", hex, false))
			cb.WriteString(c.ink(item.Label, t.Color.Muted, false))
		}
		lines = append(lines, cb.String())
	}
	lines = append(lines, "")
	chart, frame := c.chart(chartCap)
	lines = append(lines, chart...)
	start, end := c.data.Axis.Start, c.data.Axis.End
	gap := max(1, c.asked[0]-2-utf8.RuneCountInString(start)-utf8.RuneCountInString(end))
	lines = append(lines, c.ink(start+strings.Repeat(" ", gap)+end, t.Color.Muted, false))
	if frame != nil {
		date := frame.Date
		if date == "" {
			date = "—"
		}
		pct := int(math.RoundToEven(frame.Reveal * 100))
		lines = append(lines, c.ink("◷ ", t.Color.Muted, false)+c.ink(date, t.Color.Primary, false)+
			c.ink(fmt.Sprintf("   %d/%d revealed · %d%%", frame.Visible, c.data.Count, pct), t.Color.Muted, false))
	}
	return lines
}

// Count with its singular or plural catalog form
func journeyCount(one, other string, n int64) string {
	if n == 1 {
		return tr(one, fmt.Sprint(n))
	}
	return tr(other, fmt.Sprint(n))
}

// One tree row painted, the row under the cursor on the selection chip
func (c *journeyCard) treeLine(row journeyRow, active bool, w int) string {
	t := c.a.store.ui.Theme
	p := c.palette()
	var cells [][2]string
	switch row.kind {
	case "gap":
		return ""
	case "slice":
		b := row.bucket
		hex := t.Color.Label
		if b.Color != "" {
			hex = p.fadeHex(b.Color, 0.85)
		}
		meta := " · " + journeyCount("journey.slice.skillsOne", "journey.slice.skillsOther", b.Skills) + " · " +
			journeyCount("journey.slice.memoriesOne", "journey.slice.memoriesOther", b.Memories)
		if b.Category != "" {
			meta += " · " + b.Category
		}
		cells = [][2]string{{b.Label, hex}, {meta, t.Color.Muted}}
	default:
		n := row.node
		branch := " ├─ "
		if row.last {
			branch = " └─ "
		}
		label := n.FullLabel
		if label == "" {
			label = n.Label
		}
		more := ""
		if n.Body != "" {
			more = "  ›"
		}
		cells = [][2]string{{branch, t.Color.Muted}, {n.Glyph + " " + label, c.styleInk(p, n.Style, 1)}, {"  " + n.Meta + more, t.Color.Muted}}
	}
	var b strings.Builder
	for _, cell := range cells {
		hex := cell[1]
		if active {
			hex = t.Color.Accent
		}
		b.WriteString(c.ink(cell[0], hex, active))
	}
	line := ansi.Truncate(b.String(), w, "…")
	if active && c.opts.color && t.Color.SelectionBg != "" {
		pad := strings.Repeat(" ", max(0, w-ansi.StringWidth(line)))
		return lipgloss.NewStyle().Background(toneColor(t.Color.SelectionBg)).Render(line + pad)
	}
	return line
}

// Node under the cursor, nil on a slice header
func (c *journeyCard) activeNode() *journeyNode {
	if c.cursor < 0 || c.cursor >= len(c.rows) {
		return nil
	}
	return c.rows[c.cursor].node
}

// Status line of a pending delete or the last notice
func (c *journeyCard) status() string {
	t := c.a.store.ui.Theme
	if c.confirm {
		if n := c.activeNode(); n != nil {
			label := n.FullLabel
			if label == "" {
				label = n.Label
			}
			return c.ink(tr("journey.confirmDelete", label), t.Color.Error, false)
		}
	}
	if c.notice != "" {
		return c.ink(c.notice, t.Color.Accent, false)
	}
	return ""
}

func (c *journeyCard) View(w, h int) string {
	c.viewW, c.viewH = w, h
	t := c.a.store.ui.Theme
	shell := func(line string) string {
		return strings.Join([]string{c.ink("✦ "+tr("journey.title"), t.Color.Primary, true), line, c.ink(tr("journey.closeHint"), t.Color.Muted, false)}, "\n")
	}
	switch {
	case c.err != "":
		return shell(c.ink(tr("journey.error", c.err), t.Color.Error, false))
	case !c.loaded:
		return shell(c.ink(tr("journey.loading"), t.Color.Muted, false))
	case c.data.Count == 0:
		return shell(c.ink(tr("journey.empty"), t.Color.Muted, false))
	}
	if c.item {
		return c.itemView(w, h)
	}
	chartCap := journeyMaxChartRows
	if c.opts.height != 0 {
		chartCap = c.asked[1]
	}
	head := c.header(chartCap)
	var foot []string
	if status := c.status(); status != "" {
		foot = append(foot, status)
	} else if summary := c.summary(); len(summary) > 0 {
		foot = append(foot, c.ink(ansi.Truncate(strings.Join(summary, " · "), w, "…"), t.Color.Muted, false))
	}
	node := c.activeNode()
	hints := []string{tr("journey.hint.move")}
	if node != nil && node.Body != "" {
		hints = append(hints, tr("journey.hint.open"))
	}
	if node != nil {
		hints = append(hints, tr("journey.hint.edit"), tr("journey.hint.delete"))
	}
	hints = append(hints, tr("journey.hint.topBottom"), tr("journey.hint.close"))
	foot = append(foot, c.ink(ansi.Truncate(strings.Join(hints, " · "), w, "…"), t.Color.Muted, false))
	listH := max(3, h-len(head)-len(foot)-2)
	start := max(0, min(max(0, len(c.rows)-listH), c.cursor-listH/2))
	out := append(append([]string{}, head...), "")
	for i := start; i < min(len(c.rows), start+listH); i++ {
		out = append(out, c.treeLine(c.rows[i], i == c.cursor, w))
	}
	out = append(append(out, ""), foot...)
	if len(out) > h {
		out = out[len(out)-h:]
	}
	return strings.Join(out, "\n")
}

// Body of the memory under the cursor, wrapped and scrolled
func (c *journeyCard) itemView(w, h int) string {
	t := c.a.store.ui.Theme
	n := c.activeNode()
	if n == nil {
		c.item = false
		return c.View(w, h)
	}
	label := n.FullLabel
	if label == "" {
		label = n.Label
	}
	head := []string{c.ink(ansi.Truncate(n.Glyph+" "+label, w, "…"), c.styleInk(c.palette(), n.Style, 1), true),
		c.ink(c.rows[c.cursor].bucket.Label+" · "+n.Meta, t.Color.Muted, false), ""}
	body := []string{tr("journey.noDetail")}
	if n.Body != "" {
		body = nil
		for _, line := range strings.Split(strings.ReplaceAll(n.Body, "\r\n", "\n"), "\n") {
			body = append(body, strings.Split(ansi.Wrap(line, max(10, w-1), ""), "\n")...)
		}
	}
	var foot []string
	if status := c.status(); status != "" {
		foot = append(foot, status)
	}
	foot = append(foot, c.ink(ansi.Truncate(tr("journey.hint.item"), w, "…"), t.Color.Muted, false))
	bodyH := max(1, h-len(head)-len(foot)-1)
	c.scroll = max(0, min(c.scroll, len(body)-bodyH))
	out := append([]string{}, head...)
	for _, line := range body[c.scroll:min(len(body), c.scroll+bodyH)] {
		out = append(out, c.ink(line, t.Color.Text, false))
	}
	return strings.Join(append(append(out, ""), foot...), "\n")
}

// Next row in a direction that is not a gap, the same row at either end
func (c *journeyCard) step(from, dir int) int {
	i := from + dir
	for i >= 0 && i < len(c.rows) && c.rows[i].kind == "gap" {
		i += dir
	}
	if i < 0 || i >= len(c.rows) {
		return from
	}
	return i
}

// Nearest real row to an index
func (c *journeyCard) snap(i int) int {
	i = max(0, min(len(c.rows)-1, i))
	if i < 0 || c.rows[i].kind != "gap" {
		return max(i, 0)
	}
	if next := c.step(i, 1); next != i {
		return next
	}
	return c.step(i, -1)
}

// Key names of the palette actions the window manager fires at an overlay in place of the keys
var journeyActionKeys = map[string]string{"palette.pick": "enter", "palette.up": "up", "palette.down": "down",
	"palette.page.up": "pgup", "palette.page.down": "pgdown", "palette.left": "left", "palette.right": "right",
	"palette.home": "g", "palette.end": "G", "palette.clear": "ctrl+u", "tui.journey.back": "esc"}

// Keymap context of the journey window, its Esc stepping back from a memory before it closes the window
const journeyContext = "tui.journey"

// Back key of the journey window, registered with the keymap of the app
func registerJourneyKeys(km *keys.Map) {
	if keys.ContextDocs[journeyContext] == "" {
		if err := keys.AddContext(journeyContext, "learning journey window"); err != nil {
			panic(err)
		}
	}
	km.Register(keys.Action{ID: "tui.journey.back", Context: journeyContext, Label: "back", Doc: "step back from a memory to the timeline, or close the journey", Default: []string{"esc"}})
}

func (c *journeyCard) Contexts() []string { return []string{journeyContext} }

func (c *journeyCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	if c.busy {
		return c, nil
	}
	k := ""
	switch m := m.(type) {
	case tea.KeyPressMsg:
		k = m.String()
	case wm.ActionMsg:
		k = journeyActionKeys[m.ID]
	case tea.MouseWheelMsg:
		k = "down"
		if m.Button == tea.MouseWheelUp {
			k = "up"
		}
	}
	if k == "" {
		return c, nil
	}
	if c.confirm {
		c.confirm = false
		if k == "y" || k == "Y" || k == "shift+y" {
			return c, c.deleteActive()
		}
		return c, nil
	}
	if k == "q" || k == "ctrl+c" || (!c.item && (k == "esc" || k == "left" || k == "h")) {
		c.gen++
		return c, wm.RequestDismissOf(c)
	}
	if !c.loaded || c.data == nil {
		return c, nil
	}
	_, h := c.a.wm.Size()
	page := max(4, h-12)
	node := c.activeNode()
	if node != nil && k == "d" {
		c.notice, c.confirm = "", true
		return c, nil
	}
	if node != nil && k == "e" {
		c.notice = ""
		return c, c.editActive()
	}
	if c.item {
		switch k {
		case "esc", "left", "h":
			c.item = false
		case "up", "k":
			c.scroll = max(0, c.scroll-2)
		case "down", "j":
			c.scroll += 2
		case "pgup", "ctrl+u":
			c.scroll = max(0, c.scroll-page)
		case "pgdown", "ctrl+d", "space":
			c.scroll += page
		case "g":
			c.scroll = 0
		case "G", "shift+g":
			c.scroll = math.MaxInt / 2
		}
		return c, nil
	}
	switch k {
	case "enter", "right", "l":
		if node != nil && node.Body != "" {
			c.item, c.scroll = true, 0
		}
	case "up", "k":
		c.cursor = c.step(c.cursor, -1)
	case "down", "j":
		c.cursor = c.step(c.cursor, 1)
	case "pgup", "ctrl+u":
		c.cursor = c.snap(c.cursor - page)
	case "pgdown", "ctrl+d":
		c.cursor = c.snap(c.cursor + page)
	case "g":
		c.cursor = c.snap(0)
	case "G", "shift+g":
		c.cursor = max(0, len(c.rows)-1)
	}
	return c, nil
}

// Deletes the node under the cursor and reloads the timeline when it went
func (c *journeyCard) deleteActive() tea.Cmd {
	n := c.activeNode()
	if n == nil {
		return nil
	}
	c.busy = true
	return call(c.a.gw, tuigateway.LearningDelete, tuigateway.LearningNodeParams{ID: n.ID}, c.mutated)
}

// Takes a delete or edit answer, its message as the notice and the timeline reloaded when it went through
func (c *journeyCard) mutated(res *tuigateway.LearningMutationResult, err error) tea.Cmd {
	c.busy = false
	if err != nil {
		c.notice = rpcErrorMessage(err)
		return nil
	}
	c.notice = res.Message
	if res.Ok {
		return c.refetch()
	}
	return nil
}

// Edits the node under the cursor in the external editor and reloads the timeline after a change
func (c *journeyCard) editActive() tea.Cmd {
	n := c.activeNode()
	if n == nil {
		return nil
	}
	c.busy = true
	id := n.ID
	return call(c.a.gw, tuigateway.LearningDetail, tuigateway.LearningNodeParams{ID: id}, func(detail *tuigateway.LearningDetailResult, err error) tea.Cmd {
		if err != nil || !detail.Ok || detail.Content == nil {
			c.busy = false
			c.notice = tr("journey.notice.cannotEdit")
			if err != nil {
				c.notice = rpcErrorMessage(err)
			} else if detail.Message != "" {
				c.notice = detail.Message
			}
			return nil
		}
		content := *detail.Content
		return editText(content, journeySuffix(detail), func(edited string, runErr, fileErr error) tea.Msg {
			return asyncDoneMsg{run: func() tea.Cmd {
				if fileErr != nil || runErr != nil || strings.TrimSpace(edited) == strings.TrimSpace(content) {
					c.busy = false
					c.notice = tr("journey.notice.noChanges")
					if fileErr != nil {
						c.notice = fileErr.Error()
					}
					return nil
				}
				return call(c.a.gw, tuigateway.LearningEdit, tuigateway.LearningEditParams{ID: id, Content: edited}, c.mutated)
			}}
		})
	})
}

// File suffix of a node's editor buffer, markdown for a skill
func journeySuffix(detail *tuigateway.LearningDetailResult) string {
	if detail.Kind == "skill" {
		return ".md"
	}
	return ".txt"
}

// Parsed /journey run, the show options or a subcommand with its node, and the parser output when it refused
type journeyArgs struct {
	action, node string
	yes, json    bool
	opts         journeyOptions
	output       string
}

// Usage lines of the /journey parser and of its subcommands
const (
	journeyUsage = "usage: /journey [--reveal 0..1] [--play] [--fps FPS] [--width WIDTH]\n" +
		"                [--height HEIGHT] [--no-color] [--json]\n" +
		"                {list,delete,edit} ..."
	journeyListUsage   = "usage: /journey list [-h] [--no-color]"
	journeyDeleteUsage = "usage: /journey delete [-h] [-y] node"
	journeyEditUsage   = "usage: /journey edit [-h] node"
	journeyNodeHelp    = "  node        Node id (skill name, or a memory id exactly as `journey list`\n              prints it)."
)

// Help text of each /journey subcommand
var journeySubHelp = map[string]string{
	"list": journeyListUsage + "\n\noptions:\n  -h, --help  show this help message and exit\n  --no-color",
	"delete": journeyDeleteUsage + "\n\npositional arguments:\n" + journeyNodeHelp +
		"\n\noptions:\n  -h, --help  show this help message and exit\n  -y, --yes   Skip the confirmation prompt.",
	"edit": journeyEditUsage + "\n\npositional arguments:\n" + journeyNodeHelp + "\n\noptions:\n  -h, --help  show this help message and exit",
}

// Refusal of a /journey line under the usage of the parser that refused it
func journeyRefusal(usage, prog, message string) string {
	return usage + "\n" + prog + ": error: " + message
}

// Value of an option given inline or as the next word, the index moved past it
func journeyOptionValue(words []string, i int, inline string, hasInline bool) (string, int, bool) {
	if hasInline {
		return inline, i, true
	}
	if i+1 >= len(words) {
		return "", i, false
	}
	return words[i+1], i + 1, true
}

// Arguments of a /journey line, the show options before an optional list, delete or edit subcommand
func parseJourneyArgs(arg string) journeyArgs {
	parsed := journeyArgs{opts: journeyOptions{reveal: 1, fps: journeyPlayFPS, color: true}}
	words, err := cards.ShellWords(arg)
	if err != nil {
		parsed.output = err.Error()
		return parsed
	}
	refuse := func(message string) journeyArgs {
		parsed.output = journeyRefusal(journeyUsage, "/journey", message)
		return parsed
	}
	var unknown []string
	i := 0
	for ; i < len(words); i++ {
		word := words[i]
		name, inline, hasInline := strings.Cut(word, "=")
		if !strings.HasPrefix(word, "--") {
			name, hasInline = word, false
		}
		switch name {
		case "--play":
			parsed.opts.play = true
		case "--no-color":
			parsed.opts.color = false
		case "--force-color":
			parsed.opts.color = true
		case "--json":
			parsed.json = true
		case "--reveal":
			value, at, given := journeyOptionValue(words, i, inline, hasInline)
			if !given {
				return refuse("argument --reveal: expected one argument")
			}
			f, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return refuse("argument --reveal: invalid float value: '" + value + "'")
			}
			if f != 0 {
				parsed.opts.reveal = max(0, min(1, f))
			}
			i = at
		case "--fps", "--width", "--height":
			value, at, given := journeyOptionValue(words, i, inline, hasInline)
			if !given {
				return refuse("argument " + name + ": expected one argument")
			}
			n, err := strconv.Atoi(value)
			if err != nil {
				return refuse("argument " + name + ": invalid int value: '" + value + "'")
			}
			into := map[string]*int{"--fps": &parsed.opts.fps, "--width": &parsed.opts.width, "--height": &parsed.opts.height}[name]
			*into = n
			i = at
		default:
			if strings.HasPrefix(word, "-") && word != "-" {
				unknown = append(unknown, word)
				continue
			}
			if word != "list" && word != "delete" && word != "edit" {
				return refuse("argument journey_action: invalid choice: '" + word + "' (choose from 'list', 'delete', 'edit')")
			}
			parsed.action = word
		}
		if parsed.action != "" {
			i++
			break
		}
	}
	if parsed.action != "" {
		usage, prog := map[string]string{"list": journeyListUsage, "delete": journeyDeleteUsage, "edit": journeyEditUsage}[parsed.action], "/journey "+parsed.action
		var positional []string
		for _, word := range words[i:] {
			switch {
			case word == "-h" || word == "--help":
				parsed.output = journeySubHelp[parsed.action]
				return parsed
			case parsed.action == "list" && (word == "--no-color" || word == "--force-color"):
				parsed.opts.color = word == "--force-color"
			case parsed.action == "delete" && (word == "-y" || word == "--yes"):
				parsed.yes = true
			case strings.HasPrefix(word, "-") && word != "-":
				unknown = append(unknown, word)
			default:
				positional = append(positional, word)
			}
		}
		if parsed.action != "list" {
			if len(positional) == 0 {
				parsed.output = journeyRefusal(usage, prog, "the following arguments are required: node")
				return parsed
			}
			parsed.node, positional = positional[0], positional[1:]
		}
		unknown = append(unknown, positional...)
	}
	if len(unknown) > 0 {
		return refuse("unrecognized arguments: " + strings.Join(unknown, " "))
	}
	return parsed
}

// Glyphs of a learned skill and a memory entry in the journey list
const (
	journeySkillGlyph  = "\u25cf"
	journeyMemoryGlyph = "\u25c6"
)

// Day, month and year of a Unix timestamp in UTC, unknown for none
func journeyDate(ts float64, present bool) string {
	if !present || ts == 0 || math.IsNaN(ts) || math.IsInf(ts, 0) {
		return "unknown"
	}
	t := time.Unix(0, 0).UTC().Add(time.Duration(ts * float64(time.Second)))
	return fmt.Sprintf("%d %s", t.Day(), t.Format("Jan 2006"))
}

// Nodes of a learning graph oldest first with id, glyph, label and date, as journey list prints them
func journeyListText(graph *tuigateway.LearningGraphResult) string {
	nodes := append([]tuigateway.LearningGraphNode(nil), graph.Nodes...)
	if len(nodes) == 0 {
		return "No learning yet."
	}
	sort.SliceStable(nodes, func(i, j int) bool { return val(nodes[i].Timestamp) < val(nodes[j].Timestamp) })
	lines := make([]string, 0, len(nodes))
	for _, n := range nodes {
		glyph := journeySkillGlyph
		if n.Kind == "memory" {
			glyph = journeyMemoryGlyph
		}
		lines = append(lines, fmt.Sprintf("%s  %s %s  %s", n.ID, glyph, n.Label, journeyDate(val(n.Timestamp), n.Timestamp != nil)))
	}
	return strings.Join(lines, "\n")
}

// Graph payload as indented JSON, the text journey --json prints
func journeyJSONText(graph *tuigateway.LearningGraphResult) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(tuigateway.Plain(graph)); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// Text of an edited file without a leading byte order mark, refused when it is not UTF-8
func decodeUTF8Sig(saved string) (string, error) {
	text := strings.TrimPrefix(saved, "\ufeff")
	if !utf8.ValidString(text) {
		return "", errors.New("the edited file is not valid UTF-8")
	}
	return text, nil
}

// Shows what a learning.graph answer renders to, or the journey failure line
func (a *app) withJourneyGraph(render func(*tuigateway.LearningGraphResult) (string, error)) tea.Cmd {
	return call(a.gw, tuigateway.LearningGraph, tuigateway.LearningGraphParams{}, func(graph *tuigateway.LearningGraphResult, err error) tea.Cmd {
		text := ""
		if err == nil {
			text, err = render(graph)
		}
		if err != nil {
			a.sys(commandText(a.ctx, "journey.failed", map[string]any{"error": rpcErrorMessage(err)}))
			return nil
		}
		return a.showOutput(text, "journey")
	})
}

// Node detail of an id from the gateway, its failure printed and then skipped
func (a *app) journeyDetail(r *slashRun, nodeID string, then func(detail *tuigateway.LearningDetailResult) tea.Cmd) tea.Cmd {
	return call(a.gw, tuigateway.LearningDetail, tuigateway.LearningNodeParams{ID: nodeID}, func(detail *tuigateway.LearningDetailResult, err error) tea.Cmd {
		switch {
		case err != nil:
			r.sys(commandText(a.ctx, "journey.failed", map[string]any{"error": rpcErrorMessage(err)}))
			return nil
		case !detail.Ok:
			message := detail.Message
			if message == "" {
				message = "not found"
			}
			r.sys(message)
			return nil
		}
		return then(detail)
	})
}

// Message of a learning mutation answer, the failure line when the call failed
func (a *app) journeyMutation(r *slashRun) func(*tuigateway.LearningMutationResult, error) tea.Cmd {
	return func(res *tuigateway.LearningMutationResult, err error) tea.Cmd {
		if err != nil {
			r.sys(commandText(a.ctx, "journey.failed", map[string]any{"error": rpcErrorMessage(err)}))
			return nil
		}
		r.sys(res.Message)
		return nil
	}
}

// Deletes a node by id after the confirm dialog unless yes was given
func (a *app) journeyDelete(r *slashRun, nodeID string, yes bool) tea.Cmd {
	return a.journeyDetail(r, nodeID, func(detail *tuigateway.LearningDetailResult) tea.Cmd {
		commit := func() tea.Cmd {
			return call(a.gw, tuigateway.LearningDelete, tuigateway.LearningNodeParams{ID: nodeID}, a.journeyMutation(r))
		}
		if yes {
			return commit()
		}
		label := detail.Label
		a.store.overlay.Confirm = &confirmReq{CancelLabel: "Cancel", ConfirmLabel: "Delete", Danger: true, Title: "Delete '" + label + "'?",
			Detail: "A skill goes to the archive the curator can restore from, a memory entry is removed.", OnConfirm: commit}
		return nil
	})
}

// Edits a node by id in the external editor and applies a changed text
func (a *app) journeyEdit(r *slashRun, nodeID string) tea.Cmd {
	return a.journeyDetail(r, nodeID, func(detail *tuigateway.LearningDetailResult) tea.Cmd {
		content := val(detail.Content)
		return editText(content, journeySuffix(detail), func(saved string, runErr, fileErr error) tea.Msg {
			return asyncDoneMsg{run: func() tea.Cmd {
				var exit *exec.ExitError
				if runErr != nil && !errors.As(runErr, &exit) {
					r.sys("editor failed: " + runErr.Error())
					return nil
				}
				edited, err := decodeUTF8Sig(saved)
				switch {
				case fileErr != nil:
					r.sys(fileErr.Error())
					return nil
				case err != nil:
					r.sys(err.Error())
					return nil
				case strings.TrimSpace(edited) == strings.TrimSpace(content):
					r.sys("no changes")
					return nil
				}
				return call(a.gw, tuigateway.LearningEdit, tuigateway.LearningEditParams{ID: nodeID, Content: edited}, a.journeyMutation(r))
			}}
		})
	})
}

// Slash command of the learning journey, the timeline window, the node list, and delete and edit by node id
func (a *app) journeyCommands() []slashCommand {
	return []slashCommand{
		{name: "journey", aliases: []string{"learning", "memory-graph"}, help: "open the learning journey timeline",
			usage: "[list|delete <id>|edit <id>]", suggest: func(string) []string { return []string{"list", "delete", "edit"} },
			run: func(r *slashRun) tea.Cmd {
				parsed := parseJourneyArgs(r.arg)
				if parsed.output != "" {
					return a.showOutput(parsed.output, "journey")
				}
				switch parsed.action {
				case "list":
					return tea.Batch(r.ran(), a.withJourneyGraph(func(graph *tuigateway.LearningGraphResult) (string, error) { return journeyListText(graph), nil }))
				case "delete":
					return tea.Batch(r.ran(), a.journeyDelete(r, parsed.node, parsed.yes))
				case "edit":
					return tea.Batch(r.ran(), a.journeyEdit(r, parsed.node))
				}
				if parsed.json {
					return tea.Batch(r.ran(), a.withJourneyGraph(journeyJSONText))
				}
				return tea.Batch(r.ran(), a.openJourney(parsed.opts))
			}},
	}
}
