package python

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

// Parser state, err holding the first raised error and failed a pending generic failure
type parser struct {
	lx           *lexer
	toks         []token
	pos, fill    int
	level        int
	inCondition  bool
	parens       map[Expr]bool
	barry        bool
	barryTok     int
	recBarryTok  int
	barryChecked bool
	rawFString   []bool
	err          error
	failed       bool
	stop         token
	names        map[string]rune
}

const unknownFile = "<unknown>"

// Parses source bytes as a file, decoding them by byte order mark, coding declaration or UTF-8
func Parse(src []byte, filename string) (*Module, error) {
	if bytes.IndexByte(src, 0) >= 0 {
		return nil, nullBytesError()
	}
	text, serr := decodeSource(translateLines(src, true))
	if serr != nil {
		serr.Filename = fileLabel(filename)
		return nil, serr
	}
	return parseModule(string(text), filename)
}

// Parses already decoded source text as a file, ignoring any coding declaration and reading invalid UTF-8 as U+FFFD
func ParseString(src string, filename string) (*Module, error) {
	if strings.IndexByte(src, 0) >= 0 {
		return nil, nullBytesError()
	}
	return parseModule(string(translateLines([]byte(validText(src)), true)), filename)
}

// File name errors carry, the reference default name for an unnamed source
func fileLabel(filename string) string {
	if filename == "" {
		return unknownFile
	}
	return filename
}

// Parses translated text with the file rule, then applies the future feature and tree depth checks
func parseModule(text, filename string) (*Module, error) {
	n, err := parseLexed(text, false)
	if err != nil {
		return nil, labelled(err, filename)
	}
	m := n.(*Module)
	if serr := futureCheck(m); serr != nil {
		return nil, labelled(serr, filename)
	}
	if treeDepth(m) > maxTreeDepth {
		return nil, &LimitError{Kind: LimitTreeDepth}
	}
	return m, nil
}

// Parses translated text with the eval rule, expressions followed by newlines and the end marker
func parseExpression(text string) (Expr, error) {
	n, err := parseLexed(text, true)
	if err != nil {
		return nil, labelled(err, "")
	}
	e := n.(Expr)
	if treeDepth(e) > maxTreeDepth {
		return nil, &LimitError{Kind: LimitTreeDepth}
	}
	return e, nil
}

// Lexes and parses, lexing again with <> from the token where reference pass one enabled it, when that differs
func parseLexed(text string, eval bool) (Node, error) {
	var tried []int
	for at := -1; ; {
		p := &parser{lx: tokenize(text, at), parens: map[Expr]bool{}, barryTok: -1, recBarryTok: -1}
		n, err := p.run(eval)
		truth := p.barryTok
		if p.barryChecked {
			truth = p.recBarryTok
		}
		next := truth
		if next < 0 {
			next = -2
		}
		tried = append(tried, at)
		if !barryLexDiffers(p.lx.toks, p.lx.barryTok, truth) || slices.Contains(tried, next) {
			return n, err
		}
		at = next
	}
}

// Runs the start rule, then the barry_as_FLUFL second pass error and the reference parser stack check
func (p *parser) run(eval bool) (Node, error) {
	p.toks = p.lx.toks
	if len(p.toks) == 0 && p.lx.err != nil {
		return nil, p.lx.err
	}
	var n Node
	if eval {
		if e := p.evalInput(); e != nil {
			n = e
		}
	} else if m := p.file(); m != nil {
		n = m
	}
	failed := p.bad()
	err := p.result()
	if failed && p.barryRelevant() {
		if ne := p.barryErrorPass(eval); ne != nil {
			if _, limit := err.(*LimitError); !limit {
				err = ne
			}
		}
	}
	if _, limit := err.(*LimitError); !limit && p.stackOverflows(err != nil, eval) {
		return nil, &LimitError{Kind: LimitParserStack}
	}
	if err != nil {
		return nil, err
	}
	return n, nil
}

