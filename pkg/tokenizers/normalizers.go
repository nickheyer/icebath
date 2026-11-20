package tokenizers

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/text/unicode/norm"
)

type normalizer func(text) (text, error)

// The type name of a tokenizer.json component and its fields
func component(raw json.RawMessage) (string, map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", nil, err
	}
	var kind string
	if err := json.Unmarshal(fields["type"], &kind); err != nil {
		return "", nil, fmt.Errorf("tokenizers: a component without a type: %s", truncate(raw))
	}
	return kind, fields, nil
}

func truncate(raw []byte) string {
	if len(raw) > 80 {
		return string(raw[:80]) + "..."
	}
	return string(raw)
}

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

// A field decoded into v, left untouched when the field is absent or null
func field(fields map[string]json.RawMessage, name string, v any) error {
	if isNull(fields[name]) {
		return nil
	}
	if err := json.Unmarshal(fields[name], v); err != nil {
		return fmt.Errorf("tokenizers: field %s: %w", name, err)
	}
	return nil
}

// A pattern field, a literal under String or a regular expression under Regex
func patternField(raw json.RawMessage) (pattern, error) {
	var p struct {
		String *string
		Regex  *string
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("tokenizers: pattern %s: %w", truncate(raw), err)
	}
	switch {
	case p.Regex != nil:
		return compileRegex(*p.Regex)
	case p.String != nil:
		return literalPattern(*p.String), nil
	}
	return nil, fmt.Errorf("tokenizers: pattern %s is neither String nor Regex", truncate(raw))
}

func sequenceOf[T any](raw json.RawMessage, parse func(json.RawMessage) (T, error)) ([]T, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := make([]T, 0, len(items))
	for _, item := range items {
		part, err := parse(item)
		if err != nil {
			return nil, err
		}
		out = append(out, part)
	}
	return out, nil
}

func isChinese(r rune) bool {
	return r >= 0x4E00 && r <= 0x9FFF || r >= 0x3400 && r <= 0x4DBF || r >= 0x20000 && r <= 0x2A6DF || r >= 0x2A700 && r <= 0x2B73F ||
		r >= 0x2B740 && r <= 0x2B81F || r >= 0x2B920 && r <= 0x2CEAF || r >= 0xF900 && r <= 0xFAFF || r >= 0x2F800 && r <= 0x2FA1F
}

// Control, format and private use runes other than tab, line feed and carriage return
func isControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r)
}

func bertNormalizer(clean, chinese, strip, lowercase bool) normalizer {
	return func(t text) (text, error) {
		if clean {
			t = t.filter(func(r rune) bool { return r != 0 && r != 0xFFFD && !isControl(r) }).mapRunes(func(r rune) rune {
				if isSpace(r) {
					return ' '
				}
				return r
			})
		}
		if chinese {
			w := rewrite{src: t}
			for _, r := range t.s {
				if isChinese(r) {
					w.put(1, " "+string(r)+" ", false)
				} else {
					w.put(1, string(r), false)
				}
			}
			t = w.done()
		}
		if strip {
			t = t.normalForm(norm.NFD).filter(func(r rune) bool { return !unicode.Is(unicode.Mn, r) })
		}
		if lowercase {
			t = t.lower()
		}
		return t, nil
	}
}

// Text without the white space at its left or right end
func stripSpace(t text, left, right bool) text {
	a, b := 0, len(t.s)
	lead := 0
	if left {
		for a < b {
			r, w := utf8.DecodeRuneInString(t.s[a:])
			if !isSpace(r) {
				break
			}
			a += w
			lead++
		}
	}
	if right {
		for b > a {
			r, w := utf8.DecodeLastRuneInString(t.s[:b])
			if !isSpace(r) {
				break
			}
			b -= w
		}
	}
	return text{s: t.s[a:b], origin: t.origin, zero: max(t.zero-lead, 0)}
}

func nmt(t text) (text, error) {
	t = t.filter(func(r rune) bool {
		return !(r >= 1 && r <= 8 || r == 0xB || r >= 0xE && r <= 0x1F || r == 0x7F || r == 0x8F || r == 0x9F)
	})
	return t.mapRunes(func(r rune) rune {
		switch {
		case r == 0x9, r == 0xA, r == 0xC, r == 0xD, r == 0x1680, r >= 0x200B && r <= 0x200F, r == 0x2028, r == 0x2029, r == 0x2581, r == 0xFEFF, r == 0xFFFD:
			return ' '
		}
		return r
	}), nil
}

// charsMap is a SentencePiece precompiled character map, a double array trie over replacement strings
type charsMap struct {
	trie []uint32
	text []byte
}

func newCharsMap(blob []byte) (*charsMap, error) {
	if len(blob) < 4 {
		return nil, fmt.Errorf("tokenizers: precompiled character map of %d bytes", len(blob))
	}
	size := int(binary.LittleEndian.Uint32(blob))
	if size%4 != 0 || 4+size > len(blob) {
		return nil, fmt.Errorf("tokenizers: precompiled character map declares a trie of %d bytes in %d", size, len(blob))
	}
	m := &charsMap{trie: make([]uint32, size/4), text: blob[4+size:]}
	for i := range m.trie {
		m.trie[i] = binary.LittleEndian.Uint32(blob[4+4*i:])
	}
	return m, nil
}

