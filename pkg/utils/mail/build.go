package mailutil

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/quotedprintable"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type partKind uint8

const (
	textPart partKind = iota
	binaryPart
	multiPart
)

// Message or MIME part being built for sending: header fields written in their order, then text, binary data or child parts
type Part struct {
	Header Header
	kind   partKind
	text   string
	data   []byte
	parts  []*Part
}

// Empty multipart/mixed container for Attach
func NewMultipart() *Part {
	return &Part{Header: Header{{Name: "Content-Type", Value: "multipart/mixed"}}, kind: multiPart}
}

// Plain text part in UTF-8 written quoted-printable, invalid UTF-8 replaced by U+FFFD
func NewText(body string) *Part {
	return &Part{Header: Header{{Name: "Content-Type", Value: "text/plain; charset=utf-8"},
		{Name: "Content-Transfer-Encoding", Value: "quoted-printable"}}, kind: textPart, text: validText(body)}
}

// Attachment part of the data written in base64, its content type application/octet-stream when the one given is empty or malformed, named filename unless that is empty, non ASCII names encoded per RFC 2231 and long ones split into RFC 2231 continuations
func NewAttachment(filename, contentType string, data []byte) *Part {
	filename = validText(filename)
	media, params := ParseParams(contentType)
	if filename != "" {
		params["name"] = filename
	}
	ctype := formatMediaType(media, params)
	if ctype == "" {
		named := map[string]string{}
		if filename != "" {
			named["name"] = filename
		}
		ctype = formatMediaType("application/octet-stream", named)
	}
	disposition := "attachment"
	if filename != "" {
		disposition = formatMediaType("attachment", map[string]string{"filename": filename})
	}
	return &Part{Header: Header{{Name: "Content-Type", Value: ctype}, {Name: "Content-Transfer-Encoding", Value: "base64"},
		{Name: "Content-Disposition", Value: disposition}}, kind: binaryPart, data: data}
}

// Gives the header field the value, replacing any of the same name
func (p *Part) Set(name, value string) { p.Header.Set(name, value) }

// Adds a child part; a part that is not multipart becomes a multipart/mixed container whose first child takes over its content and Content fields
func (p *Part) Attach(sub *Part) {
	if p.kind != multiPart {
		first := &Part{kind: p.kind, text: p.text, data: p.data}
		var kept Header
		for _, f := range p.Header {
			if len(f.Name) > 8 && strings.EqualFold(f.Name[:8], "content-") {
				first.Header = append(first.Header, f)
			} else {
				kept = append(kept, f)
			}
		}
		p.Header = append(kept, Field{Name: "Content-Type", Value: "multipart/mixed"})
		p.kind, p.text, p.data, p.parts = multiPart, "", nil, []*Part{first}
	}
	p.parts = append(p.parts, sub)
}

// Writes the part as a whole message with CRLF line ends, a MIME-Version field first unless the header has one
func (p *Part) WriteTo(w io.Writer) (int64, error) {
	data, err := p.bytes(nil)
	if err != nil {
		return 0, err
	}
	n, err := w.Write(data)
	return int64(n), err
}

