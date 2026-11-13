package python

func (r *rec) kvpairRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expressionRule() && r.tok(11) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) forIfClausesRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.loopSome86Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) forIfClauseRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	cut := false
	res := false
	for once := true; once; once = false {
		if r.tok(698) && r.tok(694) && r.starTargetsRule() && r.tok(695) && r.cut(&cut) && r.disjunctionRule() && r.loopAny87Rule() {
			res = true
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
		}
		if r.tok(694) && r.starTargetsRule() && r.tok(695) && r.cut(&cut) && r.disjunctionRule() && r.loopAny87Rule() {
			res = true
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
		}
		if r.invalid {
			if r.invalidForIfClauseRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.invalid {
			if r.invalidForTargetRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) listcompRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(9) && r.namedExpressionRule() && r.forIfClausesRule() && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidComprehensionRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) setcompRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.namedExpressionRule() && r.forIfClausesRule() && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidComprehensionRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) genexpRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r.tmp88Rule() && r.forIfClausesRule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidComprehensionRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) dictcompRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.kvpairRule() && r.forIfClausesRule() && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidDictComprehensionRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) argumentsRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(23); hit {
		r.level--
		return ok
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.argsRule() && (r.tok(12) || true) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidArgumentsRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.memoPut(mark, 23, res, r.pos)
	r.level--
	return res
}

func (r *rec) argsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.gather90Rule() && (r.tmp91Rule() || true) {
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

func (r *rec) kwargsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather93Rule() && r.tok(12) && r.gather95Rule() {
			res = true
			break
		}
		r.pos = mark
		if r.gather93Rule() {
			res = true
			break
		}
		r.pos = mark
		if r.gather95Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starredExpressionRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidStarredExpressionUnpackingRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(16) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidStarredExpressionRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) kwargOrStarredRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidKwargRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(cNAME) && r.tok(22) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
		if r.starredExpressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) kwargOrDoubleStarredRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidKwargRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(cNAME) && r.tok(22) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(35) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starTargetsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.starTargetRule() && r.lookTok(false, 12) {
			res = true
			break
		}
		r.pos = mark
		if r.starTargetRule() && r.loopAny96Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starTargetsListSeqRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather98Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starTargetsTupleSeqRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.starTargetRule() && r.loopSome99Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.starTargetRule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starTargetRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(24); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.tmp100Rule() {
			res = true
			break
		}
		r.pos = mark
		if r.targetWithStarAtomRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 24, res, r.pos)
	r.level--
	return res
}

func (r *rec) targetWithStarAtomRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(25); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tPrimaryRule() && r.tok(23) && r.tok(cNAME) && r.look(false, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
		if r.tPrimaryRule() && r.tok(9) && r.slicesRule() && r.tok(10) && r.look(false, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
		if r.starAtomRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 25, res, r.pos)
	r.level--
	return res
}

func (r *rec) starAtomRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && r.targetWithStarAtomRule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && (r.starTargetsTupleSeqRule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(9) && (r.starTargetsListSeqRule() || true) && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) singleTargetRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.singleSubscriptAttributeTargetRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && r.singleTargetRule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) singleSubscriptAttributeTargetRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tPrimaryRule() && r.tok(23) && r.tok(cNAME) && r.look(false, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
		if r.tPrimaryRule() && r.tok(9) && r.slicesRule() && r.tok(10) && r.look(false, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tPrimaryRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(26); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 26, res, r.pos)
		r.pos = mark
		if !r.tPrimaryRaw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) tPrimaryRaw() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tPrimaryRule() && r.tok(23) && r.tok(cNAME) && r.look(true, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
		if r.tPrimaryRule() && r.tok(9) && r.slicesRule() && r.tok(10) && r.look(true, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
		if r.tPrimaryRule() && r.genexpRule() && r.look(true, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
		if r.tPrimaryRule() && r.tok(7) && (r.argumentsRule() || true) && r.tok(8) && r.look(true, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
		if r.atomRule() && r.look(true, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tLookaheadRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(9) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(23) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) delTargetsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather102Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) delTargetRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(27); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tPrimaryRule() && r.tok(23) && r.tok(cNAME) && r.look(false, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
		if r.tPrimaryRule() && r.tok(9) && r.slicesRule() && r.tok(10) && r.look(false, r.tLookaheadRule) {
			res = true
			break
		}
		r.pos = mark
		if r.delTAtomRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 27, res, r.pos)
	r.level--
	return res
}

func (r *rec) delTAtomRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && r.delTargetRule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && (r.delTargetsRule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(9) && (r.delTargetsRule() || true) && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) funcTypeCommentRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNEWLINE) && r.tok(cTYPECOMMENT) && r.look(true, r.tmp105Rule) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidDoubleTypeCommentsRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(cTYPECOMMENT) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidArgumentsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp106Rule() && r.tok(12) && r.gather108Rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.expressionRule() && r.forIfClausesRule() && r.tok(12) && (r.tmp109Rule() || true) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(cNAME) && r.tok(22) && r.expressionRule() && r.forIfClausesRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tmp110Rule() || true) && r.tok(cNAME) && r.tok(22) && r.look(true, r.tmp111Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if s := new([3]int); r.at(&s[0]) && r.argsRule() && r.at(&s[1]) && r.forIfClausesRule() && r.at(&s[2]) {
			res = r.hGenexpCall(s[0], s[1])
			break
		}
		r.pos = mark
		if r.argsRule() && r.tok(12) && r.expressionRule() && r.forIfClausesRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.argsRule() && r.tok(12) && r.argsRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidKwargRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp112Rule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(cNAME) && r.tok(22) && r.expressionRule() && r.forIfClausesRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.look(false, r.tmp113Rule) && r.expressionRule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.expressionRule() && r.tok(22) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) expressionWithoutInvalidRule() bool {
	prev := r.invalid
	r.invalid = false
	ok := r.expressionWithoutInvalidRuleBody()
	r.invalid = prev
	return ok
}

