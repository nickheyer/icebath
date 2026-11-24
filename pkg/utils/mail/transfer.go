package mailutil

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
)

// Bytes of base64 text, lenient with what real mail carries: characters outside the alphabet skipped, padding missing or repeated, and pieces padded on their own run together
func decodeBase64(src []byte) []byte {
	dst := make([]byte, base64.StdEncoding.DecodedLen(len(src)))
	if n, err := base64.StdEncoding.Decode(dst, src); err == nil {
		return dst[:n]
	}
	out := dst[:0]
	var quad [4]byte
	n := 0
	for _, c := range src {
		v, ok := base64Value(c)
		if !ok {
			if c == '=' && n >= 2 {
				out = appendQuantum(out, quad, n)
				n = 0
			}
			continue
		}
		quad[n] = v
		if n++; n == 4 {
			out = appendQuantum(out, quad, 4)
			n = 0
		}
	}
	if n >= 2 {
		out = appendQuantum(out, quad, n)
	}
	return out
}

// Six bit value of a base64 character, false for one outside the alphabet
func base64Value(c byte) (byte, bool) {
	switch {
	case c >= 'A' && c <= 'Z':
		return c - 'A', true
	case c >= 'a' && c <= 'z':
		return c - 'a' + 26, true
	case c >= '0' && c <= '9':
		return c - '0' + 52, true
	case c == '+':
		return 62, true
	case c == '/':
		return 63, true
	}
	return 0, false
}

// Bytes the first n characters of a base64 quantum carry, n being 2, 3 or 4
func appendQuantum(out []byte, q [4]byte, n int) []byte {
	out = append(out, q[0]<<2|q[1]>>4)
	if n > 2 {
		out = append(out, q[1]<<4|q[2]>>2)
	}
	if n > 3 {
		out = append(out, q[2]<<6|q[3])
	}
	return out
}

// Bytes of quoted-printable text, lenient like the decoders mail clients use: white space ending a line dropped, a = that starts no escape kept, control and 8 bit bytes passed through, line ends kept as they are
func decodeQuotedPrintable(src []byte) []byte {
	out := make([]byte, 0, len(src))
	for pos := 0; pos < len(src); {
		line, next := lineAt(src, pos)
		eol := src[pos+len(line) : next]
		line = bytes.TrimRight(line, " \t")
		soft := false
		if n := len(line); n > 0 && line[n-1] == '=' {
			line, soft = line[:n-1], true
		}
		for i := 0; i < len(line); i++ {
			if c := line[i]; c == '=' && i+2 < len(line) && isHex(line[i+1]) && isHex(line[i+2]) {
				out = append(out, unhex(line[i+1])<<4|unhex(line[i+2]))
				i += 2
			} else {
				out = append(out, c)
			}
		}
		if !soft {
			out = append(out, eol...)
		}
		pos = next
	}
	return out
}

// Bytes of a uuencoded body between its begin and end lines, false when there is no begin line or a line is broken
func decodeUU(src []byte) ([]byte, bool) {
	var out []byte
	begun := false
	for pos := 0; pos < len(src); {
		line, next := lineAt(src, pos)
		pos = next
		if !begun {
			if rest, ok := bytes.CutPrefix(line, []byte("begin ")); ok {
				mode, _, _ := bytes.Cut(rest, []byte(" "))
				_, err := strconv.ParseUint(strings.ReplaceAll(strings.TrimSpace(string(mode)), "_", ""), 8, 32)
				begun = err == nil
			}
			continue
		}
		if len(line) == 0 {
			return nil, false
		}
		if string(bytes.TrimSpace(line)) == "end" {
			return out, true
		}
		chunk, ok := uuLine(line)
		if !ok {
			n := ((int(line[0]-' ')&0x3f)*4 + 5) / 3
			if chunk, ok = uuLine(line[:min(n, len(line))]); !ok {
				return nil, false
			}
		}
		out = append(out, chunk...)
	}
	return out, begun
}

// Bytes of one uuencoded line, false for a character outside the alphabet or data after the length the line declares
func uuLine(line []byte) ([]byte, bool) {
	n := int(line[0]-' ') & 0x3f
	rest := line[1:]
	out := make([]byte, 0, n)
	var acc uint32
	bits, i := 0, 0
	for ; len(out) < n; i++ {
		var v byte
		if i < len(rest) {
			c := rest[i]
			if c < ' ' || c > ' '+64 {
				return nil, false
			}
			v = (c - ' ') & 0x3f
		}
		acc = acc<<6 | uint32(v)
		if bits += 6; bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
			acc &= 1<<bits - 1
		}
	}
	for ; i < len(rest); i++ {
		if c := rest[i]; c != ' ' && c != ' '+64 {
			return nil, false
		}
	}
	return out, true
}
