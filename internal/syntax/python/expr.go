package python

import (
	"math/big"
	"strconv"
	"strings"
)

var keywords = map[string]bool{"False": true, "None": true, "True": true, "and": true, "as": true, "assert": true, "async": true, "await": true, "break": true, "class": true, "continue": true, "def": true, "del": true, "elif": true, "else": true, "except": true, "finally": true, "for": true, "from": true, "global": true, "if": true, "import": true, "in": true, "is": true, "lambda": true, "nonlocal": true, "not": true, "or": true, "pass": true, "raise": true, "return": true, "try": true, "while": true, "with": true, "yield": true}

var softKeywords = map[string]bool{"_": true, "case": true, "match": true, "type": true}

func (p *parser) isName() bool {
	t := p.cur()
	return t.kind == tName && !keywords[t.val]
}

// Reports whether the current token can begin an expression
func (p *parser) startsExpression() bool {
	t := p.cur()
	switch t.kind {
	case tName:
		if !keywords[t.val] {
			return true
		}
		switch t.val {
		case "lambda", "await", "True", "False", "None":
			return true
		case "not":
			n := p.peek(1)
			return !(n.kind == tName && n.val == "in")
		}
		return false
	case tNumber, tString, tFStart, tTStart:
		return true
	case tOp:
		switch t.val {
		case "{", "~", "...":
			return true
		}
	}
	return false
}

// Expression with the conditional form and the comma and legacy print diagnostics
func (p *parser) expression() Expr {
	start := p.pos
	startTok := p.cur()
	if p.isKw("lambda") {
		return p.lambdef()
	}
	a := p.disjunction()
	if p.bad() {
		return nil
	}
	if p.isKw("if") {
		p.next()
		b := p.disjunction()
		if p.bad() {
			return nil
		}
		if !p.isKw("else") {
			if !p.isOp(":") {
				p.raiseRange(a, b, "expected 'else' after 'if' expression")
				return nil
			}
			p.fail()
			return nil
		}
		p.next()
		if !p.startsExpression() && !p.isOp("(", "[", "-", "+", "*") && !p.isKw("yield") {
			p.raiseNextToken("expected expression after 'else', but statement is given")
			return nil
		}
		if !p.enter(1) {
			return nil
		}
		c := p.expression()
		if p.bad() {
			return nil
		}
		p.leave(1)
		return &IfExp{Pos: p.span(startTok), Test: b, Body: a, Orelse: c}
	}
	p.checkMissingComma(start, startTok, a)
	if p.bad() {
		return nil
	}
	return a
}

// Raises the forgot a comma diagnostic when another expression directly follows inside brackets
func (p *parser) checkMissingComma(start int, startTok *token, a Expr) {
	if !p.startsExpression() {
		return
	}
	if startTok.kind == tName && (softKeywords[startTok.val] || p.tk(start+1).kind == tString) {
		return
	}
	if n, ok := a.(*Name); ok && (n.ID == "print" || n.ID == "exec") {
		return
	}
	save := p.pos
	b, ok := probe(p, p.expressionNoInvalid)
	if !ok {
		if p.err == nil {
			p.pos = save
		}
		return
	}
	if p.toks[p.pos-1].level == 0 {
		p.pos = save
		return
	}
	p.raiseRange(a, b, "invalid syntax. Perhaps you forgot a comma?")
}

// Expression without the diagnostic checks, as the speculative parses of diagnostics read it
func (p *parser) expressionNoInvalid() Expr {
	if p.isKw("lambda") {
		return p.lambdef()
	}
	startTok := p.cur()
	a := p.disjunction()
	if p.bad() {
		return nil
	}
	if p.isKw("if") {
		p.next()
		b := p.disjunction()
		if p.bad() {
			return nil
		}
		p.expectKw("else")
		if p.bad() || !p.enter(1) {
			return nil
		}
		c := p.expressionNoInvalid()
		if p.bad() {
			return nil
		}
		p.leave(1)
		return &IfExp{Pos: p.span(startTok), Test: b, Body: a, Orelse: c}
	}
	return a
}

