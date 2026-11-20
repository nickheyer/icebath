package pycodecs

import (
	"bytes"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// Punycode threshold for digit j at a bias, clamped to 1 through 26
func punyT(j int64, bias *big.Int) *big.Int {
	res := new(big.Int).Sub(big.NewInt(36*(j+1)), bias)
	if res.Cmp(big.NewInt(1)) < 0 {
		return big.NewInt(1)
	}
	if res.Cmp(big.NewInt(26)) > 0 {
		return big.NewInt(26)
	}
	return res
}

// Bias adaptation of RFC 3492 section 3.4 over Python integers
func punyAdapt(delta *big.Int, first bool, numchars int) *big.Int {
	d := new(big.Int).Set(delta)
	if first {
		d.Quo(d, big.NewInt(700))
	} else {
		d.Quo(d, big.NewInt(2))
	}
	d.Add(d, new(big.Int).Quo(d, big.NewInt(int64(numchars))))
	divisions := int64(0)
	for d.Cmp(big.NewInt(455)) > 0 {
		d.Quo(d, big.NewInt(35))
		divisions += 36
	}
	bias := new(big.Int).Mul(big.NewInt(36), d)
	bias.Quo(bias, new(big.Int).Add(d, big.NewInt(38)))
	return bias.Add(bias, big.NewInt(divisions))
}

// Punycode failure positioned in the extended part, the offset added by the caller
type punyError struct {
	start, end *big.Int
	reason     string
}

// Generalized variable length integer at extpos, nil delta for a malformed one outside strict mode
func punyDecodeNumber(extended []byte, extpos int, bias *big.Int, strict bool) (int, *big.Int, *punyError) {
	result := big.NewInt(0)
	w := big.NewInt(1)
	j := int64(0)
	for {
		if extpos >= len(extended) {
			if strict {
				return 0, nil, &punyError{big.NewInt(int64(extpos)), big.NewInt(int64(extpos + 1)), "incomplete punycode string"}
			}
			return extpos + 1, nil, nil
		}
		char := extended[extpos]
		extpos++
		var digit int64
		switch {
		case char >= 0x41 && char <= 0x5a:
			digit = int64(char - 0x41)
		case char >= 0x30 && char <= 0x39:
			digit = int64(char) - 22
		case strict:
			return 0, nil, &punyError{big.NewInt(int64(extpos - 1)), big.NewInt(int64(extpos)),
				fmt.Sprintf("Invalid extended code point '%d'", extended[extpos-1])}
		default:
			return extpos, nil, nil
		}
		t := punyT(j, bias)
		result.Add(result, new(big.Int).Mul(big.NewInt(digit), w))
		if big.NewInt(digit).Cmp(t) < 0 {
			return extpos, result, nil
		}
		w.Mul(w, new(big.Int).Sub(big.NewInt(36), t))
		j++
	}
}

// Insertion sort decoding of RFC 3492 section 3.2 into the base code points
func punyInsertionSort(base []rune, extended []byte, strict bool) ([]rune, *punyError) {
	char := big.NewInt(0x80)
	pos := big.NewInt(-1)
	bias := big.NewInt(72)
	extpos := 0
	for extpos < len(extended) {
		newpos, delta, perr := punyDecodeNumber(extended, extpos, bias, strict)
		if perr != nil {
			return nil, perr
		}
		if delta == nil {
			return base, nil
		}
		pos.Add(pos, new(big.Int).Add(delta, big.NewInt(1)))
		size := big.NewInt(int64(len(base) + 1))
		char.Add(char, new(big.Int).Div(pos, size))
		if char.Cmp(big.NewInt(0x10ffff)) > 0 {
			if strict {
				return nil, &punyError{new(big.Int).Sub(pos, big.NewInt(1)), new(big.Int).Set(pos), fmt.Sprintf("Invalid character U+%x", char)}
			}
			char = big.NewInt('?')
		}
		pos.Mod(pos, size)
		at := int(pos.Int64())
		base = append(base[:at], append([]rune{rune(char.Int64())}, base[at:]...)...)
		bias = punyAdapt(delta, extpos == 0, len(base))
		extpos = newpos
	}
	return base, nil
}

// bytes.upper of ASCII letters
func asciiUpper(b []byte) []byte {
	out := append([]byte(nil), b...)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - 32
		}
	}
	return out
}

