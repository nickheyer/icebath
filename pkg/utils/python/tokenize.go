package pythonutil

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type tokKind uint8

const (
	tEnd tokKind = iota
	tName
	tNumber
	tString
	tNewline
	tIndent
	tDedent
	tOp
	tFStart
	tFMiddle
	tFEnd
	tTStart
	tTMiddle
	tTEnd
	tError
)

// One token: its text, its span in lines and byte columns, the bracket level, its byte offsets and the reader position when it was produced
type token struct {
	kind            tokKind
	val             string
	l, c, el, ec    int
	level           int
	so, eo          int
	curLine, curCol int
}

const (
	maxIndent   = 100
	maxLevel    = 200
	maxFString  = 150
	maxExprNest = 3
	eofChar     = -1
	modeRegular = 0
	modeFString = 1
	tabSize     = 8
	altTabSize  = 1
	stringKindF = 'f'
	stringKindT = 't'
	doneOK      = 0
	doneEOF     = 1
	doneError   = 2
)

type tokMode struct {
	kind           int
	quote          byte
	quoteSize      int
	raw            bool
	stringKind     byte
	start          int
	startLine      int
	multiLineStart int
	curlyDepth     int
	exprStartDepth int
	inFormatSpec   bool
}

type lexer struct {
	src                   string
	cur, inp, lineStart   int
	lineno                int
	done                  int
	atbol                 bool
	pendin                int
	indstack, altindstack []int
	level                 int
	parenstack            []byte
	parenLine, parenCol   []int
	start                 int
	firstLineno           int
	multiLineStart        int
	modes                 []tokMode
	err                   *SyntaxError
	toks                  []token
	lineStarts            []int
	blankline             bool
	errInFString          bool
}

// Tokenizes translated source up to its end or its first error, which is kept in err
func tokenize(src string) *lexer {
	lx := &lexer{src: src, atbol: true, indstack: []int{0}, altindstack: []int{0}, modes: []tokMode{{kind: modeRegular}}}
	lx.toks = make([]token, 0, tokenEstimate(src))
	lx.lineStarts = make([]int, 1, strings.Count(src, "\n")+1)
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			lx.lineStarts = append(lx.lineStarts, i+1)
		}
	}
	for {
		k := lx.next()
		if lx.err != nil {
			return lx
		}
		if k == tError {
			lx.emit(tError, -1, -1)
			return lx
		}
		if k == tEnd {
			return lx
		}
	}
}

// About the token count, word runs and operator bytes once and newlines twice, so the token slice rarely grows
func tokenEstimate(src string) int {
	n, word := 16, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\n':
			n += 2
			word = false
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\\':
			word = false
		case c == '_' || c >= '0' && c <= '9' || c|0x20 >= 'a' && c|0x20 <= 'z' || c >= 0x80:
			if !word {
				n++
			}
			word = true
		default:
			n++
			word = false
		}
	}
	return n
}

// Returns the next byte of the current line inline and leaves line refills to nextLine
func (lx *lexer) nextc() int {
	if c := lx.cur; c < lx.inp {
		lx.cur = c + 1
		return int(lx.src[c])
	}
	return lx.nextLine()
}

func (lx *lexer) nextLine() int {
	for {
		if lx.cur != lx.inp {
			c := lx.src[lx.cur]
			lx.cur++
			return int(c)
		}
		if lx.done != doneOK {
			return eofChar
		}
		if lx.inp >= len(lx.src) {
			lx.done = doneEOF
			return eofChar
		}
		end := lx.inp
		for end < len(lx.src) && lx.src[end] != '\n' {
			end++
		}
		if end < len(lx.src) {
			end++
		}
		lx.lineno++
		lx.lineStart = lx.cur
		lx.inp = end
	}
}

func (lx *lexer) backup(c int) {
	if c != eofChar {
		lx.cur--
	}
}

func (lx *lexer) mode() *tokMode { return &lx.modes[len(lx.modes)-1] }

func (lx *lexer) insideFString() bool { return len(lx.modes) > 1 }

func (lx *lexer) prefix() byte { return lx.mode().stringKind }

