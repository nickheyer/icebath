package python

import "fmt"

func (p *parser) file() *Module {
	var body []Stmt
	for p.cur().kind != tEnd {
		ss := p.statement()
		if p.bad() {
			return nil
		}
		body = append(body, ss...)
	}
	return &Module{Body: body}
}

// Wraps a single compound statement, nil after a failure
func (p *parser) one(s Stmt) []Stmt {
	if p.bad() || s == nil {
		return nil
	}
	return []Stmt{s}
}

func (p *parser) statement() []Stmt {
	t := p.cur()
	if t.kind == tIndent || t.kind == tDedent {
		p.fail()
		return nil
	}
	if t.kind == tName {
		switch t.val {
		case "def", "if", "class", "with", "for", "try", "while":
			return p.one(p.compound())
		case "async":
			if n := p.peek(1); n.kind == tName && (n.val == "def" || n.val == "with" || n.val == "for") {
				return p.one(p.compound())
			}
		case "match":
			if m := p.matchStmt(); m != nil {
				return []Stmt{m}
			}
			if p.bad() {
				return nil
			}
		}
	}
	if t.kind == tOp && t.val == "@" {
		return p.one(p.compound())
	}
	return p.simpleStmts()
}

func (p *parser) simpleStmts() []Stmt {
	var out []Stmt
	for {
		s := p.simpleStmt()
		if p.bad() {
			return nil
		}
		out = append(out, s)
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
		return nil
	}
	p.next()
	return out
}

func (p *parser) simpleStmt() Stmt {
	t := p.cur()
	if t.kind == tName {
		switch t.val {
		case "return":
			p.next()
			var v Expr
			if p.startsStarExpression() {
				if v = p.starExpressions(); p.bad() {
					return nil
				}
			}
			return &Return{Pos: p.span(t), Value: v}
		case "import":
			return p.importName()
		case "from":
			return p.importFrom()
		case "raise":
			return p.raiseStmt()
		case "pass":
			p.next()
			return &Pass{tokSpan(t)}
		case "break":
			p.next()
			return &Break{tokSpan(t)}
		case "continue":
			p.next()
			return &Continue{tokSpan(t)}
		case "del":
			return p.delStmt()
		case "assert":
			p.next()
			test := p.expression()
			if p.bad() {
				return nil
			}
			var msg Expr
			if p.isOp(",") {
				p.next()
				if msg = p.expression(); p.bad() {
					return nil
				}
			}
			return &Assert{Pos: p.span(t), Test: test, Msg: msg}
		case "global", "nonlocal":
			return p.declaration(t)
		case "type":
			if n := p.peek(1); n.kind == tName && !keywords[n.val] {
				if n2 := p.peek(2); n2.kind == tOp && (n2.val == "=" || n2.val == "[") {
					return p.typeAlias()
				}
			}
		case "print", "exec":
			if n := p.peek(1); !(n.kind == tOp && n.val == "(") && (n.kind == tString || n.kind == tNumber || n.kind == tFStart || (n.kind == tName && !keywords[n.val])) {
				a := p.nameNode(Load)
				if b, ok := probe(p, p.starExpressions); ok {
					p.raiseRange(a, b, fmt.Sprintf("Missing parentheses in call to '%s'. Did you mean %s(...)?", t.val, t.val))
					return nil
				}
				if p.bad() {
					return nil
				}
				p.pos--
			}
		}
	}
	return p.exprOrAssign()
}

func (p *parser) raiseStmt() Stmt {
	t := p.next()
	var exc, cause Expr
	if p.startsExpression() || p.isOp("(", "[", "-", "+") {
		if exc = p.expression(); p.bad() {
			return nil
		}
		if p.isKw("from") {
			p.next()
			if cause = p.expression(); p.bad() {
				return nil
			}
		}
	}
	return &Raise{Pos: p.span(t), Exc: exc, Cause: cause}
}

// Global or nonlocal statement naming one or more variables
func (p *parser) declaration(t *token) Stmt {
	p.next()
	names := []string{p.expectName()}
	for !p.bad() && p.isOp(",") {
		p.next()
		names = append(names, p.expectName())
	}
	if p.bad() {
		return nil
	}
	if t.val == "nonlocal" {
		return &Nonlocal{Pos: p.span(t), Names: names}
	}
	return &Global{Pos: p.span(t), Names: names}
}

