package regexutil

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2/syntax"
	unicodeutil "github.com/nickheyer/icebath/pkg/utils/unicode"
)

// Class bodies of the word characters, digits and white space a scope under the a flag keeps to
const (
	asciiWord  = `0-9A-Za-z_`
	asciiDigit = `0-9`
	asciiSpace = `\t\n\v\f\r\x{20}`
)

// Flags of the scope a part of the pattern sits in that the rewriting itself has to follow
type scope struct{ verbose, ascii bool }

// Group still open: where its text starts in the output and the scope outside it
type frame struct {
	start int
	outer scope
}

// Pattern rewritten from the package dialect into the syntax regexp2 reads in its default mode
type translator struct {
	src        []rune
	i          int
	out        []byte
	open       []frame
	scope      scope
	atom       int
	quantified bool
}

// Pattern in the syntax regexp2's default mode reads, the dialect's spellings it lacks rewritten: (?P<name>...) and (?P=name), \Z as the very end, {,n}, possessive quantifiers, \UXXXXXXXX and \N{NAME}, escaped punctuation and non-ASCII characters as themselves, a literal [ inside a class, comments and free spacing removed, and the a flag spelled out as ASCII classes
func translate(pattern string) (string, error) {
	t := &translator{src: []rune(pattern), out: make([]byte, 0, len(pattern)+8), atom: -1}
	for t.i < len(t.src) {
		if err := t.item(); err != nil {
			return "", err
		}
	}
	if len(t.open) > 0 {
		return "", t.fail("missing ), unterminated subpattern")
	}
	return string(t.out), nil
}

func (t *translator) fail(msg string) error {
	return &syntax.Error{Code: syntax.ErrorCode(msg), Expr: string(t.src)}
}

func (t *translator) peek(r rune) bool { return t.i < len(t.src) && t.src[t.i] == r }

func (t *translator) emit(s string) { t.out = append(t.out, s...) }

func (t *translator) emitRunes(rs []rune) {
	for _, r := range rs {
		t.out = utf8.AppendRune(t.out, r)
	}
}

// Marks the output position where an item a quantifier may follow starts
func (t *translator) begin() {
	t.atom = len(t.out)
	t.quantified = false
}

// Marks a zero width item, after which a quantifier has nothing to repeat
func (t *translator) anchor() {
	t.atom = -1
	t.quantified = false
}

func (t *translator) item() error {
	c := t.src[t.i]
	if t.scope.verbose {
		if unicode.IsSpace(c) {
			t.i++
			return nil
		}
		if c == '#' {
			for t.i < len(t.src) && t.src[t.i] != '\n' {
				t.i++
			}
			return nil
		}
	}
	switch c {
	case '\\':
		return t.escape()
	case '[':
		return t.class()
	case '(':
		return t.group()
	case ')':
		return t.close()
	case '*', '+', '?':
		t.i++
		return t.repeat(string(c))
	case '{':
		q, ok, err := t.braces()
		if err != nil {
			return err
		}
		if ok {
			return t.repeat(q)
		}
		t.i++
		t.begin()
		t.emit(`\{`)
		return nil
	case '|', '^', '$':
		t.i++
		t.anchor()
		t.out = utf8.AppendRune(t.out, c)
		return nil
	}
	t.i++
	t.begin()
	t.out = utf8.AppendRune(t.out, c)
	return nil
}

// Writes quantifier q after the last item, a following ? making it lazy and a following + making it possessive as an atomic group
func (t *translator) repeat(q string) error {
	if t.quantified {
		return t.fail("multiple repeat")
	}
	if t.atom < 0 {
		return t.fail("nothing to repeat")
	}
	start := t.atom
	t.emit(q)
	switch {
	case t.peek('?'):
		t.i++
		t.emit("?")
	case t.peek('+'):
		t.i++
		t.out = slices.Insert(t.out, start, []byte("(?>")...)
		t.emit(")")
	}
	t.quantified = true
	return nil
}

// Repeat count at a brace, {,m} and {,} counting from zero; false when the brace is a literal one
func (t *translator) braces() (string, bool, error) {
	j := t.i + 1
	lo := t.digits(&j)
	comma := j < len(t.src) && t.src[j] == ','
	hi := ""
	if comma {
		j++
		hi = t.digits(&j)
	}
	if j >= len(t.src) || t.src[j] != '}' || (lo == "" && !comma) {
		return "", false, nil
	}
	t.i = j + 1
	if lo == "" {
		lo = "0"
	}
	if !comma {
		return "{" + lo + "}", true, nil
	}
	if hi != "" {
		l, lerr := strconv.ParseUint(lo, 10, 64)
		h, herr := strconv.ParseUint(hi, 10, 64)
		if lerr == nil && herr == nil && h < l {
			return "", false, t.fail("min repeat greater than max repeat")
		}
	}
	return "{" + lo + "," + hi + "}", true, nil
}

