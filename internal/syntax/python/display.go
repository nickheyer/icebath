package python

func (p *parser) isComprehensionStart() bool {
	if p.isKw("for") {
		return true
	}
	if !p.isKw("async") {
		return false
	}
	n := p.peek(1)
	return n.kind == tName && n.val == "for"
}

func (p *parser) forIfClauses() []*Comprehension {
	var out []*Comprehension
	for p.isComprehensionStart() {
		async := false
		if p.isKw("async") {
			p.next()
			async = true
		}
		p.next()
		target := p.starTargets(forTargets)
		if p.bad() {
			return nil
		}
		if !p.isKw("in") {
			p.fail()
			return nil
		}
		p.next()
		iter := p.disjunction()
		if p.bad() {
			return nil
		}
		var ifs []Expr
		for p.isKw("if") {
			p.next()
			c := p.disjunction()
			if p.bad() {
				return nil
			}
			ifs = append(ifs, c)
		}
		out = append(out, &Comprehension{Target: target, Iter: iter, Ifs: ifs, Async: async})
	}
	return out
}

func lastComprehensionItem(c *Comprehension) Expr {
	if len(c.Ifs) > 0 {
		return c.Ifs[len(c.Ifs)-1]
	}
	return c.Iter
}

// Parenthesized group, tuple, generator expression or yield, with the misplaced star diagnostics
func (p *parser) parenAtom() Expr {
	open := p.next()
	if p.isOp(")") {
		p.next()
		return &Tuple{Pos: p.span(open), Ctx: Load}
	}
	if p.isKw("yield") {
		y := p.yieldExpr()
		if p.bad() {
			return nil
		}
		if p.expectOp(")"); p.bad() {
			return nil
		}
		p.parens[y] = true
		return y
	}
	if p.isOp("**") {
		st := p.next()
		e := p.expression()
		if p.bad() {
			return nil
		}
		if p.isOp(")") {
			ep := e.Position()
			p.raiseAt(st.l, st.c, ep.EndLine, ep.EndCol, "cannot use double starred expression here")
			return nil
		}
		p.fail()
		return nil
	}
	first := p.starNamedExpression()
	if p.bad() {
		return nil
	}
	_, starred := first.(*Starred)
	if starred && p.isOp(")") {
		p.raiseNode(first, "cannot use starred expression here")
		return nil
	}
	if p.isComprehensionStart() {
		if starred {
			p.raiseNode(first, "iterable unpacking cannot be used in comprehension")
			return nil
		}
		gens := p.forIfClauses()
		if p.bad() {
			return nil
		}
		if p.expectOp(")"); p.bad() {
			return nil
		}
		return &GeneratorExp{Pos: p.span(open), Elt: first, Generators: gens}
	}
	if p.isOp(")") {
		p.next()
		p.parens[first] = true
		return first
	}
	if !p.isOp(",") {
		p.fail()
		return nil
	}
	elts := []Expr{first}
	for p.isOp(",") {
		p.next()
		if p.isOp(")") {
			break
		}
		e := p.starNamedExpression()
		if p.bad() {
			return nil
		}
		elts = append(elts, e)
		if p.isComprehensionStart() {
			p.raiseRange(elts[0], elts[len(elts)-1], "did you forget parentheses around the comprehension target?")
			return nil
		}
	}
	if p.expectOp(")"); p.bad() {
		return nil
	}
	return &Tuple{Pos: p.span(open), Elts: elts, Ctx: Load}
}

func (p *parser) listAtom() Expr {
	open := p.next()
	if p.isOp("]") {
		p.next()
		return &List{Pos: p.span(open), Ctx: Load}
	}
	first := p.starNamedExpressionSeq()
	if p.bad() {
		return nil
	}
	if p.isComprehensionStart() {
		if _, ok := first.(*Starred); ok {
			p.raiseNode(first, "iterable unpacking cannot be used in comprehension")
			return nil
		}
		gens := p.forIfClauses()
		if p.bad() {
			return nil
		}
		if p.expectOp("]"); p.bad() {
			return nil
		}
		return &ListComp{Pos: p.span(open), Elt: first, Generators: gens}
	}
	elts, ok := p.displayRest(first, "]")
	if !ok {
		return nil
	}
	return &List{Pos: p.span(open), Elts: elts, Ctx: Load}
}

// Remaining comma separated elements of a list or set display through its closing bracket
func (p *parser) displayRest(first Expr, close string) ([]Expr, bool) {
	elts := []Expr{first}
	for p.isOp(",") {
		p.next()
		if p.isOp(close) {
			break
		}
		e := p.starNamedExpressionSeq()
		if p.bad() {
			return nil, false
		}
		elts = append(elts, e)
		if p.isComprehensionStart() {
			p.raiseRange(elts[0], elts[len(elts)-1], "did you forget parentheses around the comprehension target?")
			return nil, false
		}
	}
	if p.expectOp(close); p.bad() {
		return nil, false
	}
	return elts, true
}

