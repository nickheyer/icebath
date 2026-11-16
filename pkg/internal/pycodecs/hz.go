package pycodecs

import (
	"strings"
	"sync"
)

var hzEncodeTable = sync.OnceValue(func() map[rune][]byte {
	table := map[rune][]byte{}
	r := &blobReader{b: blobBytes(cjkBlobs, "cjk", "hz.enc")}
	for r.more() {
		cp := r.u24()
		table[cp] = []byte{byte(r.u8()), byte(r.u8())}
	}
	return table
})

// HZ decode of _codecs_cn.c, tilde escapes switching GB2312 on and off, a strict error raised or U+FFFD written over it
func decodeHZ(data []byte, replace bool) (string, error) {
	gb := isoCharset("gb2312")
	var b strings.Builder
	gbMode := false
	p := 0
	// Raises the error of esize bytes at p, or writes U+FFFD and skips them
	fail := func(esize int) error {
		reason := mbIllegal
		if esize == mberrTooFew {
			reason, esize = mbIncomplete, len(data)-p
		}
		if !replace {
			return decodeErr("hz", data, p, p+esize, reason)
		}
		b.WriteRune(0xfffd)
		p += esize
		return nil
	}
	for p < len(data) {
		c := data[p]
		if c == '~' {
			if p+2 > len(data) {
				if err := fail(mberrTooFew); err != nil {
					return "", err
				}
				continue
			}
			c2 := data[p+1]
			switch {
			case c2 == '~' && !gbMode:
				b.WriteByte('~')
			case c2 == '{' && !gbMode:
				gbMode = true
			case c2 == '\n' && !gbMode:
			case c2 == '}' && gbMode:
				gbMode = false
			default:
				if err := fail(1); err != nil {
					return "", err
				}
				continue
			}
			p += 2
			continue
		}
		if c&0x80 != 0 {
			if err := fail(1); err != nil {
				return "", err
			}
			continue
		}
		if !gbMode {
			b.WriteByte(c)
			p++
			continue
		}
		if p+2 > len(data) {
			if err := fail(mberrTooFew); err != nil {
				return "", err
			}
			continue
		}
		decoded, ok := gb[uint16(c)<<8|uint16(data[p+1])]
		if !ok {
			if err := fail(1); err != nil {
				return "", err
			}
			continue
		}
		b.WriteString(decoded)
		p += 2
	}
	return b.String(), nil
}

// HZ encode of _codecs_cn.c, GB2312 runs between tilde braces and a literal tilde doubled
func encodeHZ(text string) (string, error) {
	table := hzEncodeTable()
	runes := textRunes(text)
	gbMode := false
	var out []byte
	for i, c := range runes {
		if c < 0x80 {
			if gbMode {
				out = append(out, '~', '}')
				gbMode = false
			}
			out = append(out, byte(c))
			if c == '~' {
				out = append(out, '~')
			}
			continue
		}
		code, ok := table[c]
		if !ok {
			return "", encodeErr("hz", runes, i, i+1, mbIllegal)
		}
		if !gbMode {
			out = append(out, '~', '{')
			gbMode = true
		}
		out = append(out, code...)
	}
	if gbMode {
		out = append(out, '~', '}')
	}
	return string(out), nil
}
