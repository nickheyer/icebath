package config

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// One assignment of an env file as the loader reads it, set false for a name without an equals sign
type envAssignment struct {
	name  string
	value string
	set   bool
}

// Cursor over env file text that counts the lines it passes
type envScanner struct {
	text string
	pos  int
	line int
}

// Variable reference of an env value, a braced name with an optional default after a colon and dash
var envVariable = regexp.MustCompile(`\$\{([^}:]*)(?::-([^}]*))?\}`)

// Characters a backslash stands for inside a double quoted value
func doubleQuoteEscapes() map[byte]string {
	return map[byte]string{'\\': `\`, '\'': `'`, '"': `"`, 'a': "\a", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t", 'v': "\v"}
}

// Moves the cursor n bytes on, counting each CRLF, LF and CR inside the stretch as one line
func (sc *envScanner) advance(n int) {
	chunk := sc.text[sc.pos : sc.pos+n]
	for i := 0; i < len(chunk); i++ {
		switch chunk[i] {
		case '\r':
			sc.line++
			if i+1 < len(chunk) && chunk[i+1] == '\n' {
				i++
			}
		case '\n':
			sc.line++
		}
	}
	sc.pos += n
}

// Rune at the cursor, ok false at the end of the text
func (sc *envScanner) peek() (rune, bool) {
	if sc.pos >= len(sc.text) {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(sc.text[sc.pos:])
	return r, true
}

// Byte length of the stretch from the cursor whose characters all satisfy keep
func (sc *envScanner) run(keep func(rune) bool) int {
	n := 0
	for sc.pos+n < len(sc.text) {
		r, size := utf8.DecodeRuneInString(sc.text[sc.pos+n:])
		if !keep(r) {
			break
		}
		n += size
	}
	return n
}

// Passes the blanks at the cursor
func (sc *envScanner) skipBlanks() { sc.advance(sc.run(isEnvBlank)) }

// Passes the rest of the current line and the first character of its line break
func (sc *envScanner) restOfLine() {
	sc.advance(sc.run(func(r rune) bool { return r != '\r' && r != '\n' }))
	if r, ok := sc.peek(); ok && (r == '\r' || r == '\n') {
		sc.advance(1)
	}
}

// Passes an export word followed by blanks
func (sc *envScanner) skipExport() {
	const word = "export"
	if !strings.HasPrefix(sc.text[sc.pos:], word) {
		return
	}
	save := sc.pos
	sc.pos += len(word)
	blanks := sc.run(isEnvBlank)
	sc.pos = save
	if blanks > 0 {
		sc.advance(len(word) + blanks)
	}
}

// Name at the cursor, bare or single quoted, has false before a comment, ok false when unreadable
func (sc *envScanner) key() (name string, has, ok bool) {
	r, _ := sc.peek()
	switch r {
	case '#':
		return "", false, true
	case '\'':
		end := strings.IndexByte(sc.text[sc.pos+1:], '\'')
		if end <= 0 {
			return "", false, false
		}
		name = sc.text[sc.pos+1 : sc.pos+1+end]
		sc.advance(end + 2)
		return name, true, true
	}
	n := sc.run(func(r rune) bool { return r != '=' && r != '#' && !isEnvSpace(r) })
	if n == 0 {
		return "", false, false
	}
	name = sc.text[sc.pos : sc.pos+n]
	sc.advance(n)
	return name, true, true
}

// Text between the quote at the cursor and its closing quote, lines spanned, ok false when unclosed
func (sc *envScanner) quoted(quote byte) (string, bool) {
	i := sc.pos + 1
	for i < len(sc.text) {
		switch c := sc.text[i]; {
		case c == '\\':
			if i+1 >= len(sc.text) {
				return "", false
			}
			_, size := utf8.DecodeRuneInString(sc.text[i+1:])
			i += 1 + size
		case c == quote:
			body := sc.text[sc.pos+1 : i]
			sc.advance(i + 1 - sc.pos)
			return body, true
		default:
			i++
		}
	}
	return "", false
}

// Quoted body with each backslash escape in escapes replaced by what it stands for
func unescapeEnv(body string, escapes map[byte]string) string {
	if !strings.Contains(body, `\`) {
		return body
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' && i+1 < len(body) {
			if v, ok := escapes[body[i+1]]; ok {
				b.WriteString(v)
				i++
				continue
			}
		}
		b.WriteByte(body[i])
	}
	return b.String()
}

// Unquoted value up to the line end without a comment that follows whitespace and without trailing whitespace
func (sc *envScanner) unquoted() string {
	n := sc.run(func(r rune) bool { return r != '\r' && r != '\n' })
	part := sc.text[sc.pos : sc.pos+n]
	sc.advance(n)
	for i := 0; i < len(part); {
		r, size := utf8.DecodeRuneInString(part[i:])
		if !isEnvSpace(r) {
			i += size
			continue
		}
		end := i
		for end < len(part) {
			r, size := utf8.DecodeRuneInString(part[end:])
			if !isEnvSpace(r) {
				break
			}
			end += size
		}
		if end < len(part) && part[end] == '#' {
			part = part[:i]
			break
		}
		i = end
	}
	return strings.TrimRightFunc(part, isEnvSpace)
}

// Value after an equals sign, quoted with its escapes decoded or bare, ok false for an unclosed quote
func (sc *envScanner) value() (string, bool) {
	r, more := sc.peek()
	switch {
	case !more || r == '\n' || r == '\r':
		return "", true
	case r == '\'':
		body, ok := sc.quoted('\'')
		return unescapeEnv(body, map[byte]string{'\\': `\`, '\'': `'`}), ok
	case r == '"':
		body, ok := sc.quoted('"')
		return unescapeEnv(body, doubleQuoteEscapes()), ok
	}
	return sc.unquoted(), true
}

