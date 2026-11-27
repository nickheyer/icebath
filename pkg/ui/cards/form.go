package cards

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/field"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/lang"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Keymap contexts of a form: the keys of every row, of a row typed into and of a row picked from
const (
	FormContext     = "form"
	FormTextContext = "form.text"
	FormPickContext = "form.pick"
)

// Keys of forms, registered with the hub keys
func registerFormKeys(km *keys.Map) {
	for name, doc := range map[string]string{FormContext: "forms", FormTextContext: "form text fields", FormPickContext: "form choice fields"} {
		if keys.ContextDocs[name] == "" {
			if err := keys.AddContext(name, doc); err != nil {
				panic(err)
			}
		}
	}
	for _, a := range []keys.Action{
		{ID: "form.next", Context: FormContext, Label: "next", Doc: "Move to the next field", Default: []string{"down", "tab"}, Quiet: true},
		{ID: "form.prev", Context: FormContext, Label: "previous", Doc: "Move to the previous field", Default: []string{"up", "shift+tab"}, Quiet: true},
		{ID: "form.submit", Context: FormContext, Label: "submit", Doc: "Submit the form", Default: []string{"ctrl+s"}},
		{ID: "form.cancel", Context: FormContext, Label: "cancel", Doc: "Close the form without saving", Default: []string{"esc"}},
		{ID: "form.enter", Context: FormTextContext, Label: "next", Doc: "Go to the next field, or submit from the last one", Default: []string{"enter"}},
		{ID: "form.toggle", Context: FormPickContext, Label: "change", Doc: "Toggle, choose or press the selected field", Default: []string{"enter", "space"}},
		{ID: "form.choice.next", Context: FormPickContext, Label: "next choice", Doc: "Move to the next choice", Default: []string{"right"}, Quiet: true},
		{ID: "form.choice.prev", Context: FormPickContext, Label: "previous choice", Doc: "Move to the previous choice", Default: []string{"left"}, Quiet: true},
	} {
		km.Register(a)
	}
}

// Kind of value a form field holds
type FieldKind int

const (
	// One line of text
	FieldText FieldKind = iota
	// Text shown as dots
	FieldSecret
	// Whole or decimal number typed as text
	FieldNumber
	// Yes or no
	FieldBool
	// One of a set of choices
	FieldChoice
	// Line of text that takes no answer
	FieldNote
	// Button that runs something of the caller's when pressed
	FieldButton
)

// One choice of a field, the value sent and the words shown
type Choice struct {
	Value   string
	Label   string
	Meaning string
}

// One field of a form
type Field struct {
	ID    string
	Label string
	// One line shown under the field while it has the cursor
	Help string
	Kind FieldKind
	// Text of the field, the value of a choice, true or false for a switch
	Value       string
	Choices     []Choice
	Required    bool
	Placeholder string
	// Kept behind the more options row until it is opened
	More bool
	// A choice made in the picker of the caller whatever the number of choices, such as one that ticks several values
	Picked bool
	// What a choice shows for a value none of its choices has, the value itself when nil
	Says func(value string) string
	// The text it opens with is selected, so the first key typed replaces it and a key that moves the cursor keeps it
	Select bool
	// Says what is wrong with a value, empty when it is fine
	Check func(value string) string
}

// Form a caller describes: its fields, what sending it does and any keys of its own
type FormSpec struct {
	Title string
	// Lines above the fields, such as the usage line of a command
	Intro  []string
	Fields []Field
	// Word on the button that sends the form
	Submit string
	// Handed the values by field id once every field checks out
	Done func(f *Form, values map[string]string) tea.Cmd
	// Runs on the cancel key, nil closing nothing on its own
	Cancel func(f *Form) tea.Cmd
	// Opens a long list of choices in a picker of the caller's, nil cycling through them in place
	Pick func(f *Form, field Field) tea.Cmd
	// Runs once a field's answer is given: a switch flipped, a choice made, or text kept as the cursor leaves the field
	Changed func(f *Form, field Field) tea.Cmd
	// Runs a button field when it is pressed
	Press func(f *Form, field Field) tea.Cmd
	// Keymap context of the form's own actions and those actions by id
	Context string
	Actions map[string]func(f *Form) tea.Cmd
	// Buttons for the form's own actions
	Hints []wm.Hint
}

// Row of a form: a field, the row that shows the fields kept back, or the send button
type formRow struct {
	field  int
	more   bool
	submit bool
}