// Named expression with the misplaced assignment diagnostics
func (p *parser) namedExpression() Expr {
	t := p.cur()
	if p.isName() && p.peekOp(1, ":=") {
		name := p.nameNode(Store)
		p.next()
		v := p.expression()
		if p.bad() {
			return nil
		}
		return &NamedExpr{Pos: p.span(t), Target: name, Value: v}
	}
	if p.isName() && p.peekOp(1, "=") {
		save, fill := p.pos, p.fill
		a := p.nameNode(Load)
		p.next()
		if p.startsExpression() || p.isOp("(", "[", "-", "+", "~") {
			if b, ok := speculate(p, p.bitOr); ok && !p.isOp("=", ":=") {
				ap, bp := a.Position(), b.Position()
				p.raiseAt(ap.Line, ap.Col, bp.EndLine, bp.EndCol, "invalid syntax. Maybe you meant '==' or ':=' instead of '='?")
				return nil
			}
			if p.bad() {
				return nil
			}
		}
		p.pos, p.fill = save, fill
	}
	e := p.expression()
	if p.bad() {
		return nil
	}
	if p.isOp(":=") {
		p.raiseNode(e, "cannot use assignment expressions with "+exprName(e))
		return nil
	}
	if p.isOp("=") && !isDisplayTarget(e) && !isSingletonConst(e) && p.inCondition {
		save, fill := p.pos, p.fill
		p.next()
		if p.startsExpression() || p.isOp("(", "[", "-", "+", "~") {
			if _, ok := speculate(p, p.bitOr); ok && !p.isOp("=", ":=") {
				p.raiseNode(e, "cannot assign to "+exprName(e)+" here. Maybe you meant '==' instead of '='?")
				return nil
			}
			if p.bad() {
				return nil
			}
		}
		p.pos, p.fill = save, fill
	}
	return e
}

// Reports whether the expression is a list, tuple or generator expression
func isDisplayTarget(e Expr) bool {
	switch e.(type) {
	case *List, *Tuple, *GeneratorExp:
		return true
	}
	return false
}

// Reports whether the expression is the None, True or False constant
func isSingletonConst(e Expr) bool {
	c, ok := e.(*Constant)
	if !ok {
		return false
	}
	switch c.Value.(type) {
	case None, Bool:
		return true
	}
	return false
}

func (p *parser) starExpressions() Expr {
	t := p.cur()
	a := p.starExpression()
	if p.bad() {
		return nil
	}
	if !p.isOp(",") {
		return a
	}
	elts := []Expr{a}
	for p.isOp(",") {
		p.next()
		if !p.startsStarExpression() {
			break
		}
		e := p.starExpression()
		if p.bad() {
			return nil
		}
		elts = append(elts, e)
	}
	return &Tuple{Pos: p.span(t), Elts: elts, Ctx: Load}
}

func (p *parser) startsStarExpression() bool {
	return p.startsExpression() || p.isOp("*", "(", "[", "-", "+", "~")
}

func (p *parser) starExpression() Expr {
	if p.isOp("*") {
		t := p.next()
		v := p.bitOr()
		if p.bad() {
			return nil
		}
		return &Starred{Pos: p.span(t), Value: v, Ctx: Load}
	}
	return p.expression()
}

// Element of a list or set display with the starred assignment and missing operand diagnostics
func (p *parser) starNamedExpressionSeq() Expr {
	if p.isOp("*") {
		t := p.next()
		if !p.startsExpression() && !p.isOp("(", "[", "-", "+", "~", "*") {
			p.raiseLast("Invalid star expression")
			return nil
		}
		v := p.bitOr()
		if p.bad() {
			return nil
		}
		if p.isOp("=") {
			p.next()
			b := p.expression()
			if p.bad() {
				return nil
			}
			bp := b.Position()
			p.raiseAt(t.l, t.c, bp.EndLine, bp.EndCol, "cannot assign to iterable argument unpacking")
			return nil
		}
		return &Starred{Pos: p.span(t), Value: v, Ctx: Load}
	}
	return p.namedExpression()
}

func (p *parser) starNamedExpression() Expr {
	if p.isOp("*") {
		t := p.next()
		v := p.bitOr()
		if p.bad() {
			return nil
		}
		return &Starred{Pos: p.span(t), Value: v, Ctx: Load}
	}
	return p.namedExpression()
}

