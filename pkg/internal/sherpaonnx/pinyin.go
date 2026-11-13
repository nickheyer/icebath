package sherpaonnx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/nickheyer/icebath/pkg/fetch"
)

// Published package carrying the single character and phrase pinyin dictionaries
var pinyinWheel = fetch.Asset{Name: "pypinyin-0.55.0-py2.py3-none-any.whl", Size: 840203,
	URL:    "https://files.pythonhosted.org/packages/b9/7b/4cabc76fcc21c3c7d5c671d8783984d30ac9d3bb387c4ba784fca3cdfa3a/pypinyin-0.55.0-py2.py3-none-any.whl",
	SHA256: "d53b1e8ad2cdb815fb2cb604ed3123372f5a28c6f447571244aca36fc62a286f"}

// Single character readings, phrase readings and every phrase prefix
type pinyinDict struct {
	chars    map[rune]string
	phrases  map[string][][]string
	prefixes map[string]bool
}

var pinyinLoaded struct {
	sync.Mutex
	dict *pinyinDict
}

// Pinyin dictionaries, fetched into the icebath cache on first use
func loadPinyin(ctx context.Context) (*pinyinDict, error) {
	pinyinLoaded.Lock()
	defer pinyinLoaded.Unlock()
	if pinyinLoaded.dict != nil {
		return pinyinLoaded.dict, nil
	}
	tree, err := fetch.Tree(ctx, pinyinWheel)
	if err != nil {
		return nil, err
	}
	d := &pinyinDict{chars: map[rune]string{}, phrases: map[string][][]string{}, prefixes: map[string]bool{}}
	var chars map[string]string
	if err := readJSON(filepath.Join(tree, "pypinyin", "pinyin_dict.json"), &chars); err != nil {
		return nil, err
	}
	for code, readings := range chars {
		if n, err := strconv.ParseInt(code, 10, 32); err == nil && n >= 0 && n <= unicode.MaxRune {
			d.chars[rune(n)] = readings
		}
	}
	if os.Getenv("PYPINYIN_NO_PHRASES") == "" {
		if err := readJSON(filepath.Join(tree, "pypinyin", "phrases_dict.json"), &d.phrases); err != nil {
			return nil, err
		}
	}
	for phrase := range d.phrases {
		runes := []rune(phrase)
		for i := range runes {
			d.prefixes[string(runes[:i+1])] = true
		}
	}
	pinyinLoaded.dict = d
	return d, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

var hanRanges = [][2]rune{{0x3007, 0x3007}, {0xE815, 0xE864}, {0xFA18, 0xFA18}, {0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0xF900, 0xFAFF},
	{0x20000, 0x2A6DF}, {0x2A703, 0x2B73F}, {0x2B740, 0x2B81D}, {0x2B825, 0x2BF6E}, {0x2C029, 0x2CE93}, {0x2D016, 0x2D016},
	{0x2D11B, 0x2EBD9}, {0x2F80A, 0x2FA1F}, {0x30000, 0x3134A}, {0x300F7, 0x31288}, {0x30EDD, 0x30EDE}, {0x31350, 0x32389}}

func isHan(r rune) bool {
	for _, span := range hanRanges {
		if r >= span[0] && r <= span[1] {
			return true
		}
	}
	return false
}

// Whether a string is one or more Han characters
func isHans(s string) bool {
	runes := []rune(strings.TrimSuffix(s, "\n"))
	for _, r := range runes {
		if !isHan(r) {
			return false
		}
	}
	return len(runes) > 0
}

// Alternating runs of Han and other characters
func simpleSeg(text string) []string {
	var out []string
	var run []rune
	han := false
	for i, r := range []rune(text) {
		if i > 0 && isHan(r) != han {
			out = append(out, string(run))
			run = nil
		}
		han = isHan(r)
		run = append(run, r)
	}
	return append(out, string(run))
}

// Han run cut into dictionary phrases by forward maximum matching, single characters where no phrase fits
func (d *pinyinDict) cut(text string) []string {
	var out []string
	remain := []rune(text)
	for len(remain) > 0 {
		best, broke := 0, false
		for i := range remain {
			word := string(remain[:i+1])
			if !d.prefixes[word] {
				broke = true
				break
			}
			if _, ok := d.phrases[word]; ok {
				best = i + 1
			}
		}
		switch {
		case best > 0:
			out = append(out, string(remain[:best]))
			remain = remain[best:]
		case broke:
			out = append(out, string(remain[0]))
			remain = remain[1:]
		default:
			for _, r := range remain {
				out = append(out, string(r))
			}
			remain = nil
		}
	}
	return out
}

// First reading of every Han character of a word, other text passed through as one item
func (d *pinyinDict) readings(word string) []string {
	if !isHans(word) {
		if word == "" {
			return nil
		}
		return []string{word}
	}
	if entry, ok := d.phrases[word]; ok && len(entry) == len([]rune(word)) {
		out := make([]string, len(entry))
		for i, item := range entry {
			if len(item) > 0 {
				out[i] = item[0]
			}
		}
		return out
	}
	var out []string
	for _, r := range word {
		if readings, ok := d.chars[r]; ok {
			first, _, _ := strings.Cut(readings, ",")
			out = append(out, first)
		} else {
			out = append(out, string(r))
		}
	}
	return out
}

// Syllables of a text, split into initial and toned final when partial is set
func (d *pinyinDict) syllables(text string, partial bool) []string {
	var out []string
	for _, seg := range simpleSeg(text) {
		words := []string{seg}
		if isHans(seg) && len(d.phrases) > 0 {
			words = d.cut(seg)
		}
		for _, w := range words {
			for _, x := range d.readings(w) {
				if !partial {
					out = append(out, x)
					continue
				}
				initial, final := initialOf(x), toneFinal(x)
				if initial == "" && final == "" {
					out = append(out, x)
					continue
				}
				if initial != "" {
					out = append(out, initial)
				}
				if final != "" {
					out = append(out, final)
				}
			}
		}
	}
	return out
}

var initials = []string{"b", "p", "m", "f", "d", "t", "n", "l", "g", "k", "h", "j", "q", "x", "zh", "ch", "sh", "r", "z", "c", "s", "y", "w"}

// Initial consonant of a syllable with y and w counted as initials, empty when it has none
func initialOf(pinyin string) string {
	for _, i := range initials {
		if strings.HasPrefix(pinyin, i) {
			return i
		}
	}
	return ""
}

// Tone marked characters and the letter plus tone number each stands for
var toneMarks = [][2]string{
	{"ā", "a1"}, {"á", "a2"}, {"ǎ", "a3"}, {"à", "a4"}, {"ē", "e1"}, {"é", "e2"}, {"ě", "e3"}, {"è", "e4"},
	{"ō", "o1"}, {"ó", "o2"}, {"ǒ", "o3"}, {"ò", "o4"}, {"ī", "i1"}, {"í", "i2"}, {"ǐ", "i3"}, {"ì", "i4"},
	{"ū", "u1"}, {"ú", "u2"}, {"ǔ", "u3"}, {"ù", "u4"}, {"ü", "v"}, {"ǖ", "v1"}, {"ǘ", "v2"}, {"ǚ", "v3"}, {"ǜ", "v4"},
	{"ń", "n2"}, {"ň", "n3"}, {"ǹ", "n4"}, {"m̄", "m1"}, {"ḿ", "m2"}, {"m̀", "m4"},
	{"ê̄", "ê1"}, {"ế", "ê2"}, {"ê̌", "ê3"}, {"ề", "ê4"},
}

// Whether a token carries a pinyin tone mark
func hasToneMark(s string) bool {
	for _, m := range toneMarks {
		if m[1] != "v" && strings.Contains(s, m[0]) {
			return true
		}
	}
	return false
}

// Replaces every tone marked character by its letter and tone number
func markToNumber(pinyin string) string {
	var b strings.Builder
	for _, r := range pinyin {
		replaced := false
		for _, m := range toneMarks {
			if rs := []rune(m[0]); len(rs) == 1 && rs[0] == r {
				b.WriteString(m[1])
				replaced = true
				break
			}
		}
		if !replaced {
			b.WriteRune(r)
		}
	}
	value := b.String()
	for _, m := range toneMarks {
		if len([]rune(m[0])) > 1 {
			value = strings.ReplaceAll(value, m[0], m[1])
		}
	}
	return value
}

func dropDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return -1
		}
		return r
	}, s)
}

