package python

func (r *rec) paramNoDefaultStarAnnotationRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.paramStarAnnotationRule() && r.tok(12) && (r.tok(cTYPECOMMENT) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.paramStarAnnotationRule() && (r.tok(cTYPECOMMENT) || true) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) paramWithDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.paramRule() && r.defaultRule() && r.tok(12) && (r.tok(cTYPECOMMENT) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.paramRule() && r.defaultRule() && (r.tok(cTYPECOMMENT) || true) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) paramMaybeDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.paramRule() && (r.defaultRule() || true) && r.tok(12) && (r.tok(cTYPECOMMENT) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.paramRule() && (r.defaultRule() || true) && (r.tok(cTYPECOMMENT) || true) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) paramRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && (r.annotationRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) paramStarAnnotationRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.starAnnotationRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) annotationRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starAnnotationRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) && r.starExpressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) defaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidDefaultRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) ifStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidIfStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(682) && r.namedExpressionRule() && r.tok(11) && r.blockRule() && r.elifStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(682) && r.namedExpressionRule() && r.tok(11) && r.blockRule() && (r.elseBlockRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) elifStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidElifStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(687) && r.namedExpressionRule() && r.tok(11) && r.blockRule() && r.elifStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(687) && r.namedExpressionRule() && r.tok(11) && r.blockRule() && (r.elseBlockRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) elseBlockRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidElseStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(686) && r.forced(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) whileStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidWhileStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(689) && r.namedExpressionRule() && r.tok(11) && r.blockRule() && (r.elseBlockRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) forStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	cut := false
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidForStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(694) && r.starTargetsRule() && r.tok(695) && r.cut(&cut) && r.starExpressionsRule() && r.tok(11) && (r.tok(cTYPECOMMENT) || true) && r.blockRule() && (r.elseBlockRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
		}
		if r.tok(698) && r.tok(694) && r.starTargetsRule() && r.tok(695) && r.cut(&cut) && r.starExpressionsRule() && r.tok(11) && (r.tok(cTYPECOMMENT) || true) && r.blockRule() && (r.elseBlockRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
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

func (r *rec) withStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidWithStmtIndentRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(647) && r.tok(7) && r.gather35Rule() && (r.tok(12) || true) && r.tok(8) && r.tok(11) && (r.tok(cTYPECOMMENT) || true) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(647) && r.gather35Rule() && r.tok(11) && (r.tok(cTYPECOMMENT) || true) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(698) && r.tok(647) && r.tok(7) && r.gather35Rule() && (r.tok(12) || true) && r.tok(8) && r.tok(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(698) && r.tok(647) && r.gather35Rule() && r.tok(11) && (r.tok(cTYPECOMMENT) || true) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidWithStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) withItemRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expressionRule() && r.tok(680) && r.starTargetRule() && r.look(true, r.tmp36Rule) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidWithItemRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tryStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidTryStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(656) && r.forced(11) && r.blockRule() && r.finallyBlockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(656) && r.forced(11) && r.blockRule() && r.loopSome37Rule() && (r.elseBlockRule() || true) && (r.finallyBlockRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(656) && r.forced(11) && r.blockRule() && r.loopSome38Rule() && (r.elseBlockRule() || true) && (r.finallyBlockRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) exceptBlockRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidExceptStmtIndentRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(677) && r.expressionRule() && r.tok(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(677) && r.expressionRule() && r.tok(680) && r.tok(cNAME) && r.tok(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(677) && r.expressionsRule() && r.tok(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidExceptStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) exceptStarBlockRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidExceptStarStmtIndentRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(677) && r.tok(16) && r.expressionRule() && r.tok(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(16) && r.expressionRule() && r.tok(680) && r.tok(cNAME) && r.tok(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(16) && r.expressionsRule() && r.tok(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidExceptStarStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) finallyBlockRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidFinallyStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(673) && r.forced(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) matchStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.soft("match") && r.subjectExprRule() && r.tok(11) && r.tok(cNEWLINE) && r.tok(cINDENT) && r.loopSome39Rule() && r.tok(cDEDENT) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidMatchStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) subjectExprRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.starNamedExpressionRule() && r.tok(12) && (r.starNamedExpressionsRule() || true) {
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

func (r *rec) caseBlockRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidCaseBlockRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.soft("case") && r.patternsRule() && (r.guardRule() || true) && r.tok(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) guardRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(682) && r.namedExpressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) patternsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.openSequencePatternRule() {
			res = true
			break
		}
		r.pos = mark
		if r.patternRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) patternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.asPatternRule() {
			res = true
			break
		}
		r.pos = mark
		if r.orPatternRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) asPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.orPatternRule() && r.tok(680) && r.patternCaptureTargetRule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidAsPatternRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) orPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.gather41Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) closedPatternRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(3); hit {
		r.level--
		return ok
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.literalPatternRule() {
			res = true
			break
		}
		r.pos = mark
		if r.capturePatternRule() {
			res = true
			break
		}
		r.pos = mark
		if r.wildcardPatternRule() {
			res = true
			break
		}
		r.pos = mark
		if r.valuePatternRule() {
			res = true
			break
		}
		r.pos = mark
		if r.groupPatternRule() {
			res = true
			break
		}
		r.pos = mark
		if r.sequencePatternRule() {
			res = true
			break
		}
		r.pos = mark
		if r.mappingPatternRule() {
			res = true
			break
		}
		r.pos = mark
		if r.classPatternRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 3, res, r.pos)
	r.level--
	return res
}

func (r *rec) literalPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.signedNumberRule() && r.look(false, r.tmp42Rule) {
			res = true
			break
		}
		r.pos = mark
		if r.complexNumberRule() {
			res = true
			break
		}
		r.pos = mark
		if r.stringsRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(623) {
			res = true
			break
		}
		r.pos = mark
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
	}
	r.level--
	return res
}

func (r *rec) literalExprRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.signedNumberRule() && r.look(false, r.tmp42Rule) {
			res = true
			break
		}
		r.pos = mark
		if r.complexNumberRule() {
			res = true
			break
		}
		r.pos = mark
		if r.look(true, r.tmp43Rule) && r.stringsRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(623) {
			res = true
			break
		}
		r.pos = mark
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
	}
	r.level--
	return res
}

func (r *rec) complexNumberRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.signedRealNumberRule() && r.tok(14) && r.imaginaryNumberRule() {
			res = true
			break
		}
		r.pos = mark
		if r.signedRealNumberRule() && r.tok(15) && r.imaginaryNumberRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) signedNumberRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.number() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(15) && r.number() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) signedRealNumberRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.realNumberRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(15) && r.realNumberRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) realNumberRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([2]int); r.at(&s[0]) && r.number() && r.at(&s[1]) {
			res = r.hEnsure(false, s[0])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) imaginaryNumberRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([2]int); r.at(&s[0]) && r.number() && r.at(&s[1]) {
			res = r.hEnsure(true, s[0])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) capturePatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.patternCaptureTargetRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) patternCaptureTargetRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lookSoft(false, "_") && r.tok(cNAME) && r.look(false, r.tmp44Rule) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) wildcardPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.soft("_") {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) valuePatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.attrRule() && r.look(false, r.tmp44Rule) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) attrRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(4); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 4, res, r.pos)
		r.pos = mark
		if !r.attrRaw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) attrRaw() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.nameOrAttrRule() && r.tok(23) && r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) nameOrAttrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.attrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) groupPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r.patternRule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) sequencePatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(9) && (r.maybeSequencePatternRule() || true) && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && (r.openSequencePatternRule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}