// Sets the file name of a syntax error, other errors returned unchanged
func labelled(err error, filename string) error {
	if se, ok := err.(*SyntaxError); ok {
		se.Filename = fileLabel(filename)
	}
	return err
}

func (p *parser) evalInput() Expr {
	body := p.expressions()
	if p.bad() {
		return nil
	}
	for p.cur().kind == tNewline {
		p.next()
	}
	if p.cur().kind != tEnd {
		p.fail()
		return nil
	}
	return body
}

// Converts the final parser state into the error the reference raises, nil after a clean parse
func (p *parser) result() error {
	switch {
	case p.err == nil && !p.failed:
		return nil
	case p.err == nil:
		p.failed = false
		return p.genericError()
	}
	se, ok := p.err.(*SyntaxError)
	if !ok || se == p.lx.err {
		return p.err
	}
	return p.fullTokenize(se)
}

// Error of a failed parse with no specific diagnostic, located at the furthest token read
func (p *parser) genericError() error {
	last := p.tk(p.fill - 1)
	if p.err != nil {
		return p.err
	}
	if last.kind == tIndent || last.kind == tDedent {
		msg := "unexpected indent"
		if last.kind == tDedent {
			msg = "unexpected unindent"
		}
		p.raiseLastClass(ClassIndentation, msg)
		return p.err
	}
	se := p.raiseAt(last.l, last.c, last.el, last.ec, "invalid syntax")
	if se == nil {
		return p.err
	}
	return p.fullTokenize(se)
}

// Lets a tokenizer error later in the file or an unclosed bracket replace a parser error as the reference does
func (p *parser) fullTokenize(e *SyntaxError) error {
	lx := p.lx
	if lx.err != nil {
		if lx.errInFString {
			return e
		}
		return lx.err
	}
	if end := p.toks[len(p.toks)-1]; end.kind == tError && len(lx.parenLine) > 0 {
		line := p.toks[max(min(p.fill, len(p.toks)), 1)-1].l
		if line > lx.parenLine[len(lx.parenLine)-1] {
			p.err, p.failed = nil, false
			p.raiseUnclosed()
			return p.err
		}
	}
	return e
}

func (p *parser) raiseUnclosed() {
	lx := p.lx
	top := len(lx.parenstack) - 1
	p.raiseAt(lx.parenLine[top], lx.parenCol[top], lx.parenLine[top], -1, fmt.Sprintf("'%c' was never closed", lx.parenstack[top]))
}

func (p *parser) bad() bool { return p.err != nil || p.failed }

// Token i, recording the furthest token read and surfacing a tokenizer error or unclosed bracket reached there
func (p *parser) tk(i int) *token {
	if p.bad() {
		return &p.stop
	}
	if i >= len(p.toks) {
		if p.lx.err != nil {
			p.fill = max(p.fill, len(p.toks)+1)
			p.err = p.lx.err
			return &p.stop
		}
		i = len(p.toks) - 1
	}
	i = max(i, 0)
	if i+1 > p.fill {
		p.fill = i + 1
	}
	if p.toks[i].kind == tError {
		p.raiseUnclosed()
		return &p.stop
	}
	return &p.toks[i]
}

func (p *parser) cur() *token { return p.tk(p.pos) }

func (p *parser) next() *token {
	t := p.cur()
	if !p.bad() && p.pos < len(p.toks) {
		p.pos++
	}
	return t
}

func (p *parser) peek(n int) *token { return p.tk(p.pos + n) }

func (p *parser) isOp(ops ...string) bool {
	t := p.cur()
	if t.kind != tOp {
		return false
	}
	for _, o := range ops {
		if t.val == o {
			return true
		}
	}
	return false
}

func (p *parser) isKw(kw string) bool {
	t := p.cur()
	return t.kind == tName && t.val == kw
}

// Reports whether the token n places ahead is the given operator
func (p *parser) peekOp(n int, op string) bool {
	t := p.peek(n)
	return t.kind == tOp && t.val == op
}

