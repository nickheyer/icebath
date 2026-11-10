package pyfs

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// Byte chunk TextIOWrapper reads at least once per refill, io.TextIOWrapper._CHUNK_SIZE
const textChunkSize = 8192

// UnicodeDecodeError a strict utf-8 text read raises for bytes that do not decode
type UnicodeDecodeError struct {
	Path   string
	Byte   byte
	Start  int
	End    int
	Reason string
}

func (e *UnicodeDecodeError) Error() string {
	if e.End-e.Start > 1 {
		return fmt.Sprintf("'utf-8' codec can't decode bytes in position %d-%d: %s", e.Start, e.End-1, e.Reason)
	}
	return fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: %s", e.Byte, e.Start, e.Reason)
}

// Incremental strict utf-8 or utf-8-sig decoder with universal newline translation like TextIOWrapper
type textDecoder struct {
	path      string
	sig       bool
	sigDone   bool
	pending   []byte
	pendingCR bool
}

// Leading bytes of buf that stop short of a complete rune, the incomplete tail CPython buffers across calls
func incompleteTail(buf []byte) int {
	for k := 1; k <= 3 && k <= len(buf); k++ {
		tail := buf[len(buf)-k:]
		if utf8.RuneStart(tail[0]) && !utf8.FullRune(tail) {
			return k
		}
	}
	return 0
}

// Second byte range a utf-8 lead byte admits, the narrower ranges CPython enforces for E0 ED F0 and F4
func secondByteRange(lead byte) (byte, byte) {
	switch lead {
	case 0xE0:
		return 0xA0, 0xBF
	case 0xED:
		return 0x80, 0x9F
	case 0xF0:
		return 0x90, 0xBF
	case 0xF4:
		return 0x80, 0x8F
	}
	return 0x80, 0xBF
}

// Reason and byte span CPython reports for the undecodable sequence at the head of buf
func decodeFailure(buf []byte) (string, int) {
	lead := buf[0]
	if lead < 0xC2 || lead > 0xF4 {
		return "invalid start byte", 1
	}
	need := 2
	if lead >= 0xF0 {
		need = 4
	} else if lead >= 0xE0 {
		need = 3
	}
	valid := 0
	for k := 1; k < need && k < len(buf); k++ {
		lo, hi := byte(0x80), byte(0xBF)
		if k == 1 {
			lo, hi = secondByteRange(lead)
		}
		if buf[k] < lo || buf[k] > hi {
			return "invalid continuation byte", 1 + valid
		}
		valid++
	}
	return "unexpected end of data", len(buf)
}

// Decodes one chunk, holding back an incomplete sequence and a trailing carriage return until more input or the end
func (d *textDecoder) decode(input []byte, final bool) (string, error) {
	buf := append(d.pending, input...)
	d.pending = nil
	if d.sig && !d.sigDone {
		bom := []byte("\xef\xbb\xbf")
		if len(buf) < 3 && strings.HasPrefix(string(bom), string(buf)) {
			d.pending = buf
			return "", nil
		}
		d.sigDone = true
		if strings.HasPrefix(string(buf), string(bom)) {
			buf = buf[3:]
		}
	}
	end := len(buf)
	if !final {
		end -= incompleteTail(buf)
	}
	var out strings.Builder
	for i := 0; i < end; {
		r, size := utf8.DecodeRune(buf[i:end])
		if r == utf8.RuneError && size <= 1 {
			reason, span := decodeFailure(buf[i:end])
			return "", &UnicodeDecodeError{Path: d.path, Byte: buf[i], Start: i, End: i + span, Reason: reason}
		}
		out.WriteRune(r)
		i += size
	}
	if end < len(buf) {
		d.pending = append([]byte(nil), buf[end:]...)
	}
	text := out.String()
	if d.pendingCR && (text != "" || final) {
		text = "\r" + text
		d.pendingCR = false
	}
	if strings.HasSuffix(text, "\r") && !final {
		text = text[:len(text)-1]
		d.pendingCR = true
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

// open(path, encoding utf-8 or utf-8-sig).read(n) with TextIOWrapper chunking, universal newlines and strict errors
func ReadTextChars(path string, n int, sig bool) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	dec := &textDecoder{path: path, sig: sig}
	var result []rune
	b2cratio := 0.0
	for len(result) < n {
		remaining := n - len(result)
		hint := int(max(b2cratio, 1.0) * float64(remaining))
		chunk := make([]byte, max(textChunkSize, hint))
		got, rerr := f.Read(chunk)
		if rerr != nil && rerr != io.EOF {
			return "", rerr
		}
		eof := got == 0
		decoded, derr := dec.decode(chunk[:got], eof)
		if derr != nil {
			return "", derr
		}
		runes := []rune(decoded)
		if len(runes) > 0 {
			b2cratio = float64(got) / float64(len(runes))
		} else {
			b2cratio = 0
		}
		result = append(result, runes[:min(remaining, len(runes))]...)
		if eof {
			break
		}
	}
	return string(result), nil
}
