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
