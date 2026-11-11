package app

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	ptCurrentWordRe            = regexp.MustCompile(`^([a-zA-Z0-9_]+|[^a-zA-Z0-9_` + ptPyWhitespace + `]+)`)
	ptCurrentWordTrailingRe    = regexp.MustCompile(`^(([a-zA-Z0-9_]+|[^a-zA-Z0-9_` + ptPyWhitespace + `]+)[` + ptPyWhitespace + `]*)`)
	ptCurrentBigWordRe         = regexp.MustCompile(`^([^` + ptPyWhitespace + `]+)`)
	ptCurrentBigWordTrailingRe = regexp.MustCompile(`^([^` + ptPyWhitespace + `]+[` + ptPyWhitespace + `]*)`)
	ptWordAlphabet             = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_"
	ptDocumentBracketPairs     = [][2]rune{{'(', ')'}, {'[', ']'}, {'{', '}'}, {'<', '>'}}
)

// ptSelectionType is how a selection or clipboard entry spans text, the SelectionType enum
type ptSelectionType int

// Selection and clipboard spans
const (
	ptSelectCharacters ptSelectionType = iota
	ptSelectLines
	ptSelectBlock
)

// ptPasteMode is where a paste lands relative to the cursor
type ptPasteMode int

// Paste placements
const (
	ptPasteEmacs ptPasteMode = iota
	ptPasteViBefore
	ptPasteViAfter
)

// ptClipboardData is clipboard text with the way it was selected
type ptClipboardData struct {
	text string
	typ  ptSelectionType
}

// Document with the selection and vi mode of another over new text and cursor
func (d ptDocument) with(text string, cursor int) ptDocument {
	n := newPTDocument(text, cursor)
	n.vi = d.vi
	return n
}

// Row and column of an index
func (d ptDocument) translateIndexToPosition(index int) (int, int) {
	row, start := d.lineStart(index)
	return row, index - start
}

// Index of a row and column with Python negative row indexing, the column clamped into the line
func (d ptDocument) translateRowColToIndex(row, col int) int {
	lines := d.lines()
	starts := make([]int, len(lines))
	pos := 0
	for i, line := range lines {
		starts[i] = pos
		pos += len([]rune(line)) + 1
	}
	idx := row
	if idx < 0 {
		idx += len(lines)
	}
	var result int
	var line string
	switch {
	case idx >= 0 && idx < len(lines):
		result, line = starts[idx], lines[idx]
	case row < 0:
		result, line = starts[0], lines[0]
	default:
		result, line = starts[len(starts)-1], lines[len(lines)-1]
	}
	result += max(0, min(col, len([]rune(line))))
	return max(0, min(result, len(d.runes)))
}

// Whether the cursor sits on a newline or past the text
func (d ptDocument) isCursorAtTheEndOfLine() bool {
	return d.cursor >= len(d.runes) || d.runes[d.cursor] == '\n'
}

// Lines from the cursor line to the end
func (d ptDocument) linesFromCurrent() []string { return d.lines()[d.cursorRow():] }

// Rune offsets of the non overlapping literal matches of sub in text
func ptLiteralMatches(text []rune, sub string, ignoreCase bool) []int {
	pattern := regexp.QuoteMeta(sub)
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re := regexp.MustCompile(pattern)
	var out []int
	for _, m := range runeMatches(re, text) {
		out = append(out, m[0])
	}
	return out
}

// Offset of the count-th occurrence of sub after the cursor, false when there is none
func (d ptDocument) find(sub string, inCurrentLine, includeCurrentPosition, ignoreCase bool, count int) (int, bool) {
	text := d.runes[d.cursor:]
	if inCurrentLine {
		text = d.currentLineAfterCursor()
	}
	if !includeCurrentPosition {
		if len(text) == 0 {
			return 0, false
		}
		text = text[1:]
	}
	for i, start := range ptLiteralMatches(text, sub, ignoreCase) {
		if i+1 == count {
			if includeCurrentPosition {
				return start, true
			}
			return start + 1, true
		}
	}
	return 0, false
}

