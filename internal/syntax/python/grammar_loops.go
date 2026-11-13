package python

func (r *rec) loopSome62Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.compareOpBitwiseOrPairRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) tmp63Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([2]int); r.at(&s[0]) && r.tok(28) && r.at(&s[1]) {
			res = r.hBarry(s[0])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny64Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.tmp163Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather65Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp163Rule() && r.loopAny64Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp66Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) && (r.expressionRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp67Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tupleRule() {
			res = true
			break
		}
		r.pos = mark
		if r.groupRule() {
			res = true
			break
		}
		r.pos = mark
		if r.genexpRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp68Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.listRule() {
			res = true
			break
		}
		r.pos = mark
		if r.listcompRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp69Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.dictRule() {
			res = true
			break
		}
		r.pos = mark
		if r.setRule() {
			res = true
			break
		}
		r.pos = mark
		if r.dictcompRule() {
			res = true
			break
		}
		r.pos = mark
		if r.setcompRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp70Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.yieldExprRule() {
			res = true
			break
		}
		r.pos = mark
		if r.namedExpressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny71Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.lambdaParamNoDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopAny72Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.lambdaParamWithDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopSome73Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.lambdaParamNoDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopSome74Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.lambdaParamWithDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopAny75Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.lambdaParamMaybeDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopSome76Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.lambdaParamMaybeDefaultRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopAny77Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.fstringFormatSpecRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopAny78Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.fstringMiddleRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopAny79Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tstringFormatSpecRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopAny80Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tstringMiddleRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopSome81Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp154Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopSome82Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tstringRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) tmp83Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.starNamedExpressionRule() && r.tok(12) && (r.starNamedExpressionsRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny84Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.doubleStarredKvpairRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather85Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.doubleStarredKvpairRule() && r.loopAny84Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopSome86Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.forIfClauseRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) loopAny87Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp164Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) tmp88Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.assignmentExpressionRule() {
			res = true
			break
		}
		r.pos = mark
		if r.expressionRule() && r.lookTok(false, 53) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny89Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.tmp165Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather90Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp165Rule() && r.loopAny89Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp91Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) && r.kwargsRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny92Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.kwargOrStarredRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather93Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.kwargOrStarredRule() && r.loopAny92Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny94Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.kwargOrDoubleStarredRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather95Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.kwargOrDoubleStarredRule() && r.loopAny94Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny96Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp166Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopAny97Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.starTargetRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather98Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.starTargetRule() && r.loopAny97Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopSome99Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp166Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) tmp100Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lookTok(false, 16) && r.starTargetRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny101Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.delTargetRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather102Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.delTargetRule() && r.loopAny101Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp105Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNEWLINE) && r.tok(cINDENT) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp106Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp167Rule() {
			res = true
			break
		}
		r.pos = mark
		if r.kwargsRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny107Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.tmp168Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather108Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp168Rule() && r.loopAny107Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp109Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.argsRule() {
			res = true
			break
		}
		r.pos = mark
		if r.expressionRule() && r.forIfClausesRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp110Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.argsRule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp111Rule() bool {
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
	}
	r.level--
	return res
}

func (r *rec) tmp112Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(622) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(624) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(623) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp113Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tok(22) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopSome114Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp169Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) tmp115Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tok(cSTRING) {
			res = true
			break
		}
		r.pos = mark
		if r.anySoft() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp116Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(686) {
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

func (r *rec) tmp117Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.passStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.breakStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.continueStmtRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp118Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(53) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp119Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.listRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tupleRule() {
			res = true
			break
		}
		r.pos = mark
		if r.genexpRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(622) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(623) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(624) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny120Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.starNamedExpressionsRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) loopAny121Rule() bool {
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
	return true
}

func (r *rec) tmp122Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(9) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp123Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(9) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp124Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.slashNoDefaultRule() {
			res = true
			break
		}
		r.pos = mark
		if r.slashWithDefaultRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp125Rule() bool {
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
		if r.paramNoDefaultRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}
