package tool

import (
	"bytes"
	"encoding/json"
	"strconv"
)

type ExactString struct {
	Text  string
	First rune
	Held  bool
}

func (c *Call) ExactArg(key string) (ExactString, bool) {
	if c == nil || c.Arguments == "" {
		return ExactString{}, false
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(c.Arguments), &members); err != nil {
		return ExactString{}, false
	}
	raw, ok := members[key]
	if !ok {
		return ExactString{}, false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return ExactString{}, false
	}
	return exactUnquote(raw[1 : len(raw)-1]), true
}

func exactUnquote(body []byte) ExactString {
	var x ExactString
	out := make([]byte, 0, len(body))
	for i := 0; i < len(body); i++ {
		b := body[i]
		if b != '\\' || i+1 >= len(body) {
			out = append(out, b)
			continue
		}
		i++
		switch body[i] {
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			u, ok := hex4(body, i+1)
			if !ok {
				out = append(out, '\\', 'u')
				continue
			}
			i += 4
			if u >= 0xD800 && u <= 0xDBFF && i+6 < len(body) && body[i+1] == '\\' && body[i+2] == 'u' {
				if low, ok := hex4(body, i+3); ok && low >= 0xDC00 && low <= 0xDFFF {
					out = append(out, string(rune(0x10000+(u-0xD800)<<10+(low-0xDC00)))...)
					i += 6
					continue
				}
			}
			switch {
			case u >= 0xDC80 && u <= 0xDCFF:
				x.Held = true
				out = append(out, byte(u-0xDC00))
			case u >= 0xD800 && u <= 0xDFFF:
				if x.First == 0 {
					x.First = rune(u)
				}
				x.Held = true
				out = append(out, byte(0xE0|u>>12), byte(0x80|(u>>6)&0x3F), byte(0x80|u&0x3F))
			default:
				out = append(out, string(rune(u))...)
			}
		default:
			out = append(out, body[i])
		}
	}
	x.Text = string(out)
	return x
}

func hex4(body []byte, at int) (int, bool) {
	if at+4 > len(body) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(body[at:at+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	return int(v), true
}