// Card of fields typed into or picked from, checked and sent with one key
type Form struct {
	env    *Env
	spec   FormSpec
	fields []Field
	// Line editor of each field typed into, nil for the others
	edits []*field.Field
	// Text of each field typed into as last handed to Changed
	kept  map[string]string
	focus int
	more  bool
	busy  bool
	errs  map[string]string
	top   int
	w, h  int
	// Cell of the cursor at the last paint, x below zero for none
	cx, cy int
}

// Window manager interfaces the form answers
var _ interface {
	wm.Card
	wm.Contexter
	wm.Hinter
	wm.Cursorer
} = (*Form)(nil)

// Builds a form from the spec, the cursor on its first field
func NewForm(env *Env, spec FormSpec) *Form {
	f := &Form{env: env, spec: spec, errs: map[string]string{}, cx: -1}
	f.place(spec.Fields)
	f.focus = f.next(-1, 1)
	return f
}

// Takes the fields in, each typed field with its own line editor
func (f *Form) place(fields []Field) {
	f.fields = slices.Clone(fields)
	f.edits = make([]*field.Field, len(f.fields))
	f.kept = map[string]string{}
	for i, fd := range f.fields {
		if typed(fd.Kind) {
			f.edits[i] = field.New(fd.Value)
			f.edits[i].Masked = fd.Kind == FieldSecret
			if fd.Select {
				f.edits[i].SelectAll()
			}
			f.kept[fd.ID] = fd.Value
		}
	}
}

// Replaces the fields, such as when an answer shows or hides others, the cursor staying on the field it was on
func (f *Form) SetFields(fields []Field) {
	at := ""
	if fd := f.field(); fd != nil {
		at = fd.ID
	}
	f.place(fields)
	for i, r := range f.rows() {
		if r.field >= 0 && f.fields[r.field].ID == at {
			f.focus = i
			return
		}
	}
	f.focus = f.next(min(f.focus, len(f.rows())-1)-1, 1)
}

// Sets a field's value and hands it to Changed, as a choice made in a picker of the caller's
func (f *Form) Commit(id, value string) tea.Cmd {
	f.SetValue(id, value)
	for _, fd := range f.fields {
		if fd.ID == id {
			return f.changed(fd)
		}
	}
	return nil
}

// Hands an answer to Changed
func (f *Form) changed(fd Field) tea.Cmd {
	if typed(fd.Kind) {
		f.kept[fd.ID] = fd.Value
	}
	if f.spec.Changed == nil {
		return nil
	}
	return f.spec.Changed(f, fd)
}

// Hands the text of the field under the cursor to Changed when it differs from the text last handed on
func (f *Form) leave() tea.Cmd {
	if fd := f.field(); fd != nil && typed(fd.Kind) && f.kept[fd.ID] != fd.Value {
		return f.changed(*fd)
	}
	return nil
}

// Moves the cursor to row i, keeping the text of the field it leaves
func (f *Form) moveTo(i int) tea.Cmd {
	if i == f.focus {
		return nil
	}
	cmd := f.leave()
	f.focus = i
	return cmd
}

// Line editor of the field under the cursor, nil when it is not typed into
func (f *Form) edit() *field.Field {
	if r := f.row(); r.field >= 0 {
		return f.edits[r.field]
	}
	return nil
}

// Copies the text of the field under the cursor out of its editor
func (f *Form) sync() {
	if r := f.row(); r.field >= 0 && f.edits[r.field] != nil {
		fd := &f.fields[r.field]
		if v := f.edits[r.field].Value(); v != fd.Value {
			fd.Value = v
			delete(f.errs, fd.ID)
		}
	}
}

func (f *Form) Title() string { return f.spec.Title }
func (f *Form) Init() tea.Cmd { return nil }

// Rows in view order, the fields kept back shown only once opened
func (f *Form) rows() []formRow {
	var out []formRow
	hidden := 0
	for i, fd := range f.fields {
		if fd.More && !f.more {
			hidden++
			continue
		}
		out = append(out, formRow{field: i})
	}
	if hidden > 0 || slices.ContainsFunc(f.fields, func(fd Field) bool { return fd.More }) {
		out = append(out, formRow{field: -1, more: true})
	}
	return append(out, formRow{field: -1, submit: true})
}

// Whether a row takes the cursor, notes being read only
func (f *Form) focusable(r formRow) bool { return r.field < 0 || f.fields[r.field].Kind != FieldNote }

