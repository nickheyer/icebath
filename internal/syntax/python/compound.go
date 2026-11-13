package python

import "fmt"

// Statement suite after a header, with the expected indented suite diagnostic
func (p *parser) block(what string, line int) []Stmt {
	if p.cur().kind != tNewline {
		return p.simpleStmts()
	}
	p.next()
	if p.cur().kind != tIndent {
		p.indentError(fmt.Sprintf("expected an indented block after %s on line %d", what, line))
		return nil
	}
	p.next()
	var body []Stmt
	for p.cur().kind != tDedent && p.cur().kind != tEnd {
		ss := p.statement()
		if p.bad() {
			return nil
		}
		body = append(body, ss...)
	}
	if p.cur().kind == tDedent {
		p.next()
	}
	return body
}

// Consumes the separator closing a compound statement header, raising the missing separator error at a line end
func (p *parser) colon() {
	if p.isOp(":") {
		p.next()
		return
	}
	if p.cur().kind == tNewline {
		p.next()
		p.raiseLast("expected ':'")
		return
	}
	p.fail()
}

func (p *parser) compound() Stmt {
	t := p.cur()
	if t.kind == tOp && t.val == "@" {
		return p.decorated()
	}
	isAsync := t.val == "async"
	kw := t.val
	if isAsync {
		kw = p.peek(1).val
	}
	switch kw {
	case "def":
		return p.funcDef()
	case "class":
		return p.classDef()
	case "if":
		return p.ifStmt("if")
	case "while":
		return p.whileStmt()
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

// Decorators followed by the function or class they decorate
func (p *parser) decorated() Stmt {
	var decos []Expr
	for p.isOp("@") {
		p.next()
		d := p.namedExpression()
		if p.bad() {
			return nil
		}
		decos = append(decos, d)
		if p.cur().kind != tNewline {
			p.fail()
			return nil
		}
		p.next()
	}
	switch {
	case p.isKw("def") || (p.isKw("async") && p.peek(1).val == "def"):
		f := p.funcDef()
		if p.bad() {
			return nil
		}
		f.Decorators = decos
		return f
	case p.isKw("class"):
		c := p.classDef()
		if p.bad() {
			return nil
		}
		c.Decorators = decos
		return c
	}
	p.fail()
	return nil
}

func (p *parser) elseBlock() []Stmt {
	if !p.isKw("else") {
		return nil
	}
	t := p.next()
	if p.forceOp(":"); p.bad() {
		return nil
	}
	return p.block("'else' statement", t.l)
}

// Test of an if, elif or while header, parsed with the assignment in test diagnostic
func (p *parser) condition() Expr {
	p.inCondition = true
	test := p.namedExpression()
	if p.bad() {
		return nil
	}
	p.inCondition = false
	return test
}

func (p *parser) ifStmt(kw string) Stmt {
	t := p.next()
	test := p.condition()
	if p.bad() {
		return nil
	}
	if p.colon(); p.bad() {
		return nil
	}
	body := p.block("'"+kw+"' statement", t.l)
	if p.bad() {
		return nil
	}
	var orelse []Stmt
	if p.isKw("elif") {
		if !p.enter(1) {
			return nil
		}
		s := p.ifStmt("elif")
		if p.bad() {
			return nil
		}
		p.leave(1)
		orelse = []Stmt{s}
	} else if orelse = p.elseBlock(); p.bad() {
		return nil
	}
	return &If{Pos: p.span(t), Test: test, Body: body, Orelse: orelse}
}

func (p *parser) whileStmt() Stmt {
	t := p.next()
	test := p.condition()
	if p.bad() {
		return nil
	}
	if p.colon(); p.bad() {
		return nil
	}
	body := p.block("'while' statement", t.l)
	if p.bad() {
		return nil
	}
	orelse := p.elseBlock()
	if p.bad() {
		return nil
	}
	return &While{Pos: p.span(t), Test: test, Body: body, Orelse: orelse}
}

func (p *parser) forStmt(isAsync bool) Stmt {
	t := p.next()
	if isAsync {
		p.next()
	}
	target := p.forTarget()
	if p.bad() {
		return nil
	}
	if !p.isKw("in") {
		p.fail()
		return nil
	}
	p.next()
	iter := p.starExpressions()
	if p.bad() {
		return nil
	}
	if p.colon(); p.bad() {
		return nil
	}
	body := p.block("'for' statement", t.l)
	if p.bad() {
		return nil
	}
	orelse := p.elseBlock()
	if p.bad() {
		return nil
	}
	return &For{Pos: p.span(t), Target: target, Iter: iter, Body: body, Orelse: orelse, Async: isAsync}
}

// Target of a for statement, reporting the first invalid target
func (p *parser) forTarget() Expr {
	save := p.pos
	if n, ok := speculate(p, p.starTargetsNoCheck); ok && p.isKw("in") {
		if invalidTarget(n, forTargets) == nil {
			setContext(n, Store)
			return n
		}
		p.raiseInvalidTarget(n, forTargets)
		return nil
	}
	if p.bad() {
		return nil
	}
	p.pos = save
	if e, ok := speculate(p, p.starExpressions); ok && invalidTarget(e, forTargets) != nil {
		p.raiseInvalidTarget(e, forTargets)
		return nil
	}
	if p.bad() {
		return nil
	}
	p.pos = save
	p.starTargetsNoCheck()
	p.fail()
	return nil
}

func (p *parser) starTargetsNoCheck() Expr {
	t := p.cur()
	a := p.starTarget()
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
		e := p.starTarget()
		if p.bad() {
			return nil
		}
		elts = append(elts, e)
	}
	return &Tuple{Pos: p.span(t), Elts: elts, Ctx: Load}
}

func (p *parser) withItem() *WithItem {
	e := p.expression()
	if p.bad() {
		return nil
	}
	var vars Expr
	if p.isKw("as") {
		p.next()
		target := p.starTarget()
		if p.bad() {
			return nil
		}
		if !p.isOp(",", ")", ":") {
			p.fail()
			return nil
		}
		if vars = p.toTarget(target, starTargets, Store); p.bad() {
			return nil
		}
	}
	return &WithItem{ContextExpr: e, OptionalVars: vars}
}

func (p *parser) withStmt(isAsync bool) Stmt {
	t := p.next()
	if isAsync {
		p.next()
	}
	var items []*WithItem
	if p.isOp("(") {
		save := p.pos
		parenthesized, ok := speculate(p, p.parenthesizedItems)
		if p.bad() {
			return nil
		}
		if ok {
			items = parenthesized
		} else {
			p.pos = save
		}
	}
	if items == nil {
		for {
			it := p.withItem()
			if p.bad() {
				return nil
			}
			items = append(items, it)
			if !p.isOp(",") {
				break
			}
			p.next()
		}
	}
	if p.colon(); p.bad() {
		return nil
	}
	body := p.block("'with' statement", t.l)
	if p.bad() {
		return nil
	}
	return &With{Pos: p.span(t), Items: items, Body: body, Async: isAsync}
}

// Parenthesized with items, failing unless a colon follows so the parenthesis is read as an expression
func (p *parser) parenthesizedItems() []*WithItem {
	p.next()
	var items []*WithItem
	for {
		it := p.withItem()
		if p.bad() {
			return nil
		}
		items = append(items, it)
		if !p.isOp(",") {
			break
		}
		p.next()
		if p.isOp(")") {
			break
		}
	}
	if p.expectOp(")"); p.bad() {
		return nil
	}
	if !p.isOp(":") {
		p.fail()
		return nil
	}
	return items
}

func (p *parser) tryStmt() Stmt {
	t := p.next()
	if p.forceOp(":"); p.bad() {
		return nil
	}
	body := p.block("'try' statement", t.l)
	if p.bad() {
		return nil
	}
	var handlers []*ExceptHandler
	star := -1
	for p.isKw("except") {
		et := p.cur()
		isStar := p.peekOp(1, "*")
		switch {
		case star == -1 && isStar:
			star = 1
		case star == -1:
			star = 0
		case (star == 1) != isStar:
			if isStar {
				n := p.peek(1)
				p.raiseAt(et.l, et.c, n.el, n.ec, "cannot have both 'except' and 'except*' on the same 'try'")
				return nil
			}
			p.raiseTok(et, "cannot have both 'except' and 'except*' on the same 'try'")
			return nil
		}
		h := p.exceptBlock(isStar)
		if p.bad() {
			return nil
		}
		handlers = append(handlers, h)
	}
	var orelse, final []Stmt
	if len(handlers) > 0 {
		if orelse = p.elseBlock(); p.bad() {
			return nil
		}
	}
	if p.isKw("finally") {
		ft := p.next()
		if p.forceOp(":"); p.bad() {
			return nil
		}
		if final = p.block("'finally' statement", ft.l); p.bad() {
			return nil
		}
	}
	if len(handlers) == 0 && final == nil {
		p.cur()
		p.raiseLast("expected 'except' or 'finally' block")
		return nil
	}
	return &Try{Pos: p.span(t), Body: body, Handlers: handlers, Orelse: orelse, Finalbody: final, Star: star == 1}
}

func (p *parser) exceptBlock(isStar bool) *ExceptHandler {
	t := p.next()
	what := "'except' statement"
	if isStar {
		p.next()
		what = "'except*' statement"
		if p.isOp(":") || p.cur().kind == tNewline {
			p.raiseLast("expected one or more exception types")
			return nil
		}
	}
	var typ Expr
	name := ""
	if !p.isOp(":") {
		if p.cur().kind == tNewline {
			p.next()
			p.raiseLast("expected ':'")
			return nil
		}
		st := p.cur()
		if typ = p.expression(); p.bad() {
			return nil
		}
		if p.isOp(",") {
			elts := []Expr{typ}
			for p.isOp(",") {
				p.next()
				if p.isOp(":") {
					break
				}
				e := p.expression()
				if p.bad() {
					return nil
				}
				elts = append(elts, e)
			}
			if p.isKw("as") {
				p.next()
				nt := p.cur()
				if p.expectName(); p.bad() {
					return nil
				}
				tp := typ.Position()
				p.raiseAt(tp.Line, tp.Col, nt.el, nt.ec, "multiple exception types must be parenthesized when using 'as'")
				return nil
			}
			typ = &Tuple{Pos: p.span(st), Elts: elts, Ctx: Load}
		} else if p.isKw("as") {
			p.next()
			if name = p.expectName(); p.bad() {
				return nil
			}
		}
	}
	if p.colon(); p.bad() {
		return nil
	}
	body := p.block(what, t.l)
	if p.bad() {
		return nil
	}
	return &ExceptHandler{Pos: p.span(t), Type: typ, Name: name, Body: body}
}

func (p *parser) funcDef() *FunctionDef {
	t := p.cur()
	isAsync := p.isKw("async")
	if isAsync {
		p.next()
	}
	p.next()
	name := p.expectName()
	if p.bad() {
		return nil
	}
	var tparams []TypeParam
	if p.isOp("[") {
		if tparams = p.typeParams(); p.bad() {
			return nil
		}
	}
	if p.forceOp("("); p.bad() {
		return nil
	}
	args := p.parameters(false)
	if p.bad() {
		return nil
	}
	if p.expectOp(")"); p.bad() {
		return nil
	}
	var returns Expr
	if p.isOp("->") {
		p.next()
		if returns = p.expression(); p.bad() {
			return nil
		}
	}
	if p.forceOp(":"); p.bad() {
		return nil
	}
	body := p.block("function definition", t.l)
	if p.bad() {
		return nil
	}
	return &FunctionDef{Pos: p.span(t), Name: name, Args: args, Body: body, Returns: returns, TypeParams: tparams, Async: isAsync}
}

func (p *parser) classDef() *ClassDef {
	t := p.next()
	name := p.expectName()
	if p.bad() {
		return nil
	}
	var tparams []TypeParam
	if p.isOp("[") {
		if tparams = p.typeParams(); p.bad() {
			return nil
		}
	}
	var bases []Expr
	var kws []*Keyword
	if p.isOp("(") {
		c := p.callArgs(t, nil, false)
		if p.bad() {
			return nil
		}
		bases, kws = c.Args, c.Keywords
	}
	if p.colon(); p.bad() {
		return nil
	}
	body := p.block("class definition", t.l)
	if p.bad() {
		return nil
	}
	return &ClassDef{Pos: p.span(t), Name: name, Bases: bases, Keywords: kws, Body: body, TypeParams: tparams}
}

// Parameters of a def or lambda with the misplaced slash, star and default diagnostics
func (p *parser) parameters(lambda bool) *Arguments {
	closer := ")"
	if lambda {
		closer = ":"
	}
	a := &Arguments{}
	seenSlash, seenStar, seenDefault := false, false, false
	for !p.isOp(closer) {
		t := p.cur()
		switch {
		case p.isOp("/"):
			switch {
			case seenSlash:
				p.raiseTok(t, "/ may appear only once")
			case seenStar:
				p.raiseTok(t, "/ must be ahead of *")
			case len(a.Args) == 0:
				p.raiseTok(t, "at least one argument must precede /")
			}
			if p.bad() {
				return nil
			}
			p.next()
			seenSlash = true
			a.PosOnly, a.Args = a.Args, nil
		case p.isOp("*"):
			if seenStar {
				p.raiseTok(t, "* argument may appear only once")
				return nil
			}
			p.next()
			seenStar = true
			if p.isOp(",") || p.isOp(closer) {
				bare := p.isOp(closer)
				if !bare {
					n := p.peek(1)
					bare = n.kind == tOp && (n.val == "**" || n.val == closer)
				}
				if bare {
					if lambda {
						p.raiseLast("named arguments must follow bare *")
					} else {
						p.raiseTok(t, "named arguments must follow bare *")
					}
					return nil
				}
			} else {
				if a.Vararg = p.param(lambda, true); p.bad() {
					return nil
				}
				if p.isOp("=") {
					p.raiseTok(p.cur(), "var-positional argument cannot have default value")
					return nil
				}
			}
		case p.isOp("**"):
			p.next()
			if a.Kwarg = p.param(lambda, false); p.bad() {
				return nil
			}
			if p.isOp("=") {
				p.raiseTok(p.cur(), "var-keyword argument cannot have default value")
				return nil
			}
			if p.isOp(",") {
				p.next()
				if !p.isOp(closer) {
					if nt := p.cur(); nt.kind == tOp && (nt.val == "*" || nt.val == "**" || nt.val == "/") {
						p.raiseTok(nt, "arguments cannot follow var-keyword argument")
						return nil
					}
					extra := p.param(lambda, false)
					if p.bad() {
						return nil
					}
					p.raiseAt(extra.Line, extra.Col, extra.EndLine, extra.EndCol, "arguments cannot follow var-keyword argument")
					return nil
				}
			}
			if !p.isOp(closer) {
				p.fail()
				return nil
			}
			continue
		case p.isOp("("):
			p.raiseParenthesized(lambda)
			return nil
		default:
			arg := p.param(lambda, false)
			if p.bad() {
				return nil
			}
			var def Expr
			if p.isOp("=") {
				p.next()
				if !p.enter(1) {
					return nil
				}
				if def = p.expression(); p.bad() {
					return nil
				}
				p.leave(1)
			}
			switch {
			case seenStar:
				a.KwOnly = append(a.KwOnly, arg)
				a.KwDefaults = append(a.KwDefaults, def)
			case def != nil:
				seenDefault = true
				a.Defaults = append(a.Defaults, def)
				a.Args = append(a.Args, arg)
			case seenDefault:
				p.raiseAt(arg.Line, arg.Col, arg.EndLine, arg.EndCol, "parameter without a default follows parameter with a default")
				return nil
			default:
				a.Args = append(a.Args, arg)
			}
		}
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if !p.isOp(closer) {
		p.fail()
		return nil
	}
	return a
}

// Raises the parenthesized parameters diagnostic when a parenthesized name list closes
func (p *parser) raiseParenthesized(lambda bool) {
	open := p.next()
	for p.isName() || p.isOp(",") {
		p.next()
	}
	if !p.isOp(")") {
		p.fail()
		return
	}
	cl := p.cur()
	msg := "Function parameters cannot be parenthesized"
	if lambda {
		msg = "Lambda expression parameters cannot be parenthesized"
	}
	p.raiseAt(open.l, open.c, cl.el, cl.ec, msg)
}

func (p *parser) param(lambda, star bool) *Arg {
	t := p.cur()
	name := p.expectName()
	if p.bad() {
		return nil
	}
	var ann Expr
	if !lambda && p.isOp(":") {
		p.next()
		if star {
			ann = p.starExpression()
		} else {
			ann = p.expression()
		}
		if p.bad() {
			return nil
		}
	}
	return &Arg{Pos: p.span(t), Name: name, Annotation: ann}
}
