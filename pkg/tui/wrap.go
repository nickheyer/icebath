package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/text/unicode/norm"
)

const (
	wrapLinkEscape = "]8;;"
	wrapBell       = '\a'
)

// Text wrapped to columns with long words broken and every space kept, the wrap-ansi hard untrimmed layout
func wrapHard(s string, columns int) string {
	s = strings.ReplaceAll(norm.NFC.String(s), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = wrapHardLine(line, columns)
	}
	return strings.Join(lines, "\n")
}

// One newline free line wrapped at spaces, a word wider than the row split across rows
func wrapHardLine(s string, columns int) string {
	words := strings.Split(s, " ")
	rows := []string{""}
	for index, word := range words {
		length := ansi.StringWidth(word)
		rowLength := ansi.StringWidth(rows[len(rows)-1])
		if index != 0 {
			if rowLength >= columns {
				rows = append(rows, "")
				rowLength = 0
			}
			rows[len(rows)-1] += " "
			rowLength++
		}
		if length > columns {
			remaining := columns - rowLength
			breaksThisLine := 1 + (length-remaining-1)/columns
			breaksNextLine := (length - 1) / columns
			if breaksNextLine < breaksThisLine {
				rows = append(rows, "")
			}
			rows = wrapWord(rows, word, columns)
			continue
		}
		if rowLength+length > columns && rowLength > 0 && length > 0 {
			rows = append(rows, "")
		}
		rows[len(rows)-1] += word
	}
	return strings.Join(rows, "\n")
}

// Rows with one word appended character by character, opening a row at each full width and skipping escapes
func wrapWord(rows []string, word string, columns int) []string {
	chars := []rune(word)
	inEscape, inLink := false, false
	visible := ansi.StringWidth(rows[len(rows)-1])
	for index, ch := range chars {
		width := ansi.StringWidth(string(ch))
		if visible+width <= columns {
			rows[len(rows)-1] += string(ch)
		} else {
			rows = append(rows, string(ch))
			visible = 0
		}
		if ch == '\x1b' || ch == '\u009b' {
			inEscape = true
			end := min(len(chars), index+1+len(wrapLinkEscape))
			inLink = string(chars[index+1:end]) == wrapLinkEscape
		}
		if inEscape {
			if inLink {
				if ch == wrapBell {
					inEscape, inLink = false, false
				}
			} else if ch == 'm' {
				inEscape = false
			}
			continue
		}
		visible += width
		if visible == columns && index < len(chars)-1 {
			rows = append(rows, "")
			visible = 0
		}
	}
	if visible == 0 && len(rows[len(rows)-1]) > 0 && len(rows) > 1 {
		rows[len(rows)-2] += rows[len(rows)-1]
		rows = rows[:len(rows)-1]
	}
	return rows
}
