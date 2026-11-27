package document

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Deepest nesting of arrays and objects read
const maxDepth = 10000

// JSON value as Jupyter writes it, NaN and Infinity included, with its object keys in document order
type node struct {
	// '"' string, '[' array, '{' object, 'n' null, 'v' number, boolean, NaN or Infinity
	kind byte
	// Text as written
	raw string
	// Value of a string
	str   string
	items []*node
	// Keys of an object in the order they first appear, a repeated key keeping its last value
	keys    []string
	members map[string]*node
}

// Member of an object, nil when absent or when n is no object
func (n *node) get(key string) *node {
	if n == nil || n.kind != '{' {
		return nil
	}
	return n.members[key]
}

// Whether n is an object holding the key
func (n *node) has(key string) bool {
	return n.get(key) != nil
}

// Items of an array and whether n is one
func (n *node) array() ([]*node, bool) {
	if n == nil || n.kind != '[' {
		return nil, false
	}
	return n.items, true
}

// Value of a string and whether n is one
func (n *node) text() (string, bool) {
	if n == nil || n.kind != '"' {
		return "", false
	}
	return n.str, true
}

// Reader of one JSON text, its errors placing the problem by line and column
type jsonReader struct {
	text  string
	pos   int
	depth int
}

// The one value of a JSON text, nothing but space around it
func parseJSON(text string) (*node, error) {
	r := &jsonReader{text: text}
	r.space()
	v, err := r.value()
	if err != nil {
		return nil, err
	}
	r.space()
	if r.pos < len(r.text) {
		return nil, r.unexpected(r.pos, "after top-level value")
	}
	return v, nil
}

// Problem at an offset of the text, with its line and its column in characters
func (r *jsonReader) fail(offset int, problem string) error {
	head := r.text[:offset]
	line := 1 + strings.Count(head, "\n")
	column := 1 + utf8.RuneCountInString(head[strings.LastIndexByte(head, '\n')+1:])
	return fmt.Errorf("%s (line %d, column %d)", problem, line, column)
}

// Problem of the character at offset, or of the end of the text, in its context
func (r *jsonReader) unexpected(offset int, context string) error {
	if offset >= len(r.text) {
		return r.fail(len(r.text), "unexpected end of input "+context)
	}
	return r.fail(offset, "invalid character "+quoteChar(r.text, offset)+" "+context)
}

// Character at offset quoted for a message
func quoteChar(text string, offset int) string {
	switch c := text[offset]; {
	case c == '\'':
		return `'\''`
	case c == '"':
		return `'"'`
	case c < utf8.RuneSelf:
		q := strconv.Quote(string(rune(c)))
		return "'" + q[1:len(q)-1] + "'"
	}
	r, _ := utf8.DecodeRuneInString(text[offset:])
	return strconv.QuoteRune(r)
}

func (r *jsonReader) space() {
	for r.pos < len(r.text) && strings.IndexByte(" \t\n\r", r.text[r.pos]) >= 0 {
		r.pos++
	}
}

func (r *jsonReader) value() (*node, error) {
	if r.pos >= len(r.text) {
		return nil, r.unexpected(r.pos, "looking for beginning of value")
	}
	start := r.pos
	scalar := func(kind byte, err error) (*node, error) {
		if err != nil {
			return nil, err
		}
		return &node{kind: kind, raw: r.text[start:r.pos]}, nil
	}
	switch c := r.text[r.pos]; {
	case c == '{':
		return r.object()
	case c == '[':
		return r.array()
	case c == '"':
		s, err := r.str()
		if err != nil {
			return nil, err
		}
		return &node{kind: '"', raw: r.text[start:r.pos], str: s}, nil
	case c == 'n':
		return scalar('n', r.literal("null"))
	case c == 't':
		return scalar('v', r.literal("true"))
	case c == 'f':
		return scalar('v', r.literal("false"))
	case c == 'N':
		return scalar('v', r.literal("NaN"))
	case c == 'I':
		return scalar('v', r.literal("Infinity"))
	case c == '-' && strings.HasPrefix(r.text[r.pos+1:], "I"):
		return scalar('v', r.literal("-Infinity"))
	case c == '-' || isDigit(c):
		return scalar('v', r.number())
	}
	return nil, r.unexpected(r.pos, "looking for beginning of value")
}

// Enters an array or object, refusing nesting past the limit
func (r *jsonReader) enter() error {
	if r.depth++; r.depth > maxDepth {
		return r.fail(r.pos, "exceeded max depth")
	}
	r.pos++
	r.space()
	return nil
}

func (r *jsonReader) object() (*node, error) {
	start := r.pos
	if err := r.enter(); err != nil {
		return nil, err
	}
	defer func() { r.depth-- }()
	n := &node{kind: '{', members: map[string]*node{}}
	if r.pos < len(r.text) && r.text[r.pos] == '}' {
		r.pos++
		n.raw = r.text[start:r.pos]
		return n, nil
	}
	for {
		if r.pos >= len(r.text) || r.text[r.pos] != '"' {
			return nil, r.unexpected(r.pos, "looking for beginning of object key string")
		}
		key, err := r.str()
		if err != nil {
			return nil, err
		}
		r.space()
		if r.pos >= len(r.text) || r.text[r.pos] != ':' {
			return nil, r.unexpected(r.pos, "after object key")
		}
		r.pos++
		r.space()
		v, err := r.value()
		if err != nil {
			return nil, err
		}
		if _, seen := n.members[key]; !seen {
			n.keys = append(n.keys, key)
		}
		n.members[key] = v
		r.space()
		if r.pos >= len(r.text) {
			return nil, r.unexpected(r.pos, "after object key:value pair")
		}
		switch r.text[r.pos] {
		case '}':
			r.pos++
			n.raw = r.text[start:r.pos]
			return n, nil
		case ',':
			comma := r.pos
			r.pos++
			r.space()
			if r.pos < len(r.text) && r.text[r.pos] == '}' {
				return nil, r.fail(comma, "trailing comma before end of object")
			}
		default:
			return nil, r.unexpected(r.pos, "after object key:value pair")
		}
	}
}