// Index of the next row from i in direction d that takes the cursor, i when none does
func (f *Form) next(i, d int) int {
	rows := f.rows()
	for j := i + d; j >= 0 && j < len(rows); j += d {
		if f.focusable(rows[j]) {
			return j
		}
	}
	if i < 0 {
		return 0
	}
	return i
}

// Row under the cursor
func (f *Form) row() formRow {
	rows := f.rows()
	f.focus = min(max(f.focus, 0), len(rows)-1)
	return rows[f.focus]
}

// Field under the cursor, nil on the more and send rows
func (f *Form) field() *Field {
	if r := f.row(); r.field >= 0 {
		return &f.fields[r.field]
	}
	return nil
}

// Reports whether a field is typed into
func typed(k FieldKind) bool { return k == FieldText || k == FieldSecret || k == FieldNumber }

func (f *Form) Contexts() []string {
	out := []string{}
	if f.spec.Context != "" {
		out = append(out, f.spec.Context)
	}
	if fd := f.field(); fd != nil && typed(fd.Kind) {
		return append(out, FormTextContext, FormContext, keys.Text)
	}
	return append(out, FormPickContext, FormContext)
}

// Buttons of the form: what enter does on the row in hand, sending the form when enter does something else, then the form's own
func (f *Form) Hints() []wm.Hint {
	send := f.spec.Submit
	if send == "" {
		send = lang.T(txt.KitFormSend)
	}
	rows := f.rows()
	r := f.row()
	var hints []wm.Hint
	switch fd := f.field(); {
	case r.submit:
		hints = []wm.Hint{{Action: "form.toggle", Label: send}}
	case r.more:
		hints = []wm.Hint{{Action: "form.toggle", Label: "show"}, {Action: "form.submit", Label: send}}
	case fd != nil && typed(fd.Kind) && rows[f.next(f.focus, 1)].submit:
		hints = []wm.Hint{{Action: "form.enter", Label: send}}
	case fd != nil && typed(fd.Kind):
		hints = []wm.Hint{{Action: "form.enter", Label: "next"}, {Action: "form.submit", Label: send}}
	case fd != nil && fd.Kind == FieldButton:
		hints = []wm.Hint{{Action: "form.toggle", Label: fd.Label}, {Action: "form.submit", Label: send}}
	default:
		hints = []wm.Hint{{Action: "form.toggle", Label: "change"}, {Action: "form.submit", Label: send}}
	}
	return append(hints, f.spec.Hints...)
}

// Value of a field by id, empty for an unknown one
func (f *Form) Value(id string) string {
	for _, fd := range f.fields {
		if fd.ID == id {
			return fd.Value
		}
	}
	return ""
}

// Sets the value of a field by id
func (f *Form) SetValue(id, value string) {
	for i := range f.fields {
		if f.fields[i].ID == id {
			f.fields[i].Value = value
			if f.edits[i] != nil {
				f.edits[i].SetValue(value)
				f.kept[id] = value
			}
			delete(f.errs, id)
		}
	}
}

// Shows what is wrong with a field under it, such as a refusal of the value, and puts the cursor on it
func (f *Form) SetError(id, text string) {
	f.errs[id] = text
	for i, r := range f.rows() {
		if r.field >= 0 && f.fields[r.field].ID == id {
			f.focus = i
		}
	}
}

// Ignores sending while the caller waits on the last one
func (f *Form) SetBusy(on bool) { f.busy = on }

// Values of every field by id
func (f *Form) Values() map[string]string {
	out := make(map[string]string, len(f.fields))
	for _, fd := range f.fields {
		if fd.Kind != FieldNote && fd.Kind != FieldButton {
			out[fd.ID] = fd.Value
		}
	}
	return out
}

// Checks every field and hands the values on, the cursor going to the first field that is wrong
func (f *Form) submit() tea.Cmd {
	if f.busy {
		return nil
	}
	kept := f.leave()
	f.errs = map[string]string{}
	first := ""
	for _, fd := range f.fields {
		problem := ""
		switch v := strings.TrimSpace(fd.Value); {
		case fd.Kind == FieldNote, fd.Kind == FieldButton:
		case fd.Required && v == "":
			problem = lang.T(txt.KitFormRequired)
		case fd.Kind == FieldNumber && v != "":
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				problem = lang.T(txt.KitFormNumber)
			}
		}
		if problem == "" && fd.Check != nil && fd.Kind != FieldNote && fd.Kind != FieldButton {
			problem = fd.Check(fd.Value)
		}
		if problem != "" {
			f.errs[fd.ID] = problem
			if first == "" {
				first = fd.ID
			}
		}
	}
	if first != "" {
		if !f.more {
			for _, fd := range f.fields {
				if fd.ID == first && fd.More {
					f.more = true
				}
			}
		}
		f.SetError(first, f.errs[first])
		return kept
	}
	if f.spec.Done == nil {
		return kept
	}
	return tea.Batch(kept, f.spec.Done(f, f.Values()))
}