// Next assignment, comment or blank stretch at the cursor, has false without a name, ok false when invalid
func (sc *envScanner) statement() (a envAssignment, has, ok bool) {
	sc.advance(sc.run(isEnvSpace))
	if sc.pos >= len(sc.text) {
		return a, false, true
	}
	sc.skipExport()
	if a.name, has, ok = sc.key(); !ok {
		return a, false, false
	}
	sc.skipBlanks()
	if r, _ := sc.peek(); r == '=' {
		sc.advance(1)
		blanks := sc.run(isEnvBlank)
		sc.advance(blanks)
		a.set = true
		if next, _ := sc.peek(); blanks == 0 || next != '#' {
			if a.value, ok = sc.value(); !ok {
				return a, false, false
			}
		}
	}
	if blanks := sc.run(isEnvBlank); strings.HasPrefix(sc.text[sc.pos+blanks:], "#") {
		sc.advance(blanks)
		sc.advance(sc.run(func(r rune) bool { return r != '\r' && r != '\n' }))
	}
	sc.skipBlanks()
	switch rest := sc.text[sc.pos:]; {
	case rest == "":
	case strings.HasPrefix(rest, "\r\n"):
		sc.advance(2)
	case rest[0] == '\n' || rest[0] == '\r':
		sc.advance(1)
	default:
		return a, false, false
	}
	return a, has, true
}

// Assignments of env file text in file order, invalid called with the line each unreadable statement starts on
func parseEnvStatements(text string, invalid func(line int)) []envAssignment {
	sc := envScanner{text: text, line: 1}
	var out []envAssignment
	for sc.pos < len(sc.text) {
		start := sc.line
		a, has, ok := sc.statement()
		if !ok {
			sc.restOfLine()
			invalid(start)
			continue
		}
		if has {
			out = append(out, a)
		}
	}
	return out
}

// Value with each variable reference replaced by what lookup gives, its default or nothing when unknown
func expandEnvValue(value string, lookup func(name string) (string, bool)) string {
	return envVariable.ReplaceAllStringFunc(value, func(ref string) string {
		m := envVariable.FindStringSubmatchIndex(ref)
		if v, ok := lookup(ref[m[2]:m[3]]); ok {
			return v
		}
		if m[4] >= 0 {
			return ref[m[4]:m[5]]
		}
		return ""
	})
}

// Assignments with references expanded, one entry per name in first seen order holding its last value
func resolveEnvAssignments(assignments []envAssignment, env map[string]string, override bool) []envAssignment {
	var out []envAssignment
	at := map[string]int{}
	lookup := func(name string) (string, bool) {
		v, inEnv := env[name]
		i, inFile := at[name]
		switch {
		case inFile && (override || !inEnv):
			return out[i].value, true
		case inEnv:
			return v, true
		}
		return "", false
	}
	for _, a := range assignments {
		if a.set {
			a.value = expandEnvValue(a.value, lookup)
		}
		if i, seen := at[a.name]; seen {
			out[i] = a
			continue
		}
		at[a.name] = len(out)
		out = append(out, a)
	}
	return out
}
