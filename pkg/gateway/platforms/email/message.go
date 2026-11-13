package email

import (
	"bytes"
	"encoding/base64"
	"net/mail"
	"net/textproto"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// Nesting depth past which a part is kept as an opaque leaf
const maxMIMEDepth = 50

// One MIME entity with its headers, raw content and parsed children
type mimeEntity struct {
	header      mail.Header
	body        []byte
	children    []*mimeEntity
	container   bool
	defaultType string
}

// Line of raw starting at pos without its terminator and the offset of the next line
func nextLine(raw []byte, pos int) (line []byte, eolStart, next int) {
	for i := pos; i < len(raw); i++ {
		switch raw[i] {
		case '\n':
			return raw[pos:i], i, i + 1
		case '\r':
			if i+1 < len(raw) && raw[i+1] == '\n' {
				return raw[pos:i], i, i + 2
			}
			return raw[pos:i], i, i + 1
		}
	}
	return raw[pos:], len(raw), len(raw)
}

// Whether the line opens a header field, its name being printable ASCII other than colon
func isHeaderField(line []byte) (name, value []byte, ok bool) {
	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return nil, nil, false
	}
	for _, c := range line[:colon] {
		if c < 0x21 || c > 0x7e {
			return nil, nil, false
		}
	}
	return line[:colon], line[colon+1:], true
}

// Header block of raw and the body after it, a non header line ending the block the way mail readers tolerate
func splitHeaders(raw []byte) (mail.Header, []byte) {
	h := mail.Header{}
	var key string
	var val []byte
	flush := func() {
		if key != "" {
			h[key] = append(h[key], strings.TrimSpace(string(val)))
		}
		key, val = "", nil
	}
	for pos, first := 0, true; pos < len(raw); first = false {
		line, _, next := nextLine(raw, pos)
		switch {
		case len(line) == 0:
			flush()
			return h, raw[next:]
		case first && bytes.HasPrefix(line, []byte("From ")):
		case line[0] == ' ' || line[0] == '\t':
			if key != "" {
				val = append(val, line...)
			}
		default:
			name, value, ok := isHeaderField(line)
			if !ok {
				flush()
				return h, raw[pos:]
			}
			flush()
			if len(name) > 0 {
				key = textproto.CanonicalMIMEHeaderKey(string(name))
				val = append([]byte(nil), bytes.TrimLeft(value, " \t")...)
			}
		}
		pos = next
	}
	flush()
	return h, nil
}

// Entity tree of a raw message whose missing Content-Type defaults to defaultType
func parseEntity(raw []byte, defaultType string, depth int) *mimeEntity {
	h, body := splitHeaders(raw)
	e := &mimeEntity{header: h, body: body, defaultType: defaultType}
	if depth >= maxMIMEDepth {
		return e
	}
	ctype := e.contentType()
	switch {
	case strings.HasPrefix(ctype, "multipart/"):
		boundary := strings.TrimRight(e.param("Content-Type", "boundary"), " \t")
		if boundary == "" {
			return e
		}
		parts, ok := splitMultipart(body, boundary)
		if !ok {
			return e
		}
		childType := "text/plain"
		if ctype == "multipart/digest" {
			childType = "message/rfc822"
		}
		e.container = true
		for _, part := range parts {
			e.children = append(e.children, parseEntity(part, childType, depth+1))
		}
	case strings.HasPrefix(ctype, "message/"):
		e.container = true
		if ctype != "message/delivery-status" {
			e.children = []*mimeEntity{parseEntity(body, "text/plain", depth+1)}
		}
	}
	return e
}

// Whether the line is the boundary delimiter, and whether it is the closing one
func delimiterLine(line []byte, delim string) (isDelim, closing bool) {
	if !bytes.HasPrefix(line, []byte(delim)) {
		return false, false
	}
	rest := line[len(delim):]
	if bytes.HasPrefix(rest, []byte("--")) {
		closing, rest = true, rest[2:]
	}
	if len(bytes.Trim(rest, " \t")) != 0 {
		return false, false
	}
	return true, closing
}

// Bodies between the boundary delimiters, an unterminated last part running to the end, false without any delimiter
func splitMultipart(body []byte, boundary string) ([][]byte, bool) {
	delim := "--" + boundary
	var parts [][]byte
	found, inPart := false, false
	partStart, prevEOL := 0, 0
	for pos := 0; pos < len(body); {
		line, eolStart, next := nextLine(body, pos)
		if isDelim, closing := delimiterLine(line, delim); isDelim {
			if inPart {
				parts = append(parts, body[partStart:max(partStart, prevEOL)])
			}
			found = true
			if closing {
				return parts, true
			}
			inPart, partStart = true, next
		}
		prevEOL, pos = eolStart, next
	}
	if inPart {
		parts = append(parts, body[partStart:])
	}
	return parts, found
}

