package pyast

var cpyKeywords = map[string]int{"if": 682, "as": 680, "in": 695, "or": 588, "is": 596, "del": 625, "def": 699, "for": 694, "try": 656, "and": 589, "not": 703, "from": 633, "pass": 526, "with": 647, "elif": 687, "else": 686, "None": 623, "True": 622, "raise": 525, "yield": 587, "break": 527, "async": 698, "class": 701, "while": 689, "False": 624, "await": 597, "return": 522, "import": 634, "assert": 532, "global": 529, "except": 677, "lambda": 621, "finally": 673, "continue": 528, "nonlocal": 530}

var cpySoftKeywords = map[string]bool{"_": true, "case": true, "match": true, "type": true}

var cpyOps = map[string]int{"!": 54, "!=": 28, "%": 24, "%=": 40, "&": 19, "&=": 41, "(": 7, ")": 8, "*": 16, "**": 35, "**=": 46, "*=": 38, "+": 14, "+=": 36, ",": 12, "-": 15, "-=": 37, "->": 51, ".": 23, "...": 52, "/": 17, "//": 47, "//=": 48, "/=": 39, ":": 11, ":=": 53, ";": 13, "<": 20, "<<": 33, "<<=": 44, "<=": 29, "<>": 28, "=": 22, "==": 27, ">": 21, ">=": 30, ">>": 34, ">>=": 45, "@": 49, "@=": 50, "[": 9, "]": 10, "^": 32, "^=": 43, "{": 25, "|": 18, "|=": 42, "}": 26, "~": 31}

const (
	cENDMARKER   = 0
	cNAME        = 1
	cNUMBER      = 2
	cSTRING      = 3
	cNEWLINE     = 4
	cINDENT      = 5
	cDEDENT      = 6
	cTYPECOMMENT = 57
	cFSTART      = 59
	cFMIDDLE     = 60
	cFEND        = 61
	cTSTART      = 62
	cTMIDDLE     = 63
	cTEND        = 64
	cNOTEQUAL    = 28
	cMemoRules   = 29
)

func (r *rec) file_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.statements_rule() || true) && r.tok(cENDMARKER) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) interactive_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.statement_newline_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) eval_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expressions_rule() && r._loop0_1_rule() && r.tok(cENDMARKER) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) func_type_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && (r.type_expressions_rule() || true) && r.tok(8) && r.tok(51) && r.expression_rule() && r._loop0_1_rule() && r.tok(cENDMARKER) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) statements_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._loop1_2_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) statement_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.compound_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.simple_stmts_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) single_compound_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.compound_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) statement_newline_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.single_compound_stmt_rule() && r.tok(cNEWLINE) {
			res = true
			break
		}
		r.pos = mark
		if r.simple_stmts_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(cNEWLINE) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(cENDMARKER) {
			res = false
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) simple_stmts_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.simple_stmt_rule() && r.lookTok(false, 13) && r.tok(cNEWLINE) {
			res = true
			break
		}
		r.pos = mark
		if r._gather_4_rule() && (r.tok(13) || true) && r.tok(cNEWLINE) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) simple_stmt_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(0); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.assignment_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookSoft(true, "type") && r.type_alias_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.star_expressions_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 522) && r.return_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.look(true, r._tmp_5_rule) && r.import_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 525) && r.raise_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 526) && r.pass_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 625) && r.del_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 587) && r.yield_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 532) && r.assert_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 527) && r.break_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 528) && r.continue_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 529) && r.global_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 530) && r.nonlocal_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 0, res, r.pos)
	r.level--
	return res
}