func (p *parser) fail() {
	if !p.bad() {
		p.failed = true
	}
}

func (p *parser) expectOp(op string) *token {
	if !p.isOp(op) {
		p.fail()
		return &p.stop
	}
	return p.next()
}

func (p *parser) expectKw(kw string) *token {
	if !p.isKw(kw) {
		p.fail()
		return &p.stop
	}
	return p.next()
}

// Forced token, raising expected 'X' at the token found
func (p *parser) forceOp(op string) *token {
	if !p.isOp(op) {
		t := p.cur()
		p.raiseAt(t.l, t.c, t.el, t.ec, "expected '"+op+"'")
		return &p.stop
	}
	return p.next()
}

// Runs a parse function, restoring the position and reporting false on a generic failure
func speculate[T any](p *parser, f func() T) (T, bool) {
	var zero T
	if p.bad() {
		return zero, false
	}
	save, level := p.pos, p.level
	n := f()
	if p.err != nil {
		return zero, false
	}
	if p.failed {
		p.failed = false
		p.pos, p.level = save, level
		return zero, false
	}
	return n, true
}

// Speculation that also leaves the furthest token read untouched
func probe[T any](p *parser, f func() T) (T, bool) {
	fill := p.fill
	n, ok := speculate(p, f)
	if !p.bad() {
		p.fill = fill
	}
	return n, ok
}

// Adds rule levels, raising the parser stack limit past the bound
func (p *parser) enter(n int) bool {
	p.level += n
	if p.level > maxStack {
		if p.err == nil {
			p.err = &LimitError{Kind: LimitParserStack}
		}
		return false
	}
	return true
}

func (p *parser) leave(n int) { p.level -= n }

// End of the last real token before the current position
func (p *parser) prevEnd() (int, int) {
	for m := p.pos - 1; m >= 0; m-- {
		if t := &p.toks[m]; t.kind != tEnd && t.kind != tNewline && t.kind != tIndent && t.kind != tDedent {
			return t.el, t.ec
		}
	}
	return 1, 0
}

// Range from the start of token t to the end of the last token read
func (p *parser) span(t *token) Pos {
	el, ec := p.prevEnd()
	return Pos{t.l, t.c, el, ec}
}

func tokSpan(t *token) Pos { return Pos{t.l, t.c, t.el, t.ec} }

// Source line an error quotes, the furthest line read when lineno lies past it, a newline kept when more follows
func (p *parser) errorLine(lineno int) string {
	if p.fill > 0 {
		if cur := p.toks[min(p.fill, len(p.toks))-1].curLine; cur < lineno {
			lineno = cur
		}
	}
	text := p.lineText(lineno)
	if lineno >= 1 && lineno <= len(p.lx.lineStarts) && p.lx.lineStarts[lineno-1]+len(text) < len(p.lx.src) {
		text += "\n"
	}
	return text
}

func (p *parser) lineText(lineno int) string {
	if lineno < 1 || lineno > len(p.lx.lineStarts) {
		return ""
	}
	s := p.lx.lineStarts[lineno-1]
	e := s
	for e < len(p.lx.src) && p.lx.src[e] != '\n' {
		e++
	}
	return p.lx.src[s:e]
}

// Character offset of a one based byte column within a line
func byteToChar(line string, col int) int {
	if col > len(line)+1 {
		col = len(line) + 1
	}
	if col > len(line) {
		return runeLen(line) + 1
	}
	return runeLen(line[:max(col, 0)])
}

const currentPos = -5

// Raises a syntax error at zero based byte columns, currentPos meaning the end of the furthest token read
func (p *parser) raiseAt(l, c, el, ec int, msg string) *SyntaxError {
	return p.raiseClassAt(ClassSyntax, l, c, el, ec, msg)
}

