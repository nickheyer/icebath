package tokenizers

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"golang.org/x/text/unicode/norm"
)

// text is a normalized string that remembers how much of its head maps to the first byte of the input
type text struct {
	s      string
	origin bool
	zero   int
}

func newText(s string) text {
	t := text{s: s, origin: true}
	if s != "" {
		t.zero = 1
	}
	return t
}

// Byte length of the head runes that map to input offset zero
func (t text) zeroBytes() int {
	n := 0
	for i := 0; i < t.zero && n < len(t.s); i++ {
		_, w := utf8.DecodeRuneInString(t.s[n:])
		n += w
	}
	return n
}

// The byte range a to b as its own text, at the origin only when it starts on a head rune
func (t text) slice(a, b int) text {
	if a == b {
		return text{origin: t.origin && a == 0}
	}
	head := t.zeroBytes()
	if !t.origin || a >= head {
		return text{s: t.s[a:b]}
	}
	return text{s: t.s[a:b], origin: true, zero: utf8.RuneCountInString(t.s[a:min(b, head)])}
}

// rewrite rebuilds a text run by run while counting the head runes that still map to input offset zero
type rewrite struct {
	src  text
	out  strings.Builder
	pos  int
	zero int
}

// Replaces the next old runes with s, every new rune sharing the first old one when joined
func (w *rewrite) put(old int, s string, joined bool) {
	w.out.WriteString(s)
	if w.pos < w.src.zero || (old == 0 && w.pos == 0) {
		head := min(w.src.zero-w.pos, old)
		n := utf8.RuneCountInString(s)
		if old == 0 || joined || head == old {
			w.zero += n
		} else {
			w.zero += min(n, head)
		}
	}
	w.pos += old
}

func (w *rewrite) keep(s string) {
	if s != "" {
		w.put(utf8.RuneCountInString(s), s, false)
	}
}

func (w *rewrite) done() text { return text{s: w.out.String(), origin: w.src.origin, zero: w.zero} }

func (t text) filter(keep func(rune) bool) text {
	var b strings.Builder
	zero, i := t.zero, 0
	for _, r := range t.s {
		if keep(r) {
			b.WriteRune(r)
		} else if i < t.zero {
			zero--
		}
		i++
	}
	return text{s: b.String(), origin: t.origin, zero: zero}
}

func (t text) mapRunes(f func(rune) rune) text {
	return text{s: strings.Map(f, t.s), origin: t.origin, zero: t.zero}
}