// Right hand side of an annotated or augmented assignment
func (p *parser) annotatedRHS() Expr {
	if p.isKw("yield") {
		return p.yieldExpr()
	}
	return p.starExpressions()
}

var augOps = map[string]BinaryOperator{"+=": Add, "-=": Sub, "*=": Mult, "@=": MatMult, "/=": Div, "%=": Mod, "&=": BitAnd, "|=": BitOr, "^=": BitXor, "<<=": LShift, ">>=": RShift, "**=": Pow, "//=": FloorDiv}

// Expression statement, assignment chain, augmented or annotated assignment
func (p *parser) exprOrAssign() Stmt {
	t := p.cur()
	parenName := p.isOp("(")
	first := p.annotatedRHS()
	if p.bad() {
		return nil
	}
	switch {
	case p.isOp(":"):
		return p.annAssign(t, first, parenName)
	case p.isOp("="):
		p.checkAssignHere(t, first)
		if p.bad() {
			return nil
		}
		targets := []Expr{first}
		var value Expr
		for p.isOp("=") {
			p.next()
			rhs := p.annotatedRHS()
			if p.bad() {
				return nil
			}
			if p.isOp("=") {
				targets = append(targets, rhs)
				continue
			}
			value = rhs
		}
		for _, tg := range targets {
			switch tg.(type) {
			case *Yield, *YieldFrom:
				p.raiseNode(tg, "assignment to yield expression not possible")
				return nil
			}
			if invalidTarget(tg, starTargets) != nil {
				p.raiseInvalidTarget(tg, starTargets)
				return nil
			}
			setContext(tg, Store)
		}
		return &Assign{Pos: p.span(t), Targets: targets, Value: value}
	}
	if c := p.cur(); c.kind == tOp {
		if op, ok := augOps[c.val]; ok {
			switch first.(type) {
			case *Name, *Attribute, *Subscript:
			default:
				p.raiseNode(first, "'"+exprName(first)+"' is an illegal expression for augmented assignment")
				return nil
			}
			p.next()
			v := p.annotatedRHS()
			if p.bad() {
				return nil
			}
			setContext(first, Store)
			return &AugAssign{Pos: p.span(t), Target: first, Op: op, Value: v}
		}
	}
	return &ExprStmt{Pos: p.span(t), Value: first}
}

// Annotated assignment after its target, with the single target diagnostics
func (p *parser) annAssign(t *token, first Expr, parenName bool) Stmt {
	if tup, ok := first.(*Tuple); ok && len(tup.Elts) > 0 && !(tup.Col == t.c && p.tokAt(first).kind == tOp) {
		p.raiseNode(tup.Elts[0], "only single target (not tuple) can be annotated")
		return nil
	}
	switch first.(type) {
	case *Name, *Attribute, *Subscript:
	case *List, *Tuple:
		p.raiseNode(first, "only single target (not "+exprName(first)+") can be annotated")
		return nil
	default:
		p.raiseNode(first, "illegal target for annotation")
		return nil
	}
	p.next()
	ann := p.expression()
	if p.bad() {
		return nil
	}
	var v Expr
	if p.isOp("=") {
		p.next()
		if v = p.annotatedRHS(); p.bad() {
			return nil
		}
	}
	_, isName := first.(*Name)
	setContext(first, Store)
	return &AnnAssign{Pos: p.span(t), Target: first, Annotation: ann, Value: v, Simple: isName && !parenName}
}

