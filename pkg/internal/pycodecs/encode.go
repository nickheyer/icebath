package pycodecs

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

// UnicodeEncodeError is the strict encode failure over characters Start to End with the CPython message
type UnicodeEncodeError struct {
	Codec      string
	Char       rune
	Start, End int
	Reason     string
}

func (e *UnicodeEncodeError) Error() string {
	if e.End-e.Start != 1 {
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
	return &UnicodeEncodeError{codec, runes[i], i, end, reason}
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
	case "ascii", "latin_1":
		limit, codec, reason := rune(0x80), "ascii", "ordinal not in range(128)"
		if mod == "latin_1" {
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
				return "", &UnicodeEncodeError{mod, r, i, i + 1, "illegal multibyte sequence"}
			}
		}
		return enc.NewEncoder().String(text)
	}
	return "", &UnportedCodecError{Module: mod}
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
			return "", &UnicodeEncodeError{codec, r, i, i + 1, "surrogates not allowed"}
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
