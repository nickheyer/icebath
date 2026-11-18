package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap context of the billing and subscription windows
const billingContext = "tui.billing"

// How often a window a prompt took the overlay from looks whether it can show again
const sheetReturnEvery = 250 * time.Millisecond

// Registers the keys the billing and subscription windows answer before the overlay keys
func registerBillingKeys(km *keys.Map) {
	if keys.ContextDocs[billingContext] == "" {
		if err := keys.AddContext(billingContext, "billing and subscription windows"); err != nil {
			panic(err)
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.billing.back", Context: billingContext, Label: "back", Doc: "go back one screen, closing the window from its first screen", Default: []string{"esc"}},
		{ID: "tui.billing.field", Context: billingContext, Label: "switch field", Doc: "switch between the two auto-reload amounts", Default: []string{"tab", "shift+tab"}},
		{ID: "tui.billing.close", Context: billingContext, Label: "close", Doc: "close the billing or subscription window", Default: []string{"ctrl+c"}},
		{ID: "tui.billing.line.up", Context: billingContext, Label: "line up", Doc: "scroll the transcript up a line and select the row above", Default: []string{"shift+up"}, Quiet: true},
		{ID: "tui.billing.line.down", Context: billingContext, Label: "line down", Doc: "scroll the transcript down a line and select the row below", Default: []string{"shift+down"}, Quiet: true},
	} {
		km.Register(a)
	}
}

// Short names of the keymap actions a billing screen reads
var sheetKeys = map[string]string{
	"palette.up": "up", "palette.down": "down", "palette.pick": "enter", "tui.billing.back": "esc", "tui.billing.field": "tab",
	"palette.delete.back": "backspace", "palette.delete.forward": "delete", "palette.left": "left", "palette.right": "right",
	"palette.home": "home", "palette.end": "end", "palette.clear": "clear", "palette.delete.word": "word",
}

// Screens a billing window draws and the keys they answer, a short key name or typed text
type sheetFlow interface {
	title() string
	draw(d *sheetDraw)
	key(id, text string) tea.Cmd
}

// Amount typed into a billing window, its runes and the cursor among them
type sheetField struct {
	value  []rune
	cursor int
}

// Field holding the text with the cursor after it
func newSheetField(text string) sheetField {
	r := []rune(text)
	return sheetField{value: r, cursor: len(r)}
}

func (f *sheetField) text() string { return string(f.value) }

// Inserts typed text or applies an editing key at the cursor
func (f *sheetField) edit(id, text string) {
	at := min(max(f.cursor, 0), len(f.value))
	switch id {
	case "":
		typed := []rune(strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(text))
		f.value = append(append(append([]rune{}, f.value[:at]...), typed...), f.value[at:]...)
		f.cursor = at + len(typed)
	case "backspace":
		if at > 0 {
			f.value, f.cursor = append(append([]rune{}, f.value[:at-1]...), f.value[at:]...), at-1
		}
	case "delete":
		if at < len(f.value) {
			f.value = append(append([]rune{}, f.value[:at]...), f.value[at+1:]...)
		}
	case "left":
		f.cursor = max(at-1, 0)
	case "right":
		f.cursor = min(at+1, len(f.value))
	case "home":
		f.cursor = 0
	case "end":
		f.cursor = len(f.value)
	case "clear":
		f.value, f.cursor = nil, 0
	case "word":
		i := at
		for i > 0 && f.value[i-1] == ' ' {
			i--
		}
		for i > 0 && f.value[i-1] != ' ' {
			i--
		}
		f.value, f.cursor = append(append([]rune{}, f.value[:i]...), f.value[at:]...), i
	}
}

// One drawn line of a billing window, a row or field it holds and what a click on it runs
type sheetLine struct {
	text    string
	click   func() tea.Cmd
	row     bool
	held    bool
	field   *sheetField
	lead    int
	focused bool
}

// Lines of the screen a billing window shows, styled in the session theme
type sheetDraw struct {
	c     themeColors
	lines []sheetLine
}

// Text line in a tone, bold when asked
func (d *sheetDraw) line(text, tone string, bold bool) {
	d.lines = append(d.lines, sheetLine{text: toneStyle(tone).Bold(bold).Render(text)})
}

// Line already styled in several tones
func (d *sheetDraw) styled(text string) { d.lines = append(d.lines, sheetLine{text: text}) }

func (d *sheetDraw) blank() { d.lines = append(d.lines, sheetLine{}) }

// Muted hint line closing a screen, kept in view with the rows
func (d *sheetDraw) footer(text string) {
	d.lines = append(d.lines, sheetLine{text: toneStyle(d.c.Muted).Render(text), held: true})
}

// Row the arrows select, accent marked and bold in its tone when active and muted otherwise
func (d *sheetDraw) row(label, tone string, active bool, run func() tea.Cmd) {
	text := " " + toneStyle(d.c.Muted).Render(label)
	if active {
		if tone == "" {
			tone = d.c.Text
		}
		text = toneStyle(d.c.Accent).Bold(true).Render("▸") + toneStyle(tone).Bold(true).Render(label)
	}
	d.lines = append(d.lines, sheetLine{text: text, click: run, row: true, held: true})
}

// Numbered menu rows, number keys picking them and a click running the one clicked
func (d *sheetDraw) menu(labels []string, sel int, run func(i int) tea.Cmd) {
	for i, label := range labels {
		d.row(fmt.Sprintf(" %d %s", i+1, label), "", i == sel, func() tea.Cmd { return run(i) })
	}
}

// Unnumbered action row in its own tone when active
func (d *sheetDraw) action(label, tone string, active bool, run func() tea.Cmd) {
	d.row(" "+label, tone, active, run)
}

// Dollar amount field after its label, the terminal cursor placed in it while focused
func (d *sheetDraw) field(label string, f *sheetField, focused bool, click func() tea.Cmd) {
	tone := d.c.Muted
	if focused {
		tone = d.c.Label
	}
	lead := label + " $ "
	text := toneStyle(tone).Render(label+" ") + toneStyle(d.c.Label).Render("$ ")
	d.lines = append(d.lines, sheetLine{text: text, click: click, held: true, field: f, lead: ansi.StringWidth(lead), focused: focused})
}

// Cells of a usage bar
const usageBarCells = 10

// Filled and empty cells of a bar at a ratio clamped to zero and one, rounded half up
func usageBar(ratio float64) string {
	filled := int(math.Floor(max(0, min(1, ratio))*usageBarCells + 0.5))
	return strings.Repeat("█", filled) + strings.Repeat("░", usageBarCells-filled)
}

// Text cut to a width and padded with spaces to it
func padCut(text string, width int) string {
	text = ansi.Truncate(text, width, "")
	return text + strings.Repeat(" ", max(width-ansi.StringWidth(text), 0))
}

// Plan and top up bars of the dollar usage model, the plan named in eight columns and nothing when unavailable
func (d *sheetDraw) usageBars(raw any) {
	model, _ := raw.(map[string]any)
	if !truthyJS(model["available"]) {
		return
	}
	topupLabel := tr("billing.bars.topupLabel")
	width := max(8, ansi.StringWidth(topupLabel))
	bar := func(label, cells, tone, tail string) {
		d.styled(toneStyle(d.c.Text).Render(padCut(label, width)) + toneStyle(d.c.Muted).Render("[") + toneStyle(tone).Render(cells) +
			toneStyle(d.c.Muted).Render("]") + toneStyle(d.c.Text).Render("  "+tail))
	}
	if plan, _ := model["plan_bar"].(map[string]any); plan != nil {
		name := firstText(tr("billing.bars.planFallback"), model["plan_name"])
		tail := tr("billing.bars.planDetail", jsTemplate(plan, "remaining_display"), jsTemplate(plan, "total_display"))
		if plan["pct_used"] != nil {
			tail += tr("billing.bars.pctUsed", jsValueString(plan["pct_used"]))
		}
		fill, _ := plan["fill_fraction"].(float64)
		bar(padCut(name, 8), usageBar(fill), d.c.Accent, tail)
	}
	if topup, _ := model["topup_bar"].(map[string]any); topup != nil {
		bar(topupLabel, usageBar(1), d.c.Ok, tr("billing.bars.topupDetail", jsTemplate(topup, "remaining_display")))
	}
}

// Org line of a billing state, the role after it when the state names one
func orgLine(s map[string]any, key string) string {
	text := tr(key, jsValueString(s["org_name"]))
	if truthyJS(s["role"]) {
		text += " · " + jsValueString(s["role"])
	}
	return text
}

// Value of a key as text unless it is null or missing, the fallback then
func nullishText(m map[string]any, key, fallback string) string {
	if v := m[key]; v != nil {
		return jsValueString(v)
	}
	return fallback
}

// Element of a JSON list, nil past its end or for anything that is not a list
func listAt(v any, i int) any {
	list, _ := v.([]any)
	if i >= 0 && i < len(list) {
		return list[i]
	}
	return nil
}

// Integer at the start of typed text as parseInt reads it, zero when there is none
func leadingInt(text string) int {
	s := strings.TrimLeftFunc(text, isJSSpace)
	sign := 1
	if strings.HasPrefix(s, "-") {
		sign, s = -1, s[1:]
	} else {
		s = strings.TrimPrefix(s, "+")
	}
	n, digits := 0, 0
	for _, r := range s {
		if r < '0' || r > '9' || digits > 6 {
			break
		}
		n, digits = n*10+int(r-'0'), digits+1
	}
	return sign * n
}

// Arrows, enter and number keys over menu rows the way the reference menu reads them, escape running back
func menuKey(sel *int, count int, id, text string, run func(i int) tea.Cmd, back func() tea.Cmd) tea.Cmd {
	switch id {
	case "esc":
		return back()
	case "up":
		if *sel > 0 {
			*sel--
		}
	case "down":
		if *sel < count-1 {
			*sel++
		}
	case "enter":
		if *sel >= 0 && *sel < count {
			return run(*sel)
		}
	case "":
		if n := leadingInt(text); n >= 1 && n <= count {
			return run(n - 1)
		}
	}
	return nil
}

// Selected row clamped to the rows there are
func shownRow(sel, count int) int { return min(sel, max(0, count-1)) }

// Number grouped by thousands with at most three decimals rounded half away from zero, as en-US renders it
func enUSNumber(n float64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	whole, frac, _ := strings.Cut(strconv.FormatFloat(n, 'f', -1, 64), ".")
	if len(frac) > 3 {
		up := frac[3] >= '5'
		digits := []byte(whole + frac[:3])
		for i := len(digits) - 1; up && i >= 0; i-- {
			if digits[i] == '9' {
				digits[i] = '0'
				continue
			}
			digits[i]++
			up = false
		}
		if up {
			digits = append([]byte{'1'}, digits...)
		}
		whole, frac = string(digits[:len(digits)-3]), string(digits[len(digits)-3:])
	}
	frac = strings.TrimRight(frac, "0")
	value, _ := strconv.ParseFloat(whole, 64)
	text := sign + groupDigits(value)
	if frac != "" {
		text += "." + frac
	}
	return text
}

// Window over the session a billing or subscription flow draws its screens in
type sheetCard struct {
	app    *app
	key    string
	flow   sheetFlow
	closed bool
	clicks []func() tea.Cmd
	cursor *tea.Cursor
}

var _ interface {
	wm.Card
	wm.Keyed
	wm.Glypher
	wm.Contexter
	wm.Sized
	wm.Cursorer
} = (*sheetCard)(nil)

func newSheetCard(a *app, key string, flow sheetFlow) *sheetCard {
	return &sheetCard{app: a, key: key, flow: flow}
}

func (c *sheetCard) Title() string       { return c.flow.title() }
func (c *sheetCard) Key() string         { return c.key }
func (c *sheetCard) Init() tea.Cmd       { return nil }
func (c *sheetCard) Glyph() string       { return c.app.uiTheme().Glyphs.Metrics }
func (c *sheetCard) Contexts() []string  { return []string{billingContext} }
func (c *sheetCard) Cursor() *tea.Cursor { return c.cursor }

// Width of a dialog at the stage width and the rows the screen needs at it
func (c *sheetCard) MinSize() (int, int) {
	w, _ := c.app.wm.Size()
	width := min(dialogWidth, max(w-8, 30))
	rows, _, _, _ := c.render(width)
	return width, len(rows)
}

// Screen rows at a width, the click action of each, where held rows start and the field cursor
func (c *sheetCard) render(width int) ([]string, []func() tea.Cmd, int, *tea.Cursor) {
	d := &sheetDraw{c: c.app.store.ui.Theme.Color}
	c.flow.draw(d)
	var rows []string
	var clicks []func() tea.Cmd
	var cursor *tea.Cursor
	held := -1
	for _, l := range d.lines {
		if l.held && held < 0 {
			held = len(rows)
		}
		switch {
		case l.row:
			rows, clicks = append(rows, ansi.Truncate(l.text, width, "…")), append(clicks, l.click)
		case l.field != nil:
			value := string(l.field.value)
			room := max(width-2-l.lead, 4)
			cut := max(ansi.StringWidth(value)-room, 0)
			if cut > 0 {
				value = ansi.TruncateLeft(value, cut, "")
			}
			if l.focused {
				at := min(max(l.field.cursor, 0), len(l.field.value))
				cursor = tea.NewCursor(1+l.lead+max(ansi.StringWidth(string(l.field.value[:at]))-cut, 0), len(rows))
			}
			rows, clicks = append(rows, " "+l.text+toneStyle(d.c.Text).Render(value)), append(clicks, l.click)
		default:
			for _, part := range strings.Split(ansi.Wrap(l.text, max(width-2, 8), ""), "\n") {
				rows, clicks = append(rows, " "+part), append(clicks, l.click)
			}
		}
	}
	if held < 0 {
		held = len(rows)
	}
	return rows, clicks, held, cursor
}

// Screen at the size, lines above the rows cut from the bottom when rows and footer need the room
func (c *sheetCard) View(w, h int) string {
	rows, clicks, held, cursor := c.render(w)
	if len(rows) > h {
		end := min(max(h-(len(rows)-held), 1), held)
		rows = append(append([]string{}, rows[:end]...), rows[held:]...)
		clicks = append(append([]func() tea.Cmd{}, clicks[:end]...), clicks[held:]...)
		if cursor != nil {
			cursor.Y -= held - end
		}
	}
	if cursor != nil && cursor.Y >= h {
		cursor = nil
	}
	c.clicks, c.cursor = clicks[:min(len(clicks), h)], cursor
	return strings.Join(rows[:min(len(rows), h)], "\n")
}

func (c *sheetCard) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch m := msg.(type) {
	case wm.FocusMsg:
		if m.Focused || c.closed {
			return c, nil
		}
		if c.app.wm.Overlay() == wm.Card(c) {
			return c, c.awaitReturn()
		}
		c.closed = true
	case wm.ActionMsg:
		switch m.ID {
		case "tui.billing.close":
			return c, c.close()
		case "palette.page.up":
			return c, c.app.card.action("session.scroll.up")
		case "palette.page.down":
			return c, c.app.card.action("session.scroll.down")
		case "tui.billing.line.up":
			return c, tea.Batch(c.app.card.action("tui.scroll.line.up"), c.flow.key("up", ""))
		case "tui.billing.line.down":
			return c, tea.Batch(c.app.card.action("tui.scroll.line.down"), c.flow.key("down", ""))
		}
		if id, known := sheetKeys[m.ID]; known {
			return c, c.flow.key(id, "")
		}
	case tea.KeyPressMsg:
		if m.Text != "" {
			return c, c.flow.key("", m.Text)
		}
	case tea.PasteMsg:
		return c, c.flow.key("", strings.TrimRight(m.Content, "\r\n"))
	case tea.MouseWheelMsg:
		_, cmd := c.app.card.Update(m)
		return c, cmd
	case tea.MouseClickMsg:
		if m.Button == tea.MouseLeft && m.Y >= 0 && m.Y < len(c.clicks) && c.clicks[m.Y] != nil {
			return c, c.clicks[m.Y]()
		}
	}
	return c, nil
}

// Closes the window for good, its pending requests still printing their outcome
func (c *sheetCard) close() tea.Cmd {
	if c.closed {
		return nil
	}
	c.closed = true
	return wm.RequestDismissOf(c)
}

// Waits behind a prompt that took the overlay and shows again after it, anything else taking it closes the window
func (c *sheetCard) awaitReturn() tea.Cmd {
	return tea.Tick(sheetReturnEvery, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			if c.closed {
				return nil
			}
			top := c.app.wm.Overlay()
			switch top.(type) {
			case nil:
				return c.app.wm.ShowOverlay(c)
			case *dialogCard:
				return c.awaitReturn()
			}
			if top != wm.Card(c) {
				c.closed = true
			}
			return nil
		}}
	})
}