func firstDigit(s string) (string, bool) {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return string(r), true
		}
	}
	return "", false
}

func runeIndex(s []rune, sub string) int {
	i := strings.Index(string(s), sub)
	if i < 0 {
		return -1
	}
	return len([]rune(string(s)[:i]))
}

// Index of the character that carries the tone mark, false when none can
func markIndex(noTone []rune) (int, bool) {
	has := func(sub string) bool { return runeIndex(noTone, sub) >= 0 }
	switch {
	case has("iou"):
		return runeIndex(noTone, "u"), true
	case has("uei"):
		return runeIndex(noTone, "i"), true
	case has("uen"):
		return runeIndex(noTone, "u"), true
	}
	for _, group := range [][]string{{"a", "o", "e"}, {"iu", "ui"}, {"i", "u", "v", "ü"}, {"n", "m", "ê"}} {
		for _, c := range group {
			if has(c) {
				return runeIndex(noTone, c) + len([]rune(c)) - 1, true
			}
		}
	}
	return 0, false
}

// Moves a trailing tone number to right after the character that carries the tone
func tone3ToTone2(tone3 string) string {
	noNumber := []rune(strings.ReplaceAll(dropDigits(tone3), "ü", "v"))
	at, ok := markIndex(noNumber)
	if !ok {
		at = len(noNumber) - 1
	}
	number, ok := firstDigit(tone3)
	if !ok {
		return tone3
	}
	return strings.ReplaceAll(string(noNumber[:at+1])+number+string(noNumber[at+1:]), "ü", "v")
}

// Replaces each letter and tone number pair by its tone marked character
func tone2ToMark(tone2 string) string {
	runes := []rune(strings.NewReplacer("ü", "v", "5", "", "0", "").Replace(tone2))
	var b strings.Builder
	for i := 0; i < len(runes); i++ {
		if i+1 < len(runes) && strings.ContainsRune("aeoiuvnmêü", runes[i]) && runes[i+1] >= '1' && runes[i+1] <= '5' {
			pair := string(runes[i : i+2])
			for _, m := range toneMarks {
				if m[1] == pair {
					pair = m[0]
					break
				}
			}
			b.WriteString(pair)
			i++
			continue
		}
		b.WriteRune(runes[i])
	}
	return strings.ReplaceAll(b.String(), "v", "ü")
}

// Final of a syllable with its tone mark, y and w counted as initials
func toneFinal(pinyin string) string {
	numbered := markToNumber(strings.ReplaceAll(pinyin, "5", ""))
	plain := strings.ReplaceAll(dropDigits(numbered), "v", "ü")
	final := plain[len(initialOf(plain)):]
	if final == "" {
		final = plain
	}
	final = strings.ReplaceAll(final, "ü", "v")
	if final != "" {
		if number, ok := firstDigit(numbered); ok {
			final += number
		}
	}
	return tone2ToMark(tone3ToTone2(final))
}