func (r *jsonReader) array() (*node, error) {
	start := r.pos
	if err := r.enter(); err != nil {
		return nil, err
	}
	defer func() { r.depth-- }()
	n := &node{kind: '['}
	if r.pos < len(r.text) && r.text[r.pos] == ']' {
		r.pos++
		n.raw = r.text[start:r.pos]
		return n, nil
	}
	for {
		v, err := r.value()
		if err != nil {
			return nil, err
		}
		n.items = append(n.items, v)
		r.space()
		if r.pos >= len(r.text) {
			return nil, r.unexpected(r.pos, "after array element")
		}
		switch r.text[r.pos] {
		case ']':
			r.pos++
			n.raw = r.text[start:r.pos]
			return n, nil
		case ',':
			comma := r.pos
			r.pos++
			r.space()
			if r.pos < len(r.text) && r.text[r.pos] == ']' {
				return nil, r.fail(comma, "trailing comma before end of array")
			}
		default:
			return nil, r.unexpected(r.pos, "after array element")
		}
	}
}

// Reads the literal word, naming the character it expected where the text departs from it
func (r *jsonReader) literal(word string) error {
	for i := 0; i < len(word); i++ {
		if p := r.pos + i; p >= len(r.text) || r.text[p] != word[i] {
			return r.unexpected(p, "in literal "+word+" (expecting "+quoteChar(word, i)+")")
		}
	}
	r.pos += len(word)
	return nil
}

// Reads a number: optional minus, integer part without leading zeros, optional fraction and exponent
func (r *jsonReader) number() error {
	t, i := r.text, r.pos
	if t[i] == '-' {
		i++
	}
	switch {
	case i < len(t) && t[i] == '0':
		i++
	case i < len(t) && t[i] >= '1' && t[i] <= '9':
		for i++; i < len(t) && isDigit(t[i]); i++ {
		}
	default:
		return r.unexpected(i, "in numeric literal")
	}
	if i < len(t) && t[i] == '.' {
		if i++; i >= len(t) || !isDigit(t[i]) {
			return r.unexpected(i, "after decimal point in numeric literal")
		}
		for i < len(t) && isDigit(t[i]) {
			i++
		}
	}
	if i < len(t) && (t[i] == 'e' || t[i] == 'E') {
		if i++; i < len(t) && (t[i] == '+' || t[i] == '-') {
			i++
		}
		if i >= len(t) || !isDigit(t[i]) {
			return r.unexpected(i, "in exponent of numeric literal")
		}
		for i < len(t) && isDigit(t[i]) {
			i++
		}
	}
	r.pos = i
	return nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Reads the string whose opening quote is at pos, one the text ends inside reported at that quote, an unpaired surrogate escape
// becoming a replacement character
func (r *jsonReader) str() (string, error) {
	t, quote := r.text, r.pos
	var b strings.Builder
	for i := quote + 1; ; {
		j := i
		for j < len(t) && t[j] != '"' && t[j] != '\\' && t[j] >= 0x20 {
			j++
		}
		b.WriteString(t[i:j])
		if j >= len(t) {
			return "", r.fail(quote, "string literal not terminated")
		}
		switch t[j] {
		case '"':
			r.pos = j + 1
			return b.String(), nil
		case '\\':
		default:
			return "", r.unexpected(j, "in string literal")
		}
		if j+1 >= len(t) {
			return "", r.fail(quote, "string literal not terminated")
		}
		i = j + 2
		switch e := t[j+1]; e {
		case '"', '\\', '/':
			b.WriteByte(e)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			c, bad := hex4(t, j+2)
			if bad >= len(t) {
				return "", r.fail(quote, "string literal not terminated")
			}
			if bad >= 0 {
				return "", r.unexpected(bad, `in \u hexadecimal character escape`)
			}
			i = j + 6
			if utf16.IsSurrogate(c) {
				if low, bad := hex4(t, i+2); c < 0xDC00 && strings.HasPrefix(t[i:], `\u`) && bad < 0 && low >= 0xDC00 && low <= 0xDFFF {
					c, i = utf16.DecodeRune(c, low), i+6
				} else {
					c = utf8.RuneError
				}
			}
			b.WriteRune(c)
		default:
			return "", r.unexpected(j+1, "in string escape code")
		}
	}
}

// Rune of the four hex digits at offset, with -1; else the offset of the first character that is no hex digit
func hex4(text string, offset int) (rune, int) {
	var c rune
	for k := offset; k < offset+4; k++ {
		if k >= len(text) {
			return 0, k
		}
		switch d := text[k]; {
		case d >= '0' && d <= '9':
			c = c<<4 | rune(d-'0')
		case d >= 'a' && d <= 'f':
			c = c<<4 | rune(d-'a'+10)
		case d >= 'A' && d <= 'F':
			c = c<<4 | rune(d-'A'+10)
		default:
			return 0, k
		}
	}
	return c, -1
}
