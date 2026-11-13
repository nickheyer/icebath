package python

import (
	"fmt"
	"unicode"
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

// Lexeme with its text, zero based byte columns, bracket level and source offsets
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
	stringKindF = 'f'
	stringKindT = 't'
	doneOK      = 0
	doneEOF     = 1
	doneError   = 2
)

// Tokenizer state of one open f-string or t-string, or of the regular code around them
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
	barry                 bool
	future                futureScan
	barryAt, barryTok     int
	lineStarts            []int
	blankline             bool
	errInFString          bool
}

// Tokens of decoded UTF-8 source, <> enabled from token at, after the scanned future import for -1, never for -2
func tokenize(src string, at int) *lexer {
	lx := &lexer{src: src, atbol: true, indstack: []int{0}, altindstack: []int{0}, modes: []tokMode{{kind: modeRegular}}, barryAt: at, barryTok: -1}
	lx.toks = make([]token, 0, tokenEstimate(src))
	lx.lineStarts = append(lx.lineStarts, 0)
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

// Byte classes of the token count estimate, word bytes 1, operators and quotes 2, newlines 3, blanks 0
var estimateClass = func() (t [256]byte) {
	for c := 0; c < 256; c++ {
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\\':
		case c == '\n':
			t[c] = 3
		case c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80:
			t[c] = 1
		default:
			t[c] = 2
		}
	}
	return
}()

// Approximate token count, word runs and operator bytes plus two per newline, so the token slice rarely grows
func tokenEstimate(src string) int {
	n, prev := 16, byte(0)
	for i := 0; i < len(src); i++ {
		k := estimateClass[src[i]]
		switch {
		case k == 3:
			n += 2
		case k == 2 || k == 1 && prev != 1:
			n++
		}
		prev = k
	}
	return n
}

// Next byte of the current line, refilling the line buffer at its end
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

// Text of the current line without its newline
func (lx *lexer) lineText() string {
	end := lx.lineStart
	for end < len(lx.src) && lx.src[end] != '\n' {
		end++
	}
	return lx.src[lx.lineStart:end]
}

// Records the tokenizer error at the current position and stops tokenizing
func (lx *lexer) syntaxError(format string, args ...any) tokKind {
	return lx.syntaxErrorRange(-1, -1, format, args...)
}

// Records the tokenizer error at explicit columns, -1 meaning the character count up to the cursor
func (lx *lexer) syntaxErrorRange(col, endCol int, format string, args ...any) tokKind {
	if col == -1 {
		col = charCount([]byte(lx.src[lx.lineStart:lx.cur]))
	}
	if endCol == -1 {
		endCol = col
	}
	lx.err = &SyntaxError{Msg: fmt.Sprintf(format, args...), Line: lx.lineno, Col: col, EndLine: lx.lineno, EndCol: endCol, Text: lx.lineText()}
	lx.errInFString = len(lx.modes) > 1
	lx.done = doneError
	return tError
}

// Records an indentation error of the given class at the given columns and stops tokenizing
func (lx *lexer) indentationError(class ErrorClass, msg string, col, endCol int) tokKind {
	lx.err = &SyntaxError{Class: class, Msg: msg, Line: lx.lineno, Col: col, EndLine: lx.lineno, EndCol: endCol, Text: lx.lineText()}
	lx.done = doneError
	return tError
}

func (lx *lexer) tabError() tokKind {
	return lx.indentationError(ClassTab, "inconsistent use of tabs and spaces in indentation", 1, 0)
}

// Appends a token, positioned from start to the cursor, or unpositioned for a start of -1
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
	switch {
	case lx.barryAt == -1:
		lx.scanFuture()
	case lx.barryAt == len(lx.toks):
		lx.barry, lx.barryTok = true, lx.barryAt
	}
}

// Progress through a from __future__ import statement over emitted tokens
type futureScan struct {
	step         int
	paren, barry bool
}