// Offset of the count-th occurrence of sub before the cursor, false when there is none
func (d ptDocument) findBackwards(sub string, inCurrentLine, ignoreCase bool, count int) (int, bool) {
	before := d.runes[:d.cursor]
	if inCurrentLine {
		before = d.currentLineBeforeCursor()
	}
	reversed := string(reverseRunes([]rune(sub)))
	for i, start := range ptLiteralMatches(reverseRunes(before), reversed, ignoreCase) {
		if i+1 == count {
			return -start - len([]rune(sub)), true
		}
	}
	return 0, false
}

// Relative start and end of the word under the cursor on its line, zero when on blanks
func (d ptDocument) findBoundariesOfCurrentWord(bigWord, leadingWhitespace, trailingWhitespace bool) (int, int) {
	before := reverseRunes(d.currentLineBeforeCursor())
	after := d.currentLineAfterCursor()
	pick := func(withWhitespace bool) *regexp.Regexp {
		switch {
		case bigWord && withWhitespace:
			return ptCurrentBigWordTrailingRe
		case bigWord:
			return ptCurrentBigWordRe
		case withWhitespace:
			return ptCurrentWordTrailingRe
		}
		return ptCurrentWordRe
	}
	groupEnd := func(re *regexp.Regexp, text []rune) (int, bool) {
		s := string(text)
		loc := re.FindStringSubmatchIndex(s)
		if loc == nil {
			return 0, false
		}
		return len([]rune(s[:loc[3]])), true
	}
	beforeEnd, beforeOK := groupEnd(pick(leadingWhitespace), before)
	afterEnd, afterOK := groupEnd(pick(trailingWhitespace), after)
	if !bigWord && beforeOK && afterOK {
		c1, c2 := d.runes[d.cursor-1], d.runes[d.cursor]
		if strings.ContainsRune(ptWordAlphabet, c1) != strings.ContainsRune(ptWordAlphabet, c2) {
			beforeOK = false
		}
	}
	start, end := 0, 0
	if beforeOK {
		start = -beforeEnd
	}
	if afterOK {
		end = afterEnd
	}
	return start, end
}

// Word under the cursor, empty on a whitespace region
func (d ptDocument) getWordUnderCursor(bigWord bool) string {
	start, end := d.findBoundariesOfCurrentWord(bigWord, false, false)
	return string(d.runes[d.cursor+start : d.cursor+end])
}

// Word or WORD pattern of the motions
func ptWordRegexp(bigWord bool) *regexp.Regexp {
	if bigWord {
		return ptFindBigWordRe
	}
	return ptFindWordRe
}

// Offset to the start of the count-th word or WORD after the cursor, false when there is none
func (d ptDocument) findNextWordBeginningKind(count int, bigWord bool) (int, bool) {
	if count < 0 {
		return d.findPreviousWordBeginningKind(-count, bigWord)
	}
	for i, m := range runeMatches(ptWordRegexp(bigWord), d.runes[d.cursor:]) {
		if i == 0 && m[0] == 0 {
			count++
		}
		if i+1 == count {
			return m[0], true
		}
	}
	return 0, false
}

// Offset to the end of the count-th word or WORD after the cursor, false when there is none
func (d ptDocument) findNextWordEndingKind(includeCurrentPosition bool, count int, bigWord bool) (int, bool) {
	if count < 0 {
		return d.findPreviousWordEndingKind(-count, bigWord)
	}
	text := d.runes[d.cursor:]
	if !includeCurrentPosition && len(text) > 0 {
		text = text[1:]
	}
	for i, m := range runeMatches(ptWordRegexp(bigWord), text) {
		if i+1 == count {
			if includeCurrentPosition {
				return m[1], true
			}
			return m[1] + 1, true
		}
	}
	return 0, false
}