func (r *rec) compound_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.look(true, r._tmp_6_rule) && r.function_def_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 682) && r.if_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.look(true, r._tmp_7_rule) && r.class_def_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.look(true, r._tmp_8_rule) && r.with_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.look(true, r._tmp_9_rule) && r.for_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 656) && r.try_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 689) && r.while_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.match_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) assignment_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	cut := false
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tok(11) && r.expression_rule() && (r._tmp_10_rule() || true) {
			res = r.hVersion(6)
			break
		}
		r.pos = mark
		if r._tmp_11_rule() && r.tok(11) && r.expression_rule() && (r._tmp_10_rule() || true) {
			res = r.hVersion(6)
			break
		}
		r.pos = mark
		if r._loop1_12_rule() && r.annotated_rhs_rule() && r.lookTok(false, 22) && (r.tok(cTYPECOMMENT) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.single_target_rule() && r.augassign_rule() && r.cut(&cut) && r.annotated_rhs_rule() {
			res = true
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
		}
		if r.invalid {
			if r.invalid_assignment_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) annotated_rhs_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.yield_expr_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.star_expressions_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) augassign_rule() bool {
	r.enter()
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

func (r *rec) return_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(522) && (r.star_expressions_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) raise_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(525) && r.expression_rule() && (r._tmp_13_rule() || true) {
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

func (r *rec) pass_stmt_rule() bool {
	r.enter()
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

func (r *rec) break_stmt_rule() bool {
	r.enter()
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

func (r *rec) continue_stmt_rule() bool {
	r.enter()
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

func (r *rec) global_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(529) && r._gather_15_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) nonlocal_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(530) && r._gather_15_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) del_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(625) && r.del_targets_rule() && r.look(true, r._tmp_16_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_del_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) yield_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.yield_expr_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) assert_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(532) && r.expression_rule() && (r._tmp_17_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) import_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_import_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.import_name_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.import_from_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) import_name_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(634) && r.dotted_as_names_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) import_from_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if s := new([6]int); r.at(&s[0]) && r.tok(633) && r.at(&s[1]) && r._loop0_18_rule() && r.at(&s[2]) && r.dotted_name_rule() && r.at(&s[3]) && r.tok(634) && r.at(&s[4]) && r.import_from_targets_rule() && r.at(&s[5]) {
			res = r.hFuture(s[0])
			break
		}
		r.pos = mark
		if r.tok(633) && r._loop1_19_rule() && r.tok(634) && r.import_from_targets_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) import_from_targets_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r.import_from_as_names_rule() && (r.tok(12) || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.import_from_as_names_rule() && r.lookTok(false, 12) {
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
			if r.invalid_import_from_targets_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) import_from_as_names_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_21_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) import_from_as_name_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_import_from_as_name_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(cNAME) && (r._tmp_22_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) dotted_as_names_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_24_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) dotted_as_name_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_dotted_as_name_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.dotted_name_rule() && (r._tmp_22_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) dotted_name_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(1); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 1, res, r.pos)
		r.pos = mark
		if !r.dotted_name_raw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) dotted_name_raw() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.dotted_name_rule() && r.tok(23) && r.tok(cNAME) {
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

func (r *rec) block_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(2); hit {
		r.level--
		return ok
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNEWLINE) && r.tok(cINDENT) && r.statements_rule() && r.tok(cDEDENT) {
			res = true
			break
		}
		r.pos = mark
		if r.simple_stmts_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_block_rule() {
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

func (r *rec) decorators_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._loop1_25_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) class_def_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.decorators_rule() && r.class_def_raw_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.class_def_raw_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) class_def_raw_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_class_def_raw_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(701) && r.tok(cNAME) && (r.type_params_rule() || true) && (r._tmp_26_rule() || true) && r.tok(11) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) function_def_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.decorators_rule() && r.function_def_raw_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.function_def_raw_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) function_def_raw_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_def_raw_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(699) && r.tok(cNAME) && (r.type_params_rule() || true) && r.tok(7) && (r.params_rule() || true) && r.tok(8) && (r._tmp_27_rule() || true) && r.tok(11) && (r.func_type_comment_rule() || true) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(698) && r.tok(699) && r.tok(cNAME) && (r.type_params_rule() || true) && r.tok(7) && (r.params_rule() || true) && r.tok(8) && (r._tmp_27_rule() || true) && r.tok(11) && (r.func_type_comment_rule() || true) && r.block_rule() {
			res = r.hVersion(5)
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) params_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_parameters_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.parameters_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) parameters_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.slash_no_default_rule() && r._loop0_28_rule() && r._loop0_29_rule() && (r.star_etc_rule() || true) {
			res = r.hVersion(8)
			break
		}
		r.pos = mark
		if r.slash_with_default_rule() && r._loop0_29_rule() && (r.star_etc_rule() || true) {
			res = r.hVersion(8)
			break
		}
		r.pos = mark
		if r._loop1_30_rule() && r._loop0_29_rule() && (r.star_etc_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r._loop1_31_rule() && (r.star_etc_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.star_etc_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) slash_no_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._loop1_30_rule() && r.tok(17) && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r._loop1_30_rule() && r.tok(17) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) slash_with_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._loop0_28_rule() && r._loop1_31_rule() && r.tok(17) && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r._loop0_28_rule() && r._loop1_31_rule() && r.tok(17) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) star_etc_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_star_etc_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(16) && r.param_no_default_rule() && r._loop0_32_rule() && (r.kwds_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) && r.param_no_default_star_annotation_rule() && r._loop0_32_rule() && (r.kwds_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) && r.tok(12) && r._loop1_33_rule() && (r.kwds_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.kwds_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) kwds_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_kwds_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(35) && r.param_no_default_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) param_no_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.param_rule() && r.tok(12) && (r.tok(cTYPECOMMENT) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.param_rule() && (r.tok(cTYPECOMMENT) || true) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) param_no_default_star_annotation_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.param_star_annotation_rule() && r.tok(12) && (r.tok(cTYPECOMMENT) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.param_star_annotation_rule() && (r.tok(cTYPECOMMENT) || true) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) param_with_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.param_rule() && r.default_rule() && r.tok(12) && (r.tok(cTYPECOMMENT) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.param_rule() && r.default_rule() && (r.tok(cTYPECOMMENT) || true) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) param_maybe_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.param_rule() && (r.default_rule() || true) && r.tok(12) && (r.tok(cTYPECOMMENT) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.param_rule() && (r.default_rule() || true) && (r.tok(cTYPECOMMENT) || true) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) param_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && (r.annotation_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) param_star_annotation_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.star_annotation_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) annotation_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) star_annotation_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) && r.star_expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_default_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) if_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_if_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(682) && r.named_expression_rule() && r.tok(11) && r.block_rule() && r.elif_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(682) && r.named_expression_rule() && r.tok(11) && r.block_rule() && (r.else_block_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) elif_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_elif_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(687) && r.named_expression_rule() && r.tok(11) && r.block_rule() && r.elif_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(687) && r.named_expression_rule() && r.tok(11) && r.block_rule() && (r.else_block_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) else_block_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_else_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(686) && r.forced(11) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) while_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_while_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(689) && r.named_expression_rule() && r.tok(11) && r.block_rule() && (r.else_block_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) for_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	cut := false
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_for_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(694) && r.star_targets_rule() && r.tok(695) && r.cut(&cut) && r.star_expressions_rule() && r.tok(11) && (r.tok(cTYPECOMMENT) || true) && r.block_rule() && (r.else_block_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
		}
		if r.tok(698) && r.tok(694) && r.star_targets_rule() && r.tok(695) && r.cut(&cut) && r.star_expressions_rule() && r.tok(11) && (r.tok(cTYPECOMMENT) || true) && r.block_rule() && (r.else_block_rule() || true) {
			res = r.hVersion(5)
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
		}
		if r.invalid {
			if r.invalid_for_target_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) with_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_with_stmt_indent_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(647) && r.tok(7) && r._gather_35_rule() && (r.tok(12) || true) && r.tok(8) && r.tok(11) && (r.tok(cTYPECOMMENT) || true) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(647) && r._gather_35_rule() && r.tok(11) && (r.tok(cTYPECOMMENT) || true) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(698) && r.tok(647) && r.tok(7) && r._gather_35_rule() && (r.tok(12) || true) && r.tok(8) && r.tok(11) && r.block_rule() {
			res = r.hVersion(5)
			break
		}
		r.pos = mark
		if r.tok(698) && r.tok(647) && r._gather_35_rule() && r.tok(11) && (r.tok(cTYPECOMMENT) || true) && r.block_rule() {
			res = r.hVersion(5)
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_with_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) with_item_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expression_rule() && r.tok(680) && r.star_target_rule() && r.look(true, r._tmp_36_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_with_item_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) try_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_try_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(656) && r.forced(11) && r.block_rule() && r.finally_block_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(656) && r.forced(11) && r.block_rule() && r._loop1_37_rule() && (r.else_block_rule() || true) && (r.finally_block_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(656) && r.forced(11) && r.block_rule() && r._loop1_38_rule() && (r.else_block_rule() || true) && (r.finally_block_rule() || true) {
			res = r.hVersion(11)
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) except_block_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_except_stmt_indent_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(677) && r.expression_rule() && r.tok(11) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(677) && r.expression_rule() && r.tok(680) && r.tok(cNAME) && r.tok(11) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(677) && r.expressions_rule() && r.tok(11) && r.block_rule() {
			res = r.hVersion(14)
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(11) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_except_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) except_star_block_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_except_star_stmt_indent_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(677) && r.tok(16) && r.expression_rule() && r.tok(11) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(16) && r.expression_rule() && r.tok(680) && r.tok(cNAME) && r.tok(11) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(16) && r.expressions_rule() && r.tok(11) && r.block_rule() {
			res = r.hVersion(14)
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_except_star_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) finally_block_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_finally_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(673) && r.forced(11) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) match_stmt_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.soft("match") && r.subject_expr_rule() && r.tok(11) && r.tok(cNEWLINE) && r.tok(cINDENT) && r._loop1_39_rule() && r.tok(cDEDENT) {
			res = r.hVersion(10)
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_match_stmt_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) subject_expr_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.star_named_expression_rule() && r.tok(12) && (r.star_named_expressions_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.named_expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) case_block_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_case_block_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.soft("case") && r.patterns_rule() && (r.guard_rule() || true) && r.tok(11) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) guard_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(682) && r.named_expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) patterns_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.open_sequence_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.pattern_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.as_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.or_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) as_pattern_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.or_pattern_rule() && r.tok(680) && r.pattern_capture_target_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_as_pattern_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) or_pattern_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r._gather_41_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) closed_pattern_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(3); hit {
		r.level--
		return ok
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.literal_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.capture_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.wildcard_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.value_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.group_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.sequence_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.mapping_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.class_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 3, res, r.pos)
	r.level--
	return res
}