// Lowercase of every rune by the full Unicode mapping, where only U+0130 grows to two runes
func (t text) lower() text {
	var b strings.Builder
	zero, i := t.zero, 0
	for _, r := range t.s {
		if r == 0x130 {
			b.WriteString("i̇")
			if i < t.zero {
				zero++
			}
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
		i++
	}
	return text{s: b.String(), origin: t.origin, zero: zero}
}

func (t text) normalForm(f norm.Form) text {
	zero := 0
	if t.zero > 0 {
		zero = utf8.RuneCountInString(f.String(t.s[:t.zeroBytes()]))
	}
	return text{s: f.String(t.s), origin: t.origin, zero: zero}
}

func (t text) prepend(p string) text {
	if t.s == "" {
		return t
	}
	zero := t.zero
	if zero > 0 {
		zero += utf8.RuneCountInString(p)
	}
	return text{s: p + t.s, origin: t.origin, zero: zero}
}

// Every match of the pattern replaced by content
func (t text) replace(p pattern, content string) (text, error) {
	matches, err := p.find(t.s)
	if err != nil || len(matches) == 0 {
		return t, err
	}
	w := rewrite{src: t}
	last := 0
	for _, m := range matches {
		w.keep(t.s[last:m[0]])
		w.put(utf8.RuneCountInString(t.s[m[0]:m[1]]), content, true)
		last = m[1]
	}
	w.keep(t.s[last:])
	return w.done(), nil
}

// pattern reports the byte ranges of its matches in order without overlap
type pattern interface {
	find(s string) ([][2]int, error)
}

type runePattern func(rune) bool

func (p runePattern) find(s string) ([][2]int, error) {
	var out [][2]int
	for i, r := range s {
		if p(r) {
			out = append(out, [2]int{i, i + utf8.RuneLen(r)})
		}
	}
	return out, nil
}

type literalPattern string

func (p literalPattern) find(s string) ([][2]int, error) {
	if p == "" {
		return nil, nil
	}
	var out [][2]int
	for at := 0; ; {
		i := strings.Index(s[at:], string(p))
		if i < 0 {
			return out, nil
		}
		out = append(out, [2]int{at + i, at + i + len(p)})
		at += i + len(p)
	}
}

type regexPattern struct{ re *regexp2.Regexp }

func (p regexPattern) find(s string) ([][2]int, error) {
	found, err := p.re.FindAllStringIndex(s, -1)
	if err != nil {
		return nil, fmt.Errorf("tokenizers: pattern %s: %w", p.re.String(), err)
	}
	out := make([][2]int, len(found))
	for i, m := range found {
		out[i] = [2]int{m[0], m[1]}
	}
	return out, nil
}

// Length of the regular expression atom that starts at i, an escape, a class, or one rune
func atomLen(expr string, i int) int {
	switch expr[i] {
	case '\\':
		if i+1 >= len(expr) {
			return 1
		}
		_, w := utf8.DecodeRuneInString(expr[i+1:])
		n := 1 + w
		if c := expr[i+1]; (c == 'p' || c == 'P' || c == 'x' || c == 'k' || c == 'g') && i+n < len(expr) && (expr[i+n] == '{' || expr[i+n] == '<') {
			if end := strings.IndexAny(expr[i+n:], "}>"); end >= 0 {
				n += end + 1
			}
		}
		return n
	case '[':
		j := i + 1
		if j < len(expr) && expr[j] == '^' {
			j++
		}
		if j < len(expr) && expr[j] == ']' {
			j++
		}
		for depth := 1; j < len(expr) && depth > 0; j++ {
			switch expr[j] {
			case '\\':
				j++
			case '[':
				depth++
			case ']':
				depth--
			}
		}
		return min(j, len(expr)) - i
	}
	_, w := utf8.DecodeRuneInString(expr[i:])
	return w
}

const (
	wordSet = `\p{L}\p{M}\p{Nd}\p{Pc}`
	hexSet  = `0-9a-fA-F`
)

var posixSets = map[string]string{
	"alnum": `\p{L}\p{M}\p{Nd}`, "alpha": `\p{L}\p{M}`, "digit": `\p{Nd}`, "space": `\s`, "upper": `\p{Lu}`, "lower": `\p{Ll}`,
	"punct": `\p{P}`, "xdigit": hexSet, "word": wordSet, "blank": `\p{Zs}\t`, "cntrl": `\p{Cc}`,
}

// An Oniguruma character class as a regexp2 class, intersections with a negated class becoming subtractions
func translateClass(cls string) (string, error) {
	body := cls[1 : len(cls)-1]
	negated := strings.HasPrefix(body, "^")
	if negated {
		body = body[1:]
	}
	var items strings.Builder
	complement, minus := "", ""
	for i := 0; i < len(body); {
		switch {
		case body[i] == '\\' && i+1 < len(body):
			switch body[i+1] {
			case 'w':
				items.WriteString(wordSet)
			case 'h':
				items.WriteString(hexSet)
			case 'W', 'H':
				if complement != "" {
					return "", fmt.Errorf("tokenizers: class %s negates two sets at once", cls)
				}
				complement = map[byte]string{'W': wordSet, 'H': hexSet}[body[i+1]]
			default:
				n := atomLen(body, i)
				items.WriteString(body[i : i+n])
				i += n
				continue
			}
			i += 2
		case strings.HasPrefix(body[i:], "[:"):
			end := strings.Index(body[i:], ":]")
			if end < 0 {
				return "", fmt.Errorf("tokenizers: class %s opens a bracket name it never closes", cls)
			}
			set, ok := posixSets[body[i+2:i+end]]
			if !ok {
				return "", fmt.Errorf("tokenizers: class %s names the bracket set %q", cls, body[i+2:i+end])
			}
			items.WriteString(set)
			i += end + 2
		case strings.HasPrefix(body[i:], "&&[^"):
			n := atomLen(body, i+2)
			inner, err := translateClass(body[i+2 : i+2+n])
			if err != nil {
				return "", err
			}
			if i+2+n != len(body) || minus != "" || strings.Contains(inner, "-[") {
				return "", fmt.Errorf("tokenizers: class %s intersects more than one negated class", cls)
			}
			minus = "[" + inner[2:]
			i += 2 + n
		case strings.HasPrefix(body[i:], "&&"):
			return "", fmt.Errorf("tokenizers: class %s intersects with something other than a negated class", cls)
		case body[i] == '[':
			n := atomLen(body, i)
			inner, err := translateClass(body[i : i+n])
			if err != nil {
				return "", err
			}
			if strings.HasPrefix(inner, "[^") || strings.Contains(inner, "-[") {
				return "", fmt.Errorf("tokenizers: class %s nests a negated class", cls)
			}
			items.WriteString(inner[1 : len(inner)-1])
			i += n
		case body[i] == ']':
			items.WriteString(`\]`)
			i++
		default:
			_, w := utf8.DecodeRuneInString(body[i:])
			items.WriteString(body[i : i+w])
			i += w
		}
	}
	set := items.String()
	if complement == "" {
		out := "["
		if negated {
			out += "^"
		}
		out += set
		if minus != "" {
			out += "-" + minus
		}
		return out + "]", nil
	}
	if minus != "" {
		return "", fmt.Errorf("tokenizers: class %s both negates a set and intersects", cls)
	}
	rest := "[" + complement + "]"
	if set != "" {
		rest = "[" + complement + "-[" + set + "]]"
	}
	if negated {
		return rest, nil
	}
	return `[\s\S-` + rest + "]", nil
}

// Length of a counted repetition such as {2,5} at the head of s, zero when s opens none
func intervalLen(s string) int {
	i := 1
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == ',') {
		i++
	}
	if i < len(s) && i > 1 && s[i] == '}' {
		return i + 1
	}
	return 0
}

