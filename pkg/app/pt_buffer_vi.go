package app

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ptSearchDirection is which way a buffer search walks
type ptSearchDirection int

// Search directions
const (
	ptSearchForward ptSearchDirection = iota
	ptSearchBackward
)

// ptSearchState is a search query with its direction and case rule
type ptSearchState struct {
	text       string
	direction  ptSearchDirection
	ignoreCase bool
}

// Same query searching the other way
func (s ptSearchState) inverted() ptSearchState {
	if s.direction == ptSearchBackward {
		s.direction = ptSearchForward
	} else {
		s.direction = ptSearchBackward
	}
	return s
}

// Panics with the read only edit error when the buffer refuses edits
func (b *ptBuffer) requireWritable() {
	if b.isReadOnly() {
		panic(ptEditReadOnly{})
	}
}

// Replaces text and cursor, a read only buffer raising the edit error
func (b *ptBuffer) mustSetDocument(text string, cursor int) {
	if err := b.setDocument(text, cursor, false); err != nil {
		panic(ptEditReadOnly{})
	}
}

// Replaces the text, a read only buffer raising the edit error
func (b *ptBuffer) mustSetText(text string) {
	if err := b.setText(text); err != nil {
		panic(ptEditReadOnly{})
	}
}

// Deletes count characters after the cursor, raising the edit error on a read only buffer with text after the cursor
func (b *ptBuffer) deleteRaising(count int) string {
	if b.cursor < len([]rune(b.text())) {
		b.requireWritable()
	}
	return b.delete(count)
}

// Deletes count characters before the cursor, raising the edit error on a read only buffer past its start
func (b *ptBuffer) deleteBeforeCursorRaising(count int) string {
	if b.cursor > 0 {
		b.requireWritable()
	}
	return b.deleteBeforeCursor(count)
}

// Restores the newest saved state whose text differs, raising the edit error when the buffer is read only
func (b *ptBuffer) undoRaising() {
	for len(b.undoStack) > 0 {
		entry := b.undoStack[len(b.undoStack)-1]
		b.undoStack = b.undoStack[:len(b.undoStack)-1]
		if entry.text != b.text() {
			b.redoStack = append(b.redoStack, ptUndoEntry{b.text(), b.cursor})
			b.mustSetDocument(entry.text, entry.cursor)
			return
		}
	}
}

// Up a line, else back in history, going to the line start when the history entry changed and asked
func (b *ptBuffer) autoUp(count int, goToStartOfLineIfHistoryChanges bool) {
	switch {
	case b.completeState != nil:
		b.completePrevious(count)
	case b.document().cursorRow() > 0:
		b.cursorUp(count)
	case b.selection == nil:
		b.historyBackward(count)
		if goToStartOfLineIfHistoryChanges {
			b.setCursorPosition(b.cursor + b.document().startOfLine(false))
		}
	}
}

// Down a line, else forward in history, going to the line start when the history entry changed and asked
func (b *ptBuffer) autoDown(count int, goToStartOfLineIfHistoryChanges bool) {
	doc := b.document()
	switch {
	case b.completeState != nil:
		b.completeNext(count)
	case doc.cursorRow() < doc.lineCount()-1:
		b.cursorDown(count)
	case b.selection == nil:
		b.historyForward(count)
		if goToStartOfLineIfHistoryChanges {
			b.setCursorPosition(b.cursor + b.document().startOfLine(false))
		}
	}
}

// Joins the next line onto the cursor line with the separator, its leading spaces dropped
func (b *ptBuffer) joinNextLine(separator string) {
	doc := b.document()
	if doc.cursorRow() == doc.lineCount()-1 {
		return
	}
	b.setCursorPosition(b.cursor + doc.endOfLinePosition())
	b.requireWritable()
	b.delete(1)
	doc = b.document()
	b.mustSetText(doc.textBeforeCursor() + separator + strings.TrimLeft(doc.textAfterCursor(), " "))
}

// Joins the selected lines with the separator, each line losing its leading spaces
func (b *ptBuffer) joinSelectedLines(separator string) {
	from, to := b.document().sortedSelection()
	r := []rune(b.text())
	before := string(r[:from])
	after := string(r[to:])
	lines := pystr.Splitlines(string(r[from:to]), false)
	joined := make([]string, len(lines))
	for i, l := range lines {
		joined[i] = strings.TrimLeft(l, " ") + separator
	}
	head := ""
	if len(joined) > 0 {
		head = strings.Join(joined[:len(joined)-1], "")
	}
	b.mustSetDocument(before+strings.Join(joined, "")+after, len([]rune(before+head))-1)
}

