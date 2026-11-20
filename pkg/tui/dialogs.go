package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Widest a dialog asks to be
const dialogWidth = 84

// Keymap context of the prompt dialogs, ahead of the palette keys the manager adds to every overlay
const promptContext = "prompt"

// Wrapped rows of a command an approval shows at once, the page key bringing the rest
const commandPageRows = 10

// Rows the manager spends around the content of an overlay: the bar, the frame and its padding
const dialogChromeRows = 5

// Least cells a field keeps for its text beside its label, a longer label going on rows of its own
const minFieldRoom = 12

// Registers the keys of the prompt dialogs: declining, stepping through rows and questions, editing the answer being typed and reading the transcript behind
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
		{ID: "tui.prompt.newline", Context: promptContext, Label: "newline", Doc: "break the line in the answer being typed", Default: []string{"shift+enter", "ctrl+j", "alt+enter", "ctrl+enter", "super+enter"}, Quiet: true},
		{ID: "tui.prompt.word.left", Context: promptContext, Label: "word left", Doc: "move the cursor one word to the left", Default: []string{"ctrl+left", "alt+left", "alt+b"}, Quiet: true},
		{ID: "tui.prompt.word.right", Context: promptContext, Label: "word right", Doc: "move the cursor one word to the right", Default: []string{"ctrl+right", "alt+right", "alt+f"}, Quiet: true},
		{ID: "tui.prompt.delete.word.back", Context: promptContext, Label: "delete word back", Doc: "delete the word before the cursor", Default: []string{"ctrl+backspace", "alt+backspace"}, Quiet: true},
		{ID: "tui.prompt.delete.word.forward", Context: promptContext, Label: "delete word forward", Doc: "delete the word after the cursor", Default: []string{"ctrl+delete", "alt+d"}, Quiet: true},
		{ID: "tui.prompt.delete.after", Context: promptContext, Label: "delete to end", Doc: "delete from the cursor to the end of the line", Default: []string{"ctrl+k"}, Quiet: true},
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
}

// Text the user types into a dialog, masked for credentials, taking several lines where an answer may run long, a hint standing in while it holds
// the focus and is empty
type dialogField struct {
	label     string
	value     []rune
	cursor    int
	masked    bool
	multiline bool
	hint      string
}

// Puts text in at the caret, control characters dropped and line breaks kept only where the field takes several lines
func (f *dialogField) insert(text string) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if !f.multiline {
		text = strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", " ")
	}
	text = strings.ReplaceAll(text, "\t", "    ")
	runes := slices.DeleteFunc([]rune(text), func(r rune) bool { return r != '\n' && unicode.IsControl(r) })
	f.cursor = min(max(f.cursor, 0), len(f.value))
	f.value = slices.Insert(f.value, f.cursor, runes...)
	f.cursor += len(runes)
}

// Offsets the line the caret is on starts and ends at
func (f *dialogField) line() (int, int) {
	start, end := f.cursor, f.cursor
	for start > 0 && f.value[start-1] != '\n' {
		start--
	}
	for end < len(f.value) && f.value[end] != '\n' {
		end++
	}
	return start, end
}

