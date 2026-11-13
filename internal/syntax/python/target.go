package python

// Target kinds whose invalid sub expressions differ
const (
	starTargets = iota
	forTargets
	delTargets
)

// Star targets as an expression converted to store targets
func (p *parser) starTargets(kind int) Expr {
	t := p.cur()
	a := p.starTarget()
	if p.bad() {
		return nil
	}
	if !p.isOp(",") {
		return p.toTarget(a, kind, Store)
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
	return p.toTarget(&Tuple{Pos: p.span(t), Elts: elts, Ctx: Load}, kind, Store)
}

func (p *parser) starTarget() Expr {
	if p.isOp("*") {
		t := p.next()
		if p.isOp("*") {
			p.fail()
			return nil
		}
		v := p.starTarget()
		if p.bad() {
			return nil
		}
		return &Starred{Pos: p.span(t), Value: v, Ctx: Load}
	}
	return p.bitOr()
}

// Description of an expression kind as target and assignment diagnostics name it
func exprName(e Expr) string {
	switch x := e.(type) {
	case *Attribute:
		return "attribute"
	case *Subscript:
		return "subscript"
	case *Starred:
		return "starred"
	case *Name:
		return "name"
	case *List:
		return "list"
	case *Tuple:
		return "tuple"
	case *Lambda:
		return "lambda"
	case *Call:
		return "function call"
	case *BoolOp, *BinOp, *UnaryOp:
		return "expression"
	case *GeneratorExp:
		return "generator expression"
	case *Yield, *YieldFrom:
		return "yield expression"
	case *Await:
		return "await expression"
	case *ListComp:
		return "list comprehension"
	case *SetComp:
		return "set comprehension"
	case *DictComp:
		return "dict comprehension"
	case *Dict:
		return "dict literal"
	case *Set:
		return "set display"
	case *JoinedStr, *FormattedValue:
		return "f-string expression"
	case *TemplateStr, *Interpolation:
		return "t-string expression"
	case *Constant:
		switch v := x.Value.(type) {
		case None:
			return "None"
		case Bool:
			if v {
				return "True"
			}
			return "False"
		case Ellipsis:
			return "ellipsis"
		}
		return "literal"
	case *Compare:
		return "comparison"
	case *IfExp:
		return "conditional expression"
	case *NamedExpr:
		return "named expression"
	}
	return "Slice"
}

// First sub expression that cannot be a target of the given kind, nil when every part can
func invalidTarget(e Expr, kind int) Expr {
	switch x := e.(type) {
	case *List:
		return invalidTargetOf(x.Elts, kind)
	case *Tuple:
		return invalidTargetOf(x.Elts, kind)
	case *Starred:
		if kind == delTargets {
			return e
		}
		return invalidTarget(x.Value, kind)
	case *Compare:
		if kind == forTargets {
			if len(x.Ops) > 0 && x.Ops[0] == In {
				return invalidTarget(x.Left, kind)
			}
			return nil
		}
		return e
	case *Name, *Subscript, *Attribute:
		return nil
	}
	return e
}

func invalidTargetOf(elts []Expr, kind int) Expr {
	for _, c := range elts {
		if bad := invalidTarget(c, kind); bad != nil {
			return bad
		}
	}
	return nil
}

func (p *parser) raiseInvalidTarget(e Expr, kind int) {
	bad := invalidTarget(e, kind)
	switch {
	case bad == nil:
		p.raiseLast("invalid syntax")
	case kind == delTargets:
		p.raiseNode(bad, "cannot delete "+exprName(bad))
	default:
		p.raiseNode(bad, "cannot assign to "+exprName(bad))
	}
}

// Sets the store or delete context through a target, raising the target error for an invalid one
func (p *parser) toTarget(e Expr, kind int, ctx Context) Expr {
	_, starred := e.(*Starred)
	if bad := invalidTarget(e, kind); bad != nil || (kind == delTargets && starred) {
		p.raiseInvalidTarget(e, kind)
		return nil
	}
	setContext(e, ctx)
	return e
}

func setContext(e Expr, ctx Context) {
	switch x := e.(type) {
	case *Name:
		x.Ctx = ctx
	case *Attribute:
		x.Ctx = ctx
	case *Subscript:
		x.Ctx = ctx
	case *Starred:
		x.Ctx = ctx
		setContext(x.Value, ctx)
	case *List:
		x.Ctx = ctx
		for _, c := range x.Elts {
			setContext(c, ctx)
		}
	case *Tuple:
		x.Ctx = ctx
		for _, c := range x.Elts {
			setContext(c, ctx)
		}
	}
}