// Raises the cannot assign here diagnostic for a bitwise or level target followed by another bitwise or expression
func (p *parser) checkAssignHere(start *token, a Expr) {
	if invalidTarget(a, starTargets) != a || !p.bitwiseOrLevel(a) {
		return
	}
	if start.kind == tName && (start.val == "True" || start.val == "None" || start.val == "False") {
		return
	}
	if lead := p.leadingAtom(a); lead != nil {
		lp := lead.Position()
		if lp.Line == start.l && (lp.Col == start.c || p.parens[lead]) {
			switch lead.(type) {
			case *List, *GeneratorExp:
				return
			case *Tuple:
				if start.kind == tOp && start.val == "(" {
					return
				}
			}
		}
	}
	save, fill := p.pos, p.fill
	p.next()
	_, ok := speculate(p, p.bitOr)
	if p.err != nil {
		return
	}
	ok = ok && !p.isOp("=", ":=")
	p.pos, p.fill = save, fill
	if ok {
		p.raiseNode(a, "cannot assign to "+exprName(a)+" here. Maybe you meant '==' instead of '='?")
	}
}

// Reports whether an expression is what the bitwise or rule produces, a parenthesized group included
func (p *parser) bitwiseOrLevel(e Expr) bool {
	if p.parens[e] {
		return true
	}
	switch x := e.(type) {
	case *BoolOp, *Compare, *IfExp, *Lambda, *NamedExpr, *Starred, *Yield, *YieldFrom:
		return false
	case *UnaryOp:
		return x.Op != Not
	}
	return true
}

// Leftmost atom of a primary or binary expression
func (p *parser) leadingAtom(e Expr) Expr {
	for !p.parens[e] {
		switch x := e.(type) {
		case *BinOp:
			e = x.Left
		case *Call:
			e = x.Func
		case *Attribute:
			e = x.Value
		case *Subscript:
			e = x.Value
		default:
			return e
		}
	}
	return e
}

// Token at which a node starts, searched back from the current position
func (p *parser) tokAt(n Expr) token {
	s := n.Position()
	for i := p.pos - 1; i >= 0; i-- {
		if p.toks[i].l == s.Line && p.toks[i].c == s.Col {
			return p.toks[i]
		}
		if p.toks[i].l < s.Line {
			break
		}
	}
	return token{}
}

func (p *parser) delStmt() Stmt {
	t := p.next()
	var targets []Expr
	for {
		e := p.delTarget()
		if p.bad() {
			return nil
		}
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
		return nil
	}
	return &Delete{Pos: p.span(t), Targets: targets}
}

func (p *parser) delTarget() Expr {
	e := p.starTarget()
	if p.bad() {
		return nil
	}
	if invalidTarget(e, delTargets) != nil {
		if p.isOp(",", ";") || p.cur().kind == tNewline {
			p.raiseInvalidTarget(e, delTargets)
			return nil
		}
		p.fail()
		return nil
	}
	setContext(e, Del)
	return e
}

func (p *parser) dottedName() string {
	name := p.expectName()
	for !p.bad() && p.isOp(".") {
		p.next()
		name += "." + p.expectName()
	}
	return name
}