// Text with the callback applied to every line whose index the iterator yields, others untouched
func (b *ptBuffer) transformLines(indexes []int, transform func(string) string) string {
	lines := strings.Split(b.text(), "\n")
	for _, index := range indexes {
		if index < 0 {
			index += len(lines)
		}
		if index >= 0 && index < len(lines) {
			lines[index] = transform(lines[index])
		}
	}
	return strings.Join(lines, "\n")
}

// Applies the callback to the cursor line
func (b *ptBuffer) transformCurrentLine(transform func(string) string) {
	doc := b.document()
	a := doc.cursor + doc.startOfLine(false)
	e := doc.cursor + doc.endOfLinePosition()
	b.mustSetText(string(doc.runes[:a]) + transform(string(doc.runes[a:e])) + string(doc.runes[e:]))
}

// Applies the callback to the text between from and to
func (b *ptBuffer) transformRegion(from, to int, transform func(string) string) {
	r := []rune(b.text())
	b.mustSetText(ptRuneSlice(r, 0, from) + transform(ptRuneSlice(r, from, to)) + ptRuneSlice(r, to, len(r)))
}

// Opens a line above the cursor line, copying its indentation when asked
func (b *ptBuffer) insertLineAbove(copyMargin bool) {
	insert := "\n"
	if copyMargin {
		insert = b.document().leadingWhitespaceInCurrentLine() + "\n"
	}
	b.setCursorPosition(b.cursor + b.document().startOfLine(false))
	b.requireWritable()
	b.insertText(insert, false, true, true)
	b.setCursorPosition(b.cursor - 1)
}

// Opens a line below the cursor line, copying its indentation when asked
func (b *ptBuffer) insertLineBelow(copyMargin bool) {
	insert := "\n"
	if copyMargin {
		insert = "\n" + b.document().leadingWhitespaceInCurrentLine()
	}
	b.setCursorPosition(b.cursor + b.document().endOfLinePosition())
	b.requireWritable()
	b.insertText(insert, false, true, true)
}

// History entry and cursor the search lands on, false when the text is not found
func (b *ptBuffer) search(state ptSearchState, includeCurrentPosition bool, count int) (int, int, bool) {
	once := func(workingIndex int, doc ptDocument) (int, ptDocument, bool) {
		n := len(b.workingLines)
		if state.direction == ptSearchForward {
			if pos, ok := doc.find(state.text, false, includeCurrentPosition, state.ignoreCase, 1); ok {
				return workingIndex, newPTDocument(doc.text(), doc.cursor+pos), true
			}
			for i := workingIndex + 1; i < n+1; i++ {
				i := i % n
				d := newPTDocument(b.workingLines[i], 0)
				if pos, ok := d.find(state.text, false, true, state.ignoreCase, 1); ok {
					return i, newPTDocument(d.text(), pos), true
				}
			}
			return 0, ptDocument{}, false
		}
		if pos, ok := doc.findBackwards(state.text, false, state.ignoreCase, 1); ok {
			return workingIndex, newPTDocument(doc.text(), doc.cursor+pos), true
		}
		for i := workingIndex - 1; i > -2; i-- {
			i := ((i % n) + n) % n
			line := b.workingLines[i]
			d := newPTDocument(line, len([]rune(line)))
			if pos, ok := d.findBackwards(state.text, false, state.ignoreCase, 1); ok {
				return i, newPTDocument(d.text(), len([]rune(line))+pos), true
			}
		}
		return 0, ptDocument{}, false
	}
	workingIndex, doc := b.workingIndex, b.document()
	for range count {
		index, next, ok := once(workingIndex, doc)
		if !ok {
			return 0, 0, false
		}
		workingIndex, doc = index, next
	}
	return workingIndex, doc.cursor, true
}

// Cursor position the search lands on in the current entry, the cursor itself when not found
func (b *ptBuffer) getSearchPosition(state ptSearchState, includeCurrentPosition bool, count int) int {
	_, cursor, ok := b.search(state, includeCurrentPosition, count)
	if !ok {
		return b.cursor
	}
	return cursor
}