func (r *rec) expressionWithoutInvalidRuleBody() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.disjunctionRule() && r.tok(682) && r.disjunctionRule() && r.tok(686) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
		if r.disjunctionRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lambdefRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidLegacyExpressionRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([4]int); r.at(&s[0]) && r.tok(cNAME) && r.at(&s[1]) && r.lookTok(false, 7) && r.at(&s[2]) && r.starExpressionsRule() && r.at(&s[3]) {
			res = r.hLegacy(s[0], s[1])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidTypeParamRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.tok(cNAME) && r.tok(11) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.tok(cNAME) && r.tok(11) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidExpressionRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cSTRING) && r.loopSome114Rule() && r.tok(cSTRING) {
			res = r.raise()
			break
		}
		r.pos = mark
		if s := new([4]int); r.at(&s[0]) && r.look(false, r.tmp115Rule) && r.at(&s[1]) && r.disjunctionRule() && r.at(&s[2]) && r.expressionWithoutInvalidRule() && r.at(&s[3]) {
			res = r.hForgotComma(s[1], s[2])
			break
		}
		r.pos = mark
		if r.disjunctionRule() && r.tok(682) && r.disjunctionRule() && r.look(false, r.tmp116Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.disjunctionRule() && r.tok(682) && r.disjunctionRule() && r.tok(686) && r.look(false, r.expressionRule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tmp117Rule() && r.tok(682) && r.disjunctionRule() && r.tok(686) && r.simpleStmtRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(621) && (r.lambdaParamsRule() || true) && r.tok(11) && r.lookTok(true, cFMIDDLE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(621) && (r.lambdaParamsRule() || true) && r.tok(11) && r.lookTok(true, cTMIDDLE) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidNamedExpressionRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(28); hit {
		r.level--
		return ok
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expressionRule() && r.tok(53) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(cNAME) && r.tok(22) && r.bitwiseOrRule() && r.look(false, r.tmp118Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.look(false, r.tmp119Rule) && r.bitwiseOrRule() && r.tok(22) && r.bitwiseOrRule() && r.look(false, r.tmp118Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 28, res, r.pos)
	r.level--
	return res
}

func (r *rec) invalidAssignmentRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalidAnnAssignTargetRule() && r.tok(11) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.starNamedExpressionRule() && r.tok(12) && r.loopAny120Rule() && r.tok(11) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.expressionRule() && r.tok(11) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if s := new([4]int); r.at(&s[0]) && r.loopAny121Rule() && r.at(&s[1]) && r.starExpressionsRule() && r.at(&s[2]) && r.tok(22) && r.at(&s[3]) {
			res = r.hInvalidTarget(starTargets, s[1], s[2])
			break
		}
		r.pos = mark
		if r.loopAny121Rule() && r.yieldExprRule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.starExpressionsRule() && r.augassignRule() && r.annotatedRhsRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidAnnAssignTargetRule() bool {
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
		if r.tok(7) && r.invalidAnnAssignTargetRule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidDelStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([3]int); r.at(&s[0]) && r.tok(625) && r.at(&s[1]) && r.starExpressionsRule() && r.at(&s[2]) {
			res = r.hInvalidTarget(delTargets, s[1], s[2])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidBlockRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidComprehensionRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp122Rule() && r.starredExpressionRule() && r.forIfClausesRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tmp123Rule() && r.starNamedExpressionRule() && r.tok(12) && r.starNamedExpressionsRule() && r.forIfClausesRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tmp123Rule() && r.starNamedExpressionRule() && r.tok(12) && r.forIfClausesRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidDictComprehensionRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.tok(35) && r.bitwiseOrRule() && r.forIfClausesRule() && r.tok(26) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}
