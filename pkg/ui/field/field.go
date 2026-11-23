// Line editor every text field of the toolkit types into, driven by the actions of the text context of the keymap that the prompt answers too
package field

import (
	"slices"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/clip"
)

// Most edits the undo trail of a field keeps
const undoLimit = 100

// Text and caret an undo or redo returns to
type state struct {
	value  []rune
	cursor int
}

// Editable text with a caret, the readline keys of the text context, undo and redo
type Field struct {
	value  []rune
	cursor int
	undo   []state
	redo   []state
	// Whether the whole text reads as selected, the next typing replacing it
	whole bool
	// Whether the paste key asked for the clipboard and its answer is awaited
	pasting bool
	// Whether a line break typed or pasted stays one, the newline action breaking the line
	Multiline bool
	// Whether the text shows as dots
	Masked bool
}

// Field holding text with the caret at its end
func New(text string) *Field {
	f := &Field{}
	f.value = f.clean(text)
	f.cursor = len(f.value)
	return f
}

// Text typed so far
func (f *Field) Value() string { return string(f.value) }

// Whether the field holds no text
func (f *Field) Empty() bool { return len(f.value) == 0 }

// Caret as a rune offset of the text
func (f *Field) Cursor() int { return f.cursor }

// Whether the whole text reads as selected, so typing replaces it
func (f *Field) Whole() bool { return f.whole && len(f.value) > 0 }

// Marks the whole text selected, the next typing replacing it and the next move or delete dropping the mark
func (f *Field) SelectAll() { f.whole = true }

// Replaces the text with the caret at its end, the text it replaced kept on the undo trail
func (f *Field) SetValue(text string) {
	next := f.clean(text)
	if slices.Equal(next, f.value) {
		f.cursor = len(f.value)
		return
	}
	f.record()
	f.value, f.cursor, f.whole = next, len(next), false
}

// Moves the caret to a rune offset of the text
func (f *Field) SetCursor(at int) { f.cursor = min(max(at, 0), len(f.value)) }

// Text with control characters dropped, tabs as spaces and line breaks folded into spaces unless the field takes several lines
func (f *Field) clean(text string) []rune {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if !f.Multiline {
		text = strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", " ")
	}
	text = strings.ReplaceAll(text, "\t", "    ")
	return slices.DeleteFunc([]rune(text), func(r rune) bool { return r != '\n' && unicode.IsControl(r) })
}

// Keeps the text before an edit on the undo trail and drops the redo trail
func (f *Field) record() {
	f.undo = append(f.undo, state{value: slices.Clone(f.value), cursor: f.cursor})
	if len(f.undo) > undoLimit {
		f.undo = f.undo[len(f.undo)-undoLimit:]
	}
	f.redo = nil
}

// Puts text in at the caret as typed, a selected whole text replaced, reporting whether anything went in
func (f *Field) Insert(text string) bool {
	runes := f.clean(text)
	if len(runes) == 0 {
		return false
	}
	f.record()
	if f.Whole() {
		f.value, f.cursor = nil, 0
	}
	f.whole = false
	f.cursor = min(max(f.cursor, 0), len(f.value))
	f.value = slices.Insert(f.value, f.cursor, runes...)
	f.cursor += len(runes)
	return true
}

// Takes typed text, a paste and the clipboard answer the paste key asked for, reporting whether the text changed
func (f *Field) Take(msg tea.Msg) bool {
	switch m := msg.(type) {
	case tea.KeyPressMsg:
		return m.Text != "" && f.Insert(m.Text)
	case tea.PasteMsg:
		return f.Insert(m.Content)
	case tea.ClipboardMsg:
		if !f.pasting {
			return false
		}
		f.pasting = false
		return f.Insert(m.Content)
	}
	return false
}

