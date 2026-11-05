package provider

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

const (
	pyWSClass  = `\t\n\x0B\f\r\x1C-\x1F \x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}`
	pyHWSClass = `\t\x0B\f\x1C-\x1F \x{85}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}`
)

var (
	dotenvNewlineRe           = regexp.MustCompile(`\r\n|\n|\r`)
	dotenvMultilineWSRe       = regexp.MustCompile(`^[` + pyWSClass + `]*`)
	dotenvWhitespaceRe        = regexp.MustCompile(`^[` + pyHWSClass + `]*`)
	dotenvExportRe            = regexp.MustCompile(`^(?:export[` + pyHWSClass + `]+)?`)
	dotenvSingleQuotedKeyRe   = regexp.MustCompile(`^'([^']+)'`)
	dotenvUnquotedKeyRe       = regexp.MustCompile(`^([^=#` + pyWSClass + `]+)`)
	dotenvEqualSignRe         = regexp.MustCompile(`^(=[` + pyHWSClass + `]*)`)
	dotenvSingleQuotedValueRe = regexp.MustCompile(`(?s)^'((?:\\.|[^'\\])*)'`)
	dotenvDoubleQuotedValueRe = regexp.MustCompile(`(?s)^"((?:\\.|[^"\\])*)"`)
	dotenvUnquotedValueRe     = regexp.MustCompile(`^([^\r\n]*)`)
	dotenvCommentRe           = regexp.MustCompile(`^(?:[` + pyHWSClass + `]*#[^\r\n]*)?`)
	dotenvEndOfLineRe         = regexp.MustCompile(`^[` + pyHWSClass + `]*(?:\r\n|\n|\r|$)`)
	dotenvRestOfLineRe        = regexp.MustCompile(`^[^\r\n]*(?:\r|\n|\r\n)?`)
	dotenvInlineCommentRe     = regexp.MustCompile(`[` + pyWSClass + `]+#`)
	dotenvDoubleEscapeRe      = regexp.MustCompile(`\\[\\'"abfnrtv]`)
	dotenvSingleEscapeRe      = regexp.MustCompile(`\\[\\']`)
	dotenvPosixVariableRe     = regexp.MustCompile(`\$\{([^}:]*)(?::-([^}]*))?\}`)
	dotenvEscapeValues        = map[byte]string{'\\': `\`, '\'': `'`, '"': `"`, 'a': "\a", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t", 'v': "\v"}
)

// One dotenv assignment, set false for a bare name without an equals sign
type dotenvAssignment struct {
	name  string
	value string
	set   bool
}

type dotenvBinding struct {
	key      string
	hasKey   bool
	value    string
	hasValue bool
	line     int
	err      bool
}

type dotenvReader struct {
	text     string
	pos      int
	line     int
	markPos  int
	markLine int
}

type dotenvParseError struct{ msg string }

func (e dotenvParseError) Error() string { return e.msg }

func (r *dotenvReader) advance(consumed string) {
	r.pos += len(consumed)
	r.line += len(dotenvNewlineRe.FindAllStringIndex(consumed, -1))
}

func (r *dotenvReader) peek() string {
	if r.pos >= len(r.text) {
		return ""
	}
	return r.text[r.pos : r.pos+1]
}

// Consumes the regex match at the cursor and returns its groups, panicking with a parse error on a miss
func (r *dotenvReader) readRegex(re *regexp.Regexp) []string {
	m := re.FindStringSubmatch(r.text[r.pos:])
	if m == nil {
		panic(dotenvParseError{"read_regex: Pattern not found"})
	}
	r.advance(m[0])
	return m[1:]
}

func dotenvDecodeEscapes(re *regexp.Regexp, s string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string { return dotenvEscapeValues[m[1]] })
}

func dotenvParseUnquotedValue(r *dotenvReader) string {
	part := r.readRegex(dotenvUnquotedValueRe)[0]
	if loc := dotenvInlineCommentRe.FindStringIndex(part); loc != nil {
		part = part[:loc[0]]
	}
	return strings.TrimRightFunc(part, func(c rune) bool { return unicode.IsSpace(c) || (c >= 0x1c && c <= 0x1f) })
}

func dotenvParseValue(r *dotenvReader) string {
	switch r.peek() {
	case "'":
		return dotenvDecodeEscapes(dotenvSingleEscapeRe, r.readRegex(dotenvSingleQuotedValueRe)[0])
	case `"`:
		return dotenvDecodeEscapes(dotenvDoubleEscapeRe, r.readRegex(dotenvDoubleQuotedValueRe)[0])
	case "", "\n", "\r":
		return ""
	}
	return dotenvParseUnquotedValue(r)
}

// Parses one statement at the cursor, an unparseable one consuming the rest of its line as an error binding
func dotenvParseBinding(r *dotenvReader) (b dotenvBinding) {
	r.markPos, r.markLine = r.pos, r.line
	defer func() {
		if rec := recover(); rec != nil {
			if _, ok := rec.(dotenvParseError); !ok {
				panic(rec)
			}
			r.readRegex(dotenvRestOfLineRe)
			b = dotenvBinding{line: r.markLine, err: true}
		}
	}()
	r.readRegex(dotenvMultilineWSRe)
	if r.pos >= len(r.text) {
		return dotenvBinding{line: r.markLine}
	}
	r.readRegex(dotenvExportRe)
	b.line = r.markLine
	switch r.peek() {
	case "#":
	case "'":
		b.key, b.hasKey = r.readRegex(dotenvSingleQuotedKeyRe)[0], true
	default:
		b.key, b.hasKey = r.readRegex(dotenvUnquotedKeyRe)[0], true
	}
	r.readRegex(dotenvWhitespaceRe)
	if r.peek() == "=" {
		equal := r.readRegex(dotenvEqualSignRe)[0]
		if len(equal) > 1 && r.peek() == "#" {
			b.value = ""
		} else {
			b.value = dotenvParseValue(r)
		}
		b.hasValue = true
	}
	r.readRegex(dotenvCommentRe)
	r.readRegex(dotenvEndOfLineRe)
	return b
}

// Assignments of dotenv text in file order like python-dotenv uninterpolated, warning on unparseable statements
func parseDotenvText(text string) []dotenvAssignment {
	r := &dotenvReader{text: strings.TrimPrefix(text, "\ufeff"), line: 1, markLine: 1}
	var out []dotenvAssignment
	for r.pos < len(r.text) {
		b := dotenvParseBinding(r)
		if b.err {
			envLoaderLog.Warn(fmt.Sprintf("python-dotenv could not parse statement starting at line %d", b.line))
		}
		if b.hasKey {
			out = append(out, dotenvAssignment{name: b.key, value: b.value, set: b.hasValue})
		}
	}
	return out
}

// Expands posix variable references in value through lookup, a None entry or a missing name with no default giving empty
func expandDotenvVariables(value string, lookup func(name string) (string, bool)) string {
	var sb strings.Builder
	cursor := 0
	for _, m := range dotenvPosixVariableRe.FindAllStringSubmatchIndex(value, -1) {
		sb.WriteString(value[cursor:m[0]])
		name := value[m[2]:m[3]]
		if v, ok := lookup(name); ok {
			sb.WriteString(v)
		} else if m[4] >= 0 {
			sb.WriteString(value[m[4]:m[5]])
		}
		cursor = m[1]
	}
	sb.WriteString(value[cursor:])
	return sb.String()
}