// Index of the choice holding the field's value, -1 for none
func choiceAt(fd *Field) int {
	return slices.IndexFunc(fd.Choices, func(c Choice) bool { return c.Value == fd.Value })
}

// Moves a choice field by d choices, wrapping round
func (f *Form) cycle(fd *Field, d int) {
	if len(fd.Choices) == 0 {
		return
	}
	i := choiceAt(fd)
	if i < 0 && d < 0 {
		i = 0
	}
	fd.Value = fd.Choices[(i+d+len(fd.Choices))%len(fd.Choices)].Value
	delete(f.errs, fd.ID)
}

// Choices a field cycles through in place before a picker opens for them
const pickAbove = 8

// Changes the row under the cursor: a switch flips, a choice moves on or opens its picker, the more row opens and the button sends
func (f *Form) toggle() tea.Cmd {
	r := f.row()
	switch {
	case r.submit:
		return f.submit()
	case r.more:
		f.more = !f.more
		return nil
	}
	fd := &f.fields[r.field]
	switch fd.Kind {
	case FieldBool:
		fd.Value = strconv.FormatBool(fd.Value != "true")
		delete(f.errs, fd.ID)
		return f.changed(*fd)
	case FieldChoice:
		if (len(fd.Choices) > pickAbove || fd.Picked) && f.spec.Pick != nil {
			return f.spec.Pick(f, *fd)
		}
		f.cycle(fd, 1)
		return f.changed(*fd)
	case FieldButton:
		if f.spec.Press != nil {
			return f.spec.Press(f, *fd)
		}
	}
	return nil
}

func (f *Form) Update(msg tea.Msg) (wm.Card, tea.Cmd) {
	switch msg := msg.(type) {
	case wm.ActionMsg:
		return f, f.act(msg.ID)
	case tea.KeyPressMsg:
		if e := f.edit(); e != nil {
			e.Take(msg)
			f.sync()
			return f, nil
		}
		fd := f.field()
		if msg.Text == "" || fd == nil || fd.Kind != FieldChoice {
			return f, nil
		}
		// A typed letter jumps to the next choice whose words start with it
		lower := strings.ToLower(msg.Text)
		start := choiceAt(fd)
		for k := 1; k <= len(fd.Choices); k++ {
			c := fd.Choices[(start+k+len(fd.Choices))%len(fd.Choices)]
			if strings.HasPrefix(strings.ToLower(c.Label), lower) {
				fd.Value = c.Value
				return f, f.changed(*fd)
			}
		}
	case tea.PasteMsg, tea.ClipboardMsg:
		if e := f.edit(); e != nil {
			e.Take(msg)
			f.sync()
		}
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return f, f.click(msg.Y)
		}
	case tea.MouseWheelMsg:
		d := 1
		if msg.Button == tea.MouseWheelUp {
			d = -1
		}
		return f, f.moveTo(f.next(f.focus, d))
	}
	return f, nil
}

// Carries out a form action, the caller's own first
func (f *Form) act(id string) tea.Cmd {
	if fn, ok := f.spec.Actions[id]; ok {
		return fn(f)
	}
	if e := f.edit(); e != nil {
		if cmd, ok := e.Action(id); ok {
			f.sync()
			return cmd
		}
	}
	switch id {
	case "form.next":
		return f.moveTo(f.next(f.focus, 1))
	case "form.prev":
		return f.moveTo(f.next(f.focus, -1))
	case "form.submit":
		return f.submit()
	case "form.cancel":
		if f.spec.Cancel != nil {
			return f.spec.Cancel(f)
		}
	case "form.enter":
		if next := f.next(f.focus, 1); f.rows()[next].submit {
			return f.submit()
		}
		return f.moveTo(f.next(f.focus, 1))
	case "form.toggle":
		return f.toggle()
	case "form.choice.next", "form.choice.prev":
		d := 1
		if id == "form.choice.prev" {
			d = -1
		}
		if fd := f.field(); fd != nil {
			switch fd.Kind {
			case FieldChoice:
				f.cycle(fd, d)
				return f.changed(*fd)
			case FieldBool:
				if on := strconv.FormatBool(d > 0); on != fd.Value {
					fd.Value = on
					return f.changed(*fd)
				}
			}
		}
	}
	return nil
}

