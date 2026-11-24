package mailutil

import (
	"bytes"
	"iter"
	"strings"
	"unicode"
)

// Parsed message or one part of it: a leaf holding content, or a container of parts
type Message struct {
	Header      Header
	body        []byte
	parts       []*Message
	multipart   bool
	defaultType string
}

// Bounds of one parse, so hostile input can neither nest without end nor make the tree many times larger than the message
const (
	maxDepth  = 64     // deepest nesting of parts parsed, anything below kept as an unparsed leaf
	maxParts  = 10000  // most parts the tree holds, later ones left out
	maxFields = 100000 // most header fields the tree holds, later ones left out
)

// What is left of the bounds of one parse
type parser struct {
	parts   int // parts the tree may still take
	fields  int // header fields the tree may still take
	decoded int // bytes of embedded messages the transfer decoding may still produce, the size of the message in all
}

// MIME tree of a raw message, read as leniently as real mail requires so nothing makes it fail: missing or stray boundaries, absent charsets and malformed header lines are tolerated; the tree reads from raw without copying it and holds at most maxParts parts and maxFields header fields
func Parse(raw []byte) *Message {
	p := &parser{parts: maxParts - 1, fields: maxFields, decoded: len(raw)}
	return p.entity(raw, "text/plain", 0)
}

// One entity and the parts below it, defaultType standing for an absent Content-Type
func (p *parser) entity(raw []byte, defaultType string, depth int) *Message {
	header, bodyAt := splitHeader(raw, p.fields)
	p.fields -= len(header)
	m := &Message{Header: header, defaultType: defaultType}
	body := raw[bodyAt:]
	if depth >= maxDepth || p.parts <= 0 {
		m.body = body
		return m
	}
	ctype := m.ContentType()
	switch major, _, _ := strings.Cut(ctype, "/"); {
	case ctype == "message/delivery-status":
		m.parts, m.multipart = p.statusBlocks(p.embedded(m, body)), true
	case major == "message":
		p.parts--
		m.parts, m.multipart = []*Message{p.entity(p.embedded(m, body), "text/plain", depth+1)}, true
	case major == "multipart":
		boundary := m.boundary()
		if boundary == "" {
			m.body = body
			break
		}
		parts, preamble, opened := splitParts(body, boundary, p.parts)
		if !opened {
			m.body = preamble
			break
		}
		childType := "text/plain"
		if ctype == "multipart/digest" {
			childType = "message/rfc822"
		}
		p.parts -= len(parts)
		m.parts = make([]*Message, len(parts))
		for i, part := range parts {
			m.parts[i] = p.entity(part, childType, depth+1)
		}
		m.multipart = true
	default:
		m.body = body
	}
	return m
}

// Body of a message part with its transfer encoding undone while the decoding budget lasts, as it is once the budget is spent; RFC 2046 allows no encoding there, but senders use base64 and quoted-printable
func (p *parser) embedded(m *Message, body []byte) []byte {
	switch m.transferEncoding() {
	case "base64", "quoted-printable", "x-uuencode", "uuencode", "uue", "x-uue":
		if len(body) > p.decoded {
			return body
		}
		p.decoded -= len(body)
		return m.transferDecoded(body)
	}
	return body
}

type boundaryKind uint8

const (
	notBoundary boundaryKind = iota
	openBoundary
	closeBoundary
)

// Whether a line is a delimiter of the multipart whose boundary line starts with sep, the closing one or another
func boundaryLine(line, sep []byte) boundaryKind {
	rest, ok := bytes.CutPrefix(line, sep)
	if !ok {
		return notBoundary
	}
	kind := openBoundary
	if r, ok := bytes.CutPrefix(rest, []byte("--")); ok {
		rest, kind = r, closeBoundary
	}
	if len(bytes.Trim(rest, " \t")) != 0 {
		return notBoundary
	}
	return kind
}

// Entities between the boundary lines of a multipart body, at most limit of them, and whether an opening boundary was found, the text before it returned either way; a missing closing boundary ends the last part at the end of the body
func splitParts(body []byte, boundary string, limit int) (parts [][]byte, preamble []byte, opened bool) {
	sep := []byte("--" + boundary)
	partAt := 0
	for pos := 0; pos < len(body); {
		line, next := lineAt(body, pos)
		if kind := boundaryLine(line, sep); kind != notBoundary {
			switch {
			case !opened:
				preamble = body[:pos]
				if kind == closeBoundary {
					return nil, preamble, false
				}
				opened = true
			case pos > partAt:
				if parts = append(parts, trimEOL(body[partAt:pos])); len(parts) == limit {
					return parts, preamble, true
				}
			}
			if kind == closeBoundary {
				return parts, preamble, true
			}
			partAt = next
		}
		pos = next
	}
	if !opened {
		return nil, body, false
	}
	if partAt < len(body) {
		parts = append(parts, trimEOL(body[partAt:]))
	}
	return parts, preamble, true
}

