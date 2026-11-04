package tools

import (
	"regexp"
	"strings"
)

// Interpreters whose quoted heredoc bodies are data, with optional assignments, env and a path prefix
var inertHeredocConsumerRe = regexp.MustCompile(`(?i)^\s*(?:[A-Z_][A-Z0-9_]*=\S+\s+)*(?:env\s+)?(?:[A-Za-z0-9_./-]+/)?(?:python(?:3(?:\.\d+)*)?|osascript|cat)(?:\s|$)`)

// Index just past the backslash aware span opened at cursor
func heredocSpanEnd(command string, cursor int, closer byte) int {
	end := cursor + 1
	for end < len(command) {
		if command[end] == closer {
			return end + 1
		}
		if command[end] == '\\' && end+1 < len(command) {
			end += 2
		} else {
			end++
		}
	}
	return end
}

// Blanks inert quoted spans while keeping substitution bearing double quoted spans visible
func maskSimpleQuotes(command string) string {
	var out strings.Builder
	cursor := 0
	for cursor < len(command) {
		ch := command[cursor]
		switch {
		case ch == '\'' || ch == '"':
			var end int
			if ch == '\'' {
				idx := strings.IndexByte(command[cursor+1:], '\'')
				if idx < 0 {
					end = 0
				} else {
					end = cursor + 1 + idx + 1
				}
			} else {
				end = heredocSpanEnd(command, cursor, '"')
			}
			var segment string
			if end > cursor {
				segment = command[cursor:end]
			}
			if !strings.HasSuffix(segment, string(ch)) {
				out.WriteString(command[cursor:])
				return out.String()
			}
			keep := ch == '"' && (strings.Contains(segment, "$(") || strings.Contains(segment, "`"))
			if keep {
				out.WriteString(segment)
			} else {
				out.WriteByte(ch)
				out.WriteByte(ch)
			}
			cursor = end
		case ch == '`':
			end := heredocSpanEnd(command, cursor, '`')
			out.WriteString(command[cursor:end])
			cursor = end
		default:
			out.WriteByte(ch)
			cursor++
		}
	}
	return out.String()
}

// One parsed heredoc opener
type heredocSpec struct {
	delimiter string
	stripTabs bool
	quoted    bool
}

// Parses one heredoc opener at index, returning the end index and the spec
func parseHeredocOperator(command string, index int) (int, heredocSpec, bool) {
	if !strings.HasPrefix(command[index:], "<<") || strings.HasPrefix(command[index:], "<<<") {
		return 0, heredocSpec{}, false
	}
	stripTabs := strings.HasPrefix(command[index+2:], "-")
	cursor := index + 2
	if stripTabs {
		cursor = index + 3
	}
	for cursor < len(command) && (command[cursor] == ' ' || command[cursor] == '\t') {
		cursor++
	}
	if cursor >= len(command) || command[cursor] == '\r' || command[cursor] == '\n' {
		return 0, heredocSpec{}, false
	}
	var delimiter strings.Builder
	quoted := false
	for cursor < len(command) && !isSpaceByte(command[cursor]) && !strings.ContainsRune(";&|<>()", rune(command[cursor])) {
		ch := command[cursor]
		switch {
		case ch == '\\':
			if cursor+1 >= len(command) || command[cursor+1] == '\r' || command[cursor+1] == '\n' {
				return 0, heredocSpec{}, false
			}
			quoted = true
			delimiter.WriteByte(command[cursor+1])
			cursor += 2
		case ch == '\'' || ch == '"':
			quoted = true
			cursor++
			for cursor < len(command) && command[cursor] != ch {
				current := command[cursor]
				if current == '\r' || current == '\n' {
					return 0, heredocSpec{}, false
				}
				if ch == '"' && current == '\\' {
					if cursor+1 >= len(command) {
						return 0, heredocSpec{}, false
					}
					if strings.ContainsRune("$`\"\\\n", rune(command[cursor+1])) {
						cursor++
						current = command[cursor]
					}
				}
				delimiter.WriteByte(current)
				cursor++
			}
			if cursor >= len(command) {
				return 0, heredocSpec{}, false
			}
			cursor++
		default:
			delimiter.WriteByte(ch)
			cursor++
		}
	}
	if delimiter.Len() == 0 && !quoted {
		return 0, heredocSpec{}, false
	}
	return cursor, heredocSpec{delimiter: delimiter.String(), stripTabs: stripTabs, quoted: quoted}, true
}

// Reports whether the ampersand at index belongs to a file descriptor redirection
func isFdRedirectAmpersand(command string, index int) bool {
	var before, after byte
	if index > 0 {
		before = command[index-1]
	}
	if index+1 < len(command) {
		after = command[index+1]
	}
	return before == '<' || before == '>' || after == '>'
}