// Character count of a byte range, the unit of the columns errors report
func charCount(s string) int { return utf8.RuneCountInString(s) }

func (lx *lexer) lineText() string {
	end := lx.lineStart
	for end < len(lx.src) && lx.src[end] != '\n' {
		end++
	}
	return lx.src[lx.lineStart:end]
}

// Text of a 1-based source line without its line end, empty outside the source
func (lx *lexer) textOfLine(lineno int) string {
	if lineno < 1 || lineno > len(lx.lineStarts) {
		return ""
	}
	s := lx.lineStarts[lineno-1]
	e := s
	for e < len(lx.src) && lx.src[e] != '\n' {
		e++
	}
	return lx.src[s:e]
}

// Raises the tokenizer SyntaxError at the current position
func (lx *lexer) syntaxError(format string, args ...any) tokKind {
	return lx.syntaxErrorRange(-1, -1, format, args...)
}

func (lx *lexer) syntaxErrorRange(col, endCol int, format string, args ...any) tokKind {
	if col == -1 {
		col = charCount(lx.src[lx.lineStart:lx.cur])
	}
	if endCol == -1 {
		endCol = col
	}
	lx.err = &SyntaxError{Msg: fmt.Sprintf(format, args...), Line: lx.lineno, Column: col, EndLine: lx.lineno, EndColumn: endCol, Text: lx.lineText()}
	lx.errInFString = len(lx.modes) > 1
	lx.done = doneError
	return tError
}

func (lx *lexer) indentError() tokKind {
	lx.err = &SyntaxError{Kind: KindTab, Msg: "inconsistent use of tabs and spaces in indentation", Line: lx.lineno, Column: 1, EndLine: lx.lineno, EndColumn: 0, Text: lx.lineText()}
	lx.done = doneError
	return tError
}

func (lx *lexer) emit(k tokKind, start, end int) {
	t := token{kind: k, l: lx.lineno, el: lx.lineno, c: -1, ec: -1, level: lx.level, so: start, eo: end, curLine: lx.lineno, curCol: lx.cur - lx.lineStart}
	if k == tString || k == tFMiddle || k == tTMiddle {
		t.l = lx.firstLineno
	}
	if start >= 0 {
		t.c = start - lx.lineStartOf(start)
		t.val = lx.src[start:end]
		t.ec = lx.cur - lx.lineStart
	}
	lx.toks = append(lx.toks, t)
}

