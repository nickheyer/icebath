package pyre

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyunicode"
)

// Error is re.error with the CPython message and code point position
type Error struct {
	Msg     string
	Pattern string
	Pos     int
	HasPos  bool
}

// Message as str(re.error) renders it, with line and column when the pattern spans lines
func (e *Error) Error() string {
	if !e.HasPos {
		return e.Msg
	}
	msg := fmt.Sprintf("%s at position %d", e.Msg, e.Pos)
	src := []rune(e.Pattern)
	if strings.ContainsRune(e.Pattern, '\n') {
		line, last := 1, -1
		for i := 0; i < e.Pos && i < len(src); i++ {
			if src[i] == '\n' {
				line++
				last = i
			}
		}
		msg = fmt.Sprintf("%s (line %d, column %d)", msg, line, e.Pos-last)
	}
	return msg
}

// OverflowError is the OverflowError re.compile raises for a repeat count at or above MAXREPEAT
type OverflowError struct{ Msg string }

func (e *OverflowError) Error() string { return e.Msg }

const maxRepeat int64 = 4294967295

const (
	wordASCII  = `a-zA-Z0-9_`
	spaceASCII = ` \t\n\r\f\v`
)

type width struct{ lo, hi int64 }

func satAdd(a, b int64) int64 {
	if a+b > maxRepeat {
		return maxRepeat
	}
	return a + b
}

func satMul(a, b int64) int64 {
	if a != 0 && b > maxRepeat/a {
		return maxRepeat
	}
	return a * b
}

type kind int

const (
	kAtom kind = iota
	kAt
	kRepeat
)

type item struct {
	s      string
	w      width
	kind   kind
	global bool
}

type flags struct {
	ascii, verbose bool
}

type parser struct {
	src          []rune
	pattern      string
	i            int
	groups       int
	names        map[string]int
	open         map[int]bool
	widths       map[int]width
	condRefs     map[int]int
	lookbehindAt int
}

// Translates a Python re pattern into regexp2 syntax, failing where re.compile fails
func Translate(pattern string) (string, error) {
	p := &parser{src: []rune(pattern), pattern: pattern, names: map[string]int{}, open: map[int]bool{}, widths: map[int]width{}, condRefs: map[int]int{}, lookbehindAt: -1}
	fl := flags{}
	out, _, err := p.parseSub(&fl, 0)
	if err != nil {
		return "", err
	}
	if p.i < len(p.src) {
		return "", p.err("unbalanced parenthesis", 0)
	}
	for g, pos := range p.condRefs {
		if g > p.groups {
			return "", &Error{Msg: fmt.Sprintf("invalid group reference %d", g), Pattern: pattern, Pos: pos, HasPos: true}
		}
	}
	return out, nil
}

// Compiles a Python re pattern with Python matching semantics on the regexp2 engine
func Compile(pattern string) (*regexp2.Regexp, error) {
	out, err := Translate(pattern)
	if err != nil {
		return nil, err
	}
	re, err := regexp2.Compile(out, regexp2.None)
	if err != nil {
		return nil, &Error{Msg: err.Error(), Pattern: pattern}
	}
	return re, nil
}

func (p *parser) err(msg string, offset int) *Error {
	return &Error{Msg: msg, Pattern: p.pattern, Pos: p.i - offset, HasPos: true}
}

func (p *parser) peek() (rune, bool) {
	if p.i < len(p.src) {
		return p.src[p.i], true
	}
	return 0, false
}

func (p *parser) match(r rune) bool {
	if c, ok := p.peek(); ok && c == r {
		p.i++
		return true
	}
	return false
}

// Next token, a backslash escape counting as one two rune token
func (p *parser) get() (string, bool, error) {
	if p.i >= len(p.src) {
		return "", false, nil
	}
	c := p.src[p.i]
	if c == '\\' {
		if p.i+1 >= len(p.src) {
			return "", false, &Error{Msg: "bad escape (end of pattern)", Pattern: p.pattern, Pos: p.i, HasPos: true}
		}
		p.i += 2
		return string(p.src[p.i-2 : p.i]), true, nil
	}
	p.i++
	return string(c), true, nil
}