// Offsets the line the caret is on starts and ends at
func (f *Field) line() (int, int) {
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
func (f *Field) wordLeft() int {
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
func (f *Field) wordRight() int {
	i := f.cursor
	for i < len(f.value) && unicode.IsSpace(f.value[i]) {
		i++
	}
	for i < len(f.value) && !unicode.IsSpace(f.value[i]) {
		i++
	}
	return i
}

// Removes the text between two offsets and leaves the caret where it began, reporting whether anything went
func (f *Field) cut(from, to int) bool {
	if from >= to {
		return false
	}
	f.record()
	f.value = slices.Delete(f.value, from, to)
	f.cursor = from
	return true
}

// Steps back or forward one edit, the text left behind kept on the other trail
func (f *Field) swap(from, to *[]state) bool {
	n := len(*from)
	if n == 0 {
		return false
	}
	s := (*from)[n-1]
	*from = (*from)[:n-1]
	*to = append(*to, state{value: slices.Clone(f.value), cursor: f.cursor})
	f.value, f.cursor = s.value, min(s.cursor, len(s.value))
	return true
}

// Carries out an action of the text context, reporting whether the field answers it, with the command it needs such as a clipboard read
func (f *Field) Action(id string) (tea.Cmd, bool) {
	if !strings.HasPrefix(id, "text.") {
		return nil, false
	}
	f.cursor = min(max(f.cursor, 0), len(f.value))
	whole := f.Whole()
	f.whole = false
	start, end := f.line()
	switch id {
	case "text.left":
		f.cursor = max(f.cursor-1, 0)
		if whole {
			f.cursor = 0
		}
	case "text.right":
		f.cursor = min(f.cursor+1, len(f.value))
	case "text.word.left":
		f.cursor = f.wordLeft()
	case "text.word.right":
		f.cursor = f.wordRight()
	case "text.line.start":
		f.cursor = start
	case "text.line.end":
		f.cursor = end
	case "text.delete.back":
		switch {
		case whole:
			f.cut(0, len(f.value))
		case f.cursor > 0:
			f.cut(f.cursor-1, f.cursor)
		}
	case "text.delete.forward":
		switch {
		case whole:
			f.cut(0, len(f.value))
		case f.cursor < len(f.value):
			f.cut(f.cursor, f.cursor+1)
		}
	case "text.delete.word.back":
		f.cut(f.wordLeft(), f.cursor)
	case "text.delete.word.forward":
		f.cut(f.cursor, f.wordRight())
	case "text.delete.line.before":
		f.cut(start, f.cursor)
	case "text.delete.line.after":
		f.cut(f.cursor, end)
	case "text.undo":
		f.swap(&f.undo, &f.redo)
	case "text.redo":
		f.swap(&f.redo, &f.undo)
	case "text.newline":
		if !f.Multiline {
			return nil, false
		}
		f.record()
		f.value = slices.Insert(f.value, f.cursor, '\n')
		f.cursor++
	case "text.select.all":
		f.whole = true
	case "text.copy":
		if !whole || f.Masked {
			return nil, false
		}
		f.whole = true
		return clip.Copy(f.Value()), true
	case "text.paste":
		f.pasting = true
		return tea.ReadClipboard, true
	default:
		return nil, false
	}
	return nil, true
}

// Moves the caret a line up or down keeping its column, false when the text has no line there
func (f *Field) Vertical(step int) bool {
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

// Cell a rune of the text shows as, a dot while masked
func (f *Field) cell(r rune) string {
	if f.Masked {
		return "•"
	}
	return string(r)
}

// One row of the text in width cells scrolled to keep the caret in view, the caret column in it
func (f *Field) Line(width int) (string, int) {
	if width <= 0 {
		return "", 0
	}
	shown := make([]string, len(f.value))
	for i, r := range f.value {
		shown[i] = f.cell(r)
		if r == '\n' {
			shown[i] = "⏎"
		}
	}
	cursor := min(f.cursor, len(shown))
	from := 0
	for from < cursor && ansi.StringWidth(strings.Join(shown[from:cursor], "")) >= width {
		from++
	}
	caret := ansi.StringWidth(strings.Join(shown[from:cursor], ""))
	return ansi.Truncate(strings.Join(shown[from:], ""), width, ""), caret
}

// Text as rows of at most room cells, broken at line breaks and wrapped, with the column and row of the caret
func (f *Field) Wrapped(room int) (lines []string, cx, cy int) {
	room = max(room, 1)
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
		cell := f.cell(r)
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