// Replacement of the shortest map key that begins the chunk
func (m *charsMap) transform(chunk string) (string, bool) {
	if len(m.trie) == 0 {
		return "", false
	}
	offset := func(u uint32) uint32 { return (u >> 10) << ((u & (1 << 9)) >> 6) }
	pos := offset(m.trie[0])
	for i := 0; i < len(chunk); i++ {
		c := uint32(chunk[i])
		pos ^= c
		if int(pos) >= len(m.trie) {
			return "", false
		}
		unit := m.trie[pos]
		if unit&(1<<31|0xFF) != c {
			return "", false
		}
		pos ^= offset(unit)
		if (unit>>8)&1 == 1 && int(pos) < len(m.trie) {
			start := int(m.trie[pos] & (1<<31 - 1))
			if start > len(m.text) {
				return "", false
			}
			end := start
			for end < len(m.text) && m.text[end] != 0 {
				end++
			}
			return string(m.text[start:end]), true
		}
	}
	return "", false
}

// Applies the map to each grapheme cluster under six bytes, else to each of its runes
func (m *charsMap) normalize(t text) (text, error) {
	w := rewrite{src: t}
	rest, state := t.s, -1
	for rest != "" {
		var cluster string
		cluster, rest, _, state = uniseg.FirstGraphemeClusterInString(rest, state)
		if len(cluster) < 6 {
			if repl, ok := m.transform(cluster); ok {
				w.put(utf8.RuneCountInString(cluster), repl, false)
				continue
			}
		}
		for _, r := range cluster {
			if repl, ok := m.transform(string(r)); ok {
				w.put(1, repl, false)
			} else {
				w.put(1, string(r), false)
			}
		}
	}
	return w.done(), nil
}

func parseNormalizer(raw json.RawMessage) (normalizer, error) {
	if isNull(raw) {
		return nil, nil
	}
	kind, f, err := component(raw)
	if err != nil {
		return nil, err
	}
	form := func(nf norm.Form) (normalizer, error) {
		return func(t text) (text, error) { return t.normalForm(nf), nil }, nil
	}
	switch kind {
	case "BertNormalizer":
		clean, chinese, lowercase := true, true, true
		var strip *bool
		if err := firstErr(field(f, "clean_text", &clean), field(f, "handle_chinese_chars", &chinese), field(f, "lowercase", &lowercase), field(f, "strip_accents", &strip)); err != nil {
			return nil, err
		}
		if strip == nil {
			strip = &lowercase
		}
		return bertNormalizer(clean, chinese, *strip, lowercase), nil
	case "Strip":
		left, right := true, true
		if err := firstErr(field(f, "strip_left", &left), field(f, "strip_right", &right)); err != nil {
			return nil, err
		}
		return func(t text) (text, error) { return stripSpace(t, left, right), nil }, nil
	case "StripAccents":
		return func(t text) (text, error) {
			return t.filter(func(r rune) bool { return !unicode.Is(unicode.M, r) }), nil
		}, nil
	case "NFC":
		return form(norm.NFC)
	case "NFD":
		return form(norm.NFD)
	case "NFKC":
		return form(norm.NFKC)
	case "NFKD":
		return form(norm.NFKD)
	case "Lowercase":
		return func(t text) (text, error) { return t.lower(), nil }, nil
	case "Nmt":
		return nmt, nil
	case "ByteLevel":
		return func(t text) (text, error) { return t.byteLevel(), nil }, nil
	case "Prepend":
		var prefix string
		if err := field(f, "prepend", &prefix); err != nil {
			return nil, err
		}
		return func(t text) (text, error) { return t.prepend(prefix), nil }, nil
	case "Replace":
		var content string
		if err := field(f, "content", &content); err != nil {
			return nil, err
		}
		p, err := patternField(f["pattern"])
		if err != nil {
			return nil, err
		}
		return func(t text) (text, error) { return t.replace(p, content) }, nil
	case "Precompiled":
		var encoded string
		if err := field(f, "precompiled_charsmap", &encoded); err != nil {
			return nil, err
		}
		blob, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("tokenizers: precompiled character map: %w", err)
		}
		m, err := newCharsMap(blob)
		if err != nil {
			return nil, err
		}
		return m.normalize, nil
	case "Sequence":
		parts, err := sequenceOf(f["normalizers"], parseNormalizer)
		if err != nil {
			return nil, err
		}
		return func(t text) (text, error) {
			var err error
			for _, part := range parts {
				if part == nil {
					continue
				}
				if t, err = part(t); err != nil {
					return t, err
				}
			}
			return t, nil
		}, nil
	}
	return nil, fmt.Errorf("tokenizers: normalizer %q is not one the format defines", kind)
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