func (p *parser) disjunction() Expr { return p.boolChain("or", Or, p.conjunction) }

func (p *parser) conjunction() Expr { return p.boolChain("and", And, p.inversion) }

// Chain of operands joined by one boolean keyword, the single operand returned as is
func (p *parser) boolChain(kw string, op BoolOperator, sub func() Expr) Expr {
	t := p.cur()
	a := sub()
	if p.bad() {
		return nil
	}
	if !p.isKw(kw) {
		return a
	}
	vals := []Expr{a}
	for p.isKw(kw) {
		p.next()
		v := sub()
		if p.bad() {
			return nil
		}
		vals = append(vals, v)
	}
	return &BoolOp{Pos: p.span(t), Op: op, Values: vals}
}

func (p *parser) inversion() Expr {
	if p.isKw("not") {
		t := p.next()
		if !p.enter(1) {
			return nil
		}
		v := p.inversion()
		if p.bad() {
			return nil
		}
		p.leave(1)
		return &UnaryOp{Pos: p.span(t), Op: Not, Operand: v}
	}
	return p.comparison()
}

// Comparison operator at the current token, false when none starts there
func (p *parser) compareOp() (CmpOperator, bool) {
	t := p.cur()
	if t.kind == tOp {
		switch t.val {
		case "==":
			return Eq, true
		case "!=", "<>":
			if t.val == "!=" && p.barry {
				p.raiseLast("with Barry as BDFL, use '<>' instead of '!='")
				return 0, false
			}
			if t.val == "<>" && !p.barry {
				return 0, false
			}
			return NotEq, true
		case "<":
			if n := p.peek(1); n.val == ">" && n.kind == tOp && n.l == t.el && n.c == t.ec {
				p.raiseAt(t.l, t.c, n.el, n.ec, "invalid syntax.  Maybe you meant '!=' instead of '<>'?")
				return 0, false
			}
			return Lt, true
		case "<=":
			return LtE, true
		case ">":
			return Gt, true
		case ">=":
			return GtE, true
		}
		return 0, false
	}
	if t.kind == tName {
		switch t.val {
		case "in":
			return In, true
		case "is":
			if n := p.peek(1); n.kind == tName && n.val == "not" {
				return IsNot, true
			}
			return Is, true
		case "not":
			if n := p.peek(1); n.kind == tName && n.val == "in" {
				return NotIn, true
			}
		}
	}
	return 0, false
}

func (p *parser) comparison() Expr {
	t := p.cur()
	a := p.bitOr()
	if p.bad() {
		return nil
	}
	op, ok := p.compareOp()
	if p.bad() {
		return nil
	}
	if !ok {
		return a
	}
	var ops []CmpOperator
	var comps []Expr
	for ok {
		p.next()
		if op == IsNot || op == NotIn {
			p.next()
		}
		ops = append(ops, op)
		c := p.bitOr()
		if p.bad() {
			return nil
		}
		comps = append(comps, c)
		if op, ok = p.compareOp(); p.bad() {
			return nil
		}
	}
	return &Compare{Pos: p.span(t), Left: a, Ops: ops, Comparators: comps}
}

// Binary operator levels from bitwise or down to the multiplicative operators
const (
	levelBitOr = iota
	levelBitXor
	levelBitAnd
	levelShift
	levelSum
	levelTerm
)

// Operator of a binary level spelled by an operator token
func binaryOp(level int, s string) (BinaryOperator, bool) {
	switch level {
	case levelBitOr:
		return BitOr, s == "|"
	case levelBitXor:
		return BitXor, s == "^"
	case levelBitAnd:
		return BitAnd, s == "&"
	case levelShift:
		switch s {
		case "<<":
			return LShift, true
		case ">>":
			return RShift, true
		}
	case levelSum:
		switch s {
		case "+":
			return Add, true
		case "-":
			return Sub, true
		}
	case levelTerm:
		switch s {
		case "*":
			return Mult, true
		case "/":
			return Div, true
		case "//":
			return FloorDiv, true
		case "%":
			return Mod, true
		case "@":
			return MatMult, true
		}
	}
	return 0, false
}

