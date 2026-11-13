package tokenizers

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// piece is one split of the input, fixed once it is an added token with its id
type piece struct {
	text
	id    int
	fixed bool
}

type preTokenizer func([]piece) ([]piece, error)

// Every open piece replaced by the non empty texts f cuts it into
func splitPieces(pieces []piece, f func(text) ([]text, error)) ([]piece, error) {
	out := make([]piece, 0, len(pieces))
	for _, p := range pieces {
		if p.fixed {
			out = append(out, p)
			continue
		}
		parts, err := f(p.text)
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			if part.s != "" {
				out = append(out, piece{text: part})
			}
		}
	}
	return out, nil
}

func splitter(p pattern, invert bool, behavior string) preTokenizer {
	return func(pieces []piece) ([]piece, error) {
		return splitPieces(pieces, func(t text) ([]text, error) { return t.split(p, invert, behavior) })
	}
}

func chain(parts []preTokenizer) preTokenizer {
	return func(pieces []piece) ([]piece, error) {
		var err error
		for _, part := range parts {
			if part == nil {
				continue
			}
			if pieces, err = part(pieces); err != nil {
				return nil, err
			}
		}
		return pieces, nil
	}
}

// Runs of word runes and runs of runes that are neither word nor white space
func whitespace(t text) ([]text, error) {
	var out []text
	start, kind := 0, 0
	flush := func(end int) {
		if kind != 0 {
			out = append(out, t.slice(start, end))
		}
	}
	for i, r := range t.s {
		now := 2
		switch {
		case isWord(r):
			now = 1
		case isSpace(r):
			now = 0
		}
		if now != kind {
			flush(i)
			start, kind = i, now
		}
	}
	flush(len(t.s))
	return out, nil
}

const byteLevelSplit = `'s|'t|'re|'ve|'m|'ll|'d| ?\p{L}+| ?\p{N}+| ?[^\s\p{L}\p{N}]+|\s+(?!\S)|\s+`

func byteLevel(prefixSpace, useRegex bool) (preTokenizer, error) {
	re, err := compileRegex(byteLevelSplit)
	if err != nil {
		return nil, err
	}
	return func(pieces []piece) ([]piece, error) {
		return splitPieces(pieces, func(t text) ([]text, error) {
			if prefixSpace && !strings.HasPrefix(t.s, " ") {
				t = t.prepend(" ")
			}
			parts := []text{t}
			if useRegex {
				var err error
				if parts, err = t.split(re, false, "Isolated"); err != nil {
					return nil, err
				}
			}
			for i := range parts {
				parts[i] = parts[i].byteLevel()
			}
			return parts, nil
		})
	}, nil
}

func metaspace(replacement rune, scheme string, split bool) preTokenizer {
	mark := string(replacement)
	return func(pieces []piece) ([]piece, error) {
		return splitPieces(pieces, func(t text) ([]text, error) {
			t = t.mapRunes(func(r rune) rune {
				if r == ' ' {
					return replacement
				}
				return r
			})
			if !strings.HasPrefix(t.s, mark) && (scheme == "always" || scheme == "first" && t.origin) {
				t = t.prepend(mark)
			}
			if !split {
				return []text{t}, nil
			}
			return t.split(runePattern(func(r rune) bool { return r == replacement }), false, "MergedWithNext")
		})
	}
}

// The replacement rune, prepend scheme and split switch of a Metaspace component, older files naming add_prefix_space
func metaspaceFields(f map[string]json.RawMessage) (rune, string, bool, error) {
	replacement, scheme, split := "▁", "always", true
	var prefixSpace *bool
	if err := firstErr(field(f, "replacement", &replacement), field(f, "prepend_scheme", &scheme), field(f, "split", &split), field(f, "add_prefix_space", &prefixSpace)); err != nil {
		return 0, "", false, err
	}
	if isNull(f["prepend_scheme"]) && prefixSpace != nil && !*prefixSpace {
		scheme = "never"
	}
	r, size := utf8.DecodeRuneInString(replacement)
	if size != len(replacement) || size == 0 {
		return 0, "", false, fmt.Errorf("tokenizers: Metaspace replacement %q is not one character", replacement)
	}
	if scheme != "always" && scheme != "never" && scheme != "first" {
		return 0, "", false, fmt.Errorf("tokenizers: Metaspace prepend scheme %q is not always, never or first", scheme)
	}
	return r, scheme, split, nil
}

