package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	uifield "github.com/nickheyer/icebath/pkg/ui/field"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Widest a dialog asks to be
const dialogWidth = 84

// Keymap context of the prompt dialogs, ahead of the palette keys the manager adds to every overlay and sheet
const promptContext = "prompt"

// Wrapped rows of a command an approval shows at once, the page key bringing the rest
const commandPageRows = 10

// Rows the manager spends around the content of an overlay: the bar, the frame and its padding
const dialogChromeRows = 5

// Least cells a field keeps for its text beside its label, a longer label going on rows of its own
const minFieldRoom = 12

// Registers the keys of the prompt dialogs: declining, stepping through rows and questions and reading the transcript behind; the answer being typed
// takes the keys every text field shares
func registerPromptKeys(km *keys.Map) {
	if keys.ContextDocs[promptContext] == "" {
		if err := keys.AddContext(promptContext, "prompt dialogs"); err != nil {
			panic(err)
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.prompt.cancel", Context: promptContext, Label: "cancel", Doc: "leave the answer being typed, then decline the prompt", Default: []string{"esc"}},
		{ID: "tui.prompt.next", Context: promptContext, Label: "next", Doc: "go to the next question of a set, or the next row of a prompt", Default: []string{"tab"}, Quiet: true},
		{ID: "tui.prompt.prev", Context: promptContext, Label: "previous", Doc: "go to the previous question of a set, or the previous row of a prompt", Default: []string{"shift+tab"}, Quiet: true},
		{ID: "tui.prompt.scroll.up", Context: promptContext, Label: "line up", Doc: "scroll the transcript behind the prompt up a line", Default: []string{"shift+up"}, Quiet: true},
		{ID: "tui.prompt.scroll.down", Context: promptContext, Label: "line down", Doc: "scroll the transcript behind the prompt down a line", Default: []string{"shift+down"}, Quiet: true},
		{ID: "tui.approval.page", Context: keys.Approval, Label: "more", Doc: "show the next page of a long command, the first one after the last", Default: []string{"v", "V"}},
	} {
		km.Register(a)
	}
}

// One answer a dialog offers
type dialogChoice struct {
	label string
	value string
	// Key that picks the choice, shown in place of its number
	key string
	// Whether picking it destroys something, its label in the error tone
	danger bool
}

// Text the user types into a dialog, masked for credentials, taking several lines where an answer may run long, a hint standing in while it holds
// the focus and is empty
type dialogField struct {
	label     string
	masked    bool
	multiline bool
	hint      string
	// Text typed so far with the keys every field shares, made on first use
	in *uifield.Field
}

// Field of the text, made on first use and following the mask and line settings of the dialog field
func (f *dialogField) input() *uifield.Field {
	if f.in == nil {
		f.in = uifield.New("")
	}
	f.in.Masked, f.in.Multiline = f.masked, f.multiline
	return f.in
}

// Text typed so far
func (f *dialogField) text() string { return f.input().Value() }

// Text wrapped to rows of at most room cells
func wrapText(text string, room int) []string {
	return strings.Split(ansi.Wrap(text, max(room, 8), ""), "\n")
}

// Question set of a clarify prompt, the question in view open among the others
type clarifyBatch struct {
	req    *clarifyReq
	active int
	// Text went into the answer of an open ended question, the close key leaving that step before it declines
	typing bool
	// Answers typed into questions not yet locked, kept while moving between the questions
	drafts map[string]*uifield.Field
}

// Dialog in the frame of the session that asked, a message with choices to pick, fields to fill, or both
type dialogCard struct {
	app       *app
	kind      string
	requestID string
	// Prompt of the store the dialog shows, a newer copy of the same request replacing the dialog
	ref   any
	title string
	body  []string
	// Command shown under the body a page at a time
	command string
	page    int
	choices []dialogChoice
	sel     int
	multi   bool
	marks   map[int]bool
	fields  []dialogField
	focus   int
	danger  bool
	// Line naming the keys, under the choices and the fields
	hint func() string
	// Question set of a clarify prompt
	batch *clarifyBatch
	// Connection target the dialog asks about and the state it is in
	subject string
	// Prompt of the store: an answer or a decline leaves the card open until the store drops it
	held bool
	// First row in view when the rows outgrow the height, and the row kept in view
	top, anchor int
	// Choice and field of each row a click lands on
	choiceAt, fieldAt map[int]int
	settled           bool
	// Answer picked, the marked values of a multiple choice and the fields as typed
	onAnswer func(choice *dialogChoice, marked []string, fields []string) tea.Cmd
	onCancel func() tea.Cmd
	cursor   *tea.Cursor
}

// A question still waiting stays up through a click elsewhere and under the palette or another overlay
func (d *dialogCard) Sticky() bool { return !d.settled }

// Dialogs a person opens with a key take their keys at once; any other holds its answer keys back for a moment, so keys typed for something else
// never answer it
func (d *dialogCard) Guarded() bool { return d.kind != "settingSecret" && d.kind != "loginKey" }

func (d *dialogCard) Title() string  { return d.title }
func (d *dialogCard) Key() string    { return "dialog:" + d.kind + ":" + d.requestID }
func (d *dialogCard) Init() tea.Cmd  { return nil }
func (d *dialogCard) Glyph() string  { return d.app.uiTheme().Glyphs.Ask }
func (d *dialogCard) Closable() bool { return true }

// Wheel turned beside the dialog scrolls the card under the pointer, the transcript the prompt asks about
func (d *dialogCard) PassesWheel() bool { return true }

// Prompt keys of every dialog, then the verdict keys of an approval while a verdict is highlighted and the yes and no keys of a confirmation, the field
// keys last
func (d *dialogCard) Contexts() []string {
	switch {
	case d.kind == "approval" && d.focus < 0:
		return []string{promptContext, keys.Approval, keys.Text}
	case d.kind == "confirm":
		return []string{promptContext, keys.Confirm, keys.Text}
	}
	return []string{promptContext, keys.Text}
}

// The close key reaches the dialog itself, which steps back from the answer being typed before it declines
func (d *dialogCard) ClosesItself() bool { return true }

// Close key while an answer is typed: back to the choices, or an open ended question leaving its typing; false when there is no such step
func (d *dialogCard) back() bool {
	switch {
	case (d.kind == "approval" || d.batch != nil && len(d.choices) > 0) && d.focus >= 0:
		d.focus = -1
	case d.batch != nil && d.batch.typing:
		d.batch.typing = false
	default:
		return false
	}
	return true
}

// Rows the dialog needs at its width when shown over the stage
func (d *dialogCard) MinSize() (int, int) {
	w, _ := d.app.wm.Size()
	width := min(dialogWidth, max(w-8, 30))
	return width, len(d.rows(width))
}

// Columns and rows the dialog takes in the room of the card it shows in
func (d *dialogCard) FitIn(w, h int) (int, int) {
	width := min(dialogWidth, w)
	return width, min(len(d.rows(width)), h)
}

// Rows of the dialog: the body, the command page, the choices and the fields, then the line naming the keys
func (d *dialogCard) rows(width int) []string {
	t := d.app.store.ui.Theme
	d.cursor, d.anchor, d.choiceAt, d.fieldAt = nil, 0, map[int]int{}, map[int]int{}
	var tail []string
	if d.hint != nil {
		if text := d.hint(); text != "" {
			tail = append(tail, "")
			for _, line := range wrapText(text, width-2) {
				tail = append(tail, " "+toneStyle(t.Color.Muted).Render(line))
			}
		}
	}
	if d.batch != nil {
		return append(d.batchRows(width), tail...)
	}
	var rows []string
	tone := t.Color.Text
	if d.danger && d.kind != "confirm" {
		tone = t.Color.Warn
	}
	for _, para := range d.body {
		for _, line := range wrapText(para, width-2) {
			rows = append(rows, " "+toneStyle(tone).Render(line))
		}
	}
	options := d.optionRows(width, 0)
	rows = append(rows, d.commandRows(width, len(rows)+len(options)+len(tail)+1)...)
	if len(rows) > 0 && len(options) > 0 {
		rows = append(rows, "")
	}
	d.place(len(rows))
	return append(append(rows, options...), tail...)
}

// Page of the command in view, with the line counting its rows when the command runs over one page
func (d *dialogCard) commandRows(width, others int) []string {
	if d.command == "" {
		return nil
	}
	t := d.app.store.ui.Theme
	var lines []string
	for _, line := range strings.Split(d.command, "\n") {
		lines = append(lines, strings.Split(ansi.Hardwrap(line, max(width-2, 8), true), "\n")...)
	}
	size := commandPageRows
	if _, screen := d.app.wm.Size(); screen > 0 {
		size = min(size, max(screen-dialogChromeRows-others-1, 3))
	}
	if len(lines) <= size+1 {
		size = len(lines)
	}
	pages := (len(lines) + size - 1) / size
	start := d.page % pages * size
	end := min(start+size, len(lines))
	rows := make([]string, 0, size+1)
	for _, line := range lines[start:end] {
		rows = append(rows, " "+toneStyle(t.Color.Text).Render(line))
	}
	if pages > 1 {
		key := txt.PromptApprovalPageEnd
		if end < len(lines) {
			key = txt.PromptApprovalPageMore
		}
		rows = append(rows, " "+toneStyle(t.Color.Warn).Render(tr(key, start+1, end, len(lines))))
	}
	return rows
}

// Rows of a question set: every question with its locked answer under it, the one in view open with its choices and the line that takes another answer
func (d *dialogCard) batchRows(width int) []string {
	t := d.app.store.ui.Theme
	req := d.batch.req
	var rows []string
	for i, q := range req.Questions {
		answer, answered := req.Answers[q.QID]
		mark, style := "·", toneStyle(t.Color.Muted)
		switch {
		case answered:
			mark = "✓"
		case i == d.batch.active:
			mark = "▸"
		}
		if i == d.batch.active {
			style = toneStyle(t.Color.Text).Bold(true)
		}
		for n, line := range wrapText(q.Question, width-4) {
			head := "   "
			if n == 0 {
				head = " " + mark + " "
			}
			rows = append(rows, style.Render(head+line))
		}
		if answered {
			text, tone := tr(txt.PromptClarifySkipped), t.Color.Muted
			if answer != "" {
				text, tone = clarifyAnswerText(answer, q.MultiSelect), t.Color.Ok
			}
			for _, line := range wrapText(text, width-6) {
				rows = append(rows, "     "+toneStyle(tone).Render(line))
			}
		}
		if i == d.batch.active {
			options := d.optionRows(width, 2)
			d.place(len(rows))
			rows = append(rows, options...)
		}
	}
	return rows
}

// Rows of the choices and the fields behind a margin, with the caret, the row kept in view and the rows a click lands on, all counted from the first of them
func (d *dialogCard) optionRows(width, margin int) []string {
	t := d.app.store.ui.Theme
	pad := strings.Repeat(" ", margin)
	var rows []string
	for i, c := range d.choices {
		mark := "  "
		if d.multi {
			mark = "☐ "
			if d.marks[i] {
				mark = "☑ "
			}
		}
		head := fmt.Sprintf(" %d %s", i+1, mark)
		if c.key != "" {
			head = " " + c.key + " " + mark
		}
		under := strings.Repeat(" ", ansi.StringWidth(head))
		lit := i == d.sel && d.focus < 0
		if lit {
			d.anchor = len(rows)
		}
		for n, line := range wrapText(c.label, width-margin-ansi.StringWidth(head)-2) {
			d.choiceAt[len(rows)] = i
			text := under + line
			if n == 0 {
				text = head + line
			}
			litTone := t.Color.Text
			if c.danger {
				litTone = t.Color.Error
			}
			switch {
			case lit && n == 0:
				rows = append(rows, pad+toneStyle(t.Color.Accent).Bold(true).Render("▸")+toneStyle(litTone).Bold(true).Render(text))
			case lit:
				rows = append(rows, pad+" "+toneStyle(litTone).Bold(true).Render(text))
			default:
				rows = append(rows, pad+" "+toneStyle(t.Color.Muted).Render(text))
			}
		}
	}
	for i := range d.fields {
		rows = append(rows, d.fieldRows(i, width, margin, len(rows))...)
	}
	return rows
}

// Rows of a field from row at on, its label then the text wrapped beside it, a label too long for the width on rows of its own, the caret placed
// when the field holds the focus
func (d *dialogCard) fieldRows(i, width, margin, at int) []string {
	t := d.app.store.ui.Theme
	f := &d.fields[i]
	focused := i == d.focus
	pad := strings.Repeat(" ", margin)
	mark, tone := " ", t.Color.Muted
	if focused {
		mark, tone = toneStyle(t.Color.Accent).Bold(true).Render("▸"), t.Color.Accent
	}
	label := " " + f.label + " "
	indent := margin + 1 + ansi.StringWidth(label)
	var rows []string
	if width-indent-1 < minFieldRoom {
		for n, line := range wrapText(f.label, width-margin-3) {
			head := pad + "  "
			if n == 0 {
				head = pad + mark + " "
			}
			d.fieldAt[at+len(rows)] = i
			rows = append(rows, head+toneStyle(tone).Render(line))
		}
		label, indent = "", margin+3
	}
	lines, cx, cy := f.input().Wrapped(max(width-indent-1, 4))
	text := t.Color.Text
	if focused {
		d.cursor = tea.NewCursor(indent+cx, at+len(rows)+cy)
		d.anchor = at + len(rows) + cy
		if f.input().Empty() && f.hint != "" {
			lines, text = wrapText(f.hint, width-indent-1), t.Color.Muted
		}
	}
	for n, line := range lines {
		head := strings.Repeat(" ", indent)
		if n == 0 && label != "" {
			head = pad + mark + toneStyle(tone).Render(label)
		}
		d.fieldAt[at+len(rows)] = i
		rows = append(rows, head+toneStyle(text).Render(line))
	}
	return rows
}

// Moves the caret, the row kept in view and the rows a click lands on down to where the choices and fields start
func (d *dialogCard) place(base int) {
	if d.cursor != nil {
		d.cursor.Y += base
	}
	d.anchor += base
	choices, fields := make(map[int]int, len(d.choiceAt)), make(map[int]int, len(d.fieldAt))
	for row, i := range d.choiceAt {
		choices[row+base] = i
	}
	for row, i := range d.fieldAt {
		fields[row+base] = i
	}
	d.choiceAt, d.fieldAt = choices, fields
}

func (d *dialogCard) View(w, h int) string {
	rows := d.rows(w)
	d.top = min(max(min(d.top, d.anchor), d.anchor-h+1), max(len(rows)-h, 0))
	d.top = max(d.top, 0)
	rows = rows[d.top:min(d.top+h, len(rows))]
	if d.cursor != nil {
		if d.cursor.Y -= d.top; d.cursor.Y < 0 || d.cursor.Y >= len(rows) {
			d.cursor = nil
		}
	}
	return strings.Join(rows, "\n")
}

func (d *dialogCard) Cursor() *tea.Cursor { return d.cursor }

// Legend of the dialog keys
func (d *dialogCard) Hints() []wm.Hint {
	switch {
	case d.kind == "approval" && d.focus >= 0:
		return []wm.Hint{{Action: "palette.pick", Label: tr(txt.HintsLabelDenyAndSend)}, {Action: "tui.prompt.cancel", Label: tr(txt.HintsLabelBack)}}
	case d.kind == "approval":
		return []wm.Hint{{Action: "approval.allow"}, {Action: "approval.deny"}, {Action: "palette.pick", Label: tr(txt.HintsLabelChoose)}}
	case d.batch != nil && len(d.batch.req.Questions) > 1:
		return []wm.Hint{{Action: "palette.pick", Label: tr(txt.HintsLabelAnswer)}, {Action: "tui.prompt.next", Pair: "tui.prompt.prev", Label: tr(txt.HintsLabelQuestion)}, {Action: "tui.prompt.cancel", Label: tr(txt.HintsLabelCancel)}}
	case len(d.choices) == 0 && len(d.fields) == 0:
		return []wm.Hint{{Action: "tui.prompt.cancel", Label: tr(txt.HintsLabelClose)}}
	}
	return []wm.Hint{{Action: "palette.pick", Label: tr(txt.HintsLabelAnswer)}, {Action: "tui.prompt.cancel", Label: tr(txt.HintsLabelCancel)}}
}

// Moves the highlight through the choices and then the fields
func (d *dialogCard) move(step int) {
	total := len(d.choices) + len(d.fields)
	if total == 0 {
		return
	}
	at := d.sel
	if d.focus >= 0 {
		at = len(d.choices) + d.focus
	}
	at = (at + step + total) % total
	if at < len(d.choices) {
		d.sel, d.focus = at, -1
		return
	}
	d.focus = at - len(d.choices)
}

// Field holding the focus, nil while a choice is highlighted
func (d *dialogCard) field() *dialogField {
	if d.focus < 0 || d.focus >= len(d.fields) {
		return nil
	}
	return &d.fields[d.focus]
}

// Notes that text went into the answer of a question set
func (d *dialogCard) wrote() {
	if d.batch != nil {
		d.batch.typing = true
	}
}

// Answers with the highlighted choice or the typed fields, a prompt of the store staying open until the store drops it
func (d *dialogCard) answer() tea.Cmd {
	if d.settled {
		return nil
	}
	values := make([]string, len(d.fields))
	for i := range d.fields {
		values[i] = d.fields[i].text()
	}
	if d.focus >= 0 && d.focus < len(d.fields)-1 && len(d.choices) == 0 {
		d.focus++
		return nil
	}
	if d.kind == "approval" && d.focus >= 0 && trimJS(values[d.focus]) == "" {
		d.focus = -1
		return nil
	}
	var marked []string
	for i, c := range d.choices {
		if d.marks[i] {
			marked = append(marked, c.value)
		}
	}
	var choice *dialogChoice
	if d.focus < 0 && d.sel < len(d.choices) {
		choice = &d.choices[d.sel]
	}
	if d.held {
		return d.onAnswer(choice, marked, values)
	}
	d.settled = true
	return tea.Batch(d.onAnswer(choice, marked, values), wm.RequestDismissOf(d))
}

// Answers with the choice of a value, as the verdict keys of an approval do
func (d *dialogCard) pick(value string) tea.Cmd {
	for i, c := range d.choices {
		if c.value == value {
			d.sel, d.focus = i, -1
			return d.answer()
		}
	}
	return nil
}

// Declines the prompt, the card staying until the store drops it
func (d *dialogCard) cancel() tea.Cmd {
	if d.settled || d.onCancel == nil {
		return nil
	}
	return d.onCancel()
}

// Close key: the answer being typed is left first, then the prompt is declined, a prompt of the store staying in place until the store drops it
// and any other dialog closing
func (d *dialogCard) closeKey() tea.Cmd {
	if d.back() {
		return nil
	}
	declined := d.cancel()
	if d.held {
		return declined
	}
	d.settled = true
	return tea.Batch(declined, wm.RequestDismissOf(d))
}

// Carries out a keymap action: the rows, the answer, the text being typed with the keys every field shares, and the transcript behind for the
// keys that scroll
func (d *dialogCard) action(id string) tea.Cmd {
	f := d.field()
	switch id {
	case "tui.prompt.cancel", "palette.close", "confirm.no":
		return d.closeKey()
	case "tui.prompt.next", "tui.prompt.prev":
		step := 1
		if id == "tui.prompt.prev" {
			step = -1
		}
		if d.batch != nil && len(d.batch.req.Questions) > 1 {
			d.showQuestion(d.batch.active + step)
			return nil
		}
		d.move(step)
	case "palette.up", "approval.up":
		if f == nil || !f.input().Vertical(-1) {
			d.move(-1)
		}
	case "palette.down", "approval.down":
		if f == nil || !f.input().Vertical(1) {
			d.move(1)
		}
	case "palette.page.up", "approval.page.up":
		return d.app.card.action("session.scroll.up")
	case "palette.page.down", "approval.page.down":
		return d.app.card.action("session.scroll.down")
	case "tui.prompt.scroll.up":
		return d.app.card.action("tui.scroll.line.up")
	case "tui.prompt.scroll.down":
		return d.app.card.action("tui.scroll.line.down")
	case "approval.top":
		d.page = 0
	case "tui.approval.page":
		d.page++
	case "palette.pick":
		return d.answer()
	case "approval.allow":
		return d.pick("once")
	case "approval.deny":
		return d.pick("deny")
	case "approval.always":
		return d.pick("always")
	case "confirm.yes":
		return d.pick("yes")
	case "text.left", "text.right":
		switch {
		case f != nil:
			f.input().Action(id)
		case id == "text.left":
			d.move(-1)
		default:
			d.move(1)
		}
	default:
		if f == nil {
			return nil
		}
		before := f.text()
		cmd, _ := f.input().Action(id)
		if f.text() != before {
			d.wrote()
		}
		return cmd
	}
	return nil
}

// Text of a key the keymap left alone: typed into the field holding the focus, else a digit picks its row, a space ticks a choice and any other
// text starts the answer of the first field
func (d *dialogCard) typed(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	if f := d.field(); f != nil {
		if f.input().Insert(text) {
			d.wrote()
		}
		return nil
	}
	if text == " " && d.multi {
		d.marks[d.sel] = !d.marks[d.sel]
		return nil
	}
	numbered := d.kind == "approval" || d.batch != nil
	if n, err := strconv.Atoi(text); err == nil {
		switch {
		case n >= 1 && n <= len(d.choices) && d.multi:
			d.sel = n - 1
			d.marks[d.sel] = !d.marks[d.sel]
			return nil
		case n >= 1 && n <= len(d.choices):
			d.sel = n - 1
			return d.answer()
		case numbered && n == len(d.choices)+1 && len(d.fields) > 0:
			d.focus = 0
			return nil
		}
	}
	if d.kind != "approval" && len(d.fields) > 0 {
		d.focus = 0
		d.fields[0].input().Insert(text)
		d.wrote()
	}
	return nil
}

// Takes a message for the dialog; the window manager holds keys and clicks back while the dialog is new, and losing the focus never declines it
func (d *dialogCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	switch m := m.(type) {
	case wm.ActionMsg:
		return d, d.action(m.ID)
	case tea.PasteMsg, tea.ClipboardMsg:
		if f := d.field(); f != nil && f.input().Take(m) {
			d.wrote()
		}
	case tea.KeyPressMsg:
		return d, d.typed(m.Text)
	case tea.MouseWheelMsg:
		_, cmd := d.app.card.Update(m)
		return d, cmd
	case tea.MouseClickMsg:
		if m.Button != tea.MouseLeft {
			return d, nil
		}
		if i, on := d.choiceAt[m.Y+d.top]; on {
			d.sel, d.focus = i, -1
			if d.multi {
				d.marks[i] = !d.marks[i]
				return d, nil
			}
			return d, d.answer()
		}
		if i, on := d.fieldAt[m.Y+d.top]; on {
			d.focus = i
		}
	}
	return d, nil
}

// Opens a question of a set with its locked answer put back: a chosen answer highlights its row, the ticked ones stay ticked and a typed answer
// waits on the line it was typed into
func (d *dialogCard) showQuestion(index int) {
	req := d.batch.req
	count := len(req.Questions)
	index = (index%count + count) % count
	q := req.Questions[index]
	if d.batch.drafts == nil {
		d.batch.drafts = map[string]*uifield.Field{}
	}
	if len(d.fields) == 1 && d.batch.active < count {
		if prev := req.Questions[d.batch.active]; prev.QID != q.QID {
			d.batch.drafts[prev.QID] = d.fields[0].input()
		}
	}
	d.batch.active, d.batch.typing = index, false
	d.title = tr(txt.PromptClarifyTitle)
	if count > 1 {
		d.title = tr(txt.PromptClarifyTitleOf, index+1, count)
	}
	d.multi, d.marks, d.sel, d.focus, d.choices = q.MultiSelect, map[int]bool{}, 0, -1, nil
	for _, c := range q.Choices {
		d.choices = append(d.choices, dialogChoice{label: c, value: c})
	}
	label := tr(txt.PromptClarifyAnswerLabel) + " ›"
	if len(q.Choices) > 0 {
		label = fmt.Sprintf("%d   %s ›", len(q.Choices)+1, tr(txt.PromptClarifyOther))
	}
	typed := ""
	if answer := req.Answers[q.QID]; answer != "" {
		items, listed := clarifyAnswerItems(answer)
		switch {
		case q.MultiSelect && listed:
			var own []string
			for _, item := range items {
				if at := slices.Index(q.Choices, item); at >= 0 {
					d.marks[at] = true
				} else {
					own = append(own, item)
				}
			}
			typed = strings.Join(own, ", ")
		case slices.Contains(q.Choices, answer):
			d.sel = slices.Index(q.Choices, answer)
		default:
			typed = answer
		}
	}
	in := uifield.New(typed)
	if kept := d.batch.drafts[q.QID]; kept != nil && kept.Value() != "" {
		in = kept
	}
	d.fields = []dialogField{{label: label, multiline: true, in: in}}
	if len(q.Choices) == 0 || in.Value() != "" {
		d.focus = 0
	}
	// An answer kept from before is typing too, so the close key leaves it before it declines the question
	d.batch.typing = len(q.Choices) == 0 && in.Value() != ""
}

// Items of a multiple choice answer, false for an answer that is no list
func clarifyAnswerItems(answer string) ([]string, bool) {
	var items []string
	if json.Unmarshal([]byte(answer), &items) != nil {
		return nil, false
	}
	return items, true
}

// Line naming the keys of a question set, Enter named by whether it locks the last open answer
func (d *dialogCard) clarifyHint() string {
	open := 0
	for _, q := range d.batch.req.Questions {
		if _, answered := d.batch.req.Answers[q.QID]; !answered {
			open++
		}
	}
	enter := tr(txt.PromptClarifyLockAnswer)
	if open == 1 {
		enter = tr(txt.PromptClarifyConfirmAndContinue)
	}
	switch {
	case d.focus >= 0 && (len(d.choices) > 0 || d.batch.typing):
		return tr(txt.PromptClarifyTypingHint, enter)
	case d.multi:
		return tr(txt.PromptClarifyToggle) + " · " + tr(txt.PromptClarifyHint, enter)
	}
	return tr(txt.PromptClarifyHint, enter)
}

// Dialogs of the session prompts in the frame of the session's card, the most urgent first, and what the bar says while one waits outside it
type dialogState struct {
	// Most urgent dialog open, nil when none is
	current *dialogCard
	// Every dialog open, most urgent first
	open []*dialogCard
}

// Prompt the store holds, its kind, request and object, ordered by urgency
type wantedPrompt struct {
	kind, id string
	ref      any
}

// Kind, request and store object of every prompt that should be showing, most urgent first
func (a *app) wantedDialogs() []wantedPrompt {
	o := a.store.overlay
	var out []wantedPrompt
	add := func(kind, id string, ref any) { out = append(out, wantedPrompt{kind: kind, id: id, ref: ref}) }
	if o.Approval != nil {
		add("approval", o.Approval.RequestID, o.Approval)
	}
	if o.Sudo != nil {
		add("sudo", o.Sudo.RequestID, o.Sudo)
	}
	if o.Secret != nil {
		add("secret", o.Secret.RequestID, o.Secret)
	}
	if o.VaultUnlock != nil {
		add("vaultUnlock", o.VaultUnlock.RequestID, o.VaultUnlock)
	}
	if o.VaultSaveLogin != nil {
		add("vaultSaveLogin", o.VaultSaveLogin.RequestID, o.VaultSaveLogin)
	}
	if o.VaultCode != nil {
		add("vaultCode", o.VaultCode.RequestID, o.VaultCode)
	}
	if o.Connection != nil && a.store.connectionOp != nil && a.store.connectionOp.OpID == o.Connection.OpID {
		op := a.store.connectionOp
		key := op.OpID + ":" + strconv.FormatInt(op.Seq, 10)
		if target := a.activeConnectionTarget(); target != nil {
			key += ":" + target.Name + ":" + string(target.State) + ":" + val(target.ConnectURL)
		}
		add("connection", key, op)
	}
	if o.Clarify != nil {
		add("clarify", o.Clarify.RequestID, o.Clarify)
	}
	if o.Confirm != nil {
		add("confirm", o.Confirm.Title, o.Confirm)
	}
	return out
}

// Whether a prompt waits on the user
func (s *dialogState) waiting() bool {
	for _, d := range s.open {
		if !d.settled {
			return true
		}
	}
	return false
}

// Bar notice of a waiting prompt shown over the stage rather than in the session's card, whose sheets the window manager names itself
func (s *dialogState) notice() (string, wm.Signal, bool) {
	for _, d := range s.open {
		if !d.settled && !d.app.wm.Attached(d) {
			return d.title, wm.Blocked, true
		}
	}
	return "", wm.Idle, false
}

// Follows a locked answer of a question set: the question in view stays while it lacks an answer, else the first one still open comes into view
func (s *dialogState) clarifyLocked(req *clarifyReq) {
	for _, d := range s.open {
		if d.batch == nil || d.batch.req != req {
			continue
		}
		if _, answered := req.Answers[req.Questions[d.batch.active].QID]; !answered {
			return
		}
		for i, q := range req.Questions {
			if _, answered := req.Answers[q.QID]; !answered {
				d.showQuestion(i)
				return
			}
		}
	}
}

// Interrupt key on a prompt of this session: the connection card stops the turn that owns its operation, a waiting prompt is declined; with no prompt
// of the session in front, the overlay closes, the session behind left alone
func (a *app) interruptOverlay() tea.Cmd {
	var cur *dialogCard
	for _, d := range a.dialogs.open {
		if top := a.wm.Overlay(); top == wm.Card(d) || (top == nil && a.wm.Sheet(a.card) == wm.Card(d)) {
			cur = d
		}
	}
	if cur == nil || cur.settled {
		return a.wm.Dismiss()
	}
	if cur.kind != "connection" {
		return cur.cancel()
	}
	if a.store.ui.Sid == "" {
		return nil
	}
	return a.interruptTurn()
}

// Opens a dialog in the session's card for every prompt the store holds, keeping what was typed into a dialog its newer copy replaces, and closes
// those whose prompt settled elsewhere
func (a *app) syncDialogs() tea.Cmd {
	wanted := a.wantedDialogs()
	var cmds []tea.Cmd
	var open []*dialogCard
	byKind := map[string]*dialogCard{}
	for _, d := range a.dialogs.open {
		shown := a.wm.Attached(d) || a.wm.ShowsOverlay(d)
		if !d.settled && shown {
			byKind[d.kind] = d
			continue
		}
		if shown {
			cmds = append(cmds, a.wm.DismissCard(d))
		}
	}
	for _, w := range wanted {
		cur := byKind[w.kind]
		if cur != nil && cur.requestID == w.id && cur.ref == w.ref {
			open = append(open, cur)
			delete(byKind, w.kind)
			continue
		}
		d := a.buildDialog(w.kind, w.id, cur)
		keepTyping(d, cur)
		if cur != nil {
			cur.settled = true
			cmds = append(cmds, a.wm.DismissCard(cur))
			delete(byKind, w.kind)
		}
		if d == nil {
			continue
		}
		d.ref, d.held = w.ref, true
		open = append(open, d)
		cmds = append(cmds, a.wm.Attach(a.card, d))
	}
	for _, gone := range byKind {
		gone.settled = true
		cmds = append(cmds, a.wm.DismissCard(gone))
	}
	a.dialogs.open, a.dialogs.current = open, nil
	if len(open) > 0 {
		a.dialogs.current = open[0]
	}
	return tea.Batch(cmds...)
}

// Carries what was typed into a dialog over to the newer copy that replaces it, field by field where the labels match, with the highlight
func keepTyping(d, prev *dialogCard) {
	if d == nil || prev == nil || prev.kind != d.kind || prev.batch != nil || len(prev.fields) != len(d.fields) {
		return
	}
	for i := range d.fields {
		if kept := prev.fields[i]; kept.label == d.fields[i].label && kept.in != nil && kept.in.Value() != "" {
			d.fields[i].in = kept.in
		}
	}
	if len(prev.choices) == len(d.choices) {
		d.sel, d.focus, d.page = prev.sel, prev.focus, prev.page
	}
}

// Answers a one value server request, the status back to running while a turn runs and to ready otherwise
func (a *app) respondPrompt(key, requestID, value string) tea.Cmd {
	a.store.respondToServerRequest(requestID, tuigateway.ValueAnswer{Value: value})
	a.store.overlay.closePrompt(key)
	a.store.ui.Status = "ready"
	if a.store.ui.Busy {
		a.store.ui.Status = "running…"
	}
	return nil
}

// Dialog of one prompt kind, wired to the answer the gateway expects, prev the dialog it takes the place of
func (a *app) buildDialog(kind, id string, prev *dialogCard) *dialogCard {
	o := &a.store.overlay
	d := &dialogCard{app: a, kind: kind, requestID: id, focus: -1, marks: map[int]bool{}}
	masked := func(key, title string, body []string, label string, plain bool, declined string) *dialogCard {
		d.title, d.body, d.fields, d.focus = title, body, []dialogField{{label: label, masked: !plain}}, 0
		requestID := id
		d.onAnswer = func(_ *dialogChoice, _ []string, fields []string) tea.Cmd {
			value := fields[0]
			if plain {
				value = trimJS(value)
			}
			return a.respondPrompt(key, requestID, value)
		}
		d.onCancel = func() tea.Cmd {
			a.sys(declined)
			return a.respondPrompt(key, requestID, "")
		}
		return d
	}
	switch kind {
	case "approval":
		req := o.Approval
		d.title, d.danger, d.command = tr(txt.PromptApprovalTitle), true, req.Command
		d.body = []string{req.Description}
		if req.SmartDenied {
			d.body = append(d.body, tr(txt.PromptApprovalSmartDenied))
		}
		choices := req.Choices
		if len(choices) == 0 {
			choices = []string{"once", "session", "always", "deny"}
		}
		for _, c := range choices {
			if c == "always" && !req.AllowPermanent {
				continue
			}
			d.choices = append(d.choices, dialogChoice{label: approvalLabel(c), value: c})
		}
		// Enter on an untouched approval denies
		if deny := slices.IndexFunc(d.choices, func(c dialogChoice) bool { return c.value == "deny" }); deny >= 0 {
			d.sel = deny
			d.fields = []dialogField{{label: fmt.Sprintf("%d   %s ›", len(d.choices)+1, tr(txt.PromptApprovalTell)), hint: tr(txt.PromptApprovalTellHint), multiline: true}}
		}
		d.onAnswer = func(choice *dialogChoice, _ []string, fields []string) tea.Cmd {
			if choice == nil {
				return a.answerApprovalTell(req, trimJS(fields[0]))
			}
			return a.answerApproval(req, choice.value)
		}
		d.onCancel = func() tea.Cmd { return a.answerApproval(req, "deny") }
	case "sudo":
		req := o.Sudo
		body := []string{tr(txt.SecureSudoBody)}
		if req.Command != "" {
			body = []string{req.Command, tr(txt.SecureSudoBody)}
		}
		if req.ProfileKey != "" {
			body = []string{tr(txt.SecureSudoInstallBody, req.ProfileKey)}
		}
		return masked("sudo", tr(txt.SecureSudoTitle), body, tr(txt.SecurePasswordLabel), false, tr(txt.SessionInputSudoCancelled))
	case "secret":
		req := o.Secret
		return masked("secret", "secret "+tr(txt.SecureSecretForEnvVar, req.EnvVar), []string{req.Prompt}, req.EnvVar, false, tr(txt.SessionInputSecretCancelled))
	case "vaultUnlock":
		req := o.VaultUnlock
		name := req.DisplayName
		if name == "" {
			name = req.Backend
		}
		return masked("vaultUnlock", tr(txt.SecureVaultUnlockTitle, name), []string{tr(txt.SecureVaultUnlockBody, name)}, tr(txt.SecurePasswordLabel), false,
			tr(txt.SessionInputVaultStaysLocked, name))
	case "vaultCode":
		req := o.VaultCode
		body := []string{tr(txt.SecureVaultCodeBody, req.Site)}
		if req.Hint != "" {
			body = []string{req.Hint, tr(txt.SecureVaultCodeBody, req.Site)}
		}
		return masked("vaultCode", tr(txt.SecureVaultCodeTitle, req.Site), body, tr(txt.SecureVaultCodeLabel), true, tr(txt.SessionInputCodeSkipped, req.Site))
	case "vaultSaveLogin":
		req := o.VaultSaveLogin
		d.title = tr(txt.SecureVaultSaveTitle, req.Site)
		d.body = []string{tr(txt.SecureVaultSaveBody, req.Origin)}
		d.fields, d.focus = []dialogField{{label: tr(txt.SecureVaultUsernameLabel)}, {label: tr(txt.SecurePasswordLabel), masked: true}}, 0
		requestID := id
		d.onAnswer = func(_ *dialogChoice, _ []string, fields []string) tea.Cmd {
			value := ""
			if fields[0] != "" && fields[1] != "" {
				raw, _ := json.Marshal(map[string]string{"identifier": fields[0], "password": fields[1]})
				value = string(raw)
			}
			return a.respondPrompt("vaultSaveLogin", requestID, value)
		}
		d.onCancel = func() tea.Cmd {
			a.sys(tr(txt.SessionInputLoginNotSaved, req.Site))
			return a.respondPrompt("vaultSaveLogin", requestID, "")
		}
	case "clarify":
		req := o.Clarify
		if len(req.Questions) == 0 {
			return nil
		}
		d.batch, d.hint = &clarifyBatch{req: req}, d.clarifyHint
		first := 0
		for i, q := range req.Questions {
			if _, answered := req.Answers[q.QID]; !answered {
				first = i
				break
			}
		}
		d.showQuestion(first)
		d.onAnswer = func(choice *dialogChoice, marked []string, fields []string) tea.Cmd {
			q := req.Questions[d.batch.active]
			answer := trimJS(fields[0])
			switch {
			case q.MultiSelect:
				if answer != "" {
					marked = append(marked, answer)
				}
				if len(marked) == 0 && choice != nil {
					marked = []string{choice.value}
				}
				answer = ""
				if len(marked) > 0 {
					raw, _ := json.Marshal(marked)
					answer = string(raw)
				}
			case choice != nil:
				answer = choice.value
			}
			return a.answerClarifyQuestion(req, q, answer)
		}
		d.onCancel = func() tea.Cmd { return a.cancelClarify(req) }
	case "connection":
		return a.connectionDialog(d, prev)
	case "confirm":
		req := o.Confirm
		d.title, d.danger = req.Title, req.Danger
		if req.Detail != "" {
			d.body = []string{req.Detail}
		}
		yes, no := req.ConfirmLabel, req.CancelLabel
		if yes == "" {
			yes = tr(txt.PromptConfirmConfirm)
		}
		if no == "" {
			no = tr(txt.PromptConfirmCancel)
		}
		d.choices = []dialogChoice{{label: yes, value: "yes", key: d.app.keys.First("confirm.yes"), danger: req.Danger},
			{label: no, value: "no", key: d.app.keys.First("confirm.no")}}
		if req.Danger {
			d.sel = 1
		}
		d.onAnswer = func(choice *dialogChoice, _ []string, _ []string) tea.Cmd {
			o.Confirm = nil
			if choice != nil && choice.value == "yes" && req.OnConfirm != nil {
				return req.OnConfirm()
			}
			return nil
		}
		d.onCancel = func() tea.Cmd { o.Confirm = nil; return nil }
	}
	return d
}

// States a connection target no longer waits in
var resolvedTargetStates = map[tuigateway.ConnectionTargetState]bool{"connected": true, "skipped": true, "not_connected": true}

// Whether a connection target still waits on the user: one that is not resolved, or one that connected while its tools could not be read
func connectionWaits(target tuigateway.ConnectionOperationTarget) bool {
	if target.State == "connected" {
		return strings.TrimFunc(val(target.DiscoveryError), isJSSpace) != ""
	}
	return !resolvedTargetStates[target.State] && !val(target.Resolved)
}

// First target of the live connection operation that still waits on the user, nil once every target resolved
func (a *app) activeConnectionTarget() *tuigateway.ConnectionOperationTarget {
	op := a.store.connectionOp
	if op == nil {
		return nil
	}
	for i := range op.Targets {
		if connectionWaits(op.Targets[i]) {
			return &op.Targets[i]
		}
	}
	return nil
}

// Answer of the connection card, one target approved with its credentials or skipped, or the operation continued
func connectionAnswer(target *tuigateway.ConnectionOperationTarget, status tuigateway.ConnectionAnswerStatus, fields []string) tuigateway.ConnectionAnswer {
	if target == nil {
		return tuigateway.ConnectionAnswer{SettledBy: ptr(tuigateway.ConnectionSettleReason("continue"))}
	}
	row := tuigateway.ConnectionAnswerTarget{Name: target.Name, Status: status}
	if status == "approved" && len(target.RequiredEnv) > 0 {
		row.Env = map[string]string{}
		for i, field := range target.RequiredEnv {
			if i < len(fields) && field.Name != "" {
				row.Env[field.Name] = fields[i]
			}
		}
	}
	return tuigateway.ConnectionAnswer{Targets: []tuigateway.ConnectionAnswerTarget{row}}
}

// Owner of the live connection operation, the viewed session
func (a *app) connectionOwner() tuigateway.SessionOwner {
	return tuigateway.SessionOwner{Type: "session", SessionID: a.store.ui.Sid}
}

// Sends an answer of the connection card to the owner of the operation, a lost answer noticed
func (a *app) respondConnection(answer tuigateway.ConnectionAnswer) tea.Cmd {
	op := a.store.connectionOp
	if op == nil || a.store.ui.Sid == "" {
		return nil
	}
	params := tuigateway.ConnectionRespondParams{Owner: a.connectionOwner(), OpID: op.OpID, Result: answer}
	return ask(a.events.rpc, a.sys, tuigateway.ConnectionRespond, params, func(r *tuigateway.ConnectionRespondResult) tea.Cmd {
		if r == nil {
			return wm.Notify(tr(txt.ConnectionNoticeAnswerNotDelivered), wm.Warn)
		}
		return nil
	})
}

// Runs a failed target of the live operation again, the card following the fresh snapshot
func (a *app) retryConnection(name string) tea.Cmd {
	if a.store.connectionOp == nil || a.store.ui.Sid == "" {
		return nil
	}
	params := tuigateway.ConnectorsConnectParams{Owner: a.connectionOwner(), Connectors: []string{name}, Reconnect: true}
	return ask(a.events.rpc, a.sys, tuigateway.ConnectorsConnect, params, func(r *tuigateway.ConnectionOperationSnapshot) tea.Cmd {
		if r == nil {
			return wm.Notify(tr(txt.ConnectionNoticeRestartFailed), wm.Warn)
		}
		for _, line := range a.store.applyConnectionSnapshot(*r) {
			a.sys(line)
		}
		return nil
	})
}

// Asks the watcher of the live operation to read the account now, after the browser step
func (a *app) wakeConnection() tea.Cmd {
	op := a.store.connectionOp
	if op == nil || a.store.ui.Sid == "" {
		return nil
	}
	params := tuigateway.ConnectionOperationParams{Owner: a.connectionOwner(), OpID: op.OpID}
	return ask(a.events.rpc, a.sys, tuigateway.ConnectorsOperationWake, params, func(*tuigateway.ConnectionWakeResult) tea.Cmd { return nil })
}

// Verb of the step a target needs
func connectionVerb(action tuigateway.ConnectionTargetAction) string {
	switch action {
	case "authorize":
		return tr(txt.ConnectionVerbAuthorize)
	case "enable":
		return tr(txt.ConnectionVerbEnable)
	case "install":
		return tr(txt.ConnectionVerbInstall)
	}
	return tr(txt.ConnectionVerbConnect)
}

// Connection card, every target with its state and the step the first waiting one needs, what was typed for that target kept from the card it replaces
func (a *app) connectionDialog(d, prev *dialogCard) *dialogCard {
	op := a.store.connectionOp
	target := a.activeConnectionTarget()
	trimmed := func(s *string) string { return strings.TrimFunc(val(s), isJSSpace) }
	hint := func(key txt.Key) func() string { return func() string { return tr(key) } }
	d.title = tr(txt.ConnectionTitle)
	for _, row := range op.Targets {
		mark := "○"
		switch row.State {
		case "connected":
			mark = "●"
		case "skipped", "not_connected":
			mark = "–"
		case "initiated":
			mark = "◌"
		case "failed":
			mark = "✗"
		}
		line := mark + " " + connectionTargetName(row) + " · " + connectionStateWord(row.State)
		if detail := trimmed(row.Detail); detail != "" {
			line += " · " + detail
		}
		d.body = append(d.body, line)
	}
	if target == nil {
		opID := op.OpID
		d.body, d.hint = append(d.body, tr(txt.ConnectionStatusFinishing)), hint(txt.ConnectionHintFinishing)
		d.onAnswer = func(*dialogChoice, []string, []string) tea.Cmd { return nil }
		d.onCancel = func() tea.Cmd {
			a.store.dismissConnectionOperation(opID)
			return nil
		}
		return d
	}
	d.subject = target.Name + ":" + string(target.State)
	if target.State == "connected" {
		proceed := func() tea.Cmd { return a.respondConnection(connectionAnswer(nil, "", nil)) }
		d.body, d.hint = append(d.body, tr(txt.ConnectionAuthorizedTitle), trimmed(target.DiscoveryError)), hint(txt.ConnectionAuthorizedHint)
		d.choices = []dialogChoice{{label: tr(txt.ConnectionAuthorizedContinue), value: "continue"}}
		d.onAnswer = func(*dialogChoice, []string, []string) tea.Cmd { return proceed() }
		d.onCancel = proceed
		return d
	}
	for _, line := range []string{trimmed(target.Description), trimmed(target.Instructions), trimmed(target.Hint), trimmed(target.DiscoveryError)} {
		if line != "" {
			d.body = append(d.body, line)
		}
	}
	name, link := target.Name, trimmed(target.ConnectURL)
	skip := dialogChoice{label: tr(txt.ConnectionSelectorSkip), value: "skip"}
	switch {
	case target.State == "failed":
		if trimmed(target.Detail) == "" {
			d.body = append(d.body, tr(txt.ConnectionFailureFailed))
		}
		d.choices, d.hint = []dialogChoice{{label: tr(txt.ConnectionSelectorTryAgain), value: "retry"}, skip}, hint(txt.ConnectionHintRetry)
	case link != "":
		d.body = append(d.body, tr(txt.ConnectionFinishInBrowser, link))
		d.choices = []dialogChoice{{label: tr(txt.ConnectionSelectorOpen), value: "open"}, {label: tr(txt.ConnectionSelectorCheckNow), value: "wake"}, skip}
		d.hint = hint(txt.ConnectionHintBrowser)
	case target.State == "initiated":
		d.body = append(d.body, tr(txt.ConnectionWaiting, connectionTargetName(*target)))
		d.choices, d.hint = []dialogChoice{{label: tr(txt.ConnectionSelectorCheckNow), value: "wake"}, skip}, hint(txt.ConnectionHintWorking)
	default:
		for _, field := range target.RequiredEnv {
			label := firstText(trimmed(field.Prompt), field.Name)
			d.fields = append(d.fields, dialogField{label: label, masked: field.Secret, in: uifield.New(field.Default)})
		}
		d.choices = []dialogChoice{{label: connectionVerb(target.Action) + " " + connectionTargetName(*target), value: "approved"}, skip}
		d.hint = hint(txt.ConnectionHintForm)
	}
	if prev != nil && prev.kind == "connection" && prev.subject == d.subject && len(prev.fields) == len(d.fields) {
		for i := range d.fields {
			if kept := prev.fields[i]; kept.label == d.fields[i].label {
				d.fields[i].in = kept.in
			}
		}
		if len(prev.choices) == len(d.choices) {
			d.sel, d.focus = prev.sel, prev.focus
		}
	}
	required := target.RequiredEnv
	d.onAnswer = func(choice *dialogChoice, _ []string, fields []string) tea.Cmd {
		value := "approved"
		if choice != nil {
			value = choice.value
		}
		switch value {
		case "open":
			if !openExternalURL(link) {
				return wm.Notify(tr(txt.ConnectionNoticeBrowserDidNotOpen), wm.Warn)
			}
			return nil
		case "wake":
			return a.wakeConnection()
		case "retry":
			return a.retryConnection(name)
		case "skip":
			return a.respondConnection(connectionAnswer(target, "skipped", nil))
		}
		for i, field := range required {
			if field.Required && (i >= len(fields) || strings.TrimFunc(fields[i], isJSSpace) == "") {
				return wm.Notify(tr(txt.ConnectionFieldRequired, firstText(trimmed(field.Prompt), field.Name)), wm.Warn)
			}
		}
		return a.respondConnection(connectionAnswer(target, "approved", fields))
	}
	d.onCancel = func() tea.Cmd { return a.respondConnection(connectionAnswer(target, "skipped", nil)) }
	return d
}

// Label of an approval verdict
func approvalLabel(choice string) string {
	switch choice {
	case "once":
		return tr(txt.PromptApprovalOnce)
	case "session":
		return tr(txt.PromptApprovalSession)
	case "always":
		return tr(txt.PromptApprovalAlways)
	case "deny":
		return tr(txt.PromptApprovalDeny)
	}
	return choice
}

// Denies a held command and tells the agent what to do instead: the words are steered into the held turn first, so that they ride on the refused
// tool result, and stay as the next prompt when the turn no longer takes them; the denial follows whatever became of the words, and from the
// moment the words are on their way no verdict releases the command
func (a *app) answerApprovalTell(req *approvalReq, text string) tea.Cmd {
	if a.store.overlay.Approval != req || req.Tell != "" || req.Answering {
		return nil
	}
	sid := a.store.ui.Sid
	destination, err := a.store.captureDestination()
	if sid == "" || err != nil {
		return a.answerApproval(req, "deny")
	}
	req.Tell = tellSteering
	told := func(status tuigateway.CorrectionStatus, answered bool) tea.Cmd {
		shown := a.store.isCurrentDestination(destination)
		if answered && status == "queued" {
			if shown {
				a.sys(tr(txt.SlashCmdCoreSteerQueued, previewOf(text)))
			}
		} else {
			a.enqueue(text, text, destination)
			if shown {
				a.sys(tr(txt.SlashCmdCoreSteerRejected))
			}
		}
		req.Tell = tellTold
		return a.answerApproval(req, "deny")
	}
	generation := infoOf(a.store.ui.Info).ExecutionGeneration
	if generation == nil {
		return told("", false)
	}
	params := tuigateway.CanonicalCorrectionParams{SessionID: sid, ExecutionGeneration: *generation, Text: text}
	return call(a.gw, tuigateway.CanonicalSessionSteer, params, func(res *tuigateway.CorrectionAnswer, err error) tea.Cmd {
		return told(val(res).Status, err == nil && res != nil)
	})
}

// Sends the verdict of a dangerous command, through the shared prompt or the server request it came as, refused while the words of a denial
// that tells the agent what to do are on their way and, after them, for anything but that denial
func (a *app) answerApproval(req *approvalReq, choice string) tea.Cmd {
	if a.store.overlay.Approval != req || req.Answering || req.Tell == tellSteering || (req.Tell == tellTold && choice != "deny") {
		return nil
	}
	settle := func() {
		if a.store.overlay.Approval == req {
			a.store.overlay.Approval = nil
		}
		a.store.turn.Outcome = tr(txt.SessionApprovalApproved, choice)
		if choice == "deny" {
			a.store.turn.Outcome = tr(txt.SessionApprovalDenied)
		}
		a.store.ui.Status = "running…"
	}
	openLink := func() tea.Cmd {
		if choice == "deny" || req.OpenURL == "" || openExternalURL(req.OpenURL) {
			return nil
		}
		a.sys(req.OpenURL)
		return wm.Notify(tr(txt.McpNoticeBrowserDidNotOpen), wm.Warn)
	}
	if req.Shared != nil {
		req.Answering = true
		params := tuigateway.CanonicalApprovalParams{SessionID: req.Shared.SessionID, ExecutionGeneration: req.Shared.ExecutionGeneration, PromptID: req.Shared.PromptID, Choice: tuigateway.ApprovalChoice(choice)}
		return ask(a.events.rpc, a.sys, tuigateway.CanonicalApprovalRespond, params, func(r *tuigateway.PromptResponseResult) tea.Cmd {
			req.Answering = false
			if r != nil {
				settle()
				return openLink()
			}
			return nil
		})
	}
	a.store.respondToServerRequest(req.RequestID, tuigateway.ApprovalAnswer{Choice: tuigateway.ApprovalChoice(choice)})
	settle()
	return openLink()
}

// Takes the clarify row out of the trail of the running turn, the questions going into the transcript in its place
func (a *app) dropClarifyTrail() {
	label := toolTrailLabel("clarify")
	kept := a.turn.turnTools[:0]
	for _, line := range a.turn.turnTools {
		if !sameToolTrailGroup(label, line) {
			kept = append(kept, line)
		}
	}
	a.turn.turnTools = kept
	a.store.turn.TurnTrail = append([]string(nil), kept...)
}

// Settles a clarify batch in the transcript, one trail row and the questions with their answers
func (a *app) settleClarify(req *clarifyReq, answers map[string]string) {
	a.dropClarifyTrail()
	a.turn.persistedToolLabels[toolTrailLabel("clarify")] = true
	trail, text := clarifySettledRows(req.Questions, answers)
	a.appendMessage(msg{Kind: "trail", Role: "system", Tools: []toolRow{trail}})
	a.appendMessage(msg{Role: "user", Text: text})
	a.store.ui.Status = "running…"
	if a.store.overlay.Clarify == req {
		a.store.overlay.Clarify = nil
	}
}

// Cancels a clarify prompt whole: a shared prompt is released with an empty answer and a request is answered with no answers, the questions
// then staying in the transcript with the answers locked so far
func (a *app) cancelClarify(req *clarifyReq) tea.Cmd {
	if a.store.overlay.Clarify != req || req.Cancelling {
		return nil
	}
	a.dropClarifyTrail()
	settle := func() {
		answers := req.Answers
		if answers == nil {
			answers = map[string]string{}
		}
		a.appendMessage(msg{Role: "system", Text: formatAbandonedClarify(req.Questions, answers, tr(txt.GatewayMsgClarifyCancelled))})
		a.store.overlay.Clarify = nil
	}
	if req.Shared != nil {
		req.Cancelling = true
		params := tuigateway.CanonicalClarifyParams{SessionID: req.Shared.SessionID, ExecutionGeneration: req.Shared.ExecutionGeneration, PromptID: req.Shared.PromptID}
		return ask(a.events.rpc, a.sys, tuigateway.CanonicalClarifyRespond, params, func(r *tuigateway.PromptResponseResult) tea.Cmd {
			req.Cancelling = false
			if r != nil && a.store.overlay.Clarify == req {
				settle()
			}
			return nil
		})
	}
	if !a.store.respondToServerRequest(req.RequestID, tuigateway.ClarifyAnswer{}) {
		a.store.overlay.Clarify = nil
		return nil
	}
	settle()
	return nil
}

// Answers one clarify question, a single shared prompt at once and a batch by locking each answer, the card following every lock until the last
// one settles the batch
func (a *app) answerClarifyQuestion(req *clarifyReq, q clarifyQuestion, answer string) tea.Cmd {
	if a.store.overlay.Clarify != req || req.Cancelling {
		return nil
	}
	if req.Shared != nil && len(req.Questions) == 1 {
		params := tuigateway.CanonicalClarifyParams{SessionID: req.Shared.SessionID, ExecutionGeneration: req.Shared.ExecutionGeneration, PromptID: req.Shared.PromptID, Answer: answer}
		return ask(a.events.rpc, a.sys, tuigateway.CanonicalClarifyRespond, params, func(r *tuigateway.PromptResponseResult) tea.Cmd {
			if r != nil && r.Status == "resolved" {
				a.settleClarify(req, map[string]string{q.QID: answer})
			}
			return nil
		})
	}
	params := tuigateway.ClarifyLockParams{RequestID: req.RequestID, QuestionID: q.QID}
	if trimJS(answer) != "" {
		params.Answer = &answer
	}
	return ask(a.events.rpc, a.sys, tuigateway.ClarifyLock, params, func(r *tuigateway.ClarifyLockResult) tea.Cmd {
		if r == nil || a.store.overlay.Clarify != req {
			return nil
		}
		if r.Status == "expired" {
			a.store.overlay.Clarify = nil
			return nil
		}
		if req.Answers == nil {
			req.Answers = map[string]string{}
		}
		req.Answers[q.QID] = answer
		if len(r.Remaining) > 0 {
			a.dialogs.clarifyLocked(req)
			return nil
		}
		a.settleClarify(req, req.Answers)
		return nil
	})
}

// Scrollable text over the session, for logs, history and long command output, read with the keys of reading mode
type pagerCard struct {
	app    *app
	title  string
	lines  []string
	offset int
	height int
	width  int
	text   string
	reader *cards.Reader
}

// Pager over text, opened at its start in reading mode
func (a *app) page(text, title string) tea.Cmd {
	p := &pagerCard{app: a, title: title, text: strings.TrimRight(stripAnsi(text), "\n")}
	p.reader = cards.NewReader(p, a.keys)
	p.reader.Start(false)
	return a.wm.ShowOverlay(p)
}

func (p *pagerCard) Title() string       { return p.title }
func (p *pagerCard) Key() string         { return "pager" }
func (p *pagerCard) Init() tea.Cmd       { return nil }
func (p *pagerCard) Selectable() bool    { return true }
func (p *pagerCard) Contexts() []string  { return p.reader.Contexts() }
func (p *pagerCard) MinSize() (int, int) { w, h := p.app.wm.Size(); return w * 8 / 10, h * 6 / 10 }
func (p *pagerCard) Hints() []wm.Hint    { return p.reader.Hints() }

func (p *pagerCard) Status() string {
	if len(p.lines) <= p.height {
		return trSome(txt.PagerLinesOne, txt.PagerLinesOther, len(p.lines))
	}
	return tr(txt.PagerRange, p.offset+1, min(p.offset+p.height, len(p.lines)), len(p.lines))
}

// Rows of the wrapped text
func (p *pagerCard) ReadRows() int { return len(p.lines) }

// Text of a row
func (p *pagerCard) ReadRow(i int) string {
	if i < 0 || i >= len(p.lines) {
		return ""
	}
	return p.lines[i]
}

// First row in view and the rows of the window
func (p *pagerCard) ReadWindow() (int, int) { return p.offset, max(p.height-1, 1) }

// Scrolls so that the row is the first in view
func (p *pagerCard) ReadScroll(top int) { p.offset = min(max(top, 0), max(len(p.lines)-p.height+1, 0)) }

// A pager has no prompts to jump between
func (p *pagerCard) ReadPrompt(int, int) (int, bool) { return 0, false }

// Rows of the paragraph holding a row, between the blank rows around it
func (p *pagerCard) ReadBlock(row int) (int, int) {
	from, to := row, row+1
	for from > 0 && strings.TrimSpace(p.lines[from-1]) != "" {
		from--
	}
	for to < len(p.lines) && strings.TrimSpace(p.lines[to]) != "" {
		to++
	}
	return from, to
}

func (p *pagerCard) View(w, h int) string {
	if w != p.width {
		p.width = w
		p.lines = strings.Split(ansi.Wrap(p.text, max(w-2, 8), ""), "\n")
	}
	p.height = h
	body := max(h-1, 1)
	p.offset = min(max(p.offset, 0), max(len(p.lines)-body, 0))
	t := p.app.store.ui.Theme
	tone := toneStyle(t.Color.Text)
	rows := make([]string, 0, h)
	for i := p.offset; i < min(p.offset+body, len(p.lines)); i++ {
		rows = append(rows, p.reader.Mark(i, " "+tone.Render(p.lines[i]), p.app.uiTheme()))
	}
	for len(rows) < body {
		rows = append(rows, "")
	}
	line, _ := p.reader.Line(w, p.app.uiTheme())
	return strings.Join(append(rows, line), "\n")
}

// Cursor of a search being typed on the reading line
func (p *pagerCard) Cursor() *tea.Cursor {
	if _, caret := p.reader.Line(p.width, p.app.uiTheme()); caret >= 0 {
		return tea.NewCursor(caret, max(p.height-1, 0))
	}
	return nil
}

func (p *pagerCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	switch m := m.(type) {
	case wm.ActionMsg:
		cmd, _ := p.reader.Action(m.ID)
		if !p.reader.Active() {
			return p, tea.Batch(cmd, wm.RequestDismissOf(p))
		}
		return p, cmd
	case tea.KeyPressMsg, tea.PasteMsg, tea.ClipboardMsg:
		p.reader.Take(m)
	case tea.MouseWheelMsg:
		if m.Button == tea.MouseWheelUp {
			p.offset -= 3
		} else if m.Button == tea.MouseWheelDown {
			p.offset += 3
		}
	}
	return p, nil
}
