package pycodecs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// LookupError is the codecs.lookup failure for an unknown name, or a bytes codec refused by the NotText operation
type LookupError struct {
	Name    string
	NotText string
}

func (e *LookupError) Error() string {
	if e.NotText != "" {
		return fmt.Sprintf("'%s' is not a text encoding; use codecs.%s() to handle arbitrary codecs", e.Name, e.NotText)
	}
	return "unknown encoding: " + e.Name
}

// CodecInfo name codecs.lookup gives for a name, false when no codec answers it
func InfoName(name string) (string, bool) {
	mod, ok := Lookup(name)
	if !ok {
		return "", false
	}
	return codecInfoNames[mod], true
}

// UnicodeDecodeError is the strict decode failure over bytes Pos to End of input Len long, End zero meaning one
type UnicodeDecodeError struct {
	Codec  string
	Byte   byte
	Pos    int
	Reason string
	End    int
	Len    int
	// Positions past the int range a punycode error can carry, used instead of Pos and End when set
	BigPos, BigEnd *big.Int
}

func (e *UnicodeDecodeError) Error() string {
	if e.BigPos != nil {
		return fmt.Sprintf("'%s' codec can't decode bytes in position %s-%s: %s", e.Codec, e.BigPos, new(big.Int).Sub(e.BigEnd, big.NewInt(1)), e.Reason)
	}
	if e.End == 0 || (e.Pos >= 0 && e.Pos < e.Len && e.End <= e.Len && e.End == e.Pos+1) {
		return fmt.Sprintf("'%s' codec can't decode byte 0x%02x in position %d: %s", e.Codec, e.Byte, e.Pos, e.Reason)
	}
	return fmt.Sprintf("'%s' codec can't decode bytes in position %d-%d: %s", e.Codec, e.Pos, e.End-1, e.Reason)
}

// Decode failure over start to end of the input, naming the byte at start when the range is one byte
func decodeErr(codec string, data []byte, start, end int, reason string) *UnicodeDecodeError {
	e := &UnicodeDecodeError{Codec: codec, Pos: start, Reason: reason, End: end, Len: len(data)}
	if start >= 0 && start < len(data) {
		e.Byte = data[start]
	}
	return e
}

// UnicodeError of a codec refusing an operation outright
type UnicodeError struct{ Msg string }

func (e *UnicodeError) Error() string      { return e.Msg }
func (e *UnicodeError) PyTypeName() string { return "UnicodeError" }

// RuntimeError a CJK codec raises for a state its tables cannot serve
type codecRuntimeError struct{ msg string }

func (e *codecRuntimeError) Error() string      { return e.msg }
func (e *codecRuntimeError) PyTypeName() string { return "RuntimeError" }

// Codecs ported from CPython's algorithms or probed tables rather than served by a charmap or x/text
var portedModules = map[string]bool{
	"utf_7": true, "unicode_escape": true, "raw_unicode_escape": true, "idna": true, "punycode": true, "charmap": true,
	"undefined": true, "hz": true,
}

var multiByte = map[string]encoding.Encoding{
	"euc_jp": japanese.EUCJP, "shift_jis": japanese.ShiftJIS, "cp932": japanese.ShiftJIS,
	"euc_kr": korean.EUCKR, "cp949": korean.EUCKR, "gb2312": simplifiedchinese.GBK, "gbk": simplifiedchinese.GBK,
	"gb18030": simplifiedchinese.GB18030, "big5": traditionalchinese.Big5,
	"cp950": traditionalchinese.Big5, "big5hkscs": traditionalchinese.Big5,
}

// Lowercased ASCII alnum and dots with each punctuation run joined by one underscore, as codecs.lookup normalizes
func normalize(name string) string {
	var b strings.Builder
	punct := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' {
			if punct && b.Len() > 0 {
				b.WriteByte('_')
			}
			punct = false
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			b.WriteByte(c)
		} else {
			punct = true
		}
	}
	return b.String()
}

