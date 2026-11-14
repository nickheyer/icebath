package pyemail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// HeaderValue is a raw header value, bytes above ASCII kept as they arrived
type HeaderValue string

// Whether the value carries bytes above ASCII, read by compat32 as an unknown-8bit Header
func (h HeaderValue) Has8Bit() bool {
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			return true
		}
	}
	return false
}

// str of the value, each byte above ASCII shown as the replacement character
func (h HeaderValue) Str() string {
	if !h.Has8Bit() {
		return string(h)
	}
	var b strings.Builder
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			b.WriteRune('�')
		} else {
			b.WriteByte(h[i])
		}
	}
	return b.String()
}

// HeaderParseError is the error decode_header raises for an encoded word that does not decode
var ErrHeaderParse = errors.New("Base64 decoding error")

// DecodedPart is one run of a decoded header, text for the single undecoded form and bytes with a charset otherwise
type DecodedPart struct {
	Text    string
	Bytes   []byte
	IsBytes bool
	Charset string
}

var encodedWordRE = regexp.MustCompile(`=\?([^?]*?)\?([qQbB])\?(.*?)\?=`)

var qpHexRE = regexp.MustCompile(`=[a-fA-F0-9]{2}`)

// Bytes a2b_base64 yields in its lenient mode, characters outside the alphabet skipped
func a2bBase64(data []byte) ([]byte, error) {
	var out []byte
	var quad [4]byte
	quadPos, pads, dataChars := 0, 0, 0
	for _, ch := range data {
		if ch == '=' {
			if quadPos >= 2 {
				pads++
				if pads >= 4-quadPos {
					quadPos = 0
					break
				}
			}
			continue
		}
		val := strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", ch)
		if val < 0 {
			continue
		}
		pads = 0
		dataChars++
		quad[quadPos] = byte(val)
		quadPos++
		if quadPos == 2 {
			out = append(out, quad[0]<<2|quad[1]>>4)
		} else if quadPos == 3 {
			out = append(out, quad[1]<<4|quad[2]>>2)
		} else if quadPos == 4 {
			out = append(out, quad[2]<<6|quad[3])
			quadPos = 0
		}
	}
	if quadPos != 0 {
		if quadPos == 1 {
			return nil, errors.New("Invalid base64-encoded string: number of data characters (" + strconv.Itoa(dataChars) +
				") cannot be 1 more than a multiple of 4")
		}
		return nil, errors.New("Incorrect padding")
	}
	return out, nil
}

// Body bytes of a base64 payload, padding repaired and stray characters skipped, the input itself when nothing decodes
func decodeB(encoded []byte) []byte {
	padded := encoded
	if padErr := len(encoded) % 4; padErr != 0 {
		padded = append(append([]byte{}, encoded...), "==="[:4-padErr]...)
	}
	if out, err := base64.StdEncoding.Strict().DecodeString(string(padded)); err == nil {
		return out
	}
	if out, err := a2bBase64(encoded); err == nil {
		return out
	}
	if out, err := a2bBase64(append(append([]byte{}, encoded...), '=', '=')); err == nil {
		return out
	}
	return encoded
}

// Lines of a body joined without their terminators
func joinLines(payload []byte) []byte {
	var out []byte
	for _, line := range splitKeepEnds(string(payload)) {
		out = append(out, strings.TrimRight(line, "\r\n")...)
	}
	return out
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// Quoted printable data decoded like a2b_qp, an underscore reading as a space in header mode
func decodeQP(data []byte, header bool) []byte {
	out := make([]byte, 0, len(data))
	for in := 0; in < len(data); {
		switch {
		case data[in] == '=':
			in++
			if in >= len(data) {
				return out
			}
			switch {
			case data[in] == '\n' || data[in] == '\r':
				if data[in] != '\n' {
					for in < len(data) && data[in] != '\n' {
						in++
					}
				}
				if in < len(data) {
					in++
				}
			case data[in] == '=':
				out = append(out, '=')
				in++
			case in+1 < len(data) && isHexDigit(data[in]) && isHexDigit(data[in+1]):
				v, _ := strconv.ParseUint(string(data[in:in+2]), 16, 8)
				out = append(out, byte(v))
				in += 2
			default:
				out = append(out, '=')
			}
		case header && data[in] == '_':
			out = append(out, ' ')
			in++
		default:
			out = append(out, data[in])
			in++
		}
	}
	return out
}

// One uuencoded line decoded like a2b_uu, false for an illegal character or trailing garbage
func a2bUU(line []byte) ([]byte, bool) {
	if len(line) == 0 {
		return make([]byte, 32), true
	}
	binLen := int(line[0]-' ') & 0x3f
	rest := line[1:]
	out := make([]byte, 0, binLen)
	var leftchar uint32
	leftbits := 0
	i := 0
	for ; len(out) < binLen; i++ {
		var ch byte
		if i < len(rest) && rest[i] != '\n' && rest[i] != '\r' {
			if rest[i] < ' ' || rest[i] > ' '+64 {
				return nil, false
			}
			ch = (rest[i] - ' ') & 0x3f
		}
		leftchar = leftchar<<6 | uint32(ch)
		leftbits += 6
		if leftbits >= 8 {
			leftbits -= 8
			out = append(out, byte(leftchar>>uint(leftbits)))
			leftchar &= 1<<uint(leftbits) - 1
		}
	}
	for ; i < len(rest); i++ {
		if c := rest[i]; c != ' ' && c != ' '+64 && c != '\n' && c != '\r' {
			return nil, false
		}
	}
	return out, true
}

// Uuencoded body decoded between its begin and end lines, false when the framing is broken
func decodeUU(encoded []byte) ([]byte, bool) {
	var lines [][]byte
	for _, line := range splitKeepEnds(string(encoded)) {
		lines = append(lines, []byte(strings.TrimRight(line, "\r\n")))
	}
	start := -1
	for i, line := range lines {
		if rest, ok := bytes.CutPrefix(line, []byte("begin ")); ok {
			mode, _, _ := bytes.Cut(rest, []byte(" "))
			if _, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(string(mode)), "_", ""), 8, 64); err == nil {
				start = i + 1
				break
			}
		}
	}
	if start < 0 {
		return nil, false
	}
	var decoded []byte
	for _, line := range lines[start:] {
		if len(line) == 0 {
			return nil, false
		}
		if string(bytes.Trim(line, " \t\r\n\f")) == "end" {
			break
		}
		part, ok := a2bUU(line)
		if !ok {
			nbytes := ((int(line[0]-32)&63)*4 + 5) / 3
			if nbytes > len(line) {
				nbytes = len(line)
			}
			if part, ok = a2bUU(line[:nbytes]); !ok {
				return nil, false
			}
		}
		decoded = append(decoded, part...)
	}
	return decoded, true
}

