package pyemail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Part is a MIME entity under construction, a leaf with an encoded text payload or a multipart container
type Part struct {
	headers   [][2]string
	payload   string
	parts     []*Part
	multipart bool
}

// Lines of base64 at most 76 characters wide, each ended by a line feed
func base64Lines(data []byte) string {
	var b strings.Builder
	for i := 0; i < len(data); i += 57 {
		end := min(i+57, len(data))
		b.WriteString(base64.StdEncoding.EncodeToString(data[i:end]))
		b.WriteByte('\n')
	}
	return b.String()
}

// Empty multipart/mixed container with its MIME version header
func NewMultipart() *Part {
	return &Part{headers: [][2]string{{"Content-Type", "multipart/mixed"}, {"MIME-Version", "1.0"}}, multipart: true}
}

// Plain text part in utf-8 carried as base64
func NewTextUTF8(body string) *Part {
	return &Part{headers: [][2]string{{"Content-Type", `text/plain; charset="utf-8"`}, {"MIME-Version", "1.0"},
		{"Content-Transfer-Encoding", "base64"}}, payload: base64Lines([]byte(body))}
}

// Binary part of the given type carried as base64, its last line unterminated unless the data ends in a line feed
func NewBase64(maintype, subtype string, data []byte) *Part {
	encoded := base64Lines(data)
	if !bytes.HasSuffix(data, []byte("\n")) {
		encoded = strings.TrimSuffix(encoded, "\n")
	}
	return &Part{headers: [][2]string{{"Content-Type", maintype + "/" + subtype}, {"MIME-Version", "1.0"},
		{"Content-Transfer-Encoding", "base64"}}, payload: encoded}
}

// Appends a header, a name already present kept beside it
func (p *Part) Set(name, value string) { p.headers = append(p.headers, [2]string{name, value}) }

// Adds a sub part to a multipart container
func (p *Part) Attach(sub *Part) { p.parts = append(p.parts, sub) }

// First value of a header matched without case
func (p *Part) Get(name string) (string, bool) {
	for _, h := range p.headers {
		if strings.EqualFold(h[0], name) {
			return h[1], true
		}
	}
	return "", false
}

// One pending header line as white space and text pairs with the width the header name already takes
type accumulator struct {
	parts       [][2]string
	initialSize int
}

func (a *accumulator) push(fws, text string) { a.parts = append(a.parts, [2]string{fws, text}) }

func (a *accumulator) pop() (string, string) {
	if len(a.parts) == 0 {
		return "", ""
	}
	last := a.parts[len(a.parts)-1]
	a.parts = a.parts[:len(a.parts)-1]
	return last[0], last[1]
}

func (a *accumulator) length() int {
	n := a.initialSize
	for _, part := range a.parts {
		n += len([]rune(part[0])) + len([]rune(part[1]))
	}
	return n
}

func (a *accumulator) String() string {
	var b strings.Builder
	for _, part := range a.parts {
		b.WriteString(part[0] + part[1])
	}
	return b.String()
}

func (a *accumulator) reset(start [][2]string) { a.parts, a.initialSize = start, 0 }

func (a *accumulator) isOnlyWS() bool {
	text := a.String()
	return a.initialSize == 0 && (len(a.parts) == 0 || (text != "" && pystr.Strip(text) == ""))
}

// Header value folder with the line breaking rules of the email.header value formatter
type valueFormatter struct {
	maxlen  int
	lines   []string
	current accumulator
}

func (f *valueFormatter) newline() {
	fws, text := f.current.pop()
	if fws != " " || text != "" {
		f.current.push(fws, text)
	}
	if f.current.length() > 0 {
		if f.current.isOnlyWS() && len(f.lines) > 0 {
			f.lines[len(f.lines)-1] += f.current.String()
		} else {
			f.lines = append(f.lines, f.current.String())
		}
	}
	f.current.reset(nil)
}

var fwsSplitRE = regexp.MustCompile(`[ \t]+`)

// Splits an ASCII value at its white space runs and appends each piece, breaking lines as they fill
func (f *valueFormatter) asciiSplit(fws, text string) {
	whole := fws + text
	var parts []string
	pos := 0
	for _, m := range fwsSplitRE.FindAllStringIndex(whole, -1) {
		parts = append(parts, whole[pos:m[0]], whole[m[0]:m[1]])
		pos = m[1]
	}
	parts = append(parts, whole[pos:])
	if parts[0] != "" {
		parts = append([]string{""}, parts...)
	} else {
		parts = parts[1:]
	}
	for i := 0; i+1 < len(parts); i += 2 {
		f.appendChunk(parts[i], parts[i+1])
	}
}

