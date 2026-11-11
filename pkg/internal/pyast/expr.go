package pyast

var keywords = map[string]bool{"False": true, "None": true, "True": true, "and": true, "as": true, "assert": true, "async": true, "await": true, "break": true, "class": true, "continue": true, "def": true, "del": true, "elif": true, "else": true, "except": true, "finally": true, "for": true, "from": true, "global": true, "if": true, "import": true, "in": true, "is": true, "lambda": true, "nonlocal": true, "not": true, "or": true, "pass": true, "raise": true, "return": true, "try": true, "while": true, "with": true, "yield": true}

var softKeywords = map[string]bool{"_": true, "case": true, "match": true, "type": true}

var (
	ctxLoad  = NewNode("Load")
	ctxStore = NewNode("Store")
	ctxDel   = NewNode("Del")
)

var binOps = map[string]string{"+": "Add", "-": "Sub", "*": "Mult", "@": "MatMult", "/": "Div", "%": "Mod", "**": "Pow", "<<": "LShift", ">>": "RShift", "|": "BitOr", "^": "BitXor", "&": "BitAnd", "//": "FloorDiv"}

var opNodes = map[string]*Node{}

func opNode(name string) *Node {
	if n, ok := opNodes[name]; ok {
		return n
	}
	n := NewNode(name)
	opNodes[name] = n
	return n
}

func init() {
	for _, name := range []string{"Add", "Sub", "Mult", "MatMult", "Div", "Mod", "Pow", "LShift", "RShift", "BitOr", "BitXor", "BitAnd", "FloorDiv", "And", "Or", "Invert", "Not", "UAdd", "USub", "Eq", "NotEq", "Lt", "LtE", "Gt", "GtE", "Is", "IsNot", "In", "NotIn"} {
		opNode(name)
	}
}

func (p *parser) isName() bool {
	t := p.cur()
	return t.kind == tName && !keywords[t.val]
}

// Whether the current token can begin an expression
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
			return !(p.tk(p.pos+1).kind == tName && p.tk(p.pos+1).val == "in")
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

// expression with the comma and legacy print diagnostics of invalid_expression
func (p *parser) expression() *Node {
	start := p.pos
	startTok := p.cur()
	if p.isKw("lambda") {
		return p.lambdef()
	}
	a := p.disjunction()
	if p.isKw("if") {
		p.next()
		b := p.disjunction()
		if !p.isKw("else") {
			if !p.isOp(":") {
				p.raiseRange(a, b, "expected 'else' after 'if' expression")
			}
			p.fail()
		}
		p.next()
		if !p.startsExpression() && !p.isOp("(", "[", "-", "+", "*") && !p.isKw("yield") {
			p.raiseNextToken("expected expression after 'else', but statement is given")
		}
		p.enter(1)
		c := p.expression()
		p.leave(1)
		return p.mkFrom(startTok, "IfExp", b, a, c)
	}
	p.checkMissingComma(start, startTok, a)
	return a
}

// Raises the forgot a comma diagnostic when another expression directly follows inside brackets
func (p *parser) checkMissingComma(start int, startTok *token, a *Node) {
	if !p.startsExpression() {
		return
	}
	if startTok.kind == tName && (softKeywords[startTok.val] || p.tk(start+1).kind == tString) {
		return
	}
	if a.Is("Name") {
		id, _ := a.Str("id")
		if id == "print" || id == "exec" {
			return
		}
	}
	save := p.pos
	b, ok := p.probe(p.expressionNoInvalid)
	if !ok {
		p.pos = save
		return
	}
	if p.toks[p.pos-1].level == 0 {
		p.pos = save
		return
	}
	p.raiseRange(a, b, "invalid syntax. Perhaps you forgot a comma?")
}

func (p *parser) expressionNoInvalid() *Node {
	if p.isKw("lambda") {
		return p.lambdef()
	}
	startTok := p.cur()
	a := p.disjunction()
	if p.isKw("if") {
		p.next()
		b := p.disjunction()
		p.expectKw("else")
		c := p.expressionNoInvalid()
		return p.mkFrom(startTok, "IfExp", b, a, c)
	}
	return a
}

