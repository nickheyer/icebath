package app

import (
	"bufio"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Characters Python str.isspace and the re module whitespace class accept
const ptPyWhitespace = `\t\n\v\f\r\x{1c}-\x{1f}\x{85}\p{Z}`

var (
	ptFindWordRe    = regexp.MustCompile(`([a-zA-Z0-9_]+|[^a-zA-Z0-9_` + ptPyWhitespace + `]+)`)
	ptFindBigWordRe = regexp.MustCompile(`([^` + ptPyWhitespace + `]+)`)
	ptQuotedWordsRe = regexp.MustCompile(`(\s+|".*?"|'.*?')`)
)

// errEditReadOnlyBuffer reports an edit attempted while the buffer is read only
var errEditReadOnlyBuffer = errors.New("EditReadOnlyBuffer")

// ptDocument is an immutable text with a cursor, positions counted in code points
type ptDocument struct {
	runes     []rune
	cursor    int
	selection *ptSelection
	vi        bool
}

// ptSelection marks where a selection started and whether shift movement drives it
type ptSelection struct {
	original  int
	typ       ptSelectionType
	shiftMode bool
}

// Builds a document, the cursor clamped into the text
func newPTDocument(text string, cursor int) ptDocument {
	r := []rune(text)
	return ptDocument{runes: r, cursor: max(0, min(cursor, len(r)))}
}

func (d ptDocument) text() string             { return string(d.runes) }
func (d ptDocument) textBeforeCursor() string { return string(d.runes[:d.cursor]) }
func (d ptDocument) textAfterCursor() string  { return string(d.runes[d.cursor:]) }

// Reports whether two documents hold the same text and cursor
func (d ptDocument) equal(o ptDocument) bool {
	return d.cursor == o.cursor && string(d.runes) == string(o.runes)
}

// Lines of the text split on newlines
func (d ptDocument) lines() []string { return strings.Split(string(d.runes), "\n") }

// Row of the cursor and the index its line starts at
func (d ptDocument) lineStart(index int) (row, start int) {
	for i := 0; i < index && i < len(d.runes); i++ {
		if d.runes[i] == '\n' {
			row++
			start = i + 1
		}
	}
	return row, start
}

func (d ptDocument) cursorRow() int { row, _ := d.lineStart(d.cursor); return row }
func (d ptDocument) cursorCol() int { _, start := d.lineStart(d.cursor); return d.cursor - start }
func (d ptDocument) lineCount() int { return strings.Count(string(d.runes), "\n") + 1 }
func (d ptDocument) atEnd() bool    { return d.cursor == len(d.runes) }

// Character under the cursor, zero at the end of the text
func (d ptDocument) currentChar() rune {
	if d.cursor < len(d.runes) {
		return d.runes[d.cursor]
	}
	return 0
}

// Text of the cursor line left of the cursor
func (d ptDocument) currentLineBeforeCursor() []rune {
	_, start := d.lineStart(d.cursor)
	return d.runes[start:d.cursor]
}

// Text of the cursor line right of the cursor
func (d ptDocument) currentLineAfterCursor() []rune {
	end := d.cursor
	for end < len(d.runes) && d.runes[end] != '\n' {
		end++
	}
	return d.runes[d.cursor:end]
}

// Whole line the cursor is on
func (d ptDocument) currentLine() string {
	return string(d.currentLineBeforeCursor()) + string(d.currentLineAfterCursor())
}

// Leading blanks of the cursor line
func (d ptDocument) leadingWhitespaceInCurrentLine() string {
	line := d.currentLine()
	return line[:len(line)-len(strings.TrimLeftFunc(line, unicode.IsSpace))]
}

// Index of a row and column, both clamped into the text
func (d ptDocument) rowColToIndex(row, col int) int {
	lines := d.lines()
	if row < 0 {
		row = 0
	}
	if row >= len(lines) {
		row = len(lines) - 1
	}
	result := 0
	for i := 0; i < row; i++ {
		result += len([]rune(lines[i])) + 1
	}
	result += max(0, min(col, len([]rune(lines[row]))))
	return max(0, min(result, len(d.runes)))
}

func (d ptDocument) startOfLinePosition() int { return -len(d.currentLineBeforeCursor()) }
func (d ptDocument) endOfLinePosition() int   { return len(d.currentLineAfterCursor()) }

// Offset to the first non blank of the cursor line
func (d ptDocument) startOfLineAfterWhitespace() int {
	line := d.currentLine()
	return len([]rune(line)) - len([]rune(strings.TrimLeftFunc(line, unicode.IsSpace))) - d.cursorCol()
}

// Offset moving count characters left within the line
func (d ptDocument) cursorLeftPosition(count int) int {
	if count < 0 {
		return d.cursorRightPosition(-count)
	}
	return -min(d.cursorCol(), count)
}

// Offset moving count characters right within the line
func (d ptDocument) cursorRightPosition(count int) int {
	if count < 0 {
		return d.cursorLeftPosition(-count)
	}
	return min(count, len(d.currentLineAfterCursor()))
}

// Offset moving count rows up keeping the preferred column
func (d ptDocument) cursorUpPosition(count int, preferred *int) int {
	col := d.cursorCol()
	if preferred != nil {
		col = *preferred
	}
	return d.rowColToIndex(max(0, d.cursorRow()-count), col) - d.cursor
}

// Offset moving count rows down keeping the preferred column
func (d ptDocument) cursorDownPosition(count int, preferred *int) int {
	col := d.cursorCol()
	if preferred != nil {
		col = *preferred
	}
	return d.rowColToIndex(d.cursorRow()+count, col) - d.cursor
}

// Reversed copy of the runes
func reverseRunes(r []rune) []rune {
	out := make([]rune, len(r))
	for i, c := range r {
		out[len(r)-1-i] = c
	}
	return out
}

// Rune index spans of every match of the pattern in the runes
func runeMatches(re *regexp.Regexp, r []rune) [][2]int {
	s := string(r)
	var out [][2]int
	for _, loc := range re.FindAllStringIndex(s, -1) {
		out = append(out, [2]int{len([]rune(s[:loc[0]])), len([]rune(s[:loc[1]]))})
	}
	return out
}

// Offset to the start of the count-th word before the cursor, ok false when there is none
func (d ptDocument) findStartOfPreviousWord(count int, bigWord bool) (int, bool) {
	re := ptFindWordRe
	if bigWord {
		re = ptFindBigWordRe
	}
	for i, m := range runeMatches(re, reverseRunes(d.runes[:d.cursor])) {
		if i+1 == count {
			return -m[1], true
		}
	}
	return 0, false
}

// Offset to the start of the count-th word after the cursor, ok false when there is none
func (d ptDocument) findNextWordBeginning(count int) (int, bool) {
	if count < 0 {
		return d.findPreviousWordBeginning(-count)
	}
	for i, m := range runeMatches(ptFindWordRe, d.runes[d.cursor:]) {
		if i == 0 && m[0] == 0 {
			count++
		}
		if i+1 == count {
			return m[0], true
		}
	}
	return 0, false
}

// Offset to the end of the count-th word after the cursor, ok false when there is none
func (d ptDocument) findNextWordEnding(count int) (int, bool) {
	if count < 0 {
		return d.findPreviousWordEnding(-count)
	}
	after := d.runes[d.cursor:]
	if len(after) > 0 {
		after = after[1:]
	}
	for i, m := range runeMatches(ptFindWordRe, after) {
		if i+1 == count {
			return m[1] + 1, true
		}
	}
	return 0, false
}

// Offset to the start of the count-th word before the cursor, ok false when there is none
func (d ptDocument) findPreviousWordBeginning(count int) (int, bool) {
	if count < 0 {
		return d.findNextWordBeginning(-count)
	}
	for i, m := range runeMatches(ptFindWordRe, reverseRunes(d.runes[:d.cursor])) {
		if i+1 == count {
			return -m[1], true
		}
	}
	return 0, false
}

// Offset to the end of the count-th word before the cursor, ok false when there is none
func (d ptDocument) findPreviousWordEnding(count int) (int, bool) {
	if count < 0 {
		return d.findNextWordEnding(-count)
	}
	var text []rune
	if d.cursor < len(d.runes) {
		text = append(text, d.runes[d.cursor])
	}
	text = append(text, reverseRunes(d.runes[:d.cursor])...)
	for i, m := range runeMatches(ptFindWordRe, text) {
		if i == 0 && m[0] == 0 {
			count++
		}
		if i+1 == count {
			return -m[0] + 1, true
		}
	}
	return 0, false
}

// Offset of the count-th occurrence of char on the cursor line, forward or backward
func (d ptDocument) findChar(char string, count int, backwards bool) (int, bool) {
	needle := []rune(char)
	if len(needle) != 1 || count < 1 {
		return 0, false
	}
	seen := 0
	if backwards {
		before := d.currentLineBeforeCursor()
		for i := len(before) - 1; i >= 0; i-- {
			if before[i] == needle[0] {
				seen++
				if seen == count {
					return i - len(before), true
				}
			}
		}
		return 0, false
	}
	after := d.currentLineAfterCursor()
	for i := 1; i < len(after); i++ {
		if after[i] == needle[0] {
			seen++
			if seen == count {
				return i, true
			}
		}
	}
	return 0, false
}

// Start and end of the selection, the cursor twice when nothing is selected
func (d ptDocument) selectionRange() (int, int) {
	if d.selection == nil {
		return d.cursor, d.cursor
	}
	a, b := d.cursor, d.selection.original
	if a > b {
		a, b = b, a
	}
	return a, b
}

// ptCompletion is one completion replacing start characters before the cursor
type ptCompletion struct {
	Text    string
	Start   int
	Display string
	Meta    string
}

// ptCompletionState is the open completion menu of a buffer
type ptCompletionState struct {
	original    ptDocument
	completions []ptCompletion
	index       *int
}

// Selected completion, nil when none is selected
func (s *ptCompletionState) current() *ptCompletion {
	if s.index == nil {
		return nil
	}
	return &s.completions[*s.index]
}

// Text and cursor the buffer shows for the selected completion
func (s *ptCompletionState) newTextAndPosition() (string, int) {
	if s.index == nil {
		return s.original.text(), s.original.cursor
	}
	c := s.completions[*s.index]
	before := s.original.runes[:s.original.cursor]
	if c.Start != 0 {
		before = before[:max(0, len(before)+c.Start)]
	}
	text := string(before) + c.Text + s.original.textAfterCursor()
	return text, len(before) + len([]rune(c.Text))
}

// ptHistory is the prompt history, newest entry last
type ptHistory interface {
	strings() []string
	appendString(s string)
}

// ptInMemoryHistory keeps history for the life of the process
type ptInMemoryHistory struct {
	mu      sync.Mutex
	entries []string
}

func (h *ptInMemoryHistory) strings() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.entries...)
}