// Punycode decode of encodings.punycode, strict raising or replace mode keeping what decoded and writing U+FFFD for bad base bytes
func decodePunycode(text []byte, strict bool) (string, error) {
	out, err := punyDecodeRunes(text, strict)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range out {
		writeCodePoint(&b, r)
	}
	return b.String(), nil
}

// Code points punycode_decode yields, the UnicodeDecodeError of the ascii base or the extended part
func punyDecodeRunes(text []byte, strict bool) ([]rune, *UnicodeDecodeError) {
	pos := bytes.LastIndexByte(text, '-')
	var base []rune
	var extended []byte
	if pos == -1 {
		extended = asciiUpper(text)
	} else {
		for i, c := range text[:pos] {
			if c >= 0x80 {
				if strict {
					return nil, decodeErr("ascii", text, i, i+1, "ordinal not in range(128)")
				}
				base = append(base, 0xfffd)
				continue
			}
			base = append(base, rune(c))
		}
		extended = asciiUpper(text[pos+1:])
	}
	out, perr := punyInsertionSort(base, extended, strict)
	if perr != nil {
		offset := big.NewInt(int64(pos + 1))
		return nil, punyDecodeError("punycode", text, new(big.Int).Add(offset, perr.start), new(big.Int).Add(offset, perr.end), perr.reason)
	}
	return out, nil
}

// UnicodeDecodeError over Python integer positions, the big fields carrying positions past the int range
func punyDecodeError(codec string, obj []byte, start, end *big.Int, reason string) *UnicodeDecodeError {
	if start.IsInt64() && end.IsInt64() && start.Int64() > -1<<40 && end.Int64() < 1<<40 {
		return decodeErr(codec, obj, int(start.Int64()), int(end.Int64()), reason)
	}
	return &UnicodeDecodeError{Codec: codec, Reason: reason, Len: len(obj), BigPos: start, BigEnd: end}
}

// Generalized variable length integer of RFC 3492 section 3.3
func punyEncodeNumber(n int64, bias int64) []byte {
	const digits = "abcdefghijklmnopqrstuvwxyz0123456789"
	var out []byte
	for j := int64(0); ; j++ {
		t := punyT(j, big.NewInt(bias)).Int64()
		if n < t {
			return append(out, digits[n])
		}
		out = append(out, digits[t+(n-t)%(36-t)])
		n = (n - t) / (36 - t)
	}
}

// Punycode encode of encodings.punycode over code points
func encodePunycode(text string) string {
	runes := textRunes(text)
	var base []byte
	seen := map[rune]bool{}
	var extended []rune
	for _, r := range runes {
		if r < 128 {
			base = append(base, byte(r))
		} else if !seen[r] {
			seen[r] = true
			extended = append(extended, r)
		}
	}
	sort.Slice(extended, func(i, j int) bool { return extended[i] < extended[j] })
	var deltas []int64
	oldchar := int64(0x80)
	oldindex := int64(-1)
	for _, c := range extended {
		index, pos := int64(-1), -1
		char := int64(c)
		curlen := int64(0)
		for _, r := range runes {
			if int64(r) < char {
				curlen++
			}
		}
		delta := (curlen + 1) * (char - oldchar)
		for {
			index, pos = punySelectiveFind(runes, c, index, pos)
			if index == -1 {
				break
			}
			delta += index - oldindex
			deltas = append(deltas, delta-1)
			oldindex = index
			delta = 0
		}
		oldchar = char
	}
	var ext []byte
	bias := int64(72)
	for points, delta := range deltas {
		ext = append(ext, punyEncodeNumber(delta, bias)...)
		bias = punyAdapt(big.NewInt(delta), points == 0, len(base)+points+1).Int64()
	}
	if len(base) > 0 {
		return string(base) + "-" + string(ext)
	}
	return string(ext)
}

// Next occurrence of char after pos, its index among characters up to char, both minus one when none remains
func punySelectiveFind(runes []rune, char rune, index int64, pos int) (int64, int) {
	for {
		pos++
		if pos == len(runes) {
			return -1, -1
		}
		c := runes[pos]
		if c == char {
			return index + 1, pos
		}
		if c < char {
			index++
		}
	}
}
