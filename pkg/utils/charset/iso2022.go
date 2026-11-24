package charsetutil

import (
	"bytes"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/width"
)

const (
	esc = 0x1b
	so  = 0x0e
	si  = 0x0f
)

// One 94 by 94 set of ISO 2022 double-byte characters, the character of each row and cell and the row and cell of each character
type set94 struct {
	dec [94 * 94]rune
	enc map[rune]uint16
}

// Set read from the EUC form of an x/text encoding, row and cell bytes with the high bit set behind an optional prefix byte, private use characters left out
func readSet94(enc encoding.Encoding, prefix byte) *set94 {
	s := &set94{enc: make(map[rune]uint16, 8192)}
	dec := enc.NewDecoder()
	var src [3]byte
	var dst [8]byte
	for i := range len(s.dec) {
		seq := src[:0]
		if prefix != 0 {
			seq = append(seq, prefix)
		}
		seq = append(seq, 0xa1+byte(i/94), 0xa1+byte(i%94))
		dec.Reset()
		n, _, err := dec.Transform(dst[:], seq, true)
		r, size := utf8.DecodeRune(dst[:n])
		if err != nil || size != n || r == utf8.RuneError || r >= 0xe000 && r <= 0xf8ff {
			continue
		}
		s.dec[i] = r
		if _, taken := s.enc[r]; !taken {
			s.enc[r] = uint16(i)
		}
	}
	return s
}

// Character of a row and cell byte pair, false outside the set
func (s *set94) char(c1, c2 byte) (rune, bool) {
	if c1 < 0x21 || c1 > 0x7e || c2 < 0x21 || c2 > 0x7e {
		return 0, false
	}
	r := s.dec[int(c1-0x21)*94+int(c2-0x21)]
	return r, r != 0
}

// Row and cell byte pair of a character, false when the set lacks it
func (s *set94) code(r rune) (byte, byte, bool) {
	i, ok := s.enc[r]
	return 0x21 + byte(i/94), 0x21 + byte(i%94), ok
}

var (
	jisX0208 = sync.OnceValue(func() *set94 { return readSet94(japanese.EUCJP, 0) })
	jisX0212 = sync.OnceValue(func() *set94 { return readSet94(japanese.EUCJP, 0x8f) })
	ksX1001  = sync.OnceValue(func() *set94 { return readSet94(korean.EUCKR, 0) })
	gb2312   = sync.OnceValue(func() *set94 { return readSet94(simplifiedchinese.GBK, 0) })
)

// Double-byte character of a set at the start of s, its length, or why it does not decode
func doubleByte(set *set94, s []byte) (rune, int, string) {
	if len(s) < 2 {
		return 0, 1, "incomplete double-byte character"
	}
	if r, ok := set.char(s[0], s[1]); ok {
		return r, 2, ""
	}
	if s[1] > 0x20 && s[1] < 0x7f {
		return 0, 2, "invalid double-byte character"
	}
	return 0, 1, "invalid double-byte character"
}

// ISO-2022-KR of RFC 1557: ASCII, with KS X 1001 shifted in by SO and out by SI once ESC $ ) C designates it
type iso2022KR struct{}

const krDesignation = "\x1b$)C"

func (iso2022KR) decode(data []byte, replace bool) (string, int, string) {
	set := ksX1001()
	out := make([]byte, 0, 2*len(data))
	shifted := false
	for p := 0; p < len(data); {
		c := data[p]
		var r rune
		n, reason := 1, ""
		switch {
		case c == esc && bytes.HasPrefix(data[p:], []byte(krDesignation)):
			p += len(krDesignation)
			continue
		case c == esc && len(data)-p < len(krDesignation) && strings.HasPrefix(krDesignation, string(data[p:])):
			reason, n = "incomplete escape sequence", len(data)-p
		case c == esc:
			reason = "invalid escape sequence"
		case c == so || c == si:
			shifted, p = c == so, p+1
			continue
		case c >= utf8.RuneSelf:
			reason = "byte above 0x7f"
		case shifted && c > 0x20 && c < 0x7f:
			if r, n, reason = doubleByte(set, data[p:]); reason == "" {
				out, p = utf8.AppendRune(out, r), p+n
				continue
			}
		default:
			if c == '\n' {
				shifted = false
			}
			out, p = append(out, c), p+1
			continue
		}
		if !replace {
			return "", p, reason
		}
		out, p = append(out, replacement...), p+n
	}
	return string(out), -1, ""
}

