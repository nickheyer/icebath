package pyast

import (
	"fmt"
	"strings"
)

type failSignal struct{}

type parser struct {
	lx          *lexer
	toks        []token
	pos, fill   int
	level       int
	inCondition bool
	parens      map[*Node]bool
	filename    string
	// fv is the feature_version minor, warnErrors turns warnings into SyntaxErrors as the error filter does
	fv           int
	warnErrors   bool
	warns        []Warning
	warnedEscape map[*token]bool
	rawFString   []bool
	probing      int
	seen, main   int
	mode         string
	barry        bool
	// barryTok is the fill when the barry_as_FLUFL import set the flag, recBarryTok the same from CPython's pass one
	barryTok, recBarryTok int
	barryChecked          bool
	// noFullTokenize marks an error raised after parsing, knownErrLine the line of p->known_err_token
	noFullTokenize bool
	knownErrLine   int
	// sourceFile names the file whose lines exec mode errors quote, read with encoding
	sourceFile, encoding string
}

const maxStack = 6000

func (p *parser) tk(i int) *token {
	if i >= len(p.toks) {
		if p.lx.err != nil {
			if p.fill < len(p.toks)+1 {
				p.fill = len(p.toks) + 1
			}
			panic(p.lx.err)
		}
		i = len(p.toks) - 1
	}
	if i+1 > p.fill {
		p.fill = i + 1
	}
	if i+1 > p.seen {
		p.seen = i + 1
	}
	if p.probing == 0 && i+1 > p.main {
		p.lexWarnings(p.main, i+1)
		p.main = i + 1
	}
	if p.toks[i].kind == tError {
		p.raiseUnclosed()
	}
	return &p.toks[i]
}

// Returns a token already read without repeating the fill, warning and error bookkeeping of tk
func (p *parser) cur() *token {
	if i := p.pos; i < p.main && i < p.fill && i < p.seen && i < len(p.toks) {
		if t := &p.toks[i]; t.kind != tError {
			return t
		}
	}
	return p.tk(p.pos)
}

func (p *parser) next() *token {
	t := p.cur()
	if p.pos < len(p.toks) {
		p.pos++
	}
	return t
}

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

func (p *parser) fail() { panic(failSignal{}) }

func (p *parser) expectOp(op string) *token {
	if !p.isOp(op) {
		p.fail()
	}
	return p.next()
}

func (p *parser) expectKw(kw string) *token {
	if !p.isKw(kw) {
		p.fail()
	}
	return p.next()
}

// Forced token, raising expected 'X' at the token found like the && grammar operator
func (p *parser) forceOp(op string) *token {
	if !p.isOp(op) {
		t := p.cur()
		p.raiseAt(t.l, t.c, t.el, t.ec, "expected '"+op+"'")
	}
	return p.next()
}

// Runs a parse function, restoring the position and reporting false on a generic failure
func (p *parser) speculate(f func() *Node) (n *Node, ok bool) {
	save, level := p.pos, p.level
	defer func() {
		if r := recover(); r != nil {
			if _, isFail := r.(failSignal); isFail {
				p.pos, p.level = save, level
				n, ok = nil, false
				return
			}
			panic(r)
		}
	}()
	return f(), true
}

// Runs a pass two diagnostic parse that leaves the furthest token of pass one untouched
func (p *parser) probe(f func() *Node) (*Node, bool) {
	fill := p.fill
	p.probing++
	defer func() { p.probing-- }()
	n, ok := p.speculate(f)
	p.fill = fill
	return n, ok
}

// Appends the tokenizer warnings issued while reading tokens lo up to hi
func (p *parser) lexWarnings(lo, hi int) {
	for _, w := range p.lx.warns {
		if w.tok >= lo && w.tok < hi {
			w.w.Filename = p.filename
			p.warns = append(p.warns, w.w)
		}
	}
}

// Raises the "X only supported in Python 3.N and greater" error of CHECK_VERSION at the furthest token
func (p *parser) checkVersion(version int, what string) {
	if p.fv < version {
		p.raiseLast(fmt.Sprintf("%s only supported in Python 3.%d and greater", what, version))
	}
}

// Consumes an optional TYPE_COMMENT token, returning its text or nil
func (p *parser) typeComment() any {
	if t := p.cur(); t.kind == tTypeComment {
		p.next()
		return t.val
	}
	return nil
}

func (p *parser) enter(n int) {
	p.level += n
	if p.level > maxStack {
		panic(&LimitError{Class: "MemoryError", Msg: "Parser stack overflowed - Python source too complex to parse"})
	}
}

func (p *parser) leave(n int) { p.level -= n }

func (p *parser) prevEnd() (int, int) {
	for m := p.pos - 1; m >= 0; m-- {
		if t := &p.toks[m]; t.kind != tEnd && t.kind != tNewline && t.kind != tIndent && t.kind != tDedent {
			return t.el, t.ec
		}
	}
	return 1, 0
}

func (p *parser) mkFrom(t *token, typ string, vals ...any) *Node {
	n := NewNode(typ, vals...)
	n.Lineno, n.ColOffset = t.l, t.c
	n.EndLineno, n.EndColOffset = p.prevEnd()
	return n
}

func (p *parser) mkTok(t *token, typ string, vals ...any) *Node {
	n := NewNode(typ, vals...)
	n.Lineno, n.ColOffset, n.EndLineno, n.EndColOffset = t.l, t.c, t.el, t.ec
	return n
}

func mkAt(typ string, l, c, el, ec int, vals ...any) *Node {
	n := NewNode(typ, vals...)
	n.Lineno, n.ColOffset, n.EndLineno, n.EndColOffset = l, c, el, ec
	return n
}