// Dict or set display or comprehension after an opening brace
func (p *parser) braceAtom() Expr {
	open := p.next()
	if p.isOp("}") {
		p.next()
		return &Dict{Pos: p.span(open)}
	}
	if p.isOp("**") {
		return p.dictRest(open, nil, nil)
	}
	if p.isOp("*") {
		return p.setRest(open, nil)
	}
	first := p.namedExpression()
	if p.bad() {
		return nil
	}
	if _, named := first.(*NamedExpr); p.isOp(":") && (!named || p.parens[first]) {
		p.next()
		if p.isOp("*") {
			p.raiseFrom(p.cur(), "cannot use a starred expression in a dictionary value")
			return nil
		}
		if p.isOp("}", ",") {
			p.raiseTok(&p.toks[p.pos-1], "expression expected after dictionary key and ':'")
			return nil
		}
		v := p.expression()
		if p.bad() {
			return nil
		}
		if p.isComprehensionStart() {
			gens := p.forIfClauses()
			if p.bad() {
				return nil
			}
			if p.expectOp("}"); p.bad() {
				return nil
			}
			return &DictComp{Pos: p.span(open), Key: first, Value: v, Generators: gens}
		}
		return p.dictRest(open, []Expr{first}, []Expr{v})
	}
	return p.setRest(open, first)
}