// Left associative chain of one binary level over the next tighter level
func (p *parser) binLevel(level int, sub func() Expr) Expr {
	t := p.cur()
	a := sub()
	if p.bad() {
		return nil
	}
	for {
		c := p.cur()
		if c.kind != tOp {
			return a
		}
		op, ok := binaryOp(level, c.val)
		if !ok {
			return a
		}
		p.next()
		b := sub()
		if p.bad() {
			return nil
		}
		a = &BinOp{Pos: p.span(t), Left: a, Op: op, Right: b}
	}
}

func (p *parser) bitOr() Expr     { return p.binLevel(levelBitOr, p.bitXor) }
func (p *parser) bitXor() Expr    { return p.binLevel(levelBitXor, p.bitAnd) }
func (p *parser) bitAnd() Expr    { return p.binLevel(levelBitAnd, p.shiftExpr) }
func (p *parser) shiftExpr() Expr { return p.binLevel(levelShift, p.sum) }
func (p *parser) sum() Expr       { return p.binLevel(levelSum, p.term) }
func (p *parser) term() Expr      { return p.binLevel(levelTerm, p.factor) }

func (p *parser) factor() Expr {
	if t := p.cur(); t.kind == tOp {
		op, found := UAdd, true
		switch t.val {
		case "+":
		case "-":
			op = USub
		case "~":
			op = Invert
		default:
			found = false
		}
		if found {
			p.next()
			if !p.enter(1) {
				return nil
			}
			v := p.factor()
			if p.bad() {
				return nil
			}
			p.leave(1)
			return &UnaryOp{Pos: p.span(t), Op: op, Operand: v}
		}
	}
	return p.power()
}

func (p *parser) power() Expr {
	t := p.cur()
	a := p.awaitPrimary()
	if p.bad() {
		return nil
	}
	if p.isOp("**") {
		p.next()
		if !p.enter(2) {
			return nil
		}
		b := p.factor()
		if p.bad() {
			return nil
		}
		p.leave(2)
		return &BinOp{Pos: p.span(t), Left: a, Op: Pow, Right: b}
	}
	return a
}

func (p *parser) awaitPrimary() Expr {
	if p.isKw("await") {
		t := p.next()
		if !p.enter(2) {
			return nil
		}
		v := p.primary()
		if p.bad() {
			return nil
		}
		p.leave(2)
		return &Await{Pos: p.span(t), Value: v}
	}
	return p.primary()
}

// Atom followed by any chain of attribute accesses, calls and subscripts
func (p *parser) primary() Expr {
	t := p.cur()
	a := p.atom()
	if p.bad() {
		return nil
	}
	for {
		switch {
		case p.isOp("."):
			p.next()
			name := p.expectName()
			if p.bad() {
				return nil
			}
			a = &Attribute{Pos: p.span(t), Value: a, Attr: name, Ctx: Load}
		case p.isOp("("):
			c := p.callArgs(t, a, true)
			if p.bad() {
				return nil
			}
			a = c
		case p.isOp("["):
			p.next()
			s := p.slices()
			if p.bad() {
				return nil
			}
			p.expectOp("]")
			if p.bad() {
				return nil
			}
			a = &Subscript{Pos: p.span(t), Value: a, Slice: s, Ctx: Load}
		default:
			return a
		}
	}
}

func (p *parser) slices() Expr {
	t := p.cur()
	a := p.sliceItem()
	if p.bad() {
		return nil
	}
	if !p.isOp(",") {
		if _, ok := a.(*Starred); ok {
			return &Tuple{Pos: p.span(t), Elts: []Expr{a}, Ctx: Load}
		}
		return a
	}
	elts := []Expr{a}
	for p.isOp(",") {
		p.next()
		if p.isOp("]") {
			break
		}
		e := p.sliceItem()
		if p.bad() {
			return nil
		}
		elts = append(elts, e)
	}
	return &Tuple{Pos: p.span(t), Elts: elts, Ctx: Load}
}