// Module of the encodings package that answers name, alias first like encodings.search_function
func Lookup(name string) (string, bool) {
	norm := normalize(name)
	var candidates []string
	if a, ok := codecAliases[norm]; ok {
		candidates = append(candidates, a)
	} else if a, ok := codecAliases[strings.ReplaceAll(norm, ".", "_")]; ok {
		candidates = append(candidates, a)
	}
	candidates = append(candidates, norm)
	for _, mod := range candidates {
		if mod == "" || strings.Contains(mod, ".") {
			continue
		}
		if knownModule(mod) {
			return mod, true
		}
	}
	return "", false
}

func knownModule(mod string) bool {
	if _, ok := charmapTables[mod]; ok {
		return true
	}
	if _, ok := multiByte[mod]; ok {
		return true
	}
	switch mod {
	case "utf_8", "utf_8_sig", "ascii", "latin_1", "utf_16", "utf_16_le", "utf_16_be", "utf_32", "utf_32_le", "utf_32_be":
		return true
	case "mbcs", "oem", "cp65001":
		return platformCodePage(mod) != ""
	}
	return portedModules[mod] || isoVariants[mod] != nil || statelessModules[mod] || nonTextModules[mod]
}

// bytes.decode with errors strict through the CPython codec registry
func Decode(data []byte, name string) (string, error) {
	mod, ok := Lookup(name)
	if !ok {
		return "", &LookupError{Name: name}
	}
	if cp := platformCodePage(mod); cp != "" && (mod == "mbcs" || mod == "oem") {
		mod, _ = Lookup(cp)
	}
	switch mod {
	case "utf_8", "cp65001":
		return decodeUTF8(data, "utf-8")
	case "utf_8_sig":
		return decodeUTF8(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), "utf-8")
	case "ascii":
		for i, c := range data {
			if c >= 0x80 {
				return "", &UnicodeDecodeError{Codec: "ascii", Byte: c, Pos: i, Reason: "ordinal not in range(128)"}
			}
		}
		return string(data), nil
	case "latin_1", "charmap":
		rs := make([]rune, len(data))
		for i, c := range data {
			rs[i] = rune(c)
		}
		return string(rs), nil
	case "utf_16", "utf_16_le", "utf_16_be":
		return decodeUTF16(data, mod)
	case "utf_32", "utf_32_le", "utf_32_be":
		return decodeUTF32(data, mod)
	case "utf_7":
		return decodeUTF7(data, false)
	case "unicode_escape":
		return decodeUnicodeEscape(data, false)
	case "raw_unicode_escape":
		return decodeRawUnicodeEscape(data, false)
	case "punycode":
		return decodePunycode(data, true)
	case "idna":
		return decodeIDNA(data)
	case "undefined":
		return "", &UnicodeError{Msg: "undefined encoding"}
	case "hz":
		return decodeHZ(data, false)
	}
	if v := isoVariants[mod]; v != nil {
		return v.decode(data, false)
	}
	if statelessModules[mod] {
		return loadStateless(mod).decode(data, false)
	}
	if nonTextModules[mod] {
		return "", &LookupError{Name: name, NotText: "decode"}
	}
	if table, ok := charmapTables[mod]; ok {
		rs := []rune(table)
		var b strings.Builder
		for i, c := range data {
			if rs[c] == 0xfffe {
				return "", &UnicodeDecodeError{Codec: "charmap", Byte: c, Pos: i, Reason: "character maps to <undefined>"}
			}
			b.WriteRune(rs[c])
		}
		return b.String(), nil
	}
	if enc, ok := multiByte[mod]; ok {
		return decodeMultiByte(enc, data, mod)
	}
	panic("pycodecs: module " + mod + " is known but has no decoder")
}

// Strict UTF-8 with the CPython reason for the first bad sequence at its start byte
func decodeUTF8(data []byte, codec string) (string, error) {
	for i := 0; i < len(data); {
		r, n := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || n > 1 {
			i += n
			continue
		}
		c := data[i]
		if c < 0xc2 || c > 0xf4 {
			return "", &UnicodeDecodeError{Codec: codec, Byte: c, Pos: i, Reason: "invalid start byte"}
		}
		need := 2
		if c >= 0xf0 {
			need = 4
		} else if c >= 0xe0 {
			need = 3
		}
		reason := "unexpected end of data"
		for k := 1; k < need && i+k < len(data); k++ {
			lo, hi := byte(0x80), byte(0xbf)
			if k == 1 {
				switch c {
				case 0xe0:
					lo = 0xa0
				case 0xed:
					hi = 0x9f
				case 0xf0:
					lo = 0x90
				case 0xf4:
					hi = 0x8f
				}
			}
			if b := data[i+k]; b < lo || b > hi {
				reason = "invalid continuation byte"
				break
			}
		}
		return "", &UnicodeDecodeError{Codec: codec, Byte: c, Pos: i, Reason: reason}
	}
	return string(data), nil
}