func (p *parser) namedExpression() *Node {
	t := p.cur()
	if p.isName() && p.tk(p.pos+1).kind == tOp && p.tk(p.pos+1).val == ":=" {
		name := p.nameNode(ctxStore)
		p.next()
		v := p.expression()
		n := p.mkFrom(t, "NamedExpr", name, v)
		p.checkVersion(8, "Assignment expressions are")
		return n
	}
	if p.isName() && p.tk(p.pos+1).kind == tOp && p.tk(p.pos+1).val == "=" {
		save, fill := p.pos, p.fill
		a := p.nameNode(ctxLoad)
		p.next()
		if p.startsExpression() || p.isOp("(", "[", "-", "+", "~") {
			if b, ok := p.speculate(p.bitOr); ok && !p.isOp("=", ":=") {
				p.raiseAt(a.Lineno, a.ColOffset, b.EndLineno, b.EndColOffset, "invalid syntax. Maybe you meant '==' or ':=' instead of '='?")
			}
		}
		p.pos, p.fill = save, fill
	}
	e := p.expression()
	if p.isOp(":=") {
		p.raiseNode(e, "cannot use assignment expressions with "+exprName(e))
	}
	if p.isOp("=") && !e.Is("List", "Tuple", "GeneratorExp") && !isSingletonConst(e) && p.inCondition {
		save, fill := p.pos, p.fill
		p.next()
		if p.startsExpression() || p.isOp("(", "[", "-", "+", "~") {
			if _, ok := p.speculate(p.bitOr); ok && !p.isOp("=", ":=") {
				p.raiseNode(e, "cannot assign to "+exprName(e)+" here. Maybe you meant '==' instead of '='?")
			}
		}
		p.pos, p.fill = save, fill
	}
	return e
}

func isSingletonConst(e *Node) bool {
	if !e.Is("Constant") {
		return false
	}
	switch e.Get("value").(type) {
	case nil, bool:
		return true
	}
	return false
}

func (p *parser) starExpressions() *Node {
	t := p.cur()
	a := p.starExpression()
	if !p.isOp(",") {
		return a
	}
	elts := []*Node{a}
	for p.isOp(",") {
		p.next()
		if !p.startsStarExpression() {
			break
		}
		elts = append(elts, p.starExpression())
	}
	return p.mkFrom(t, "Tuple", elts, ctxLoad)
}

func (p *parser) startsStarExpression() bool {
	return p.startsExpression() || p.isOp("*", "(", "[", "-", "+", "~")
}

// Raises Invalid star expression when a star has no operand
func (p *parser) starOperand() {
	if !p.startsExpression() && !p.isOp("(", "[", "-", "+", "~", "*") {
		p.raiseLast("Invalid star expression")
	}
}

func (p *parser) starExpression() *Node {
	if p.isOp("*") {
		t := p.next()
		v := p.bitOr()
		return p.mkFrom(t, "Starred", v, ctxLoad)
	}
	return p.expression()
}

// Element of a list or set display with the invalid_starred_expression_unpacking_sequence diagnostics
func (p *parser) starNamedExpressionSeq() *Node {
	if p.isOp("*") {
		t := p.next()
		p.starOperand()
		v := p.bitOr()
		if p.isOp("=") {
			p.next()
			b := p.expression()
			p.raiseAt(t.l, t.c, b.EndLineno, b.EndColOffset, "cannot assign to iterable argument unpacking")
		}
		return p.mkFrom(t, "Starred", v, ctxLoad)
	}
	return p.namedExpression()
}

func (p *parser) starNamedExpression() *Node {
	if p.isOp("*") {
		t := p.next()
		v := p.bitOr()
		return p.mkFrom(t, "Starred", v, ctxLoad)
	}
	return p.namedExpression()
}