// Field blocks of a message/delivery-status body, one header only part for each run of fields between blank lines, while the bounds of the parse last
func (p *parser) statusBlocks(body []byte) []*Message {
	var out []*Message
	for pos := 0; pos < len(body) && p.parts > 0; {
		line, next := lineAt(body, pos)
		if len(bytes.TrimSpace(line)) == 0 {
			pos = next
			continue
		}
		header, n := splitHeader(body[pos:], p.fields)
		if len(header) > 0 {
			p.parts--
			p.fields -= len(header)
			out = append(out, &Message{Header: header, defaultType: "text/plain"})
		}
		pos += max(n, next-pos)
	}
	return out
}

// Whether the part holds parts: a multipart with boundaries, or a message part holding the message it embeds
func (m *Message) IsMultipart() bool { return m.multipart }

// Children in order, the embedded message alone for a message part, none for a leaf
func (m *Message) Parts() []*Message { return m.parts }

// The message and every part below it, depth first with each container before its parts
func (m *Message) Walk() iter.Seq[*Message] {
	return func(yield func(*Message) bool) { m.walk(yield) }
}

func (m *Message) walk(yield func(*Message) bool) bool {
	if !yield(m) {
		return false
	}
	for _, part := range m.parts {
		if !part.walk(yield) {
			return false
		}
	}
	return true
}

// Lower cased type/subtype of the Content-Type field, the default of the context when the field is absent and text/plain when it is malformed
func (m *Message) ContentType() string {
	v, ok := m.Header.raw("Content-Type")
	if !ok {
		return m.defaultType
	}
	ctype, _, _ := strings.Cut(v, ";")
	ctype = strings.ToLower(strings.Join(strings.Fields(ctype), ""))
	if strings.Count(ctype, "/") != 1 || strings.HasPrefix(ctype, "/") || strings.HasSuffix(ctype, "/") {
		return "text/plain"
	}
	return ctype
}

// Lower cased charset parameter of the Content-Type field, empty when the field names none
func (m *Message) Charset() string {
	v, _ := m.Header.raw("Content-Type")
	_, params := ParseParams(v)
	return strings.ToLower(strings.TrimSpace(params["charset"]))
}

// Lower cased disposition the Content-Disposition field gives, such as attachment or inline, empty when the field is absent
func (m *Message) Disposition() string {
	v, ok := m.Header.raw("Content-Disposition")
	if !ok {
		return ""
	}
	disposition, _ := ParseParams(v)
	return disposition
}

// Name of the file the part carries as the sender wrote it, the Content-Disposition filename or else the Content-Type name, decoded from RFC 2231 and RFC 2047 encodings and trimmed, empty when it declares none; it may hold path separators, .. or control characters, so a caller that writes files takes only a cleaned base name from it
func (m *Message) Filename() string {
	for _, source := range [...][2]string{{"Content-Disposition", "filename"}, {"Content-Type", "name"}} {
		v, ok := m.Header.raw(source[0])
		if !ok {
			continue
		}
		_, params := ParseParams(v)
		if name := strings.TrimSpace(DecodeHeader(params[source[1]])); name != "" {
			return name
		}
	}
	return ""
}

// Boundary parameter of a multipart, trailing white space removed
func (m *Message) boundary() string {
	v, _ := m.Header.raw("Content-Type")
	_, params := ParseParams(v)
	return strings.TrimRightFunc(params["boundary"], unicode.IsSpace)
}

// Content of a leaf with its transfer encoding undone, base64, quoted-printable and uuencode read leniently and any other encoding as it is; nil for a container
func (m *Message) Body() []byte {
	if m.multipart {
		return nil
	}
	return m.transferDecoded(m.body)
}

// Lower cased Content-Transfer-Encoding of the part, empty when it names none
func (m *Message) transferEncoding() string {
	v, _ := m.Header.raw("Content-Transfer-Encoding")
	return strings.ToLower(strings.TrimSpace(v))
}

// Body bytes with the Content-Transfer-Encoding of the part undone
func (m *Message) transferDecoded(body []byte) []byte {
	switch m.transferEncoding() {
	case "base64":
		return decodeBase64(body)
	case "quoted-printable":
		return decodeQuotedPrintable(body)
	case "x-uuencode", "uuencode", "uue", "x-uue":
		if decoded, ok := decodeUU(body); ok {
			return decoded
		}
	}
	return body
}

// Content of a leaf decoded from its transfer encoding and its charset to UTF-8, CRLF line ends turned into LF; empty for a container
func (m *Message) Text() string {
	if m.multipart {
		return ""
	}
	return strings.ReplaceAll(decodeText(m.Body(), m.Charset()), "\r\n", "\n")
}