// Moves to the history entry and cursor the search lands on when it finds the text
func (b *ptBuffer) applySearch(state ptSearchState, includeCurrentPosition bool, count int) {
	workingIndex, cursor, ok := b.search(state, includeCurrentPosition, count)
	if !ok {
		return
	}
	b.setWorkingIndex(workingIndex)
	b.setCursorPosition(cursor)
}

// Completes the cursor line from the other lines of every history entry, newest first
func (b *ptBuffer) startHistoryLinesCompletion() {
	found := map[string]bool{}
	var completions []ptCompletion
	current := strings.TrimLeftFunc(string(b.document().currentLineBeforeCursor()), pystr.IsSpace)
	for i, entry := range b.workingLines {
		for j, l := range strings.Split(entry, "\n") {
			l = pystr.Strip(l)
			if l == "" || !strings.HasPrefix(l, current) || found[l] {
				continue
			}
			found[l] = true
			meta := fmt.Sprintf("History %d, line %d", i+1, j+1)
			if i == b.workingIndex {
				meta = fmt.Sprintf("Current, line %d", j+1)
			}
			completions = append(completions, ptCompletion{Text: l, Start: -len([]rune(current)), Meta: meta})
		}
	}
	for i, j := 0, len(completions)-1; i < j; i, j = i+1, j-1 {
		completions[i], completions[j] = completions[j], completions[i]
	}
	b.completeState = &ptCompletionState{original: b.document(), completions: completions}
	b.changed()
	zero := 0
	b.goToCompletion(&zero)
}

// Range of rows from start up to but not including end
func ptRowRange(from, to int) []int {
	var out []int
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// Indents rows from up to to by four spaces per count, the cursor keeping its column in the text
func ptIndentRows(b *ptBuffer, fromRow, toRow, count int) {
	doc := b.document()
	row, col := doc.cursorRow(), doc.cursorCol()
	content := strings.Repeat("    ", max(count, 0))
	text := b.transformLines(ptRowRange(fromRow, toRow), func(l string) string { return content + l })
	b.mustSetDocument(text, newPTDocument(text, 0).translateRowColToIndex(row, 0))
	b.setCursorPosition(b.cursor + col + len(content))
}

// Removes four spaces per count from rows, stripping all leading blanks from a row that lacks them
func ptUnindentRows(b *ptBuffer, fromRow, toRow, count int) {
	doc := b.document()
	row, col := doc.cursorRow(), doc.cursorCol()
	content := strings.Repeat("    ", max(count, 0))
	text := b.transformLines(ptRowRange(fromRow, toRow), func(l string) string {
		if strings.HasPrefix(l, content) {
			return l[len(content):]
		}
		return strings.TrimLeftFunc(l, pystr.IsSpace)
	})
	b.mustSetDocument(text, newPTDocument(text, 0).translateRowColToIndex(row, 0))
	b.setCursorPosition(b.cursor + col - len(content))
}

var ptLeadingWhitespaceRe = regexp.MustCompile(`^[` + ptPyWhitespace + `]*`)

// Rewraps rows from through to at eighty columns less the indent of the first, the vi gq operator
func ptReshapeText(b *ptBuffer, fromRow, toRow int) {
	lines := pystr.Splitlines(b.text(), true)
	clamp := func(i int) int { return min(max(i, 0), len(lines)) }
	before := lines[:clamp(fromRow)]
	after := lines[clamp(toRow+1):]
	reformat := lines[clamp(fromRow):max(clamp(fromRow), clamp(toRow+1))]
	if len(reformat) == 0 {
		return
	}
	loc := ptLeadingWhitespaceRe.FindStringIndex(reformat[0])
	indent := strings.ReplaceAll(reformat[0][:loc[1]], "\n", "")
	words := pystr.SplitWhitespace(strings.Join(reformat, ""), -1)
	width := 80 - len([]rune(indent))
	reshaped := []string{indent}
	current := 0
	for _, w := range words {
		if current != 0 {
			if len([]rune(w))+current+1 > width {
				reshaped = append(reshaped, "\n", indent)
				current = 0
			} else {
				reshaped = append(reshaped, " ")
				current++
			}
		}
		reshaped = append(reshaped, w)
		current += len([]rune(w))
	}
	if reshaped[len(reshaped)-1] != "\n" {
		reshaped = append(reshaped, "\n")
	}
	head := strings.Join(before, "") + strings.Join(reshaped, "")
	b.mustSetDocument(head+strings.Join(after, ""), len([]rune(head)))
}
