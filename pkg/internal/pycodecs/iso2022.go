package pycodecs

import (
	"strings"
	"sync"
)

const (
	isoESC  = 0x1b
	isoSO   = 0x0e
	isoSI   = 0x0f
	isoLF   = 0x0a
	isoDBCS = 0x80

	isoASCII    = 'B'
	isoISO88591 = 'A'
	isoISO88597 = 'F'
	isoJISX0208 = 'B' | isoDBCS

	isoNoShift     = 0x01
	isoUseG2       = 0x02
	isoJISX0208Ext = 0x04

	isoMaxEscSeqLen = 16
)

// One charset a variant can designate, its final byte with the DBCS flag, plane, width and probed table
type isoDesignation struct {
	mark  byte
	plane int
	width int
	table string
}

// One iso2022 codec, its flags and designations in the order the encoder tries them
type isoVariant struct {
	name  string
	flags int
	dsgs  []isoDesignation
}

var (
	isoKSX1001G0 = isoDesignation{'C' | isoDBCS, 0, 2, "ksx1001"}
	isoKSX1001G1 = isoDesignation{'C' | isoDBCS, 1, 2, "ksx1001"}
	isoJISX0201R = isoDesignation{'J', 0, 1, "jisx0201_r"}
	isoJISX0201K = isoDesignation{'I', 0, 1, "jisx0201_k"}
	isoJIS0208   = isoDesignation{isoJISX0208, 0, 2, "jisx0208"}
	isoJIS0208O  = isoDesignation{'@' | isoDBCS, 0, 2, "jisx0208"}
	isoJIS0212   = isoDesignation{'D' | isoDBCS, 0, 2, "jisx0212"}
	isoJIS2000_1 = isoDesignation{'O' | isoDBCS, 0, 2, "jisx0213_2000_1"}
	isoJIS2000_2 = isoDesignation{'P' | isoDBCS, 0, 2, "jisx0213_2000_2"}
	isoJIS2004_1 = isoDesignation{'Q' | isoDBCS, 0, 2, "jisx0213_2004_1"}
	isoJIS2004_2 = isoDesignation{'P' | isoDBCS, 0, 2, "jisx0213_2004_2"}
	isoGB2312    = isoDesignation{'A' | isoDBCS, 0, 2, "gb2312"}
	isoLatin1G2  = isoDesignation{isoISO88591, 2, 1, ""}
	isoGreekG2   = isoDesignation{isoISO88597, 2, 1, ""}
)

// The seven CPython iso2022 variants with the designation lists of _codecs_iso2022.c
var isoVariants = map[string]*isoVariant{
	"iso2022_kr": {"iso2022_kr", 0, []isoDesignation{isoKSX1001G1}},
	"iso2022_jp": {"iso2022_jp", isoNoShift | isoJISX0208Ext, []isoDesignation{isoJIS0208, isoJISX0201R, isoJIS0208O}},
	"iso2022_jp_1": {"iso2022_jp_1", isoNoShift | isoJISX0208Ext,
		[]isoDesignation{isoJIS0208, isoJIS0212, isoJISX0201R, isoJIS0208O}},
	"iso2022_jp_2": {"iso2022_jp_2", isoNoShift | isoUseG2 | isoJISX0208Ext,
		[]isoDesignation{isoJIS0208, isoJIS0212, isoKSX1001G0, isoGB2312, isoJISX0201R, isoJIS0208O, isoLatin1G2, isoGreekG2}},
	"iso2022_jp_2004": {"iso2022_jp_2004", isoNoShift | isoJISX0208Ext,
		[]isoDesignation{isoJIS2004_1, isoJIS0208, isoJIS2004_1, isoJIS2004_2}},
	"iso2022_jp_3": {"iso2022_jp_3", isoNoShift | isoJISX0208Ext,
		[]isoDesignation{isoJIS2000_1, isoJIS0208, isoJIS2000_1, isoJIS2000_2}},
	"iso2022_jp_ext": {"iso2022_jp_ext", isoNoShift | isoJISX0208Ext,
		[]isoDesignation{isoJIS0208, isoJIS0212, isoJISX0201R, isoJISX0201K, isoJIS0208O}},
}