func (p *parser) dictRest(open *token, keys, vals []Expr) Expr {
	if len(keys) > 0 {
		if !p.isOp(",") {
			if p.expectOp("}"); p.bad() {
				return nil
			}
			return &Dict{Pos: p.span(open), Keys: keys, Values: vals}
		}
		p.next()
	}
	for !p.isOp("}") {
		if p.isOp("**") {
			st := p.next()
			v := p.bitOr()
			if p.bad() {
				return nil
			}
			if p.isComprehensionStart() && len(keys) == 0 {
				p.raiseTok(st, "dict unpacking cannot be used in dict comprehension")
				return nil
			}
			keys = append(keys, nil)
			vals = append(vals, v)
		} else {
			k := p.expression()
			if p.bad() {
				return nil
			}
			if !p.isOp(":") {
				kp := k.Position()
				p.raiseAt(kp.Line, kp.EndCol-1, kp.EndLine, -1, "':' expected after dictionary key")
				return nil
			}
			p.next()
			if p.isOp("*") {
				p.raiseFrom(p.cur(), "cannot use a starred expression in a dictionary value")
				return nil
			}
			if p.isOp("}", ",") {
				p.raiseTok(&p.toks[p.pos-1], "expression expected after dictionary key and ':'")
				return nil
			}
			v := p.expression()
			if p.bad() {
				return nil
			}
			keys = append(keys, k)
			vals = append(vals, v)
		}
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if p.expectOp("}"); p.bad() {
		return nil
	}
	return &Dict{Pos: p.span(open), Keys: keys, Values: vals}
}

// Set display or comprehension, its first element already read as a named expression or nil before a star
func (p *parser) setRest(open *token, first Expr) Expr {
	if first == nil {
		if first = p.starNamedExpressionSeq(); p.bad() {
			return nil
		}
	}
	if p.isComprehensionStart() {
		if _, ok := first.(*Starred); ok {
			p.raiseNode(first, "iterable unpacking cannot be used in comprehension")
			return nil
		}
		gens := p.forIfClauses()
		if p.bad() {
			return nil
		}
		if p.expectOp("}"); p.bad() {
			return nil
		}
		return &SetComp{Pos: p.span(open), Elt: first, Generators: gens}
	}
	elts, ok := p.displayRest(first, "}")
	if !ok {
		return nil
	}
	return &Set{Pos: p.span(open), Elts: elts}
}

func (p *parser) yieldExpr() Expr {
	t := p.next()
	if p.isKw("from") {
		p.next()
		v := p.expression()
		if p.bad() {
			return nil
		}
		return &YieldFrom{Pos: p.span(t), Value: v}
	}
	if p.startsStarExpression() {
		v := p.starExpressions()
		if p.bad() {
			return nil
		}
		return &Yield{Pos: p.span(t), Value: v}
	}
	return &Yield{Pos: p.span(t)}
}

// Argument list of a call or class header after its opening parenthesis, a bare generator allowed only in a call
func (p *parser) callArgs(start *token, fn Expr, genexp bool) *Call {
	open := p.next()
	if p.isOp(")") {
		p.next()
		return &Call{Pos: p.span(start), Func: fn}
	}
	var args []Expr
	var kws []*Keyword
	total := 0
	sawKeyword, sawDoubleStar := false, false
	for !p.isOp(")") {
		t := p.cur()
		prev := &p.toks[p.pos-1]
		switch {
		case p.isOp("*"):
			p.next()
			if !p.startsExpression() && !p.isOp("(", "[", "-", "+", "*") {
				p.raiseLast("Invalid star expression")
				return nil
			}
			v := p.expression()
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
			st := &Starred{Pos: p.span(t), Value: v, Ctx: Load}
			if sawDoubleStar {
				p.raiseAt(prev.l, prev.c, st.EndLine, st.EndCol, "iterable argument unpacking follows keyword argument unpacking")
				return nil
			}
			args = append(args, st)
		case p.isOp("**"):
			p.next()
			v := p.expression()
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
				p.raiseAt(t.l, t.c, bp.EndLine, bp.EndCol, "cannot assign to keyword argument unpacking")
				return nil
			}
			kws = append(kws, &Keyword{Pos: p.span(t), Value: v})
			sawDoubleStar = true
		case t.kind == tName && p.peekOp(1, "="):
			if keywords[t.val] {
				if t.val == "True" || t.val == "False" || t.val == "None" {
					eq := p.peek(1)
					p.raiseAt(t.l, t.c, eq.el, eq.ec, "cannot assign to "+t.val)
					return nil
				}
				p.fail()
				return nil
			}
			p.next()
			eq := p.next()
			if p.isOp(",", ")") {
				p.raiseAt(t.l, t.c, eq.el, eq.ec, "expected argument value expression")
				return nil
			}
			v := p.expression()
			if p.bad() {
				return nil
			}
			if p.isComprehensionStart() {
				p.raiseAt(t.l, t.c, eq.el, eq.ec, "invalid syntax. Maybe you meant '==' or ':=' instead of '='?")
				return nil
			}
			kws = append(kws, &Keyword{Pos: p.span(t), Arg: normIdent(t.val), Value: v})
			sawKeyword = true
		default:
			v, ok := p.positionalArg(t)
			if !ok {
				return nil
			}
			if p.isComprehensionStart() {
				return p.callGenerator(start, open, fn, v, genexp, total, len(args))
			}
			if sawDoubleStar {
				p.argumentsError("positional argument follows keyword argument unpacking")
				return nil
			}
			if sawKeyword {
				p.argumentsError("positional argument follows keyword argument")
				return nil
			}
			args = append(args, v)
		}
		total++
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if p.expectOp(")"); p.bad() {
		return nil
	}
	return &Call{Pos: p.span(start), Func: fn, Args: args, Keywords: kws}
}

// Positional argument or assignment expression with the misplaced assignment diagnostics
func (p *parser) positionalArg(t *token) (Expr, bool) {
	var v Expr
	if p.isName() && p.peekOp(1, ":=") {
		name := p.nameNode(Store)
		p.next()
		val := p.expression()
		if p.bad() {
			return nil, false
		}
		v = &NamedExpr{Pos: p.span(t), Target: name, Value: val}
	} else {
		if v = p.expression(); p.bad() {
			return nil, false
		}
		if p.isOp(":=") {
			p.raiseNode(v, "cannot use assignment expressions with "+exprName(v))
			return nil, false
		}
	}
	if _, named := v.(*NamedExpr); p.isOp("=") && !named {
		eq := p.cur()
		vp := v.Position()
		p.raiseAt(vp.Line, vp.Col, eq.el, eq.ec, "expression cannot contain assignment, perhaps you meant \"==\"?")
		return nil, false
	}
	return v, true
}

// Call whose argument starts a generator, accepted only as the sole argument of a call
func (p *parser) callGenerator(start, open *token, fn, v Expr, genexp bool, total, positional int) *Call {
	if !genexp && total == 0 {
		p.fail()
		return nil
	}
	gens := p.forIfClauses()
	if p.bad() {
		return nil
	}
	last := lastComprehensionItem(gens[len(gens)-1])
	if total == 0 && p.isOp(")") {
		p.next()
		g := &GeneratorExp{Pos: p.span(open), Elt: v, Generators: gens}
		return &Call{Pos: p.span(start), Func: fn, Args: []Expr{g}}
	}
	if total == 0 || positional+1 > 1 {
		p.raiseRange(v, last, "Generator expression must be parenthesized")
		return nil
	}
	p.raiseRange(v, gens[len(gens)-1].Target, "Generator expression must be parenthesized")
	return nil
}

// Reports the misplaced positional argument at the furthest token after reading the rest of the arguments
func (p *parser) argumentsError(msg string) {
	for !p.isOp(")") && p.cur().kind != tEnd && p.cur().kind != tNewline {
		if p.isOp(",") {
			p.next()
			continue
		}
		if _, ok := speculate(p, p.expression); !ok {
			break
		}
	}
	if p.bad() {
		return
	}
	p.cur()
	p.raiseLast(msg)
}

func (p *parser) lambdef() Expr {
	t := p.next()
	args := p.parameters(true)
	if p.bad() {
		return nil
	}
	colon := p.cur()
	if p.expectOp(":"); p.bad() {
		return nil
	}
	if nt := p.cur(); nt.kind == tFMiddle || nt.kind == tTMiddle {
		kind := "f"
		if nt.kind == tTMiddle {
			kind = "t"
		}
		p.raiseAt(t.l, t.c, colon.el, colon.ec, kind+"-string: lambda expressions are not allowed without parentheses")
		return nil
	}
	if !p.enter(2) {
		return nil
	}
	body := p.expression()
	if p.bad() {
		return nil
	}
	p.leave(2)
	return &Lambda{Pos: p.span(t), Args: args, Body: body}
}
