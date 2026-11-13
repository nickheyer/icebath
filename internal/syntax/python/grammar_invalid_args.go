package python

func (r *rec) invalidParametersRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(17) && r.tok(12) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tmp124Rule() && r.loopAny32Rule() && r.tok(17) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.slashNoDefaultRule() || true) && r.loopAny28Rule() && r.invalidParametersHelperRule() && r.paramNoDefaultRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.loopAny28Rule() && r.tok(7) && r.loopSome30Rule() && (r.tok(12) || true) && r.tok(8) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tmp124Rule() || true) && r.loopAny32Rule() && r.tok(16) && r.tmp125Rule() && r.loopAny32Rule() && r.tok(17) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.loopSome33Rule() && r.tok(17) && r.tok(16) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) && r.look(true, r.tmp126Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidStarEtcRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.tmp127Rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(16) && r.tok(12) && r.tok(cTYPECOMMENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(16) && r.paramRule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(16) && r.tmp128Rule() && r.loopAny32Rule() && r.tok(16) && r.tmp128Rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidKwdsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(35) && r.paramRule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.paramRule() && r.tok(12) && r.paramRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.paramRule() && r.tok(12) && r.tmp129Rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidParametersHelperRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.slashWithDefaultRule() {
			res = true
			break
		}
		r.pos = mark
		if r.loopSome31Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidLambdaParametersRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(17) && r.tok(12) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tmp130Rule() && r.loopAny75Rule() && r.tok(17) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.lambdaSlashNoDefaultRule() || true) && r.loopAny71Rule() && r.invalidLambdaParametersHelperRule() && r.lambdaParamNoDefaultRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.loopAny71Rule() && r.tok(7) && r.gather132Rule() && (r.tok(12) || true) && r.tok(8) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tmp130Rule() || true) && r.loopAny75Rule() && r.tok(16) && r.tmp133Rule() && r.loopAny75Rule() && r.tok(17) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.loopSome76Rule() && r.tok(17) && r.tok(16) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidLambdaParametersHelperRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambdaSlashWithDefaultRule() {
			res = true
			break
		}
		r.pos = mark
		if r.loopSome74Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidLambdaStarEtcRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.tmp134Rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(16) && r.lambdaParamRule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(16) && r.tmp135Rule() && r.loopAny75Rule() && r.tok(16) && r.tmp135Rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidLambdaKwdsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(35) && r.lambdaParamRule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.lambdaParamRule() && r.tok(12) && r.lambdaParamRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.lambdaParamRule() && r.tok(12) && r.tmp129Rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidDoubleTypeCommentsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cTYPECOMMENT) && r.tok(cNEWLINE) && r.tok(cTYPECOMMENT) && r.tok(cNEWLINE) && r.tok(cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidWithItemRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([5]int); r.at(&s[0]) && r.expressionRule() && r.at(&s[1]) && r.tok(680) && r.at(&s[2]) && r.expressionRule() && r.at(&s[3]) && r.look(true, r.tmp36Rule) && r.at(&s[4]) {
			res = r.hInvalidTarget(starTargets, s[2], s[3])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidForIfClauseRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tok(698) || true) && r.tok(694) && r.tmp136Rule() && r.lookTok(false, 695) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidForTargetRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([4]int); r.at(&s[0]) && (r.tok(698) || true) && r.at(&s[1]) && r.tok(694) && r.at(&s[2]) && r.starExpressionsRule() && r.at(&s[3]) {
			res = r.hInvalidTarget(forTargets, s[2], s[3])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidGroupRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r.starredExpressionRule() && r.tok(8) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(7) && r.tok(35) && r.expressionRule() && r.tok(8) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidImportRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(634) && r.gather138Rule() && r.tok(633) && r.dottedNameRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(634) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidDottedAsNameRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.dottedNameRule() && r.tok(680) && r.look(false, r.tmp139Rule) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidImportFromAsNameRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tok(680) && r.look(false, r.tmp139Rule) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidImportFromTargetsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.importFromAsNamesRule() && r.tok(12) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidWithStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tok(698) || true) && r.tok(647) && r.gather141Rule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tok(698) || true) && r.tok(647) && r.tok(7) && r.gather143Rule() && (r.tok(12) || true) && r.tok(8) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidWithStmtIndentRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tok(698) || true) && r.tok(647) && r.gather141Rule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tok(698) || true) && r.tok(647) && r.tok(7) && r.gather143Rule() && (r.tok(12) || true) && r.tok(8) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidTryStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(656) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(656) && r.tok(11) && r.blockRule() && r.look(false, r.tmp144Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(656) && r.tok(11) && r.loopAny145Rule() && r.loopSome37Rule() && r.tok(677) && r.tok(16) && r.expressionRule() && (r.tmp22Rule() || true) && r.tok(11) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(656) && r.tok(11) && r.loopAny145Rule() && r.loopSome38Rule() && r.tok(677) && (r.tmp146Rule() || true) && r.tok(11) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidExceptStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(677) && r.expressionRule() && r.tok(12) && r.expressionsRule() && r.tok(680) && r.tok(cNAME) && r.tok(11) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.expressionRule() && (r.tmp22Rule() || true) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.expressionRule() && r.tok(680) && r.expressionRule() && r.tok(11) && r.blockRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidExceptStarStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(677) && r.tok(16) && r.expressionRule() && r.tok(12) && r.expressionsRule() && r.tok(680) && r.tok(cNAME) && r.tok(11) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(16) && r.expressionRule() && (r.tmp22Rule() || true) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(16) && r.tmp147Rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(16) && r.expressionRule() && r.tok(680) && r.expressionRule() && r.tok(11) && r.blockRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidFinallyStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(673) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidExceptStmtIndentRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(677) && r.expressionRule() && (r.tmp22Rule() || true) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidExceptStarStmtIndentRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(677) && r.tok(16) && r.expressionRule() && (r.tmp22Rule() || true) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidMatchStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.soft("match") && r.subjectExprRule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.soft("match") && r.subjectExprRule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidCaseBlockRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.soft("case") && r.patternsRule() && (r.guardRule() || true) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.soft("case") && r.patternsRule() && (r.guardRule() || true) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidAsPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.orPatternRule() && r.tok(680) && r.soft("_") {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.orPatternRule() && r.tok(680) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidClassPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.nameOrAttrRule() && r.tok(7) && r.invalidClassArgumentPatternRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidClassArgumentPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tmp148Rule() || true) && r.keywordPatternsRule() && r.tok(12) && r.positionalPatternsRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidIfStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(682) && r.namedExpressionRule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(682) && r.namedExpressionRule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidElifStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(687) && r.namedExpressionRule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(687) && r.namedExpressionRule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidElseStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(686) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(686) && r.tok(11) && r.blockRule() && r.tok(687) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidWhileStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(689) && r.namedExpressionRule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(689) && r.namedExpressionRule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidForStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tok(698) || true) && r.tok(694) && r.starTargetsRule() && r.tok(695) && r.starExpressionsRule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tok(698) || true) && r.tok(694) && r.starTargetsRule() && r.tok(695) && r.starExpressionsRule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidDefRawRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tok(698) || true) && r.tok(699) && r.tok(cNAME) && (r.typeParamsRule() || true) && r.tok(7) && (r.paramsRule() || true) && r.tok(8) && (r.tmp27Rule() || true) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tok(698) || true) && r.tok(699) && r.tok(cNAME) && (r.typeParamsRule() || true) && r.forced(7) && (r.paramsRule() || true) && r.tok(8) && (r.tmp27Rule() || true) && r.forced(11) && (r.funcTypeCommentRule() || true) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidClassDefRawRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(701) && r.tok(cNAME) && (r.typeParamsRule() || true) && (r.tmp26Rule() || true) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(701) && r.tok(cNAME) && (r.typeParamsRule() || true) && (r.tmp26Rule() || true) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidDoubleStarredKvpairsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather85Rule() && r.tok(12) && r.invalidKvpairRule() {
			res = true
			break
		}
		r.pos = mark
		if r.expressionRule() && r.tok(11) && r.tok(16) && r.bitwiseOrRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.expressionRule() && r.tok(11) && r.look(true, r.tmp149Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidKvpairRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expressionRule() && r.lookTok(false, 11) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.expressionRule() && r.tok(11) && r.tok(16) && r.bitwiseOrRule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.expressionRule() && r.tok(11) && r.look(true, r.tmp149Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidStarredExpressionUnpackingRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.expressionRule() && r.tok(22) && r.expressionRule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidStarredExpressionRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(16) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidFstringReplacementFieldRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.tok(54) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.tok(11) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.tok(26) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.look(false, r.annotatedRhsRule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && r.look(false, r.tmp150Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && r.tok(22) && r.look(false, r.tmp151Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && r.invalidFstringConversionCharacterRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && (r.tmp152Rule() || true) && r.look(false, r.tmp153Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && (r.tmp152Rule() || true) && r.tok(11) && r.loopAny77Rule() && r.lookTok(false, 26) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && (r.tmp152Rule() || true) && r.lookTok(false, 26) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidFstringConversionCharacterRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(54) && r.look(true, r.tmp153Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(54) && r.look(false, r.nameTok) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidTstringReplacementFieldRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.tok(54) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.tok(11) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.tok(26) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.look(false, r.annotatedRhsRule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && r.look(false, r.tmp150Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && r.tok(22) && r.look(false, r.tmp151Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && r.invalidTstringConversionCharacterRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && (r.tmp152Rule() || true) && r.look(false, r.tmp153Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && (r.tmp152Rule() || true) && r.tok(11) && r.loopAny77Rule() && r.lookTok(false, 26) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && (r.tmp152Rule() || true) && r.lookTok(false, 26) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalidTstringConversionCharacterRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(54) && r.look(true, r.tmp153Rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(54) && r.look(false, r.nameTok) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}