func (r *rec) literal_pattern_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.signed_number_rule() && r.look(false, r._tmp_42_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.complex_number_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.strings_rule() {
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

func (r *rec) literal_expr_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.signed_number_rule() && r.look(false, r._tmp_42_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.complex_number_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.look(true, r._tmp_43_rule) && r.strings_rule() {
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

func (r *rec) complex_number_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.signed_real_number_rule() && r.tok(14) && r.imaginary_number_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.signed_real_number_rule() && r.tok(15) && r.imaginary_number_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) signed_number_rule() bool {
	r.enter()
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

func (r *rec) signed_real_number_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.real_number_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(15) && r.real_number_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) real_number_rule() bool {
	r.enter()
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

func (r *rec) imaginary_number_rule() bool {
	r.enter()
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

func (r *rec) capture_pattern_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.pattern_capture_target_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) pattern_capture_target_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lookSoft(false, "_") && r.tok(cNAME) && r.look(false, r._tmp_44_rule) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) wildcard_pattern_rule() bool {
	r.enter()
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

func (r *rec) value_pattern_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.attr_rule() && r.look(false, r._tmp_44_rule) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) attr_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(4); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 4, res, r.pos)
		r.pos = mark
		if !r.attr_raw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) attr_raw() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.name_or_attr_rule() && r.tok(23) && r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) name_or_attr_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.attr_rule() {
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

func (r *rec) group_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r.pattern_rule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) sequence_pattern_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(9) && (r.maybe_sequence_pattern_rule() || true) && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && (r.open_sequence_pattern_rule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) open_sequence_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.maybe_star_pattern_rule() && r.tok(12) && (r.maybe_sequence_pattern_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) maybe_sequence_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_46_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) maybe_star_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.star_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.pattern_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) star_pattern_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(5); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.pattern_capture_target_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) && r.wildcard_pattern_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 5, res, r.pos)
	r.level--
	return res
}

func (r *rec) mapping_pattern_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.double_star_pattern_rule() && (r.tok(12) || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.items_pattern_rule() && r.tok(12) && r.double_star_pattern_rule() && (r.tok(12) || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.items_pattern_rule() && (r.tok(12) || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) items_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_48_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) key_value_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_49_rule() && r.tok(11) && r.pattern_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) double_star_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(35) && r.pattern_capture_target_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) class_pattern_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.name_or_attr_rule() && r.tok(7) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.name_or_attr_rule() && r.tok(7) && r.positional_patterns_rule() && (r.tok(12) || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.name_or_attr_rule() && r.tok(7) && r.keyword_patterns_rule() && (r.tok(12) || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.name_or_attr_rule() && r.tok(7) && r.positional_patterns_rule() && r.tok(12) && r.keyword_patterns_rule() && (r.tok(12) || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_class_pattern_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) positional_patterns_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_51_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) keyword_patterns_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_53_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) keyword_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tok(22) && r.pattern_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) type_alias_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.soft("type") && r.tok(cNAME) && (r.type_params_rule() || true) && r.tok(22) && r.expression_rule() {
			res = r.hVersion(12)
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) type_params_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_type_params_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(9) && r.type_param_seq_rule() && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) type_param_seq_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_55_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) type_param_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(6); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && (r.type_param_bound_rule() || true) && (r.type_param_default_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_type_param_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(16) && r.tok(cNAME) && (r.type_param_starred_default_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(35) && r.tok(cNAME) && (r.type_param_default_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 6, res, r.pos)
	r.level--
	return res
}

func (r *rec) type_param_bound_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) type_param_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) && r.expression_rule() {
			res = r.hVersion(13)
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) type_param_starred_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) && r.star_expression_rule() {
			res = r.hVersion(13)
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) expressions_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.expression_rule() && r._loop1_56_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.expression_rule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) expression_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(7); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_expression_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.invalid {
			if r.invalid_legacy_expression_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.disjunction_rule() && r.tok(682) && r.disjunction_rule() && r.tok(686) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.disjunction_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lambdef_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 7, res, r.pos)
	r.level--
	return res
}

func (r *rec) yield_expr_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(587) && r.tok(633) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(587) && (r.star_expressions_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) star_expressions_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.star_expression_rule() && r._loop1_57_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.star_expression_rule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.star_expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) star_expression_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(8); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 8, res, r.pos)
	r.level--
	return res
}

func (r *rec) star_named_expressions_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_59_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) star_named_expression_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.named_expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) assignment_expression_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	cut := false
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tok(53) && r.cut(&cut) && r.expression_rule() {
			res = r.hVersion(8)
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

func (r *rec) named_expression_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.assignment_expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_named_expression_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.expression_rule() && r.lookTok(false, 53) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) disjunction_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(9); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.conjunction_rule() && r._loop1_60_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.conjunction_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 9, res, r.pos)
	r.level--
	return res
}

func (r *rec) conjunction_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(10); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.inversion_rule() && r._loop1_61_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.inversion_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 10, res, r.pos)
	r.level--
	return res
}

func (r *rec) inversion_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(11); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(703) && r.inversion_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.comparison_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 11, res, r.pos)
	r.level--
	return res
}

func (r *rec) comparison_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.bitwise_or_rule() && r._loop1_62_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) compare_op_bitwise_or_pair_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.eq_bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.noteq_bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lte_bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_noteq_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.lt_bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.gte_bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.gt_bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.notin_bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.in_bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.isnot_bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.is_bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) eq_bitwise_or_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(27) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) noteq_bitwise_or_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_63_rule() && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lte_bitwise_or_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(29) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lt_bitwise_or_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(20) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) gte_bitwise_or_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(30) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) gt_bitwise_or_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(21) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) notin_bitwise_or_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(703) && r.tok(695) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) in_bitwise_or_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(695) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) isnot_bitwise_or_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(596) && r.tok(703) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) is_bitwise_or_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(596) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) bitwise_or_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(12); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 12, res, r.pos)
		r.pos = mark
		if !r.bitwise_or_raw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) bitwise_or_raw() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.bitwise_or_rule() && r.tok(18) && r.bitwise_xor_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.bitwise_xor_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) bitwise_xor_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(13); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 13, res, r.pos)
		r.pos = mark
		if !r.bitwise_xor_raw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) bitwise_xor_raw() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.bitwise_xor_rule() && r.tok(32) && r.bitwise_and_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.bitwise_and_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) bitwise_and_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(14); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 14, res, r.pos)
		r.pos = mark
		if !r.bitwise_and_raw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) bitwise_and_raw() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.bitwise_and_rule() && r.tok(19) && r.shift_expr_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.shift_expr_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) shift_expr_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(15); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 15, res, r.pos)
		r.pos = mark
		if !r.shift_expr_raw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) shift_expr_raw() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.shift_expr_rule() && r.tok(33) && r.sum_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.shift_expr_rule() && r.tok(34) && r.sum_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_arithmetic_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.sum_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) sum_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(16); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 16, res, r.pos)
		r.pos = mark
		if !r.sum_raw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) sum_raw() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.sum_rule() && r.tok(14) && r.term_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.sum_rule() && r.tok(15) && r.term_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.term_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) term_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(17); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 17, res, r.pos)
		r.pos = mark
		if !r.term_raw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) term_raw() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.term_rule() && r.tok(16) && r.factor_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.term_rule() && r.tok(17) && r.factor_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.term_rule() && r.tok(47) && r.factor_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.term_rule() && r.tok(24) && r.factor_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.term_rule() && r.tok(49) && r.factor_rule() {
			res = r.hVersion(5)
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_factor_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.factor_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) factor_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(18); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(14) && r.factor_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(15) && r.factor_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(31) && r.factor_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.power_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 18, res, r.pos)
	r.level--
	return res
}