var isoCharsetTables sync.Map

// Probed decode table of one charset keyed by its one or two code bytes
func isoCharset(name string) map[uint16]string {
	if v, ok := isoCharsetTables.Load(name); ok {
		return v.(map[uint16]string)
	}
	width := 2
	if name == "jisx0201_r" || name == "jisx0201_k" {
		width = 1
	}
	table := map[uint16]string{}
	r := &blobReader{b: blobBytes(cjkBlobs, "cjk", "cs."+name)}
	for r.more() {
		key := uint16(r.u8())
		if width == 2 {
			key = key<<8 | uint16(r.u8())
		}
		table[key] = r.str()
	}
	v, _ := isoCharsetTables.LoadOrStore(name, table)
	return v.(map[uint16]string)
}

// Encoder choice for one character or pair, the designation mark, its plane and the code bytes
type isoCode struct {
	mark  byte
	plane int
	code  []byte
}

// Probed encoder choices of a variant for single characters and JIS X 0213 pairs
type isoEncodeTables struct {
	single map[rune]isoCode
	pairs  map[[2]rune]isoCode
}

var isoEncodeCache sync.Map

func isoEncoder(name string) *isoEncodeTables {
	if v, ok := isoEncodeCache.Load(name); ok {
		return v.(*isoEncodeTables)
	}
	t := &isoEncodeTables{single: map[rune]isoCode{}, pairs: map[[2]rune]isoCode{}}
	r := &blobReader{b: blobBytes(cjkBlobs, "cjk", name+".enc")}
	for r.more() {
		cp := r.u24()
		mark, plane := byte(r.u8()), r.u8()
		t.single[cp] = isoCode{mark, plane, r.raw()}
	}
	r = &blobReader{b: blobBytes(cjkBlobs, "cjk", name+".pairs")}
	for r.more() {
		a, b := r.u24(), r.u24()
		mark, plane := byte(r.u8()), r.u8()
		t.pairs[[2]rune{a, b}] = isoCode{mark, plane, r.raw()}
	}
	v, _ := isoEncodeCache.LoadOrStore(name, t)
	return v.(*isoEncodeTables)
}

func isEscEnd(c byte) bool { return (c >= 'A' && c <= 'Z') || c == '@' }

func isISO2022Esc(c byte) bool { return c == '(' || c == ')' || c == '$' || c == '.' || c == '&' }

// Decoder state of one iso2022 decode, the three designated sets and the shift and escape throughout flags
type isoDecodeState struct {
	g             [3]byte
	shifted       bool
	escThroughout bool
}

// Designation of the variant with a mark, nil when the variant has none
func (v *isoVariant) find(mark byte) *isoDesignation {
	for i := range v.dsgs {
		if v.dsgs[i].mark == mark {
			return &v.dsgs[i]
		}
	}
	return nil
}

// Escape sequence at buf processed into the state, the consumed length or the CPython error code
func (v *isoVariant) processEsc(st *isoDecodeState, buf []byte) (int, int) {
	esclen := 0
	for i := 1; i < isoMaxEscSeqLen; i++ {
		if i >= len(buf) {
			return 0, mberrTooFew
		}
		if isEscEnd(buf[i]) {
			esclen = i + 1
			break
		} else if v.flags&isoJISX0208Ext != 0 && i+1 < len(buf) && buf[i] == '&' && buf[i+1] == '@' {
			i += 2
		}
	}
	var charset byte
	designation := 0
	switch esclen {
	case 0:
		return 0, 1
	case 3:
		if buf[1] == '$' {
			charset = buf[2] | isoDBCS
		} else {
			charset = buf[2]
			switch {
			case buf[1] == '(':
			case buf[1] == ')':
				designation = 1
			case v.flags&isoUseG2 != 0 && buf[1] == '.':
				designation = 2
			default:
				return 0, 3
			}
		}
	case 4:
		if buf[1] != '$' {
			return 0, 4
		}
		charset = buf[3] | isoDBCS
		switch buf[2] {
		case '(':
		case ')':
			designation = 1
		default:
			return 0, 4
		}
	case 6:
		if v.flags&isoJISX0208Ext != 0 && buf[3] == isoESC && buf[4] == '$' && buf[5] == 'B' {
			charset = 'B' | isoDBCS
		} else {
			return 0, 6
		}
	default:
		return 0, esclen
	}
	if charset != isoASCII && v.find(charset) == nil {
		return 0, esclen
	}
	st.g[designation] = charset
	return esclen, 0
}