func (t *translator) digits(j *int) string {
	start := *j
	for *j < len(t.src) && isDigit(t.src[*j]) {
		*j++
	}
	return string(t.src[start:*j])
}

func (t *translator) push(start int) {
	t.open = append(t.open, frame{start: start, outer: t.scope})
	t.anchor()
}

func (t *translator) close() error {
	t.i++
	if len(t.open) == 0 {
		return t.fail("unbalanced parenthesis")
	}
	f := t.open[len(t.open)-1]
	t.open = t.open[:len(t.open)-1]
	t.scope = f.outer
	t.emit(")")
	t.atom = f.start
	t.quantified = false
	return nil
}

func (t *translator) group() error {
	start := len(t.out)
	t.i++
	if !t.peek('?') {
		t.push(start)
		t.emit("(")
		return nil
	}
	t.i++
	if t.i >= len(t.src) {
		return t.fail("unexpected end of pattern")
	}
	c := t.src[t.i]
	switch {
	case c == 'P':
		t.i++
		switch {
		case t.peek('<'):
			t.i++
			name, err := t.name('>')
			if err != nil {
				return err
			}
			t.push(start)
			t.emit("(?<" + name + ">")
		case t.peek('='):
			t.i++
			name, err := t.name(')')
			if err != nil {
				return err
			}
			t.begin()
			t.emit(`\k<` + name + `>`)
		default:
			return t.fail("unknown extension ?P")
		}
	case c == '#':
		return t.comment()
	case c == '(':
		t.i++
		if t.peek('?') {
			// the condition is a lookaround group of its own, sharing this parenthesis
			t.i--
			t.push(start)
			t.emit("(?")
			return nil
		}
		cond, err := t.name(')')
		if err != nil {
			return err
		}
		t.push(start)
		t.emit("(?(" + cond + ")")
	case c == '<' && t.i+1 < len(t.src) && (t.src[t.i+1] == '=' || t.src[t.i+1] == '!'):
		t.push(start)
		t.emit("(?<" + string(t.src[t.i+1]))
		t.i += 2
	case c == '<' || c == '\'':
		end := '>'
		if c == '\'' {
			end = '\''
		}
		t.i++
		name, err := t.name(end)
		if err != nil {
			return err
		}
		t.push(start)
		t.emit("(?" + string(c) + name + string(end))
	case c == ':' || c == '=' || c == '!' || c == '>':
		t.i++
		t.push(start)
		t.emit("(?" + string(c))
	case c == '-' || isASCIILetter(c):
		return t.flags(start)
	default:
		return t.fail("unknown extension ?" + string(c))
	}
	return nil
}

// Text up to the closing rune, which is consumed
func (t *translator) name(end rune) (string, error) {
	start := t.i
	for t.i < len(t.src) && t.src[t.i] != end {
		t.i++
	}
	if t.i >= len(t.src) {
		return "", t.fail("missing " + string(end) + ", unterminated name")
	}
	name := string(t.src[start:t.i])
	t.i++
	if name == "" {
		return "", t.fail("missing group name")
	}
	return name, nil
}

// Skips a comment group up to its closing parenthesis, a backslash escaping the character after it
func (t *translator) comment() error {
	t.i++
	for t.i < len(t.src) {
		c := t.src[t.i]
		t.i++
		if c == ')' {
			return nil
		}
		if c == '\\' {
			t.i++
		}
	}
	return t.fail("missing ), unterminated comment")
}