// Result of scanning one logical command for heredoc openers
type heredocUnit struct {
	end                     int
	specs                   []heredocSpec
	unknownOperator         bool
	postHeredocListOperator bool
	ownerStart              int
}

// Scans one logical command, collecting heredoc openers and the simple command that owns them
func scanHeredocCommandUnit(command string, start int) heredocUnit {
	unit := heredocUnit{ownerStart: start}
	cursor := start
	var quote byte
	comment := false
	for cursor < len(command) {
		ch := command[cursor]
		if ch == '\n' && (comment || quote == 0) {
			break
		}
		escaped := ch == '\\' && quote != '\'' && !comment && cursor+1 < len(command)
		switch {
		case comment || quote != 0 || escaped:
			if ch == quote {
				quote = 0
			}
			if escaped {
				cursor += 2
			} else {
				cursor++
			}
		case ch == '\'' || ch == '"' || ch == '`':
			quote = ch
			cursor++
		case ch == '#' && (cursor == start || isSpaceByte(command[cursor-1]) || strings.ContainsRune(";&|()", rune(command[cursor-1]))):
			comment = true
			cursor++
		case strings.HasPrefix(command[cursor:], "<<<"):
			cursor += 3
		case strings.HasPrefix(command[cursor:], "<<"):
			end, spec, ok := parseHeredocOperator(command, cursor)
			if !ok {
				unit.unknownOperator = true
				cursor += 2
			} else {
				cursor = end
				unit.specs = append(unit.specs, spec)
			}
		default:
			if (ch == ';' || ch == '|' || ch == '&') && !(ch == '&' && isFdRedirectAmpersand(command, cursor)) {
				if len(unit.specs) > 0 {
					unit.postHeredocListOperator = true
				} else {
					unit.ownerStart = cursor + 1
				}
			}
			cursor++
		}
	}
	unit.end = cursor
	return unit
}

// Position after the exact heredoc terminator line, or -1 when the body never closes
func findHeredocClose(command string, bodyStart int, delimiter string, stripTabs bool) int {
	cursor := bodyStart
	for {
		newline := strings.IndexByte(command[cursor:], '\n')
		after := len(command)
		if newline >= 0 {
			after = cursor + newline + 1
		}
		line := strings.TrimSuffix(strings.TrimSuffix(command[cursor:after], "\n"), "\r")
		candidate := line
		if stripTabs {
			candidate = strings.TrimLeft(line, "\t")
		}
		if candidate == delimiter {
			return after
		}
		if newline < 0 {
			return -1
		}
		cursor = after
	}
}

// Heredoc opener markers that keep a body visible to the scanners
var heredocOpenerMarkers = []string{"$(", "`", "<(", ">(", "(", ")", "{", "}"}

// StripInertHeredocBodies masks heredoc bodies that are provably inert data, keeping their newline count
func StripInertHeredocBodies(command string) string {
	if !strings.Contains(command, "<<") {
		return command
	}
	lastOpener := strings.LastIndex(command, "<<")
	var ranges [][2]int
	commandStart := 0
	for commandStart <= lastOpener {
		unit := scanHeredocCommandUnit(command, commandStart)
		if unit.unknownOperator {
			return command
		}
		if len(unit.specs) == 0 {
			if unit.end >= len(command) {
				break
			}
			commandStart = unit.end + 1
			continue
		}
		if unit.end >= len(command) {
			return command
		}
		bodyCursor := unit.end + 1
		var bodyRanges [][2]int
		for _, spec := range unit.specs {
			closeEnd := findHeredocClose(command, bodyCursor, spec.delimiter, spec.stripTabs)
			if closeEnd < 0 {
				return command
			}
			bodyRanges = append(bodyRanges, [2]int{bodyCursor, closeEnd})
			bodyCursor = closeEnd
		}
		allQuoted := true
		for _, spec := range unit.specs {
			if !spec.quoted {
				allQuoted = false
			}
		}
		if allQuoted && !unit.postHeredocListOperator {
			maskedOpener := maskSimpleQuotes(command[commandStart:unit.end])
			maskedOwner := maskSimpleQuotes(command[unit.ownerStart:unit.end])
			hasMarker := false
			for _, marker := range heredocOpenerMarkers {
				if strings.Contains(maskedOpener, marker) {
					hasMarker = true
					break
				}
			}
			if !hasMarker && inertHeredocConsumerRe.MatchString(maskedOwner) {
				ranges = append(ranges, bodyRanges...)
			}
		}
		commandStart = bodyCursor
	}
	var out strings.Builder
	previous := 0
	for _, r := range ranges {
		out.WriteString(command[previous:r[0]])
		out.WriteString(strings.Repeat("\n", strings.Count(command[r[0]:r[1]], "\n")))
		previous = r[1]
	}
	out.WriteString(command[previous:])
	return out.String()
}

// Reports whether a byte is ASCII whitespace
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}