func decodeUTF16(data []byte, mod string) (string, error) {
	order := binary.ByteOrder(binary.LittleEndian)
	switch {
	case mod == "utf_16_be":
		order = binary.BigEndian
	case mod == "utf_16" && bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		order, data = binary.BigEndian, data[2:]
	case mod == "utf_16" && bytes.HasPrefix(data, []byte{0xff, 0xfe}):
		data = data[2:]
	}
	codec := strings.ReplaceAll(mod, "_", "-")
	if len(data)%2 != 0 {
		return "", &UnicodeDecodeError{Codec: codec, Byte: data[len(data)-1], Pos: len(data) - 1, Reason: "truncated data"}
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = order.Uint16(data[2*i:])
	}
	for i := 0; i < len(units); i++ {
		u := units[i]
		if u >= 0xd800 && u < 0xdc00 && i+1 < len(units) && units[i+1] >= 0xdc00 && units[i+1] < 0xe000 {
			i++
			continue
		}
		if u >= 0xd800 && u < 0xe000 {
			reason := "illegal encoding"
			if u < 0xdc00 {
				reason = "unexpected end of data"
				if i+1 < len(units) {
					reason = "illegal UTF-16 surrogate"
				}
			}
			return "", &UnicodeDecodeError{Codec: codec, Byte: data[2*i], Pos: 2 * i, Reason: reason}
		}
	}
	return string(utf16.Decode(units)), nil
}

func decodeUTF32(data []byte, mod string) (string, error) {
	order := binary.ByteOrder(binary.LittleEndian)
	switch {
	case mod == "utf_32_be":
		order = binary.BigEndian
	case mod == "utf_32" && bytes.HasPrefix(data, []byte{0, 0, 0xfe, 0xff}):
		order, data = binary.BigEndian, data[4:]
	case mod == "utf_32" && bytes.HasPrefix(data, []byte{0xff, 0xfe, 0, 0}):
		data = data[4:]
	}
	codec := strings.ReplaceAll(mod, "_", "-")
	var b strings.Builder
	for i := 0; i < len(data); i += 4 {
		if i+4 > len(data) {
			return "", &UnicodeDecodeError{Codec: codec, Byte: data[i], Pos: i, Reason: "truncated data"}
		}
		r := order.Uint32(data[i:])
		if r > 0x10ffff || r >= 0xd800 && r < 0xe000 {
			reason := "code point not in range(0x110000)"
			if r >= 0xd800 && r < 0xe000 {
				reason = "code point in surrogate code point range(0xd800, 0xe000)"
			}
			return "", &UnicodeDecodeError{Codec: codec, Byte: data[i], Pos: i, Reason: reason}
		}
		b.WriteRune(rune(r))
	}
	return b.String(), nil
}

// Strict multibyte decode, a replacement character the input never encoded marks the first illegal sequence
func decodeMultiByte(enc encoding.Encoding, data []byte, mod string) (string, error) {
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		return "", &UnicodeDecodeError{Codec: mod, Byte: data[0], Pos: 0, Reason: "illegal multibyte sequence"}
	}
	pos := 0
	for _, r := range string(out) {
		want, encErr := enc.NewEncoder().Bytes(utf8.AppendRune(nil, r))
		if encErr != nil || !bytes.HasPrefix(data[pos:], want) {
			if pos >= len(data) {
				pos = len(data) - 1
			}
			return "", &UnicodeDecodeError{Codec: mod, Byte: data[pos], Pos: pos, Reason: "illegal multibyte sequence"}
		}
		pos += len(want)
	}
	return string(out), nil
}
