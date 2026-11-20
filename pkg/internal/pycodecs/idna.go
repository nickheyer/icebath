package pycodecs

import (
	"bytes"
	"fmt"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// IDNA section 3.1 label separators
func isIDNADot(r rune) bool { return r == 0x2e || r == 0x3002 || r == 0xff0e || r == 0xff61 }

func allASCIIRunes(rs []rune) bool {
	for _, r := range rs {
		if r >= 0x80 {
			return false
		}
	}
	return true
}

func asciiLower(b []byte) []byte {
	out := append([]byte(nil), b...)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + 32
		}
	}
	return out
}

// Text of code points with lone surrogates in generalized UTF-8
func runesText(rs []rune) string {
	var b strings.Builder
	for _, r := range rs {
		writeCodePoint(&b, r)
	}
	return b.String()
}

// ToASCII of RFC 3490 for one label, a failure positioned in the label or in its nameprep result
func idnaToASCII(label []rune) ([]byte, *idnaFail) {
	asciiResult := func(rs []rune) ([]byte, *idnaFail) {
		if len(rs) > 0 && len(rs) < 64 {
			return []byte(string(rs)), nil
		}
		if len(rs) == 0 {
			return nil, &idnaFail{0, 1, "label empty"}
		}
		return nil, &idnaFail{0, len(rs), "label too long"}
	}
	if allASCIIRunes(label) {
		return asciiResult(label)
	}
	label, fail := nameprep(label)
	if fail != nil {
		return nil, fail
	}
	if allASCIIRunes(label) {
		return asciiResult(label)
	}
	if strings.HasPrefix(pystr.Lower(runesText(label)), "xn--") {
		return nil, &idnaFail{0, 4, "Label starts with ACE prefix"}
	}
	out := []byte("xn--" + encodePunycode(runesText(label)))
	if len(out) < 64 {
		return out, nil
	}
	return nil, &idnaFail{0, len(label), "label too long"}
}

// idna encode of encodings.idna, the ASCII fast path or each dot separated label through ToASCII
func encodeIDNA(text string) (string, error) {
	runes := textRunes(text)
	if len(runes) == 0 {
		return "", nil
	}
	if allASCIIRunes(runes) {
		labels := strings.Split(string(runes), ".")
		offset := func(i int) int {
			n := i
			for _, l := range labels[:i] {
				n += len(l)
			}
			return n
		}
		for i, label := range labels[:len(labels)-1] {
			if label == "" {
				return "", encodeErr("idna", runes, offset(i), offset(i)+1, "label empty")
			}
		}
		for i, label := range labels {
			if len(label) >= 64 {
				return "", encodeErr("idna", runes, offset(i), offset(i)+len(label), "label too long")
			}
		}
		return string(runes), nil
	}
	labels := [][]rune{{}}
	for _, r := range runes {
		if isIDNADot(r) {
			labels = append(labels, []rune{})
			continue
		}
		labels[len(labels)-1] = append(labels[len(labels)-1], r)
	}
	trailing := ""
	if len(labels[len(labels)-1]) == 0 {
		trailing = "."
		labels = labels[:len(labels)-1]
	}
	var result []byte
	for i, label := range labels {
		if len(result) > 0 {
			result = append(result, '.')
		}
		out, fail := idnaToASCII(label)
		if fail != nil {
			offset := i
			for _, l := range labels[:i] {
				offset += len(l)
			}
			return "", encodeErr("idna", runes, offset+fail.start, offset+fail.end, fail.reason)
		}
		result = append(result, out...)
	}
	return string(result) + trailing, nil
}

// Failure of ToUnicode over Python integer positions in the label
type idnaDecodeFail struct {
	start, end *big.Int
	reason     string
}

func bigFail(start, end int, reason string) *idnaDecodeFail {
	return &idnaDecodeFail{big.NewInt(int64(start)), big.NewInt(int64(end)), reason}
}

// ToUnicode of RFC 3490 for one label, punycode decoded and verified to round trip through ToASCII
func idnaToUnicode(label []byte) ([]rune, *idnaDecodeFail) {
	if len(label) > 1024 {
		return nil, bigFail(0, len(label), "label way too long")
	}
	if !bytes.HasPrefix(asciiLower(label), []byte("xn--")) {
		for i, c := range label {
			if c >= 0x80 {
				return nil, bigFail(i, i+1, "ordinal not in range(128)")
			}
		}
		return []rune(string(label)), nil
	}
	result, err := punyDecodeRunes(label[4:], true)
	if err != nil {
		start, end := err.BigPos, err.BigEnd
		if start == nil {
			start, end = big.NewInt(int64(err.Pos)), big.NewInt(int64(err.End))
		}
		four := big.NewInt(4)
		return nil, &idnaDecodeFail{new(big.Int).Add(four, start), new(big.Int).Add(four, end), err.Reason}
	}
	label2, fail := idnaToASCII(result)
	if fail != nil {
		return nil, bigFail(fail.start, fail.end, fail.reason)
	}
	if !bytes.Equal(asciiLower(label), label2) {
		return nil, bigFail(0, len(label), fmt.Sprintf("IDNA does not round-trip, '%s' != '%s'", pyrepr.BytesRepr(label), pyrepr.BytesRepr(label2)))
	}
	return result, nil
}

// idna decode of encodings.idna, the ASCII fast path or each dot separated label through ToUnicode
func decodeIDNA(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if !bytes.Contains(asciiLower(data), []byte("xn--")) {
		ascii := true
		for _, c := range data {
			if c >= 0x80 {
				ascii = false
				break
			}
		}
		if ascii {
			return string(data), nil
		}
	}
	labels := bytes.Split(data, []byte("."))
	trailing := ""
	if len(labels[len(labels)-1]) == 0 {
		trailing = "."
		labels = labels[:len(labels)-1]
	}
	var parts []string
	for i, label := range labels {
		u, fail := idnaToUnicode(label)
		if fail != nil {
			offset := int64(i)
			for _, l := range labels[:i] {
				offset += int64(len(l))
			}
			off := big.NewInt(offset)
			return "", punyDecodeError("idna", data, new(big.Int).Add(off, fail.start), new(big.Int).Add(off, fail.end), fail.reason)
		}
		parts = append(parts, runesText(u))
	}
	return strings.Join(parts, ".") + trailing, nil
}