// Lowercased type and subtype, the default when absent and text/plain when malformed
func (e *mimeEntity) contentType() string {
	v, ok := e.header["Content-Type"]
	if !ok || len(v) == 0 {
		return e.defaultType
	}
	mt, _, _ := strings.Cut(v[0], ";")
	mt = strings.ToLower(strings.TrimSpace(mt))
	if strings.Count(mt, "/") != 1 {
		return "text/plain"
	}
	return mt
}

// Subtype of the content type
func (e *mimeEntity) contentSubtype() string {
	_, sub, _ := strings.Cut(e.contentType(), "/")
	return sub
}

// Lowercased disposition type, empty when the part has none
func (e *mimeEntity) disposition() string {
	d, _, _ := strings.Cut(e.header.Get("Content-Disposition"), ";")
	return strings.ToLower(strings.TrimSpace(d))
}

// Named parameter of a structured header, RFC 2231 encoded values decoded
func (e *mimeEntity) param(header, name string) string {
	return headerParams(e.header.Get(header))[name]
}

// Declared charset of the part, empty when it names none
func (e *mimeEntity) charset() string {
	return strings.ToLower(strings.TrimSpace(e.param("Content-Type", "charset")))
}

// Attachment file name from the disposition, else the content type name
func (e *mimeEntity) filename() string {
	params := headerParams(e.header.Get("Content-Disposition"))
	name, ok := params["filename"]
	if !ok {
		name, ok = headerParams(e.header.Get("Content-Type"))["name"]
	}
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

// Content of a leaf part with its transfer encoding removed, nil for containers
func (e *mimeEntity) payload() []byte {
	if e.container {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(e.header.Get("Content-Transfer-Encoding"))) {
	case "base64":
		return decodeBase64Lenient(e.body)
	case "quoted-printable":
		return decodeQuotedPrintable(e.body)
	case "x-uuencode", "uuencode", "uue", "x-uue":
		return decodeUU(e.body)
	}
	return e.body
}

// Every entity of the tree in depth first order starting with this one
func (e *mimeEntity) walk() []*mimeEntity {
	out := []*mimeEntity{e}
	for _, c := range e.children {
		out = append(out, c.walk()...)
	}
	return out
}

// Base64 content with whitespace, stray characters and missing padding tolerated
func decodeBase64Lenient(b []byte) []byte {
	clean := make([]byte, 0, len(b))
	for _, c := range b {
		if c == '=' {
			break
		}
		if isBase64Char(c) {
			clean = append(clean, c)
		}
	}
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	out := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
	n, err := base64.RawStdEncoding.Decode(out, clean)
	if err != nil {
		return b
	}
	return out[:n]
}

// Value of one hex digit, false for any other byte
func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Quoted-printable content with soft breaks joined and malformed escapes kept literally
func decodeQuotedPrintable(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '=' {
			out = append(out, b[i])
			continue
		}
		switch {
		case i+1 == len(b):
		case b[i+1] == '\n':
			i++
		case b[i+1] == '\r' && i+2 < len(b) && b[i+2] == '\n':
			i += 2
		case b[i+1] == '\r':
			i++
		case i+2 < len(b):
			hi, ok1 := hexValue(b[i+1])
			lo, ok2 := hexValue(b[i+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				i += 2
			} else {
				out = append(out, '=')
			}
		default:
			out = append(out, '=')
		}
	}
	return out
}

// Uuencoded content between its begin and end lines
func decodeUU(b []byte) []byte {
	var out []byte
	started := false
	for pos := 0; pos < len(b); {
		line, _, next := nextLine(b, pos)
		pos = next
		if !started {
			started = bytes.HasPrefix(line, []byte("begin "))
			continue
		}
		if bytes.Equal(bytes.TrimSpace(line), []byte("end")) {
			break
		}
		if len(line) == 0 {
			continue
		}
		n := int((line[0] - ' ') & 63)
		data := line[1:]
		var chunk []byte
		for i := 0; i < len(data) && len(chunk) < n; i += 4 {
			var q [4]byte
			for k := 0; k < 4; k++ {
				if i+k < len(data) {
					q[k] = (data[i+k] - ' ') & 63
				}
			}
			chunk = append(chunk, q[0]<<2|q[1]>>4, q[1]<<4|q[2]>>2, q[2]<<6|q[3])
		}
		if len(chunk) > n {
			chunk = chunk[:n]
		}
		out = append(out, chunk...)
	}
	if !started {
		return b
	}
	return out
}

// Parameter value split on semicolons outside quoted strings
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	quoted, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case r == ';' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	return append(out, cur.String())
}