// Wire form of the part as a whole message, the fields skip reports left out of its own header
func (p *Part) bytes(skip func(name string) bool) ([]byte, error) {
	var b bytes.Buffer
	if _, ok := p.Header.raw("MIME-Version"); !ok {
		b.WriteString("MIME-Version: 1.0\r\n")
	}
	if err := p.write(&b, skip); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Writes the header and the content of the part, a multipart given a boundary unless its Content-Type declares one
func (p *Part) write(b *bytes.Buffer, skip func(name string) bool) error {
	boundary, declared := "", false
	if p.kind == multiPart {
		boundary, declared = p.boundary()
	}
	typed := false
	for _, f := range p.Header {
		if skip != nil && skip(f.Name) {
			continue
		}
		value := f.Value
		if p.kind == multiPart && strings.EqualFold(f.Name, "Content-Type") {
			if typed {
				continue
			}
			if !declared {
				value += `; boundary="` + boundary + `"`
			}
			typed = true
		}
		if err := writeField(b, f.Name, value); err != nil {
			return err
		}
	}
	if p.kind == multiPart && !typed {
		writeFolded(b, "Content-Type", `multipart/mixed; boundary="`+boundary+`"`)
	}
	b.WriteString("\r\n")
	switch p.kind {
	case textPart:
		qp := quotedprintable.NewWriter(b)
		_, _ = io.WriteString(qp, p.text)
		_ = qp.Close()
	case binaryPart:
		writeBase64(b, p.data)
	case multiPart:
		if len(p.parts) == 0 {
			b.WriteString("--")
			b.WriteString(boundary)
			b.WriteString("\r\n\r\n")
		}
		for _, sub := range p.parts {
			b.WriteString("--")
			b.WriteString(boundary)
			b.WriteString("\r\n")
			if err := sub.write(b, nil); err != nil {
				return err
			}
			b.WriteString("\r\n")
		}
		b.WriteString("--")
		b.WriteString(boundary)
		b.WriteString("--\r\n")
	}
	return nil
}

// Boundary the Content-Type declares and true, else a fresh random one that base64 and quoted-printable content can never contain
func (p *Part) boundary() (string, bool) {
	if v, ok := p.Header.raw("Content-Type"); ok {
		if _, params := ParseParams(v); params["boundary"] != "" {
			return params["boundary"], true
		}
	}
	return "=_" + rand.Text(), false
}

// Base64 of data in lines of 76 characters separated by CRLF
func writeBase64(b *bytes.Buffer, data []byte) {
	const chunk = 57
	enc := base64.StdEncoding
	b.Grow(enc.EncodedLen(len(data)) + len(data)/chunk*2)
	var line [76]byte
	for i := 0; i < len(data); i += chunk {
		if i > 0 {
			b.WriteString("\r\n")
		}
		piece := data[i:min(i+chunk, len(data))]
		n := enc.EncodedLen(len(piece))
		enc.Encode(line[:n], piece)
		b.Write(line[:n])
	}
}

// Longest header line RFC 5322 allows, its CRLF not counted
const maxLineLen = 998

// Writes one header field: the value put on one line, encoded when it carries characters a header cannot, then folded; a word too long for any line becomes encoded words in an unstructured field and an error in any other
func writeField(b *bytes.Buffer, name, value string) error {
	if !validFieldName(name) {
		return fmt.Errorf("mail: %w: header field name %q", errArgument, name)
	}
	value = oneLine(value)
	if needsEncoding(value) {
		formatted, ok := "", false
		if isAddressField(name) {
			formatted, ok = formatAddressList(value)
		}
		if !ok {
			formatted = encodeWords(value)
		}
		value = formatted
	}
	if !foldable(name, value) {
		if !isUnstructured(name) {
			return fmt.Errorf("mail: %w: %s field holds a word longer than a header line", errArgument, name)
		}
		value = splitLongWords(value)
	}
	writeFolded(b, name, value)
	return nil
}

// Whether writeFolded can keep every line of the field within maxLineLen, which a word without spaces longer than a line defeats
func foldable(name, value string) bool {
	room := maxLineLen - len(name) - 2
	for word := range strings.SplitSeq(value, " ") {
		if len(word) > room {
			return false
		}
		room = maxLineLen - 1
	}
	return true
}

// Whether a field holds free text, in which encoded words may stand for any word
func isUnstructured(name string) bool {
	switch strings.ToLower(name) {
	case "subject", "comments", "content-description", "thread-topic":
		return true
	}
	return false
}

// Value with every word too long for a header line written as UTF-8 base64 encoded words, which a reader joins back into the word
func splitLongWords(v string) string {
	const chunk = 45
	var b strings.Builder
	b.Grow(len(v) + len(v)/2)
	for i, word := range strings.Split(v, " ") {
		if i > 0 {
			b.WriteByte(' ')
		}
		if len(word) <= maxLineLen/2 {
			b.WriteString(word)
			continue
		}
		for j := 0; len(word) > 0; j++ {
			n := min(chunk, len(word))
			for n < len(word) && n > 1 && !utf8.RuneStart(word[n]) {
				n--
			}
			if j > 0 {
				b.WriteByte(' ')
			}
			b.WriteString("=?utf-8?b?")
			b.WriteString(base64.StdEncoding.EncodeToString([]byte(word[:n])))
			b.WriteString("?=")
			word = word[n:]
		}
	}
	return b.String()
}

// Value of a structured field such as Content-Type or Content-Disposition with its parameters in name order, one too long for a folded line split into RFC 2231 continuations; empty when the value or a parameter name is no token
func formatMediaType(value string, params map[string]string) string {
	head := mime.FormatMediaType(value, nil)
	if head == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(head)
	for _, name := range slices.Sorted(maps.Keys(params)) {
		one := mime.FormatMediaType("a", map[string]string{name: params[name]})
		if one == "" {
			return ""
		}
		if len(one) <= maxParamLen+3 {
			b.WriteString(one[1:])
			continue
		}
		writeContinuations(&b, strings.ToLower(name), params[name])
	}
	return b.String()
}

// Longest parameter written whole, so that it fits a folded line with the space before it and the semicolon after it
const maxParamLen = 76

// Writes a parameter as RFC 2231 continuations name*0*, name*1* and on, the first naming the utf-8 charset, each a percent encoded piece short enough for a folded line and cut only between characters
func writeContinuations(b *strings.Builder, name, value string) {
	const upperhex = "0123456789ABCDEF"
	var piece []byte
	n := 0
	start := func() {
		piece = append(piece[:0], name...)
		piece = append(piece, '*')
		piece = strconv.AppendInt(piece, int64(n), 10)
		piece = append(piece, "*="...)
		if n == 0 {
			piece = append(piece, "utf-8''"...)
		}
	}
	flush := func() {
		b.WriteString("; ")
		b.Write(piece)
		n++
	}
	start()
	head := len(piece)
	for len(value) > 0 {
		_, size := utf8.DecodeRuneInString(value)
		var enc [12]byte
		e := enc[:0]
		for i := range size {
			if c := value[i]; isAttributeChar(c) {
				e = append(e, c)
			} else {
				e = append(e, '%', upperhex[c>>4], upperhex[c&0x0f])
			}
		}
		if len(piece)+len(e) > maxParamLen && len(piece) > head {
			flush()
			start()
			head = len(piece)
		}
		piece = append(piece, e...)
		value = value[size:]
	}
	flush()
}

// Whether a byte may stand for itself in an RFC 2231 extended value: printable ASCII other than space, *, ', % and the MIME tspecials
func isAttributeChar(c byte) bool {
	return c > ' ' && c < 0x7f && strings.IndexByte(`*'%()<>@,;:\"/[]?=`, c) < 0
}

// Writes name: value and CRLF, breaking the line before a space wherever it would pass 78 characters
func writeFolded(b *bytes.Buffer, name, value string) {
	b.WriteString(name)
	b.WriteByte(':')
	width := len(name) + 1
	first := true
	for word := range strings.SplitSeq(value, " ") {
		if !first && word != "" && width+1+len(word) > 78 {
			b.WriteString("\r\n ")
			width = 1
		} else {
			b.WriteByte(' ')
			width++
		}
		b.WriteString(word)
		width += len(word)
		first = false
	}
	b.WriteString("\r\n")
}

// Whether a field name is printable ASCII without a colon
func validFieldName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c <= ' ' || c >= 0x7f || c == ':' {
			return false
		}
	}
	return true
}