func (r *rec) power_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.await_primary_rule() && r.tok(35) && r.factor_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.await_primary_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) await_primary_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(19); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(597) && r.primary_rule() {
			res = r.hVersion(5)
			break
		}
		r.pos = mark
		if r.primary_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 19, res, r.pos)
	r.level--
	return res
}

func (r *rec) primary_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(20); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 20, res, r.pos)
		r.pos = mark
		if !r.primary_raw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) primary_raw() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.primary_rule() && r.tok(23) && r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
		if r.primary_rule() && r.genexp_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.primary_rule() && r.tok(7) && (r.arguments_rule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.primary_rule() && r.tok(9) && r.slices_rule() && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
		if r.atom_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) slices_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.slice_rule() && r.lookTok(false, 12) {
			res = true
			break
		}
		r.pos = mark
		if r._gather_65_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) slice_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if (r.expression_rule() || true) && r.tok(11) && (r.expression_rule() || true) && (r._tmp_66_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.named_expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) atom_rule() bool {
	r.enter()
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
		if r.look(true, r._tmp_43_rule) && r.strings_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.number() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 7) && r._tmp_67_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 9) && r._tmp_68_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lookTok(true, 25) && r._tmp_69_rule() {
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

func (r *rec) group_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r._tmp_70_rule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_group_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) lambdef_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(621) && (r.lambda_params_rule() || true) && r.tok(11) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambda_params_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_lambda_parameters_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.lambda_parameters_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambda_parameters_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambda_slash_no_default_rule() && r._loop0_71_rule() && r._loop0_72_rule() && (r.lambda_star_etc_rule() || true) {
			res = r.hVersion(8)
			break
		}
		r.pos = mark
		if r.lambda_slash_with_default_rule() && r._loop0_72_rule() && (r.lambda_star_etc_rule() || true) {
			res = r.hVersion(8)
			break
		}
		r.pos = mark
		if r._loop1_73_rule() && r._loop0_72_rule() && (r.lambda_star_etc_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r._loop1_74_rule() && (r.lambda_star_etc_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.lambda_star_etc_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambda_slash_no_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._loop1_73_rule() && r.tok(17) && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r._loop1_73_rule() && r.tok(17) && r.lookTok(true, 11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambda_slash_with_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._loop0_71_rule() && r._loop1_74_rule() && r.tok(17) && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r._loop0_71_rule() && r._loop1_74_rule() && r.tok(17) && r.lookTok(true, 11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambda_star_etc_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_lambda_star_etc_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(16) && r.lambda_param_no_default_rule() && r._loop0_75_rule() && (r.lambda_kwds_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) && r.tok(12) && r._loop1_76_rule() && (r.lambda_kwds_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.lambda_kwds_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambda_kwds_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_lambda_kwds_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(35) && r.lambda_param_no_default_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambda_param_no_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambda_param_rule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.lambda_param_rule() && r.lookTok(true, 11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambda_param_with_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambda_param_rule() && r.default_rule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.lambda_param_rule() && r.default_rule() && r.lookTok(true, 11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambda_param_maybe_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambda_param_rule() && (r.default_rule() || true) && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.lambda_param_rule() && (r.default_rule() || true) && r.lookTok(true, 11) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) lambda_param_rule() bool {
	r.enter()
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

func (r *rec) fstring_middle_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.fstring_replacement_field_rule() {
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

func (r *rec) fstring_replacement_field_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && (r.fstring_conversion_rule() || true) && (r.fstring_full_format_spec_rule() || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_fstring_replacement_field_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) fstring_conversion_rule() bool {
	r.enter()
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

func (r *rec) fstring_full_format_spec_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(11) && r._loop0_77_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) fstring_format_spec_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cFMIDDLE) {
			res = true
			break
		}
		r.pos = mark
		if r.fstring_replacement_field_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) fstring_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cFSTART) && r._loop0_78_rule() && r.tok(cFEND) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tstring_format_spec_replacement_field_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && (r.fstring_conversion_rule() || true) && (r.tstring_full_format_spec_rule() || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_tstring_replacement_field_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) tstring_format_spec_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cTMIDDLE) {
			res = true
			break
		}
		r.pos = mark
		if r.tstring_format_spec_replacement_field_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tstring_full_format_spec_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(11) && r._loop0_79_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tstring_replacement_field_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && (r.fstring_conversion_rule() || true) && (r.tstring_full_format_spec_rule() || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_tstring_replacement_field_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) tstring_middle_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tstring_replacement_field_rule() {
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

func (r *rec) tstring_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(21); hit {
		r.level--
		return ok
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cTSTART) && r._loop0_80_rule() && r.tok(cTEND) {
			res = r.hVersion(14)
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 21, res, r.pos)
	r.level--
	return res
}

func (r *rec) string_rule() bool {
	r.enter()
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

func (r *rec) strings_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(22); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_string_tstring_concat_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if s := new([2]int); r.at(&s[0]) && r._loop1_81_rule() && r.at(&s[1]) {
			res = r.hStrings(s[0])
			break
		}
		r.pos = mark
		if s := new([2]int); r.at(&s[0]) && r._loop1_82_rule() && r.at(&s[1]) {
			res = r.hStrings(s[0])
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 22, res, r.pos)
	r.level--
	return res
}

func (r *rec) list_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(9) && (r.star_named_expressions_rule() || true) && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tuple_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(7) && (r._tmp_83_rule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) set_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.star_named_expressions_rule() && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) dict_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && (r.double_starred_kvpairs_rule() || true) && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.invalid_double_starred_kvpairs_rule() && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) double_starred_kvpairs_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_85_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) double_starred_kvpair_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(35) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.kvpair_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) kvpair_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expression_rule() && r.tok(11) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) for_if_clauses_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._loop1_86_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) for_if_clause_rule() bool {
	r.enter()
	mark := r.pos
	cut := false
	res := false
	for once := true; once; once = false {
		if r.tok(698) && r.tok(694) && r.star_targets_rule() && r.tok(695) && r.cut(&cut) && r.disjunction_rule() && r._loop0_87_rule() {
			res = r.hVersion(6)
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
		}
		if r.tok(694) && r.star_targets_rule() && r.tok(695) && r.cut(&cut) && r.disjunction_rule() && r._loop0_87_rule() {
			res = true
			break
		}
		r.pos = mark
		if cut {
			r.level--
			return false
		}
		if r.invalid {
			if r.invalid_for_if_clause_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.invalid {
			if r.invalid_for_target_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) listcomp_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(9) && r.named_expression_rule() && r.for_if_clauses_rule() && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_comprehension_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) setcomp_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.named_expression_rule() && r.for_if_clauses_rule() && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_comprehension_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) genexp_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r._tmp_88_rule() && r.for_if_clauses_rule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_comprehension_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) dictcomp_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.kvpair_rule() && r.for_if_clauses_rule() && r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_dict_comprehension_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) arguments_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(23); hit {
		r.level--
		return ok
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.args_rule() && (r.tok(12) || true) && r.lookTok(true, 8) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_arguments_rule() {
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

func (r *rec) args_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r._gather_90_rule() && (r._tmp_91_rule() || true) {
			res = true
			break
		}
		r.pos = mark
		if r.kwargs_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) kwargs_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_93_rule() && r.tok(12) && r._gather_95_rule() {
			res = true
			break
		}
		r.pos = mark
		if r._gather_93_rule() {
			res = true
			break
		}
		r.pos = mark
		if r._gather_95_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) starred_expression_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_starred_expression_unpacking_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(16) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_starred_expression_rule() {
				res = true
				break
			}
			r.pos = mark
		}
	}
	r.level--
	return res
}