// Places the cursor on the row painted at line y, a second click changing it
func (f *Form) click(y int) tea.Cmd {
	lines := f.layout(f.w)
	at := y + f.top
	if at < 0 || at >= len(lines) {
		return nil
	}
	if r := lines[at].row; r >= 0 && f.focusable(f.rows()[r]) {
		if r == f.focus {
			return f.toggle()
		}
		return f.moveTo(r)
	}
	return nil
}

// One painted line of a form and the row it belongs to, -1 for the intro and gaps
type formLine struct {
	text string
	row  int
	// Column of the cursor on a field typed into, -1 for none
	cursor int
}

// Width of the label column: the widest label shown
func (f *Form) labelWidth(w int) int {
	n := 0
	for _, r := range f.rows() {
		if r.field >= 0 && f.fields[r.field].Kind != FieldButton && f.fields[r.field].Kind != FieldNote {
			n = max(n, ansi.StringWidth(f.fields[r.field].Label)+2)
		}
	}
	// A label is never cut: its column is as wide as the longest, short of the room a value needs beside it
	return min(n, max(w-12, 12))
}

// Every line of the form at width w
func (f *Form) layout(w int) []formLine {
	t := f.env.Theme
	var out []formLine
	for _, line := range f.spec.Intro {
		for _, part := range wrapWords(line, max(w-2, 8)) {
			out = append(out, formLine{text: " " + render.Dim(part, t), row: -1, cursor: -1})
		}
	}
	if len(out) > 0 {
		out = append(out, formLine{row: -1, cursor: -1})
	}
	lw := f.labelWidth(w)
	vw := max(w-lw-5, 8)
	for i, r := range f.rows() {
		focused := i == f.focus
		mark := "  "
		if focused {
			mark = render.Paint(lipgloss.NewStyle().Foreground(t.Accent), "▍ ")
		}
		switch {
		case r.submit:
			label := " " + f.spec.Submit + " "
			if f.spec.Submit == "" {
				label = " " + lang.T(txt.KitFormSend) + " "
			}
			style := lipgloss.NewStyle().Foreground(t.On(t.SteelBright)).Background(t.SteelBright)
			if focused {
				style = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Accent).Bold(true)
			}
			out = append(out, formLine{row: -1, cursor: -1}, formLine{text: mark + strings.Repeat(" ", lw+1) + render.Paint(style, label), row: i, cursor: -1})
			continue
		case r.more:
			word := lang.T(txt.KitFormMore)
			if f.more {
				word = lang.T(txt.KitFormLess)
			}
			out = append(out, formLine{text: mark + render.Dim(word, t), row: i, cursor: -1})
			continue
		}
		fd := f.fields[r.field]
		if fd.Kind == FieldNote {
			for _, part := range wrapWords(fd.Label, max(w-4, 8)) {
				out = append(out, formLine{text: "  " + render.Dim(part, t), row: i, cursor: -1})
			}
			continue
		}
		if fd.Kind == FieldButton {
			style := lipgloss.NewStyle().Foreground(t.On(t.SteelBright)).Background(t.SteelBright)
			if focused {
				style = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Accent).Bold(true)
			}
			out = append(out, formLine{text: mark + strings.Repeat(" ", lw+1) + render.Paint(style, " "+fd.Label+" "), row: i, cursor: -1})
			if focused && fd.Help != "" {
				for _, part := range wrapWords(fd.Help, vw) {
					out = append(out, formLine{text: strings.Repeat(" ", lw+3) + render.Dim(part, t), row: i, cursor: -1})
				}
			}
			continue
		}
		label := padRight(ansi.Truncate(fd.Label, lw, "…"), lw)
		if focused {
			label = render.Paint(lipgloss.NewStyle().Foreground(t.Text).Bold(true), label)
		} else {
			label = render.Dim(label, t)
		}
		value, cursor := f.valueCell(fd, vw, focused)
		line := formLine{text: mark + label + " " + value, row: i, cursor: -1}
		if cursor >= 0 {
			line.cursor = 2 + lw + 1 + cursor
		}
		out = append(out, line)
		under := ""
		switch {
		case f.errs[fd.ID] != "":
			under = render.Bad(f.errs[fd.ID], t)
		case focused && fd.Kind == FieldChoice:
			if c := choiceAt(&fd); c >= 0 && fd.Choices[c].Meaning != "" {
				under = render.Dim(fd.Choices[c].Meaning, t)
			} else if fd.Help != "" {
				under = dimLines(fd.Help, t)
			}
		case focused && fd.Help != "":
			under = dimLines(fd.Help, t)
		}
		for _, said := range strings.Split(under, "\n") {
			if said == "" {
				continue
			}
			for _, part := range wrapWords(said, vw) {
				out = append(out, formLine{text: strings.Repeat(" ", lw+3) + part, row: i, cursor: -1})
			}
		}
	}
	return out
}

