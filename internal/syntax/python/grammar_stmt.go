package python

func (r *rec) fileRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.statementsRule() || true) && r.tok(cENDMARKER) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) evalRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expressionsRule() && r.loopAny1Rule() && r.tok(cENDMARKER) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) statementsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.loopSome2Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) statementRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.compoundStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.simpleStmtsRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) simpleStmtsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.simpleStmtRule() && r.lookTok(false, 13) && r.tok(cNEWLINE) {
			res = true
			break
		}
		r.pos = mark
		if r.gather4Rule() && (r.tok(13) || true) && r.tok(cNEWLINE) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) simpleStmtRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(0); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.assignmentRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookSoft(true, "type") && r.typeAliasRule() {
			res = true
			break
		}
		r.pos = mark
		if r.starExpressionsRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 522) && r.returnStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.look(true, r.tmp5Rule) && r.importStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 525) && r.raiseStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 526) && r.passStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 625) && r.delStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 587) && r.yieldStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 532) && r.assertStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 527) && r.breakStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 528) && r.continueStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 529) && r.globalStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 530) && r.nonlocalStmtRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 0, res, r.pos)
	r.level--
	return res
}

func (r *rec) compoundStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.look(true, r.tmp6Rule) && r.functionDefRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 682) && r.ifStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.look(true, r.tmp7Rule) && r.classDefRule() {
			res = true
			break
		}
		r.pos = mark
		if r.look(true, r.tmp8Rule) && r.withStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.look(true, r.tmp9Rule) && r.forStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 656) && r.tryStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 689) && r.whileStmtRule() {
			res = true
			break
		}
		r.pos = mark
		if r.matchStmtRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) assignmentRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	cut := false
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tok(11) && r.expressionRule() && (r.tmp10Rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tmp11Rule() && r.tok(11) && r.expressionRule() && (r.tmp10Rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.loopSome12Rule() && r.annotatedRhsRule() && r.lookTok(false, 22) && (r.tok(cTYPECOMMENT) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.singleTargetRule() && r.augassignRule() && r.cut(&cut) && r.annotatedRhsRule() {
			res = true
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
		}
		if r.invalid {
			if r.invalidAssignmentRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) annotatedRhsRule() bool {
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
		if r.starExpressionsRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) augassignRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(36) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(37) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(38) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(50) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(39) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(40) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(41) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(42) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(43) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(44) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(45) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(46) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(48) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) returnStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(522) && (r.starExpressionsRule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) raiseStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(525) && r.expressionRule() && (r.tmp13Rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(525) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) passStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(526) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) breakStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(527) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) continueStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(528) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) globalStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(529) && r.gather15Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) nonlocalStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(530) && r.gather15Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) delStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(625) && r.delTargetsRule() && r.look(true, r.tmp16Rule) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidDelStmtRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) yieldStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.yieldExprRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) assertStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(532) && r.expressionRule() && (r.tmp17Rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) importStmtRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidImportRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.importNameRule() {
			res = true
			break
		}
		r.pos = mark
		if r.importFromRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) importNameRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(634) && r.dottedAsNamesRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) importFromRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if s := new([6]int); r.at(&s[0]) && r.tok(633) && r.at(&s[1]) && r.loopAny18Rule() && r.at(&s[2]) && r.dottedNameRule() && r.at(&s[3]) && r.tok(634) && r.at(&s[4]) && r.importFromTargetsRule() && r.at(&s[5]) {
			res = r.hFuture(s[0])
			break
		}
		r.pos = mark
		if r.tok(633) && r.loopSome19Rule() && r.tok(634) && r.importFromTargetsRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) importFromTargetsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r.importFromAsNamesRule() && (r.tok(12) || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.importFromAsNamesRule() && r.lookTok(false, 12) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidImportFromTargetsRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) importFromAsNamesRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather21Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) importFromAsNameRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidImportFromAsNameRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(cNAME) && (r.tmp22Rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) dottedAsNamesRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather24Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) dottedAsNameRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidDottedAsNameRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.dottedNameRule() && (r.tmp22Rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) dottedNameRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(1); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 1, res, r.pos)
		r.pos = mark
		if !r.dottedNameRaw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) dottedNameRaw() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.dottedNameRule() && r.tok(23) && r.tok(cNAME) {
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

func (r *rec) blockRule() bool {
	if !r.enter() {
		return false
	}
	if ok, hit := r.memoGet(2); hit {
		r.level--
		return ok
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNEWLINE) && r.tok(cINDENT) && r.statementsRule() && r.tok(cDEDENT) {
			res = true
			break
		}
		r.pos = mark
		if r.simpleStmtsRule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalidBlockRule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.memoPut(mark, 2, res, r.pos)
	r.level--
	return res
}

func (r *rec) decoratorsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.loopSome25Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) classDefRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.decoratorsRule() && r.classDefRawRule() {
			res = true
			break
		}
		r.pos = mark
		if r.classDefRawRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) classDefRawRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidClassDefRawRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(701) && r.tok(cNAME) && (r.typeParamsRule() || true) && (r.tmp26Rule() || true) && r.tok(11) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) functionDefRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.decoratorsRule() && r.functionDefRawRule() {
			res = true
			break
		}
		r.pos = mark
		if r.functionDefRawRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) functionDefRawRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidDefRawRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(699) && r.tok(cNAME) && (r.typeParamsRule() || true) && r.tok(7) && (r.paramsRule() || true) && r.tok(8) && (r.tmp27Rule() || true) && r.tok(11) && (r.funcTypeCommentRule() || true) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(698) && r.tok(699) && r.tok(cNAME) && (r.typeParamsRule() || true) && r.tok(7) && (r.paramsRule() || true) && r.tok(8) && (r.tmp27Rule() || true) && r.tok(11) && (r.funcTypeCommentRule() || true) && r.blockRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) paramsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidParametersRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.parametersRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) parametersRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.slashNoDefaultRule() && r.loopAny28Rule() && r.loopAny29Rule() && (r.starEtcRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.slashWithDefaultRule() && r.loopAny29Rule() && (r.starEtcRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.loopSome30Rule() && r.loopAny29Rule() && (r.starEtcRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.loopSome31Rule() && (r.starEtcRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.starEtcRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) slashNoDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.loopSome30Rule() && r.tok(17) && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.loopSome30Rule() && r.tok(17) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) slashWithDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.loopAny28Rule() && r.loopSome31Rule() && r.tok(17) && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.loopAny28Rule() && r.loopSome31Rule() && r.tok(17) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starEtcRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidStarEtcRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(16) && r.paramNoDefaultRule() && r.loopAny32Rule() && (r.kwdsRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) && r.paramNoDefaultStarAnnotationRule() && r.loopAny32Rule() && (r.kwdsRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) && r.tok(12) && r.loopSome33Rule() && (r.kwdsRule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.kwdsRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) kwdsRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalidKwdsRule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(35) && r.paramNoDefaultRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) paramNoDefaultRule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.paramRule() && r.tok(12) && (r.tok(cTYPECOMMENT) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.paramRule() && (r.tok(cTYPECOMMENT) || true) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}
