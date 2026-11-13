package python

func (r *rec) openSequencePatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.maybeStarPatternRule() && r.tok(12) && (r.maybeSequencePatternRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) maybeSequencePatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather46Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) maybeStarPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.starPatternRule() {
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

func (r *rec) starPatternRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(5); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.patternCaptureTargetRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) && r.wildcardPatternRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 5, res, r.pos)
	r.level--
	return res
}

func (r *rec) mappingPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.doubleStarPatternRule() && (r.tok(12) || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.itemsPatternRule() && r.tok(12) && r.doubleStarPatternRule() && (r.tok(12) || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.itemsPatternRule() && (r.tok(12) || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) itemsPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather48Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) keyValuePatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp49Rule() && r.tok(11) && r.patternRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) doubleStarPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(35) && r.patternCaptureTargetRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) classPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.nameOrAttrRule() && r.tok(7) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.nameOrAttrRule() && r.tok(7) && r.positionalPatternsRule() && (r.tok(12) || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.nameOrAttrRule() && r.tok(7) && r.keywordPatternsRule() && (r.tok(12) || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.nameOrAttrRule() && r.tok(7) && r.positionalPatternsRule() && r.tok(12) && r.keywordPatternsRule() && (r.tok(12) || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidClassPatternRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) positionalPatternsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather51Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) keywordPatternsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather53Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) keywordPatternRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tok(22) && r.patternRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) typeAliasRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.soft("type") && r.tok(cNAME) && (r.typeParamsRule() || true) && r.tok(22) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) typeParamsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidTypeParamsRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(9) && r.typeParamSeqRule() && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) typeParamSeqRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather55Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) typeParamRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(6); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && (r.typeParamBoundRule() || true) && (r.typeParamDefaultRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidTypeParamRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(16) && r.tok(cNAME) && (r.typeParamStarredDefaultRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(35) && r.tok(cNAME) && (r.typeParamDefaultRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 6, res, r.pos)
	r.level--
	return res
}

func (r *rec) typeParamBoundRule() bool {
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

func (r *rec) typeParamDefaultRule() bool {
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
	}
	r.level--
	return res
}

func (r *rec) typeParamStarredDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) && r.starExpressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) expressionsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.expressionRule() && r.loopSome56Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.expressionRule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) expressionRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(7); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidExpressionRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.invalid {
			if r.invalidLegacyExpressionRule() {
				res = true
				break
			}
			r.pos = mark
		}
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
	r.memoPut(mark, 7, res, r.pos)
	r.level--
	return res
}

func (r *rec) yieldExprRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(587) && r.tok(633) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(587) && (r.starExpressionsRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starExpressionsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.starExpressionRule() && r.loopSome57Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.starExpressionRule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.starExpressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starExpressionRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(8); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 8, res, r.pos)
	r.level--
	return res
}

func (r *rec) starNamedExpressionsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather59Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starNamedExpressionRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.bitwiseOrRule() {
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

func (r *rec) assignmentExpressionRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	cut := false
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tok(53) && r.cut(&cut) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
		}
	}
	r.level--
	return res
}

func (r *rec) namedExpressionRule() bool {
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
		if r.invalid {
			if r.invalidNamedExpressionRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.expressionRule() && r.lookTok(false, 53) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) disjunctionRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(9); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.conjunctionRule() && r.loopSome60Rule() {
			res = true
			break
		}
		r.pos = mark
		if r.conjunctionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 9, res, r.pos)
	r.level--
	return res
}

func (r *rec) conjunctionRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(10); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.inversionRule() && r.loopSome61Rule() {
			res = true
			break
		}
		r.pos = mark
		if r.inversionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 10, res, r.pos)
	r.level--
	return res
}

func (r *rec) inversionRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(11); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(703) && r.inversionRule() {
			res = true
			break
		}
		r.pos = mark
		if r.comparisonRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 11, res, r.pos)
	r.level--
	return res
}

func (r *rec) comparisonRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.bitwiseOrRule() && r.loopSome62Rule() {
			res = true
			break
		}
		r.pos = mark
		if r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) compareOpBitwiseOrPairRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.eqBitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.noteqBitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lteBitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidNoteqRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.ltBitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.gteBitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.gtBitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.notinBitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.inBitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.isnotBitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.isBitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) eqBitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(27) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) noteqBitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp63Rule() && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lteBitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(29) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) ltBitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(20) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) gteBitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(30) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) gtBitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(21) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) notinBitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(703) && r.tok(695) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) inBitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(695) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) isnotBitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(596) && r.tok(703) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) isBitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(596) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) bitwiseOrRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(12); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 12, res, r.pos)
		r.pos = mark
		if !r.bitwiseOrRaw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) bitwiseOrRaw() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.bitwiseOrRule() && r.tok(18) && r.bitwiseXorRule() {
			res = true
			break
		}
		r.pos = mark
		if r.bitwiseXorRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) bitwiseXorRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(13); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 13, res, r.pos)
		r.pos = mark
		if !r.bitwiseXorRaw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) bitwiseXorRaw() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.bitwiseXorRule() && r.tok(32) && r.bitwiseAndRule() {
			res = true
			break
		}
		r.pos = mark
		if r.bitwiseAndRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) bitwiseAndRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(14); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 14, res, r.pos)
		r.pos = mark
		if !r.bitwiseAndRaw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) bitwiseAndRaw() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.bitwiseAndRule() && r.tok(19) && r.shiftExprRule() {
			res = true
			break
		}
		r.pos = mark
		if r.shiftExprRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}