// MBERR_TOOFEW of a sequence cut by the end of the input
const mberrTooFew = -2

// Character of ISO-8859-7 the G2 single shift decodes, false for a byte the macro leaves unmapped
func iso88597Decode(c int) (rune, bool) {
	switch {
	case c < 0xa0:
		return rune(c), true
	case c < 0xc0 && (0x288f3bc9&(1<<(c-0xa0))) != 0:
		return rune(c), true
	case c >= 0xb4 && c <= 0xfe && (c >= 0xd4 || (0xbffffd77&(1<<(c-0xb4))) != 0):
		return rune(0x02d0 + c), true
	case c == 0xa1:
		return 0x2018, true
	case c == 0xa2:
		return 0x2019, true
	case c == 0xaf:
		return 0x2015, true
	}
	return 0, false
}

// Single shift two character at buf, the CPython error code or the internal error for an unsupported G2 set
func (st *isoDecodeState) processG2(b *strings.Builder, buf []byte) (int, error) {
	c := buf[2]
	switch st.g[2] {
	case isoISO88591:
		if c >= 0x80 {
			return 3, nil
		}
		b.WriteRune(rune(c) + 0x80)
	case isoISO88597:
		r, ok := iso88597Decode(int(c ^ 0x80))
		if !ok {
			return 3, nil
		}
		b.WriteRune(r)
	case isoASCII:
		if c&0x80 != 0 {
			return 3, nil
		}
		b.WriteByte(c)
	default:
		return 0, &codecRuntimeError{"internal codec error"}
	}
	return 0, nil
}