// Turns on the <> operator after the token ending a from __future__ import that names barry_as_FLUFL
func (lx *lexer) scanFuture() {
	f := &lx.future
	n := len(lx.toks)
	t := &lx.toks[n-1]
	name := t.kind == tName
	finish := func() {
		if f.barry && !lx.barry {
			lx.barry, lx.barryTok = true, n
		}
		*f = futureScan{}
	}
	switch f.step {
	case 0:
		if name && t.val == "from" && !lx.barry {
			if n == 1 || lx.toks[n-2].kind == tNewline || lx.toks[n-2].kind == tIndent || lx.toks[n-2].kind == tDedent || lx.toks[n-2].val == ";" || lx.toks[n-2].val == ":" {
				f.step = 1
			}
		}
	case 1, 2:
		if name && (f.step == 1 && normIdent(t.val) == "__future__" || f.step == 2 && t.val == "import") {
			f.step++
		} else {
			*f = futureScan{}
		}
	case 3, 4:
		switch {
		case f.step == 3 && t.val == "(":
			f.paren, f.step = true, 4
		case name:
			f.barry = f.barry || normIdent(t.val) == "barry_as_FLUFL"
			f.step = 5
		case f.step == 4 && f.paren && t.val == ")":
			finish()
		default:
			*f = futureScan{}
		}
	case 5, 7:
		switch {
		case f.step == 5 && name && t.val == "as":
			f.step = 6
		case t.val == ",":
			f.step = 4
		case f.paren && t.val == ")" || !f.paren:
			finish()
		default:
			*f = futureScan{}
		}
	case 6:
		switch {
		case name:
			f.step = 7
		case !f.paren:
			finish()
		default:
			*f = futureScan{}
		}
	}
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

// Reports whether a code point prints, space included and separators and the C categories refused
func isPrintable(r rune) bool {
	return r == ' ' || (unicode.IsPrint(r) && !(r >= 0xd800 && r <= 0xdfff))
}

// Produces the following lexeme or tError, appending real lexemes to the list
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

// Measures the leading whitespace of a new logical line, returning tError after a failure
func (lx *lexer) indentation() tokKind {
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
			altcol++
		} else if c == '\014' {
			col, altcol = 0, 0
		} else if c == '\\' {
			if contCol == 0 {
				contCol = col
			}
			if c = lx.continuation(); c == -2 {
				return tError
			}
		} else {
			break
		}
	}
	lx.backup(c)
	if c == '#' || c == '\n' || c == '\r' {
		lx.blankline = true
	}
	if lx.blankline || lx.level != 0 {
		return tEnd
	}
	if contCol != 0 {
		col, altcol = contCol, contCol
	}
	top := len(lx.indstack) - 1
	switch {
	case col == lx.indstack[top]:
		if altcol != lx.altindstack[top] {
			return lx.tabError()
		}
	case col > lx.indstack[top]:
		if top+1 >= maxIndent {
			lx.cur = lx.inp
			return lx.indentationError(ClassIndentation, "too many levels of indentation", 1, 0)
		}
		if altcol <= lx.altindstack[top] {
			return lx.tabError()
		}
		lx.pendin++
		lx.indstack = append(lx.indstack, col)
		lx.altindstack = append(lx.altindstack, altcol)
	default:
		for len(lx.indstack) > 1 && col < lx.indstack[len(lx.indstack)-1] {
			lx.pendin--
			lx.indstack = lx.indstack[:len(lx.indstack)-1]
			lx.altindstack = lx.altindstack[:len(lx.altindstack)-1]
		}
		top = len(lx.indstack) - 1
		if col != lx.indstack[top] {
			lx.cur = lx.inp
			offset := charCount([]byte(lx.src[lx.lineStart:lx.cur]))
			return lx.indentationError(ClassIndentation, "unindent does not match any outer indentation level", offset, -1)
		}
		if altcol != lx.altindstack[top] {
			return lx.tabError()
		}
	}
	return tEnd
}

// Scans one token of regular code, the expression parts of f-strings included
func (lx *lexer) normalMode(m *tokMode) tokKind {
nextline:
	lx.blankline = false
	if lx.atbol {
		if lx.indentation() == tError {
			return tError
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
		if lx.done == doneError || lx.level > 0 {
			return tError
		}
		return lx.tok(tEnd, -1, -1)
	}
	if isIdentStart(c) {
		return lx.nameOrPrefixedString(c)
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
			return tError
		}
		goto again
	}
	return lx.operator(c)
}