// Offset to the start of the count-th word or WORD before the cursor, false when there is none
func (d ptDocument) findPreviousWordBeginningKind(count int, bigWord bool) (int, bool) {
	if count < 0 {
		return d.findNextWordBeginningKind(-count, bigWord)
	}
	for i, m := range runeMatches(ptWordRegexp(bigWord), reverseRunes(d.runes[:d.cursor])) {
		if i+1 == count {
			return -m[1], true
		}
	}
	return 0, false
}

// Offset to the end of the count-th word or WORD before the cursor, false when there is none
func (d ptDocument) findPreviousWordEndingKind(count int, bigWord bool) (int, bool) {
	if count < 0 {
		return d.findNextWordEndingKind(false, -count, bigWord)
	}
	var text []rune
	if d.cursor < len(d.runes) {
		text = append(text, d.runes[d.cursor])
	}
	text = append(text, reverseRunes(d.runes[:d.cursor])...)
	for i, m := range runeMatches(ptWordRegexp(bigWord), text) {
		if i == 0 && m[0] == 0 {
			count++
		}
		if i+1 == count {
			return -m[0] + 1, true
		}
	}
	return 0, false
}

// Whether a line is empty or only whitespace
func ptBlankLine(line string) bool { return line == "" || pystr.All(line, pystr.IsSpace) }

// Rows below the cursor matching match up to the count-th, relative to the cursor row
func (d ptDocument) findNextMatchingLine(match func(string) bool, count int) (int, bool) {
	result, found := 0, false
	for index, line := range d.lines()[d.cursorRow()+1:] {
		if match(line) {
			result, found = 1+index, true
			count--
		}
		if count == 0 {
			break
		}
	}
	return result, found
}

// Rows above the cursor matching match up to the count-th, relative to the cursor row
func (d ptDocument) findPreviousMatchingLine(match func(string) bool, count int) (int, bool) {
	lines := d.lines()[:d.cursorRow()]
	result, found := 0, false
	for index := 0; index < len(lines); index++ {
		if match(lines[len(lines)-1-index]) {
			result, found = -1-index, true
			count--
		}
		if count == 0 {
			break
		}
	}
	return result, found
}

// Relative position of the right bracket enclosing the cursor, false when unbalanced
func (d ptDocument) findEnclosingBracketRight(left, right rune, endPos *int) (int, bool) {
	if d.currentChar() == right && d.cursor < len(d.runes) {
		return 0, true
	}
	end := len(d.runes)
	if endPos != nil {
		end = min(len(d.runes), *endPos)
	}
	stack := 1
	for i := d.cursor + 1; i < end; i++ {
		switch d.runes[i] {
		case left:
			stack++
		case right:
			stack--
		}
		if stack == 0 {
			return i - d.cursor, true
		}
	}
	return 0, false
}

// Relative position of the left bracket enclosing the cursor, false when unbalanced
func (d ptDocument) findEnclosingBracketLeft(left, right rune, startPos *int) (int, bool) {
	if d.currentChar() == left && d.cursor < len(d.runes) {
		return 0, true
	}
	start := 0
	if startPos != nil {
		start = max(0, *startPos)
	}
	stack := 1
	for i := d.cursor - 1; i > start-1; i-- {
		switch d.runes[i] {
		case right:
			stack++
		case left:
			stack--
		}
		if stack == 0 {
			return i - d.cursor, true
		}
	}
	return 0, false
}

// Relative position of the bracket matching the one under the cursor, zero when none matches
func (d ptDocument) findMatchingBracketPosition(startPos, endPos *int) int {
	if d.cursor >= len(d.runes) {
		return 0
	}
	for _, pair := range ptDocumentBracketPairs {
		switch d.currentChar() {
		case pair[0]:
			pos, _ := d.findEnclosingBracketRight(pair[0], pair[1], endPos)
			return pos
		case pair[1]:
			pos, _ := d.findEnclosingBracketLeft(pair[0], pair[1], startPos)
			return pos
		}
	}
	return 0
}