// Offset the word before the caret starts at
func (f *dialogField) wordLeft() int {
	i := f.cursor
	for i > 0 && unicode.IsSpace(f.value[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(f.value[i-1]) {
		i--
	}
	return i
}

// Offset the word after the caret ends at
func (f *dialogField) wordRight() int {
	i := f.cursor
	for i < len(f.value) && unicode.IsSpace(f.value[i]) {
		i++
	}
	for i < len(f.value) && !unicode.IsSpace(f.value[i]) {
		i++
	}
	return i
}

// Removes the text between two offsets and leaves the caret where it began
func (f *dialogField) cut(from, to int) {
	f.value = slices.Delete(f.value, from, to)
	f.cursor = from
}

// Moves the caret a line up or down keeping its column, false when the text has no line there
func (f *dialogField) vertical(step int) bool {
	f.cursor = min(max(f.cursor, 0), len(f.value))
	start, end := f.line()
	column := f.cursor - start
	if step < 0 {
		if start == 0 {
			return false
		}
		f.cursor = start - 1
		above, _ := f.line()
		f.cursor = min(above+column, start-1)
		return true
	}
	if end == len(f.value) {
		return false
	}
	f.cursor = end + 1
	_, below := f.line()
	f.cursor = min(end+1+column, below)
	return true
}

// Carries out an editing action on the text, false for an action that is not one
func (f *dialogField) edit(id string) bool {
	f.cursor = min(max(f.cursor, 0), len(f.value))
	start, end := f.line()
	switch id {
	case "palette.left":
		f.cursor = max(f.cursor-1, 0)
	case "palette.right":
		f.cursor = min(f.cursor+1, len(f.value))
	case "palette.home":
		f.cursor = start
	case "palette.end":
		f.cursor = end
	case "tui.prompt.word.left":
		f.cursor = f.wordLeft()
	case "tui.prompt.word.right":
		f.cursor = f.wordRight()
	case "palette.delete.back":
		if f.cursor > 0 {
			f.cut(f.cursor-1, f.cursor)
		}
	case "palette.delete.forward":
		if f.cursor < len(f.value) {
			f.cut(f.cursor, f.cursor+1)
		}
	case "palette.delete.word", "tui.prompt.delete.word.back":
		f.cut(f.wordLeft(), f.cursor)
	case "tui.prompt.delete.word.forward":
		f.cut(f.cursor, f.wordRight())
	case "palette.clear":
		f.cut(start, f.cursor)
	case "tui.prompt.delete.after":
		f.cut(f.cursor, end)
	case "tui.prompt.newline":
		if f.multiline {
			f.insert("\n")
		}
	default:
		return false
	}
	return true
}

// Text of the field as rows of at most room cells, a credential as dots, with the column and row of the caret
func (f *dialogField) wrapped(room int) (lines []string, cx, cy int) {
	var row strings.Builder
	column := 0
	flush := func() {
		lines = append(lines, row.String())
		row.Reset()
		column = 0
	}
	for i, r := range f.value {
		if r == '\n' {
			if i == f.cursor {
				cx, cy = column, len(lines)
			}
			flush()
			continue
		}
		cell := string(r)
		if f.masked {
			cell = "•"
		}
		w := ansi.StringWidth(cell)
		if column > 0 && column+w > room {
			flush()
		}
		if i == f.cursor {
			cx, cy = column, len(lines)
		}
		row.WriteString(cell)
		column += w
	}
	if f.cursor >= len(f.value) {
		cx, cy = column, len(lines)
	}
	return append(lines, row.String()), cx, cy
}

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
}

// Dialog over the session, a message with choices to pick, fields to fill, or both
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
	// Prompt of the store: an answer or a decline leaves the card open until the store drops the prompt
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

func (d *dialogCard) Title() string  { return d.title }
func (d *dialogCard) Key() string    { return "dialog:" + d.kind + ":" + d.requestID }
func (d *dialogCard) Init() tea.Cmd  { return nil }
func (d *dialogCard) Glyph() string  { return d.app.uiTheme().Glyphs.Ask }
func (d *dialogCard) Closable() bool { return true }

// Wheel turned beside the dialog scrolls the card under the pointer, the transcript the prompt asks about
func (d *dialogCard) PassesWheel() bool { return true }

// Prompt keys of every dialog, then the verdict keys of an approval while a verdict is highlighted and the yes and no keys of a confirmation
func (d *dialogCard) Contexts() []string {
	switch {
	case d.kind == "approval" && d.focus < 0:
		return []string{promptContext, keys.Approval}
	case d.kind == "confirm":
		return []string{promptContext, keys.Confirm}
	}
	return []string{promptContext}
}

// Close key while an answer is typed beside choices: back to the choices, the prompt still open, an open ended question leaving its typing first
func (d *dialogCard) Back() bool {
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

// Rows the dialog needs at its width
func (d *dialogCard) MinSize() (int, int) {
	w, _ := d.app.wm.Size()
	width := min(dialogWidth, max(w-8, 30))
	return width, len(d.rows(width))
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
	if d.danger {
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
		key := "prompt.approval.pageEnd"
		if end < len(lines) {
			key = "prompt.approval.pageMore"
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
			text, tone := tr("prompt.clarify.skipped"), t.Color.Muted
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
			switch {
			case lit && n == 0:
				rows = append(rows, pad+toneStyle(t.Color.Accent).Bold(true).Render("▸")+toneStyle(t.Color.Text).Bold(true).Render(text))
			case lit:
				rows = append(rows, pad+" "+toneStyle(t.Color.Text).Bold(true).Render(text))
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
	lines, cx, cy := f.wrapped(max(width-indent-1, 4))
	text := t.Color.Text
	if focused {
		d.cursor = tea.NewCursor(indent+cx, at+len(rows)+cy)
		d.anchor = at + len(rows) + cy
		if len(f.value) == 0 && f.hint != "" {
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
		return []wm.Hint{{Action: "palette.pick", Label: "deny and send"}, {Action: "tui.prompt.cancel", Label: "back"}}
	case d.kind == "approval":
		return []wm.Hint{{Action: "approval.allow"}, {Action: "approval.deny"}, {Action: "palette.pick", Label: "choose"}}
	case d.batch != nil && len(d.batch.req.Questions) > 1:
		return []wm.Hint{{Action: "palette.pick", Label: "answer"}, {Action: "tui.prompt.next", Pair: "tui.prompt.prev", Label: "question"}, {Action: "tui.prompt.cancel", Label: "cancel"}}
	case len(d.choices) == 0 && len(d.fields) == 0:
		return []wm.Hint{{Action: "tui.prompt.cancel", Label: "close"}}
	}
	return []wm.Hint{{Action: "palette.pick", Label: "answer"}, {Action: "tui.prompt.cancel", Label: "cancel"}}
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
	for i, f := range d.fields {
		values[i] = string(f.value)
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

// Close key: the answer being typed is left first, then a prompt of the store is declined in place and any other dialog closes
func (d *dialogCard) closeKey() tea.Cmd {
	switch {
	case d.Back():
		return nil
	case d.held:
		return d.cancel()
	}
	return wm.RequestDismissOf(d)
}

// Carries out a keymap action: the rows, the answer, the text being typed, and the transcript behind for the keys that scroll
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
		if f == nil || !f.vertical(-1) {
			d.move(-1)
		}
	case "palette.down", "approval.down":
		if f == nil || !f.vertical(1) {
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
	case "palette.left", "palette.right":
		switch {
		case f != nil:
			f.edit(id)
		case id == "palette.left":
			d.move(-1)
		default:
			d.move(1)
		}
	default:
		if f != nil && f.edit(id) {
			d.wrote()
		}
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
		f.insert(text)
		d.wrote()
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
		d.fields[0].insert(text)
		d.wrote()
	}
	return nil
}

func (d *dialogCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	switch m := m.(type) {
	case wm.FocusMsg:
		if !m.Focused && !d.settled {
			d.settled = true
			if d.onCancel != nil {
				return d, d.onCancel()
			}
		}
	case wm.ActionMsg:
		return d, d.action(m.ID)
	case tea.PasteMsg:
		if f := d.field(); f != nil {
			f.insert(m.Content)
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
	d.batch.active, d.batch.typing = index, false
	d.title = tr("prompt.clarify.title")
	if count > 1 {
		d.title = tr("prompt.clarify.titleOf", index+1, count)
	}
	d.multi, d.marks, d.sel, d.focus, d.choices = q.MultiSelect, map[int]bool{}, 0, -1, nil
	for _, c := range q.Choices {
		d.choices = append(d.choices, dialogChoice{label: c, value: c})
	}
	label := tr("prompt.clarify.answerLabel") + " ›"
	if len(q.Choices) > 0 {
		label = fmt.Sprintf("%d   %s ›", len(q.Choices)+1, tr("prompt.clarify.other"))
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
	d.fields = []dialogField{{label: label, value: []rune(typed), cursor: len([]rune(typed)), multiline: true}}
	if len(q.Choices) == 0 || typed != "" {
		d.focus = 0
	}
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
	enter := tr("prompt.clarify.lockAnswer")
	if open == 1 {
		enter = tr("prompt.clarify.confirmAndContinue")
	}
	switch {
	case d.focus >= 0 && (len(d.choices) > 0 || d.batch.typing):
		return tr("prompt.clarify.typingHint", enter)
	case d.multi:
		return tr("prompt.clarify.toggle") + " · " + tr("prompt.clarify.hint", enter)
	}
	return tr("prompt.clarify.hint", enter)
}

// Dialogs of the session prompts, the one showing and what the bar says while one waits
type dialogState struct {
	current *dialogCard
}

// Kind, request and store object of the prompt that should be showing, by urgency
func (a *app) wantedDialog() (kind, id string, ref any) {
	o := a.store.overlay
	switch {
	case o.Approval != nil:
		return "approval", o.Approval.RequestID, o.Approval
	case o.Sudo != nil:
		return "sudo", o.Sudo.RequestID, o.Sudo
	case o.Secret != nil:
		return "secret", o.Secret.RequestID, o.Secret
	case o.VaultUnlock != nil:
		return "vaultUnlock", o.VaultUnlock.RequestID, o.VaultUnlock
	case o.VaultSaveLogin != nil:
		return "vaultSaveLogin", o.VaultSaveLogin.RequestID, o.VaultSaveLogin
	case o.VaultCode != nil:
		return "vaultCode", o.VaultCode.RequestID, o.VaultCode
	case o.Connection != nil && a.store.connectionOp != nil && a.store.connectionOp.OpID == o.Connection.OpID:
		op := a.store.connectionOp
		key := op.OpID + ":" + strconv.FormatInt(op.Seq, 10)
		if target := a.activeConnectionTarget(); target != nil {
			key += ":" + target.Name + ":" + string(target.State) + ":" + val(target.ConnectURL)
		}
		return "connection", key, op
	case o.Clarify != nil:
		return "clarify", o.Clarify.RequestID, o.Clarify
	case o.Confirm != nil:
		return "confirm", o.Confirm.Title, o.Confirm
	}
	return "", "", nil
}

// Whether a prompt waits on the user
func (s *dialogState) waiting() bool { return s.current != nil && !s.current.settled }

// Bar notice of the waiting prompt
func (s *dialogState) notice() (string, wm.Signal, bool) {
	if !s.waiting() {
		return "", wm.Idle, false
	}
	return s.current.title, wm.Blocked, true
}

// Follows a locked answer of a question set: the question in view stays while it lacks an answer, else the first one still open comes into view
func (s *dialogState) clarifyLocked(req *clarifyReq) {
	d := s.current
	if d == nil || d.batch == nil || d.batch.req != req {
		return
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

// Interrupt key while an overlay is up: the connection card stops the turn that owns its operation, a waiting prompt is declined and any other
// overlay closes, the session behind left alone
func (a *app) interruptOverlay() tea.Cmd {
	cur := a.dialogs.current
	if !a.dialogs.waiting() || a.wm.Overlay() != wm.Card(cur) {
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

// Opens the dialog of the prompt the store holds and closes one whose prompt settled elsewhere
func (a *app) syncDialogs() tea.Cmd {
	kind, id, ref := a.wantedDialog()
	cur := a.dialogs.current
	if cur != nil && (cur.settled || a.wm.Overlay() != wm.Card(cur)) {
		a.dialogs.current, cur = nil, nil
	}
	if cur != nil && (cur.kind != kind || cur.requestID != id || cur.ref != ref) {
		cur.settled = true
		a.dialogs.current = nil
		if kind == "" {
			return a.wm.Dismiss()
		}
	}
	if kind == "" || a.dialogs.current != nil {
		return nil
	}
	d := a.buildDialog(kind, id, cur)
	if d == nil {
		return nil
	}
	d.ref, d.held = ref, true
	a.dialogs.current = d
	return a.wm.ShowOverlay(d)
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
		d.title, d.danger, d.command = tr("prompt.approval.title"), true, req.Command
		d.body = []string{req.Description}
		if req.SmartDenied {
			d.body = append(d.body, tr("prompt.approval.smartDenied"))
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
		if slices.ContainsFunc(d.choices, func(c dialogChoice) bool { return c.value == "deny" }) {
			d.fields = []dialogField{{label: fmt.Sprintf("%d   %s ›", len(d.choices)+1, tr("prompt.approval.tell")), hint: tr("prompt.approval.tellHint"), multiline: true}}
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
		body := []string{tr("secure.sudo.body")}
		if req.Command != "" {
			body = []string{req.Command, tr("secure.sudo.body")}
		}
		if req.ProfileKey != "" {
			body = []string{tr("secure.sudo.installBody", req.ProfileKey)}
		}
		return masked("sudo", tr("secure.sudo.title"), body, tr("secure.passwordLabel"), false, tr("session.input.sudoCancelled"))
	case "secret":
		req := o.Secret
		return masked("secret", "secret "+tr("secure.secret.forEnvVar", req.EnvVar), []string{req.Prompt}, req.EnvVar, false, tr("session.input.secretCancelled"))
	case "vaultUnlock":
		req := o.VaultUnlock
		name := req.DisplayName
		if name == "" {
			name = req.Backend
		}
		return masked("vaultUnlock", tr("secure.vault.unlockTitle", name), []string{tr("secure.vault.unlockBody", name)}, tr("secure.passwordLabel"), false,
			tr("session.input.vaultStaysLocked", name))
	case "vaultCode":
		req := o.VaultCode
		body := []string{tr("secure.vault.codeBody", req.Site)}
		if req.Hint != "" {
			body = []string{req.Hint, tr("secure.vault.codeBody", req.Site)}
		}
		return masked("vaultCode", tr("secure.vault.codeTitle", req.Site), body, tr("secure.vault.codeLabel"), true, tr("session.input.codeSkipped", req.Site))
	case "vaultSaveLogin":
		req := o.VaultSaveLogin
		d.title = tr("secure.vault.saveTitle", req.Site)
		d.body = []string{tr("secure.vault.saveBody", req.Origin)}
		d.fields, d.focus = []dialogField{{label: tr("secure.vault.usernameLabel")}, {label: tr("secure.passwordLabel"), masked: true}}, 0
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
			a.sys(tr("session.input.loginNotSaved", req.Site))
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
			yes = tr("prompt.confirm.confirm")
		}
		if no == "" {
			no = tr("prompt.confirm.cancel")
		}
		d.choices = []dialogChoice{{label: no, value: "no"}, {label: yes, value: "yes"}}
		d.hint = func() string { return tr("prompt.confirm.hint") }
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
			return wm.Notify(tr("connection.notice.answerNotDelivered"), wm.Warn)
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
			return wm.Notify(tr("connection.notice.restartFailed"), wm.Warn)
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
		return tr("connection.verb.authorize")
	case "enable":
		return tr("connection.verb.enable")
	case "install":
		return tr("connection.verb.install")
	}
	return tr("connection.verb.connect")
}

// Connection card, every target with its state and the step the first waiting one needs, what was typed for that target kept from the card it replaces
func (a *app) connectionDialog(d, prev *dialogCard) *dialogCard {
	op := a.store.connectionOp
	target := a.activeConnectionTarget()
	trimmed := func(s *string) string { return strings.TrimFunc(val(s), isJSSpace) }
	hint := func(key string) func() string { return func() string { return tr(key) } }
	d.title = tr("connection.title")
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
		d.body, d.hint = append(d.body, tr("connection.status.finishing")), hint("connection.hint.finishing")
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
		d.body, d.hint = append(d.body, tr("connection.authorized.title"), trimmed(target.DiscoveryError)), hint("connection.authorized.hint")
		d.choices = []dialogChoice{{label: tr("connection.authorized.continue"), value: "continue"}}
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
	skip := dialogChoice{label: tr("connection.selector.skip"), value: "skip"}
	switch {
	case target.State == "failed":
		if trimmed(target.Detail) == "" {
			d.body = append(d.body, tr("connection.failure.failed"))
		}
		d.choices, d.hint = []dialogChoice{{label: tr("connection.selector.tryAgain"), value: "retry"}, skip}, hint("connection.hint.retry")
	case link != "":
		d.body = append(d.body, tr("connection.finishInBrowser", link))
		d.choices = []dialogChoice{{label: tr("connection.selector.open"), value: "open"}, {label: tr("connection.selector.checkNow"), value: "wake"}, skip}
		d.hint = hint("connection.hint.browser")
	case target.State == "initiated":
		d.body = append(d.body, tr("connection.waiting", connectionTargetName(*target)))
		d.choices, d.hint = []dialogChoice{{label: tr("connection.selector.checkNow"), value: "wake"}, skip}, hint("connection.hint.working")
	default:
		for _, field := range target.RequiredEnv {
			label := firstText(trimmed(field.Prompt), field.Name)
			d.fields = append(d.fields, dialogField{label: label, masked: field.Secret, value: []rune(field.Default), cursor: len([]rune(field.Default))})
		}
		d.choices = []dialogChoice{{label: connectionVerb(target.Action) + " " + connectionTargetName(*target), value: "approved"}, skip}
		d.hint = hint("connection.hint.form")
	}
	if prev != nil && prev.kind == "connection" && prev.subject == d.subject && len(prev.fields) == len(d.fields) {
		for i := range d.fields {
			if kept := prev.fields[i]; kept.label == d.fields[i].label {
				d.fields[i].value, d.fields[i].cursor = kept.value, kept.cursor
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
				return wm.Notify(tr("connection.notice.browserDidNotOpen"), wm.Warn)
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
				return wm.Notify(tr("connection.field.required", firstText(trimmed(field.Prompt), field.Name)), wm.Warn)
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
	case "once", "session", "always", "deny":
		return tr("prompt.approval." + choice)
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
				a.sys(tr("slashCmd.core.steer.queued", previewOf(text)))
			}
		} else {
			a.enqueue(text, text, destination)
			if shown {
				a.sys(tr("slashCmd.core.steer.rejected"))
			}
		}
		req.Tell = tellTold
		return a.answerApproval(req, "deny")
	}
	if !a.gw.canonical() {
		return call(a.gw, tuigateway.SessionSteer, tuigateway.SessionCorrectionParams{SessionID: sid, Text: text}, func(res *tuigateway.SessionCorrectionResult, err error) tea.Cmd {
			return told(val(res).Status, err == nil && res != nil)
		})
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
		a.store.turn.Outcome = tr("session.approval.approved", choice)
		if choice == "deny" {
			a.store.turn.Outcome = tr("session.approval.denied")
		}
		a.store.ui.Status = "running…"
	}
	openLink := func() tea.Cmd {
		if choice == "deny" || req.OpenURL == "" || openExternalURL(req.OpenURL) {
			return nil
		}
		a.sys(req.OpenURL)
		return wm.Notify(tr("mcp.notice.browserDidNotOpen"), wm.Warn)
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
	a.appendMessage(msg{Kind: "trail", Role: "system", Tools: []string{trail}})
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
		a.appendMessage(msg{Role: "system", Text: formatAbandonedClarify(req.Questions, answers, tr("gatewayMsg.clarify.cancelled"))})
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

// Scrollable text over the session, for logs, history and long command output
type pagerCard struct {
	app    *app
	title  string
	lines  []string
	offset int
	height int
	width  int
	text   string
}

// Pager over text, opened at its start
func (a *app) page(text, title string) tea.Cmd {
	return a.wm.ShowOverlay(&pagerCard{app: a, title: title, text: strings.TrimRight(stripAnsi(text), "\n")})
}

func (p *pagerCard) Title() string       { return p.title }
func (p *pagerCard) Key() string         { return "pager" }
func (p *pagerCard) Init() tea.Cmd       { return nil }
func (p *pagerCard) Selectable() bool    { return true }
func (p *pagerCard) Contexts() []string  { return []string{keys.Transcript} }
func (p *pagerCard) MinSize() (int, int) { w, h := p.app.wm.Size(); return w * 8 / 10, h * 6 / 10 }

func (p *pagerCard) Status() string {
	if len(p.lines) <= p.height {
		return countNoun(len(p.lines), "line")
	}
	return fmt.Sprintf("%d-%d of %d", p.offset+1, min(p.offset+p.height, len(p.lines)), len(p.lines))
}

func (p *pagerCard) Hints() []wm.Hint {
	return []wm.Hint{{Action: "transcript.down", Pair: "transcript.up"}, {Action: "transcript.page.down"}, {Action: "transcript.top"}, {Action: "transcript.bottom"}, {Action: "palette.close", Label: "close"}}
}

func (p *pagerCard) View(w, h int) string {
	if w != p.width {
		p.width = w
		p.lines = strings.Split(ansi.Wrap(p.text, max(w-2, 8), ""), "\n")
	}
	p.height = h
	p.offset = min(max(p.offset, 0), max(len(p.lines)-h, 0))
	tone := toneStyle(p.app.store.ui.Theme.Color.Text)
	rows := make([]string, 0, h)
	for i := p.offset; i < min(p.offset+h, len(p.lines)); i++ {
		rows = append(rows, " "+tone.Render(p.lines[i]))
	}
	return strings.Join(rows, "\n")
}

func (p *pagerCard) Update(m tea.Msg) (wm.Card, tea.Cmd) {
	switch m := m.(type) {
	case wm.ActionMsg:
		switch m.ID {
		case "transcript.down", "palette.down":
			p.offset++
		case "transcript.up", "palette.up":
			p.offset--
		case "transcript.page.down", "palette.page.down":
			p.offset += max(p.height-1, 1)
		case "transcript.page.up", "palette.page.up":
			p.offset -= max(p.height-1, 1)
		case "transcript.top":
			p.offset = 0
		case "transcript.bottom":
			p.offset = len(p.lines)
		case "palette.pick":
			return p, wm.RequestDismissOf(p)
		}
	case tea.KeyPressMsg:
		switch m.Text {
		case "q":
			return p, wm.RequestDismissOf(p)
		case "b":
			p.offset -= max(p.height-1, 1)
		}
	case tea.MouseWheelMsg:
		if m.Button == tea.MouseWheelUp {
			p.offset -= 3
		} else if m.Button == tea.MouseWheelDown {
			p.offset += 3
		}
	}
	return p, nil
}
