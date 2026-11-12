package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/nickheyer/icebath/pkg/ui/cards"
)

// Byte span of one visual row of the composer value
type visualLine struct{ start, end int }

// Row and cell column of a caret in wrapped text
type caretLayout struct{ Column, Line int }

// Rendered cell width of one grapheme, never below one
func graphemeWidth(g string) int { return max(1, ansi.StringWidth(g)) }

// Byte spans of the visual rows the value wraps into at cols, matching the wrap break points
func visualLines(value string, cols int) []visualLine {
	if value == "" {
		return []visualLine{{0, 0}}
	}
	wrapped := wrapHard(value, max(1, cols))
	var lines []visualLine
	orig, lineStart := 0, 0
	for _, ch := range wrapped {
		if ch == '\n' {
			lines = append(lines, visualLine{lineStart, orig})
			if orig < len(value) && value[orig] == '\n' {
				orig++
			}
			lineStart = orig
			continue
		}
		if orig >= len(value) {
			break
		}
		if r, _ := utf8.DecodeRuneInString(value[orig:]); r != ch {
			at := strings.IndexRune(value[orig:], ch)
			if at < 0 {
				break
			}
			orig += at
		}
		_, size := utf8.DecodeRuneInString(value[orig:])
		orig += size
	}
	lines = append(lines, visualLine{lineStart, orig})
	return lines
}

// Cell width of the graphemes between two byte offsets
func widthBetween(value string, start, end int) int {
	width := 0
	for _, g := range cards.Graphemes(value[start:end]) {
		width += graphemeWidth(g)
	}
	return width
}

// Visual row and column of the caret at a byte offset of the value wrapped at cols
func cursorLayout(value string, cursor, cols int) caretLayout {
	pos := max(0, min(cursor, len(value)))
	lines := visualLines(value, max(1, cols))
	index := 0
	for i, l := range lines {
		if l.start > pos {
			break
		}
		index = i
	}
	line := lines[index]
	return caretLayout{Column: widthBetween(value, line.start, min(pos, line.end)), Line: index}
}

// Byte offset of the grapheme under a visual row and column of the wrapped value
func offsetFromPosition(value string, row, col, cols int) int {
	if value == "" {
		return 0
	}
	lines := visualLines(value, cols)
	target := lines[max(0, min(len(lines)-1, row))]
	targetCol := max(0, col)
	column, at := 0, target.start
	for _, g := range cards.Graphemes(value[target.start:target.end]) {
		w := graphemeWidth(g)
		if targetCol <= column+max(0, w-1) {
			return at
		}
		column += w
		at += len(g)
	}
	return target.end
}

// Rows the value takes when wrapped at columns
func inputVisualHeight(value string, columns int) int {
	return cursorLayout(value, len(value), columns).Line + 1
}

// Cells between the prompt glyph and the composer text
const composerPromptGapWidth = 1

// Cells the prompt glyph and its gap take
func composerPromptWidth(promptText string) int {
	return max(1, ansi.StringWidth(promptText)) + composerPromptGapWidth
}

// Gutter cells before a transcript row, the prompt width for user rows and three otherwise
func transcriptGutterWidth(role, userPrompt string) int {
	if role == "user" {
		return composerPromptWidth(userPrompt)
	}
	return 3
}

// Body width of a transcript row after its gutter and side reserve, at least twenty outside Termux
func transcriptBodyWidth(totalCols int, role, userPrompt string, termuxMode bool) int {
	reserve := 4
	if termuxMode {
		reserve = 2
	}
	available := max(1, totalCols-transcriptGutterWidth(role, userPrompt)-reserve)
	if termuxMode {
		return available
	}
	return max(20, available)
}

// Composer text columns after the prompt, keeping two cells for the scrollbar on wide panes
func stableComposerColumns(totalCols, promptWidth int, termuxMode bool) int {
	afterPrompt := totalCols - promptWidth
	threshold := 24
	if termuxMode {
		threshold = 36
	}
	reserve := 0
	if afterPrompt >= threshold {
		reserve = 2
	}
	return max(1, totalCols-promptWidth-2-reserve)
}
