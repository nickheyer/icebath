package terminal

import (
	"errors"
	"regexp"
	"strings"
)

// Characters a shell word may hold without quoting
var safeWord = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

// One word quoted for a POSIX shell
func Quote(word string) string {
	if word == "" {
		return "''"
	}
	if safeWord.MatchString(word) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'"'"'`) + "'"
}

// Words joined into one shell quoted command line
func Join(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = Quote(word)
	}
	return strings.Join(quoted, " ")
}

var errUnbalanced = errors.New("The command has a quotation mark that is never closed.")

// Words of a command line as a POSIX shell splits them, without expanding anything
func Split(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	in := false
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				return nil, errUnbalanced
			}
			word.WriteString(line[i+1 : i+1+end])
			in, i = true, i+end+2
		case c == '"':
			i++
			closed := false
			for i < len(line) {
				d := line[i]
				if d == '"' {
					closed = true
					i++
					break
				}
				if d == '\\' && i+1 < len(line) && strings.IndexByte("\"\\$`\n", line[i+1]) >= 0 {
					word.WriteByte(line[i+1])
					i += 2
					continue
				}
				word.WriteByte(d)
				i++
			}
			if !closed {
				return nil, errUnbalanced
			}
			in = true
		case c == '\\':
			if i+1 >= len(line) {
				return nil, errors.New("The command ends in a backslash with nothing after it.")
			}
			word.WriteByte(line[i+1])
			in, i = true, i+2
		case space(c):
			if in {
				words = append(words, word.String())
				word.Reset()
				in = false
			}
			i++
		default:
			word.WriteByte(c)
			in, i = true, i+1
		}
	}
	if in {
		words = append(words, word.String())
	}
	return words, nil
}