// An Oniguruma expression in the syntax regexp2 compiles, possessive quantifiers becoming atomic groups
func translateRegex(expr string) (string, error) {
	var out strings.Builder
	var groups []int
	atom := -1
	wrap := func(open, shut string) {
		whole := out.String()
		out.Reset()
		out.WriteString(whole[:atom] + open + whole[atom:] + shut)
	}
	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == '(':
			groups = append(groups, out.Len())
			out.WriteByte(c)
			atom = -1
			i++
		case c == ')':
			out.WriteByte(c)
			atom = -1
			if n := len(groups); n > 0 {
				atom = groups[n-1]
				groups = groups[:n-1]
			}
			i++
		case (c == '?' || c == '*' || c == '+') && atom >= 0:
			if i+1 < len(expr) && expr[i+1] == '+' {
				wrap("(?>", string(c)+")")
				i += 2
				continue
			}
			out.WriteByte(c)
			i++
		case c == '{' && atom >= 0 && intervalLen(expr[i:]) > 0:
			n := intervalLen(expr[i:])
			if i+n < len(expr) && expr[i+n] == '+' {
				wrap("(?:", expr[i:i+n]+")+")
				i += n + 1
				continue
			}
			out.WriteString(expr[i : i+n])
			i += n
		case c == '|' || c == '^' || c == '$':
			out.WriteByte(c)
			atom = -1
			i++
		default:
			n := atomLen(expr, i)
			part := expr[i : i+n]
			switch {
			case part == `\w`:
				part = "[" + wordSet + "]"
			case part == `\W`:
				part = "[^" + wordSet + "]"
			case part == `\h`:
				part = "[" + hexSet + "]"
			case part == `\H`:
				part = "[^" + hexSet + "]"
			case c == '[' && n > 1 && part[n-1] == ']':
				var err error
				if part, err = translateClass(part); err != nil {
					return "", err
				}
			}
			atom = out.Len()
			out.WriteString(part)
			i += n
		}
	}
	return out.String(), nil
}

func compileRegex(expr string) (pattern, error) {
	translated, err := translateRegex(expr)
	if err != nil {
		return nil, err
	}
	re, err := regexp2.Compile(translated, regexp2.Multiline)
	if err != nil {
		return nil, fmt.Errorf("tokenizers: pattern %q: %w", expr, err)
	}
	return regexPattern{re}, nil
}

// span is one stretch of a string, marked when the pattern matched it
type span struct {
	a, b  int
	match bool
}

// The whole string as alternating unmatched and matched spans, marks flipped when inverted
func partition(s string, p pattern, invert bool) ([]span, error) {
	if s == "" {
		return nil, nil
	}
	matches, err := p.find(s)
	if err != nil {
		return nil, err
	}
	var out []span
	last := 0
	for _, m := range matches {
		if last != m[0] {
			out = append(out, span{last, m[0], invert})
		}
		out = append(out, span{m[0], m[1], !invert})
		last = m[1]
	}
	if last != len(s) {
		out = append(out, span{last, len(s), invert})
	}
	return out, nil
}