// Offset to the start of the cursor line, after its leading whitespace when asked
func (d ptDocument) startOfLine(afterWhitespace bool) int {
	if afterWhitespace {
		line := []rune(d.currentLine())
		leading := 0
		for leading < len(line) && pystr.IsSpace(line[leading]) {
			leading++
		}
		return leading - d.cursorCol()
	}
	return -len(d.currentLineBeforeCursor())
}

// Offset to the last non blank character of the cursor line
func (d ptDocument) lastNonBlankOfCurrentLinePosition() int {
	return len([]rune(pystr.Rstrip(d.currentLine()))) - d.cursorCol() - 1
}

// Offset to a column of the cursor line, clamped to the line
func (d ptDocument) getColumnCursorPosition(column int) int {
	lineLength := len([]rune(d.currentLine()))
	column = max(0, min(lineLength, column))
	return column - d.cursorCol()
}

// Selection bounds sorted ascending
func (d ptDocument) sortedSelection() (int, int) {
	from, to := d.cursor, d.selection.original
	if from > to {
		from, to = to, from
	}
	return from, to
}

// Half open ranges of the selection, one per line of a block selection
func (d ptDocument) selectionRanges() [][2]int {
	if d.selection == nil {
		return nil
	}
	from, to := d.sortedSelection()
	if d.selection.typ == ptSelectBlock {
		fromLine, fromColumn := d.translateIndexToPosition(from)
		toLine, toColumn := d.translateIndexToPosition(to)
		if fromColumn > toColumn {
			fromColumn, toColumn = toColumn, fromColumn
		}
		lines := d.lines()
		if d.vi {
			toColumn++
		}
		var out [][2]int
		for l := fromLine; l <= toLine; l++ {
			lineLength := len([]rune(lines[l]))
			if fromColumn <= lineLength {
				out = append(out, [2]int{d.translateRowColToIndex(l, fromColumn), d.translateRowColToIndex(l, min(lineLength, toColumn))})
			}
		}
		return out
	}
	if d.selection.typ == ptSelectLines {
		from = max(0, ptRuneLastIndex(d.runes[:from], '\n')+1)
		if i := ptRuneIndexFrom(d.runes, '\n', to); i >= 0 {
			to = i
		} else {
			to = len(d.runes) - 1
		}
	}
	if d.vi {
		to++
	}
	return [][2]int{{from, to}}
}

// Text of runes from a to b with Python slice clamping of non negative bounds
func ptRuneSlice(runes []rune, a, b int) string {
	a, b = min(max(a, 0), len(runes)), min(max(b, 0), len(runes))
	if a >= b {
		return ""
	}
	return string(runes[a:b])
}

