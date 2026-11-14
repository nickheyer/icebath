package approval

import (
	"strings"
)

var inertHeredocConsumerRe = wideRe(`^\s*(?:[A-Z_][A-Z0-9_]*=\S+\s+)*(?:env\s+)?(?:[A-Za-z0-9_./-]+/)?`+
	`(?:python(?:3(?:\.\d+)*)?|osascript|cat)(?:\s|\Z)`, true)

func spanEnd(command string, cursor int, closer byte) int {
	end := cursor + 1
	for end < len(command) {
		if command[end] == closer {
			return end + 1
		}
		if command[end] == '\\' && end+1 < len(command) {
			end += 1 + runeWidth(command, end+1)
		} else {
			end += runeWidth(command, end)
		}
	}
	return end
}

func maskSimpleQuotes(command string) string {
	var b strings.Builder
	for cursor := 0; cursor < len(command); {
		char := command[cursor]
		switch {
		case char == '\'' || char == '"':
			end := 0
			if char == '\'' {
				end = strings.IndexByte(command[cursor+1:], '\'') + cursor + 1 + 1
				if end == cursor+1 {
					end = 0
				}
			} else {
				end = spanEnd(command, cursor, '"')
			}
			segment := pySlice(command, cursor, end)
			if !strings.HasSuffix(segment, string(char)) || end <= cursor {
				b.WriteString(command[cursor:])
				return b.String()
			}
			if char == '"' && (strings.Contains(segment, "$(") || strings.Contains(segment, "`")) {
				b.WriteString(segment)
			} else {
				b.WriteByte(char)
				b.WriteByte(char)
			}
			cursor = end
		case char == '`':
			end := spanEnd(command, cursor, '`')
			b.WriteString(command[cursor:end])
			cursor = end
		default:
			w := runeWidth(command, cursor)
			b.WriteString(command[cursor : cursor+w])
			cursor += w
		}
	}
	return b.String()
}

type heredocSpec struct {
	delimiter string
	stripTabs bool
	quoted    bool
}

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
	for cursor < len(command) && !spaceAt(command, cursor) && strings.IndexByte(";&|<>()", command[cursor]) < 0 {
		char := command[cursor]
		switch {
		case char == '\\':
			if cursor+1 >= len(command) || command[cursor+1] == '\r' || command[cursor+1] == '\n' {
				return 0, heredocSpec{}, false
			}
			quoted = true
			w := runeWidth(command, cursor+1)
			delimiter.WriteString(command[cursor+1 : cursor+1+w])
			cursor += 1 + w
		case char == '\'' || char == '"':
			quoted = true
			cursor++
			for cursor < len(command) && command[cursor] != char {
				current := cursor
				if command[cursor] == '\r' || command[cursor] == '\n' {
					return 0, heredocSpec{}, false
				}
				if char == '"' && command[cursor] == '\\' {
					if cursor+1 >= len(command) {
						return 0, heredocSpec{}, false
					}
					if strings.IndexByte("$`\"\\\n", command[cursor+1]) >= 0 {
						cursor++
						current = cursor
					}
				}
				w := runeWidth(command, current)
				delimiter.WriteString(command[current : current+w])
				cursor = current + w
			}
			if cursor >= len(command) {
				return 0, heredocSpec{}, false
			}
			cursor++
		default:
			w := runeWidth(command, cursor)
			delimiter.WriteString(command[cursor : cursor+w])
			cursor += w
		}
	}
	if delimiter.Len() == 0 && !quoted {
		return 0, heredocSpec{}, false
	}
	return cursor, heredocSpec{delimiter.String(), stripTabs, quoted}, true
}

func fdRedirectAmpersand(command string, index int) bool {
	before := byte(0)
	if index > 0 {
		before = command[index-1]
	}
	after := byte(0)
	if index+1 < len(command) {
		after = command[index+1]
	}
	return index == 0 || before == '<' || before == '>' || after == '>'
}

type heredocUnit struct {
	end             int
	specs           []heredocSpec
	unknownOperator bool
	postListOp      bool
	ownerStart      int
}

