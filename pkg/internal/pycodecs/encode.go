package pycodecs

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

// UnicodeEncodeError is the strict encode failure over characters Start to End, Len the text length when known
type UnicodeEncodeError struct {
	Codec      string
	Char       rune
	Start, End int
	Reason     string
	Len        int
}

// Encode failure over start to end of the characters, the character at start named when the range is that one
func encodeErr(codec string, runes []rune, start, end int, reason string) *UnicodeEncodeError {
	e := &UnicodeEncodeError{Codec: codec, Start: start, End: end, Reason: reason, Len: len(runes)}
	if start >= 0 && start < len(runes) {
		e.Char = runes[start]
	} else {
		e.Len = -1
	}
	return e
}

func (e *UnicodeEncodeError) Error() string {
	if e.End-e.Start != 1 || e.Len < 0 || (e.Len > 0 && e.End > e.Len) {
		return fmt.Sprintf("'%s' codec can't encode characters in position %d-%d: %s", e.Codec, e.Start, e.End-1, e.Reason)
	}
	var char string
	switch {
	case e.Char < 0x100:
		char = fmt.Sprintf(`\x%02x`, e.Char)
	case e.Char < 0x10000:
		char = fmt.Sprintf(`\u%04x`, e.Char)
	default:
		char = fmt.Sprintf(`\U%08x`, e.Char)
	}
	return fmt.Sprintf("'%s' codec can't encode character '%s' in position %d: %s", e.Codec, char, e.Start, e.Reason)
}

// Characters of text with each byte of an invalid sequence as the lone surrogate surrogateescape decodes it to
func textRunes(text string) []rune {
	var out []rune
	for i := 0; i < len(text); {
		r, n := utf8.DecodeRuneInString(text[i:])
		if r == utf8.RuneError && n == 1 {
			r = 0xdc00 + rune(text[i])
		}
		out = append(out, r)
		i += n
	}
	return out
}

func isSurrogate(r rune) bool { return r >= 0xd800 && r < 0xe000 }

// Failure over the run of characters from i the codec cannot encode, as the strict handler reports it
func encodeRun(codec string, runes []rune, i int, bad func(rune) bool, reason string) error {
	end := i + 1
	for end < len(runes) && bad(runes[end]) {
		end++
	}
	return &UnicodeEncodeError{Codec: codec, Char: runes[i], Start: i, End: end, Reason: reason}
}

var charmapReverse sync.Map

// Byte each character of a charmap table encodes to, undefined slots left out
func reverseCharmap(mod string) map[rune]byte {
	if v, ok := charmapReverse.Load(mod); ok {
		return v.(map[rune]byte)
	}
	table := []rune(charmapTables[mod])
	out := make(map[rune]byte, len(table))
	for i := len(table) - 1; i >= 0; i-- {
		if table[i] != 0xfffe {
			out[table[i]] = byte(i)
		}
	}
	charmapReverse.Store(mod, out)
	return out
}

// str.encode with errors strict through the CPython codec registry
func Encode(text, name string) (string, error) {
	mod, ok := Lookup(name)
	if !ok {
		return "", &LookupError{Name: name}
	}
	if cp := platformCodePage(mod); cp != "" && (mod == "mbcs" || mod == "oem") {
		mod, _ = Lookup(cp)
	}
	runes := textRunes(text)
	switch mod {
	case "utf_8", "cp65001", "utf_8_sig":
		for i, r := range runes {
			if isSurrogate(r) {
				return "", encodeRun("utf-8", runes, i, isSurrogate, "surrogates not allowed")
			}
		}
		if mod == "utf_8_sig" {
			return "\xef\xbb\xbf" + text, nil
		}
		return text, nil
	case "ascii", "latin_1", "charmap":
		limit, codec, reason := rune(0x80), "ascii", "ordinal not in range(128)"
		if mod != "ascii" {
			limit, codec, reason = 0x100, "latin-1", "ordinal not in range(256)"
		}
		out := make([]byte, len(runes))
		for i, r := range runes {
			if r >= limit {
				return "", encodeRun(codec, runes, i, func(r rune) bool { return r >= limit }, reason)
			}
			out[i] = byte(r)
		}
		return string(out), nil
	case "utf_16", "utf_16_le", "utf_16_be", "utf_32", "utf_32_le", "utf_32_be":
		return encodeWide(runes, mod)
	case "utf_7":
		return encodeUTF7(text), nil
	case "unicode_escape":
		return encodeUnicodeEscape(text), nil
	case "raw_unicode_escape":
		return encodeRawUnicodeEscape(text), nil
	case "punycode":
		return encodePunycode(text), nil
	case "idna":
		return encodeIDNA(text)
	case "undefined":
		return "", &UnicodeError{Msg: "undefined encoding"}
	case "hz":
		return encodeHZ(text)
	}
	if v := isoVariants[mod]; v != nil {
		return v.encode(text)
	}
	if statelessModules[mod] {
		return loadStateless(mod).encode(text)
	}
	if nonTextModules[mod] {
		return "", &LookupError{Name: name, NotText: "encode"}
	}
	if _, ok := charmapTables[mod]; ok {
		reverse := reverseCharmap(mod)
		undefined := func(r rune) bool { _, ok := reverse[r]; return !ok }
		out := make([]byte, len(runes))
		for i, r := range runes {
			if undefined(r) {
				return "", encodeRun("charmap", runes, i, undefined, "character maps to <undefined>")
			}
			out[i] = reverse[r]
		}
		return string(out), nil
	}
	if enc, ok := multiByte[mod]; ok {
		for i, r := range runes {
			if _, err := enc.NewEncoder().String(string(r)); isSurrogate(r) || err != nil {
				return "", &UnicodeEncodeError{Codec: mod, Char: r, Start: i, End: i + 1, Reason: "illegal multibyte sequence"}
			}
		}
		return enc.NewEncoder().String(text)
	}
	panic("pycodecs: module " + mod + " is known but has no encoder")
}

// UTF-16 and UTF-32 in the byte order the module names, the plain forms in native order after a BOM
func encodeWide(runes []rune, mod string) (string, error) {
	order := binary.AppendByteOrder(binary.LittleEndian)
	switch {
	case strings.HasSuffix(mod, "_be"):
		order = binary.BigEndian
	case mod == "utf_16" || mod == "utf_32":
		order = binary.NativeEndian
	}
	codec := strings.ReplaceAll(mod, "_", "-")
	wide := strings.HasPrefix(mod, "utf_32")
	var out []byte
	if mod == "utf_16" {
		out = order.AppendUint16(out, 0xfeff)
	} else if mod == "utf_32" {
		out = order.AppendUint32(out, 0xfeff)
	}
	for i, r := range runes {
		if isSurrogate(r) {
			return "", &UnicodeEncodeError{Codec: codec, Char: r, Start: i, End: i + 1, Reason: "surrogates not allowed"}
		}
		if wide {
			out = order.AppendUint32(out, uint32(r))
			continue
		}
		for _, unit := range utf16.Encode([]rune{r}) {
			out = order.AppendUint16(out, unit)
		}
	}
	return string(out), nil
}
