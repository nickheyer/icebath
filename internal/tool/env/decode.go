package env

import (
	"strings"
	"unicode/utf8"
)

type utf8Decoder struct {
	pending []byte
}

func (d *utf8Decoder) decode(chunk []byte, final bool) string {
	data := chunk
	if len(d.pending) > 0 {
		data = append(d.pending, chunk...)
		d.pending = nil
	}
	var b strings.Builder
	b.Grow(len(data))
	i := 0
	for i < len(data) {
		c := data[i]
		if c < utf8.RuneSelf {
			b.WriteByte(c)
			i++
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			b.Write(data[i : i+size])
			i += size
			continue
		}
		n, incomplete := invalidSpan(data[i:])
		if incomplete && !final {
			d.pending = append([]byte(nil), data[i:]...)
			break
		}
		b.WriteRune(utf8.RuneError)
		i += n
	}
	return b.String()
}

func invalidSpan(p []byte) (int, bool) {
	need, lo, hi := 0, byte(0x80), byte(0xBF)
	switch lead := p[0]; {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		need, lo = 2, 0xA0
	case lead >= 0xE1 && lead <= 0xEC, lead == 0xEE, lead == 0xEF:
		need = 2
	case lead == 0xED:
		need, hi = 2, 0x9F
	case lead == 0xF0:
		need, lo = 3, 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	case lead == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1, false
	}
	for k := 1; k <= need; k++ {
		if k >= len(p) {
			return k, true
		}
		low, high := byte(0x80), byte(0xBF)
		if k == 1 {
			low, high = lo, hi
		}
		if p[k] < low || p[k] > high {
			return k, false
		}
	}
	return need + 1, false
}