func space(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// Interpreters whose quoted heredoc bodies are data, with optional assignments, env and a path prefix
var inertConsumer = regexp.MustCompile(`(?i)^\s*(?:[A-Z_][A-Z0-9_]*=\S+\s+)*(?:env\s+)?(?:[A-Za-z0-9_./-]+/)?(?:python(?:3(?:\.\d+)*)?|osascript|cat)(?:\s|$)`)

// Heredoc opener markers that keep a body visible to the scanners
var openerMarkers = []string{"$(", "`", "<(", ">(", "(", ")", "{", "}"}

// Command with the bodies of heredocs that are provably data blanked out, their line count kept, so checks for keywords don't match inside them
func StripInertHeredocs(command string) string {
	if !strings.Contains(command, "<<") {
		return command
	}
	last := strings.LastIndex(command, "<<")
	var ranges [][2]int
	start := 0
	for start <= last {
		unit := scanHeredocUnit(command, start)
		if unit.unknown {
			return command
		}
		if len(unit.specs) == 0 {
			if unit.end >= len(command) {
				break
			}
			start = unit.end + 1
			continue
		}
		if unit.end >= len(command) {
			return command
		}
		cursor := unit.end + 1
		var bodies [][2]int
		for _, spec := range unit.specs {
			end := heredocClose(command, cursor, spec.delimiter, spec.tabs)
			if end < 0 {
				return command
			}
			bodies = append(bodies, [2]int{cursor, end})
			cursor = end
		}
		quoted := true
		for _, spec := range unit.specs {
			quoted = quoted && spec.quoted
		}
		if quoted && !unit.listAfter {
			opener := maskQuotes(command[start:unit.end])
			owner := maskQuotes(command[unit.owner:unit.end])
			marked := false
			for _, marker := range openerMarkers {
				marked = marked || strings.Contains(opener, marker)
			}
			if !marked && inertConsumer.MatchString(owner) {
				ranges = append(ranges, bodies...)
			}
		}
		start = cursor
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

// Index just past the backslash aware span opened at cursor
func spanEnd(command string, cursor int, closer byte) int {
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

// Inert quoted spans blanked, double quoted spans holding a substitution kept visible
func maskQuotes(command string) string {
	var out strings.Builder
	for cursor := 0; cursor < len(command); {
		ch := command[cursor]
		switch ch {
		case '\'', '"':
			end := 0
			if ch == '\'' {
				if i := strings.IndexByte(command[cursor+1:], '\''); i >= 0 {
					end = cursor + i + 2
				}
			} else {
				end = spanEnd(command, cursor, '"')
			}
			segment := ""
			if end > cursor {
				segment = command[cursor:end]
			}
			if !strings.HasSuffix(segment, string(ch)) {
				out.WriteString(command[cursor:])
				return out.String()
			}
			if ch == '"' && (strings.Contains(segment, "$(") || strings.Contains(segment, "`")) {
				out.WriteString(segment)
			} else {
				out.WriteByte(ch)
				out.WriteByte(ch)
			}
			cursor = end
		case '`':
			end := spanEnd(command, cursor, '`')
			out.WriteString(command[cursor:end])
			cursor = end
		default:
			out.WriteByte(ch)
			cursor++
		}
	}
	return out.String()
}

type heredocSpec struct {
	delimiter string
	tabs      bool
	quoted    bool
}

// Heredoc opener at index, its end and spec
func heredocOperator(command string, index int) (int, heredocSpec, bool) {
	if !strings.HasPrefix(command[index:], "<<") || strings.HasPrefix(command[index:], "<<<") {
		return 0, heredocSpec{}, false
	}
	tabs := strings.HasPrefix(command[index+2:], "-")
	cursor := index + 2
	if tabs {
		cursor++
	}
	for cursor < len(command) && (command[cursor] == ' ' || command[cursor] == '\t') {
		cursor++
	}
	if cursor >= len(command) || command[cursor] == '\r' || command[cursor] == '\n' {
		return 0, heredocSpec{}, false
	}
	var delimiter strings.Builder
	quoted := false
	for cursor < len(command) && !space(command[cursor]) && !strings.ContainsRune(";&|<>()", rune(command[cursor])) {
		switch ch := command[cursor]; ch {
		case '\\':
			if cursor+1 >= len(command) || command[cursor+1] == '\r' || command[cursor+1] == '\n' {
				return 0, heredocSpec{}, false
			}
			quoted = true
			delimiter.WriteByte(command[cursor+1])
			cursor += 2
		case '\'', '"':
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
	return cursor, heredocSpec{delimiter: delimiter.String(), tabs: tabs, quoted: quoted}, true
}

// One logical command scanned for heredoc openers and the simple command that owns them
type heredocUnit struct {
	end       int
	specs     []heredocSpec
	unknown   bool
	listAfter bool
	owner     int
}

func scanHeredocUnit(command string, start int) heredocUnit {
	unit := heredocUnit{owner: start}
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
		case ch == '#' && (cursor == start || space(command[cursor-1]) || strings.ContainsRune(";&|()", rune(command[cursor-1]))):
			comment = true
			cursor++
		case strings.HasPrefix(command[cursor:], "<<<"):
			cursor += 3
		case strings.HasPrefix(command[cursor:], "<<"):
			end, spec, ok := heredocOperator(command, cursor)
			if !ok {
				unit.unknown = true
				cursor += 2
				continue
			}
			cursor = end
			unit.specs = append(unit.specs, spec)
		default:
			redirect := ch == '&' && (cursor > 0 && (command[cursor-1] == '<' || command[cursor-1] == '>') || cursor+1 < len(command) && command[cursor+1] == '>')
			if (ch == ';' || ch == '|' || ch == '&') && !redirect {
				if len(unit.specs) > 0 {
					unit.listAfter = true
				} else {
					unit.owner = cursor + 1
				}
			}
			cursor++
		}
	}
	unit.end = cursor
	return unit
}

// Index after the exact terminator line of a heredoc body, -1 when the body never closes
func heredocClose(command string, start int, delimiter string, tabs bool) int {
	for cursor := start; ; {
		newline := strings.IndexByte(command[cursor:], '\n')
		after := len(command)
		if newline >= 0 {
			after = cursor + newline + 1
		}
		line := strings.TrimSuffix(strings.TrimSuffix(command[cursor:after], "\n"), "\r")
		if tabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == delimiter {
			return after
		}
		if newline < 0 {
			return -1
		}
		cursor = after
	}
}