// Pieces of the text around the pattern matches, the matches treated as the split behavior names
func (t text) split(p pattern, invert bool, behavior string) ([]text, error) {
	spans, err := partition(t.s, p, invert)
	if err != nil {
		return nil, err
	}
	var keep []span
	switch behavior {
	case "Removed":
		for _, sp := range spans {
			if !sp.match {
				keep = append(keep, sp)
			}
		}
	case "Isolated":
		keep = spans
	case "Contiguous":
		previous := false
		for _, sp := range spans {
			if sp.match == previous && len(keep) > 0 {
				keep[len(keep)-1].b = sp.b
			} else {
				keep = append(keep, sp)
			}
			previous = sp.match
		}
	case "MergedWithPrevious":
		previous := false
		for _, sp := range spans {
			if sp.match && !previous && len(keep) > 0 {
				keep[len(keep)-1].b = sp.b
			} else {
				keep = append(keep, sp)
			}
			previous = sp.match
		}
	case "MergedWithNext":
		previous := false
		for i := len(spans) - 1; i >= 0; i-- {
			sp := spans[i]
			if sp.match && !previous && len(keep) > 0 {
				keep[len(keep)-1].a = sp.a
			} else {
				keep = append(keep, sp)
			}
			previous = sp.match
		}
		for i, j := 0, len(keep)-1; i < j; i, j = i+1, j-1 {
			keep[i], keep[j] = keep[j], keep[i]
		}
	default:
		return nil, fmt.Errorf("tokenizers: split behavior %q is not one the format defines", behavior)
	}
	out := make([]text, 0, len(keep))
	for _, sp := range keep {
		out = append(out, t.slice(sp.a, sp.b))
	}
	return out, nil
}

// Split behavior names of tokenizer.json, both the current and the lowercase spelling
func behaviorName(name string) string {
	switch strings.ToLower(strings.ReplaceAll(name, "_", "")) {
	case "removed":
		return "Removed"
	case "isolated":
		return "Isolated"
	case "contiguous":
		return "Contiguous"
	case "mergedwithprevious":
		return "MergedWithPrevious"
	case "mergedwithnext":
		return "MergedWithNext"
	}
	return name
}

func isSpace(r rune) bool { return unicode.IsSpace(r) }

// Punctuation as the format reads it, every ASCII symbol and the Unicode punctuation categories
func isPunct(r rune) bool {
	if r < 128 {
		return r >= '!' && r <= '/' || r >= ':' && r <= '@' || r >= '[' && r <= '`' || r >= '{' && r <= '~'
	}
	return unicode.IsPunct(r)
}

// A word rune of Unicode regular expressions, alphabetic, mark, decimal digit, connector or join control
func isWord(r rune) bool {
	if r < 128 {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_'
	}
	return unicode.IsLetter(r) || unicode.Is(unicode.M, r) || unicode.Is(unicode.Nd, r) || unicode.Is(unicode.Pc, r) ||
		unicode.Is(unicode.Nl, r) || unicode.Is(unicode.Other_Alphabetic, r) || unicode.Is(unicode.Other_Lowercase, r) ||
		unicode.Is(unicode.Other_Uppercase, r) || r == 0x200C || r == 0x200D
}

// The character each byte becomes under byte level tokenization, printable bytes themselves and the rest from U+0100 up
var byteRunes = func() (table [256]rune) {
	n := rune(0)
	for b := 0; b < 256; b++ {
		if b >= '!' && b <= '~' || b >= 0xA1 && b <= 0xAC || b >= 0xAE {
			table[b] = rune(b)
		} else {
			table[b] = 256 + n
			n++
		}
	}
	return table
}()

var runeBytes = func() map[rune]byte {
	m := make(map[rune]byte, 256)
	for b, r := range byteRunes {
		m[r] = byte(b)
	}
	return m
}()

// Every byte of the text as its byte level character
func (t text) byteLevel() text {
	var b strings.Builder
	b.Grow(len(t.s) * 2)
	for i := 0; i < len(t.s); i++ {
		b.WriteRune(byteRunes[t.s[i]])
	}
	return text{s: b.String(), origin: t.origin, zero: t.zeroBytes()}
}