// Value as one line of valid UTF-8, each line break with the white space after it turned into a space so no value can start a field of its own
func oneLine(v string) string {
	v = validText(v)
	if !strings.ContainsAny(v, "\r\n") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if c := v[i]; c != '\r' && c != '\n' {
			b.WriteByte(c)
			continue
		}
		for i+1 < len(v) && strings.IndexByte("\r\n \t", v[i+1]) >= 0 {
			i++
		}
		b.WriteByte(' ')
	}
	return b.String()
}

// Whether a value holds control characters or bytes outside ASCII
func needsEncoding(v string) bool {
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < ' ' && c != '\t' || c >= 0x7f {
			return true
		}
	}
	return false
}

// Value as UTF-8 encoded words, base64 when that comes out shorter than quoted-printable
func encodeWords(v string) string {
	special := 0
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < ' ' || c > '~' || c == '=' || c == '?' || c == '_' {
			special++
		}
	}
	if special*6 > len(v) {
		return mime.BEncoding.Encode("utf-8", v)
	}
	return mime.QEncoding.Encode("utf-8", v)
}

// Whether a field carries addresses
func isAddressField(name string) bool {
	switch strings.ToLower(name) {
	case "from", "sender", "reply-to", "to", "cc", "bcc", "resent-from", "resent-sender", "resent-to", "resent-cc", "resent-bcc",
		"disposition-notification-to":
		return true
	}
	return false
}
