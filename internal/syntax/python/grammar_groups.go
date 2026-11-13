package python

func (r *rec) tmp126Rule() bool {
	if !r.enter() {
		return false
	}
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

func (r *rec) tmp127Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(8) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(12) && r.tmp170Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp128Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.paramNoDefaultRule() {
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

func (r *rec) tmp129Rule() bool {
	if !r.enter() {
		return false
	}
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

func (r *rec) tmp130Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambdaSlashNoDefaultRule() {
			res = true
			break
		}
		r.pos = mark
		if r.lambdaSlashWithDefaultRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny131Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.lambdaParamRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather132Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambdaParamRule() && r.loopAny131Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp133Rule() bool {
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
		if r.lambdaParamNoDefaultRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp134Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(11) {
			res = true
			break
		}
		r.pos = mark
		if r.tok(12) && r.tmp171Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp135Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.lambdaParamNoDefaultRule() {
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

func (r *rec) tmp136Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.bitwiseOrRule() && r.loopAny172Rule() && (r.tok(12) || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny137Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.dottedNameRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather138Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.dottedNameRule() && r.loopAny137Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp139Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(cNAME) && r.tmp173Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny140Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.tmp174Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather141Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp174Rule() && r.loopAny140Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) loopAny142Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tok(12) && r.tmp175Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) gather143Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tmp175Rule() && r.loopAny142Rule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp144Rule() bool {
	if !r.enter() {
		return false
	}
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

func (r *rec) loopAny145Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.blockRule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) tmp146Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expressionRule() && (r.tmp22Rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp147Rule() bool {
	if !r.enter() {
		return false
	}
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

func (r *rec) tmp148Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.positionalPatternsRule() && r.tok(12) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp149Rule() bool {
	if !r.enter() {
		return false
	}
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

func (r *rec) tmp150Rule() bool {
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

func (r *rec) tmp151Rule() bool {
	if !r.enter() {
		return false
	}
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

func (r *rec) tmp152Rule() bool {
	if !r.enter() {
		return false
	}
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

func (r *rec) tmp153Rule() bool {
	if !r.enter() {
		return false
	}
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

func (r *rec) tmp154Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.fstringRule() {
			res = true
			break
		}
		r.pos = mark
		if r.stringRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp155Rule() bool {
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

func (r *rec) tmp156Rule() bool {
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
		if r.tok(31) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp157Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.starTargetsRule() && r.tok(22) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp158Rule() bool {
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
		if r.tok(52) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp159Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(49) && r.namedExpressionRule() && r.tok(cNEWLINE) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp160Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) && r.starExpressionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp161Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(588) && r.conjunctionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp162Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(589) && r.inversionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp163Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.sliceRule() {
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

func (r *rec) tmp164Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(682) && r.disjunctionRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp165Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.starredExpressionRule() {
			res = true
			break
		}
		r.pos = mark
		if r.tmp88Rule() && r.lookTok(false, 22) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp166Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) && r.starTargetRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp167Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.gather90Rule() && r.tok(12) && r.kwargsRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp168Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.starredExpressionRule() && r.lookTok(false, 22) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp169Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.look(false, r.stringTok) && r.expressionWithoutInvalidRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp170Rule() bool {
	if !r.enter() {
		return false
	}
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

func (r *rec) tmp171Rule() bool {
	if !r.enter() {
		return false
	}
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

func (r *rec) loopAny172Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	n := 0
	for r.tmp176Rule() {
		mark = r.pos
		n++
	}
	r.pos = mark
	r.level--
	return true
}

func (r *rec) tmp173Rule() bool {
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

func (r *rec) tmp174Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expressionRule() && (r.tmp177Rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp175Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.expressionsRule() && (r.tmp177Rule() || true) {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp176Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(12) && r.bitwiseOrRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}

func (r *rec) tmp177Rule() bool {
	if !r.enter() {
		return false
	}
	mark := r.pos
	res := false
	for once := true; once; once = false {
		if r.tok(680) && r.starTargetRule() {
			res = true
			break
		}
		r.pos = mark
	}
	r.level--
	return res
}