// Bytes of a str under the raw-unicode-escape codec
func rawUnicodeEscape(s string) []byte {
	var out []byte
	for _, r := range s {
		switch {
		case r < 0x100:
			out = append(out, byte(r))
		case r < 0x10000:
			out = append(out, []byte(`\u`+leftPadHex(r, 4))...)
		default:
			out = append(out, []byte(`\U`+leftPadHex(r, 8))...)
		}
	}
	return out
}

// Decodes a header value without converting charsets, one text part when it holds no encoded word
func DecodeHeader(header HeaderValue) ([]DecodedPart, error) {
	if header.Has8Bit() {
		return []DecodedPart{{Bytes: []byte(header), IsBytes: true, Charset: "unknown-8bit"}}, nil
	}
	text := string(header)
	if !encodedWordRE.MatchString(text) {
		return []DecodedPart{{Text: text}}, nil
	}
	type word struct {
		text     string
		encoding string
		charset  string
	}
	var words []word
	for _, line := range pystr.Splitlines(text, false) {
		first := true
		pos := 0
		for _, m := range encodedWordRE.FindAllStringSubmatchIndex(line, -1) {
			unencoded := line[pos:m[0]]
			if first {
				unencoded = pystr.Lstrip(unencoded)
				first = false
			}
			if unencoded != "" {
				words = append(words, word{text: unencoded})
			}
			words = append(words, word{text: line[m[6]:m[7]], encoding: pystr.Lower(line[m[4]:m[5]]), charset: pystr.Lower(line[m[2]:m[3]])})
			pos = m[1]
		}
		tail := line[pos:]
		if first {
			tail = pystr.Lstrip(tail)
		}
		if tail != "" {
			words = append(words, word{text: tail})
		}
	}
	var droplist []int
	for n, w := range words {
		if n > 1 && w.encoding != "" && words[n-2].encoding != "" && words[n-1].text != "" && pystr.Strip(words[n-1].text) == "" {
			droplist = append(droplist, n-1)
		}
	}
	for i := len(droplist) - 1; i >= 0; i-- {
		d := droplist[i]
		words = append(words[:d:d], words[d+1:]...)
	}
	type decoded struct {
		data    []byte
		charset string
		encoded bool
	}
	var decodedWords []decoded
	for _, w := range words {
		switch w.encoding {
		case "":
			decodedWords = append(decodedWords, decoded{data: rawUnicodeEscape(w.text)})
		case "q":
			unquoted := qpHexRE.ReplaceAllStringFunc(strings.ReplaceAll(w.text, "_", " "), func(m string) string {
				v, _ := strconv.ParseUint(m[1:], 16, 8)
				return string(rune(v))
			})
			decodedWords = append(decodedWords, decoded{data: rawUnicodeEscape(unquoted), charset: w.charset, encoded: true})
		case "b":
			encoded := w.text
			if padErr := len([]rune(encoded)) % 4; padErr != 0 {
				encoded += "==="[:4-padErr]
			}
			var data []byte
			if encoded != "" {
				var err error
				if data, err = a2bBase64(rawUnicodeEscape(encoded)); err != nil {
					return nil, ErrHeaderParse
				}
			}
			decodedWords = append(decodedWords, decoded{data: data, charset: w.charset, encoded: true})
		}
	}
	var collapsed []DecodedPart
	var last *decoded
	for i := range decodedWords {
		w := decodedWords[i]
		switch {
		case last == nil:
			last = &w
		case w.charset != last.charset || w.encoded != last.encoded:
			collapsed = append(collapsed, DecodedPart{Bytes: last.data, IsBytes: true, Charset: last.charset})
			last = &w
		case !last.encoded:
			last.data = append(append(last.data, ' '), w.data...)
		default:
			last.data = append(last.data, w.data...)
		}
	}
	collapsed = append(collapsed, DecodedPart{Bytes: last.data, IsBytes: true, Charset: last.charset})
	return collapsed, nil
}