// Source line _PyPegen_raise_error_known_location reports, the tokenizer's current line when lineno lies past it
func (p *parser) errorLine(lineno int) string {
	if p.sourceFile != "" {
		if text, ok := programText(p.sourceFile, lineno, p.encoding); ok {
			return text
		}
	}
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

// Character offset of a one based byte column within a line as _PyPegen_byte_offset_to_character_offset
func byteToCharRaw(line string, col int) int {
	b := []byte(line)
	if col > len(b)+1 {
		col = len(b) + 1
	}
	if col > len(b) {
		return charCount(b) + 1
	}
	return charCount(b[:max(col, 0)])
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
	return string(p.lx.src[s:e])
}

// Character offset of a one based byte column as _PyPegen_byte_offset_to_character_offset
func byteToChar(line string, col int) int {
	b := []byte(line + "\n")
	if col > len(b)+1 {
		col = len(b) + 1
	}
	if col > len(b) {
		return charCount(b) + 1
	}
	return charCount(b[:max(col, 0)])
}

const currentPos = -5

// Raises a SyntaxError at zero based byte columns like RAISE_ERROR_KNOWN_LOCATION
func (p *parser) raiseAt(l, c, el, ec int, msg string) {
	p.raiseClassAt("SyntaxError", l, c, el, ec, msg)
}

func (p *parser) raiseClassAt(class string, l, c, el, ec int, msg string) {
	col, end := c+1, ec+1
	if c == currentPos {
		col = currentPos
	}
	if ec == currentPos {
		end = currentPos
	}
	p.raiseKnown(class, l, col, el, end, msg)
}

func (p *parser) raiseKnown(class string, l, col, el, end int, msg string) {
	if el == currentPos || end == currentPos {
		last := p.tk(p.fill - 1)
		el, end = last.curLine, last.curCol
	}
	text := p.errorLine(l)
	e := &SyntaxError{Class: class, Msg: msg, Lineno: l, Offset: byteToCharRaw(text, col), EndLineno: el, EndOffset: end, Text: strings.TrimSuffix(text, "\n"), HasText: true, HasEnd: true, Filename: p.filename}
	if end > 0 {
		e.EndOffset = byteToCharRaw(p.errorLine(el), end)
	}
	panic(e)
}

func (p *parser) raiseNode(n *Node, msg string) {
	p.raiseAt(n.Lineno, n.ColOffset, n.EndLineno, n.EndColOffset, msg)
}

func (p *parser) raiseRange(a, b *Node, msg string) {
	p.raiseAt(a.Lineno, a.ColOffset, b.EndLineno, b.EndColOffset, msg)
}

func (p *parser) raiseTok(t *token, msg string) { p.raiseAt(t.l, t.c, t.el, t.ec, msg) }

func (p *parser) raiseFrom(t *token, msg string) { p.raiseAt(t.l, t.c, currentPos, currentPos, msg) }

// Raises at the furthest token read like _PyPegen_raise_error without use_mark
func (p *parser) raiseLast(msg string) { p.raiseLastClass("SyntaxError", msg) }

func (p *parser) raiseLastClass(class, msg string) {
	t := p.tk(p.fill - 1)
	p.raiseOnToken(class, t, msg)
}

func (p *parser) raiseOnToken(class string, t *token, msg string) {
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

func (p *parser) raiseNextToken(msg string) { p.raiseOnToken("SyntaxError", p.cur(), msg) }

func (p *parser) indentError(msg string) {
	p.cur()
	p.raiseLastClass("IndentationError", msg)
}

func (p *parser) file() *Node {
	var body []*Node
	for p.cur().kind != tEnd {
		body = append(body, p.statement()...)
	}
	var ignores []*Node
	for _, ti := range p.lx.typeIgnores {
		ignores = append(ignores, NewNode("TypeIgnore", ti.lineno, ti.tag))
	}
	return NewNode("Module", body, ignores)
}

func (p *parser) statement() []*Node {
	t := p.cur()
	if t.kind == tIndent || t.kind == tDedent {
		p.fail()
	}
	if t.kind == tName {
		switch t.val {
		case "def", "if", "class", "with", "for", "try", "while":
			return []*Node{p.compound()}
		case "async":
			if n := p.tk(p.pos + 1); n.kind == tName && (n.val == "def" || n.val == "with" || n.val == "for") {
				return []*Node{p.compound()}
			}
		case "match":
			if m := p.matchStmt(); m != nil {
				return []*Node{m}
			}
		}
	}
	if t.kind == tOp && t.val == "@" {
		return []*Node{p.compound()}
	}
	return p.simpleStmts()
}

func (p *parser) simpleStmts() []*Node {
	var out []*Node
	for {
		out = append(out, p.simpleStmt())
		if !p.isOp(";") {
			break
		}
		p.next()
		if p.cur().kind == tNewline {
			break
		}
	}
	if p.cur().kind != tNewline {
		p.fail()
	}
	p.next()
	return out
}

func (p *parser) simpleStmt() *Node {
	t := p.cur()
	if t.kind == tName {
		switch t.val {
		case "return":
			p.next()
			var v *Node
			if p.startsStarExpression() {
				v = p.starExpressions()
			}
			return p.mkFrom(t, "Return", v)
		case "import":
			return p.importName()
		case "from":
			return p.importFrom()
		case "raise":
			p.next()
			var exc, cause *Node
			if p.startsExpression() || p.isOp("(", "[", "-", "+") {
				exc = p.expression()
				if p.isKw("from") {
					p.next()
					cause = p.expression()
				}
			}
			return p.mkFrom(t, "Raise", exc, cause)
		case "pass":
			p.next()
			return p.mkTok(t, "Pass")
		case "break":
			p.next()
			return p.mkTok(t, "Break")
		case "continue":
			p.next()
			return p.mkTok(t, "Continue")
		case "del":
			return p.delStmt()
		case "assert":
			p.next()
			test := p.expression()
			var msg *Node
			if p.isOp(",") {
				p.next()
				msg = p.expression()
			}
			return p.mkFrom(t, "Assert", test, msg)
		case "global", "nonlocal":
			p.next()
			names := []string{p.expectName()}
			for p.isOp(",") {
				p.next()
				names = append(names, p.expectName())
			}
			typ := "Global"
			if t.val == "nonlocal" {
				typ = "Nonlocal"
			}
			return p.mkFrom(t, typ, names)
		case "type":
			if n := p.tk(p.pos + 1); n.kind == tName && !keywords[n.val] {
				if n2 := p.tk(p.pos + 2); n2.kind == tOp && (n2.val == "=" || n2.val == "[") {
					return p.typeAlias()
				}
			}
		case "print", "exec":
			if n := p.tk(p.pos + 1); !(n.kind == tOp && n.val == "(") && (n.kind == tString || n.kind == tNumber || n.kind == tFStart || (n.kind == tName && !keywords[n.val])) {
				a := p.nameNode(ctxLoad)
				if b, ok := p.probe(p.starExpressions); ok {
					p.raiseRange(a, b, fmt.Sprintf("Missing parentheses in call to '%s'. Did you mean %s(...)?", t.val, t.val))
				}
				p.pos--
			}
		}
	}
	return p.exprOrAssign()
}

func (p *parser) annotatedRHS() *Node {
	if p.isKw("yield") {
		return p.yieldExpr()
	}
	return p.starExpressions()
}

var augOps = map[string]string{"+=": "Add", "-=": "Sub", "*=": "Mult", "@=": "MatMult", "/=": "Div", "%=": "Mod", "&=": "BitAnd", "|=": "BitOr", "^=": "BitXor", "<<=": "LShift", ">>=": "RShift", "**=": "Pow", "//=": "FloorDiv"}

// Expression statement, assignment chain, augmented or annotated assignment
func (p *parser) exprOrAssign() *Node {
	t := p.cur()
	parenName := p.isOp("(")
	var first *Node
	if p.isKw("yield") {
		first = p.yieldExpr()
	} else {
		first = p.starExpressions()
	}
	switch {
	case p.isOp(":"):
		if first.Is("Tuple") && !(first.ColOffset == t.c && p.tokAt(first).kind == tOp) {
			elts := first.Children("elts")
			p.raiseNode(elts[0], "only single target (not tuple) can be annotated")
		}
		if !first.Is("Name", "Attribute", "Subscript") {
			if first.Is("List", "Tuple") {
				p.raiseNode(first, "only single target (not "+exprName(first)+") can be annotated")
			}
			p.raiseNode(first, "illegal target for annotation")
		}
		p.next()
		ann := p.expression()
		var v *Node
		if p.isOp("=") {
			p.next()
			v = p.annotatedRHS()
		}
		simple := 0
		if first.Is("Name") && !parenName {
			simple = 1
		}
		setContext(first, ctxStore)
		n := p.mkFrom(t, "AnnAssign", first, ann, v, simple)
		if simple == 1 {
			p.checkVersion(6, "Variable annotation syntax is")
		}
		p.checkVersion(6, "Variable annotations syntax is")
		return n
	case p.isOp("="):
		p.checkAssignHere(t, first)
		targets := []*Node{first}
		var value *Node
		for p.isOp("=") {
			p.next()
			var rhs *Node
			if p.isKw("yield") {
				rhs = p.yieldExpr()
			} else {
				rhs = p.starExpressions()
			}
			if p.isOp("=") {
				targets = append(targets, rhs)
				continue
			}
			value = rhs
		}
		for _, tg := range targets {
			if tg.Is("Yield", "YieldFrom") {
				p.raiseNode(tg, "assignment to yield expression not possible")
			}
			if invalidTarget(tg, starTargets) != nil {
				p.raiseInvalidTarget(tg, starTargets)
			}
			setContext(tg, ctxStore)
		}
		tc := p.typeComment()
		return p.mkFrom(t, "Assign", targets, value, tc)
	case p.isOp("@=") && p.fv < 5:
		p.next()
		p.checkVersion(5, "The '@' operator is")
	case p.cur().kind == tOp && augOps[p.cur().val] != "":
		if !first.Is("Name", "Attribute", "Subscript") {
			p.raiseNode(first, "'"+exprName(first)+"' is an illegal expression for augmented assignment")
		}
		op := augOps[p.next().val]
		v := p.annotatedRHS()
		setContext(first, ctxStore)
		return p.mkFrom(t, "AugAssign", first, opNode(op), v)
	}
	return p.mkFrom(t, "Expr", first)
}

// Raises the cannot assign here diagnostic of invalid_named_expression for a bitwise_or level target
func (p *parser) checkAssignHere(start *token, a *Node) {
	if invalidTarget(a, starTargets) != a || !p.bitwiseOrLevel(a) {
		return
	}
	if start.kind == tName && (start.val == "True" || start.val == "None" || start.val == "False") {
		return
	}
	if lead := p.leadingAtom(a); lead != nil && lead.Lineno == start.l && (lead.ColOffset == start.c || p.parens[lead]) {
		if lead.Is("List", "GeneratorExp") || (lead.Is("Tuple") && start.kind == tOp && start.val == "(") {
			return
		}
	}
	save, fill := p.pos, p.fill
	p.next()
	_, ok := p.speculate(p.bitOr)
	ok = ok && !p.isOp("=", ":=")
	p.pos, p.fill = save, fill
	if ok {
		p.raiseNode(a, "cannot assign to "+exprName(a)+" here. Maybe you meant '==' instead of '='?")
	}
}

// Whether an expression is what bitwise_or produces, a parenthesized group included
func (p *parser) bitwiseOrLevel(e *Node) bool {
	if p.parens[e] {
		return true
	}
	switch e.Type {
	case "BoolOp", "Compare", "IfExp", "Lambda", "NamedExpr", "Starred", "Yield", "YieldFrom":
		return false
	case "UnaryOp":
		return e.Child("op").Type != "Not"
	}
	return true
}

// Leftmost atom of a primary or binary expression
func (p *parser) leadingAtom(e *Node) *Node {
	for !p.parens[e] {
		switch e.Type {
		case "BinOp":
			e = e.Child("left")
		case "Call":
			e = e.Child("func")
		case "Attribute", "Subscript":
			e = e.Child("value")
		default:
			return e
		}
	}
	return e
}

// Token at which a node starts
func (p *parser) tokAt(n *Node) *token {
	for i := p.pos - 1; i >= 0; i-- {
		if p.toks[i].l == n.Lineno && p.toks[i].c == n.ColOffset {
			return &p.toks[i]
		}
		if p.toks[i].l < n.Lineno {
			break
		}
	}
	return &token{}
}

func (p *parser) delStmt() *Node {
	t := p.next()
	var targets []*Node
	for {
		e := p.delTarget()
		targets = append(targets, e)
		if !p.isOp(",") {
			break
		}
		p.next()
		if p.cur().kind == tNewline || p.isOp(";") {
			break
		}
	}
	if !(p.cur().kind == tNewline || p.isOp(";")) {
		p.fail()
	}
	return p.mkFrom(t, "Delete", targets)
}

func (p *parser) delTarget() *Node {
	e := p.starTarget()
	if invalidTarget(e, delTargets) != nil {
		if p.isOp(",", ";") || p.cur().kind == tNewline {
			p.raiseInvalidTarget(e, delTargets)
		}
		p.fail()
	}
	setContext(e, ctxDel)
	return e
}

func (p *parser) dottedName() string {
	name := p.expectName()
	for p.isOp(".") {
		p.next()
		name += "." + p.expectName()
	}
	return name
}

func (p *parser) importName() *Node {
	t := p.next()
	if p.cur().kind == tNewline {
		p.raiseFrom(p.cur(), "Expected one or more names after 'import'")
	}
	var names []*Node
	for {
		at := p.cur()
		name := p.dottedName()
		var as any
		if p.isKw("as") {
			p.next()
			as = p.expectName()
		}
		names = append(names, p.mkFrom(at, "alias", name, as))
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if p.isKw("from") {
		p.raiseFromTokRange(t, "Did you mean to use 'from ... import ...' instead?")
	}
	return p.mkFrom(t, "Import", names)
}

func (p *parser) raiseFromTokRange(t *token, msg string) {
	for p.cur().kind != tNewline && p.cur().kind != tEnd {
		p.next()
	}
	end := &p.toks[p.pos-1]
	p.raiseAt(t.l, t.c, end.el, end.ec, msg)
}

func (p *parser) importFrom() *Node {
	t := p.next()
	level := 0
	for p.isOp(".", "...") {
		level += len(p.next().val)
	}
	var module any
	if !p.isKw("import") {
		module = p.dottedName()
	}
	p.expectKw("import")
	var names []*Node
	switch {
	case p.isOp("*"):
		st := p.next()
		names = []*Node{p.mkTok(st, "alias", "*", nil)}
	case p.isOp("("):
		p.next()
		names = p.importAsNames()
		if p.isOp(",") {
			p.next()
		}
		p.expectOp(")")
	case p.cur().kind == tNewline:
		p.raiseFrom(p.cur(), "Expected one or more names after 'import'")
	default:
		names = p.importAsNames()
		if p.isOp(",") {
			p.next()
			if p.cur().kind == tNewline {
				p.raiseLast("trailing comma not allowed without surrounding parentheses")
			}
			p.fail()
		}
	}
	if module == "__future__" && level == 0 {
		for _, a := range names {
			if !p.barry && a.Get("name") == "barry_as_FLUFL" {
				p.barry, p.barryTok = true, p.fill
			}
		}
	}
	return p.mkFrom(t, "ImportFrom", module, names, level)
}

func (p *parser) importAsNames() []*Node {
	var names []*Node
	for {
		at := p.cur()
		name := p.expectName()
		var as any
		if p.isKw("as") {
			p.next()
			as = p.expectName()
		}
		names = append(names, p.mkFrom(at, "alias", name, as))
		if !p.isOp(",") || !(p.tk(p.pos+1).kind == tName && !keywords[p.tk(p.pos+1).val]) {
			return names
		}
		p.next()
	}
}

func (p *parser) typeAlias() *Node {
	t := p.next()
	name := p.nameNode(ctxStore)
	var params []*Node
	if p.isOp("[") {
		params = p.typeParams()
	}
	p.expectOp("=")
	v := p.expression()
	n := p.mkFrom(t, "TypeAlias", name, params, v)
	p.checkVersion(12, "Type statement is")
	return n
}

func (p *parser) typeParams() []*Node {
	p.next()
	if p.isOp("]") {
		p.raiseFrom(p.cur(), "Type parameter list cannot be empty")
	}
	var out []*Node
	for !p.isOp("]") {
		t := p.cur()
		switch {
		case p.isOp("*"):
			p.next()
			name := p.expectName()
			if p.isOp(":") {
				colon := p.cur()
				p.next()
				e := p.expression()
				p.raiseAt(colon.l, colon.c, e.EndLineno, e.EndColOffset, "cannot use bound with TypeVarTuple")
			}
			var def *Node
			if p.isOp("=") {
				p.next()
				def = p.starExpression()
				p.checkVersion(13, "Type parameter defaults are")
			}
			out = append(out, p.mkFrom(t, "TypeVarTuple", name, def))
		case p.isOp("**"):
			p.next()
			name := p.expectName()
			if p.isOp(":") {
				colon := p.cur()
				p.next()
				e := p.expression()
				p.raiseAt(colon.l, colon.c, e.EndLineno, e.EndColOffset, "cannot use bound with ParamSpec")
			}
			var def *Node
			if p.isOp("=") {
				p.next()
				def = p.expression()
				p.checkVersion(13, "Type parameter defaults are")
			}
			out = append(out, p.mkFrom(t, "ParamSpec", name, def))
		default:
			name := p.expectName()
			var bound, def *Node
			if p.isOp(":") {
				p.next()
				bound = p.expression()
			}
			if p.isOp("=") {
				p.next()
				def = p.expression()
				p.checkVersion(13, "Type parameter defaults are")
			}
			out = append(out, p.mkFrom(t, "TypeVar", name, bound, def))
		}
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	p.expectOp("]")
	p.checkVersion(12, "Type parameter lists are")
	return out
}

// Statement block after a colon, with the expected indented block diagnostic
func (p *parser) block(what string, line int) []*Node {
	if p.cur().kind == tNewline {
		p.next()
		if p.cur().kind != tIndent {
			p.indentError(fmt.Sprintf("expected an indented block after %s on line %d", what, line))
		}
		p.next()
		var body []*Node
		for p.cur().kind != tDedent && p.cur().kind != tEnd {
			body = append(body, p.statement()...)
		}
		if p.cur().kind == tDedent {
			p.next()
		}
		return body
	}
	return p.simpleStmts()
}

// Requires the colon of a compound statement, raising expected ':' when the line ends first
func (p *parser) colon() {
	if p.isOp(":") {
		p.next()
		return
	}
	if p.cur().kind == tNewline {
		p.next()
		p.raiseLast("expected ':'")
	}
	p.fail()
}

func (p *parser) compound() *Node {
	t := p.cur()
	if t.kind == tOp && t.val == "@" {
		var decos []*Node
		for p.isOp("@") {
			p.next()
			decos = append(decos, p.namedExpression())
			if p.cur().kind != tNewline {
				p.fail()
			}
			p.next()
		}
		var n *Node
		switch {
		case p.isKw("def") || (p.isKw("async") && p.tk(p.pos+1).val == "def"):
			n = p.funcDef()
		case p.isKw("class"):
			n = p.classDef()
		default:
			p.fail()
		}
		n.Set("decorator_list", decos)
		return n
	}
	isAsync := t.val == "async"
	kw := t.val
	if isAsync {
		kw = p.tk(p.pos + 1).val
	}
	switch kw {
	case "def":
		return p.funcDef()
	case "class":
		return p.classDef()
	case "if":
		return p.ifStmt("if")
	case "while":
		p.next()
		p.inCondition = true
		test := p.namedExpression()
		p.inCondition = false
		p.colon()
		body := p.block("'while' statement", t.l)
		orelse := p.elseBlock()
		return p.mkFrom(t, "While", test, body, orelse)
	case "for":
		return p.forStmt(isAsync)
	case "with":
		return p.withStmt(isAsync)
	case "try":
		return p.tryStmt()
	}
	p.fail()
	return nil
}

func (p *parser) elseBlock() []*Node {
	if !p.isKw("else") {
		return nil
	}
	t := p.next()
	p.forceOp(":")
	return p.block("'else' statement", t.l)
}

func (p *parser) ifStmt(kw string) *Node {
	t := p.next()
	p.inCondition = true
	test := p.namedExpression()
	p.inCondition = false
	p.colon()
	body := p.block("'"+kw+"' statement", t.l)
	var orelse []*Node
	if p.isKw("elif") {
		orelse = []*Node{p.ifStmt("elif")}
	} else {
		orelse = p.elseBlock()
	}
	return p.mkFrom(t, "If", test, body, orelse)
}

func (p *parser) forStmt(isAsync bool) *Node {
	t := p.next()
	if isAsync {
		p.next()
	}
	target := p.forTarget()
	if !p.isKw("in") {
		p.fail()
	}
	p.next()
	iter := p.starExpressions()
	p.colon()
	tc := p.typeComment()
	body := p.block("'for' statement", t.l)
	orelse := p.elseBlock()
	typ := "For"
	if isAsync {
		typ = "AsyncFor"
	}
	n := p.mkFrom(t, typ, target, iter, body, orelse, tc)
	if isAsync {
		p.checkVersion(5, "Async for loops are")
	}
	return n
}

// Target of a for statement, reporting the first invalid target like invalid_for_target
func (p *parser) forTarget() *Node {
	save := p.pos
	if n, ok := p.speculate(func() *Node { return p.starTargetsNoCheck() }); ok && p.isKw("in") {
		if invalidTarget(n, forTargets) == nil {
			setContext(n, ctxStore)
			return n
		}
		p.raiseInvalidTarget(n, forTargets)
	}
	p.pos = save
	e, ok := p.speculate(p.starExpressions)
	if ok && invalidTarget(e, forTargets) != nil {
		p.raiseInvalidTarget(e, forTargets)
	}
	p.pos = save
	p.starTargetsNoCheck()
	p.fail()
	return nil
}

func (p *parser) starTargetsNoCheck() *Node {
	t := p.cur()
	a := p.starTarget()
	if !p.isOp(",") {
		return a
	}
	elts := []*Node{a}
	for p.isOp(",") {
		p.next()
		if !p.startsStarExpression() {
			break
		}
		elts = append(elts, p.starTarget())
	}
	return p.mkFrom(t, "Tuple", elts, ctxLoad)
}

func (p *parser) withItem() *Node {
	e := p.expression()
	var vars *Node
	if p.isKw("as") {
		p.next()
		target := p.starTarget()
		if !p.isOp(",", ")", ":") {
			p.fail()
		}
		vars = p.toTarget(target, starTargets, ctxStore)
	}
	return NewNode("withitem", e, vars)
}

func (p *parser) withStmt(isAsync bool) *Node {
	t := p.next()
	if isAsync {
		p.next()
	}
	typ := "With"
	if isAsync {
		typ = "AsyncWith"
	}
	if p.isOp("(") {
		save := p.pos
		if items, ok := p.speculateItems(); ok {
			p.colon()
			var tc any
			if !isAsync {
				tc = p.typeComment()
			}
			body := p.block("'with' statement", t.l)
			n := p.mkFrom(t, typ, items, body, tc)
			if isAsync {
				p.checkVersion(5, "Async with statements are")
			}
			return n
		}
		p.pos = save
	}
	var items []*Node
	for {
		items = append(items, p.withItem())
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	p.colon()
	tc := p.typeComment()
	body := p.block("'with' statement", t.l)
	n := p.mkFrom(t, typ, items, body, tc)
	if isAsync {
		p.checkVersion(5, "Async with statements are")
	}
	return n
}

// Parenthesized with items followed by a colon, or false so the parenthesis is read as an expression
func (p *parser) speculateItems() (items []*Node, ok bool) {
	save := p.pos
	defer func() {
		if r := recover(); r != nil {
			if _, isFail := r.(failSignal); isFail {
				p.pos = save
				items, ok = nil, false
				return
			}
			panic(r)
		}
	}()
	p.next()
	for {
		items = append(items, p.withItem())
		if !p.isOp(",") {
			break
		}
		p.next()
		if p.isOp(")") {
			break
		}
	}
	p.expectOp(")")
	if !p.isOp(":") {
		p.fail()
	}
	return items, true
}

func (p *parser) tryStmt() *Node {
	t := p.next()
	p.forceOp(":")
	body := p.block("'try' statement", t.l)
	var handlers []*Node
	star := -1
	for p.isKw("except") {
		et := p.cur()
		isStar := p.tk(p.pos+1).kind == tOp && p.tk(p.pos+1).val == "*"
		if star == -1 {
			star = map[bool]int{false: 0, true: 1}[isStar]
		} else if (star == 1) != isStar {
			if isStar {
				p.raiseAt(et.l, et.c, p.tk(p.pos+1).el, p.tk(p.pos+1).ec, "cannot have both 'except' and 'except*' on the same 'try'")
			}
			p.raiseTok(et, "cannot have both 'except' and 'except*' on the same 'try'")
		}
		handlers = append(handlers, p.exceptBlock(isStar))
	}
	var orelse, final []*Node
	if len(handlers) > 0 {
		orelse = p.elseBlock()
	}
	if p.isKw("finally") {
		ft := p.next()
		p.forceOp(":")
		final = p.block("'finally' statement", ft.l)
	}
	if len(handlers) == 0 && final == nil {
		p.cur()
		p.raiseLast("expected 'except' or 'finally' block")
	}
	typ := "Try"
	if star == 1 {
		typ = "TryStar"
	}
	n := p.mkFrom(t, typ, body, handlers, orelse, final)
	if star == 1 {
		p.checkVersion(11, "Exception groups are")
	}
	return n
}

func (p *parser) exceptBlock(isStar bool) *Node {
	t := p.next()
	what := "'except' statement"
	bare := false
	if isStar {
		p.next()
		what = "'except*' statement"
		if p.isOp(":") || p.cur().kind == tNewline {
			p.raiseLast("expected one or more exception types")
		}
	}
	var typ *Node
	var name any
	if !p.isOp(":") {
		if p.cur().kind == tNewline {
			p.next()
			p.raiseLast("expected ':'")
		}
		st := p.cur()
		typ = p.expression()
		if p.isOp(",") {
			elts := []*Node{typ}
			for p.isOp(",") {
				p.next()
				if p.isOp(":") {
					break
				}
				elts = append(elts, p.expression())
			}
			if p.isKw("as") {
				p.next()
				nt := p.cur()
				p.expectName()
				p.raiseAt(typ.Lineno, typ.ColOffset, nt.el, nt.ec, "multiple exception types must be parenthesized when using 'as'")
			}
			typ = p.mkFrom(st, "Tuple", elts, ctxLoad)
			bare = true
		} else if p.isKw("as") {
			p.next()
			name = p.expectName()
		}
	}
	p.colon()
	body := p.block(what, t.l)
	n := p.mkFrom(t, "ExceptHandler", typ, name, body)
	if bare {
		p.checkVersion(14, "except expressions without parentheses are")
	}
	return n
}

func (p *parser) funcDef() *Node {
	t := p.cur()
	isAsync := p.isKw("async")
	if isAsync {
		p.next()
	}
	p.next()
	name := p.expectName()
	var tparams []*Node
	if p.isOp("[") {
		tparams = p.typeParams()
	}
	p.forceOp("(")
	args := p.parameters(false)
	p.expectOp(")")
	var returns *Node
	if p.isOp("->") {
		p.next()
		returns = p.expression()
	}
	p.forceOp(":")
	tc := p.funcTypeComment()
	body := p.block("function definition", t.l)
	typ := "FunctionDef"
	if isAsync {
		typ = "AsyncFunctionDef"
	}
	n := p.mkFrom(t, typ, name, args, body, nil, returns, tc, tparams)
	if isAsync {
		p.checkVersion(5, "Async functions are")
	}
	return n
}

// Rule func_type_comment, a type comment after the colon or alone on the next line before the indented body
func (p *parser) funcTypeComment() any {
	if p.cur().kind == tNewline && p.tk(p.pos+1).kind == tTypeComment {
		if p.tk(p.pos+2).kind == tNewline && p.tk(p.pos+3).kind == tIndent {
			p.next()
			return p.next().val
		}
	}
	if p.cur().kind != tTypeComment {
		return nil
	}
	if p.tk(p.pos+1).kind == tNewline && p.tk(p.pos+2).kind == tTypeComment && p.tk(p.pos+3).kind == tNewline && p.tk(p.pos+4).kind == tIndent {
		p.raiseLast("Cannot have two type comments on def")
	}
	return p.next().val
}

func (p *parser) classDef() *Node {
	t := p.next()
	name := p.expectName()
	var tparams, bases, kws []*Node
	if p.isOp("[") {
		tparams = p.typeParams()
	}
	if p.isOp("(") {
		c := p.callArgs(t, nil, false)
		bases, kws = c.Children("args"), c.Children("keywords")
	}
	p.colon()
	body := p.block("class definition", t.l)
	return p.mkFrom(t, "ClassDef", name, bases, kws, body, nil, tparams)
}

// Parameters of a def or lambda with the invalid_parameters diagnostics
func (p *parser) parameters(lambda bool) *Node {
	close := ")"
	if lambda {
		close = ":"
	}
	var posonly, args, kwonly, defaults, kwDefaults []*Node
	var vararg, kwarg *Node
	seenSlash, seenStar, seenDefault := false, false, false
	var slashTok, starTok *token
	for !p.isOp(close) {
		t := p.cur()
		var last *Node
		switch {
		case p.isOp("/"):
			if seenSlash {
				p.raiseTok(t, "/ may appear only once")
			}
			if seenStar {
				p.raiseTok(t, "/ must be ahead of *")
			}
			if len(args) == 0 {
				p.raiseTok(t, "at least one argument must precede /")
			}
			p.next()
			seenSlash, slashTok = true, t
			posonly, args = args, nil
		case p.isOp("*"):
			if seenStar {
				p.raiseTok(t, "* argument may appear only once")
			}
			p.next()
			seenStar, starTok = true, t
			if p.isOp(",") || p.isOp(close) {
				if p.isOp(close) || p.tk(p.pos+1).kind == tOp && (p.tk(p.pos+1).val == "**" || p.tk(p.pos+1).val == close) {
					if lambda {
						p.raiseLast("named arguments must follow bare *")
					}
					p.raiseTok(t, "named arguments must follow bare *")
				}
				if p.isOp(",") && p.tk(p.pos+1).kind == tTypeComment {
					p.raiseLast("bare * has associated type comment")
				}
			} else {
				vararg = p.param(lambda, true)
				last = vararg
				if p.isOp("=") {
					p.raiseTok(p.cur(), "var-positional argument cannot have default value")
				}
			}
		case p.isOp("**"):
			p.next()
			kwarg = p.param(lambda, false)
			if p.isOp("=") {
				p.raiseTok(p.cur(), "var-keyword argument cannot have default value")
			}
			p.argTypeComment(kwarg, lambda, false)
			if p.isOp(",") {
				p.next()
				p.argTypeComment(kwarg, lambda, true)
				if !p.isOp(close) {
					nt := p.cur()
					if nt.kind == tOp && (nt.val == "*" || nt.val == "**" || nt.val == "/") {
						p.raiseTok(nt, "arguments cannot follow var-keyword argument")
					}
					a := p.param(lambda, false)
					p.raiseNode(a, "arguments cannot follow var-keyword argument")
				}
			}
			if !p.isOp(close) {
				p.fail()
			}
			continue
		case p.isOp("("):
			p.raiseParenthesized(lambda)
		default:
			a := p.param(lambda, false)
			last = a
			var def *Node
			if p.isOp("=") {
				p.next()
				def = p.expression()
			}
			if seenStar {
				kwonly = append(kwonly, a)
				kwDefaults = append(kwDefaults, def)
			} else {
				if def != nil {
					seenDefault = true
					defaults = append(defaults, def)
				} else if seenDefault {
					p.raiseNode(a, "parameter without a default follows parameter with a default")
				}
				args = append(args, a)
			}
		}
		p.argTypeComment(last, lambda, false)
		if !p.isOp(",") {
			break
		}
		p.next()
		p.argTypeComment(last, lambda, true)
	}
	_, _ = slashTok, starTok
	if !p.isOp(close) {
		p.fail()
	}
	n := NewNode("arguments", posonly, args, vararg, kwonly, kwDefaults, kwarg, defaults)
	if seenSlash {
		p.checkVersion(8, "Positional-only parameters are")
	}
	return n
}

// Attaches a TYPE_COMMENT after a def parameter, after its comma or right before the closing parenthesis
func (p *parser) argTypeComment(a *Node, lambda, afterComma bool) {
	if a == nil || lambda || p.cur().kind != tTypeComment {
		return
	}
	if !afterComma {
		if n := p.tk(p.pos + 1); n.kind != tOp || n.val != ")" {
			return
		}
	}
	a.Set("type_comment", p.next().val)
}

func (p *parser) raiseParenthesized(lambda bool) {
	open := p.next()
	for p.isName() || p.isOp(",") {
		p.next()
	}
	if p.isOp(")") {
		cl := p.cur()
		if lambda {
			p.raiseAt(open.l, open.c, cl.el, cl.ec, "Lambda expression parameters cannot be parenthesized")
		}
		p.raiseAt(open.l, open.c, cl.el, cl.ec, "Function parameters cannot be parenthesized")
	}
	p.fail()
}

func (p *parser) param(lambda, star bool) *Node {
	t := p.cur()
	name := p.expectName()
	var ann *Node
	if !lambda && p.isOp(":") {
		p.next()
		if star {
			ann = p.starExpression()
		} else {
			ann = p.expression()
		}
	}
	return p.mkFrom(t, "arg", name, ann, nil)
}

// Match statement, or nil when the soft keyword starts an ordinary statement
func (p *parser) matchStmt() *Node {
	t := p.cur()
	save := p.pos
	p.next()
	subject, ok := p.speculate(p.subjectExpr)
	if !ok || !p.isOp(":") {
		newline := ok && p.cur().kind == tNewline
		p.pos = save
		if newline {
			if _, sok := p.speculateStmts(); !sok {
				p.pos = save
				p.next()
				p.subjectExpr()
				p.next()
				p.raiseLast("expected ':'")
			}
			p.pos = save
		}
		return nil
	}
	p.next()
	if p.cur().kind != tNewline {
		p.pos = save
		return nil
	}
	p.next()
	if p.cur().kind != tIndent {
		p.indentError(fmt.Sprintf("expected an indented block after 'match' statement on line %d", t.l))
	}
	p.next()
	var cases []*Node
	for p.isKw("case") {
		cases = append(cases, p.caseBlock())
	}
	if len(cases) == 0 || p.cur().kind != tDedent {
		p.fail()
	}
	p.next()
	n := p.mkFrom(t, "Match", subject, cases)
	p.checkVersion(10, "Pattern matching is")
	return n
}

func (p *parser) speculateStmts() (n *Node, ok bool) {
	return p.speculate(func() *Node { p.simpleStmts(); return nil })
}

func (p *parser) subjectExpr() *Node {
	t := p.cur()
	first := p.starNamedExpression()
	if !p.isOp(",") {
		if first.Is("Starred") {
			p.fail()
		}
		return first
	}
	elts := []*Node{first}
	for p.isOp(",") {
		p.next()
		if p.isOp(":") {
			break
		}
		elts = append(elts, p.starNamedExpression())
	}
	return p.mkFrom(t, "Tuple", elts, ctxLoad)
}

func (p *parser) caseBlock() *Node {
	t := p.next()
	pat := p.patterns()
	var guard *Node
	if p.isKw("if") {
		p.next()
		guard = p.namedExpression()
	}
	p.colon()
	body := p.block("'case' statement", t.l)
	return NewNode("match_case", pat, guard, body)
}

func (p *parser) patterns() *Node {
	t := p.cur()
	first := p.maybeStarPattern()
	if !p.isOp(",") {
		if first.Is("MatchStar") {
			p.fail()
		}
		return first
	}
	pats := []*Node{first}
	for p.isOp(",") {
		p.next()
		if p.isOp(":") || p.isKw("if") {
			break
		}
		pats = append(pats, p.maybeStarPattern())
	}
	return p.mkFrom(t, "MatchSequence", pats)
}

func (p *parser) maybeStarPattern() *Node {
	if p.isOp("*") {
		t := p.next()
		name := p.expectName()
		var v any = name
		if name == "_" {
			v = nil
		}
		return p.mkFrom(t, "MatchStar", v)
	}
	return p.pattern()
}

func (p *parser) pattern() *Node {
	t := p.cur()
	or := p.orPattern()
	if p.isKw("as") {
		p.next()
		nt := p.cur()
		if nt.kind == tName && nt.val == "_" {
			p.raiseTok(nt, "cannot use '_' as a target")
		}
		if !p.isName() {
			if e, ok := p.speculate(p.expression); ok {
				p.raiseNode(e, "invalid pattern target")
			}
			p.fail()
		}
		name := p.expectName()
		if p.isOp(".", "(", "=") {
			p.fail()
		}
		return p.mkFrom(t, "MatchAs", or, name)
	}
	return or
}

func (p *parser) orPattern() *Node {
	t := p.cur()
	first := p.closedPattern()
	if !p.isOp("|") {
		return first
	}
	pats := []*Node{first}
	for p.isOp("|") {
		p.next()
		pats = append(pats, p.closedPattern())
	}
	return p.mkFrom(t, "MatchOr", pats)
}

func (p *parser) signedNumber() *Node {
	t := p.cur()
	if p.isOp("-") {
		p.next()
		nt := p.cur()
		if nt.kind != tNumber {
			p.fail()
		}
		p.next()
		num := p.mkTok(nt, "Constant", p.numberValue(nt), nil)
		return p.mkFrom(t, "UnaryOp", opNode("USub"), num)
	}
	if t.kind != tNumber {
		p.fail()
	}
	p.next()
	return p.mkTok(t, "Constant", p.numberValue(t), nil)
}

// Literal of a pattern or mapping key, the complex literal checks included
func (p *parser) literalExpr() *Node {
	t := p.cur()
	if t.kind == tNumber || p.isOp("-") {
		left := p.signedNumber()
		if p.isOp("+", "-") {
			opTok := p.next()
			real := left
			if real.Is("UnaryOp") {
				real = real.Child("operand")
			}
			if _, isComplex := real.Get("value").(Complex); isComplex {
				p.raiseNode(real, "real number required in complex literal")
			}
			nt := p.cur()
			if nt.kind != tNumber {
				p.fail()
			}
			p.next()
			im := p.mkTok(nt, "Constant", p.numberValue(nt), nil)
			if _, isComplex := im.Get("value").(Complex); !isComplex {
				p.raiseNode(im, "imaginary number required in complex literal")
			}
			op := "Add"
			if opTok.val == "-" {
				op = "Sub"
			}
			return p.mkFrom(t, "BinOp", left, opNode(op), im)
		}
		return left
	}
	if t.kind == tString || t.kind == tFStart || t.kind == tTStart {
		s := p.strings()
		if s.Is("JoinedStr", "TemplateStr") {
			p.raiseNode(s, "patterns may only match literals and attribute lookups")
		}
		return s
	}
	if t.kind == tName {
		switch t.val {
		case "None":
			p.next()
			return p.mkTok(t, "Constant", nil, nil)
		case "True":
			p.next()
			return p.mkTok(t, "Constant", true, nil)
		case "False":
			p.next()
			return p.mkTok(t, "Constant", false, nil)
		}
	}
	p.fail()
	return nil
}

func (p *parser) nameOrAttr() *Node {
	t := p.cur()
	n := p.nameNode(ctxLoad)
	for p.isOp(".") {
		p.next()
		attr := p.expectName()
		n = p.mkFrom(t, "Attribute", n, attr, ctxLoad)
	}
	return n
}

func (p *parser) closedPattern() *Node {
	t := p.cur()
	switch {
	case t.kind == tNumber || p.isOp("-") || t.kind == tString || t.kind == tFStart || t.kind == tTStart:
		v := p.literalExpr()
		return p.mkFrom(t, "MatchValue", v)
	case t.kind == tName && (t.val == "None" || t.val == "True" || t.val == "False"):
		p.next()
		var v any
		if t.val != "None" {
			v = t.val == "True"
		}
		return p.mkTok(t, "MatchSingleton", v)
	case p.isName():
		n := p.nameOrAttr()
		if p.isOp("(") {
			return p.classPattern(t, n)
		}
		if p.isOp("=") {
			p.fail()
		}
		if n.Is("Attribute") {
			return p.mkFrom(t, "MatchValue", n)
		}
		id, _ := n.Str("id")
		if id == "_" {
			return p.mkTok(t, "MatchAs", nil, nil)
		}
		return p.mkTok(t, "MatchAs", nil, id)
	case p.isOp("("):
		p.next()
		if p.isOp(")") {
			p.next()
			return p.mkFrom(t, "MatchSequence", nil)
		}
		first := p.maybeStarPattern()
		if p.isOp(")") && !first.Is("MatchStar") {
			p.next()
			return first
		}
		pats := []*Node{first}
		if !p.isOp(",") {
			p.fail()
		}
		for p.isOp(",") {
			p.next()
			if p.isOp(")") {
				break
			}
			pats = append(pats, p.maybeStarPattern())
		}
		p.expectOp(")")
		return p.mkFrom(t, "MatchSequence", pats)
	case p.isOp("["):
		p.next()
		var pats []*Node
		for !p.isOp("]") {
			pats = append(pats, p.maybeStarPattern())
			if !p.isOp(",") {
				break
			}
			p.next()
		}
		p.expectOp("]")
		return p.mkFrom(t, "MatchSequence", pats)
	case p.isOp("{"):
		return p.mappingPattern()
	}
	p.fail()
	return nil
}

func (p *parser) mappingPattern() *Node {
	t := p.next()
	var keys, pats []*Node
	var rest any
	for !p.isOp("}") {
		if p.isOp("**") {
			p.next()
			rest = p.expectName()
			if p.isOp(",") {
				p.next()
			}
			if !p.isOp("}") {
				p.fail()
			}
			break
		}
		var k *Node
		if p.isName() {
			k = p.nameOrAttr()
			if !k.Is("Attribute") {
				p.fail()
			}
		} else {
			k = p.literalExpr()
		}
		p.expectOp(":")
		keys = append(keys, k)
		pats = append(pats, p.pattern())
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	p.expectOp("}")
	return p.mkFrom(t, "MatchMapping", keys, pats, rest)
}

func (p *parser) classPattern(t *token, cls *Node) *Node {
	p.next()
	var pats, kwPats []*Node
	var kwNames []string
	for !p.isOp(")") {
		if p.isName() && p.tk(p.pos+1).kind == tOp && p.tk(p.pos+1).val == "=" {
			kwNames = append(kwNames, p.expectName())
			p.next()
			kwPats = append(kwPats, p.pattern())
		} else {
			pat := p.pattern()
			if len(kwPats) > 0 {
				last := pat
				for p.isOp(",") && !(p.tk(p.pos+1).kind == tOp && p.tk(p.pos+1).val == ")") {
					save := p.pos
					p.next()
					if p.isName() && p.tk(p.pos+1).kind == tOp && p.tk(p.pos+1).val == "=" {
						p.pos = save
						break
					}
					last = p.pattern()
				}
				p.raiseRange(pat, last, "positional patterns follow keyword patterns")
			}
			pats = append(pats, pat)
		}
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	p.expectOp(")")
	return p.mkFrom(t, "MatchClass", cls, pats, kwNames, kwPats)
}