// Quoted string body with its escapes removed, the input itself when it is not quoted
func unquoteParam(v string) string {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	var out strings.Builder
	inner := v[1 : len(v)-1]
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			i++
		}
		out.WriteByte(inner[i])
	}
	return out.String()
}

// Percent escapes decoded, malformed escapes kept literally
func percentDecode(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := hexValue(s[i+1])
			lo, ok2 := hexValue(s[i+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				i += 2
				continue
			}
		}
		out = append(out, s[i])
	}
	return out
}

// Lowercased parameters of a structured header value with RFC 2231 continuations and charsets decoded
func headerParams(value string) map[string]string {
	params := map[string]string{}
	_, rest, ok := strings.Cut(value, ";")
	if !ok {
		return params
	}
	type piece struct {
		index   int
		encoded bool
		value   string
	}
	extended := map[string][]piece{}
	for _, tok := range splitParams(rest) {
		k, v, ok := strings.Cut(tok, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		if !ok || k == "" {
			continue
		}
		v = unquoteParam(strings.TrimSpace(v))
		name, suffix, star := strings.Cut(k, "*")
		if !star {
			if _, seen := params[k]; !seen {
				params[k] = v
			}
			continue
		}
		p := piece{encoded: strings.HasSuffix(suffix, "*") || suffix == "", value: v}
		if digits := strings.TrimSuffix(suffix, "*"); digits != "" {
			n, err := strconv.Atoi(digits)
			if err != nil {
				continue
			}
			p.index = n
		}
		extended[name] = append(extended[name], p)
	}
	for name, pieces := range extended {
		sort.SliceStable(pieces, func(i, j int) bool { return pieces[i].index < pieces[j].index })
		charset := ""
		var raw []byte
		for i, p := range pieces {
			v := p.value
			if p.encoded && i == 0 {
				if cs, after, ok := strings.Cut(v, "'"); ok {
					if _, text, ok := strings.Cut(after, "'"); ok {
						charset, v = cs, text
					}
				}
			}
			if p.encoded {
				raw = append(raw, percentDecode(v)...)
			} else {
				raw = append(raw, v...)
			}
		}
		params[name] = decodeText(raw, charset)
	}
	return params
}

// Text of the first non attachment part of the content type, empty when none carries any
func firstBodyPart(root *mimeEntity, contentType string) string {
	for _, part := range root.walk() {
		if part.disposition() == "attachment" || part.contentType() != contentType {
			continue
		}
		if payload := part.payload(); len(payload) > 0 {
			return decodeText(payload, part.charset())
		}
	}
	return ""
}

// Plain text body of the message, the plain part first and the HTML part rendered to text otherwise
func extractTextBody(root *mimeEntity) string {
	if root.container {
		if text := firstBodyPart(root, "text/plain"); text != "" {
			return text
		}
		if markup := firstBodyPart(root, "text/html"); markup != "" {
			return htmlToText(markup)
		}
		return ""
	}
	text := ""
	if payload := root.payload(); len(payload) > 0 {
		text = decodeText(payload, root.charset())
	}
	if root.contentType() == "text/html" {
		return htmlToText(text)
	}
	return text
}

// Elements whose content is never shown as text
var hiddenElements = map[string]bool{"script": true, "style": true, "head": true, "title": true, "template": true, "noscript": true}

// Elements that sit on their own lines when rendered
var blockElements = map[string]bool{
	"div": true, "li": true, "tr": true, "table": true, "blockquote": true, "pre": true, "hr": true, "ul": true,
	"ol": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// Visible text of an HTML document with breaks and paragraphs as newlines and runs of blank lines collapsed
func htmlToText(markup string) string {
	z := html.NewTokenizer(strings.NewReader(markup))
	var out strings.Builder
	breakLine := func() {
		if s := out.String(); s != "" && !strings.HasSuffix(s, "\n") {
			out.WriteByte('\n')
		}
	}
	hidden := 0
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		name, _ := z.TagName()
		tag := string(name)
		switch tt {
		case html.TextToken:
			if hidden == 0 {
				out.WriteString(strings.ReplaceAll(string(z.Text()), " ", " "))
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			switch {
			case hiddenElements[tag] && tt == html.StartTagToken:
				hidden++
			case hiddenElements[tag] && tt == html.EndTagToken && hidden > 0:
				hidden--
			case tag == "br" || tag == "p":
				out.WriteByte('\n')
			case blockElements[tag]:
				breakLine()
			}
		}
	}
	text := out.String()
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(text)
}