// Last index of r in runes, minus one when absent
func ptRuneLastIndex(runes []rune, r rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

// First index of r at or after start, minus one when absent
func ptRuneIndexFrom(runes []rune, r rune, start int) int {
	for i := max(start, 0); i < len(runes); i++ {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

// Columns of a row the selection covers, false when it misses the row
func (d ptDocument) selectionRangeAtLine(row int) (int, int, bool) {
	if d.selection == nil {
		return 0, 0, false
	}
	line := []rune(d.lines()[row])
	rowStart := d.translateRowColToIndex(row, 0)
	rowEnd := d.translateRowColToIndex(row, len(line))
	from, to := d.sortedSelection()
	start, end := max(rowStart, from), min(rowEnd, to)
	if start > end {
		return 0, 0, false
	}
	switch d.selection.typ {
	case ptSelectLines:
		start, end = rowStart, rowEnd
	case ptSelectBlock:
		_, col1 := d.translateIndexToPosition(from)
		_, col2 := d.translateIndexToPosition(to)
		if col1 > col2 {
			col1, col2 = col2, col1
		}
		if col1 > len(line) {
			return 0, 0, false
		}
		start, end = d.translateRowColToIndex(row, col1), d.translateRowColToIndex(row, col2)
	}
	_, fromColumn := d.translateIndexToPosition(start)
	_, toColumn := d.translateIndexToPosition(end)
	if d.vi {
		toColumn++
	}
	return fromColumn, toColumn, true
}

// Document left after cutting the selection and the clipboard entry holding what was cut
func (d ptDocument) cutSelection() (ptDocument, ptClipboardData) {
	if d.selection == nil {
		return d, ptClipboardData{}
	}
	var cutParts []string
	var remaining strings.Builder
	newCursor, lastTo := d.cursor, 0
	for _, r := range d.selectionRanges() {
		from, to := r[0], r[1]
		if lastTo == 0 {
			newCursor = from
		}
		remaining.WriteString(ptRuneSlice(d.runes, lastTo, from))
		cutParts = append(cutParts, ptRuneSlice(d.runes, from, to))
		lastTo = to
	}
	remaining.WriteString(ptRuneSlice(d.runes, lastTo, len(d.runes)))
	cutText := strings.Join(cutParts, "\n")
	if d.selection.typ == ptSelectLines && strings.HasSuffix(cutText, "\n") {
		cutText = cutText[:len(cutText)-1]
	}
	return d.with(remaining.String(), newCursor), ptClipboardData{text: cutText, typ: d.selection.typ}
}

// Document after pasting the clipboard entry count times at the cursor in the given mode
func (d ptDocument) pasteClipboardData(data ptClipboardData, mode ptPasteMode, count int) ptDocument {
	before, after := mode == ptPasteViBefore, mode == ptPasteViAfter
	count = max(count, 0)
	dataRunes := len([]rune(data.text))
	switch data.typ {
	case ptSelectLines:
		lines := d.lines()
		l := d.cursorRow()
		repeated := make([]string, count)
		for i := range repeated {
			repeated[i] = data.text
		}
		var out []string
		var cursor int
		if before {
			out = append(append(append(out, lines[:l]...), repeated...), lines[l:]...)
			cursor = len([]rune(strings.Join(lines[:l], ""))) + l
		} else {
			out = append(append(append(out, lines[:l+1]...), repeated...), lines[l+1:]...)
			cursor = len([]rune(strings.Join(lines[:l+1], ""))) + l + 1
		}
		return d.with(strings.Join(out, "\n"), cursor)
	case ptSelectBlock:
		lines := d.lines()
		startLine := d.cursorRow()
		startColumn := d.cursorCol() + 1
		if before {
			startColumn = d.cursorCol()
		}
		for i, line := range strings.Split(data.text, "\n") {
			index := i + startLine
			if index >= len(lines) {
				lines = append(lines, "")
			}
			padded := []rune(pystr.Ljust(lines[index], startColumn))
			lines[index] = string(padded[:startColumn]) + strings.Repeat(line, count) + string(padded[startColumn:])
		}
		cursor := d.cursor + 1
		if before {
			cursor = d.cursor
		}
		return d.with(strings.Join(lines, "\n"), cursor)
	}
	repeated := strings.Repeat(data.text, count)
	var text string
	if after {
		split := min(d.cursor+1, len(d.runes))
		text = string(d.runes[:split]) + repeated + string(d.runes[split:])
	} else {
		text = d.textBeforeCursor() + repeated + d.textAfterCursor()
	}
	cursor := d.cursor + dataRunes*count
	if before {
		cursor--
	}
	return d.with(text, cursor)
}

// Offset to the start of the paragraph, the blank line above it when before is set
func (d ptDocument) startOfParagraph(count int, before bool) int {
	lineIndex, _ := d.findPreviousMatchingLine(ptBlankLine, count)
	if lineIndex != 0 {
		add := 1
		if before {
			add = 0
		}
		return min(0, d.cursorUpPosition(-lineIndex, nil)+add)
	}
	return -d.cursor
}

// Offset to the end of the paragraph, the blank line below it when after is set
func (d ptDocument) endOfParagraph(count int, after bool) int {
	lineIndex, _ := d.findNextMatchingLine(ptBlankLine, count)
	if lineIndex != 0 {
		add := 1
		if after {
			add = 0
		}
		return max(0, d.cursorDownPosition(lineIndex, nil)-add)
	}
	return len(d.runes) - d.cursor
}