func (p *parser) importName() Stmt {
	t := p.next()
	if p.cur().kind == tNewline {
		p.raiseFrom(p.cur(), "Expected one or more names after 'import'")
		return nil
	}
	var names []*Alias
	for {
		at := p.cur()
		name := p.dottedName()
		if p.bad() {
			return nil
		}
		as := ""
		if p.isKw("as") {
			p.next()
			if as = p.expectName(); p.bad() {
				return nil
			}
		}
		names = append(names, &Alias{Pos: p.span(at), Name: name, Asname: as})
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if p.isKw("from") {
		p.raiseFromTokRange(t, "Did you mean to use 'from ... import ...' instead?")
		return nil
	}
	return &Import{Pos: p.span(t), Names: names}
}

// Raises from token t to the end of its logical line
func (p *parser) raiseFromTokRange(t *token, msg string) {
	for p.cur().kind != tNewline && p.cur().kind != tEnd {
		p.next()
	}
	if p.bad() {
		return
	}
	end := &p.toks[p.pos-1]
	p.raiseAt(t.l, t.c, end.el, end.ec, msg)
}

func (p *parser) importFrom() Stmt {
	t := p.next()
	level := 0
	for p.isOp(".", "...") {
		level += len(p.next().val)
	}
	module := ""
	if !p.isKw("import") {
		if module = p.dottedName(); p.bad() {
			return nil
		}
	}
	if p.expectKw("import"); p.bad() {
		return nil
	}
	var names []*Alias
	switch {
	case p.isOp("*"):
		st := p.next()
		names = []*Alias{{Pos: tokSpan(st), Name: "*"}}
	case p.isOp("("):
		p.next()
		if names = p.importAsNames(); p.bad() {
			return nil
		}
		if p.isOp(",") {
			p.next()
		}
		if p.expectOp(")"); p.bad() {
			return nil
		}
	case p.cur().kind == tNewline:
		p.raiseFrom(p.cur(), "Expected one or more names after 'import'")
		return nil
	default:
		if names = p.importAsNames(); p.bad() {
			return nil
		}
		if p.isOp(",") {
			p.next()
			if p.cur().kind == tNewline {
				p.raiseLast("trailing comma not allowed without surrounding parentheses")
				return nil
			}
			p.fail()
			return nil
		}
	}
	if module == "__future__" && level == 0 {
		for _, a := range names {
			if !p.barry && a.Name == "barry_as_FLUFL" {
				p.barry, p.barryTok = true, p.fill
			}
		}
	}
	return &ImportFrom{Pos: p.span(t), Module: module, Names: names, Level: level}
}

func (p *parser) importAsNames() []*Alias {
	var names []*Alias
	for {
		at := p.cur()
		name := p.expectName()
		if p.bad() {
			return nil
		}
		as := ""
		if p.isKw("as") {
			p.next()
			if as = p.expectName(); p.bad() {
				return nil
			}
		}
		names = append(names, &Alias{Pos: p.span(at), Name: name, Asname: as})
		if !p.isOp(",") {
			return names
		}
		if n := p.peek(1); !(n.kind == tName && !keywords[n.val]) {
			return names
		}
		p.next()
	}
}

func (p *parser) typeAlias() Stmt {
	t := p.next()
	name := p.nameNode(Store)
	if p.bad() {
		return nil
	}
	var params []TypeParam
	if p.isOp("[") {
		if params = p.typeParams(); p.bad() {
			return nil
		}
	}
	if p.expectOp("="); p.bad() {
		return nil
	}
	v := p.expression()
	if p.bad() {
		return nil
	}
	return &TypeAlias{Pos: p.span(t), Name: name, TypeParams: params, Value: v}
}

func (p *parser) typeParams() []TypeParam {
	p.next()
	if p.isOp("]") {
		p.raiseFrom(p.cur(), "Type parameter list cannot be empty")
		return nil
	}
	var out []TypeParam
	for !p.isOp("]") {
		tp := p.typeParam()
		if p.bad() {
			return nil
		}
		out = append(out, tp)
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if p.expectOp("]"); p.bad() {
		return nil
	}
	return out
}

func (p *parser) typeParam() TypeParam {
	t := p.cur()
	switch {
	case p.isOp("*"), p.isOp("**"):
		double := t.val == "**"
		p.next()
		name := p.expectName()
		if p.bad() {
			return nil
		}
		if p.isOp(":") {
			colon := p.cur()
			p.next()
			e := p.expression()
			if p.bad() {
				return nil
			}
			msg := "cannot use bound with TypeVarTuple"
			if double {
				msg = "cannot use bound with ParamSpec"
			}
			ep := e.Position()
			p.raiseAt(colon.l, colon.c, ep.EndLine, ep.EndCol, msg)
			return nil
		}
		var def Expr
		if p.isOp("=") {
			p.next()
			if double {
				def = p.expression()
			} else {
				def = p.starExpression()
			}
			if p.bad() {
				return nil
			}
		}
		if double {
			return &ParamSpec{Pos: p.span(t), Name: name, Default: def}
		}
		return &TypeVarTuple{Pos: p.span(t), Name: name, Default: def}
	}
	name := p.expectName()
	if p.bad() {
		return nil
	}
	var bound, def Expr
	if p.isOp(":") {
		p.next()
		if bound = p.expression(); p.bad() {
			return nil
		}
	}
	if p.isOp("=") {
		p.next()
		if def = p.expression(); p.bad() {
			return nil
		}
	}
	return &TypeVar{Pos: p.span(t), Name: name, Bound: bound, Default: def}
}
