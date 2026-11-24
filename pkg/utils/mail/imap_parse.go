package mailutil

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

const (
	// Longest response line accepted, literals not counted: a SEARCH over a large mailbox comes as one line
	maxIMAPLine = 64 << 20
	// Deepest nesting of parenthesized lists accepted in a response
	maxNesting = 100
	// Largest literal kept in memory until SetMaxLiteral says otherwise, above the message size limit of every mainstream mail provider
	defaultMaxLiteral = 128 << 20
	// Largest literal over the limit that is read past and dropped so the connection stays usable; a larger one closes it
	maxLiteralDrain = 1 << 30
)

// One server response
type response struct {
	tag    string   // command tag, * for untagged data and + for a continuation request
	kind   string   // upper cased status or data name: OK, NO, BAD, BYE, PREAUTH, CAPABILITY, SEARCH, EXISTS, FETCH and so on
	num    uint32   // number before EXISTS, RECENT, EXPUNGE and FETCH
	code   string   // upper cased response code of a status response, such as CAPABILITY or READ-ONLY
	arg    string   // what follows the response code inside its brackets
	text   string   // human readable text of a status response or a continuation request
	fields []any    // data of other responses: atoms as string, quoted strings and literals as []byte, lists as []any, NIL as nil
	nums   []uint32 // numbers of a SEARCH response
	err    error    // why the data could not be read, the stream kept in step
}

// Malformed data in a response
type syntaxError struct{ msg string }

// Message naming what is malformed
func (e *syntaxError) Error() string { return "imap: malformed response: " + e.msg }

// Next response with the literals it carries
func (c *IMAP) readResponse() (*response, error) {
	line, err := c.readLine(maxIMAPLine)
	if err != nil {
		return nil, err
	}
	if rest, ok := strings.CutPrefix(line, "+"); ok {
		return &response{tag: "+", text: strings.TrimSpace(rest)}, nil
	}
	tag, rest, ok := strings.Cut(line, " ")
	if !ok || tag == "" {
		return nil, fmt.Errorf("imap: malformed response %q", line[:min(len(line), 200)])
	}
	r := &response{tag: tag}
	word, rest, _ := strings.Cut(rest, " ")
	if tag == "*" {
		if n, err := strconv.ParseUint(word, 10, 32); err == nil {
			r.num = uint32(n)
			word, rest, _ = strings.Cut(rest, " ")
		}
	}
	r.kind = strings.ToUpper(word)
	p := &dataParser{c: c, line: rest}
	switch r.kind {
	case "OK", "NO", "BAD", "BYE", "PREAUTH":
		r.code, r.arg, r.text = respText(rest)
		return r, nil
	case "SEARCH":
		if r.nums, err = searchNumbers(rest); err != nil {
			if err := p.drain(); err != nil {
				return nil, err
			}
			r.err = err
		}
		return r, nil
	}
	fields, err := p.values()
	if err != nil {
		var syntax *syntaxError
		if !errors.As(err, &syntax) {
			return nil, err
		}
		if err := p.drain(); err != nil {
			return nil, err
		}
		r.err = err
		return r, nil
	}
	r.fields = fields
	if p.tooLarge != nil {
		r.err = p.tooLarge
	}
	return r, nil
}

// Numbers of the data of a SEARCH response, a parenthesized MODSEQ group skipped; an error for anything else, a literal included
func searchNumbers(s string) ([]uint32, error) {
	nums := make([]uint32, 0, strings.Count(s, " ")+1)
	depth := 0
	for token := range strings.FieldsSeq(s) {
		if depth > 0 || token[0] == '(' {
			depth += strings.Count(token, "(") - strings.Count(token, ")")
			continue
		}
		n, err := strconv.ParseUint(token, 10, 32)
		if err != nil || n == 0 {
			return nil, &syntaxError{msg: "SEARCH response holds " + strconv.Quote(token[:min(len(token), 200)])}
		}
		nums = append(nums, uint32(n))
	}
	return nums, nil
}

