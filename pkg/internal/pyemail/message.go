package pyemail

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	headerLineRE  = regexp.MustCompile(`^(From |[\x21-\x39\x3b-\x7e]*:|[\t ])`)
	boundaryEndRE = regexp.MustCompile(`^(--)?([ \t]*)(\r\n|\r|\n)?$`)
)

// Message is a parsed MIME message under the compat32 policy, header values and payload kept as raw bytes
type Message struct {
	headers     [][2]string
	payload     string
	parts       []*Message
	multipart   bool
	defaultType string
}

// Lines of a text with their terminators kept, split at CRLF, CR and LF
func splitKeepEnds(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			lines = append(lines, text[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			lines = append(lines, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

// Line source with pushback and a stack of predicates that end the current part
type subFile struct {
	lines []string
	eof   []func(line string) bool
}

// Next line, empty at the end of the input or when the line ends the current part
func (f *subFile) readline() string {
	if len(f.lines) == 0 {
		return ""
	}
	line := f.lines[0]
	f.lines = f.lines[1:]
	for i := len(f.eof) - 1; i >= 0; i-- {
		if f.eof[i](line) {
			f.lines = append([]string{line}, f.lines...)
			return ""
		}
	}
	return line
}

func (f *subFile) unreadline(line string) { f.lines = append([]string{line}, f.lines...) }

// Every remaining line of the current part
func (f *subFile) rest() []string {
	var out []string
	for {
		line := f.readline()
		if line == "" {
			return out
		}
		out = append(out, line)
	}
}

// Feed parser state, the message stack with the message being built on top
type feedParser struct {
	input       *subFile
	headersOnly bool
	stack       []*Message
	cur, last   *Message
}

// Starts a message attached to the one being built, a digest part defaulting to an embedded message
func (p *feedParser) newMessage() {
	msg := &Message{defaultType: "text/plain"}
	if p.cur != nil && p.cur.GetContentType() == "multipart/digest" {
		msg.defaultType = "message/rfc822"
	}
	if len(p.stack) > 0 {
		parent := p.stack[len(p.stack)-1]
		parent.parts, parent.multipart = append(parent.parts, msg), true
	}
	p.stack = append(p.stack, msg)
	p.cur, p.last = msg, msg
}

// Finishes the message on top of the stack and returns it
func (p *feedParser) popMessage() *Message {
	retval := p.stack[len(p.stack)-1]
	p.stack = p.stack[:len(p.stack)-1]
	p.cur = nil
	if len(p.stack) > 0 {
		p.cur = p.stack[len(p.stack)-1]
	}
	return retval
}

// Whether a line is only a line break
func isLineBreak(line string) bool {
	return strings.HasPrefix(line, "\r") || strings.HasPrefix(line, "\n")
}

// Text without one trailing line break
func stripEOL(text string) string {
	for _, eol := range []string{"\r\n", "\r", "\n"} {
		if strings.HasSuffix(text, eol) {
			return text[:len(text)-len(eol)]
		}
	}
	return text
}

// Parses one message from the input, recursing into multipart and embedded message parts
func (p *feedParser) parsegen() {
	p.newMessage()
	var headers []string
	for {
		line := p.input.readline()
		if line == "" {
			break
		}
		if !headerLineRE.MatchString(line) {
			if !isLineBreak(line) {
				p.input.unreadline(line)
			}
			break
		}
		headers = append(headers, line)
	}
	p.parseHeaders(headers)
	if p.headersOnly {
		p.cur.payload = strings.Join(p.input.rest(), "")
		return
	}
	if p.cur.GetContentType() == "message/delivery-status" {
		for {
			p.input.eof = append(p.input.eof, isLineBreak)
			p.parsegen()
			p.popMessage()
			p.input.eof = p.input.eof[:len(p.input.eof)-1]
			p.input.readline()
			line := p.input.readline()
			if line == "" {
				break
			}
			p.input.unreadline(line)
		}
		return
	}
	if p.cur.GetContentMaintype() == "message" {
		p.parsegen()
		p.popMessage()
		return
	}
	if p.cur.GetContentMaintype() == "multipart" {
		boundary, ok := p.cur.GetBoundary()
		if !ok {
			p.cur.payload = strings.Join(p.input.rest(), "")
			return
		}
		separator := "--" + boundary
		boundaryMatch := func(line string) (end, matched bool) {
			if !strings.HasPrefix(line, separator) {
				return false, false
			}
			m := boundaryEndRE.FindStringSubmatch(line[len(separator):])
			if m == nil {
				return false, false
			}
			return m[1] != "", true
		}
		isBoundary := func(line string) bool {
			_, matched := boundaryMatch(line)
			return matched
		}
		capturingPreamble, closeBoundarySeen := true, false
		var preamble []string
		for {
			line := p.input.readline()
			if line == "" {
				break
			}
			end, matched := boundaryMatch(line)
			if !matched {
				preamble = append(preamble, line)
				continue
			}
			if end {
				closeBoundarySeen = true
				break
			}
			if capturingPreamble {
				capturingPreamble = false
				p.input.unreadline(line)
				continue
			}
			for {
				line = p.input.readline()
				if !isBoundary(line) {
					if line != "" {
						p.input.unreadline(line)
					}
					break
				}
			}
			p.input.eof = append(p.input.eof, isBoundary)
			p.parsegen()
			if p.last.GetContentMaintype() != "multipart" && !p.last.multipart {
				p.last.payload = stripEOL(p.last.payload)
			}
			p.input.eof = p.input.eof[:len(p.input.eof)-1]
			p.popMessage()
			p.last = p.cur
		}
		if capturingPreamble {
			p.cur.payload = strings.Join(preamble, "")
			p.input.rest()
			return
		}
		if closeBoundarySeen {
			p.input.rest()
		}
		return
	}
	p.cur.payload = strings.Join(p.input.rest(), "")
}

// Stores the header lines on the message being built, continuation lines joined to their header
func (p *feedParser) parseHeaders(lines []string) {
	lastheader := ""
	var lastvalue []string
	flush := func() {
		name, value, _ := strings.Cut(lastvalue[0], ":")
		value = strings.TrimLeft(value+strings.Join(lastvalue[1:], ""), " \t\r\n")
		p.cur.headers = append(p.cur.headers, [2]string{name, strings.TrimRight(value, "\r\n")})
	}
	for lineno, line := range lines {
		if line[0] == ' ' || line[0] == '\t' {
			if lastheader == "" {
				continue
			}
			lastvalue = append(lastvalue, line)
			continue
		}
		if lastheader != "" {
			flush()
			lastheader, lastvalue = "", nil
		}
		if strings.HasPrefix(line, "From ") {
			if lineno == 0 {
				continue
			}
			if lineno == len(lines)-1 {
				p.input.unreadline(line)
				return
			}
			continue
		}
		i := strings.Index(line, ":")
		if i == 0 {
			continue
		}
		lastheader, lastvalue = line[:i], []string{line}
	}
	if lastheader != "" {
		flush()
	}
}

// Parses a whole message from bytes, headers only when asked with the rest of the text as its payload
func ParseBytes(raw []byte, headersOnly bool) *Message {
	p := &feedParser{input: &subFile{lines: splitKeepEnds(string(raw))}, headersOnly: headersOnly}
	p.parsegen()
	root := p.popMessage()
	return root
}

// First value of a header matched without case, false when the message has none
func (m *Message) Get(name string) (HeaderValue, bool) {
	lower := strings.ToLower(name)
	for _, h := range m.headers {
		if strings.ToLower(h[0]) == lower {
			return HeaderValue(h[1]), true
		}
	}
	return "", false
}

// Every value of a header matched without case in message order
func (m *Message) GetAll(name string) []HeaderValue {
	lower := strings.ToLower(name)
	var out []HeaderValue
	for _, h := range m.headers {
		if strings.ToLower(h[0]) == lower {
			out = append(out, HeaderValue(h[1]))
		}
	}
	return out
}

// Header names and values in message order
func (m *Message) Items() [][2]string {
	return append([][2]string(nil), m.headers...)
}

// Whether the payload is a list of sub messages
func (m *Message) IsMultipart() bool { return m.multipart }

// The message and every sub message in depth first order
func (m *Message) Walk() []*Message {
	out := []*Message{m}
	if m.multipart {
		for _, part := range m.parts {
			out = append(out, part.Walk()...)
		}
	}
	return out
}

// Lowercased maintype/subtype of the Content-Type header, the default type without one and text/plain for a malformed one
func (m *Message) GetContentType() string {
	value, ok := m.Get("content-type")
	if !ok {
		return m.defaultType
	}
	ctype, _, _ := strings.Cut(value.Str(), ";")
	ctype = pystr.Lower(pystr.Strip(ctype))
	if strings.Count(ctype, "/") != 1 {
		return "text/plain"
	}
	return ctype
}

func (m *Message) GetContentMaintype() string {
	main, _, _ := strings.Cut(m.GetContentType(), "/")
	return main
}

func (m *Message) GetContentSubtype() string {
	_, sub, _ := strings.Cut(m.GetContentType(), "/")
	return sub
}

// Unquoted parameter of a header matched without case, false when the header or the parameter is missing
func (m *Message) getParam(param, header string) (Param, bool) {
	value, ok := m.Get(header)
	if !ok {
		return Param{}, false
	}
	params, err := GetParams(value.Str())
	if err != nil {
		return Param{}, false
	}
	for _, p := range params {
		if strings.EqualFold(p.Name, param) {
			return p, true
		}
	}
	return Param{}, false
}

// Boundary parameter of the Content-Type header with trailing white space removed
func (m *Message) GetBoundary() (string, bool) {
	p, ok := m.getParam("boundary", "content-type")
	if !ok {
		return "", false
	}
	return pystr.Rstrip(CollapseRFC2231Value(p)), true
}

// File name of the payload from the Content-Disposition filename, else the Content-Type name
func (m *Message) GetFilename() (string, bool) {
	p, ok := m.getParam("filename", "content-disposition")
	if !ok {
		if p, ok = m.getParam("name", "content-type"); !ok {
			return "", false
		}
	}
	return pystr.Strip(CollapseRFC2231Value(p)), true
}

// Lowercased charset parameter of the Content-Type header, false when missing or not ASCII
func (m *Message) GetContentCharset() (string, bool) {
	p, ok := m.getParam("charset", "content-type")
	if !ok {
		return "", false
	}
	charset := p.Value
	if p.RFC2231 {
		charset = CollapseRFC2231Value(p)
	}
	for i := 0; i < len(charset); i++ {
		if charset[i] >= 0x80 {
			return "", false
		}
	}
	return pystr.Lower(charset), true
}

// Payload decoded by its Content-Transfer-Encoding, nil for a multipart message
func (m *Message) GetPayloadDecoded() []byte {
	if m.multipart {
		return nil
	}
	cte := ""
	if value, ok := m.Get("content-transfer-encoding"); ok {
		cte = pystr.Lower(pystr.Strip(value.Str()))
	}
	payload := []byte(m.payload)
	switch cte {
	case "quoted-printable":
		return decodeQP(payload, false)
	case "base64":
		return decodeB(joinLines(payload))
	case "x-uuencode", "uuencode", "uue", "x-uue":
		if decoded, ok := decodeUU(payload); ok {
			return decoded
		}
	}
	return payload
}