// Appends a piece to the current line and moves the tail to a new line at the best split point once it is too long
func (f *valueFormatter) appendChunk(fws, text string) {
	f.current.push(fws, text)
	if f.current.length() <= f.maxlen {
		return
	}
	split := -1
search:
	for _, ch := range ";, \t" {
		for i := len(f.current.parts) - 1; i > 0; i-- {
			if ch == ' ' || ch == '\t' {
				if lead := f.current.parts[i][0]; lead != "" && rune(lead[0]) == ch {
					split = i
					break search
				}
			}
			if prev := f.current.parts[i-1][1]; prev != "" && rune(prev[len(prev)-1]) == ch {
				split = i
				break search
			}
		}
	}
	if split < 0 {
		lead, part := f.current.pop()
		if f.current.initialSize > 0 {
			f.newline()
			if lead == "" {
				lead = " "
			}
		}
		f.current.push(lead, part)
		return
	}
	remainder := append([][2]string(nil), f.current.parts[split:]...)
	f.current.parts = f.current.parts[:split]
	f.lines = append(f.lines, f.current.String())
	f.current.reset(remainder)
}

// Characters a quoted printable encoded word carries unescaped
const qpHeaderSafe = "-!*+/ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func qpHeaderLength(data []byte) int {
	n := 0
	for _, c := range data {
		if c == ' ' || strings.IndexByte(qpHeaderSafe, c) >= 0 {
			n++
		} else {
			n += 3
		}
	}
	return n
}

func b64HeaderLength(data []byte) int {
	n := len(data) / 3 * 4
	if len(data)%3 != 0 {
		n += 4
	}
	return n
}

// One utf-8 encoded word, base64 when that is shorter than quoted printable for the whole value
func encodeWord(data []byte, useB64 bool) string {
	if useB64 {
		return "=?utf-8?b?" + base64.StdEncoding.EncodeToString(data) + "?="
	}
	var b strings.Builder
	for _, c := range data {
		switch {
		case c == ' ':
			b.WriteByte('_')
		case strings.IndexByte(qpHeaderSafe, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "=%02X", c)
		}
	}
	return "=?utf-8?q?" + b.String() + "?="
}

// Encoded words of a non ASCII value, each sized to the room its line has, the first empty when nothing fits there
func headerEncodeLines(text string, firstMax, nextMax int) (lines []string, firstFits bool) {
	useB64 := b64HeaderLength([]byte(text)) < qpHeaderLength([]byte(text))
	length := func(s string) int {
		if useB64 {
			return b64HeaderLength([]byte(s))
		}
		return qpHeaderLength([]byte(s))
	}
	const extra = len("utf-8") + 7
	firstFits = true
	maxlen := firstMax - extra
	var current []rune
	for _, character := range text {
		current = append(current, character)
		if length(string(current)) > maxlen {
			current = current[:len(current)-1]
			if len(lines) == 0 && len(current) == 0 && firstFits {
				firstFits = false
			} else {
				lines = append(lines, encodeWord([]byte(string(current)), useB64))
			}
			current = []rune{character}
			maxlen = nextMax - extra
		}
	}
	lines = append(lines, encodeWord([]byte(string(current)), useB64))
	return lines, firstFits
}

var embeddedHeaderRE = regexp.MustCompile(`\n[^ \t]+:`)