func (p *parser) raiseClassAt(class ErrorClass, l, c, el, ec int, msg string) *SyntaxError {
	col, end := c+1, ec+1
	if c == currentPos {
		col = currentPos
	}
	if ec == currentPos {
		end = currentPos
	}
	return p.raiseKnown(class, l, col, el, end, msg)
}

// Raises a syntax error at one based byte columns converted to character columns of the quoted lines
func (p *parser) raiseKnown(class ErrorClass, l, col, el, end int, msg string) *SyntaxError {
	if p.bad() {
		return nil
	}
	if el == currentPos || end == currentPos {
		last := p.tk(p.fill - 1)
		if p.bad() {
			return nil
		}
		el, end = last.curLine, last.curCol
	}
	text := p.errorLine(l)
	e := &SyntaxError{Class: class, Msg: msg, Line: l, Col: byteToChar(text, col), EndLine: el, EndCol: end, Text: strings.TrimSuffix(text, "\n")}
	if end > 0 {
		e.EndCol = byteToChar(p.errorLine(el), end)
	}
	p.err = e
	return e
}

func (p *parser) raiseNode(n Expr, msg string) {
	s := n.Position()
	p.raiseAt(s.Line, s.Col, s.EndLine, s.EndCol, msg)
}

func (p *parser) raiseRange(a, b Expr, msg string) {
	s, e := a.Position(), b.Position()
	p.raiseAt(s.Line, s.Col, e.EndLine, e.EndCol, msg)
}

func (p *parser) raiseTok(t *token, msg string) { p.raiseAt(t.l, t.c, t.el, t.ec, msg) }

func (p *parser) raiseFrom(t *token, msg string) { p.raiseAt(t.l, t.c, currentPos, currentPos, msg) }

// Raises at the furthest token read
func (p *parser) raiseLast(msg string) { p.raiseLastClass(ClassSyntax, msg) }

func (p *parser) raiseLastClass(class ErrorClass, msg string) {
	t := p.tk(p.fill - 1)
	if p.bad() {
		return
	}
	p.raiseOnToken(class, t, msg)
}

func (p *parser) raiseOnToken(class ErrorClass, t *token, msg string) {
	col := t.c + 1
	if t.c == -1 {
		col = t.curCol
	}
	end := -1
	if t.ec != -1 {
		end = t.ec + 1
	}
	p.raiseKnown(class, t.l, col, t.el, end, msg)
}

func (p *parser) raiseNextToken(msg string) {
	t := p.cur()
	if p.bad() {
		return
	}
	p.raiseOnToken(ClassSyntax, t, msg)
}

func (p *parser) indentError(msg string) {
	p.cur()
	p.raiseLastClass(ClassIndentation, msg)
}

// Leading from __future__ imports after an optional docstring, rejecting braces and unknown features
func futureCheck(m *Module) *SyntaxError {
	body := m.Body
	i := 0
	if len(body) > 0 {
		if es, ok := body[0].(*ExprStmt); ok {
			if c, ok := es.Value.(*Constant); ok {
				if _, isStr := c.Value.(Str); isStr {
					i++
				}
			}
		}
	}
	for ; i < len(body); i++ {
		s, ok := body[i].(*ImportFrom)
		if !ok || s.Level != 0 || s.Module != "__future__" {
			return nil
		}
		for _, a := range s.Names {
			switch a.Name {
			case "nested_scopes", "generators", "division", "absolute_import", "with_statement", "print_function", "unicode_literals", "barry_as_FLUFL", "generator_stop", "annotations":
			case "braces":
				return futureError("not a chance", a)
			default:
				return futureError("future feature "+firstBytes(a.Name, 100)+" is not defined", a)
			}
		}
	}
	return nil
}

// Truncates text to n bytes short of a split UTF-8 sequence
func firstBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}

// Error at an import alias with one based byte columns and no quoted line
func futureError(msg string, a *Alias) *SyntaxError {
	return &SyntaxError{Msg: msg, Line: a.Line, Col: a.Col + 1, EndLine: a.EndLine, EndCol: a.EndCol + 1}
}
