package pycodecs

import "strings"

// RFC 2152 character classes of the CPython encoder, 0 set D, 1 set O, 2 whitespace, 3 special
var utf7Category = [128]byte{
	3, 3, 3, 3, 3, 3, 3, 3, 3, 2, 2, 3, 3, 2, 3, 3,
	3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3,
	2, 1, 1, 1, 1, 1, 1, 0, 0, 0, 1, 3, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 0,
	1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 3, 1, 1, 1,
	1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 3, 3,
}

func isBase64Byte(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/'
}

func fromBase64(c byte) uint64 {
	switch {
	case c >= 'A' && c <= 'Z':
		return uint64(c - 'A')
	case c >= 'a' && c <= 'z':
		return uint64(c-'a') + 26
	case c >= '0' && c <= '9':
		return uint64(c-'0') + 52
	case c == '+':
		return 62
	}
	return 63
}

const base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

func utf7DecodeDirect(c byte) bool { return c <= 127 && c != '+' }

// UTF-7 decode of CPython, its error positions and the pending surrogate handling, replace writing U+FFFD
func decodeUTF7(data []byte, replace bool) (string, error) {
	var b strings.Builder
	inShift := false
	startinpos := 0
	var base64bits uint
	var base64buffer uint64
	var surrogate rune
	e := len(data)
	s := 0
	// Raises the error over startinpos to endinpos, or writes U+FFFD and resumes at endinpos
	fail := func(endinpos int, msg string) error {
		if !replace {
			return decodeErr("utf7", data, startinpos, endinpos, msg)
		}
		b.WriteRune(0xfffd)
		s = endinpos
		return nil
	}
	for {
		for s < e {
			ch := data[s]
			if inShift {
				if isBase64Byte(ch) {
					base64buffer = base64buffer<<6 | fromBase64(ch)
					base64bits += 6
					s++
					if base64bits >= 16 {
						outCh := rune(base64buffer >> (base64bits - 16))
						base64bits -= 16
						base64buffer &= (1 << base64bits) - 1
						if surrogate != 0 {
							if outCh >= 0xdc00 && outCh <= 0xdfff {
								writeCodePoint(&b, 0x10000+((surrogate-0xd800)<<10)+(outCh-0xdc00))
								surrogate = 0
								continue
							}
							writeCodePoint(&b, surrogate)
							surrogate = 0
						}
						if outCh >= 0xd800 && outCh <= 0xdbff {
							surrogate = outCh
						} else {
							writeCodePoint(&b, outCh)
						}
					}
					continue
				}
				inShift = false
				if base64bits > 0 {
					if base64bits >= 6 {
						s++
						if err := fail(s, "partial character in shift sequence"); err != nil {
							return "", err
						}
						continue
					}
					if base64buffer != 0 {
						s++
						if err := fail(s, "non-zero padding bits in shift sequence"); err != nil {
							return "", err
						}
						continue
					}
				}
				if surrogate != 0 && utf7DecodeDirect(ch) {
					writeCodePoint(&b, surrogate)
				}
				surrogate = 0
				if ch == '-' {
					s++
				}
				continue
			}
			switch {
			case ch == '+':
				startinpos = s
				s++
				switch {
				case s < e && data[s] == '-':
					s++
					b.WriteByte('+')
				case s < e && !isBase64Byte(data[s]):
					s++
					if err := fail(s, "ill-formed sequence"); err != nil {
						return "", err
					}
				default:
					inShift = true
					surrogate = 0
					base64bits = 0
					base64buffer = 0
				}
			case utf7DecodeDirect(ch):
				s++
				b.WriteByte(ch)
			default:
				startinpos = s
				s++
				if err := fail(s, "unexpected special character"); err != nil {
					return "", err
				}
			}
		}
		if !inShift {
			break
		}
		inShift = false
		if surrogate == 0 && base64bits < 6 && (base64bits == 0 || base64buffer == 0) {
			break
		}
		if err := fail(len(data), "unterminated shift sequence"); err != nil {
			return "", err
		}
		if s >= e {
			break
		}
	}
	return b.String(), nil
}

// UTF-7 encode of CPython with set O and whitespace written directly, lone surrogates encoded as their units
func encodeUTF7(text string) string {
	runes := textRunes(text)
	direct := func(ch rune) bool { return ch < 128 && ch > 0 && utf7Category[ch] != 3 }
	var out []byte
	inShift := false
	var base64bits uint
	var base64buffer uint64
	for _, ch := range runes {
		if inShift {
			if direct(ch) {
				if base64bits > 0 {
					out = append(out, base64Alphabet[(base64buffer<<(6-base64bits))&0x3f])
					base64buffer, base64bits = 0, 0
				}
				inShift = false
				if (ch < 128 && isBase64Byte(byte(ch))) || ch == '-' {
					out = append(out, '-')
				}
				out = append(out, byte(ch))
				continue
			}
		} else {
			switch {
			case ch == '+':
				out = append(out, '+', '-')
				continue
			case direct(ch):
				out = append(out, byte(ch))
				continue
			}
			out = append(out, '+')
			inShift = true
		}
		if ch >= 0x10000 {
			base64bits += 16
			base64buffer = base64buffer<<16 | uint64(0xd800+((ch-0x10000)>>10))
			for base64bits >= 6 {
				out = append(out, base64Alphabet[(base64buffer>>(base64bits-6))&0x3f])
				base64bits -= 6
			}
			ch = 0xdc00 + ((ch - 0x10000) & 0x3ff)
		}
		base64bits += 16
		base64buffer = base64buffer<<16 | uint64(ch)
		for base64bits >= 6 {
			out = append(out, base64Alphabet[(base64buffer>>(base64bits-6))&0x3f])
			base64bits -= 6
		}
	}
	if base64bits > 0 {
		out = append(out, base64Alphabet[(base64buffer<<(6-base64bits))&0x3f])
	}
	if inShift {
		out = append(out, '-')
	}
	return string(out)
}