// The Unicode script of a rune, kana counted as Han, a space and unassigned runes as no script
func scriptOf(r rune, hint string) string {
	switch {
	case r == 0x30FC:
		return "Han"
	case r == ' ':
		return ""
	}
	name := ""
	if unicode.Is(unicode.Scripts[hint], r) {
		name = hint
	} else {
		for candidate, table := range unicode.Scripts {
			if unicode.Is(table, r) {
				name = candidate
				break
			}
		}
	}
	if name == "Hiragana" || name == "Katakana" {
		return "Han"
	}
	return name
}

// Cuts wherever the script changes, runes without a script joining either side
func unicodeScripts(t text) ([]text, error) {
	var cuts []int
	last, seen, hint := "", false, "Common"
	for i, r := range t.s {
		script := scriptOf(r, hint)
		if script == "" {
			continue
		}
		hint = script
		if !seen || last != script {
			cuts = append(cuts, i)
		}
		last, seen = script, true
	}
	cuts = append(cuts, len(t.s))
	out := make([]text, 0, len(cuts))
	for i := 0; i+1 < len(cuts); i++ {
		out = append(out, t.slice(cuts[i], cuts[i+1]))
	}
	return out, nil
}

func fixedLength(length int) preTokenizer {
	return func(pieces []piece) ([]piece, error) {
		return splitPieces(pieces, func(t text) ([]text, error) {
			var out []text
			start, count := 0, 0
			for i := range t.s {
				if count == length {
					out = append(out, t.slice(start, i))
					start, count = i, 0
				}
				count++
			}
			return append(out, t.slice(start, len(t.s))), nil
		})
	}
}

func parsePreTokenizer(raw json.RawMessage) (preTokenizer, error) {
	if isNull(raw) {
		return nil, nil
	}
	kind, f, err := component(raw)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "BertPreTokenizer":
		return chain([]preTokenizer{splitter(runePattern(isSpace), false, "Removed"), splitter(runePattern(isPunct), false, "Isolated")}), nil
	case "ByteLevel":
		prefixSpace, useRegex := true, true
		if err := firstErr(field(f, "add_prefix_space", &prefixSpace), field(f, "use_regex", &useRegex)); err != nil {
			return nil, err
		}
		return byteLevel(prefixSpace, useRegex)
	case "CharDelimiterSplit":
		var delimiter string
		if err := field(f, "delimiter", &delimiter); err != nil {
			return nil, err
		}
		d, size := utf8.DecodeRuneInString(delimiter)
		if size != len(delimiter) || size == 0 {
			return nil, fmt.Errorf("tokenizers: delimiter %q is not one character", delimiter)
		}
		return splitter(runePattern(func(r rune) bool { return r == d }), false, "Removed"), nil
	case "Digits":
		individual := false
		if err := field(f, "individual_digits", &individual); err != nil {
			return nil, err
		}
		if individual {
			return splitter(runePattern(unicode.IsNumber), false, "Isolated"), nil
		}
		return splitter(runePattern(unicode.IsNumber), false, "Contiguous"), nil
	case "FixedLength":
		length := 5
		if err := field(f, "length", &length); err != nil {
			return nil, err
		}
		if length < 1 {
			return nil, fmt.Errorf("tokenizers: FixedLength of %d characters", length)
		}
		return fixedLength(length), nil
	case "Metaspace":
		replacement, scheme, split, err := metaspaceFields(f)
		if err != nil {
			return nil, err
		}
		return metaspace(replacement, scheme, split), nil
	case "Punctuation":
		behavior := "Isolated"
		if err := field(f, "behavior", &behavior); err != nil {
			return nil, err
		}
		return splitter(runePattern(isPunct), false, behaviorName(behavior)), nil
	case "Sequence":
		parts, err := sequenceOf(f["pretokenizers"], parsePreTokenizer)
		if err != nil {
			return nil, err
		}
		return chain(parts), nil
	case "Split":
		behavior, invert := "Removed", false
		if err := firstErr(field(f, "behavior", &behavior), field(f, "invert", &invert)); err != nil {
			return nil, err
		}
		p, err := patternField(f["pattern"])
		if err != nil {
			return nil, err
		}
		return splitter(p, invert, behaviorName(behavior)), nil
	case "UnicodeScripts":
		return func(pieces []piece) ([]piece, error) { return splitPieces(pieces, unicodeScripts) }, nil
	case "Whitespace":
		return func(pieces []piece) ([]piece, error) { return splitPieces(pieces, whitespace) }, nil
	case "WhitespaceSplit":
		return splitter(runePattern(isSpace), false, "Removed"), nil
	}
	return nil, fmt.Errorf("tokenizers: pre tokenizer %q is not one the format defines", kind)
}
