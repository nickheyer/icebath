package python

func (r *rec) invalidStringTstringConcatRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.loopSome81Rule() && r.tstringRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.loopSome82Rule() && r.tmp154Rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidArithmeticRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.sumRule() && r.tmp155Rule() && r.tok(703) && r.inversionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidFactorRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp156Rule() && r.tok(703) && r.factorRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidTypeParamsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(9) && r.tok(10) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidNoteqRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([3]int); r.at(&s[0]) && r.tok(20) && r.at(&s[1]) && r.tok(21) && r.at(&s[2]) {
			res = r.hAdjacent(s[0], s[1])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny1Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(cNEWLINE) {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopSome2Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.statementRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopAny3Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(13) && r.simpleStmtRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather4Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.simpleStmtRule() && r.loopAny3Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp5Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(634) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(633) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp6Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(699) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(49) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(698) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp7Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(701) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(49) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp8Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(647) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(698) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp9Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(694) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(698) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp10Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) && r.annotatedRhsRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp11Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r.singleTargetRule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.singleSubscriptAttributeTargetRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopSome12Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp157Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) tmp13Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(633) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny14Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.tok(cNAME) {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather15Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.loopAny14Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp16Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(13) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(cNEWLINE) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp17Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny18Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp158Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopSome19Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp158Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopAny20Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.importFromAsNameRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather21Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.importFromAsNameRule() && r.loopAny20Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp22Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(680) && r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny23Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.dottedAsNameRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather24Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.dottedAsNameRule() && r.loopAny23Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopSome25Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp159Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) tmp26Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && (r.argumentsRule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp27Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(51) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny28Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.paramNoDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopAny29Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.paramWithDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopSome30Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.paramNoDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopSome31Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.paramWithDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopAny32Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.paramMaybeDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopSome33Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.paramMaybeDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopAny34Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.withItemRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather35Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.withItemRule() && r.loopAny34Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp36Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopSome37Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.exceptBlockRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopSome38Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.exceptStarBlockRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopSome39Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.caseBlockRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopAny40Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(18) && r.closedPatternRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather41Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.closedPatternRule() && r.loopAny40Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp42Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(14) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(15) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp43Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cSTRING) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(cFSTART) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(cTSTART) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp44Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(23) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(22) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny45Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.maybeStarPatternRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather46Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.maybeStarPatternRule() && r.loopAny45Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny47Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.keyValuePatternRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather48Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.keyValuePatternRule() && r.loopAny47Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp49Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.literalExprRule() {
			res = true
			break
		}
		r.pos = mark
		if r.attrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny50Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.patternRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather51Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.patternRule() && r.loopAny50Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny52Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.keywordPatternRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather53Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.keywordPatternRule() && r.loopAny52Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny54Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.typeParamRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather55Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.typeParamRule() && r.loopAny54Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopSome56Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp17Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopSome57Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp160Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopAny58Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.starNamedExpressionRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather59Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.starNamedExpressionRule() && r.loopAny58Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopSome60Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp161Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopSome61Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp162Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}