// iso2022 decode of one variant, a strict error raised or U+FFFD written over its bytes with the state kept
func (v *isoVariant) decode(data []byte, replace bool) (string, error) {
	st := &isoDecodeState{g: [3]byte{isoASCII, isoASCII, isoASCII}}
	var b strings.Builder
	p := 0
	// Raises the error of esize bytes at p, or writes U+FFFD and skips them
	fail := func(esize int) error {
		reason := mbIllegal
		if esize == mberrTooFew {
			reason, esize = mbIncomplete, len(data)-p
		}
		if !replace {
			return decodeErr(v.name, data, p, p+esize, reason)
		}
		b.WriteRune(0xfffd)
		p += esize
		return nil
	}
	for p < len(data) {
		c := data[p]
		if st.escThroughout {
			b.WriteRune(rune(c))
			p++
			if isEscEnd(c) {
				st.escThroughout = false
			}
			continue
		}
		bypass := false
		switch {
		case c == isoESC:
			if p+2 > len(data) {
				if err := fail(mberrTooFew); err != nil {
					return "", err
				}
				continue
			}
			c2 := data[p+1]
			switch {
			case isISO2022Esc(c2):
				n, code := v.processEsc(st, data[p:])
				if code != 0 {
					if err := fail(code); err != nil {
						return "", err
					}
					continue
				}
				p += n
			case v.flags&isoUseG2 != 0 && c2 == 'N':
				if p+3 > len(data) {
					if err := fail(mberrTooFew); err != nil {
						return "", err
					}
					continue
				}
				code, err := st.processG2(&b, data[p:])
				if err != nil {
					return "", err
				}
				if code != 0 {
					if err := fail(code); err != nil {
						return "", err
					}
					continue
				}
				p += 3
			default:
				b.WriteByte(isoESC)
				st.escThroughout = true
				p++
			}
		case c == isoSI:
			if v.flags&isoNoShift != 0 {
				bypass = true
				break
			}
			st.shifted = false
			p++
		case c == isoSO:
			if v.flags&isoNoShift != 0 {
				bypass = true
				break
			}
			st.shifted = true
			p++
		case c == isoLF:
			st.shifted = false
			b.WriteByte(isoLF)
			p++
		case c < 0x20:
			bypass = true
		case c >= 0x80:
			if err := fail(1); err != nil {
				return "", err
			}
		default:
			charset := st.g[0]
			if st.shifted {
				charset = st.g[1]
			}
			if charset == isoASCII {
				bypass = true
				break
			}
			dsg := v.find(charset)
			if p+dsg.width > len(data) {
				if err := fail(mberrTooFew); err != nil {
					return "", err
				}
				continue
			}
			decoded, ok := "", false
			if dsg.table != "" {
				key := uint16(data[p])
				if dsg.width == 2 {
					key = key<<8 | uint16(data[p+1])
				}
				decoded, ok = isoCharset(dsg.table)[key]
			}
			if !ok {
				if err := fail(dsg.width); err != nil {
					return "", err
				}
				continue
			}
			b.WriteString(decoded)
			p += dsg.width
		}
		if bypass {
			b.WriteByte(c)
			p++
		}
	}
	return b.String(), nil
}

// iso2022 encode of one variant, designations and shifts emitted as the CPython encoder emits them
func (v *isoVariant) encode(text string) (string, error) {
	tables := isoEncoder(v.name)
	trunc := pairTruncates[v.name]
	runes := textRunes(text)
	g0, g1 := byte(isoASCII), byte(isoASCII)
	shifted := false
	var out []byte
	for i := 0; i < len(runes); {
		c := runes[i]
		if c < 0x80 {
			if g0 != isoASCII {
				out = append(out, isoESC, '(', 'B')
				g0 = isoASCII
			}
			if shifted {
				out = append(out, isoSI)
				shifted = false
			}
			out = append(out, byte(c))
			i++
			continue
		}
		insize := 1
		choice, ok := isoCode{}, false
		if i+1 < len(runes) && len(tables.pairs) > 0 {
			if n := runes[i+1]; trunc || n <= 0xffff {
				if choice, ok = tables.pairs[[2]rune{c, n & 0xffff}]; ok {
					insize = 2
				}
			}
		}
		if !ok {
			if choice, ok = tables.single[c]; !ok {
				return "", encodeErr(v.name, runes, i, i+1, mbIllegal)
			}
		}
		width := len(choice.code)
		switch choice.plane {
		case 0:
			if shifted {
				out = append(out, isoSI)
				shifted = false
			}
			if g0 != choice.mark {
				switch {
				case width == 1:
					out = append(out, isoESC, '(', choice.mark&0x7f)
				case choice.mark == isoJISX0208:
					out = append(out, isoESC, '$', choice.mark&0x7f)
				default:
					out = append(out, isoESC, '$', '(', choice.mark&0x7f)
				}
				g0 = choice.mark
			}
		case 1:
			if g1 != choice.mark {
				if width == 1 {
					out = append(out, isoESC, ')', choice.mark&0x7f)
				} else {
					out = append(out, isoESC, '$', ')', choice.mark&0x7f)
				}
				g1 = choice.mark
			}
			if !shifted {
				out = append(out, isoSO)
				shifted = true
			}
		}
		out = append(out, choice.code...)
		i += insize
	}
	if shifted {
		out = append(out, isoSI)
	}
	if g0 != isoASCII {
		out = append(out, isoESC, '(', 'B')
	}
	return string(out), nil
}