// Offset of the line holding a source position
func (lx *lexer) lineStartOf(pos int) int {
	lo, hi := 0, len(lx.lineStarts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if lx.lineStarts[mid] <= pos {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lx.lineStarts[lo]
}

func isIdentStart(c int) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c >= 128
}

func isIdentChar(c int) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

func isDigit(c int) bool { return c >= '0' && c <= '9' }

func isXDigit(c int) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// Produces the next token or tError, appending real tokens to the token list
func (lx *lexer) next() tokKind {
	m := lx.mode()
	if m.kind == modeFString {
		return lx.fstringMode(m)
	}
	return lx.normalMode(m)
}

func (lx *lexer) tok(k tokKind, start, end int) tokKind {
	lx.emit(k, start, end)
	return k
}

func (lx *lexer) normalMode(m *tokMode) tokKind {
nextline:
	lx.blankline = false
	if lx.atbol {
		col, altcol, contCol := 0, 0, 0
		lx.atbol = false
		var c int
		for {
			c = lx.nextc()
			if c == ' ' {
				col++
				altcol++
			} else if c == '\t' {
				col = (col/tabSize + 1) * tabSize
				altcol = (altcol/altTabSize + 1) * altTabSize
			} else if c == '\014' {
				col, altcol = 0, 0
			} else if c == '\\' {
				if contCol == 0 {
					contCol = col
				}
				if c = lx.continuation(); c == -2 {
					return lx.tokenizerFailure()
				}
			} else {
				break
			}
		}
		lx.backup(c)
		if c == '#' || c == '\n' || c == '\r' {
			lx.blankline = true
		}
		if !lx.blankline && lx.level == 0 {
			if contCol != 0 {
				col, altcol = contCol, contCol
			}
			top := len(lx.indstack) - 1
			if col == lx.indstack[top] {
				if altcol != lx.altindstack[top] {
					return lx.indentError()
				}
			} else if col > lx.indstack[top] {
				if top+1 >= maxIndent {
					lx.cur = lx.inp
					lx.err = &SyntaxError{Kind: KindIndentation, Msg: "too many levels of indentation", Line: lx.lineno, Column: 1, EndLine: lx.lineno, EndColumn: 0, Text: lx.lineText()}
					lx.done = doneError
					return tError
				}
				if altcol <= lx.altindstack[top] {
					return lx.indentError()
				}
				lx.pendin++
				lx.indstack = append(lx.indstack, col)
				lx.altindstack = append(lx.altindstack, altcol)
			} else {
				for len(lx.indstack) > 1 && col < lx.indstack[len(lx.indstack)-1] {
					lx.pendin--
					lx.indstack = lx.indstack[:len(lx.indstack)-1]
					lx.altindstack = lx.altindstack[:len(lx.altindstack)-1]
				}
				top = len(lx.indstack) - 1
				if col != lx.indstack[top] {
					lx.cur = lx.inp
					offset := charCount(lx.src[lx.lineStart:lx.cur])
					lx.err = &SyntaxError{Kind: KindIndentation, Msg: "unindent does not match any outer indentation level", Line: lx.lineno, Column: offset, EndLine: lx.lineno, EndColumn: -1, Text: lx.lineText()}
					lx.done = doneError
					return tError
				}
				if altcol != lx.altindstack[top] {
					return lx.indentError()
				}
			}
		}
	}
	lx.start = lx.cur
	if lx.pendin != 0 {
		if lx.pendin < 0 {
			lx.pendin++
			return lx.tok(tDedent, -1, -1)
		}
		lx.pendin--
		return lx.tok(tIndent, -1, -1)
	}
	c := lx.nextc()
	lx.backup(c)
again:
	for {
		c = lx.nextc()
		if c != ' ' && c != '\t' && c != '\014' {
			break
		}
	}
	lx.start = lx.cur - 1
	if c == eofChar {
		lx.start = lx.cur
	}
	if c == '#' {
		for c != eofChar && c != '\n' && c != '\r' {
			c = lx.nextc()
		}
	}
	if c == eofChar {
		if lx.done == doneError {
			return tError
		}
		if lx.level > 0 {
			return tError
		}
		return lx.tok(tEnd, -1, -1)
	}
	if isIdentStart(c) {
		sawB, sawR, sawU, sawF, sawT := false, false, false, false, false
		for {
			if !sawB && (c == 'b' || c == 'B') {
				sawB = true
			} else if !sawU && (c == 'u' || c == 'U') {
				sawU = true
			} else if !sawR && (c == 'r' || c == 'R') {
				sawR = true
			} else if !sawF && (c == 'f' || c == 'F') {
				sawF = true
			} else if !sawT && (c == 't' || c == 'T') {
				sawT = true
			} else {
				break
			}
			c = lx.nextc()
			if c == '"' || c == '\'' {
				if k := prefixConflict(sawB, sawU, sawR, sawF, sawT); k != "" {
					return lx.syntaxErrorRange(lx.start+1-lx.lineStart, lx.cur-lx.lineStart, "%s prefixes are incompatible", k)
				}
				if sawF || sawT {
					return lx.fstringStart(c)
				}
				return lx.stringLiteral(c)
			}
		}
		nonASCII := false
		for isIdentChar(c) {
			if c >= 128 {
				nonASCII = true
			}
			c = lx.nextc()
		}
		lx.backup(c)
		if nonASCII && !lx.verifyIdentifier() {
			return tError
		}
		return lx.tok(tName, lx.start, lx.cur)
	}
	if c == '\r' {
		c = lx.nextc()
	}
	if c == '\n' {
		lx.atbol = true
		if lx.blankline || lx.level > 0 {
			goto nextline
		}
		return lx.tok(tNewline, lx.start, lx.cur)
	}
	if c == '.' {
		c = lx.nextc()
		if isDigit(c) {
			return lx.number(c, true)
		} else if c == '.' {
			c = lx.nextc()
			if c == '.' {
				return lx.tok(tOp, lx.start, lx.cur)
			}
			lx.backup(c)
			lx.backup('.')
		} else {
			lx.backup(c)
		}
		return lx.tok(tOp, lx.start, lx.cur)
	}
	if isDigit(c) {
		return lx.number(c, false)
	}
	if c == '\'' || c == '"' {
		return lx.stringLiteral(c)
	}
	if c == '\\' {
		if c = lx.continuation(); c == -2 {
			return lx.tokenizerFailure()
		}
		goto again
	}
	if (c == ':' || c == '}' || c == '!' || c == '{') && lx.insideFString() && lx.mode().exprStartDepth >= 0 {
		cm := lx.mode()
		cursor := cm.curlyDepth
		if c != '{' {
			cursor--
		}
		if c == ':' && cursor == cm.exprStartDepth {
			cm.kind = modeFString
			cm.inFormatSpec = true
			return lx.tok(tOp, lx.start, lx.cur)
		}
	}
	c2 := lx.nextc()
	if c2 != eofChar && twoCharOp(byte(c), byte(c2)) {
		c3 := lx.nextc()
		if c3 != eofChar && threeCharOp(byte(c), byte(c2), byte(c3)) {
			return lx.tok(tOp, lx.start, lx.cur)
		}
		lx.backup(c3)
		return lx.tok(tOp, lx.start, lx.cur)
	}
	lx.backup(c2)
	cm := lx.mode()
	switch c {
	case '(', '[', '{':
		if lx.level >= maxLevel {
			return lx.syntaxError("too many nested parentheses")
		}
		lx.parenstack = append(lx.parenstack, byte(c))
		lx.parenLine = append(lx.parenLine, lx.lineno)
		lx.parenCol = append(lx.parenCol, lx.start-lx.lineStart)
		lx.level++
		if lx.insideFString() {
			cm.curlyDepth++
		}
	case ')', ']', '}':
		if lx.insideFString() && cm.curlyDepth == 0 && c == '}' {
			return lx.syntaxError("%c-string: single '}' is not allowed", lx.prefix())
		}
		if lx.level == 0 {
			return lx.syntaxError("unmatched '%c'", c)
		}
		lx.level--
		opening := lx.parenstack[lx.level]
		openLine := lx.parenLine[lx.level]
		lx.parenstack = lx.parenstack[:lx.level]
		lx.parenLine = lx.parenLine[:lx.level]
		lx.parenCol = lx.parenCol[:lx.level]
		if !((opening == '(' && c == ')') || (opening == '[' && c == ']') || (opening == '{' && c == '}')) {
			if lx.insideFString() && opening == '{' && cm.curlyDepth-1 == cm.exprStartDepth {
				return lx.syntaxError("%c-string: unmatched '%c'", lx.prefix(), c)
			}
			if openLine != lx.lineno {
				return lx.syntaxError("closing parenthesis '%c' does not match opening parenthesis '%c' on line %d", c, opening, openLine)
			}
			return lx.syntaxError("closing parenthesis '%c' does not match opening parenthesis '%c'", c, opening)
		}
		if lx.insideFString() {
			cm.curlyDepth--
			if cm.curlyDepth < 0 {
				return lx.syntaxError("%c-string: unmatched '%c'", lx.prefix(), c)
			}
			if c == '}' && cm.curlyDepth == cm.exprStartDepth {
				cm.exprStartDepth--
				cm.kind = modeFString
				cm.inFormatSpec = false
			}
		}
	}
	if !isPrintable(rune(c)) {
		return lx.syntaxError("invalid non-printable character U+%04X", c)
	}
	return lx.tok(tOp, lx.start, lx.cur)
}

// Whether two characters make one operator token
func twoCharOp(a, b byte) bool {
	switch b {
	case '=':
		return strings.IndexByte("!%&*+-/:<=>@^|", a) >= 0
	case '*', '/', '<', '>':
		return a == b || b == '>' && a == '-'
	}
	return false
}

// Whether three characters make one operator token
func threeCharOp(a, b, c byte) bool {
	switch {
	case a == '.':
		return b == '.' && c == '.'
	case c == '=':
		return a == b && (a == '*' || a == '/' || a == '<' || a == '>')
	}
	return false
}

// Consumes a backslash newline, returning -2 after setting the failure state
func (lx *lexer) continuation() int {
	c := lx.nextc()
	if c == '\r' {
		c = lx.nextc()
	}
	if c != '\n' {
		col := lx.cur - lx.lineStart - 1
		lx.err = &SyntaxError{Msg: "unexpected character after line continuation character", Line: lx.lineno, Column: charCount(lx.src[lx.lineStart:lx.lineStart+max(col, 0)]) + 1, EndLine: lx.lineno, EndColumn: 0, Text: lx.lineText()}
		lx.done = doneError
		return -2
	}
	c = lx.nextc()
	if c == eofChar {
		lx.cur = lx.inp
		lx.err = &SyntaxError{Msg: "unexpected EOF while parsing", Line: lx.lineno, Column: charCount(lx.src[lx.lineStart:lx.cur]), EndLine: lx.lineno, EndColumn: -1, Text: lx.lineText()}
		lx.done = doneError
		return -2
	}
	lx.backup(c)
	return c
}

func (lx *lexer) tokenizerFailure() tokKind { return tError }

// Incompatible string prefix pair, in the order Python reports them
func prefixConflict(b, u, r, f, t bool) string {
	switch {
	case u && b:
		return "'u' and 'b'"
	case u && r:
		return "'u' and 'r'"
	case u && f:
		return "'u' and 'f'"
	case u && t:
		return "'u' and 't'"
	case b && f:
		return "'b' and 'f'"
	case b && t:
		return "'b' and 't'"
	case f && t:
		return "'f' and 't'"
	}
	return ""
}

// Checks a non ASCII identifier against the Unicode identifier rules, raising at the first character that breaks them
func (lx *lexer) verifyIdentifier() bool {
	s := lx.src[lx.start:lx.cur]
	for i, r := range s {
		if i == 0 && (r == '_' || isXIDStart(r)) || i > 0 && isXIDContinue(r) {
			continue
		}
		if _, size := utf8.DecodeRuneInString(s[i:]); i+size < len(s) {
			lx.cur = lx.start + i + size
		}
		if isPrintable(r) {
			lx.syntaxError("invalid character '%c' (U+%04X)", r, r)
		} else {
			lx.syntaxError("invalid non-printable character U+%04X", r)
		}
		return false
	}
	return true
}

func (lx *lexer) lookahead(test string) bool {
	var read []int
	ok := true
	for i := 0; i < len(test); i++ {
		c := lx.nextc()
		read = append(read, c)
		if c != int(test[i]) {
			ok = false
			break
		}
	}
	for i := len(read) - 1; i >= 0; i-- {
		lx.backup(read[i])
	}
	return ok
}

func (lx *lexer) verifyEndOfNumber(c int, kind string) bool {
	r := false
	switch c {
	case 'a':
		r = lx.lookahead("nd")
	case 'e':
		r = lx.lookahead("lse")
	case 'f':
		r = lx.lookahead("or")
	case 'i':
		c2 := lx.nextc()
		r = c2 == 'f' || c2 == 'n' || c2 == 's'
		lx.backup(c2)
	case 'o':
		r = lx.lookahead("r")
	case 'n':
		r = lx.lookahead("ot")
	}
	if r {
		return true
	}
	if c >= 0 && c < 128 && isIdentChar(c) {
		lx.backup(c)
		lx.syntaxError("invalid %s literal", kind)
		return false
	}
	return true
}

func (lx *lexer) decimalTail() int {
	var c int
	for {
		for {
			c = lx.nextc()
			if !isDigit(c) {
				break
			}
		}
		if c != '_' {
			break
		}
		c = lx.nextc()
		if !isDigit(c) {
			lx.backup(c)
			lx.syntaxError("invalid decimal literal")
			return 0
		}
	}
	return c
}

func (lx *lexer) radixDigits(c int, kind string, ok func(int) bool, digitErr bool) bool {
	c = lx.nextc()
	for {
		if c == '_' {
			c = lx.nextc()
		}
		if !ok(c) {
			if digitErr && isDigit(c) {
				lx.syntaxError("invalid digit '%c' in %s literal", c, kind)
				return false
			}
			lx.backup(c)
			lx.syntaxError("invalid %s literal", kind)
			return false
		}
		for {
			c = lx.nextc()
			if !ok(c) {
				break
			}
		}
		if c != '_' {
			break
		}
	}
	if digitErr && isDigit(c) {
		lx.syntaxError("invalid digit '%c' in %s literal", c, kind)
		return false
	}
	if !lx.verifyEndOfNumber(c, kind) {
		return false
	}
	lx.backup(c)
	return true
}

// Scans a numeric literal from its first character, or from the digit after a leading dot
func (lx *lexer) number(c int, afterDot bool) tokKind {
	if afterDot {
		goto fraction
	}
	if c == '0' {
		c = lx.nextc()
		switch c {
		case 'x', 'X':
			if !lx.radixDigits(c, "hexadecimal", isXDigit, false) {
				return tError
			}
			return lx.tok(tNumber, lx.start, lx.cur)
		case 'o', 'O':
			if !lx.radixDigits(c, "octal", func(c int) bool { return c >= '0' && c < '8' }, true) {
				return tError
			}
			return lx.tok(tNumber, lx.start, lx.cur)
		case 'b', 'B':
			if !lx.radixDigits(c, "binary", func(c int) bool { return c == '0' || c == '1' }, true) {
				return tError
			}
			return lx.tok(tNumber, lx.start, lx.cur)
		}
		nonzero := false
		for {
			if c == '_' {
				c = lx.nextc()
				if !isDigit(c) {
					lx.backup(c)
					return lx.syntaxError("invalid decimal literal")
				}
			}
			if c != '0' {
				break
			}
			c = lx.nextc()
		}
		zerosEnd := lx.cur
		if isDigit(c) {
			nonzero = true
			if c = lx.decimalTail(); c == 0 {
				return tError
			}
		}
		if c == '.' {
			c = lx.nextc()
			goto fraction
		} else if c == 'e' || c == 'E' {
			goto exponent
		} else if c == 'j' || c == 'J' {
			goto imaginary
		} else if nonzero {
			lx.backup(c)
			return lx.syntaxErrorRange(lx.start+1-lx.lineStart, zerosEnd-lx.lineStart, "leading zeros in decimal integer literals are not permitted; use an 0o prefix for octal integers")
		}
		if !lx.verifyEndOfNumber(c, "decimal") {
			return tError
		}
		lx.backup(c)
		return lx.tok(tNumber, lx.start, lx.cur)
	}
	if c = lx.decimalTail(); c == 0 {
		return tError
	}
	if c == '.' {
		c = lx.nextc()
		goto fraction
	}
	goto afterFraction
fraction:
	if isDigit(c) {
		if c = lx.decimalTail(); c == 0 {
			return tError
		}
	}
afterFraction:
	if c == 'e' || c == 'E' {
		goto exponent
	}
	if c == 'j' || c == 'J' {
		goto imaginary
	}
	if !lx.verifyEndOfNumber(c, "decimal") {
		return tError
	}
	lx.backup(c)
	return lx.tok(tNumber, lx.start, lx.cur)
exponent:
	{
		e := c
		c = lx.nextc()
		if c == '+' || c == '-' {
			c = lx.nextc()
			if !isDigit(c) {
				lx.backup(c)
				return lx.syntaxError("invalid decimal literal")
			}
		} else if !isDigit(c) {
			lx.backup(c)
			if !lx.verifyEndOfNumber(e, "decimal") {
				return tError
			}
			lx.backup(e)
			return lx.tok(tNumber, lx.start, lx.cur)
		}
		if c = lx.decimalTail(); c == 0 {
			return tError
		}
		if c == 'j' || c == 'J' {
			goto imaginary
		}
		if !lx.verifyEndOfNumber(c, "decimal") {
			return tError
		}
		lx.backup(c)
		return lx.tok(tNumber, lx.start, lx.cur)
	}
imaginary:
	c = lx.nextc()
	if !lx.verifyEndOfNumber(c, "imaginary") {
		return tError
	}
	lx.backup(c)
	return lx.tok(tNumber, lx.start, lx.cur)
}

// Scans a plain or bytes string literal whose opening quote was just read
func (lx *lexer) stringLiteral(quote int) tokKind {
	quoteSize, endQuoteSize := 1, 0
	escapedQuote := false
	lx.firstLineno = lx.lineno
	lx.multiLineStart = lx.lineStart
	c := lx.nextc()
	if c == quote {
		c = lx.nextc()
		if c == quote {
			quoteSize = 3
		} else {
			endQuoteSize = 1
		}
	}
	if c != quote {
		lx.backup(c)
	}
	for endQuoteSize != quoteSize {
		c = lx.nextc()
		if lx.done == doneError {
			return tError
		}
		if c == eofChar || (quoteSize == 1 && c == '\n') {
			detected := lx.lineno
			lx.cur = lx.start + 1
			lx.lineStart = lx.multiLineStart
			lx.lineno = lx.firstLineno
			if lx.insideFString() {
				cm := lx.mode()
				if int(cm.quote) == quote && cm.quoteSize == quoteSize {
					return lx.syntaxError("%c-string: expecting '}'", lx.prefix())
				}
			}
			if quoteSize == 3 {
				return lx.syntaxError("unterminated triple-quoted string literal (detected at line %d)", detected)
			}
			if escapedQuote {
				return lx.syntaxError("unterminated string literal (detected at line %d); perhaps you escaped the end quote?", detected)
			}
			return lx.syntaxError("unterminated string literal (detected at line %d)", detected)
		}
		if c == quote {
			endQuoteSize++
		} else {
			endQuoteSize = 0
			if c == '\\' {
				c = lx.nextc()
				if c == quote {
					escapedQuote = true
				}
				if c == '\r' {
					lx.nextc()
				}
			}
		}
	}
	return lx.tok(tString, lx.start, lx.cur)
}

// Opens an f-string or t-string mode after its prefix and first quote
func (lx *lexer) fstringStart(quote int) tokKind {
	quoteSize := 1
	lx.firstLineno = lx.lineno
	lx.multiLineStart = lx.lineStart
	after := lx.nextc()
	if after == quote {
		afterAfter := lx.nextc()
		if afterAfter == quote {
			quoteSize = 3
		} else {
			lx.backup(afterAfter)
			lx.backup(after)
		}
	}
	if after != quote {
		lx.backup(after)
	}
	if len(lx.modes) >= maxFString {
		return lx.syntaxError("too many nested f-strings or t-strings")
	}
	m := tokMode{kind: modeFString, quote: byte(quote), quoteSize: quoteSize, start: lx.start, multiLineStart: lx.lineStart, startLine: lx.lineno, exprStartDepth: -1, stringKind: stringKindF}
	p0, p1 := lx.src[lx.start]|0x20, byte(0)
	if lx.start+1 < len(lx.src) {
		p1 = lx.src[lx.start+1] | 0x20
	}
	switch p0 {
	case 't':
		m.raw = p1 == 'r'
		m.stringKind = stringKindT
	case 'f':
		m.raw = p1 == 'r'
	case 'r':
		m.raw = true
		if p1 == 't' {
			m.stringKind = stringKindT
		}
	}
	k := tFStart
	if m.stringKind == stringKindT {
		k = tTStart
	}
	lx.emit(k, lx.start, lx.cur)
	lx.modes = append(lx.modes, m)
	return k
}

func middleKind(m *tokMode) tokKind {
	if m.stringKind == stringKindT {
		return tTMiddle
	}
	return tFMiddle
}

// Scans the literal part of an f-string or t-string up to a replacement field or the closing quote
func (lx *lexer) fstringMode(m *tokMode) tokKind {
	lx.start = lx.cur
	lx.firstLineno = lx.lineno
	startChar := lx.nextc()
	if startChar == '{' {
		peek := lx.nextc()
		lx.backup(peek)
		lx.backup(startChar)
		if peek != '{' {
			m.exprStartDepth++
			if m.exprStartDepth >= maxExprNest {
				return lx.syntaxError("%c-string: expressions nested too deeply", lx.prefix())
			}
			m.kind = modeRegular
			return lx.normalMode(m)
		}
	} else {
		lx.backup(startChar)
	}
	atEnd := true
	var read []int
	for i := 0; i < m.quoteSize; i++ {
		q := lx.nextc()
		read = append(read, q)
		if q != int(m.quote) {
			atEnd = false
			break
		}
	}
	if atEnd {
		k := tFEnd
		if m.stringKind == stringKindT {
			k = tTEnd
		}
		lx.emit(k, lx.start, lx.cur)
		lx.modes = lx.modes[:len(lx.modes)-1]
		return k
	}
	for i := len(read) - 1; i >= 0; i-- {
		lx.backup(read[i])
	}
	lx.multiLineStart = lx.lineStart
	endQuoteSize := 0
	unicodeEscape := false
	for endQuoteSize != m.quoteSize {
		c := lx.nextc()
		if lx.done == doneError {
			return tError
		}
		inFormatSpec := m.inFormatSpec && m.exprStartDepth >= 0
		if c == eofChar || (m.quoteSize == 1 && c == '\n') {
			if inFormatSpec && c == '\n' {
				if m.quoteSize == 1 {
					return lx.syntaxError("%c-string: newlines are not allowed in format specifiers for single quoted %c-strings", lx.prefix(), lx.prefix())
				}
				lx.backup(c)
				m.kind = modeRegular
				m.inFormatSpec = false
				return lx.tok(middleKind(m), lx.start, lx.cur)
			}
			detected := lx.lineno
			lx.cur = m.start + 1
			lx.lineStart = m.multiLineStart
			lx.lineno = m.startLine
			if m.quoteSize == 3 {
				return lx.syntaxError("unterminated triple-quoted %c-string literal (detected at line %d)", lx.prefix(), detected)
			}
			return lx.syntaxError("unterminated %c-string literal (detected at line %d)", lx.prefix(), detected)
		}
		if c == int(m.quote) {
			endQuoteSize++
			continue
		}
		endQuoteSize = 0
		switch c {
		case '{':
			peek := lx.nextc()
			if peek != '{' || inFormatSpec {
				lx.backup(peek)
				lx.backup(c)
				m.exprStartDepth++
				if m.exprStartDepth >= maxExprNest {
					return lx.syntaxError("%c-string: expressions nested too deeply", lx.prefix())
				}
				m.kind = modeRegular
				m.inFormatSpec = false
				return lx.tok(middleKind(m), lx.start, lx.cur)
			}
			return lx.tok(middleKind(m), lx.start, lx.cur-1)
		case '}':
			if unicodeEscape {
				return lx.tok(middleKind(m), lx.start, lx.cur)
			}
			peek := lx.nextc()
			if peek == '}' && !inFormatSpec && m.curlyDepth == 0 {
				return lx.tok(middleKind(m), lx.start, lx.cur-1)
			}
			lx.backup(peek)
			lx.backup(c)
			m.kind = modeRegular
			m.inFormatSpec = false
			return lx.tok(middleKind(m), lx.start, lx.cur)
		case '\\':
			peek := lx.nextc()
			if peek == '\r' {
				peek = lx.nextc()
			}
			if peek == '{' || peek == '}' {
				lx.backup(peek)
				continue
			}
			if !m.raw && peek == 'N' {
				peek = lx.nextc()
				if peek == '{' {
					unicodeEscape = true
				} else {
					lx.backup(peek)
				}
			}
		}
	}
	for i := 0; i < m.quoteSize; i++ {
		lx.backup(int(m.quote))
	}
	return lx.tok(middleKind(m), lx.start, lx.cur)
}