func (h *ptInMemoryHistory) appendString(s string) {
	h.mu.Lock()
	h.entries = append(h.entries, s)
	h.mu.Unlock()
}

// ptFileHistory stores history in the prompt_toolkit file format
type ptFileHistory struct {
	ptInMemoryHistory
	filename string
	loaded   bool
}

// Opens a history file, entries read on first use
func newPTFileHistory(filename string) *ptFileHistory { return &ptFileHistory{filename: filename} }

// Reads the entries of the history file, oldest first
func (h *ptFileHistory) load() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loaded {
		return
	}
	h.loaded = true
	f, err := os.Open(h.filename)
	if err != nil {
		return
	}
	defer f.Close()
	var lines []string
	add := func() {
		if len(lines) > 0 {
			joined := strings.Join(lines, "")
			h.entries = append(h.entries, strings.TrimSuffix(joined, "\n"))
		}
	}
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.ToValidUTF8(line, "�")
			if strings.HasPrefix(line, "+") {
				lines = append(lines, line[1:])
			} else {
				add()
				lines = nil
			}
		}
		if err != nil {
			break
		}
	}
	add()
}

func (h *ptFileHistory) strings() []string {
	h.load()
	return h.ptInMemoryHistory.strings()
}

// Appends an entry in memory and to the history file
func (h *ptFileHistory) appendString(s string) {
	h.load()
	h.ptInMemoryHistory.appendString(s)
	f, err := os.OpenFile(h.filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	var b strings.Builder
	b.WriteString("\n# " + time.Now().Format("2006-01-02 15:04:05.000000") + "\n")
	for _, line := range strings.Split(s, "\n") {
		b.WriteString("+" + line + "\n")
	}
	_, _ = f.WriteString(b.String())
}

// ptClipboard is the kill ring shared by the editing commands
type ptClipboard struct {
	ring []ptClipboardData
}

// Pushes an entry as the newest kill, the ring capped at sixty entries
func (c *ptClipboard) setData(data ptClipboardData) {
	c.ring = append([]ptClipboardData{data}, c.ring...)
	if len(c.ring) > 60 {
		c.ring = c.ring[:60]
	}
}

// Pushes text as the newest character kill
func (c *ptClipboard) setText(text string) { c.setData(ptClipboardData{text: text}) }

// Newest kill, an empty character entry when the ring is empty
func (c *ptClipboard) getData() ptClipboardData {
	if len(c.ring) == 0 {
		return ptClipboardData{}
	}
	return c.ring[0]
}

// Text of the newest kill
func (c *ptClipboard) text() string { return c.getData().text }

// Moves the newest kill to the back of the ring
func (c *ptClipboard) rotate() {
	if len(c.ring) > 0 {
		c.ring = append(c.ring[1:], c.ring[0])
	}
}

// ptYankNthArgState tracks repeated yanks of an argument from history
type ptYankNthArgState struct {
	historyPosition int
	n               int
	previousWord    string
}

// ptUndoEntry is one saved text and cursor
type ptUndoEntry struct {
	text   string
	cursor int
}

// ptBuffer is the editable prompt text with history, completion, suggestion, selection and undo
type ptBuffer struct {
	history             ptHistory
	workingLines        []string
	workingIndex        int
	cursor              int
	selection           *ptSelection
	preferredColumn     *int
	completeState       *ptCompletionState
	yankNthArgState     *ptYankNthArgState
	documentBeforePaste *ptDocument
	suggestion          *string
	undoStack           []ptUndoEntry
	redoStack           []ptUndoEntry
	readOnly            func() bool
	completer           func(textBeforeCursor string) []ptCompletion
	completeWhileTyping bool
	suggester           func(b *ptBuffer, doc ptDocument) (string, bool)
	onTextChanged       []func(b *ptBuffer)
	onChange            func()
	spawn               func(work func() func())
	tempfileSuffix      string
	editingVi           func() bool
	multipleCursors     []int
	acceptHandler       func(b *ptBuffer) bool
	multiline           bool
	completeGen         int
	suggestGen          int
}

// Builds a buffer over a history, in memory history when nil
func newPTBuffer(history ptHistory) *ptBuffer {
	if history == nil {
		history = &ptInMemoryHistory{}
	}
	b := &ptBuffer{history: history, multiline: true}
	b.reset(false)
	return b
}

// Current text
func (b *ptBuffer) text() string { return b.workingLines[b.workingIndex] }

// Current text and cursor as a document
func (b *ptBuffer) document() ptDocument {
	d := newPTDocument(b.text(), b.cursor)
	d.selection = b.selection
	d.vi = b.editingVi != nil && b.editingVi()
	return d
}

// Reports whether edits are refused right now
func (b *ptBuffer) isReadOnly() bool { return b.readOnly != nil && b.readOnly() }

// Clears the text and every editing state, optionally recording the text in history first
func (b *ptBuffer) reset(appendToHistory bool) {
	if appendToHistory {
		b.appendToHistory()
	}
	b.cursor = 0
	b.selection, b.preferredColumn, b.completeState, b.yankNthArgState = nil, nil, nil, nil
	b.documentBeforePaste, b.suggestion, b.multipleCursors = nil, nil, nil
	b.undoStack, b.redoStack = nil, nil
	b.workingLines = append(b.history.strings(), "")
	b.workingIndex = len(b.workingLines) - 1
	b.changed()
}

// Tells the owner the buffer needs a repaint
func (b *ptBuffer) changed() {
	if b.onChange != nil {
		b.onChange()
	}
}

// Records the text as the newest history entry unless it repeats the last one
func (b *ptBuffer) appendToHistory() {
	text := b.text()
	if text == "" {
		return
	}
	entries := b.history.strings()
	if len(entries) == 0 || entries[len(entries)-1] != text {
		b.history.appendString(text)
	}
}

// Stores text without firing events, reporting whether it differed
func (b *ptBuffer) setTextRaw(text string) bool {
	changed := b.workingLines[b.workingIndex] != text
	b.workingLines[b.workingIndex] = text
	return changed
}

// Stores the cursor without firing events, reporting whether it moved
func (b *ptBuffer) setCursorRaw(pos int) bool {
	pos = max(0, min(pos, len([]rune(b.text()))))
	changed := b.cursor != pos
	b.cursor = pos
	return changed
}

// Drops text bound state and notifies text change listeners
func (b *ptBuffer) textChanged() {
	b.completeState, b.yankNthArgState, b.documentBeforePaste = nil, nil, nil
	b.selection, b.suggestion, b.preferredColumn = nil, nil, nil
	for _, fn := range b.onTextChanged {
		fn(b)
	}
	b.changed()
}

// Drops cursor bound state
func (b *ptBuffer) cursorPositionChanged() {
	b.completeState, b.yankNthArgState, b.documentBeforePaste = nil, nil, nil
	b.preferredColumn = nil
	b.changed()
}

// Replaces the text, the cursor pulled back inside it
func (b *ptBuffer) setText(text string) error {
	if b.cursor > len([]rune(text)) {
		b.cursor = len([]rune(text))
	}
	if b.isReadOnly() {
		return errEditReadOnlyBuffer
	}
	if b.setTextRaw(text) {
		b.textChanged()
	}
	return nil
}

// Moves the cursor, clamped into the text
func (b *ptBuffer) setCursorPosition(pos int) {
	if b.setCursorRaw(pos) {
		b.cursorPositionChanged()
	}
}

// Replaces text and cursor together
func (b *ptBuffer) setDocument(text string, cursor int, bypassReadOnly bool) error {
	if !bypassReadOnly && b.isReadOnly() {
		return errEditReadOnlyBuffer
	}
	textChanged := b.setTextRaw(text)
	cursorChanged := b.setCursorRaw(cursor)
	if textChanged {
		b.textChanged()
	}
	if cursorChanged {
		b.cursorPositionChanged()
	}
	return nil
}

// Switches to another history line, the cursor reset to its start
func (b *ptBuffer) setWorkingIndex(index int) {
	if b.workingIndex != index {
		b.workingIndex = index
		b.setCursorPosition(0)
		b.textChanged()
	}
}

// Saves text and cursor for undo
func (b *ptBuffer) saveToUndoStack(clearRedo bool) {
	if n := len(b.undoStack); n > 0 && b.undoStack[n-1].text == b.text() {
		b.undoStack[n-1].cursor = b.cursor
	} else {
		b.undoStack = append(b.undoStack, ptUndoEntry{b.text(), b.cursor})
	}
	if clearRedo {
		b.redoStack = nil
	}
}

// Restores the newest saved state whose text differs
func (b *ptBuffer) undo() {
	for len(b.undoStack) > 0 {
		entry := b.undoStack[len(b.undoStack)-1]
		b.undoStack = b.undoStack[:len(b.undoStack)-1]
		if entry.text != b.text() {
			b.redoStack = append(b.redoStack, ptUndoEntry{b.text(), b.cursor})
			_ = b.setDocument(entry.text, entry.cursor, false)
			break
		}
	}
}

// Reapplies the newest undone state
func (b *ptBuffer) redo() {
	if len(b.redoStack) == 0 {
		return
	}
	b.saveToUndoStack(false)
	entry := b.redoStack[len(b.redoStack)-1]
	b.redoStack = b.redoStack[:len(b.redoStack)-1]
	_ = b.setDocument(entry.text, entry.cursor, false)
}

func (b *ptBuffer) cursorLeft(count int) {
	b.setCursorPosition(b.cursor + b.document().cursorLeftPosition(count))
}
func (b *ptBuffer) cursorRight(count int) {
	b.setCursorPosition(b.cursor + b.document().cursorRightPosition(count))
}

// Moves the cursor up keeping its preferred column
func (b *ptBuffer) cursorUp(count int) {
	doc := b.document()
	col := doc.cursorCol()
	if b.preferredColumn != nil && *b.preferredColumn != 0 {
		col = *b.preferredColumn
	}
	b.setCursorPosition(b.cursor + doc.cursorUpPosition(count, &col))
	b.preferredColumn = &col
}

// Moves the cursor down keeping its preferred column
func (b *ptBuffer) cursorDown(count int) {
	doc := b.document()
	col := doc.cursorCol()
	if b.preferredColumn != nil && *b.preferredColumn != 0 {
		col = *b.preferredColumn
	}
	b.setCursorPosition(b.cursor + doc.cursorDownPosition(count, &col))
	b.preferredColumn = &col
}

// Up through completions, then lines, then history
func (b *ptBuffer) autoMoveUp(count int) {
	switch {
	case b.completeState != nil:
		b.completePrevious(count)
	case b.document().cursorRow() > 0:
		b.cursorUp(count)
	case b.selection == nil:
		b.historyBackward(count)
	}
}

// Down through completions, then lines, then history
func (b *ptBuffer) autoMoveDown(count int) {
	doc := b.document()
	switch {
	case b.completeState != nil:
		b.completeNext(count)
	case doc.cursorRow() < doc.lineCount()-1:
		b.cursorDown(count)
	case b.selection == nil:
		b.historyForward(count)
	}
}

// Deletes count characters before the cursor and returns them
func (b *ptBuffer) deleteBeforeCursor(count int) string {
	if b.cursor <= 0 || count <= 0 {
		return ""
	}
	r := []rune(b.text())
	start := max(0, b.cursor-count)
	deleted := string(r[start:b.cursor])
	if b.setDocument(string(r[:start])+string(r[b.cursor:]), start, false) != nil {
		return ""
	}
	return deleted
}

// Deletes count characters after the cursor and returns them
func (b *ptBuffer) delete(count int) string {
	r := []rune(b.text())
	if b.cursor >= len(r) || count <= 0 {
		return ""
	}
	end := min(len(r), b.cursor+count)
	deleted := string(r[b.cursor:end])
	if b.setText(string(r[:b.cursor])+string(r[end:])) != nil {
		return ""
	}
	return deleted
}

// Swaps the two characters before the cursor
func (b *ptBuffer) swapCharactersBeforeCursor() {
	r := []rune(b.text())
	if pos := b.cursor; pos >= 2 {
		r[pos-2], r[pos-1] = r[pos-1], r[pos-2]
		_ = b.setText(string(r))
	}
}

// Jumps to a history line with the cursor at its end
func (b *ptBuffer) goToHistory(index int) {
	if index < len(b.workingLines) {
		b.setWorkingIndex(index)
		b.setCursorPosition(len([]rune(b.text())))
	}
}

// Moves forward through history
func (b *ptBuffer) historyForward(count int) {
	found := false
	for i := b.workingIndex + 1; i < len(b.workingLines); i++ {
		b.setWorkingIndex(i)
		count--
		found = true
		if count == 0 {
			break
		}
	}
	if found {
		b.setCursorPosition(0)
		b.setCursorPosition(b.cursor + b.document().endOfLinePosition())
	}
}

// Moves backward through history
func (b *ptBuffer) historyBackward(count int) {
	found := false
	for i := b.workingIndex - 1; i >= 0; i-- {
		b.setWorkingIndex(i)
		count--
		found = true
		if count == 0 {
			break
		}
	}
	if found {
		b.setCursorPosition(len([]rune(b.text())))
	}
}

// Selects the next completion, wrapping through no selection
func (b *ptBuffer) completeNext(count int) {
	state := b.completeState
	if state == nil {
		return
	}
	n := len(state.completions)
	var index *int
	switch {
	case state.index == nil:
		zero := 0
		index = &zero
	case *state.index == n-1:
		index = nil
	default:
		v := min(n-1, *state.index+count)
		index = &v
	}
	b.goToCompletion(index)
}

// Selects the previous completion, wrapping through no selection
func (b *ptBuffer) completePrevious(count int) {
	state := b.completeState
	if state == nil {
		return
	}
	var index *int
	switch {
	case state.index != nil && *state.index == 0:
		index = nil
	case state.index == nil:
		v := len(state.completions) - 1
		index = &v
	default:
		v := max(0, *state.index-count)
		index = &v
	}
	b.goToCompletion(index)
}

// Closes the completion menu restoring the text typed before it
func (b *ptBuffer) cancelCompletion() {
	if b.completeState != nil {
		b.goToCompletion(nil)
		b.completeState = nil
		b.changed()
	}
}

// Shows the text of the completion at index, nil showing what was typed
func (b *ptBuffer) goToCompletion(index *int) {
	state := b.completeState
	if state == nil {
		return
	}
	if index != nil && (*index < 0 || *index >= len(state.completions)) {
		return
	}
	state.index = index
	text, cursor := state.newTextAndPosition()
	_ = b.setDocument(text, cursor, false)
	b.completeState = state
	b.changed()
}

// Inserts a completion and closes the menu
func (b *ptBuffer) applyCompletion(c ptCompletion) {
	if b.completeState != nil {
		b.goToCompletion(nil)
	}
	b.completeState = nil
	b.deleteBeforeCursor(-c.Start)
	b.insertText(c.Text, false, true, true)
}

// Reports whether applying the completion would leave the text unchanged
func completionDoesNothing(doc ptDocument, c ptCompletion) bool {
	before := doc.runes[:doc.cursor]
	replaced := before[max(0, len(before)+c.Start):]
	return string(replaced) == c.Text
}

// Inserts text at the cursor, starting completion and suggestion when fireEvent is set
func (b *ptBuffer) insertText(data string, overwrite, moveCursor, fireEvent bool) {
	r := []rune(b.text())
	ins := []rune(data)
	pos := b.cursor
	var text string
	if overwrite {
		over := r[pos:min(len(r), pos+len(ins))]
		for i, c := range over {
			if c == '\n' {
				over = over[:i]
				break
			}
		}
		text = string(r[:pos]) + data + string(r[pos+len(over):])
	} else {
		text = string(r[:pos]) + data + string(r[pos:])
	}
	cpos := pos
	if moveCursor {
		cpos = pos + len(ins)
	}
	if b.setDocument(text, cpos, false) != nil {
		return
	}
	if fireEvent {
		if b.completer != nil && b.completeWhileTyping {
			b.startCompletion(false, false, false)
		}
		if b.suggester != nil {
			b.startSuggestion()
		}
	}
}

// Runs work off the loop and applies its result on it, inline when no loop is attached
func (b *ptBuffer) runAsync(work func() func()) {
	if b.spawn != nil {
		b.spawn(work)
		return
	}
	if apply := work(); apply != nil {
		apply()
	}
}

// Loads completions for the text before the cursor and opens the menu when the text still matches
func (b *ptBuffer) startCompletion(selectFirst, selectLast, insertCommonPart bool) {
	if b.completeState != nil || b.completer == nil {
		return
	}
	doc := b.document()
	state := &ptCompletionState{original: doc}
	b.completeState = state
	b.completeGen++
	gen := b.completeGen
	completer := b.completer
	before := doc.textBeforeCursor()
	b.runAsync(func() func() {
		completions := completer(before)
		return func() {
			if gen != b.completeGen {
				return
			}
			if len(completions) == 1 && completionDoesNothing(doc, completions[0]) {
				completions = nil
			}
			if b.completeState != state {
				now := b.document().textBeforeCursor()
				if now != before && strings.HasPrefix(now, before) {
					b.startCompletion(selectFirst, selectLast, insertCommonPart)
				}
				return
			}
			state.completions = completions
			if state.index != nil {
				return
			}
			if len(completions) == 0 {
				b.completeState = nil
				b.changed()
				return
			}
			switch {
			case selectFirst:
				zero := 0
				b.goToCompletion(&zero)
			case selectLast:
				last := len(completions) - 1
				b.goToCompletion(&last)
			case insertCommonPart:
				b.insertCommonCompletionPart(state)
			}
			b.changed()
		}
	})
}

// Inserts the text every completion shares, selecting the only completion when nothing is shared
func (b *ptBuffer) insertCommonCompletionPart(state *ptCompletionState) {
	common := commonCompletionSuffix(state.original, state.completions)
	if common == "" {
		if len(state.completions) == 1 {
			zero := 0
			b.goToCompletion(&zero)
		}
		return
	}
	b.insertText(common, false, true, false)
	if len(state.completions) <= 1 {
		b.completeState = nil
		return
	}
	shift := len([]rune(common))
	trimmed := make([]ptCompletion, len(state.completions))
	for i, c := range state.completions {
		r := []rune(c.Text)
		trimmed[i] = ptCompletion{Text: string(r[min(len(r), shift-c.Start):]), Display: c.Display, Meta: c.Meta}
	}
	b.completeState = &ptCompletionState{original: b.document(), completions: trimmed}
}

// Longest text every completion adds after what is typed, empty when one rewrites typed text
func commonCompletionSuffix(doc ptDocument, completions []ptCompletion) string {
	before := doc.textBeforeCursor()
	var suffixes [][]rune
	for _, c := range completions {
		r := []rune(c.Text)
		typed := min(len(r), max(0, -c.Start))
		if !strings.HasSuffix(before, string(r[:typed])) {
			return ""
		}
		suffixes = append(suffixes, r[typed:])
	}
	if len(suffixes) == 0 {
		return ""
	}
	prefix := suffixes[0]
	for _, s := range suffixes[1:] {
		n := 0
		for n < len(prefix) && n < len(s) && prefix[n] == s[n] {
			n++
		}
		prefix = prefix[:n]
	}
	return string(prefix)
}

// Asks the suggester for ghost text and keeps it when the document is still the same
func (b *ptBuffer) startSuggestion() {
	if b.suggestion != nil || b.suggester == nil {
		return
	}
	doc := b.document()
	b.suggestGen++
	gen := b.suggestGen
	suggester := b.suggester
	b.runAsync(func() func() {
		text, ok := suggester(b, doc)
		return func() {
			if gen != b.suggestGen {
				return
			}
			if !b.document().equal(doc) {
				b.startSuggestion()
				return
			}
			if ok {
				b.suggestion = &text
			} else {
				b.suggestion = nil
			}
			b.changed()
		}
	})
}

// Inserts a newline, copying the indentation of the current line when asked
func (b *ptBuffer) newline(copyMargin bool) {
	if copyMargin {
		b.insertText("\n"+b.document().leadingWhitespaceInCurrentLine(), false, true, true)
		return
	}
	b.insertText("\n", false, true, true)
}

// Starts a selection of the given kind at the cursor
func (b *ptBuffer) startSelection(typ ptSelectionType) {
	b.selection = &ptSelection{original: b.cursor, typ: typ}
	b.changed()
}

// Ends the selection keeping the text
func (b *ptBuffer) exitSelection() { b.selection = nil; b.changed() }

// Clipboard entry of the selection, the selection removed from the text when cut is set
func (b *ptBuffer) copySelection(cut bool) ptClipboardData {
	newDocument, data := b.document().cutSelection()
	if cut {
		if err := b.setDocument(newDocument.text(), newDocument.cursor, false); err != nil {
			panic(ptEditReadOnly{})
		}
	}
	b.selection = nil
	b.changed()
	return data
}

// Removes the selection and returns it as a clipboard entry
func (b *ptBuffer) cutSelection() ptClipboardData { return b.copySelection(true) }

// Pastes a clipboard entry count times in the given mode, remembering the document for a yank pop
func (b *ptBuffer) pasteClipboardData(data ptClipboardData, mode ptPasteMode, count int) {
	original := b.document()
	pasted := original.pasteClipboardData(data, mode, count)
	if err := b.setDocument(pasted.text(), pasted.cursor, false); err != nil {
		panic(ptEditReadOnly{})
	}
	b.documentBeforePaste = &original
}

// Inserts the n-th word of an earlier history line, repeated calls walking further back
func (b *ptBuffer) yankNthArg(n *int, last bool) {
	entries := b.history.strings()
	if len(entries) == 0 {
		return
	}
	state := b.yankNthArgState
	if state == nil {
		state = &ptYankNthArgState{n: 1}
		if last {
			state.n = -1
		}
	}
	if n != nil {
		state.n = *n
	}
	newPos := state.historyPosition - 1
	if -newPos > len(entries) {
		newPos = -1
	}
	line := entries[len(entries)+newPos]
	var words []string
	for _, w := range splitKeepingDelimiters(ptQuotedWordsRe, line) {
		if w = strings.TrimSpace(w); w != "" {
			words = append(words, w)
		}
	}
	word := ""
	idx := state.n
	if idx < 0 {
		idx += len(words)
	}
	if idx >= 0 && idx < len(words) {
		word = words[idx]
	}
	if state.previousWord != "" {
		b.deleteBeforeCursor(len([]rune(state.previousWord)))
	}
	b.insertText(word, false, true, true)
	state.previousWord = word
	state.historyPosition = newPos
	b.yankNthArgState = state
}

// Splits text on a pattern keeping the matched delimiters as items, like re.split with a group
func splitKeepingDelimiters(re *regexp.Regexp, text string) []string {
	var out []string
	last := 0
	for _, loc := range re.FindAllStringIndex(text, -1) {
		out = append(out, text[last:loc[0]], text[loc[0]:loc[1]])
		last = loc[1]
	}
	return append(out, text[last:])
}