// Inline flag group (?flags) or (?flags:...): a and x are followed here and dropped, u is a no-op, the others go to the engine
func (t *translator) flags(start int) error {
	var on, off strings.Builder
	next := t.scope
	turningOff := false
	for t.i < len(t.src) {
		c := t.src[t.i]
		t.i++
		switch {
		case c == ')' || c == ':':
			head := "(?" + on.String()
			if off.Len() > 0 {
				head += "-" + off.String()
			}
			if c == ':' {
				t.push(start)
				t.scope = next
				t.emit(head + ":")
				return nil
			}
			t.scope = next
			t.anchor()
			if on.Len() > 0 || off.Len() > 0 {
				t.emit(head + ")")
			}
			return nil
		case c == '-':
			if turningOff {
				return t.fail("bad inline flag: - given twice")
			}
			turningOff = true
		case c == 'a' || c == 'u':
			if turningOff {
				return t.fail("bad inline flag: flags a and u cannot be turned off")
			}
			if c == 'a' {
				next.ascii = true
			}
		case c == 'L':
			return t.fail("bad inline flag: L is not supported")
		case c == 'x' || c == 'X':
			next.verbose = !turningOff
		case isASCIILetter(c):
			if turningOff {
				off.WriteRune(c)
			} else {
				on.WriteRune(c)
			}
		default:
			return t.fail("missing -, : or ) after inline flags")
		}
	}
	return t.fail("missing -, : or ) after inline flags")
}

func (t *translator) escape() error {
	from := t.i
	if t.i+1 >= len(t.src) {
		return t.fail(`bad escape (end of pattern)`)
	}
	c := t.src[t.i+1]
	t.i += 2
	switch c {
	case 'A', 'z', 'G':
		t.anchor()
		t.emitRunes(t.src[from:t.i])
		return nil
	case 'Z':
		t.anchor()
		t.emit(`\z`)
		return nil
	case 'b', 'B':
		t.anchor()
		if t.scope.ascii {
			t.emit(asciiBoundary(c == 'B'))
		} else {
			t.emitRunes(t.src[from:t.i])
		}
		return nil
	case 'w', 'W', 'd', 'D', 's', 'S':
		t.begin()
		if t.scope.ascii {
			t.emit(asciiClass(c))
		} else {
			t.emitRunes(t.src[from:t.i])
		}
		return nil
	case 'p', 'P':
		if err := t.property(); err != nil {
			return err
		}
		t.begin()
		t.emitRunes(t.src[from:t.i])
		return nil
	case 'k':
		if !t.peek('<') && !t.peek('\'') {
			return t.fail(`bad escape \k`)
		}
		end := '>'
		if t.peek('\'') {
			end = '\''
		}
		t.i++
		name, err := t.name(end)
		if err != nil {
			return err
		}
		t.begin()
		t.emit(`\k<` + name + `>`)
		return nil
	}
	if c >= '1' && c <= '9' {
		return t.reference(c)
	}
	r, err := t.char(c)
	if err != nil {
		return err
	}
	t.begin()
	if c == 'U' || c == 'N' || c == '<' || c == '\'' || c == '_' || c >= utf8.RuneSelf {
		// regexp2 reads these escapes otherwise or rejects them
		t.emitChar(r)
	} else {
		t.emitRunes(t.src[from:t.i])
	}
	return nil
}