func (iso2022KR) encode(text string) ([]byte, int) {
	set := ksX1001()
	out := make([]byte, 0, 2*len(text)+len(krDesignation))
	if strings.IndexFunc(text, func(r rune) bool { return r >= utf8.RuneSelf }) >= 0 {
		out = append(out, krDesignation...)
	}
	shifted := false
	for i, r := range text {
		if r < utf8.RuneSelf {
			if shifted {
				out, shifted = append(out, si), false
			}
			out = append(out, byte(r))
			continue
		}
		c1, c2, ok := set.code(r)
		if !ok {
			return nil, i
		}
		if !shifted {
			out, shifted = append(out, so), true
		}
		out = append(out, c1, c2)
	}
	if shifted {
		out = append(out, si)
	}
	return out, -1
}

// ISO-2022-JP-2 of RFC 1554: ASCII, JIS X 0201, JIS X 0208, JIS X 0212, GB 2312 and KS C 5601 designated to G0, the upper halves of ISO-8859-1 and ISO-8859-7 reached through single shifts from G2
type iso2022JP2 struct{}

// Graphic sets ISO-2022-JP-2 designates
const (
	setASCII = iota
	setRoman
	setKatakana
	setJISX0208
	setJISX0212
	setGB2312
	setKSX1001
	setLatin1
	setGreek
)

// Designating escape sequences the decoder accepts, the JIS X 0201 Katakana and four byte forms besides those RFC 1554 lists
var jp2Escapes = [...]struct {
	seq string
	set int
}{
	{"\x1b(B", setASCII}, {"\x1b(J", setRoman}, {"\x1b(I", setKatakana},
	{"\x1b$@", setJISX0208}, {"\x1b$B", setJISX0208}, {"\x1b&@\x1b$B", setJISX0208},
	{"\x1b$(@", setJISX0208}, {"\x1b$(B", setJISX0208}, {"\x1b$A", setGB2312}, {"\x1b$(A", setGB2312},
	{"\x1b$(C", setKSX1001}, {"\x1b$(D", setJISX0212}, {"\x1b.A", setLatin1}, {"\x1b.F", setGreek},
}

// Set an escape sequence at the start of s designates and its length, zero with cut set when s ends inside a known sequence
func jp2Escape(s []byte) (set, n int, cut bool) {
	for _, e := range jp2Escapes {
		if len(s) >= len(e.seq) && string(s[:len(e.seq)]) == e.seq {
			return e.set, len(e.seq), false
		}
		if len(s) < len(e.seq) && string(s) == e.seq[:len(s)] {
			cut = true
		}
	}
	return 0, 0, cut
}

// Character a single shift two reaches through the G2 set, false without a G2 set or for a byte that set leaves undefined
func jp2Shifted(g2 int, c byte) (rune, bool) {
	if c < 0x20 || c > 0x7f {
		return 0, false
	}
	switch g2 {
	case setLatin1:
		return rune(c | 0x80), true
	case setGreek:
		r := charmap.ISO8859_7.DecodeByte(c | 0x80)
		return r, r != utf8.RuneError
	}
	return 0, false
}

// Graphic character at the start of s in the G0 set, its length, or why it does not decode
func jp2Char(g0 int, s []byte) (rune, int, string) {
	c := s[0]
	switch g0 {
	case setASCII:
		return rune(c), 1, ""
	case setRoman:
		switch c {
		case '\\':
			return 0xa5, 1, ""
		case '~':
			return 0x203e, 1, ""
		}
		return rune(c), 1, ""
	case setKatakana:
		if c <= 0x5f {
			return 0xff61 + rune(c-0x21), 1, ""
		}
		return 0, 1, "invalid katakana byte"
	case setJISX0208:
		return doubleByte(jisX0208(), s)
	case setJISX0212:
		return doubleByte(jisX0212(), s)
	case setGB2312:
		return doubleByte(gb2312(), s)
	}
	return doubleByte(ksX1001(), s)
}