// ErrEmbeddedHeader is the HeaderParseError of a value that would inject a header line
var ErrEmbeddedHeader = errors.New("header value appears to contain an embedded header")

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// Folded header line without its terminator, an ASCII value split at white space and any other as utf-8 encoded words
func foldHeader(name, value, linesep string) (string, error) {
	f := &valueFormatter{maxlen: 78, current: accumulator{initialSize: len(name) + 2}}
	ascii := isASCII(value)
	feed := func(fws, text string) {
		if ascii {
			f.asciiSplit(fws, text)
			return
		}
		encoded, firstFits := headerEncodeLines(text, f.maxlen-f.current.length(), f.maxlen-1)
		if firstFits {
			f.appendChunk(fws, encoded[0])
			encoded = encoded[1:]
		}
		if len(encoded) == 0 {
			return
		}
		last := encoded[len(encoded)-1]
		f.newline()
		f.current.push(" ", last)
		for _, line := range encoded[:len(encoded)-1] {
			f.lines = append(f.lines, " "+line)
		}
	}
	lines := pystr.Splitlines(value, false)
	if len(lines) > 0 {
		feed("", lines[0])
	} else {
		feed("", "")
	}
	for _, line := range lines[min(1, len(lines)):] {
		f.newline()
		stripped := pystr.Lstrip(line)
		if ascii {
			feed(line[:len(line)-len(stripped)], stripped)
		} else {
			feed(" ", " "+stripped)
		}
	}
	if len(lines) > 1 {
		f.newline()
	}
	f.current.push(" ", "")
	f.newline()
	folded := strings.Join(f.lines, linesep)
	if embeddedHeaderRE.MatchString(folded) {
		return "", fmt.Errorf("%w: %s", ErrEmbeddedHeader, folded)
	}
	return name + ": " + folded, nil
}

// Random multipart boundary that no line of the body text equals
func makeBoundary(text string) string {
	boundary := strings.Repeat("=", 15) + fmt.Sprintf("%019d", rand.Int63()) + "=="
	b := boundary
	for counter := 0; ; counter++ {
		taken := false
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line == "--"+b || line == "--"+b+"--" {
				taken = true
				break
			}
		}
		if !taken {
			return b
		}
		b = fmt.Sprintf("%s.%d", boundary, counter)
	}
}

// Text with every line break written as the given separator
func writeLines(b *strings.Builder, text, linesep string) {
	if text == "" {
		return
	}
	lines := strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text), "\n")
	for _, line := range lines[:len(lines)-1] {
		b.WriteString(line + linesep)
	}
	b.WriteString(lines[len(lines)-1])
}

// Wire form of the entity with the given line separator, a multipart getting a fresh boundary in its Content-Type
func (p *Part) flatten(linesep string) (string, error) {
	var body strings.Builder
	headers := append([][2]string(nil), p.headers...)
	if p.multipart {
		texts := make([]string, len(p.parts))
		for i, sub := range p.parts {
			text, err := sub.flatten(linesep)
			if err != nil {
				return "", err
			}
			texts[i] = text
		}
		boundary := makeBoundary(strings.Join(texts, linesep))
		for i, h := range headers {
			if strings.EqualFold(h[0], "Content-Type") {
				headers[i][1] = h[1] + `; boundary="` + boundary + `"`
				break
			}
		}
		body.WriteString("--" + boundary + linesep)
		for i, text := range texts {
			if i > 0 {
				body.WriteString(linesep + "--" + boundary + linesep)
			}
			body.WriteString(text)
		}
		body.WriteString(linesep + "--" + boundary + "--" + linesep)
	} else {
		writeLines(&body, p.payload, linesep)
	}
	var out strings.Builder
	for _, h := range headers {
		folded, err := foldHeader(h[0], h[1], linesep)
		if err != nil {
			return "", err
		}
		out.WriteString(folded + linesep)
	}
	out.WriteString(linesep)
	out.WriteString(body.String())
	return out.String(), nil
}

// Bytes of the message as the SMTP generator writes it, lines ended by CRLF
func (p *Part) AsBytes() ([]byte, error) {
	text, err := p.flatten("\r\n")
	if err != nil {
		return nil, err
	}
	return []byte(text), nil
}

// RFC 2822 date of the current local time with its numeric zone
func FormatDateLocal() string {
	return time.Now().Format("Mon, 02 Jan 2006 15:04:05 -0700")
}

// Every value of a header matched without case in order
func (p *Part) GetAll(name string) []string {
	var out []string
	for _, h := range p.headers {
		if strings.EqualFold(h[0], name) {
			out = append(out, h[1])
		}
	}
	return out
}

// Shallow copy of the entity without the named headers
func (p *Part) Without(names ...string) *Part {
	clone := *p
	clone.headers = nil
	for _, h := range p.headers {
		dropped := false
		for _, name := range names {
			dropped = dropped || strings.EqualFold(h[0], name)
		}
		if !dropped {
			clone.headers = append(clone.headers, h)
		}
	}
	return &clone
}