// Response code, its argument and the text of a status response
func respText(s string) (code, arg, text string) {
	if strings.HasPrefix(s, "[") {
		if end := strings.IndexByte(s, ']'); end > 0 {
			code, arg, _ = strings.Cut(s[1:end], " ")
			return strings.ToUpper(code), arg, strings.TrimSpace(s[end+1:])
		}
	}
	return "", "", s
}

// Reader of the data of one response: its line and the literals and lines that continue it
type dataParser struct {
	c        *IMAP
	line     string
	pos      int
	tooLarge *LiteralTooLargeError // first literal over the limit that was read past
}

func (p *dataParser) syntax(msg string) error { return &syntaxError{msg: msg} }

func (p *dataParser) skipSpaces() {
	for p.pos < len(p.line) && p.line[p.pos] == ' ' {
		p.pos++
	}
}

// Every value up to the end of the response
func (p *dataParser) values() ([]any, error) {
	var out []any
	for {
		p.skipSpaces()
		if p.pos >= len(p.line) {
			return out, nil
		}
		v, err := p.value(0)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

// One value at the current position
func (p *dataParser) value(depth int) (any, error) {
	switch p.line[p.pos] {
	case '(':
		if depth >= maxNesting {
			return nil, p.syntax("lists nested too deep")
		}
		p.pos++
		list := []any{}
		for {
			p.skipSpaces()
			if p.pos >= len(p.line) {
				return nil, p.syntax("unterminated list")
			}
			if p.line[p.pos] == ')' {
				p.pos++
				return list, nil
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
	case ')':
		return nil, p.syntax("unbalanced )")
	case '"':
		return p.quoted()
	case '{':
		return p.literal()
	case '~':
		if p.pos+1 < len(p.line) && p.line[p.pos+1] == '{' {
			p.pos++
			return p.literal()
		}
	}
	return p.atom(), nil
}

// Atom at the current position, a bracketed section such as BODY[HEADER.FIELDS (FROM)] read whole, nil for NIL
func (p *dataParser) atom() any {
	start := p.pos
scan:
	for p.pos < len(p.line) {
		switch p.line[p.pos] {
		case ' ', '(', ')':
			break scan
		case '[':
			if end := strings.IndexByte(p.line[p.pos:], ']'); end >= 0 {
				p.pos += end + 1
				continue
			}
		}
		p.pos++
	}
	s := p.line[start:p.pos]
	if strings.EqualFold(s, "NIL") {
		return nil
	}
	return s
}

// Quoted string at the current position with its escapes undone
func (p *dataParser) quoted() (any, error) {
	p.pos++
	out := []byte{}
	for p.pos < len(p.line) {
		c := p.line[p.pos]
		p.pos++
		switch c {
		case '"':
			return out, nil
		case '\\':
			if p.pos < len(p.line) {
				c = p.line[p.pos]
				p.pos++
			}
		}
		out = append(out, c)
	}
	return nil, p.syntax("unterminated quoted string")
}

// Literal announced at the end of the current line, read from the stream with the line that continues the response; one over the limit is read past and stands as NIL, the response then reporting a drained *LiteralTooLargeError
func (p *dataParser) literal() (any, error) {
	end := strings.IndexByte(p.line[p.pos:], '}')
	if end < 0 || p.pos+end+1 != len(p.line) {
		return nil, p.syntax("literal not at the end of a line")
	}
	n, ok := literalSize(p.line[p.pos+1 : p.pos+end])
	if !ok {
		return nil, p.syntax("malformed literal size")
	}
	var v any
	if limit := p.c.literalLimit(); n > limit {
		if err := p.skipLiteral(n, limit); err != nil {
			return nil, err
		}
	} else {
		data, err := readLiteral(p.c.r, n)
		if err != nil {
			return nil, err
		}
		v = data
	}
	line, err := p.c.readLine(maxIMAPLine)
	if err != nil {
		return nil, err
	}
	p.line, p.pos = line, 0
	return v, nil
}

// Reads past a literal over the limit and records it for the response, or refuses it when even reading past it would take more than maxLiteralDrain octets, which leaves the connection to be closed
func (p *dataParser) skipLiteral(n, limit int64) error {
	if n > maxLiteralDrain {
		return &LiteralTooLargeError{Size: n, Limit: limit}
	}
	if _, err := io.CopyN(io.Discard, p.c.r, n); err != nil {
		return fmt.Errorf("imap: reading past a literal of %d bytes: %w", n, err)
	}
	if p.tooLarge == nil {
		p.tooLarge = &LiteralTooLargeError{Size: n, Limit: limit, Drained: true}
	}
	return nil
}

// Octets of a literal of n bytes, the buffer growing only with what has arrived, so a size the server announces but does not send reserves no memory
func readLiteral(r io.Reader, n int64) ([]byte, error) {
	const chunk = 64 << 10
	data := make([]byte, 0, min(n, chunk))
	for int64(len(data)) < n {
		step := int(min(n-int64(len(data)), chunk))
		data = slices.Grow(data, step)
		got, err := io.ReadFull(r, data[len(data):len(data)+step])
		data = data[:len(data)+got]
		if err != nil {
			return nil, fmt.Errorf("imap: reading a literal of %d bytes: %w", n, err)
		}
	}
	return data, nil
}

// Reads past the literals the rest of a malformed response announces, keeping the stream in step, unless one is larger than maxLiteralDrain
func (p *dataParser) drain() error {
	for {
		n, ok := trailingLiteral(p.line)
		if !ok {
			return nil
		}
		if n > maxLiteralDrain {
			return &LiteralTooLargeError{Size: n, Limit: p.c.literalLimit()}
		}
		if _, err := io.CopyN(io.Discard, p.c.r, n); err != nil {
			return fmt.Errorf("imap: reading past a literal of %d bytes: %w", n, err)
		}
		line, err := p.c.readLine(maxIMAPLine)
		if err != nil {
			return err
		}
		p.line, p.pos = line, 0
	}
}

// Size of the literal a line announces at its end
func trailingLiteral(line string) (int64, bool) {
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	open := strings.LastIndexByte(line, '{')
	if open < 0 {
		return 0, false
	}
	return literalSize(line[open+1 : len(line)-1])
}

// Octet count of a literal announcement such as 42 or 42+, the largest int64 for digits beyond it, false for text that is no count
func literalSize(digits string) (int64, bool) {
	n, err := strconv.ParseUint(strings.TrimSuffix(digits, "+"), 10, 63)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return int64(n), true
}

// Text of a string, atom or NIL value
func valueText(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// Piece of a command line: text written as it is, or a string sent as a literal
type cmdArg struct {
	text    string
	literal []byte
	isLit   bool
}

// Argument carrying a string: an atom when atomOK and it is one, a quoted string when it can be, else a literal
func stringArg(s string, atomOK bool) (cmdArg, error) {
	if strings.IndexByte(s, 0) >= 0 {
		return cmdArg{}, fmt.Errorf("imap: %w: string contains NUL", errArgument)
	}
	if atomOK && isAtom(s) {
		return cmdArg{text: s}, nil
	}
	if quotable(s) {
		var b strings.Builder
		b.Grow(len(s) + 2)
		b.WriteByte('"')
		for i := 0; i < len(s); i++ {
			if s[i] == '"' || s[i] == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(s[i])
		}
		b.WriteByte('"')
		return cmdArg{text: b.String()}, nil
	}
	return cmdArg{literal: []byte(s), isLit: true}, nil
}

// Whether s can go unquoted as an astring
func isAtom(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c >= 0x7f || strings.IndexByte(`(){%*"\`, c) >= 0 {
			return false
		}
	}
	return true
}

// Whether s fits a quoted string: 7 bit text without line breaks, short enough for every server
func quotable(s string) bool {
	if len(s) > 1000 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 0x80 || c == '\r' || c == '\n' {
			return false
		}
	}
	return true
}
