// Dangerous command detection, unconditional floors, approval context, the guardian verdict and permission modes
package approval

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
)

const pySpaceClass = `\t\n\v\f\r \x1c-\x1f\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

func pySpace(r rune) bool {
	switch {
	case r >= '\t' && r <= '\r', r == ' ', r >= 0x1c && r <= 0x1f, r == 0x85, r == 0xa0, r == 0x1680,
		r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f, r == 0x205f, r == 0x3000:
		return true
	}
	return false
}

func pyWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func runeWidth(s string, i int) int {
	if s[i] < utf8.RuneSelf {
		return 1
	}
	_, w := utf8.DecodeRuneInString(s[i:])
	return w
}

func runeAt(s string, i int) (rune, int) {
	if i >= len(s) {
		return -1, 0
	}
	if s[i] < utf8.RuneSelf {
		return rune(s[i]), 1
	}
	return utf8.DecodeRuneInString(s[i:])
}

func runeBefore(s string, i int) rune {
	if i <= 0 {
		return -1
	}
	if s[i-1] < utf8.RuneSelf {
		return rune(s[i-1])
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r
}

func spaceAt(s string, i int) bool {
	r, w := runeAt(s, i)
	return w > 0 && pySpace(r)
}

func skipSpace(s string, i int) int {
	for i < len(s) {
		r, w := runeAt(s, i)
		if !pySpace(r) {
			break
		}
		i += w
	}
	return i
}

func runeCount(s string) int { return utf8.RuneCountInString(s) }

func blank(s string) string { return strings.Repeat(" ", runeCount(s)) }

func pyStrip(s string) string { return strings.TrimFunc(s, pySpace) }

func pyRStrip(s string) string { return strings.TrimRightFunc(s, pySpace) }

func pyFields(s string) []string { return strings.FieldsFunc(s, pySpace) }

func pySlice(s string, a, b int) string {
	if a < 0 {
		a = 0
	}
	if b > len(s) {
		b = len(s)
	}
	if b <= a {
		return ""
	}
	return s[a:b]
}

type edit struct {
	start, end int
	text       string
}

func splice(s string, edits []edit) string {
	var b strings.Builder
	prev := 0
	for _, e := range edits {
		b.WriteString(pySlice(s, prev, e.start))
		b.WriteString(e.text)
		prev = e.end
	}
	b.WriteString(pySlice(s, prev, len(s)))
	return b.String()
}

func caseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', 0xb7, 0x387, 0x55f, 0x5f4, 0x2018, 0x2019, 0x2024, 0x2027, 0xfe13, 0xfe52, 0xfe55, 0xff07, 0xff0e, 0xff1a:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}

func cased(r rune) bool {
	return unicode.In(r, unicode.Lu, unicode.Ll, unicode.Lt, unicode.Other_Lowercase, unicode.Other_Uppercase)
}

func finalSigma(rs []rune, k int) bool {
	before := false
	for j := k - 1; j >= 0; j-- {
		if caseIgnorable(rs[j]) {
			continue
		}
		before = cased(rs[j])
		break
	}
	if !before {
		return false
	}
	for j := k + 1; j < len(rs); j++ {
		if caseIgnorable(rs[j]) {
			continue
		}
		return !cased(rs[j])
	}
	return true
}

func pyLower(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for k, r := range rs {
		switch {
		case r == 0x130:
			b.WriteString("i̇")
		case r == 0x3a3:
			if finalSigma(rs, k) {
				b.WriteRune(0x3c2)
			} else {
				b.WriteRune(0x3c3)
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

func casefold(s string) string { return cases.Fold().String(s) }

func foldRune(r rune) rune {
	switch {
	case r >= 'A' && r <= 'Z':
		return r + ('a' - 'A')
	case r == 0x130 || r == 0x131:
		return 'i'
	case r == 0x212a:
		return 'k'
	case r == 0x17f:
		return 's'
	}
	return r
}

func foldPrefix(s, lit string) (int, bool) {
	i := 0
	for k := 0; k < len(lit); k++ {
		r, w := runeAt(s, i)
		if w == 0 || foldRune(r) != rune(lit[k]) {
			return 0, false
		}
		i += w
	}
	return i, true
}

func foldEqual(s, lit string) bool {
	n, ok := foldPrefix(s, lit)
	return ok && n == len(s)
}

func foldSuffix(s, lit string) (int, bool) {
	for start := len(s) - len(lit); start >= 0 && start >= len(s)-4*len(lit); start-- {
		if !utf8.RuneStart(s[start]) {
			continue
		}
		if n, ok := foldPrefix(s[start:], lit); ok && start+n == len(s) {
			return start, true
		}
	}
	return 0, false
}

func wide(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch {
		case r == '\v' || (r >= 0x1c && r <= 0x1f):
			b.WriteByte(' ')
		case r < utf8.RuneSelf:
			b.WriteByte(byte(r))
		case r == 0x130 || r == 0x131:
			b.WriteByte('i')
		case r == 0x212a:
			b.WriteByte('k')
		case r == 0x17f:
			b.WriteByte('s')
		case pySpace(r):
			b.WriteByte(' ')
		case pyWord(r):
			b.WriteByte('_')
			b.WriteRune(r)
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func asciiWord(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

const phBase = 0x10ff00

func ph(k int) string { return string(rune(phBase + k)) }

func translate(src string, vm, ci bool) string {
	var b strings.Builder
	b.WriteString("(?s")
	if ci {
		b.WriteString("i")
	}
	b.WriteString(")")
	stack := []bool{ci}
	cur := func() bool { return stack[len(stack)-1] }
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\\':
			i = translateEscape(&b, src, i, vm, false)
		case c == '[':
			i = translateClass(&b, src, i, vm, vm && cur())
		case c == '(':
			if strings.HasPrefix(src[i:], "(?=") || strings.HasPrefix(src[i:], "(?!") || strings.HasPrefix(src[i:], "(?<=") || strings.HasPrefix(src[i:], "(?<!") {
				panic(fmt.Sprintf("approval pattern keeps a lookaround: %q", src))
			}
			if strings.HasPrefix(src[i:], "(?-i:") {
				stack = append(stack, false)
			} else {
				stack = append(stack, cur())
			}
			if strings.HasPrefix(src[i:], "(?") {
				end := strings.IndexAny(src[i+2:], ":>")
				b.WriteString(src[i : i+2+end+1])
				i += 2 + end + 1
			} else {
				b.WriteByte('(')
				i++
			}
		case c == ')':
			stack = stack[:len(stack)-1]
			b.WriteByte(')')
			i++
		case c == '$':
			if vm {
				b.WriteString(ph(asPyEnd))
			} else {
				b.WriteString(`(?:\n?\z)`)
			}
			i++
		case (c == '+' || c == '*' || c == '?' || c == '}') && i+1 < len(src) && src[i+1] == '+':
			panic(fmt.Sprintf("approval pattern keeps a possessive quantifier: %q", src))
		case vm && cur() && (c == 'i' || c == 'I'):
			b.WriteString("[iIıİ]")
			i++
		default:
			w := runeWidth(src, i)
			b.WriteString(src[i : i+w])
			i += w
		}
	}
	return b.String()
}

func translateEscape(b *strings.Builder, src string, i int, vm, inClass bool) int {
	if i+1 >= len(src) {
		panic(fmt.Sprintf("approval pattern ends in a backslash: %q", src))
	}
	c := src[i+1]
	switch c {
	case 's':
		switch {
		case inClass && vm:
			b.WriteString(pySpaceClass)
		case vm:
			b.WriteString("[" + pySpaceClass + "]")
		default:
			b.WriteString(`\s`)
		}
	case 'S':
		switch {
		case inClass:
			panic(fmt.Sprintf("approval pattern negates whitespace inside a class: %q", src))
		case vm:
			b.WriteString("[^" + pySpaceClass + "]")
		default:
			b.WriteString(`\S`)
		}
	case 'w':
		switch {
		case inClass && vm:
			b.WriteString(`\pL\pN_`)
		case inClass:
			b.WriteString(`\w\pL\pN`)
		case vm:
			b.WriteString(`[\pL\pN_]`)
		default:
			b.WriteString(`[\w\pL\pN]`)
		}
	case 'W':
		if inClass {
			panic(fmt.Sprintf("approval pattern negates words inside a class: %q", src))
		}
		if vm {
			b.WriteString(`[^\pL\pN_]`)
		} else {
			b.WriteString(`[^\w\pL\pN]`)
		}
	case 'd':
		switch {
		case vm:
			b.WriteString(`\p{Nd}`)
		case inClass:
			panic(fmt.Sprintf("approval pattern puts a digit escape inside a class: %q", src))
		default:
			b.WriteString(`(?:[0-9]|_\p{Nd}_)`)
		}
	case 'b':
		if inClass {
			b.WriteString(`\x08`)
		} else if vm {
			b.WriteString(ph(asWordBoundary))
		} else {
			b.WriteString(`\b`)
		}
	case 'B':
		if vm {
			b.WriteString(ph(asNotWordBoundary))
		} else {
			b.WriteString(`\B`)
		}
	case 'Z':
		b.WriteString(`\z`)
	case 'A':
		b.WriteString(`\A`)
	case 'u', 'U', 'x':
		n := map[byte]int{'u': 4, 'U': 8, 'x': 2}[c]
		b.WriteString(`\x{` + src[i+2:i+2+n] + `}`)
		return i + 2 + n
	case 'n', 'r', 't', 'f', 'v':
		b.WriteString(src[i : i+2])
	default:
		if c < utf8.RuneSelf && (unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))) {
			panic(fmt.Sprintf("approval pattern escape \\%c has no counterpart: %q", c, src))
		}
		b.WriteByte('\\')
		b.WriteByte(c)
	}
	return i + 2
}

func translateClass(b *strings.Builder, src string, i int, vm, expand bool) int {
	b.WriteByte('[')
	j := i + 1
	if j < len(src) && src[j] == '^' {
		b.WriteByte('^')
		j++
	}
	if j < len(src) && src[j] == ']' {
		b.WriteString(`\]`)
		j++
	}
	var lits []rune
	hasI := false
	for j < len(src) && src[j] != ']' {
		if src[j] == '\\' {
			var inner strings.Builder
			next := translateEscape(&inner, src, j, vm, true)
			b.WriteString(inner.String())
			if next == j+2 && !unicode.IsLetter(rune(src[j+1])) {
				lits = append(lits, rune(src[j+1]))
			} else {
				lits = append(lits, -1)
			}
			j = next
			continue
		}
		r, w := utf8.DecodeRuneInString(src[j:])
		if r == '-' && len(lits) > 0 && lits[len(lits)-1] >= 0 && j+1 < len(src) && src[j+1] != ']' {
			lo := lits[len(lits)-1]
			hi, hw := utf8.DecodeRuneInString(src[j+1:])
			if src[j+1] == '\\' {
				hi = rune(src[j+2])
			}
			if (lo <= 'i' && 'i' <= hi) || (lo <= 'I' && 'I' <= hi) {
				hasI = true
			}
			b.WriteByte('-')
			if src[j+1] == '\\' {
				b.WriteString(src[j+1 : j+3])
				j += 3
			} else {
				b.WriteString(src[j+1 : j+1+hw])
				j += 1 + hw
			}
			lits = append(lits, -1)
			continue
		}
		if r == 'i' || r == 'I' {
			hasI = true
		}
		b.WriteString(src[j : j+w])
		lits = append(lits, r)
		j += w
	}
	if expand && hasI {
		b.WriteString("ıİ")
	}
	b.WriteByte(']')
	return j + 1
}

func legacyKey(src string) string {
	if k := strings.Index(src, `\b`); k >= 0 {
		rest := src[k+2:]
		if e := strings.Index(rest, `\b`); e >= 0 {
			return rest[:e]
		}
		return rest
	}
	rs := []rune(src)
	if len(rs) > 20 {
		rs = rs[:20]
	}
	return string(rs)
}