func (p *parser) sliceItem() Expr {
	t := p.cur()
	if p.isOp("*") {
		st := p.next()
		v := p.expression()
		if p.bad() {
			return nil
		}
		return &Starred{Pos: p.span(st), Value: v, Ctx: Load}
	}
	var lower, upper, step Expr
	if !p.isOp(":") {
		e := p.namedExpression()
		if p.bad() {
			return nil
		}
		if !p.isOp(":") {
			return e
		}
		if _, ok := e.(*NamedExpr); ok {
			p.fail()
			return nil
		}
		lower = e
	}
	p.next()
	if !p.isOp(":", ",", "]") {
		if upper = p.expression(); p.bad() {
			return nil
		}
	}
	if p.isOp(":") {
		p.next()
		if !p.isOp(",", "]") {
			if step = p.expression(); p.bad() {
				return nil
			}
		}
	}
	return &Slice{Pos: p.span(t), Lower: lower, Upper: upper, Step: step}
}

func (p *parser) nameNode(ctx Context) *Name {
	t := p.cur()
	if t.kind != tName || keywords[t.val] {
		p.fail()
		return nil
	}
	p.next()
	return &Name{Pos: tokSpan(t), ID: normIdent(t.val), Ctx: ctx}
}

func (p *parser) expectName() string {
	t := p.cur()
	if t.kind != tName || keywords[t.val] {
		p.fail()
		return ""
	}
	p.next()
	return normIdent(t.val)
}

func (p *parser) atom() Expr {
	t := p.cur()
	switch t.kind {
	case tName:
		switch t.val {
		case "True":
			p.next()
			return &Constant{Pos: tokSpan(t), Value: Bool(true)}
		case "False":
			p.next()
			return &Constant{Pos: tokSpan(t), Value: Bool(false)}
		case "None":
			p.next()
			return &Constant{Pos: tokSpan(t), Value: None{}}
		}
		if keywords[t.val] {
			p.fail()
			return nil
		}
		p.next()
		return &Name{Pos: tokSpan(t), ID: normIdent(t.val), Ctx: Load}
	case tNumber:
		p.next()
		v := p.numberValue(t)
		if p.bad() {
			return nil
		}
		return &Constant{Pos: tokSpan(t), Value: v}
	case tString, tFStart, tTStart:
		return p.strings()
	case tOp:
		switch t.val {
		case "(":
			return p.parenAtom()
		case "[":
			return p.listAtom()
		case "{":
			return p.braceAtom()
		case "...":
			p.next()
			return &Constant{Pos: tokSpan(t), Value: Ellipsis{}}
		}
	}
	p.fail()
	return nil
}

// Value of a NUMBER token, raising the huge decimal literal error past 4300 digits
func (p *parser) numberValue(t *token) Value {
	s := strings.ReplaceAll(t.val, "_", "")
	last := s[len(s)-1]
	if last == 'j' || last == 'J' {
		f, _ := strconv.ParseFloat(s[:len(s)-1], 64)
		return Complex(complex(0, f))
	}
	if len(s) > 1 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			v, _ := new(big.Int).SetString(s[2:], base)
			return Int{v}
		}
	}
	if strings.ContainsAny(s, ".eE") {
		f, _ := strconv.ParseFloat(s, 64)
		return Float(f)
	}
	if len(s) > maxStrDigits {
		p.raiseKnown(ClassSyntax, t.l, 0, t.el, 0, "Exceeds the limit (4300 digits) for integer string conversion: value has "+strconv.Itoa(len(s))+" digits; use sys.set_int_max_str_digits() to increase the limit - Consider hexadecimal for huge integer literals to avoid decimal conversion limits.")
		return nil
	}
	v, _ := new(big.Int).SetString(s, 10)
	return Int{v}
}

// Comma separated eval input, a tuple when a comma appears
func (p *parser) expressions() Expr {
	t := p.cur()
	first := p.expression()
	if p.bad() {
		return nil
	}
	if !p.isOp(",") {
		return first
	}
	elts := []Expr{first}
	for p.isOp(",") {
		p.next()
		if !p.startsExpression() && !p.isOp("(", "[", "-", "+", "~") {
			break
		}
		e := p.expression()
		if p.bad() {
			return nil
		}
		elts = append(elts, e)
	}
	return &Tuple{Pos: p.span(t), Elts: elts, Ctx: Load}
}