func (r *rec) kwarg_or_starred_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_kwarg_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(cNAME) && r.tok(22) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.starred_expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) kwarg_or_double_starred_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.invalid {
			if r.invalid_kwarg_rule() {
				res = true
				break
			}
			r.pos = mark
		}
		if r.tok(cNAME) && r.tok(22) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(35) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) star_targets_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.star_target_rule() && r.lookTok(false, 12) {
			res = true
			break
		}
		r.pos = mark
		if r.star_target_rule() && r._loop0_96_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) star_targets_list_seq_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_98_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) star_targets_tuple_seq_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.star_target_rule() && r._loop1_99_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
		if r.star_target_rule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) star_target_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(24); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r._tmp_100_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.target_with_star_atom_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 24, res, r.pos)
	r.level--
	return res
}

func (r *rec) target_with_star_atom_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(25); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.t_primary_rule() && r.tok(23) && r.tok(cNAME) && r.look(false, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.t_primary_rule() && r.tok(9) && r.slices_rule() && r.tok(10) && r.look(false, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.star_atom_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 25, res, r.pos)
	r.level--
	return res
}

func (r *rec) star_atom_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && r.target_with_star_atom_rule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && (r.star_targets_tuple_seq_rule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(9) && (r.star_targets_list_seq_rule() || true) && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) single_target_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.single_subscript_attribute_target_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && r.single_target_rule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) single_subscript_attribute_target_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.t_primary_rule() && r.tok(23) && r.tok(cNAME) && r.look(false, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.t_primary_rule() && r.tok(9) && r.slices_rule() && r.tok(10) && r.look(false, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) t_primary_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(26); hit {
		r.level--
		return ok
	}
	mark, resmark, res := r.pos, r.pos, false
	for {
		r.memoPut(mark, 26, res, r.pos)
		r.pos = mark
		if !r.t_primary_raw() || r.pos <= resmark {
			break
		}
		resmark, res = r.pos, true
	}
	r.pos = resmark
	r.level--
	return res
}