// Text dimmed line by line, so each line of a help of several lines keeps its color once it is split
func dimLines(text string, t theme.Theme) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = render.Dim(line, t)
	}
	return strings.Join(lines, "\n")
}

// Value of a field as one cell vw wide, lit while it has the cursor, with the cursor column inside it for a field typed into
func (f *Form) valueCell(fd Field, vw int, focused bool) (string, int) {
	t := f.env.Theme
	switch fd.Kind {
	case FieldBool:
		box := "[ ] " + lang.T(txt.KitFormNo)
		if fd.Value == "true" {
			box = "[x] " + lang.T(txt.KitFormYes)
		}
		if focused {
			return render.Paint(lipgloss.NewStyle().Foreground(t.Accent), box), -1
		}
		return box, -1
	case FieldChoice:
		label := fd.Value
		if c := choiceAt(&fd); c >= 0 {
			label = fd.Choices[c].Label
		} else if fd.Says != nil && label != "" {
			label = fd.Says(label)
		}
		if label == "" {
			label = render.Dim(fd.Placeholder, t)
		}
		if focused {
			return render.Paint(lipgloss.NewStyle().Foreground(t.Accent), "‹ ") + label + render.Paint(lipgloss.NewStyle().Foreground(t.Accent), " ›"), -1
		}
		return label, -1
	}
	text := fd.Value
	if fd.Kind == FieldSecret {
		text = strings.Repeat("•", len([]rune(text)))
	}
	if !focused {
		if text == "" {
			return render.Dim(fd.Placeholder, t), -1
		}
		return ansi.Truncate(text, vw, "…"), -1
	}
	cell, cursor, selected := text, ansi.StringWidth(text), false
	if e := f.editOf(fd.ID); e != nil {
		cell, cursor = e.Line(vw - 1)
		selected = e.Whole()
	}
	if text == "" && fd.Placeholder != "" {
		cell = render.Dim(ansi.Truncate(fd.Placeholder, vw, "…"), t)
	}
	pad := strings.Repeat(" ", max(vw-ansi.StringWidth(ansi.Strip(cell)), 0))
	box := lipgloss.NewStyle().Background(t.Ghost)
	if selected {
		// Text the next key replaces shows as a selection does
		return lipgloss.NewStyle().Foreground(t.Bg).Background(t.Ice).Render(ansi.Strip(cell)) + box.Render(pad), cursor
	}
	return render.Paint(box, cell+pad), cursor
}

// Line editor of the field with the id, nil when it is not typed into
func (f *Form) editOf(id string) *field.Field {
	for i, fd := range f.fields {
		if fd.ID == id {
			return f.edits[i]
		}
	}
	return nil
}

func (f *Form) View(w, h int) string {
	f.w, f.h = w, h
	lines := f.layout(w)
	// The rows of the field under the cursor stay in view
	first, last := -1, -1
	for i, l := range lines {
		if l.row == f.focus {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first >= 0 {
		if first < f.top {
			f.top = first
		}
		if last >= f.top+h {
			f.top = last - h + 1
		}
	}
	f.top = max(min(f.top, len(lines)-h), 0)
	f.cx = -1
	var out []string
	for i := f.top; i < len(lines) && len(out) < h; i++ {
		if lines[i].cursor >= 0 {
			f.cx, f.cy = lines[i].cursor, len(out)
		}
		out = append(out, render.Fit(lines[i].text, w, f.env.Theme))
	}
	for len(out) < h {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// Rows the form fills at the width
func (f *Form) Rows(w int) int { return len(f.layout(w)) }

func (f *Form) Cursor() *tea.Cursor {
	if f.cx < 0 {
		return nil
	}
	return tea.NewCursor(f.cx, f.cy)
}

// Lines of text broken between words to fit width cells, a word longer than the width cut, the first line keeping its indent and the
// rest led by none
func wrapWords(text string, width int) []string {
	lines := strings.Split(ansi.Wrap(text, width, ""), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = strings.TrimLeft(lines[i], " ")
	}
	return lines
}
