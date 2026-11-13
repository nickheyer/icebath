package python

import "fmt"

// Match statement, or nil when the soft keyword starts an ordinary statement
func (p *parser) matchStmt() Stmt {
	t := p.cur()
	save := p.pos
	p.next()
	subject, ok := speculate(p, p.subjectExpr)
	if p.bad() {
		return nil
	}
	if !ok || !p.isOp(":") {
		newline := ok && p.cur().kind == tNewline
		p.pos = save
		if !newline {
			return nil
		}
		_, sok := speculate(p, p.simpleStmts)
		if p.bad() {
			return nil
		}
		if !sok {
			p.pos = save
			p.next()
			p.subjectExpr()
			p.next()
			p.raiseLast("expected ':'")
			return nil
		}
		p.pos = save
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
		return nil
	}
	p.next()
	var cases []*MatchCase
	for p.isKw("case") {
		c := p.caseBlock()
		if p.bad() {
			return nil
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 || p.cur().kind != tDedent {
		p.fail()
		return nil
	}
	p.next()
	return &Match{Pos: p.span(t), Subject: subject, Cases: cases}
}

func (p *parser) subjectExpr() Expr {
	t := p.cur()
	first := p.starNamedExpression()
	if p.bad() {
		return nil
	}
	if !p.isOp(",") {
		if _, ok := first.(*Starred); ok {
			p.fail()
			return nil
		}
		return first
	}
	elts := []Expr{first}
	for p.isOp(",") {
		p.next()
		if p.isOp(":") {
			break
		}
		e := p.starNamedExpression()
		if p.bad() {
			return nil
		}
		elts = append(elts, e)
	}
	return &Tuple{Pos: p.span(t), Elts: elts, Ctx: Load}
}

func (p *parser) caseBlock() *MatchCase {
	t := p.next()
	pat := p.patterns()
	if p.bad() {
		return nil
	}
	var guard Expr
	if p.isKw("if") {
		p.next()
		if guard = p.namedExpression(); p.bad() {
			return nil
		}
	}
	if p.colon(); p.bad() {
		return nil
	}
	body := p.block("'case' statement", t.l)
	if p.bad() {
		return nil
	}
	return &MatchCase{Pattern: pat, Guard: guard, Body: body}
}

// Pattern of a case clause, a sequence pattern when commas separate several
func (p *parser) patterns() Pattern {
	t := p.cur()
	first := p.maybeStarPattern()
	if p.bad() {
		return nil
	}
	if !p.isOp(",") {
		if _, ok := first.(*MatchStar); ok {
			p.fail()
			return nil
		}
		return first
	}
	pats := []Pattern{first}
	for p.isOp(",") {
		p.next()
		if p.isOp(":") || p.isKw("if") {
			break
		}
		pt := p.maybeStarPattern()
		if p.bad() {
			return nil
		}
		pats = append(pats, pt)
	}
	return &MatchSequence{Pos: p.span(t), Patterns: pats}
}

func (p *parser) maybeStarPattern() Pattern {
	if p.isOp("*") {
		t := p.next()
		name := p.expectName()
		if p.bad() {
			return nil
		}
		if name == "_" {
			name = ""
		}
		return &MatchStar{Pos: p.span(t), Name: name}
	}
	return p.pattern()
}

// Alternatives with an optional as capture and the invalid capture target diagnostics
func (p *parser) pattern() Pattern {
	t := p.cur()
	or := p.orPattern()
	if p.bad() {
		return nil
	}
	if !p.isKw("as") {
		return or
	}
	p.next()
	nt := p.cur()
	if nt.kind == tName && nt.val == "_" {
		p.raiseTok(nt, "cannot use '_' as a target")
		return nil
	}
	if !p.isName() {
		if e, ok := speculate(p, p.expression); ok {
			p.raiseNode(e, "invalid pattern target")
			return nil
		}
		p.fail()
		return nil
	}
	name := p.expectName()
	if p.isOp(".", "(", "=") {
		p.fail()
		return nil
	}
	return &MatchAs{Pos: p.span(t), Pattern: or, Name: name}
}

func (p *parser) orPattern() Pattern {
	t := p.cur()
	first := p.closedPattern()
	if p.bad() {
		return nil
	}
	if !p.isOp("|") {
		return first
	}
	pats := []Pattern{first}
	for p.isOp("|") {
		p.next()
		pt := p.closedPattern()
		if p.bad() {
			return nil
		}
		pats = append(pats, pt)
	}
	return &MatchOr{Pos: p.span(t), Patterns: pats}
}

// Number constant of a pattern, optionally negated
func (p *parser) signedNumber() Expr {
	t := p.cur()
	if p.isOp("-") {
		p.next()
		nt := p.cur()
		if nt.kind != tNumber {
			p.fail()
			return nil
		}
		p.next()
		v := p.numberValue(nt)
		if p.bad() {
			return nil
		}
		num := &Constant{Pos: tokSpan(nt), Value: v}
		return &UnaryOp{Pos: p.span(t), Op: USub, Operand: num}
	}
	if t.kind != tNumber {
		p.fail()
		return nil
	}
	p.next()
	v := p.numberValue(t)
	if p.bad() {
		return nil
	}
	return &Constant{Pos: tokSpan(t), Value: v}
}

// Literal of a value pattern or mapping key, the complex literal checks included
func (p *parser) literalExpr() Expr {
	t := p.cur()
	if t.kind == tNumber || p.isOp("-") {
		left := p.signedNumber()
		if p.bad() {
			return nil
		}
		if !p.isOp("+", "-") {
			return left
		}
		opTok := p.next()
		real := left
		if u, ok := real.(*UnaryOp); ok {
			real = u.Operand
		}
		if c, ok := real.(*Constant); ok {
			if _, isComplex := c.Value.(Complex); isComplex {
				p.raiseNode(real, "real number required in complex literal")
				return nil
			}
		}
		nt := p.cur()
		if nt.kind != tNumber {
			p.fail()
			return nil
		}
		p.next()
		v := p.numberValue(nt)
		if p.bad() {
			return nil
		}
		im := &Constant{Pos: tokSpan(nt), Value: v}
		if _, isComplex := v.(Complex); !isComplex {
			p.raiseNode(im, "imaginary number required in complex literal")
			return nil
		}
		op := Add
		if opTok.val == "-" {
			op = Sub
		}
		return &BinOp{Pos: p.span(t), Left: left, Op: op, Right: im}
	}
	if t.kind == tString || t.kind == tFStart || t.kind == tTStart {
		return p.strings()
	}
	if t.kind == tName {
		switch t.val {
		case "None":
			p.next()
			return &Constant{Pos: tokSpan(t), Value: None{}}
		case "True":
			p.next()
			return &Constant{Pos: tokSpan(t), Value: Bool(true)}
		case "False":
			p.next()
			return &Constant{Pos: tokSpan(t), Value: Bool(false)}
		}
	}
	p.fail()
	return nil
}

// Name or dotted attribute chain of a value or class pattern
func (p *parser) nameOrAttr() Expr {
	t := p.cur()
	n := p.nameNode(Load)
	if p.bad() {
		return nil
	}
	var e Expr = n
	for p.isOp(".") {
		p.next()
		attr := p.expectName()
		if p.bad() {
			return nil
		}
		e = &Attribute{Pos: p.span(t), Value: e, Attr: attr, Ctx: Load}
	}
	return e
}

func (p *parser) closedPattern() Pattern {
	t := p.cur()
	switch {
	case t.kind == tNumber || p.isOp("-") || t.kind == tString || t.kind == tFStart || t.kind == tTStart:
		v := p.literalExpr()
		if p.bad() {
			return nil
		}
		return &MatchValue{Pos: p.span(t), Value: v}
	case t.kind == tName && (t.val == "None" || t.val == "True" || t.val == "False"):
		p.next()
		var v Value = None{}
		if t.val != "None" {
			v = Bool(t.val == "True")
		}
		return &MatchSingleton{Pos: tokSpan(t), Value: v}
	case p.isName():
		n := p.nameOrAttr()
		if p.bad() {
			return nil
		}
		if p.isOp("(") {
			return p.classPattern(t, n)
		}
		if p.isOp("=") {
			p.fail()
			return nil
		}
		name, ok := n.(*Name)
		if !ok {
			return &MatchValue{Pos: p.span(t), Value: n}
		}
		if name.ID == "_" {
			return &MatchAs{Pos: tokSpan(t)}
		}
		return &MatchAs{Pos: tokSpan(t), Name: name.ID}
	case p.isOp("("):
		return p.groupPattern(t)
	case p.isOp("["):
		p.next()
		var pats []Pattern
		for !p.isOp("]") {
			pt := p.maybeStarPattern()
			if p.bad() {
				return nil
			}
			pats = append(pats, pt)
			if !p.isOp(",") {
				break
			}
			p.next()
		}
		if p.expectOp("]"); p.bad() {
			return nil
		}
		return &MatchSequence{Pos: p.span(t), Patterns: pats}
	case p.isOp("{"):
		return p.mappingPattern()
	}
	p.fail()
	return nil
}

// Parenthesized pattern or sequence pattern in parentheses
func (p *parser) groupPattern(t *token) Pattern {
	p.next()
	if p.isOp(")") {
		p.next()
		return &MatchSequence{Pos: p.span(t)}
	}
	first := p.maybeStarPattern()
	if p.bad() {
		return nil
	}
	if _, star := first.(*MatchStar); p.isOp(")") && !star {
		p.next()
		return first
	}
	pats := []Pattern{first}
	if !p.isOp(",") {
		p.fail()
		return nil
	}
	for p.isOp(",") {
		p.next()
		if p.isOp(")") {
			break
		}
		pt := p.maybeStarPattern()
		if p.bad() {
			return nil
		}
		pats = append(pats, pt)
	}
	if p.expectOp(")"); p.bad() {
		return nil
	}
	return &MatchSequence{Pos: p.span(t), Patterns: pats}
}

func (p *parser) mappingPattern() Pattern {
	t := p.next()
	var keys []Expr
	var pats []Pattern
	rest := ""
	for !p.isOp("}") {
		if p.isOp("**") {
			p.next()
			if rest = p.expectName(); p.bad() {
				return nil
			}
			if p.isOp(",") {
				p.next()
			}
			if !p.isOp("}") {
				p.fail()
				return nil
			}
			break
		}
		var k Expr
		if p.isName() {
			k = p.nameOrAttr()
			if p.bad() {
				return nil
			}
			if _, ok := k.(*Attribute); !ok {
				p.fail()
				return nil
			}
		} else if k = p.literalExpr(); p.bad() {
			return nil
		}
		if p.expectOp(":"); p.bad() {
			return nil
		}
		pt := p.pattern()
		if p.bad() {
			return nil
		}
		keys = append(keys, k)
		pats = append(pats, pt)
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if p.expectOp("}"); p.bad() {
		return nil
	}
	return &MatchMapping{Pos: p.span(t), Keys: keys, Patterns: pats, Rest: rest}
}

// Reports whether a keyword pattern name followed by an equals sign starts here
func (p *parser) keywordPatternStart() bool {
	return p.isName() && p.peekOp(1, "=")
}

func (p *parser) classPattern(t *token, cls Expr) Pattern {
	p.next()
	var pats, kwPats []Pattern
	var kwNames []string
	for !p.isOp(")") {
		if p.keywordPatternStart() {
			name := p.expectName()
			p.next()
			pt := p.pattern()
			if p.bad() {
				return nil
			}
			kwNames = append(kwNames, name)
			kwPats = append(kwPats, pt)
		} else {
			pat := p.pattern()
			if p.bad() {
				return nil
			}
			if len(kwPats) > 0 {
				last := pat
				for p.isOp(",") && !p.peekOp(1, ")") {
					save := p.pos
					p.next()
					if p.keywordPatternStart() {
						p.pos = save
						break
					}
					if last = p.pattern(); p.bad() {
						return nil
					}
				}
				s, e := pat.Position(), last.Position()
				p.raiseAt(s.Line, s.Col, e.EndLine, e.EndCol, "positional patterns follow keyword patterns")
				return nil
			}
			pats = append(pats, pat)
		}
		if !p.isOp(",") {
			break
		}
		p.next()
	}
	if p.expectOp(")"); p.bad() {
		return nil
	}
	return &MatchClass{Pos: p.span(t), Cls: cls, Patterns: pats, KwdAttrs: kwNames, KwdPatterns: kwPats}
}