func scanHeredocUnit(command string, start int) heredocUnit {
	u := heredocUnit{ownerStart: start}
	cursor := start
	var quote byte
	comment := false
	for cursor < len(command) {
		char := command[cursor]
		if char == '\n' && (comment || quote == 0) {
			break
		}
		escaped := char == '\\' && quote != '\'' && !comment && cursor+1 < len(command)
		switch {
		case comment || quote != 0 || escaped:
			if quote != 0 && char == quote {
				quote = 0
			}
			if escaped {
				cursor += 1 + runeWidth(command, cursor+1)
			} else {
				cursor += runeWidth(command, cursor)
			}
		case char == '\'' || char == '"' || char == '`':
			quote = char
			cursor++
		case char == '#' && (cursor == start || pySpace(runeBefore(command, cursor)) || strings.IndexByte(";&|()", command[cursor-1]) >= 0):
			comment = true
			cursor++
		case strings.HasPrefix(command[cursor:], "<<<"):
			cursor += 3
		case strings.HasPrefix(command[cursor:], "<<"):
			end, spec, ok := parseHeredocOperator(command, cursor)
			if !ok {
				u.unknownOperator = true
				cursor += 2
			} else {
				cursor = end
				u.specs = append(u.specs, spec)
			}
		default:
			if (char == ';' || char == '|' || char == '&') && !(char == '&' && fdRedirectAmpersand(command, cursor)) {
				if len(u.specs) > 0 {
					u.postListOp = true
				} else {
					u.ownerStart = cursor + 1
				}
			}
			cursor += runeWidth(command, cursor)
		}
	}
	u.end = cursor
	return u
}

func findHeredocClose(command string, bodyStart int, spec heredocSpec) int {
	for cursor := bodyStart; ; {
		newline := strings.IndexByte(command[cursor:], '\n')
		after := len(command)
		if newline >= 0 {
			newline += cursor
			after = newline + 1
		}
		line := strings.TrimSuffix(strings.TrimSuffix(command[cursor:after], "\n"), "\r")
		candidate := line
		if spec.stripTabs {
			candidate = strings.TrimLeft(line, "\t")
		}
		if candidate == spec.delimiter {
			return after
		}
		if newline < 0 {
			return -1
		}
		cursor = after
	}
}

func StripInertHeredocs(command string) string {
	if !strings.Contains(command, "<<") {
		return command
	}
	lastOpener := strings.LastIndex(command, "<<")
	var ranges []textSpan
	for commandStart := 0; commandStart <= lastOpener; {
		u := scanHeredocUnit(command, commandStart)
		if u.unknownOperator {
			return command
		}
		if len(u.specs) == 0 {
			if u.end >= len(command) {
				break
			}
			commandStart = u.end + 1
			continue
		}
		if u.end >= len(command) {
			return command
		}
		bodyCursor := u.end + 1
		var bodies []textSpan
		allQuoted := true
		for _, spec := range u.specs {
			closeEnd := findHeredocClose(command, bodyCursor, spec)
			if closeEnd < 0 {
				return command
			}
			bodies = append(bodies, textSpan{bodyCursor, closeEnd})
			bodyCursor = closeEnd
			allQuoted = allQuoted && spec.quoted
		}
		if allQuoted && !u.postListOp {
			opener := maskSimpleQuotes(command[commandStart:u.end])
			owner := maskSimpleQuotes(pySlice(command, u.ownerStart, u.end))
			if !strings.ContainsAny(opener, "`(){}") && !strings.Contains(opener, "$(") &&
				!strings.Contains(opener, "<(") && !strings.Contains(opener, ">(") &&
				inertHeredocConsumerRe.MatchString(wide(owner)) {
				ranges = append(ranges, bodies...)
			}
		}
		commandStart = bodyCursor
	}
	var b strings.Builder
	prev := 0
	for _, r := range ranges {
		b.WriteString(command[prev:r.start])
		b.WriteString(strings.Repeat("\n", strings.Count(command[r.start:r.end], "\n")))
		prev = r.end
	}
	b.WriteString(command[prev:])
	return b.String()
}