// Comma separated star_named_expressions closed by the given bracket, with the comma diagnostic
func (p *parser) starNamedExpressions(close string) []*Node {
	var elts []*Node
	for !p.isOp(close) {
		elts = append(elts, p.starNamedExpression())
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	return elts
}

func (p *parser) disjunction() *Node {
	t := p.cur()
	a := p.conjunction()
	if !p.isKw("or") {
		return a
	}
	vals := []*Node{a}
	for p.isKw("or") {
		p.next()
		vals = append(vals, p.conjunction())
	}
	return p.mkFrom(t, "BoolOp", opNode("Or"), vals)
}

func (p *parser) conjunction() *Node {
	t := p.cur()
	a := p.inversion()
	if !p.isKw("and") {
		return a
	}
	vals := []*Node{a}
	for p.isKw("and") {
		p.next()
		vals = append(vals, p.inversion())
	}
	return p.mkFrom(t, "BoolOp", opNode("And"), vals)
}

func (p *parser) inversion() *Node {
	if p.isKw("not") {
		t := p.next()
		p.enter(1)
		v := p.inversion()
		p.leave(1)
		return p.mkFrom(t, "UnaryOp", opNode("Not"), v)
	}
	return p.comparison()
}

func (p *parser) compareOp() string {
	t := p.cur()
	if t.kind == tOp {
		switch t.val {
		case "==":
			return "Eq"
		case "!=", "<>":
			if t.val == "!=" && p.barry {
				p.raiseLast("with Barry as BDFL, use '<>' instead of '!='")
			}
			if t.val == "<>" && !p.barry {
				return ""
			}
			return "NotEq"
		case "<":
			if n := p.tk(p.pos + 1); n.val == ">" && n.kind == tOp && n.l == t.el && n.c == t.ec {
				p.raiseAt(t.l, t.c, n.el, n.ec, "invalid syntax.  Maybe you meant '!=' instead of '<>'?")
			}
			return "Lt"
		case "<=":
			return "LtE"
		case ">":
			return "Gt"
		case ">=":
			return "GtE"
		}
		return ""
	}
	if t.kind == tName {
		switch t.val {
		case "in":
			return "In"
		case "is":
			n := p.tk(p.pos + 1)
			if n.kind == tName && n.val == "not" {
				return "IsNot"
			}
			return "Is"
		case "not":
			n := p.tk(p.pos + 1)
			if n.kind == tName && n.val == "in" {
				return "NotIn"
			}
		}
	}
	return ""
}

func (p *parser) comparison() *Node {
	t := p.cur()
	a := p.bitOr()
	op := p.compareOp()
	if op == "" {
		return a
	}
	var ops, comps []*Node
	for op != "" {
		p.next()
		if op == "IsNot" || op == "NotIn" {
			p.next()
		}
		ops = append(ops, opNode(op))
		comps = append(comps, p.bitOr())
		op = p.compareOp()
	}
	return p.mkFrom(t, "Compare", a, ops, comps)
}

func (p *parser) binLevel(ops []string, sub func() *Node) *Node {
	t := p.cur()
	a := sub()
	for p.cur().kind == tOp && contains(ops, p.cur().val) {
		op := p.next().val
		b := sub()
		a = p.mkFrom(t, "BinOp", a, opNode(binOps[op]), b)
		if op == "@" {
			p.checkVersion(5, "The '@' operator is")
		}
	}
	return a
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (p *parser) bitOr() *Node  { return p.binLevel([]string{"|"}, p.bitXor) }
func (p *parser) bitXor() *Node { return p.binLevel([]string{"^"}, p.bitAnd) }
func (p *parser) bitAnd() *Node { return p.binLevel([]string{"&"}, p.shiftExpr) }
func (p *parser) shiftExpr() *Node {
	return p.binLevel([]string{"<<", ">>"}, p.sum)
}
func (p *parser) sum() *Node { return p.binLevel([]string{"+", "-"}, p.term) }
func (p *parser) term() *Node {
	return p.binLevel([]string{"*", "/", "//", "%", "@"}, p.factor)
}

func (p *parser) factor() *Node {
	if p.cur().kind == tOp {
		var op string
		switch p.cur().val {
		case "+":
			op = "UAdd"
		case "-":
			op = "USub"
		case "~":
			op = "Invert"
		}
		if op != "" {
			t := p.next()
			p.enter(1)
			v := p.factor()
			p.leave(1)
			return p.mkFrom(t, "UnaryOp", opNode(op), v)
		}
	}
	return p.power()
}

func (p *parser) power() *Node {
	t := p.cur()
	a := p.awaitPrimary()
	if p.isOp("**") {
		p.next()
		p.enter(2)
		b := p.factor()
		p.leave(2)
		return p.mkFrom(t, "BinOp", a, opNode("Pow"), b)
	}
	return a
}

func (p *parser) awaitPrimary() *Node {
	if p.isKw("await") {
		t := p.next()
		p.enter(2)
		v := p.primary()
		p.leave(2)
		n := p.mkFrom(t, "Await", v)
		p.checkVersion(5, "Await expressions are")
		return n
	}
	return p.primary()
}

func (p *parser) primary() *Node {
	t := p.cur()
	a := p.atom()
	for {
		switch {
		case p.isOp("."):
			p.next()
			name := p.expectName()
			a = p.mkFrom(t, "Attribute", a, name, ctxLoad)
		case p.isOp("("):
			a = p.call(t, a)
		case p.isOp("["):
			p.next()
			s := p.slices()
			p.expectOp("]")
			a = p.mkFrom(t, "Subscript", a, s, ctxLoad)
		default:
			return a
		}
	}
}

func (p *parser) slices() *Node {
	t := p.cur()
	a := p.sliceItem()
	if !p.isOp(",") {
		if a.Is("Starred") {
			return p.mkFrom(t, "Tuple", []*Node{a}, ctxLoad)
		}
		return a
	}
	elts := []*Node{a}
	for p.isOp(",") {
		p.next()
		if p.isOp("]") {
			break
		}
		elts = append(elts, p.sliceItem())
	}
	return p.mkFrom(t, "Tuple", elts, ctxLoad)
}

func (p *parser) sliceItem() *Node {
	t := p.cur()
	if p.isOp("*") {
		st := p.next()
		v := p.expression()
		return p.mkFrom(st, "Starred", v, ctxLoad)
	}
	var lower, upper, step *Node
	if !p.isOp(":") {
		e := p.namedExpression()
		if !p.isOp(":") {
			return e
		}
		if e.Is("NamedExpr") {
			p.fail()
		}
		lower = e
	}
	p.next()
	if !p.isOp(":", ",", "]") {
		upper = p.expression()
	}
	if p.isOp(":") {
		p.next()
		if !p.isOp(",", "]") {
			step = p.expression()
		}
	}
	return p.mkFrom(t, "Slice", lower, upper, step)
}

func (p *parser) nameNode(ctx *Node) *Node {
	t := p.next()
	if t.kind != tName || keywords[t.val] {
		p.pos--
		p.fail()
	}
	return p.mkTok(t, "Name", p.identifier(t.val), ctx)
}

func (p *parser) expectName() string {
	t := p.cur()
	if t.kind != tName || keywords[t.val] {
		p.fail()
	}
	p.next()
	return p.identifier(t.val)
}

func (p *parser) atom() *Node {
	t := p.cur()
	switch t.kind {
	case tName:
		switch t.val {
		case "True":
			p.next()
			return p.mkTok(t, "Constant", true, nil)
		case "False":
			p.next()
			return p.mkTok(t, "Constant", false, nil)
		case "None":
			p.next()
			return p.mkTok(t, "Constant", nil, nil)
		}
		if keywords[t.val] {
			p.fail()
		}
		p.next()
		return p.mkTok(t, "Name", p.identifier(t.val), ctxLoad)
	case tNumber:
		p.next()
		return p.mkTok(t, "Constant", p.numberValue(t), nil)
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
			return p.mkTok(t, "Constant", Ellipsis{}, nil)
		}
	}
	p.fail()
	return nil
}

func (p *parser) isComprehensionStart() bool {
	return p.isKw("for") || (p.isKw("async") && p.tk(p.pos+1).kind == tName && p.tk(p.pos+1).val == "for")
}

func (p *parser) forIfClauses() []*Node {
	var out []*Node
	for p.isComprehensionStart() {
		isAsync := 0
		if p.isKw("async") {
			p.next()
			isAsync = 1
		}
		p.next()
		target := p.starTargets(forTargets)
		if !p.isKw("in") {
			p.fail()
		}
		p.next()
		iter := p.disjunction()
		var ifs []*Node
		for p.isKw("if") {
			p.next()
			ifs = append(ifs, p.disjunction())
		}
		out = append(out, NewNode("comprehension", target, iter, ifs, isAsync))
		if isAsync == 1 {
			p.checkVersion(6, "Async comprehensions are")
		}
	}
	return out
}

func lastComprehensionItem(c *Node) *Node {
	if ifs := c.Children("ifs"); len(ifs) > 0 {
		return ifs[len(ifs)-1]
	}
	return c.Child("iter")
}

func (p *parser) parenAtom() *Node {
	open := p.next()
	if p.isOp(")") {
		p.next()
		return p.mkFrom(open, "Tuple", nil, ctxLoad)
	}
	if p.isKw("yield") {
		y := p.yieldExpr()
		p.expectOp(")")
		p.parens[y] = true
		return y
	}
	if p.isOp("**") {
		st := p.next()
		e := p.expression()
		if p.isOp(")") {
			p.raiseAt(st.l, st.c, e.EndLineno, e.EndColOffset, "cannot use double starred expression here")
		}
		p.fail()
	}
	first := p.starNamedExpression()
	if first.Is("Starred") && p.isOp(")") {
		p.raiseNode(first, "cannot use starred expression here")
	}
	if p.isComprehensionStart() {
		if first.Is("Starred") {
			p.raiseNode(first, "iterable unpacking cannot be used in comprehension")
		}
		gens := p.forIfClauses()
		p.expectOp(")")
		return p.mkFrom(open, "GeneratorExp", first, gens)
	}
	if p.isOp(")") {
		p.next()
		p.parens[first] = true
		return first
	}
	if !p.isOp(",") {
		p.fail()
	}
	elts := []*Node{first}
	for p.isOp(",") {
		p.next()
		if p.isOp(")") {
			break
		}
		elts = append(elts, p.starNamedExpression())
		if p.isComprehensionStart() && len(elts) > 0 {
			p.raiseRange(elts[0], elts[len(elts)-1], "did you forget parentheses around the comprehension target?")
		}
	}
	p.expectOp(")")
	return p.mkFrom(open, "Tuple", elts, ctxLoad)
}

func (p *parser) listAtom() *Node {
	open := p.next()
	if p.isOp("]") {
		p.next()
		return p.mkFrom(open, "List", nil, ctxLoad)
	}
	first := p.starNamedExpressionSeq()
	if p.isComprehensionStart() {
		if first.Is("Starred") {
			p.raiseNode(first, "iterable unpacking cannot be used in comprehension")
		}
		gens := p.forIfClauses()
		p.expectOp("]")
		return p.mkFrom(open, "ListComp", first, gens)
	}
	elts := []*Node{first}
	for p.isOp(",") {
		p.next()
		if p.isOp("]") {
			break
		}
		elts = append(elts, p.starNamedExpressionSeq())
		if p.isComprehensionStart() {
			p.raiseRange(elts[0], elts[len(elts)-1], "did you forget parentheses around the comprehension target?")
		}
	}
	p.expectOp("]")
	return p.mkFrom(open, "List", elts, ctxLoad)
}

func (p *parser) braceAtom() *Node {
	open := p.next()
	if p.isOp("}") {
		p.next()
		return p.mkFrom(open, "Dict", nil, nil)
	}
	if p.isOp("**") {
		return p.dictRest(open, nil, nil)
	}
	if p.isOp("*") {
		return p.setRest(open)
	}
	save := p.pos
	first := p.namedExpression()
	if p.isOp(":") && (!first.Is("NamedExpr") || p.parens[first]) {
		p.next()
		if p.isOp("*") {
			st := p.cur()
			p.raiseFrom(st, "cannot use a starred expression in a dictionary value")
		}
		if p.isOp("}", ",") {
			p.raiseTok(&p.toks[p.pos-1], "expression expected after dictionary key and ':'")
		}
		v := p.expression()
		if p.isComprehensionStart() {
			gens := p.forIfClauses()
			p.expectOp("}")
			return p.mkFrom(open, "DictComp", first, v, gens)
		}
		return p.dictRest(open, []*Node{first}, []*Node{v})
	}
	p.pos = save
	return p.setRest(open)
}

func (p *parser) dictRest(open *token, keys, vals []*Node) *Node {
	if len(keys) > 0 {
		if !p.isOp(",") {
			p.expectOp("}")
			return p.mkFrom(open, "Dict", keys, vals)
		}
		p.next()
	}
	for !p.isOp("}") {
		if p.isOp("**") {
			st := p.next()
			v := p.bitOr()
			if p.isComprehensionStart() && len(keys) == 0 {
				p.raiseTok(st, "dict unpacking cannot be used in dict comprehension")
			}
			keys = append(keys, nil)
			vals = append(vals, v)
		} else {
			k := p.expression()
			if !p.isOp(":") {
				if p.isOp(",", "}") || true {
					p.raiseAt(k.Lineno, k.EndColOffset-1, k.EndLineno, -1, "':' expected after dictionary key")
				}
			}
			p.next()
			if p.isOp("*") {
				p.raiseFrom(p.cur(), "cannot use a starred expression in a dictionary value")
			}
			if p.isOp("}", ",") {
				p.raiseTok(&p.toks[p.pos-1], "expression expected after dictionary key and ':'")
			}
			keys = append(keys, k)
			vals = append(vals, p.expression())
		}
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	p.expectOp("}")
	return p.mkFrom(open, "Dict", keys, vals)
}

func (p *parser) setRest(open *token) *Node {
	first := p.starNamedExpressionSeq()
	if p.isComprehensionStart() {
		if first.Is("Starred") {
			p.raiseNode(first, "iterable unpacking cannot be used in comprehension")
		}
		gens := p.forIfClauses()
		p.expectOp("}")
		return p.mkFrom(open, "SetComp", first, gens)
	}
	elts := []*Node{first}
	for p.isOp(",") {
		p.next()
		if p.isOp("}") {
			break
		}
		elts = append(elts, p.starNamedExpressionSeq())
		if p.isComprehensionStart() {
			p.raiseRange(elts[0], elts[len(elts)-1], "did you forget parentheses around the comprehension target?")
		}
	}
	p.expectOp("}")
	return p.mkFrom(open, "Set", elts)
}

func (p *parser) yieldExpr() *Node {
	t := p.next()
	if p.isKw("from") {
		p.next()
		v := p.expression()
		return p.mkFrom(t, "YieldFrom", v)
	}
	if p.startsStarExpression() {
		v := p.starExpressions()
		return p.mkFrom(t, "Yield", v)
	}
	return p.mkFrom(t, "Yield", nil)
}

type callArg struct {
	kind  int
	node  *Node
	start *token
}

const (
	argPositional = iota
	argStarred
	argKeyword
	argDoubleStar
)

// Parses call arguments after the opening parenthesis into a Call, a lone generator argument included
func (p *parser) call(start *token, fn *Node) *Node {
	return p.callArgs(start, fn, true)
}

// Argument list of a call or class header, a bare generator argument allowed only in a call
func (p *parser) callArgs(start *token, fn *Node, genexp bool) *Node {
	open := p.next()
	if p.isOp(")") {
		p.next()
		return p.mkFrom(start, "Call", fn, nil, nil)
	}
	var args []callArg
	sawKeyword, sawDoubleStar := false, false
	for !p.isOp(")") {
		t := p.cur()
		prev := &p.toks[p.pos-1]
		var a callArg
		switch {
		case p.isOp("*"):
			p.next()
			if !p.startsExpression() && !p.isOp("(", "[", "-", "+", "*") {
				p.raiseLast("Invalid star expression")
			}
			v := p.expression()
			if p.isOp("=") {
				p.next()
				b := p.expression()
				p.raiseAt(t.l, t.c, b.EndLineno, b.EndColOffset, "cannot assign to iterable argument unpacking")
			}
			a = callArg{argStarred, p.mkFrom(t, "Starred", v, ctxLoad), t}
			if sawDoubleStar {
				p.raiseAt(prev.l, prev.c, a.node.EndLineno, a.node.EndColOffset, "iterable argument unpacking follows keyword argument unpacking")
			}
		case p.isOp("**"):
			p.next()
			v := p.expression()
			if p.isOp("=") {
				p.next()
				b := p.expression()
				p.raiseAt(t.l, t.c, b.EndLineno, b.EndColOffset, "cannot assign to keyword argument unpacking")
			}
			a = callArg{argDoubleStar, p.mkFrom(t, "keyword", nil, v), t}
			sawDoubleStar = true
		case t.kind == tName && p.tk(p.pos+1).kind == tOp && p.tk(p.pos+1).val == "=":
			if keywords[t.val] {
				if t.val == "True" || t.val == "False" || t.val == "None" {
					eq := p.tk(p.pos + 1)
					p.raiseAt(t.l, t.c, eq.el, eq.ec, "cannot assign to "+t.val)
				}
				p.fail()
			}
			p.next()
			eq := p.next()
			if p.isOp(",", ")") {
				p.raiseAt(t.l, t.c, eq.el, eq.ec, "expected argument value expression")
			}
			v := p.expression()
			if p.isComprehensionStart() {
				p.raiseAt(t.l, t.c, eq.el, eq.ec, "invalid syntax. Maybe you meant '==' or ':=' instead of '='?")
			}
			a = callArg{argKeyword, p.mkFrom(t, "keyword", p.identifier(t.val), v), t}
			sawKeyword = true
		default:
			var v *Node
			if p.isName() && p.tk(p.pos+1).kind == tOp && p.tk(p.pos+1).val == ":=" {
				name := p.nameNode(ctxStore)
				p.next()
				val := p.expression()
				v = p.mkFrom(t, "NamedExpr", name, val)
				p.checkVersion(8, "Assignment expressions are")
			} else {
				v = p.expression()
				if p.isOp(":=") {
					p.raiseNode(v, "cannot use assignment expressions with "+exprName(v))
				}
			}
			if p.isOp("=") && !v.Is("NamedExpr") {
				eq := p.cur()
				p.raiseAt(v.Lineno, v.ColOffset, eq.el, eq.ec, "expression cannot contain assignment, perhaps you meant \"==\"?")
			}
			if p.isComprehensionStart() {
				if !genexp && len(args) == 0 {
					p.fail()
				}
				gens := p.forIfClauses()
				last := lastComprehensionItem(gens[len(gens)-1])
				if len(args) == 0 && p.isOp(")") {
					p.next()
					g := p.mkFrom(open, "GeneratorExp", v, gens)
					return p.mkFrom(start, "Call", fn, []*Node{g}, nil)
				}
				if len(args) == 0 {
					p.raiseRange(v, last, "Generator expression must be parenthesized")
				}
				positional := 0
				for _, x := range args {
					if x.kind == argPositional || x.kind == argStarred {
						positional++
					}
				}
				if positional+1 > 1 {
					p.raiseRange(v, last, "Generator expression must be parenthesized")
				}
				p.raiseRange(v, gens[len(gens)-1].Child("target"), "Generator expression must be parenthesized")
			}
			a = callArg{argPositional, v, t}
			if sawDoubleStar {
				p.argumentsError("positional argument follows keyword argument unpacking")
			}
			if sawKeyword {
				p.argumentsError("positional argument follows keyword argument")
			}
		}
		args = append(args, a)
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	p.expectOp(")")
	var pos, kws []*Node
	for _, a := range args {
		if a.kind == argPositional || a.kind == argStarred {
			pos = append(pos, a.node)
		} else {
			kws = append(kws, a.node)
		}
	}
	return p.mkFrom(start, "Call", fn, pos, kws)
}

// Reports the misplaced positional argument at the furthest token like _PyPegen_arguments_parsing_error
func (p *parser) argumentsError(msg string) {
	for !p.isOp(")") && p.cur().kind != tEnd && p.cur().kind != tNewline {
		if p.isOp(",") {
			p.next()
			continue
		}
		if _, ok := p.speculate(p.expression); !ok {
			break
		}
	}
	p.cur()
	p.raiseLast(msg)
}

func (p *parser) lambdef() *Node {
	t := p.next()
	args := p.parameters(true)
	colon := p.cur()
	p.expectOp(":")
	if nt := p.cur(); nt.kind == tFMiddle || nt.kind == tTMiddle {
		kind := "f"
		if nt.kind == tTMiddle {
			kind = "t"
		}
		p.raiseAt(t.l, t.c, colon.el, colon.ec, kind+"-string: lambda expressions are not allowed without parentheses")
	}
	p.enter(2)
	body := p.expression()
	p.leave(2)
	return p.mkFrom(t, "Lambda", args, body)
}

const (
	starTargets = iota
	forTargets
	delTargets
)

// Parses star_targets as an expression and converts it to a store target
func (p *parser) starTargets(kind int) *Node {
	t := p.cur()
	a := p.starTarget()
	if !p.isOp(",") {
		return p.toTarget(a, kind, ctxStore)
	}
	elts := []*Node{a}
	for p.isOp(",") {
		p.next()
		if !p.startsStarExpression() {
			break
		}
		elts = append(elts, p.starTarget())
	}
	return p.toTarget(p.mkFrom(t, "Tuple", elts, ctxLoad), kind, ctxStore)
}

func (p *parser) starTarget() *Node {
	if p.isOp("*") {
		t := p.next()
		if p.isOp("*") {
			p.fail()
		}
		v := p.starTarget()
		return p.mkFrom(t, "Starred", v, ctxLoad)
	}
	return p.bitOr()
}

func exprName(e *Node) string {
	switch e.Type {
	case "Attribute":
		return "attribute"
	case "Subscript":
		return "subscript"
	case "Starred":
		return "starred"
	case "Name":
		return "name"
	case "List":
		return "list"
	case "Tuple":
		return "tuple"
	case "Lambda":
		return "lambda"
	case "Call":
		return "function call"
	case "BoolOp", "BinOp", "UnaryOp":
		return "expression"
	case "GeneratorExp":
		return "generator expression"
	case "Yield", "YieldFrom":
		return "yield expression"
	case "Await":
		return "await expression"
	case "ListComp":
		return "list comprehension"
	case "SetComp":
		return "set comprehension"
	case "DictComp":
		return "dict comprehension"
	case "Dict":
		return "dict literal"
	case "Set":
		return "set display"
	case "JoinedStr", "FormattedValue":
		return "f-string expression"
	case "TemplateStr", "Interpolation":
		return "t-string expression"
	case "Constant":
		switch v := e.Get("value").(type) {
		case nil:
			return "None"
		case bool:
			if v {
				return "True"
			}
			return "False"
		case Ellipsis:
			return "ellipsis"
		}
		return "literal"
	case "Compare":
		return "comparison"
	case "IfExp":
		return "conditional expression"
	case "NamedExpr":
		return "named expression"
	}
	return e.Type
}

// First sub-expression that cannot be a target, as _PyPegen_get_invalid_target
func invalidTarget(e *Node, kind int) *Node {
	switch e.Type {
	case "List", "Tuple":
		for _, c := range e.Children("elts") {
			if bad := invalidTarget(c, kind); bad != nil {
				return bad
			}
		}
		return nil
	case "Starred":
		if kind == delTargets {
			return e
		}
		return invalidTarget(e.Child("value"), kind)
	case "Compare":
		if kind == forTargets {
			if ops := e.Children("ops"); len(ops) > 0 && ops[0].Type == "In" {
				return invalidTarget(e.Child("left"), kind)
			}
			return nil
		}
		return e
	case "Name", "Subscript", "Attribute":
		return nil
	}
	return e
}

func (p *parser) raiseInvalidTarget(e *Node, kind int) {
	bad := invalidTarget(e, kind)
	if bad == nil {
		p.raiseLast("invalid syntax")
	}
	if kind == delTargets {
		p.raiseNode(bad, "cannot delete "+exprName(bad))
	}
	p.raiseNode(bad, "cannot assign to "+exprName(bad))
}

// Sets the store or delete context through a target, raising the CPython target error for an invalid one
func (p *parser) toTarget(e *Node, kind int, ctx *Node) *Node {
	if bad := invalidTarget(e, kind); bad != nil || (kind == delTargets && e.Is("Starred")) {
		p.raiseInvalidTarget(e, kind)
	}
	setContext(e, ctx)
	return e
}

func setContext(e *Node, ctx *Node) {
	switch e.Type {
	case "Name", "Attribute", "Subscript":
		e.Set("ctx", ctx)
	case "Starred":
		e.Set("ctx", ctx)
		setContext(e.Child("value"), ctx)
	case "List", "Tuple":
		e.Set("ctx", ctx)
		for _, c := range e.Children("elts") {
			setContext(c, ctx)
		}
	}
}