// Scans a name, or a string whose prefix letters were read as the start of a name
func (lx *lexer) nameOrPrefixedString(c int) tokKind {
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

// Scans a punctuation lexeme or bracket, tracking bracket nesting and the replacement field ends of f-strings
func (lx *lexer) operator(c int) tokKind {
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
	if c2 != eofChar && c2 < 128 {
		two := string([]byte{byte(c), byte(c2)})
		if twoCharOps[two] && (two != "<>" || lx.barry) {
			c3 := lx.nextc()
			if c3 != eofChar && c3 < 128 && threeCharOps[two+string(rune(c3))] {
				return lx.tok(tOp, lx.start, lx.cur)
			}
			lx.backup(c3)
			return lx.tok(tOp, lx.start, lx.cur)
		}
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

var twoCharOps = map[string]bool{"!=": true, "%=": true, "&=": true, "**": true, "*=": true, "+=": true, "-=": true, "->": true, "//": true, "/=": true, ":=": true, "<<": true, "<=": true, "<>": true, "==": true, ">=": true, ">>": true, "@=": true, "^=": true, "|=": true}

var threeCharOps = map[string]bool{"**=": true, "...": true, "//=": true, "<<=": true, ">>=": true}

// Consumes a backslash newline, returning -2 after recording the failure
func (lx *lexer) continuation() int {
	c := lx.nextc()
	if c == '\r' {
		c = lx.nextc()
	}
	if c != '\n' {
		col := lx.cur - lx.lineStart - 1
		lx.err = &SyntaxError{Msg: "unexpected character after line continuation character", Line: lx.lineno, Col: charCount([]byte(lx.src[lx.lineStart:lx.lineStart+max(col, 0)])) + 1, EndLine: lx.lineno, EndCol: 0, Text: lx.lineText()}
		lx.done = doneError
		return -2
	}
	c = lx.nextc()
	if c == eofChar {
		lx.cur = lx.inp
		lx.err = &SyntaxError{Msg: "unexpected EOF while parsing", Line: lx.lineno, Col: charCount([]byte(lx.src[lx.lineStart:lx.cur])), EndLine: lx.lineno, EndCol: -1, Text: lx.lineText()}
		lx.done = doneError
		return -2
	}
	lx.backup(c)
	return c
}

// Incompatible string prefix pair in the order the reference checks them
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

// Checks a name holding non ASCII characters against XID_Start and XID_Continue
func (lx *lexer) verifyIdentifier() bool {
	s := lx.src[lx.start:lx.cur]
	runes := []rune(s)
	invalid := len(runes)
	for i, r := range runes {
		if i == 0 {
			if r != '_' && !isXIDStart(r) {
				invalid = 0
				break
			}
		} else if !isXIDContinue(r) {
			invalid = i
			break
		}
	}
	if invalid == len(runes) {
		return true
	}
	ch := runes[invalid]
	if invalid+1 < len(runes) {
		lx.cur = lx.start + len(string(runes[:invalid+1]))
	}
	if isPrintable(ch) {
		lx.syntaxError("invalid character '%c' (U+%04X)", ch, ch)
	} else {
		lx.syntaxError("invalid non-printable character U+%04X", ch)
	}
	return false
}

// Code points ID_Start holds that NFKC closure removes from XID_Start
var xidStartDrop = &unicode.RangeTable{R16: []unicode.Range16{
	{0x37a, 0x37a, 1}, {0xe33, 0xe33, 1}, {0xeb3, 0xeb3, 1}, {0x309b, 0x309c, 1}, {0xfc5e, 0xfc63, 1},
	{0xfdfa, 0xfdfb, 1}, {0xfe70, 0xfe7e, 2}, {0xff9e, 0xff9f, 1},
}}

// Code points ID_Continue holds that NFKC closure removes from XID_Continue
var xidContinueDrop = &unicode.RangeTable{R16: []unicode.Range16{
	{0x37a, 0x37a, 1}, {0x309b, 0x309c, 1}, {0xfc5e, 0xfc63, 1}, {0xfdfa, 0xfdfb, 1}, {0xfe70, 0xfe7e, 2},
}}

// Unicode XID_Start, letters, letter numbers and Other_ID_Start minus pattern and NFKC unstable characters
func isXIDStart(r rune) bool {
	if unicode.In(r, unicode.Pattern_Syntax, unicode.Pattern_White_Space) || unicode.Is(xidStartDrop, r) {
		return false
	}
	return unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start)
}

// Unicode XID_Continue, XID_Start plus marks, digits, connectors and Other_ID_Continue
func isXIDContinue(r rune) bool {
	if unicode.In(r, unicode.Pattern_Syntax, unicode.Pattern_White_Space) || unicode.Is(xidContinueDrop, r) {
		return false
	}
	return unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc, unicode.Other_ID_Continue)
}

// Number of characters of a string
func runeLen(s string) int { return utf8.RuneCountInString(s) }