// Group reference \N or \NN, or an octal escape of three octal digits, at a backslash and digit c; a two digit reference is written \k<NN> so no digit after it can lengthen it
func (t *translator) reference(c rune) error {
	t.begin()
	if t.i >= len(t.src) || !isDigit(t.src[t.i]) {
		t.emit(`\` + string(c))
		return nil
	}
	d := t.src[t.i]
	if isOct(c) && isOct(d) && t.i+1 < len(t.src) && isOct(t.src[t.i+1]) {
		v := (c-'0')*64 + (d-'0')*8 + (t.src[t.i+1] - '0')
		t.i += 2
		if v > 0o377 {
			return t.fail("octal escape value outside of range 0-0o377")
		}
		t.emitChar(v)
		return nil
	}
	t.i++
	t.emit(`\k<` + string([]rune{c, d}) + `>`)
	return nil
}

// Character an escape of c stands for, consuming the digits or name it takes; an unknown ASCII letter or digit is an error
func (t *translator) char(c rune) (rune, error) {
	switch c {
	case 'a':
		return '\a', nil
	case 'f':
		return '\f', nil
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	case 'v':
		return '\v', nil
	case 'e':
		return 0x1B, nil
	case 'x':
		if t.peek('{') {
			return t.hexBrace()
		}
		return t.hex(c, 2)
	case 'u':
		return t.hex(c, 4)
	case 'U':
		return t.hex(c, 8)
	case 'N':
		return t.named()
	case 'c':
		if t.i >= len(t.src) {
			return 0, t.fail(`bad escape \c`)
		}
		ch := t.src[t.i]
		t.i++
		if ch >= 'a' && ch <= 'z' {
			ch -= 'a' - 'A'
		}
		if ch -= '@'; ch < 0 || ch >= ' ' {
			return 0, t.fail(`bad escape \c`)
		}
		return ch, nil
	case '0':
		return t.octal(c)
	}
	if c < utf8.RuneSelf && (isASCIILetter(c) || isDigit(c)) {
		return 0, t.fail(`bad escape \` + string(c))
	}
	return c, nil
}

// Exactly n hex digits after \x, \u or \U
func (t *translator) hex(c rune, n int) (rune, error) {
	start := t.i
	for range n {
		if t.i >= len(t.src) || !isHex(t.src[t.i]) {
			return 0, t.fail(`incomplete escape \` + string(c) + string(t.src[start:t.i]))
		}
		t.i++
	}
	v, _ := strconv.ParseUint(string(t.src[start:t.i]), 16, 32)
	if v > unicode.MaxRune {
		return 0, t.fail(`bad escape \` + string(c) + string(t.src[start:t.i]))
	}
	return rune(v), nil
}

// Hex digits between braces after \x
func (t *translator) hexBrace() (rune, error) {
	t.i++
	start := t.i
	for t.i < len(t.src) && isHex(t.src[t.i]) {
		t.i++
	}
	if t.i == start || !t.peek('}') || t.i-start > 8 {
		return 0, t.fail(`bad escape \x{`)
	}
	v, _ := strconv.ParseUint(string(t.src[start:t.i]), 16, 32)
	t.i++
	if v > unicode.MaxRune {
		return 0, t.fail(`bad escape \x{`)
	}
	return rune(v), nil
}

// Character named by \N{NAME}
func (t *translator) named() (rune, error) {
	if !t.peek('{') {
		return 0, t.fail(`missing { after \N`)
	}
	t.i++
	name, err := t.name('}')
	if err != nil {
		return 0, err
	}
	r, ok := unicodeutil.Lookup(name)
	if !ok {
		return 0, t.fail("undefined character name " + strconv.Quote(name))
	}
	return r, nil
}

// Octal escape starting with digit c, taking up to two more octal digits
func (t *translator) octal(c rune) (rune, error) {
	v := c - '0'
	for range 2 {
		if t.i >= len(t.src) || !isOct(t.src[t.i]) {
			break
		}
		v = v*8 + t.src[t.i] - '0'
		t.i++
	}
	if v > 0o377 {
		return 0, t.fail("octal escape value outside of range 0-0o377")
	}
	return v, nil
}

// Consumes the name of \p or \P: one letter or a braced name
func (t *translator) property() error {
	if t.i >= len(t.src) {
		return t.fail(`bad escape \p`)
	}
	if t.src[t.i] != '{' {
		t.i++
		return nil
	}
	for t.i < len(t.src) && t.src[t.i] != '}' {
		t.i++
	}
	if t.i >= len(t.src) {
		return t.fail(`missing }, unterminated \p{...}`)
	}
	t.i++
	return nil
}

// Character written so the engine reads it as itself anywhere in a pattern; a digit is always escaped so it cannot extend a group reference before it
func (t *translator) emitChar(r rune) {
	if unicode.IsLetter(r) {
		t.out = utf8.AppendRune(t.out, r)
		return
	}
	t.out = append(t.out, `\x{`...)
	t.out = strconv.AppendInt(t.out, int64(r), 16)
	t.out = append(t.out, '}')
}

// One member of a class: a character, or the escape text of a class shorthand or property
type member struct {
	r   rune
	cat string
}

// Character class, copied as written unless a member needs rewriting, then rebuilt member by member
func (t *translator) class() error {
	from := t.i
	t.i++
	negate := t.peek('^')
	if negate {
		t.i++
	}
	var items [][2]member
	rewrite := false
	first := true
	for {
		if t.i >= len(t.src) {
			return t.fail("unterminated character set")
		}
		if t.src[t.i] == ']' && !first {
			t.i++
			break
		}
		first = false
		lo, changed, err := t.classMember()
		if err != nil {
			return err
		}
		rewrite = rewrite || changed
		if t.peek('-') && t.i+1 < len(t.src) && t.src[t.i+1] != ']' {
			t.i++
			hi, changed, err := t.classMember()
			if err != nil {
				return err
			}
			rewrite = rewrite || changed
			if lo.cat != "" || hi.cat != "" || hi.r < lo.r {
				return t.fail("bad character range")
			}
			items = append(items, [2]member{lo, hi})
			continue
		}
		items = append(items, [2]member{lo, lo})
	}
	t.begin()
	if !rewrite {
		t.emitRunes(t.src[from:t.i])
		return nil
	}
	var pos strings.Builder
	var neg []string
	for _, it := range items {
		lo, hi := it[0], it[1]
		if lo.cat == "" {
			writeClassChar(&pos, lo.r)
			if hi.r != lo.r {
				pos.WriteByte('-')
				writeClassChar(&pos, hi.r)
			}
			continue
		}
		body, negated, ascii := t.asciiCategory(lo.cat)
		switch {
		case !ascii:
			pos.WriteString(lo.cat)
		case negated:
			neg = append(neg, body)
		default:
			pos.WriteString(body)
		}
	}
	if len(neg) == 0 {
		if negate {
			t.emit("[^" + pos.String() + "]")
		} else {
			t.emit("[" + pos.String() + "]")
		}
		return nil
	}
	alts := make([]string, 0, len(neg)+1)
	if pos.Len() > 0 {
		alts = append(alts, "["+pos.String()+"]")
	}
	for _, n := range neg {
		alts = append(alts, "[^"+n+"]")
	}
	union := "(?:" + strings.Join(alts, "|") + ")"
	if negate {
		t.emit("(?:(?!" + union + `)[\s\S])`)
	} else {
		t.emit(union)
	}
	return nil
}

// One class member, true when its text must be rewritten for the engine
func (t *translator) classMember() (member, bool, error) {
	c := t.src[t.i]
	t.i++
	if c != '\\' {
		return member{r: c}, c == '[', nil
	}
	if t.i >= len(t.src) {
		return member{}, false, t.fail(`bad escape (end of pattern)`)
	}
	from := t.i - 1
	e := t.src[t.i]
	t.i++
	switch e {
	case 'w', 'W', 'd', 'D', 's', 'S':
		return member{cat: `\` + string(e)}, t.scope.ascii, nil
	case 'p', 'P':
		if err := t.property(); err != nil {
			return member{}, false, err
		}
		return member{cat: string(t.src[from:t.i])}, false, nil
	case 'b':
		return member{r: '\b'}, false, nil
	}
	if e >= '1' && e <= '7' {
		r, err := t.octal(e)
		return member{r: r}, false, err
	}
	r, err := t.char(e)
	if err != nil {
		return member{}, false, err
	}
	return member{r: r}, e == 'U' || e == 'N' || e == '_' || e >= utf8.RuneSelf, nil
}

// Class body of a shorthand under the a flag, whether it is a negated one, and false when the scope keeps the engine's own meaning
func (t *translator) asciiCategory(cat string) (string, bool, bool) {
	if !t.scope.ascii || len(cat) != 2 {
		return "", false, false
	}
	switch cat[1] {
	case 'w', 'W':
		return asciiWord, cat[1] == 'W', true
	case 'd', 'D':
		return asciiDigit, cat[1] == 'D', true
	case 's', 'S':
		return asciiSpace, cat[1] == 'S', true
	}
	return "", false, false
}

// Character written so the engine reads it as itself inside a class
func writeClassChar(b *strings.Builder, r rune) {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		b.WriteRune(r)
		return
	}
	b.WriteString(`\x{`)
	b.WriteString(strconv.FormatInt(int64(r), 16))
	b.WriteByte('}')
}

// Shorthand class under the a flag
func asciiClass(c rune) string {
	switch c {
	case 'w':
		return "[" + asciiWord + "]"
	case 'W':
		return "[^" + asciiWord + "]"
	case 'd':
		return "[" + asciiDigit + "]"
	case 'D':
		return "[^" + asciiDigit + "]"
	case 's':
		return "[" + asciiSpace + "]"
	}
	return "[^" + asciiSpace + "]"
}

// Word boundary, or its negation, between ASCII word characters
func asciiBoundary(negated bool) string {
	w := "[" + asciiWord + "]"
	if negated {
		return "(?:(?<=" + w + ")(?=" + w + ")|(?<!" + w + ")(?!" + w + "))"
	}
	return "(?:(?<=" + w + ")(?!" + w + ")|(?<!" + w + ")(?=" + w + "))"
}

func isDigit(r rune) bool       { return r >= '0' && r <= '9' }
func isOct(r rune) bool         { return r >= '0' && r <= '7' }
func isASCIILetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }
func isHex(r rune) bool {
	return isDigit(r) || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}