func (iso2022JP2) decode(data []byte, replace bool) (string, int, string) {
	out := make([]byte, 0, 2*len(data))
	g0, g2 := setASCII, -1
	for p := 0; p < len(data); {
		c := data[p]
		var r rune
		n, reason := 1, ""
		switch {
		case c == esc:
			set, m, cut := jp2Escape(data[p:])
			switch {
			case m > 0 && (set == setLatin1 || set == setGreek):
				g2, p = set, p+m
				continue
			case m > 0:
				g0, p = set, p+m
				continue
			case cut:
				reason, n = "incomplete escape sequence", len(data)-p
			case p+1 < len(data) && data[p+1] == 'N':
				if p+2 == len(data) {
					reason, n = "incomplete escape sequence", 2
				} else if r, ok := jp2Shifted(g2, data[p+2]); ok {
					out, p = utf8.AppendRune(out, r), p+3
					continue
				} else if b := data[p+2]; b < 0x20 || b > 0x7f {
					reason, n = "invalid single shift", 2
				} else {
					reason, n = "invalid single shift", 3
				}
			default:
				reason = "invalid escape sequence"
			}
		case c >= utf8.RuneSelf:
			reason = "byte above 0x7f"
		case c <= 0x20 || c == 0x7f:
			if c == '\n' {
				g0 = setASCII
			}
			out, p = append(out, c), p+1
			continue
		default:
			if r, n, reason = jp2Char(g0, data[p:]); reason == "" {
				out, p = utf8.AppendRune(out, r), p+n
				continue
			}
		}
		if !replace {
			return "", p, reason
		}
		out, p = append(out, replacement...), p+n
	}
	return string(out), -1, ""
}

// Encode through JIS X 0208 first, the ISO-8859-1 upper half for its characters, then JIS X 0212, KS C 5601, GB 2312 and the ISO-8859-7 upper half; half-width katakana goes as its full-width form
func (iso2022JP2) encode(text string) ([]byte, int) {
	out := make([]byte, 0, len(text)+16)
	g0, g2 := setASCII, -1
	designate := func(set int, seq string) {
		if g0 != set {
			out, g0 = append(out, seq...), set
		}
	}
	shift := func(set int, seq string, b byte) {
		if g2 != set {
			out, g2 = append(out, seq...), set
		}
		out = append(out, esc, 'N', b-0x80)
	}
	for i, r := range text {
		if r < utf8.RuneSelf {
			designate(setASCII, "\x1b(B")
			out = append(out, byte(r))
			if r == '\n' {
				g2 = -1
			}
			continue
		}
		if r >= 0xff61 && r <= 0xff9f {
			if wide := width.LookupRune(r).Wide(); wide != 0 {
				r = wide
			}
		}
		if c1, c2, ok := jisX0208().code(r); ok {
			designate(setJISX0208, "\x1b$B")
			out = append(out, c1, c2)
			continue
		}
		if r >= 0xa0 && r <= 0xff {
			shift(setLatin1, "\x1b.A", byte(r))
			continue
		}
		if c1, c2, set, seq, ok := jp2OtherDoubleByte(r); ok {
			designate(set, seq)
			out = append(out, c1, c2)
			continue
		}
		if b, ok := charmap.ISO8859_7.EncodeRune(r); ok && b >= 0xa0 {
			shift(setGreek, "\x1b.F", b)
			continue
		}
		return nil, i
	}
	designate(setASCII, "\x1b(B")
	return out, -1
}

// Row and cell bytes of a character in JIS X 0212, KS C 5601 or GB 2312, the first that has it, with the set and the escape that designates it
func jp2OtherDoubleByte(r rune) (c1, c2 byte, set int, seq string, ok bool) {
	if c1, c2, ok = jisX0212().code(r); ok {
		return c1, c2, setJISX0212, "\x1b$(D", true
	}
	if c1, c2, ok = ksX1001().code(r); ok {
		return c1, c2, setKSX1001, "\x1b$(C", true
	}
	if c1, c2, ok = gb2312().code(r); ok {
		return c1, c2, setGB2312, "\x1b$A", true
	}
	return 0, 0, 0, "", false
}