func (r *rec) t_primary_raw() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.t_primary_rule() && r.tok(23) && r.tok(cNAME) && r.look(true, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.t_primary_rule() && r.tok(9) && r.slices_rule() && r.tok(10) && r.look(true, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.t_primary_rule() && r.genexp_rule() && r.look(true, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.t_primary_rule() && r.tok(7) && (r.arguments_rule() || true) && r.tok(8) && r.look(true, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.atom_rule() && r.look(true, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) t_lookahead_rule() bool {
	r.enter()
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

func (r *rec) del_targets_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_102_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) del_target_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(27); hit {
		r.level--
		return ok
	}
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.t_primary_rule() && r.tok(23) && r.tok(cNAME) && r.look(false, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.t_primary_rule() && r.tok(9) && r.slices_rule() && r.tok(10) && r.look(false, r.t_lookahead_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.del_t_atom_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 27, res, r.pos)
	r.level--
	return res
}

func (r *rec) del_t_atom_rule() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && r.del_target_rule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && (r.del_targets_rule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(9) && (r.del_targets_rule() || true) && r.tok(10) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) type_expressions_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_104_rule() && r.tok(12) && r.tok(16) && r.expression_rule() && r.tok(12) && r.tok(35) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r._gather_104_rule() && r.tok(12) && r.tok(16) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r._gather_104_rule() && r.tok(12) && r.tok(35) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) && r.expression_rule() && r.tok(12) && r.tok(35) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(16) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(35) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r._gather_104_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) func_type_comment_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNEWLINE) && r.tok(cTYPECOMMENT) && r.look(true, r._tmp_105_rule) {
			res = true
			break
		}
		r.pos = mark
		if r.invalid {
			if r.invalid_double_type_comments_rule() {
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

func (r *rec) invalid_arguments_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_106_rule() && r.tok(12) && r._gather_108_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.expression_rule() && r.for_if_clauses_rule() && r.tok(12) && (r._tmp_109_rule() || true) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(cNAME) && r.tok(22) && r.expression_rule() && r.for_if_clauses_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r._tmp_110_rule() || true) && r.tok(cNAME) && r.tok(22) && r.look(true, r._tmp_111_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if s := new([3]int); r.at(&s[0]) && r.args_rule() && r.at(&s[1]) && r.for_if_clauses_rule() && r.at(&s[2]) {
			res = r.hGenexpCall(s[0], s[1])
			break
		}
		r.pos = mark
		if r.args_rule() && r.tok(12) && r.expression_rule() && r.for_if_clauses_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.args_rule() && r.tok(12) && r.args_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_kwarg_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_112_rule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(cNAME) && r.tok(22) && r.expression_rule() && r.for_if_clauses_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.look(false, r._tmp_113_rule) && r.expression_rule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.expression_rule() && r.tok(22) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) expression_without_invalid_rule() bool {
	prev := r.invalid
	r.invalid = false
	ok := r.expression_without_invalid_rule_body()
	r.invalid = prev
	return ok
}

func (r *rec) expression_without_invalid_rule_body() bool {
	r.enter()
	mark := r.pos
	r.fillCur()
	res := false
	for once := true; once; once = false {
		if r.disjunction_rule() && r.tok(682) && r.disjunction_rule() && r.tok(686) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.disjunction_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lambdef_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_legacy_expression_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([4]int); r.at(&s[0]) && r.tok(cNAME) && r.at(&s[1]) && r.lookTok(false, 7) && r.at(&s[2]) && r.star_expressions_rule() && r.at(&s[3]) {
			res = r.hLegacy(s[0], s[1])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_type_param_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.tok(cNAME) && r.tok(11) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.tok(cNAME) && r.tok(11) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_expression_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cSTRING) && r._loop1_114_rule() && r.tok(cSTRING) {
			res = r.raise()
			break
		}
		r.pos = mark
		if s := new([4]int); r.at(&s[0]) && r.look(false, r._tmp_115_rule) && r.at(&s[1]) && r.disjunction_rule() && r.at(&s[2]) && r.expression_without_invalid_rule() && r.at(&s[3]) {
			res = r.hForgotComma(s[1], s[2])
			break
		}
		r.pos = mark
		if r.disjunction_rule() && r.tok(682) && r.disjunction_rule() && r.look(false, r._tmp_116_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.disjunction_rule() && r.tok(682) && r.disjunction_rule() && r.tok(686) && r.look(false, r.expression_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r._tmp_117_rule() && r.tok(682) && r.disjunction_rule() && r.tok(686) && r.simple_stmt_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(621) && (r.lambda_params_rule() || true) && r.tok(11) && r.lookTok(true, cFMIDDLE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(621) && (r.lambda_params_rule() || true) && r.tok(11) && r.lookTok(true, cTMIDDLE) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_named_expression_rule() bool {
	r.enter()
	if ok, hit := r.memoGet(28); hit {
		r.level--
		return ok
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expression_rule() && r.tok(53) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(cNAME) && r.tok(22) && r.bitwise_or_rule() && r.look(false, r._tmp_118_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.look(false, r._tmp_119_rule) && r.bitwise_or_rule() && r.tok(22) && r.bitwise_or_rule() && r.look(false, r._tmp_118_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.memoPut(mark, 28, res, r.pos)
	r.level--
	return res
}

func (r *rec) invalid_assignment_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.invalid_ann_assign_target_rule() && r.tok(11) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.star_named_expression_rule() && r.tok(12) && r._loop0_120_rule() && r.tok(11) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.expression_rule() && r.tok(11) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if s := new([4]int); r.at(&s[0]) && r._loop0_121_rule() && r.at(&s[1]) && r.star_expressions_rule() && r.at(&s[2]) && r.tok(22) && r.at(&s[3]) {
			res = r.hInvalidTarget(starTargets, s[1], s[2])
			break
		}
		r.pos = mark
		if r._loop0_121_rule() && r.yield_expr_rule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.star_expressions_rule() && r.augassign_rule() && r.annotated_rhs_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_ann_assign_target_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.list_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tuple_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(7) && r.invalid_ann_assign_target_rule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_del_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([3]int); r.at(&s[0]) && r.tok(625) && r.at(&s[1]) && r.star_expressions_rule() && r.at(&s[2]) {
			res = r.hInvalidTarget(delTargets, s[1], s[2])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_block_rule() bool {
	r.enter()
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

func (r *rec) invalid_comprehension_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_122_rule() && r.starred_expression_rule() && r.for_if_clauses_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r._tmp_123_rule() && r.star_named_expression_rule() && r.tok(12) && r.star_named_expressions_rule() && r.for_if_clauses_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r._tmp_123_rule() && r.star_named_expression_rule() && r.tok(12) && r.for_if_clauses_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_dict_comprehension_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(25) && r.tok(35) && r.bitwise_or_rule() && r.for_if_clauses_rule() && r.tok(26) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_parameters_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(17) && r.tok(12) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r._tmp_124_rule() && r._loop0_32_rule() && r.tok(17) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.slash_no_default_rule() || true) && r._loop0_28_rule() && r.invalid_parameters_helper_rule() && r.param_no_default_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r._loop0_28_rule() && r.tok(7) && r._loop1_30_rule() && (r.tok(12) || true) && r.tok(8) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r._tmp_124_rule() || true) && r._loop0_32_rule() && r.tok(16) && r._tmp_125_rule() && r._loop0_32_rule() && r.tok(17) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r._loop1_33_rule() && r.tok(17) && r.tok(16) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_default_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) && r.look(true, r._tmp_126_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_star_etc_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r._tmp_127_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(16) && r.tok(12) && r.tok(cTYPECOMMENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(16) && r.param_rule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(16) && r._tmp_128_rule() && r._loop0_32_rule() && r.tok(16) && r._tmp_128_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_kwds_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(35) && r.param_rule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.param_rule() && r.tok(12) && r.param_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.param_rule() && r.tok(12) && r._tmp_129_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_parameters_helper_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.slash_with_default_rule() {
			res = true
			break
		}
		r.pos = mark
		if r._loop1_31_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_lambda_parameters_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(17) && r.tok(12) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r._tmp_130_rule() && r._loop0_75_rule() && r.tok(17) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.lambda_slash_no_default_rule() || true) && r._loop0_71_rule() && r.invalid_lambda_parameters_helper_rule() && r.lambda_param_no_default_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r._loop0_71_rule() && r.tok(7) && r._gather_132_rule() && (r.tok(12) || true) && r.tok(8) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r._tmp_130_rule() || true) && r._loop0_75_rule() && r.tok(16) && r._tmp_133_rule() && r._loop0_75_rule() && r.tok(17) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r._loop1_76_rule() && r.tok(17) && r.tok(16) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_lambda_parameters_helper_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambda_slash_with_default_rule() {
			res = true
			break
		}
		r.pos = mark
		if r._loop1_74_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_lambda_star_etc_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r._tmp_134_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(16) && r.lambda_param_rule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(16) && r._tmp_135_rule() && r._loop0_75_rule() && r.tok(16) && r._tmp_135_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_lambda_kwds_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(35) && r.lambda_param_rule() && r.tok(22) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.lambda_param_rule() && r.tok(12) && r.lambda_param_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(35) && r.lambda_param_rule() && r.tok(12) && r._tmp_129_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_double_type_comments_rule() bool {
	r.enter()
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

func (r *rec) invalid_with_item_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([5]int); r.at(&s[0]) && r.expression_rule() && r.at(&s[1]) && r.tok(680) && r.at(&s[2]) && r.expression_rule() && r.at(&s[3]) && r.look(true, r._tmp_36_rule) && r.at(&s[4]) {
			res = r.hInvalidTarget(starTargets, s[2], s[3])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_for_if_clause_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tok(698) || true) && r.tok(694) && r._tmp_136_rule() && r.lookTok(false, 695) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_for_target_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if s := new([4]int); r.at(&s[0]) && (r.tok(698) || true) && r.at(&s[1]) && r.tok(694) && r.at(&s[2]) && r.star_expressions_rule() && r.at(&s[3]) {
			res = r.hInvalidTarget(forTargets, s[2], s[3])
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_group_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r.starred_expression_rule() && r.tok(8) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(7) && r.tok(35) && r.expression_rule() && r.tok(8) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_import_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(634) && r._gather_138_rule() && r.tok(633) && r.dotted_name_rule() {
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

func (r *rec) invalid_dotted_as_name_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.dotted_name_rule() && r.tok(680) && r.look(false, r._tmp_139_rule) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_import_from_as_name_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tok(680) && r.look(false, r._tmp_139_rule) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_import_from_targets_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.import_from_as_names_rule() && r.tok(12) && r.tok(cNEWLINE) {
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

func (r *rec) invalid_with_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tok(698) || true) && r.tok(647) && r._gather_141_rule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tok(698) || true) && r.tok(647) && r.tok(7) && r._gather_143_rule() && (r.tok(12) || true) && r.tok(8) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_with_stmt_indent_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tok(698) || true) && r.tok(647) && r._gather_141_rule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tok(698) || true) && r.tok(647) && r.tok(7) && r._gather_143_rule() && (r.tok(12) || true) && r.tok(8) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_try_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(656) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(656) && r.tok(11) && r.block_rule() && r.look(false, r._tmp_144_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(656) && r.tok(11) && r._loop0_145_rule() && r._loop1_37_rule() && r.tok(677) && r.tok(16) && r.expression_rule() && (r._tmp_22_rule() || true) && r.tok(11) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(656) && r.tok(11) && r._loop0_145_rule() && r._loop1_38_rule() && r.tok(677) && (r._tmp_146_rule() || true) && r.tok(11) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_except_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(677) && r.expression_rule() && r.tok(12) && r.expressions_rule() && r.tok(680) && r.tok(cNAME) && r.tok(11) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.expression_rule() && (r._tmp_22_rule() || true) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.expression_rule() && r.tok(680) && r.expression_rule() && r.tok(11) && r.block_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_except_star_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(677) && r.tok(16) && r.expression_rule() && r.tok(12) && r.expressions_rule() && r.tok(680) && r.tok(cNAME) && r.tok(11) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(16) && r.expression_rule() && (r._tmp_22_rule() || true) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(16) && r._tmp_147_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(677) && r.tok(16) && r.expression_rule() && r.tok(680) && r.expression_rule() && r.tok(11) && r.block_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_finally_stmt_rule() bool {
	r.enter()
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

func (r *rec) invalid_except_stmt_indent_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(677) && r.expression_rule() && (r._tmp_22_rule() || true) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
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

func (r *rec) invalid_except_star_stmt_indent_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(677) && r.tok(16) && r.expression_rule() && (r._tmp_22_rule() || true) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_match_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.soft("match") && r.subject_expr_rule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.soft("match") && r.subject_expr_rule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_case_block_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.soft("case") && r.patterns_rule() && (r.guard_rule() || true) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.soft("case") && r.patterns_rule() && (r.guard_rule() || true) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_as_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.or_pattern_rule() && r.tok(680) && r.soft("_") {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.or_pattern_rule() && r.tok(680) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_class_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.name_or_attr_rule() && r.tok(7) && r.invalid_class_argument_pattern_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_class_argument_pattern_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r._tmp_148_rule() || true) && r.keyword_patterns_rule() && r.tok(12) && r.positional_patterns_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_if_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(682) && r.named_expression_rule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(682) && r.named_expression_rule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_elif_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(687) && r.named_expression_rule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(687) && r.named_expression_rule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_else_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(686) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(686) && r.tok(11) && r.block_rule() && r.tok(687) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_while_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(689) && r.named_expression_rule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(689) && r.named_expression_rule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_for_stmt_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tok(698) || true) && r.tok(694) && r.star_targets_rule() && r.tok(695) && r.star_expressions_rule() && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tok(698) || true) && r.tok(694) && r.star_targets_rule() && r.tok(695) && r.star_expressions_rule() && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_def_raw_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if (r.tok(698) || true) && r.tok(699) && r.tok(cNAME) && (r.type_params_rule() || true) && r.tok(7) && (r.params_rule() || true) && r.tok(8) && (r._tmp_27_rule() || true) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
		if (r.tok(698) || true) && r.tok(699) && r.tok(cNAME) && (r.type_params_rule() || true) && r.forced(7) && (r.params_rule() || true) && r.tok(8) && (r._tmp_27_rule() || true) && r.forced(11) && (r.func_type_comment_rule() || true) && r.block_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_class_def_raw_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(701) && r.tok(cNAME) && (r.type_params_rule() || true) && (r._tmp_26_rule() || true) && r.tok(cNEWLINE) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(701) && r.tok(cNAME) && (r.type_params_rule() || true) && (r._tmp_26_rule() || true) && r.tok(11) && r.tok(cNEWLINE) && r.lookTok(false, cINDENT) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_double_starred_kvpairs_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_85_rule() && r.tok(12) && r.invalid_kvpair_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.expression_rule() && r.tok(11) && r.tok(16) && r.bitwise_or_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.expression_rule() && r.tok(11) && r.look(true, r._tmp_149_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_kvpair_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expression_rule() && r.lookTok(false, 11) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.expression_rule() && r.tok(11) && r.tok(16) && r.bitwise_or_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.expression_rule() && r.tok(11) && r.look(true, r._tmp_149_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_starred_expression_unpacking_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(16) && r.expression_rule() && r.tok(22) && r.expression_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_starred_expression_rule() bool {
	r.enter()
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

func (r *rec) invalid_fstring_replacement_field_rule() bool {
	r.enter()
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
		if r.tok(25) && r.look(false, r.annotated_rhs_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && r.look(false, r._tmp_150_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && r.tok(22) && r.look(false, r._tmp_151_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && r.invalid_fstring_conversion_character_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && (r._tmp_152_rule() || true) && r.look(false, r._tmp_153_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && (r._tmp_152_rule() || true) && r.tok(11) && r._loop0_77_rule() && r.lookTok(false, 26) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && (r._tmp_152_rule() || true) && r.lookTok(false, 26) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_fstring_conversion_character_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(54) && r.look(true, r._tmp_153_rule) {
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

func (r *rec) invalid_tstring_replacement_field_rule() bool {
	r.enter()
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
		if r.tok(25) && r.look(false, r.annotated_rhs_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && r.look(false, r._tmp_150_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && r.tok(22) && r.look(false, r._tmp_151_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && r.invalid_tstring_conversion_character_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && (r._tmp_152_rule() || true) && r.look(false, r._tmp_153_rule) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && (r._tmp_152_rule() || true) && r.tok(11) && r._loop0_77_rule() && r.lookTok(false, 26) {
			res = r.raise()
			break
		}
		r.pos = mark
		if r.tok(25) && r.annotated_rhs_rule() && (r.tok(22) || true) && (r._tmp_152_rule() || true) && r.lookTok(false, 26) {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_tstring_conversion_character_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(54) && r.look(true, r._tmp_153_rule) {
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

func (r *rec) invalid_string_tstring_concat_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._loop1_81_rule() && r.tstring_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
		if r._loop1_82_rule() && r._tmp_154_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_arithmetic_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.sum_rule() && r._tmp_155_rule() && r.tok(703) && r.inversion_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_factor_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_156_rule() && r.tok(703) && r.factor_rule() {
			res = r.raise()
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) invalid_type_params_rule() bool {
	r.enter()
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

func (r *rec) invalid_noteq_rule() bool {
	r.enter()
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

func (r *rec) _loop0_1_rule() bool {
	r.enter()
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

func (r *rec) _loop1_2_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.statement_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop0_3_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(13) && r.simple_stmt_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_4_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.simple_stmt_rule() && r._loop0_3_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_5_rule() bool {
	r.enter()
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

func (r *rec) _tmp_6_rule() bool {
	r.enter()
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

func (r *rec) _tmp_7_rule() bool {
	r.enter()
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

func (r *rec) _tmp_8_rule() bool {
	r.enter()
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

func (r *rec) _tmp_9_rule() bool {
	r.enter()
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

func (r *rec) _tmp_10_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) && r.annotated_rhs_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_11_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && r.single_target_rule() && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.single_subscript_attribute_target_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop1_12_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_157_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _tmp_13_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(633) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_14_rule() bool {
	r.enter()
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

func (r *rec) _gather_15_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r._loop0_14_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_16_rule() bool {
	r.enter()
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

func (r *rec) _tmp_17_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_18_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_158_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop1_19_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_158_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop0_20_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.import_from_as_name_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_21_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.import_from_as_name_rule() && r._loop0_20_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_22_rule() bool {
	r.enter()
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

func (r *rec) _loop0_23_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.dotted_as_name_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_24_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.dotted_as_name_rule() && r._loop0_23_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop1_25_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_159_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _tmp_26_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(7) && (r.arguments_rule() || true) && r.tok(8) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_27_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(51) && r.expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_28_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.param_no_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop0_29_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.param_with_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop1_30_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.param_no_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop1_31_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.param_with_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop0_32_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.param_maybe_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop1_33_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.param_maybe_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop0_34_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.with_item_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_35_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.with_item_rule() && r._loop0_34_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_36_rule() bool {
	r.enter()
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

func (r *rec) _loop1_37_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.except_block_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop1_38_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.except_star_block_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop1_39_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.case_block_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop0_40_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(18) && r.closed_pattern_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_41_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.closed_pattern_rule() && r._loop0_40_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_42_rule() bool {
	r.enter()
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

func (r *rec) _tmp_43_rule() bool {
	r.enter()
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

func (r *rec) _tmp_44_rule() bool {
	r.enter()
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

func (r *rec) _loop0_45_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.maybe_star_pattern_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_46_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.maybe_star_pattern_rule() && r._loop0_45_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_47_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.key_value_pattern_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_48_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.key_value_pattern_rule() && r._loop0_47_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_49_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.literal_expr_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.attr_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_50_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.pattern_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_51_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.pattern_rule() && r._loop0_50_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_52_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.keyword_pattern_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_53_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.keyword_pattern_rule() && r._loop0_52_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_54_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.type_param_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_55_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.type_param_rule() && r._loop0_54_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop1_56_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_17_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop1_57_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_160_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop0_58_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.star_named_expression_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_59_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.star_named_expression_rule() && r._loop0_58_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop1_60_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_161_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop1_61_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_162_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop1_62_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.compare_op_bitwise_or_pair_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _tmp_63_rule() bool {
	r.enter()
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

func (r *rec) _loop0_64_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r._tmp_163_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_65_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_163_rule() && r._loop0_64_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_66_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) && (r.expression_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_67_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tuple_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.group_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.genexp_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_68_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.list_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.listcomp_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_69_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.dict_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.set_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.dictcomp_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.setcomp_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_70_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.yield_expr_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.named_expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_71_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.lambda_param_no_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop0_72_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.lambda_param_with_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop1_73_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.lambda_param_no_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop1_74_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.lambda_param_with_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop0_75_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.lambda_param_maybe_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop1_76_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.lambda_param_maybe_default_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop0_77_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.fstring_format_spec_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop0_78_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.fstring_middle_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop0_79_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tstring_format_spec_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop0_80_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tstring_middle_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop1_81_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_154_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop1_82_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tstring_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _tmp_83_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.star_named_expression_rule() && r.tok(12) && (r.star_named_expressions_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_84_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.double_starred_kvpair_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_85_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.double_starred_kvpair_rule() && r._loop0_84_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop1_86_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.for_if_clause_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _loop0_87_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_164_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _tmp_88_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.assignment_expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.expression_rule() && r.lookTok(false, 53) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_89_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r._tmp_165_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_90_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_165_rule() && r._loop0_89_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_91_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) && r.kwargs_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_92_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.kwarg_or_starred_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_93_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.kwarg_or_starred_rule() && r._loop0_92_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_94_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.kwarg_or_double_starred_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_95_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.kwarg_or_double_starred_rule() && r._loop0_94_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_96_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_166_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop0_97_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.star_target_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_98_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.star_target_rule() && r._loop0_97_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop1_99_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_166_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _tmp_100_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lookTok(false, 16) && r.star_target_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_101_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.del_target_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_102_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.del_target_rule() && r._loop0_101_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_103_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.expression_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_104_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expression_rule() && r._loop0_103_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_105_rule() bool {
	r.enter()
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

func (r *rec) _tmp_106_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_167_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.kwargs_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_107_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r._tmp_168_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_108_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_168_rule() && r._loop0_107_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_109_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.args_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.expression_rule() && r.for_if_clauses_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_110_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.args_rule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_111_rule() bool {
	r.enter()
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

func (r *rec) _tmp_112_rule() bool {
	r.enter()
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

func (r *rec) _tmp_113_rule() bool {
	r.enter()
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

func (r *rec) _loop1_114_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_169_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return n > 0
}

func (r *rec) _tmp_115_rule() bool {
	r.enter()
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

func (r *rec) _tmp_116_rule() bool {
	r.enter()
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

func (r *rec) _tmp_117_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.pass_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.break_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.continue_stmt_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_118_rule() bool {
	r.enter()
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

func (r *rec) _tmp_119_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.list_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tuple_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.genexp_rule() {
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

func (r *rec) _loop0_120_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.star_named_expressions_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _loop0_121_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_157_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _tmp_122_rule() bool {
	r.enter()
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

func (r *rec) _tmp_123_rule() bool {
	r.enter()
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

func (r *rec) _tmp_124_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.slash_no_default_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.slash_with_default_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_125_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.param_no_default_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_126_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(12) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_127_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(12) && r._tmp_170_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_128_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.param_no_default_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(12) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_129_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(16) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(35) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(17) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_130_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambda_slash_no_default_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.lambda_slash_with_default_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_131_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.lambda_param_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_132_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambda_param_rule() && r._loop0_131_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_133_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) {
			res = true
			break
		}
		r.pos = mark
		if r.lambda_param_no_default_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_134_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(12) && r._tmp_171_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_135_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambda_param_no_default_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.tok(12) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_136_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.bitwise_or_rule() && r._loop0_172_rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_137_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r.dotted_name_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_138_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.dotted_name_rule() && r._loop0_137_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_139_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r._tmp_173_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_140_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r._tmp_174_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_141_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_174_rule() && r._loop0_140_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_142_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.tok(12) && r._tmp_175_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _gather_143_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._tmp_175_rule() && r._loop0_142_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_144_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(677) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(673) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_145_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r.block_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _tmp_146_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expression_rule() && (r._tmp_22_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_147_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNEWLINE) {
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

func (r *rec) _tmp_148_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.positional_patterns_rule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_149_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(26) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(12) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_150_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(22) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(54) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(11) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(26) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_151_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(54) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(11) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(26) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_152_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(54) && r.tok(cNAME) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_153_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(26) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_154_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.fstring_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.string_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_155_rule() bool {
	r.enter()
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
		if r.tok(16) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(17) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(24) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(47) {
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

func (r *rec) _tmp_156_rule() bool {
	r.enter()
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
		if r.tok(31) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_157_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.star_targets_rule() && r.tok(22) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_158_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(23) {
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

func (r *rec) _tmp_159_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(49) && r.named_expression_rule() && r.tok(cNEWLINE) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_160_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) && r.star_expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_161_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(588) && r.conjunction_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_162_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(589) && r.inversion_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_163_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.slice_rule() {
			res = true
			break
		}
		r.pos = mark
		if r.starred_expression_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_164_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(682) && r.disjunction_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_165_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.starred_expression_rule() {
			res = true
			break
		}
		r.pos = mark
		if r._tmp_88_rule() && r.lookTok(false, 22) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_166_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) && r.star_target_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_167_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r._gather_90_rule() && r.tok(12) && r.kwargs_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_168_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.starred_expression_rule() && r.lookTok(false, 22) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_169_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.look(false, r.stringTok) && r.expression_without_invalid_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_170_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(35) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_171_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(35) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _loop0_172_rule() bool {
	r.enter()
	mark := r.pos
	n := 0
	for r._tmp_176_rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) _tmp_173_rule() bool {
	r.enter()
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

func (r *rec) _tmp_174_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expression_rule() && (r._tmp_177_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_175_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expressions_rule() && (r._tmp_177_rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_176_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) && r.bitwise_or_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) _tmp_177_rule() bool {
	r.enter()
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(680) && r.star_target_rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}
