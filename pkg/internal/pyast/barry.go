package pyast

// Reports whether a barry_as_FLUFL future import can change how CPython lexes or parses the tokens
func (p *parser) barryRelevant() bool {
	return p.lx.barryAt != -1 || p.lx.barryTok >= 0 || p.barryTok >= 0
}

// Runs CPython's passes after a failed parse, recording the pass one barry fill and returning the != error pass two raises
func (p *parser) barryErrorPass() (ne error) {
	pos, fill, seen, main, level, barry := p.pos, p.fill, p.seen, p.main, p.level, p.barry
	warns, warned, known := p.warns, p.warnedEscape, p.knownErrLine
	p.warnedEscape = map[*token]bool{}
	p.probing++
	defer func() {
		p.pos, p.fill, p.seen, p.main, p.level, p.barry = pos, fill, seen, main, level, barry
		p.warns, p.warnedEscape, p.knownErrLine = warns, warned, known
		p.probing--
	}()
	r, overflow := p.recPasses(cpyTokenTypes(p.toks))
	p.recBarryTok, p.barryChecked = r.barryTok, true
	if overflow || !r.barryStop {
		return nil
	}
	p.fill, p.knownErrLine = r.filled, 0
	e := p.genericErrorOf(func() {
		p.raiseOnToken("SyntaxError", &p.toks[r.filled-1], "with Barry as BDFL, use '<>' instead of '!='")
	})
	return p.fullTokenize(e)
}

// Reports whether lexing with <> enabled from token a instead of b changes a token, -1 meaning never
func barryLexDiffers(toks []token, a, b int) bool {
	if a == b {
		return false
	}
	if a < 0 || b >= 0 && b < a {
		a, b = b, a
	}
	if b < 0 {
		b = len(toks)
	}
	for i := max(a, 0); i < b && i < len(toks); i++ {
		t := &toks[i]
		if t.kind != tOp {
			continue
		}
		if t.val == "<>" {
			return true
		}
		if t.val == "<" && i+1 < len(toks) {
			if n := &toks[i+1]; n.kind == tOp && n.val == ">" && n.l == t.el && n.c == t.ec {
				return true
			}
		}
	}
	return false
}
