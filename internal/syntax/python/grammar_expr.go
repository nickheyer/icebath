package python

func (r *rec) shiftExprRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(15); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 15, res, r.pos)
		r.pos = mark
		if !r.shiftExprRaw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) shiftExprRaw() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.shiftExprRule() && r.tok(33) && r.sumRule() {
			res = true
			break
		}
		r.pos = mark
		if r.shiftExprRule() && r.tok(34) && r.sumRule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidArithmeticRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.sumRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) sumRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(16); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 16, res, r.pos)
		r.pos = mark
		if !r.sumRaw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) sumRaw() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.sumRule() && r.tok(14) && r.termRule() {
			res = true
			break
		}
		r.pos = mark
		if r.sumRule() && r.tok(15) && r.termRule() {
			res = true
			break
		}
		r.pos = mark
		if r.termRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) termRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(17); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 17, res, r.pos)
		r.pos = mark
		if !r.termRaw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) termRaw() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.termRule() && r.tok(16) && r.factorRule() {
			res = true
			break
		}
		r.pos = mark
		if r.termRule() && r.tok(17) && r.factorRule() {
			res = true
			break
		}
		r.pos = mark
		if r.termRule() && r.tok(47) && r.factorRule() {
			res = true
			break
		}
		r.pos = mark
		if r.termRule() && r.tok(24) && r.factorRule() {
			res = true
			break
		}
		r.pos = mark
		if r.termRule() && r.tok(49) && r.factorRule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidFactorRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.factorRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) factorRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(18); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(14) && r.factorRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(15) && r.factorRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(31) && r.factorRule() {
			res = true
			break
		}
		r.pos = mark
		if r.powerRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 18, res, r.pos)
	r.level--
	return res
}

func (r *rec) powerRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.awaitPrimaryRule() && r.tok(35) && r.factorRule() {
			res = true
			break
		}
		r.pos = mark
		if r.awaitPrimaryRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) awaitPrimaryRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(19); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(597) && r.primaryRule() {
			res = true
			break
		}
		r.pos = mark
		if r.primaryRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 19, res, r.pos)
	r.level--
	return res
}

func (r *rec) primaryRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(20); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 20, res, r.pos)
		r.pos = mark
		if !r.primaryRaw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) primaryRaw() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.primaryRule() && r.tok(23) && r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
		if r.primaryRule() && r.genexpRule() {
			res = true
			break
		}
		r.pos = mark
		if r.primaryRule() && r.tok(7) && (r.argumentsRule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.primaryRule() && r.tok(9) && r.slicesRule() && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
		if r.atomRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) slicesRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.sliceRule() && r.lookTok(false, 12) {
			res = true
			break
		}
		r.pos = mark
		if r.gather65Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) sliceRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if (r.expressionRule() || true) && r.tok(11) && (r.expressionRule() || true) && (r.tmp66Rule() || true) {
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

func (r *rec) atomRule() bool {
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
		if r.look(true, r.tmp43Rule) && r.stringsRule() {
			res = true
			break
		}
		r.pos = mark
		if r.number() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 7) && r.tmp67Rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 9) && r.tmp68Rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 25) && r.tmp69Rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(52) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) groupRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r.tmp70Rule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidGroupRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) lambdefRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(621) && (r.lambdaParamsRule() || true) && r.tok(11) && r.expressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambdaParamsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidLambdaParametersRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.lambdaParametersRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambdaParametersRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambdaSlashNoDefaultRule() && r.loopAny71Rule() && r.loopAny72Rule() && (r.lambdaStarEtcRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.lambdaSlashWithDefaultRule() && r.loopAny72Rule() && (r.lambdaStarEtcRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.loopSome73Rule() && r.loopAny72Rule() && (r.lambdaStarEtcRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.loopSome74Rule() && (r.lambdaStarEtcRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.lambdaStarEtcRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambdaSlashNoDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.loopSome73Rule() && r.tok(17) && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.loopSome73Rule() && r.tok(17) && r.lookTok(true, 11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambdaSlashWithDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.loopAny71Rule() && r.loopSome74Rule() && r.tok(17) && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.loopAny71Rule() && r.loopSome74Rule() && r.tok(17) && r.lookTok(true, 11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambdaStarEtcRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidLambdaStarEtcRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(16) && r.lambdaParamNoDefaultRule() && r.loopAny75Rule() && (r.lambdaKwdsRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) && r.tok(12) && r.loopSome76Rule() && (r.lambdaKwdsRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.lambdaKwdsRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambdaKwdsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidLambdaKwdsRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(35) && r.lambdaParamNoDefaultRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambdaParamNoDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambdaParamRule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.lambdaParamRule() && r.lookTok(true, 11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambdaParamWithDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambdaParamRule() && r.defaultRule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.lambdaParamRule() && r.defaultRule() && r.lookTok(true, 11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambdaParamMaybeDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambdaParamRule() && (r.defaultRule() || true) && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.lambdaParamRule() && (r.defaultRule() || true) && r.lookTok(true, 11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambdaParamRule() bool {
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
	}
	r.level--
	return res
}

func (r *rec) fstringMiddleRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.fstringReplacementFieldRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(cFMIDDLE) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) fstringReplacementFieldRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && (r.fstringConversionRule() || true) && (r.fstringFullFormatSpecRule() || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidFstringReplacementFieldRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) fstringConversionRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([3]int); r.at(&s[0]) && r.tok(54) && r.at(&s[1]) && r.tok(cNAME) && r.at(&s[2]) {
			res = r.hConversion(s[0], s[1])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) fstringFullFormatSpecRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(11) && r.loopAny77Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) fstringFormatSpecRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cFMIDDLE) {
			res = true
			break
		}
		r.pos = mark
		if r.fstringReplacementFieldRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) fstringRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cFSTART) && r.loopAny78Rule() && r.tok(cFEND) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tstringFormatSpecReplacementFieldRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && (r.fstringConversionRule() || true) && (r.tstringFullFormatSpecRule() || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidTstringReplacementFieldRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) tstringFormatSpecRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cTMIDDLE) {
			res = true
			break
		}
		r.pos = mark
		if r.tstringFormatSpecReplacementFieldRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tstringFullFormatSpecRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(11) && r.loopAny79Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tstringReplacementFieldRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.annotatedRhsRule() && (r.tok(22) || true) && (r.fstringConversionRule() || true) && (r.tstringFullFormatSpecRule() || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidTstringReplacementFieldRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) tstringMiddleRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tstringReplacementFieldRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(cTMIDDLE) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tstringRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(21); hit {
		r.level--
		return ok
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cTSTART) && r.loopAny80Rule() && r.tok(cTEND) {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 21, res, r.pos)
	r.level--
	return res
}

func (r *rec) stringRule() bool {
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
	}
	r.level--
	return res
}

func (r *rec) stringsRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(22); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidStringTstringConcatRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if s := new([2]int); r.at(&s[0]) && r.loopSome81Rule() && r.at(&s[1]) {
			res = r.hStrings(s[0])
			break
		}
		r.pos = mark
		if s := new([2]int); r.at(&s[0]) && r.loopSome82Rule() && r.at(&s[1]) {
			res = r.hStrings(s[0])
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 22, res, r.pos)
	r.level--
	return res
}

func (r *rec) listRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(9) && (r.starNamedExpressionsRule() || true) && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tupleRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(7) && (r.tmp83Rule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) setRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.starNamedExpressionsRule() && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) dictRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && (r.doubleStarredKvpairsRule() || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.invalidDoubleStarredKvpairsRule() && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) doubleStarredKvpairsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather85Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) doubleStarredKvpairRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(35) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
		if r.kvpairRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}
