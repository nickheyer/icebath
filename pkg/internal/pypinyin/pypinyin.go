package pypinyin

import (
	"os"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// A Python exception the package raises, named by its class
type PyError struct{ Class, Msg string }

func (e *PyError) Error() string { return e.Msg }

// The imported pypinyin package with its single character and phrase dictionaries loaded
type Module struct {
	pinyinDict  map[rune]string
	phrasesDict map[string][][]string
	prefixSet   map[string]struct{}
}

// Parsed JSON object of a dictionary file of the package, read as UTF-8
func loadDict(dir, name string) (*pyjson.Object, error) {
	raw, err := os.ReadFile(pypath.Join(dir, name))
	if err != nil {
		return nil, &PyError{Class: pypath.OSErrorClass(err), Msg: pypath.OSErrorText(err)}
	}
	text, err := pycodecs.Decode(raw, "utf-8")
	if err != nil {
		return nil, &PyError{Class: "UnicodeDecodeError", Msg: err.Error()}
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return nil, &PyError{Class: "JSONDecodeError", Msg: err.Error()}
	}
	object, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Msg: name + " does not hold a JSON object"}
	}
	return object, nil
}

// Imports the package installed at dir, loading both dictionaries and training the phrase segmenter
func Import(dir string) (*Module, error) {
	m := &Module{pinyinDict: map[rune]string{}, phrasesDict: map[string][][]string{}, prefixSet: map[string]struct{}{}}
	if os.Getenv("PYPINYIN_NO_PHRASES") == "" {
		phrases, err := loadDict(dir, "phrases_dict.json")
		if err != nil {
			return nil, err
		}
		for _, phrase := range phrases.Keys {
			items, ok := phrases.Vals[phrase].([]any)
			if !ok {
				return nil, &PyError{Class: "TypeError", Msg: "phrases_dict.json entry " + pyrepr.StrRepr(phrase) + " is not a list"}
			}
			entry := make([][]string, len(items))
			for i, item := range items {
				readings, ok := item.([]any)
				if !ok {
					return nil, &PyError{Class: "TypeError", Msg: "phrases_dict.json entry " + pyrepr.StrRepr(phrase) + " is not a list of lists"}
				}
				for _, reading := range readings {
					text, ok := reading.(string)
					if !ok {
						return nil, &PyError{Class: "TypeError", Msg: "phrases_dict.json entry " + pyrepr.StrRepr(phrase) + " holds a reading that is not a string"}
					}
					entry[i] = append(entry[i], text)
				}
			}
			m.phrasesDict[phrase] = entry
		}
	}
	single, err := loadDict(dir, "pinyin_dict.json")
	if err != nil {
		return nil, err
	}
	for _, k := range single.Keys {
		num, ok := pyint.Parse(k)
		if !ok {
			return nil, &PyError{Class: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.StrRepr(k)}
		}
		readings, isText := single.Vals[k].(string)
		if !isText {
			return nil, &PyError{Class: "TypeError", Msg: "pinyin_dict.json entry " + pyrepr.StrRepr(k) + " is not a string"}
		}
		if num.IsInt64() && num.Int64() >= 0 && num.Int64() <= unicode.MaxRune {
			m.pinyinDict[rune(num.Int64())] = readings
		}
	}
	for word := range m.phrasesDict {
		runes := []rune(word)
		for i := range runes {
			m.prefixSet[string(runes[:i+1])] = struct{}{}
		}
	}
	return m, nil
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

// Whether a string is one or more Han characters, optionally followed by a final newline
func isHans(s string) bool {
	runes := []rune(strings.TrimSuffix(s, "\n"))
	for _, r := range runes {
		if !isHan(r) {
			return false
		}
	}
	return len(runes) > 0
}

// Splits text into alternating runs of Han and other characters
func simpleSeg(chars string) []string {
	s := ""
	ret := []string{}
	flag := 0
	for n, c := range []rune(chars) {
		kind := 1
		if isHan(c) {
			kind = 0
		}
		if n == 0 {
			flag = kind
		}
		if flag == kind {
			s += string(c)
		} else {
			ret = append(ret, s)
			flag = kind
			s = string(c)
		}
	}
	return append(ret, s)
}

// Cuts a Han run into dictionary phrases by forward maximum matching, single characters where no phrase fits
func (m *Module) cut(text string) []string {
	var out []string
	remain := []rune(text)
	for len(remain) > 0 {
		lastValidWord, lastValidIndex := "", 0
		broke := false
		for index := range remain {
			word := string(remain[:index+1])
			if _, ok := m.prefixSet[word]; ok {
				if _, phrase := m.phrasesDict[word]; phrase {
					lastValidWord, lastValidIndex = word, index+1
				}
				continue
			}
			if lastValidWord != "" {
				out = append(out, lastValidWord)
				remain = remain[lastValidIndex:]
			} else {
				out = append(out, string(remain[0]))
				remain = remain[1:]
			}
			broke = true
			break
		}
		if broke {
			continue
		}
		if lastValidWord != "" {
			out = append(out, lastValidWord)
			remain = remain[lastValidIndex:]
			continue
		}
		if _, phrase := m.phrasesDict[string(remain)]; phrase {
			out = append(out, string(remain))
		} else {
			for _, x := range remain {
				out = append(out, string(x))
			}
		}
		break
	}
	return out
}

// Words of the text, Han runs cut into phrases and other runs kept whole
func (m *Module) seg(hans string) []string {
	ret := []string{}
	for _, x := range simpleSeg(hans) {
		if !isHans(x) || len(m.phrasesDict) == 0 {
			ret = append(ret, x)
		} else {
			ret = append(ret, m.cut(x)...)
		}
	}
	return ret
}

// Readings of one word, the first reading per character in tone mark style and other text passed through
func (m *Module) convert(words string) ([][]string, error) {
	pys := [][]string{}
	if isHans(words) {
		phrase := []rune(words)
		if entry, ok := m.phrasesDict[words]; ok {
			for _, item := range entry {
				pys = append(pys, append([]string(nil), item...))
			}
		} else {
			for _, han := range phrase {
				if readings, ok := m.pinyinDict[han]; ok {
					pys = append(pys, strings.Split(readings, ","))
				} else {
					pys = append(pys, []string{string(han)})
				}
			}
		}
		for idx, item := range pys {
			if idx >= len(phrase) {
				return nil, &PyError{Class: "IndexError", Msg: "string index out of range"}
			}
			if len(item) == 0 {
				return nil, &PyError{Class: "IndexError", Msg: "list index out of range"}
			}
			pys[idx] = []string{item[0]}
		}
	} else if words != "" {
		pys = append(pys, []string{words})
	}
	for i, lst := range pys {
		kept := []string{}
		for _, item := range lst {
			if item != "" && !contains(kept, item) {
				kept = append(kept, item)
			}
		}
		if len(kept) == 0 {
			kept = []string{""}
		}
		pys[i] = kept
	}
	return pys, nil
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// Readings of a string in tone mark style, one single reading list per Han character or run of other text
func (m *Module) Pinyin(hans string) ([][]string, error) {
	pys := [][]string{}
	for _, words := range m.seg(hans) {
		converted, err := m.convert(words)
		if err != nil {
			return nil, err
		}
		pys = append(pys, converted...)
	}
	return pys, nil
}

var initialsNotStrict = []string{"b", "p", "m", "f", "d", "t", "n", "l", "g", "k", "h", "j", "q", "x", "zh", "ch", "sh", "r", "z", "c", "s", "y", "w"}

// Tone marked characters and the letter plus tone number each stands for, in dictionary order
var phoneticSymbols = [][2]string{
	{"ā", "a1"}, {"á", "a2"}, {"ǎ", "a3"}, {"à", "a4"}, {"ē", "e1"}, {"é", "e2"}, {"ě", "e3"}, {"è", "e4"},
	{"ō", "o1"}, {"ó", "o2"}, {"ǒ", "o3"}, {"ò", "o4"}, {"ī", "i1"}, {"í", "i2"}, {"ǐ", "i3"}, {"ì", "i4"},
	{"ū", "u1"}, {"ú", "u2"}, {"ǔ", "u3"}, {"ù", "u4"}, {"ü", "v"}, {"ǖ", "v1"}, {"ǘ", "v2"}, {"ǚ", "v3"}, {"ǜ", "v4"},
	{"ń", "n2"}, {"ň", "n3"}, {"ǹ", "n4"}, {"m̄", "m1"}, {"ḿ", "m2"}, {"m̀", "m4"},
	{"ê̄", "ê1"}, {"ế", "ê2"}, {"ê̌", "ê3"}, {"ề", "ê4"},
}

// Initial consonant of a pinyin syllable with y and w counted as initials, empty when it has none
func ToInitialsNotStrict(pinyin string) string {
	for _, i := range initialsNotStrict {
		if strings.HasPrefix(pinyin, i) {
			return i
		}
	}
	return ""
}

// Replaces every tone marked character by its letter and tone number
func replaceSymbolToNumber(pinyin string) string {
	var b strings.Builder
	for _, r := range pinyin {
		replaced := false
		for _, pair := range phoneticSymbols {
			if len([]rune(pair[0])) == 1 && []rune(pair[0])[0] == r {
				b.WriteString(pair[1])
				replaced = true
				break
			}
		}
		if !replaced {
			b.WriteRune(r)
		}
	}
	value := b.String()
	for _, pair := range phoneticSymbols {
		if len([]rune(pair[0])) > 1 {
			value = strings.ReplaceAll(value, pair[0], pair[1])
		}
	}
	return value
}

func removeDigits(s string) string {
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

// Index of the character that carries the tone mark, false when no markable character exists
func rightMarkIndex(pinyinNoTone []rune) (int, bool) {
	has := func(sub string) bool { return runeIndex(pinyinNoTone, sub) >= 0 }
	switch {
	case has("iou"):
		return runeIndex(pinyinNoTone, "u"), true
	case has("uei"):
		return runeIndex(pinyinNoTone, "i"), true
	case has("uen"):
		return runeIndex(pinyinNoTone, "u"), true
	}
	for _, group := range [][]string{{"a", "o", "e"}, {"iu", "ui"}, {"i", "u", "v", "ü"}, {"n", "m", "ê"}} {
		for _, c := range group {
			if has(c) {
				return runeIndex(pinyinNoTone, c) + len([]rune(c)) - 1, true
			}
		}
	}
	return 0, false
}

// Moves the tone number of a syllable from its end to right after the character that carries the tone
func tone3ToTone2(tone3 string) string {
	noNumber := []rune(strings.ReplaceAll(removeDigits(tone3), "ü", "v"))
	markIndex, ok := rightMarkIndex(noNumber)
	if !ok {
		markIndex = len(noNumber) - 1
	}
	number, ok := firstDigit(tone3)
	if !ok {
		return tone3
	}
	s := string(noNumber[:markIndex+1]) + number + string(noNumber[markIndex+1:])
	return strings.ReplaceAll(s, "ü", "v")
}

// Replaces each letter and tone number pair by its tone marked character
func tone2ToTone(tone2 string) string {
	runes := []rune(strings.NewReplacer("ü", "v", "5", "", "0", "").Replace(tone2))
	var b strings.Builder
	for i := 0; i < len(runes); i++ {
		if i+1 < len(runes) && strings.ContainsRune("aeoiuvnmêü", runes[i]) && runes[i+1] >= '1' && runes[i+1] <= '5' {
			pair := string(runes[i : i+2])
			for _, symbol := range phoneticSymbols {
				if symbol[1] == pair {
					pair = symbol[0]
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

// Final of a pinyin syllable with its tone mark, y and w counted as initials
func ToFinalsToneNotStrict(pinyin string) string {
	pinyin = strings.ReplaceAll(pinyin, "5", "")
	numbered := replaceSymbolToNumber(pinyin)
	newPinyin := strings.ReplaceAll(removeDigits(numbered), "v", "ü")
	finals := newPinyin[len(ToInitialsNotStrict(newPinyin)):]
	if finals == "" {
		finals = newPinyin
	}
	finals = strings.ReplaceAll(finals, "ü", "v")
	if finals != "" {
		if number, ok := firstDigit(numbered); ok {
			finals += number
		}
	}
	return tone2ToTone(tone3ToTone2(finals))
}