func (p *parser) getUntil(term rune, name string) (string, error) {
	start := p.i
	for {
		c, ok := p.peek()
		if !ok {
			if p.i == start {
				return "", p.err("missing "+name, 0)
			}
			return "", p.err(fmt.Sprintf("missing %c, unterminated name", term), p.i-start)
		}
		if c == term {
			if p.i == start {
				p.i++
				return "", p.err("missing "+name, 1)
			}
			p.i++
			return string(p.src[start : p.i-1]), nil
		}
		p.i++
	}
}

func (p *parser) parseSub(fl *flags, nested int) (string, width, error) {
	var branches []string
	w := width{lo: maxRepeat, hi: 0}
	for {
		s, bw, err := p.parseSeq(fl, nested, nested == 0 && len(branches) == 0)
		if err != nil {
			return "", w, err
		}
		branches = append(branches, s)
		w.lo, w.hi = min(w.lo, bw.lo), max(w.hi, bw.hi)
		if !p.match('|') {
			break
		}
	}
	return strings.Join(branches, "|"), w, nil
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }
func isOct(r rune) bool   { return r >= '0' && r <= '7' }
func isHex(r rune) bool {
	return isDigit(r) || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}
func isASCIILetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

func literal(r rune) string {
	if strings.ContainsRune(`\.^$|?*+()[]{}#`, r) {
		return `\` + string(r)
	}
	return string(r)
}

func classLiteral(r rune) string {
	if strings.ContainsRune(`\]-[^`, r) {
		return `\` + string(r)
	}
	return string(r)
}

func (p *parser) parseSeq(fl *flags, nested int, first bool) (string, width, error) {
	var items []item
	real := 0
	for {
		real = len(items)
		if real > 0 && items[0].global {
			real = 0
			for _, it := range items {
				if !it.global {
					real++
				}
			}
		}
		c, ok := p.peek()
		if !ok || c == '|' || c == ')' {
			break
		}
		if fl.verbose && pystr.IsSpace(c) {
			p.i++
			continue
		}
		if fl.verbose && c == '#' {
			for p.i < len(p.src) && p.src[p.i] != '\n' {
				p.i++
			}
			continue
		}
		this, _, err := p.get()
		if err != nil {
			return "", width{}, err
		}
		switch {
		case this[0] == '\\':
			it, err := p.escape(fl, this)
			if err != nil {
				return "", width{}, err
			}
			items = append(items, it)
		case this == "[":
			it, err := p.class(fl)
			if err != nil {
				return "", width{}, err
			}
			items = append(items, it)
		case this == ".":
			items = append(items, item{".", width{1, 1}, kAtom, false})
		case this == "^" || this == "$":
			items = append(items, item{this, width{}, kAt, false})
		case strings.Contains("*+?{", this):
			if err := p.repeat(this, &items); err != nil {
				return "", width{}, err
			}
		case this == "(":
			it, skip, err := p.group(fl, nested, first && real == 0)
			if err != nil {
				return "", width{}, err
			}
			if !skip {
				items = append(items, it)
			}
		default:
			r := []rune(this)[0]
			items = append(items, item{literal(r), width{1, 1}, kAtom, false})
		}
	}
	var b strings.Builder
	w := width{}
	for _, it := range items {
		b.WriteString(it.s)
		w.lo, w.hi = satAdd(w.lo, it.w.lo), satAdd(w.hi, it.w.hi)
	}
	return b.String(), w, nil
}

// Applies a quantifier to the last item or reports the CPython repeat error
func (p *parser) repeat(this string, items *[]item) error {
	here := p.i
	lo, hi := int64(0), maxRepeat
	quant := this
	switch this {
	case "+":
		lo = 1
	case "?":
		hi = 1
	case "{":
		if c, ok := p.peek(); ok && c == '}' {
			*items = append(*items, item{`\{`, width{1, 1}, kAtom, false})
			return nil
		}
		var los, his string
		for c, ok := p.peek(); ok && isDigit(c); c, ok = p.peek() {
			los += string(c)
			p.i++
		}
		if p.match(',') {
			for c, ok := p.peek(); ok && isDigit(c); c, ok = p.peek() {
				his += string(c)
				p.i++
			}
		} else {
			his = los
		}
		if !p.match('}') {
			*items = append(*items, item{`\{`, width{1, 1}, kAtom, false})
			p.i = here
			return nil
		}
		if los != "" {
			v, err := strconv.ParseInt(los, 10, 64)
			if err != nil || v >= maxRepeat {
				return &OverflowError{Msg: "the repetition number is too large"}
			}
			lo = v
		}
		if his != "" {
			v, err := strconv.ParseInt(his, 10, 64)
			if err != nil || v >= maxRepeat {
				return &OverflowError{Msg: "the repetition number is too large"}
			}
			hi = v
			if hi < lo {
				return p.err("min repeat greater than max repeat", p.i-here)
			}
		}
		switch {
		case hi == maxRepeat:
			quant = fmt.Sprintf("{%d,}", lo)
		case lo == hi:
			quant = fmt.Sprintf("{%d}", lo)
		default:
			quant = fmt.Sprintf("{%d,%d}", lo, hi)
		}
	}
	n := len(*items)
	if n == 0 || (*items)[n-1].kind == kAt {
		return p.err("nothing to repeat", p.i-here+len(this))
	}
	if (*items)[n-1].kind == kRepeat {
		return p.err("multiple repeat", p.i-here+len(this))
	}
	last := (*items)[n-1]
	s := last.s + quant
	if p.match('?') {
		s += "?"
	} else if p.match('+') {
		s = "(?>" + s + ")"
	}
	(*items)[n-1] = item{s, width{satMul(last.w.lo, lo), satMul(last.w.hi, hi)}, kRepeat, false}
	return nil
}

func (p *parser) checkGroupRef(g int, offset int) error {
	if p.open[g] {
		return p.err("cannot refer to an open group", offset)
	}
	if p.lookbehindAt >= 0 && g > p.lookbehindAt {
		return p.err("cannot refer to group defined in the same lookbehind subpattern", offset)
	}
	return nil
}

func categoryClass(c rune, ascii bool) (pos string, neg bool) {
	switch c {
	case 'd', 'D':
		pos = `\d`
		if ascii {
			pos = `0-9`
		}
	case 's', 'S':
		pos = `\s`
		if ascii {
			pos = spaceASCII
		}
	case 'w', 'W':
		pos = `\w`
		if ascii {
			pos = wordASCII
		}
	}
	return pos, c == 'D' || c == 'S' || c == 'W'
}

// Parses the escape forms shared by the class and the top level, returning a literal rune
func (p *parser) numericEscape(esc string, inClass bool) (rune, bool, error) {
	c := []rune(esc)[1]
	readHex := func(n int) (rune, error) {
		start := p.i
		for k := 0; k < n; k++ {
			if r, ok := p.peek(); ok && isHex(r) {
				p.i++
				continue
			}
			text := esc + string(p.src[start:p.i])
			return 0, p.err("incomplete escape "+text, len([]rune(text)))
		}
		v, _ := strconv.ParseUint(string(p.src[start:p.i]), 16, 32)
		return rune(v), nil
	}
	switch c {
	case 'x':
		r, err := readHex(2)
		return r, true, err
	case 'u':
		r, err := readHex(4)
		return r, true, err
	case 'U':
		start := p.i
		r, err := readHex(8)
		if err == nil && r > 0x10FFFF {
			text := esc + string(p.src[start:p.i])
			return 0, true, p.err("bad escape "+text, len([]rune(text)))
		}
		return r, true, err
	case 'N':
		if !p.match('{') {
			return 0, true, p.err("missing {", 0)
		}
		start := p.i
		name, err := p.getUntil('}', "character name")
		if err != nil {
			return 0, true, err
		}
		r, ok := pyunicode.Lookup(name)
		if !ok {
			return 0, true, p.err(fmt.Sprintf("undefined character name %s", pyQuote(name)), p.i-start+3)
		}
		return r, true, nil
	}
	if inClass && isOct(c) {
		start := p.i
		for k := 0; k < 2; k++ {
			if r, ok := p.peek(); ok && isOct(r) {
				p.i++
			}
		}
		text := esc + string(p.src[start:p.i])
		v, _ := strconv.ParseUint(text[1:], 8, 32)
		if v > 0o377 {
			return 0, true, p.err(fmt.Sprintf("octal escape value %s outside of range 0-0o377", text), len([]rune(text)))
		}
		return rune(v), true, nil
	}
	return 0, false, nil
}

func pyQuote(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

func (p *parser) escape(fl *flags, esc string) (item, error) {
	c := []rune(esc)[1]
	switch c {
	case 'A':
		return item{`\A`, width{}, kAt, false}, nil
	case 'Z':
		return item{`\z`, width{}, kAt, false}, nil
	case 'b', 'B':
		if !fl.ascii {
			return item{esc, width{}, kAt, false}, nil
		}
		w := "[" + wordASCII + "]"
		if c == 'b' {
			return item{"(?:(?<=" + w + ")(?!" + w + ")|(?<!" + w + ")(?=" + w + "))", width{}, kAt, false}, nil
		}
		return item{"(?:(?<=" + w + ")(?=" + w + ")|(?<!" + w + ")(?!" + w + "))", width{}, kAt, false}, nil
	case 'd', 'D', 's', 'S', 'w', 'W':
		if !fl.ascii {
			return item{esc, width{1, 1}, kAtom, false}, nil
		}
		pos, neg := categoryClass(c, true)
		if neg {
			return item{"[^" + pos + "]", width{1, 1}, kAtom, false}, nil
		}
		return item{"[" + pos + "]", width{1, 1}, kAtom, false}, nil
	case 'a', 'f', 'n', 'r', 't', 'v':
		return item{esc, width{1, 1}, kAtom, false}, nil
	}
	if r, ok, err := p.numericEscape(esc, false); ok || err != nil {
		return item{literal(r), width{1, 1}, kAtom, false}, err
	}
	if c == '0' {
		start := p.i
		for k := 0; k < 2; k++ {
			if r, ok := p.peek(); ok && isOct(r) {
				p.i++
			}
		}
		v, _ := strconv.ParseUint("0"+string(p.src[start:p.i]), 8, 32)
		return item{literal(rune(v)), width{1, 1}, kAtom, false}, nil
	}
	if isDigit(c) {
		text := esc
		if r, ok := p.peek(); ok && isDigit(r) {
			p.i++
			text += string(r)
			if r2, ok2 := p.peek(); ok2 && isOct(c) && isOct(r) && isOct(r2) {
				p.i++
				text += string(r2)
				v, _ := strconv.ParseUint(text[1:], 8, 32)
				if v > 0o377 {
					return item{}, p.err(fmt.Sprintf("octal escape value %s outside of range 0-0o377", text), len(text))
				}
				return item{literal(rune(v)), width{1, 1}, kAtom, false}, nil
			}
		}
		g, _ := strconv.Atoi(text[1:])
		if g <= p.groups {
			if err := p.checkGroupRef(g, len(text)); err != nil {
				return item{}, err
			}
			return item{fmt.Sprintf(`(?:\%d)`, g), p.widths[g], kAtom, false}, nil
		}
		return item{}, p.err(fmt.Sprintf("invalid group reference %d", g), len(text)-1)
	}
	if isASCIILetter(c) {
		return item{}, p.err("bad escape "+esc, len(esc))
	}
	return item{literal(c), width{1, 1}, kAtom, false}, nil
}

type classMember struct {
	lo, hi rune
	cat    rune
}

func (p *parser) classEscape(fl *flags, esc string) (classMember, error) {
	c := []rune(esc)[1]
	switch c {
	case 'a':
		return classMember{lo: 7, hi: 7}, nil
	case 'b':
		return classMember{lo: 8, hi: 8}, nil
	case 'f':
		return classMember{lo: 12, hi: 12}, nil
	case 'n':
		return classMember{lo: 10, hi: 10}, nil
	case 'r':
		return classMember{lo: 13, hi: 13}, nil
	case 't':
		return classMember{lo: 9, hi: 9}, nil
	case 'v':
		return classMember{lo: 11, hi: 11}, nil
	case 'd', 'D', 's', 'S', 'w', 'W':
		return classMember{cat: c}, nil
	}
	if r, ok, err := p.numericEscape(esc, true); ok || err != nil {
		return classMember{lo: r, hi: r}, err
	}
	if isDigit(c) || isASCIILetter(c) {
		return classMember{}, p.err("bad escape "+esc, len(esc))
	}
	return classMember{lo: c, hi: c}, nil
}

// Parses a character class into an explicit regexp2 class or alternation
func (p *parser) class(fl *flags) (item, error) {
	here := p.i - 1
	var set []classMember
	negate := p.match('^')
	for {
		this, ok, err := p.get()
		if err != nil {
			return item{}, err
		}
		if !ok {
			return item{}, p.err("unterminated character set", p.i-here)
		}
		if this == "]" && len(set) > 0 {
			break
		}
		var code1 classMember
		if this[0] == '\\' {
			if code1, err = p.classEscape(fl, this); err != nil {
				return item{}, err
			}
		} else {
			r := []rune(this)[0]
			code1 = classMember{lo: r, hi: r}
		}
		if !p.match('-') {
			set = append(set, code1)
			continue
		}
		that, ok, err := p.get()
		if err != nil {
			return item{}, err
		}
		if !ok {
			return item{}, p.err("unterminated character set", p.i-here)
		}
		if that == "]" {
			set = append(set, code1, classMember{lo: '-', hi: '-'})
			break
		}
		var code2 classMember
		if that[0] == '\\' {
			if code2, err = p.classEscape(fl, that); err != nil {
				return item{}, err
			}
		} else {
			r := []rune(that)[0]
			code2 = classMember{lo: r, hi: r}
		}
		msg := fmt.Sprintf("bad character range %s-%s", this, that)
		if code1.cat != 0 || code2.cat != 0 || code2.lo < code1.lo {
			return item{}, p.err(msg, len([]rune(this))+1+len([]rune(that)))
		}
		set = append(set, classMember{lo: code1.lo, hi: code2.lo})
	}
	var pos strings.Builder
	var negCats []string
	for _, m := range set {
		switch {
		case m.cat != 0 && fl.ascii:
			cls, neg := categoryClass(m.cat, true)
			if neg {
				negCats = append(negCats, "[^"+cls+"]")
			} else {
				pos.WriteString(cls)
			}
		case m.cat != 0:
			pos.WriteString(`\` + string(m.cat))
		case m.lo == m.hi:
			pos.WriteString(classLiteral(m.lo))
		default:
			pos.WriteString(classLiteral(m.lo) + "-" + classLiteral(m.hi))
		}
	}
	if len(negCats) == 0 {
		if negate {
			return item{"[^" + pos.String() + "]", width{1, 1}, kAtom, false}, nil
		}
		return item{"[" + pos.String() + "]", width{1, 1}, kAtom, false}, nil
	}
	alts := negCats
	if pos.Len() > 0 {
		alts = append([]string{"[" + pos.String() + "]"}, negCats...)
	}
	union := "(?:" + strings.Join(alts, "|") + ")"
	if negate {
		return item{"(?:(?!" + union + `)[\s\S])`, width{1, 1}, kAtom, false}, nil
	}
	return item{union, width{1, 1}, kAtom, false}, nil
}

// Parses a parenthesised construct, skip true when it emits nothing such as a comment or global flags
func (p *parser) group(fl *flags, nested int, first bool) (item, bool, error) {
	start := p.i - 1
	capture := true
	name := ""
	if p.match('?') {
		char, ok, err := p.get()
		if err != nil {
			return item{}, false, err
		}
		if !ok {
			return item{}, false, p.err("unexpected end of pattern", 0)
		}
		switch {
		case char == "P":
			if p.match('<') {
				n, err := p.getUntil('>', "group name")
				if err != nil {
					return item{}, false, err
				}
				if !pystr.IsIdentifier(n) {
					return item{}, false, p.err(fmt.Sprintf("bad character in group name %s", pyQuote(n)), len([]rune(n))+1)
				}
				name = n
			} else if p.match('=') {
				n, err := p.getUntil(')', "group name")
				if err != nil {
					return item{}, false, err
				}
				if !pystr.IsIdentifier(n) {
					return item{}, false, p.err(fmt.Sprintf("bad character in group name %s", pyQuote(n)), len([]rune(n))+1)
				}
				g, known := p.names[n]
				if !known {
					return item{}, false, p.err(fmt.Sprintf("unknown group name %s", pyQuote(n)), len([]rune(n))+1)
				}
				if err := p.checkGroupRef(g, len([]rune(n))+1); err != nil {
					return item{}, false, err
				}
				return item{fmt.Sprintf(`(?:\%d)`, g), p.widths[g], kAtom, false}, false, nil
			} else {
				c2, ok, err := p.get()
				if err != nil {
					return item{}, false, err
				}
				if !ok {
					return item{}, false, p.err("unexpected end of pattern", 0)
				}
				return item{}, false, p.err("unknown extension ?P"+c2, len([]rune(c2))+2)
			}
		case char == ":":
			capture = false
		case char == "#":
			for {
				tok, ok, err := p.get()
				if err != nil {
					return item{}, false, err
				}
				if !ok {
					return item{}, false, &Error{Msg: "missing ), unterminated comment", Pattern: p.pattern, Pos: start, HasPos: true}
				}
				if tok == ")" {
					return item{}, true, nil
				}
			}
		case char == "=" || char == "!" || char == "<":
			dir := 1
			if char == "<" {
				c2, ok, err := p.get()
				if err != nil {
					return item{}, false, err
				}
				if !ok {
					return item{}, false, p.err("unexpected end of pattern", 0)
				}
				if c2 != "=" && c2 != "!" {
					return item{}, false, p.err("unknown extension ?<"+c2, len([]rune(c2))+2)
				}
				dir, char = -1, c2
			}
			saved := p.lookbehindAt
			if dir < 0 && p.lookbehindAt < 0 {
				p.lookbehindAt = p.groups
			}
			sub, w, err := p.parseSub(fl, nested+1)
			p.lookbehindAt = saved
			if err != nil {
				return item{}, false, err
			}
			if !p.match(')') {
				return item{}, false, &Error{Msg: "missing ), unterminated subpattern", Pattern: p.pattern, Pos: start, HasPos: true}
			}
			if dir < 0 && w.lo != w.hi {
				return item{}, false, &Error{Msg: "look-behind requires fixed-width pattern"}
			}
			head := "(?" + char
			if dir < 0 {
				head = "(?<" + char
			}
			return item{head + sub + ")", width{}, kAtom, false}, false, nil
		case char == "(":
			return p.conditional(fl, nested, start)
		case char == ">":
			sub, w, err := p.parseSub(fl, nested+1)
			if err != nil {
				return item{}, false, err
			}
			if !p.match(')') {
				return item{}, false, &Error{Msg: "missing ), unterminated subpattern", Pattern: p.pattern, Pos: start, HasPos: true}
			}
			return item{"(?>" + sub + ")", w, kAtom, false}, false, nil
		case strings.Contains("aiLmsux-", char):
			return p.inlineFlags(fl, nested, first, start, []rune(char)[0])
		default:
			return item{}, false, p.err("unknown extension ?"+char, len([]rune(char))+1)
		}
	}
	g := 0
	if capture {
		p.groups++
		g = p.groups
		if name != "" {
			if old, dup := p.names[name]; dup {
				return item{}, false, p.err(fmt.Sprintf("redefinition of group name %s as group %d; was group %d", pyQuote(name), g, old), len([]rune(name))+1)
			}
			p.names[name] = g
		}
		p.open[g] = true
	}
	sub, w, err := p.parseSub(fl, nested+1)
	if err != nil {
		return item{}, false, err
	}
	if !p.match(')') {
		return item{}, false, &Error{Msg: "missing ), unterminated subpattern", Pattern: p.pattern, Pos: start, HasPos: true}
	}
	if !capture {
		return item{"(?:" + sub + ")", w, kAtom, false}, false, nil
	}
	delete(p.open, g)
	p.widths[g] = w
	return item{"(" + sub + ")", w, kAtom, false}, false, nil
}

func (p *parser) conditional(fl *flags, nested, start int) (item, bool, error) {
	condName, err := p.getUntil(')', "group name")
	if err != nil {
		return item{}, false, err
	}
	nameLen := len([]rune(condName)) + 1
	var g int
	if pystr.IsIdentifier(condName) {
		known := false
		if g, known = p.names[condName]; !known {
			return item{}, false, p.err(fmt.Sprintf("unknown group name %s", pyQuote(condName)), nameLen)
		}
	} else {
		ok := condName != ""
		for _, r := range condName {
			ok = ok && isDigit(r)
		}
		v, perr := strconv.Atoi(condName)
		if !ok || perr != nil {
			return item{}, false, p.err(fmt.Sprintf("bad character in group name %s", pyQuote(condName)), nameLen)
		}
		if v == 0 {
			return item{}, false, p.err("bad group number", nameLen)
		}
		if v >= 1<<30 {
			return item{}, false, p.err(fmt.Sprintf("invalid group reference %d", v), nameLen)
		}
		g = v
		if _, seen := p.condRefs[g]; !seen {
			p.condRefs[g] = p.i - nameLen
		}
	}
	if p.lookbehindAt >= 0 {
		if err := p.checkGroupRef(g, nameLen); err != nil {
			return item{}, false, err
		}
	}
	yes, yw, err := p.parseSeq(fl, nested+1, false)
	if err != nil {
		return item{}, false, err
	}
	no, nw, hasNo := "", width{}, false
	if p.match('|') {
		hasNo = true
		if no, nw, err = p.parseSeq(fl, nested+1, false); err != nil {
			return item{}, false, err
		}
		if c, ok := p.peek(); ok && c == '|' {
			return item{}, false, p.err("conditional backref with more than two branches", 0)
		}
	}
	if !p.match(')') {
		return item{}, false, &Error{Msg: "missing ), unterminated subpattern", Pattern: p.pattern, Pos: start, HasPos: true}
	}
	w := yw
	if hasNo {
		w.lo, w.hi = min(w.lo, nw.lo), max(w.hi, nw.hi)
	} else {
		w.lo = 0
	}
	return item{fmt.Sprintf("(?(%d)%s|%s)", g, yes, no), w, kAtom, false}, false, nil
}

// Parses an inline flag group, global at the start or scoped with a colon
func (p *parser) inlineFlags(fl *flags, nested int, first bool, start int, char rune) (item, bool, error) {
	add, del := map[rune]bool{}, map[rune]bool{}
	missing := func(msg string, c rune) error {
		if pystr.IsAlpha(c) {
			return p.err("unknown flag", 1)
		}
		return p.err(msg, 1)
	}
	if char != '-' {
		for {
			if char == 'L' {
				return item{}, false, p.err("bad inline flag: cannot use 'L' flag with a str pattern", 0)
			}
			add[char] = true
			if add['a'] && add['u'] {
				return item{}, false, p.err("bad inline flag: flags 'a', 'u' and 'L' are incompatible", 0)
			}
			c, ok := p.peek()
			if !ok {
				return item{}, false, p.err("missing -, : or )", 0)
			}
			p.i++
			char = c
			if strings.ContainsRune(")-:", char) {
				break
			}
			if !strings.ContainsRune("aiLmsux", char) {
				return item{}, false, missing("missing -, : or )", char)
			}
		}
	}
	if char == ')' {
		if !first {
			return item{}, false, p.err("global flags not at the start of the expression", p.i-start)
		}
		if add['a'] {
			fl.ascii = true
		}
		if add['x'] {
			fl.verbose = true
		}
		return item{netFlags(add, nil, ")"), width{}, kAt, true}, !(add['i'] || add['m'] || add['s']), nil
	}
	if char == '-' {
		c, ok := p.peek()
		if !ok {
			return item{}, false, p.err("missing flag", 0)
		}
		p.i++
		if !strings.ContainsRune("aiLmsux", c) {
			return item{}, false, missing("missing flag", c)
		}
		char = c
		for {
			if strings.ContainsRune("auL", char) {
				return item{}, false, p.err("bad inline flag: cannot turn off flags 'a', 'u' and 'L'", 0)
			}
			del[char] = true
			c, ok := p.peek()
			if !ok {
				return item{}, false, p.err("missing :", 0)
			}
			p.i++
			char = c
			if char == ':' {
				break
			}
			if !strings.ContainsRune("aiLmsux", char) {
				return item{}, false, missing("missing :", char)
			}
		}
	}
	for f := range add {
		if del[f] {
			return item{}, false, p.err("bad inline flag: flag turned on and off", 1)
		}
	}
	scoped := *fl
	if add['a'] {
		scoped.ascii = true
	}
	if add['x'] {
		scoped.verbose = true
	}
	if del['x'] {
		scoped.verbose = false
	}
	sub, w, err := p.parseSub(&scoped, nested+1)
	if err != nil {
		return item{}, false, err
	}
	if !p.match(')') {
		return item{}, false, &Error{Msg: "missing ), unterminated subpattern", Pattern: p.pattern, Pos: start, HasPos: true}
	}
	return item{netFlags(add, del, ":") + sub + ")", w, kAtom, false}, false, nil
}

// regexp2 inline flag group head keeping only the i, m and s flags it shares with Python
func netFlags(add, del map[rune]bool, tail string) string {
	pick := func(m map[rune]bool) string {
		s := ""
		for _, f := range "ims" {
			if m[f] {
				s += string(f)
			}
		}
		return s
	}
	on, off := pick(add), pick(del)
	if off != "" {
		off = "-" + off
	}
	return "(?" + on + off + tail
}
