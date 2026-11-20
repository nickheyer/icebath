package pyjsonschema

// CPython 3.14 list.sort, its exact compare sequence kept so a TypeError and NaN placement match

const (
	minGallop = 7
	maxMinrun = 64
)

type sortRun struct {
	base, len, power int
}

type mergeState struct {
	minGallop int
	keys      []any
	pending   []sortRun
}

// Sorts keys in place with Python <, false when a comparison raised a TypeError
func listSort(keys []any) (sorted bool) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*Exception); ok && e.Type == "TypeError" {
				sorted = false
				return
			}
			panic(r)
		}
	}()
	n := len(keys)
	if n < 2 {
		return true
	}
	ms := &mergeState{minGallop: minGallop, keys: keys}
	minrun := computeMinrun(n)
	lo, remaining := 0, n
	for remaining > 0 {
		r := ms.countRun(lo, remaining)
		if r < minrun {
			force := min(remaining, minrun)
			binarysort(keys[lo:lo+force], r)
			r = force
		}
		ms.foundNewRun(r)
		ms.pending = append(ms.pending, sortRun{base: lo, len: r})
		lo += r
		remaining -= r
	}
	ms.forceCollapse()
	return true
}

func computeMinrun(n int) int {
	r := 0
	for n >= maxMinrun {
		r |= n & 1
		n >>= 1
	}
	return n + r
}

func reverseSlice(a []any) {
	for i, j := 0, len(a)-1; i < j; i, j = i+1, j-1 {
		a[i], a[j] = a[j], a[i]
	}
}

// Stable binary insertion sort of a with its first ok items already sorted
func binarysort(a []any, ok int) {
	if ok == 0 {
		ok = 1
	}
	for ; ok < len(a); ok++ {
		l, r := 0, ok
		pivot := a[ok]
		for l < r {
			m := (l + r) >> 1
			if pyLess(pivot, a[m]) {
				r = m
			} else {
				l = m + 1
			}
		}
		copy(a[l+1:ok+1], a[l:ok])
		a[l] = pivot
	}
}

// Length of the run at lo made ascending in place, all equal descending stretches kept stable
func (ms *mergeState) countRun(lo, remaining int) int {
	a := ms.keys[lo:]
	n := 1
	for ; n < remaining; n++ {
		if pyLess(a[n], a[n-1]) {
			break
		}
	}
	if n == remaining {
		return n
	}
	if n > 1 {
		if pyLess(a[0], a[n-1]) {
			return n
		}
		reverseSlice(a[:n])
	}
	n++
	neq := 0
	reverseLastNeq := func() {
		if neq != 0 {
			neq++
			reverseSlice(a[n-neq : n])
			neq = 0
		}
	}
	for ; n < remaining; n++ {
		if pyLess(a[n], a[n-1]) {
			reverseLastNeq()
		} else {
			if pyLess(a[n-1], a[n]) {
				break
			}
			neq++
		}
	}
	reverseLastNeq()
	reverseSlice(a[:n])
	for ; n < remaining; n++ {
		if pyLess(a[n], a[n-1]) {
			break
		}
	}
	return n
}

// Position left of the leftmost item of a equal to key
func gallopLeft(key any, a []any, hint int) int {
	n := len(a)
	lastofs, ofs := 0, 1
	if pyLess(a[hint], key) {
		maxofs := n - hint
		for ofs < maxofs {
			if pyLess(a[hint+ofs], key) {
				lastofs = ofs
				ofs = (ofs << 1) + 1
			} else {
				break
			}
		}
		if ofs > maxofs {
			ofs = maxofs
		}
		lastofs += hint
		ofs += hint
	} else {
		maxofs := hint + 1
		for ofs < maxofs {
			if pyLess(a[hint-ofs], key) {
				break
			}
			lastofs = ofs
			ofs = (ofs << 1) + 1
		}
		if ofs > maxofs {
			ofs = maxofs
		}
		k := lastofs
		lastofs = hint - ofs
		ofs = hint - k
	}
	lastofs++
	for lastofs < ofs {
		m := lastofs + ((ofs - lastofs) >> 1)
		if pyLess(a[m], key) {
			lastofs = m + 1
		} else {
			ofs = m
		}
	}
	return ofs
}

// Position right of the rightmost item of a equal to key
func gallopRight(key any, a []any, hint int) int {
	n := len(a)
	lastofs, ofs := 0, 1
	if pyLess(key, a[hint]) {
		maxofs := hint + 1
		for ofs < maxofs {
			if pyLess(key, a[hint-ofs]) {
				lastofs = ofs
				ofs = (ofs << 1) + 1
			} else {
				break
			}
		}
		if ofs > maxofs {
			ofs = maxofs
		}
		k := lastofs
		lastofs = hint - ofs
		ofs = hint - k
	} else {
		maxofs := n - hint
		for ofs < maxofs {
			if pyLess(key, a[hint+ofs]) {
				break
			}
			lastofs = ofs
			ofs = (ofs << 1) + 1
		}
		if ofs > maxofs {
			ofs = maxofs
		}
		lastofs += hint
		ofs += hint
	}
	lastofs++
	for lastofs < ofs {
		m := lastofs + ((ofs - lastofs) >> 1)
		if pyLess(key, a[m]) {
			ofs = m
		} else {
			lastofs = m + 1
		}
	}
	return ofs
}

// Merges the run at ssa of length na with the following run at ssb, na not above nb
func (ms *mergeState) mergeLo(ssa, na, ssb, nb int) {
	keys := ms.keys
	tmp := append([]any(nil), keys[ssa:ssa+na]...)
	dest, pa, pb := ssa, 0, ssb
	keys[dest] = keys[pb]
	dest, pb, nb = dest+1, pb+1, nb-1
	if nb == 0 {
		copy(keys[dest:dest+na], tmp[pa:pa+na])
		return
	}
	if na == 1 {
		copy(keys[dest:dest+nb], keys[pb:pb+nb])
		keys[dest+nb] = tmp[pa]
		return
	}
	mg := ms.minGallop
	for {
		acount, bcount := 0, 0
		for {
			if pyLess(keys[pb], tmp[pa]) {
				keys[dest] = keys[pb]
				dest, pb = dest+1, pb+1
				bcount++
				acount = 0
				nb--
				if nb == 0 {
					copy(keys[dest:dest+na], tmp[pa:pa+na])
					return
				}
				if bcount >= mg {
					break
				}
			} else {
				keys[dest] = tmp[pa]
				dest, pa = dest+1, pa+1
				acount++
				bcount = 0
				na--
				if na == 1 {
					copy(keys[dest:dest+nb], keys[pb:pb+nb])
					keys[dest+nb] = tmp[pa]
					return
				}
				if acount >= mg {
					break
				}
			}
		}
		mg++
		for {
			if mg > 1 {
				mg--
			}
			ms.minGallop = mg
			k := gallopRight(keys[pb], tmp[pa:pa+na], 0)
			acount = k
			if k > 0 {
				copy(keys[dest:dest+k], tmp[pa:pa+k])
				dest, pa, na = dest+k, pa+k, na-k
				if na == 1 {
					copy(keys[dest:dest+nb], keys[pb:pb+nb])
					keys[dest+nb] = tmp[pa]
					return
				}
				if na == 0 {
					return
				}
			}
			keys[dest] = keys[pb]
			dest, pb, nb = dest+1, pb+1, nb-1
			if nb == 0 {
				copy(keys[dest:dest+na], tmp[pa:pa+na])
				return
			}
			k = gallopLeft(tmp[pa], keys[pb:pb+nb], 0)
			bcount = k
			if k > 0 {
				copy(keys[dest:dest+k], keys[pb:pb+k])
				dest, pb, nb = dest+k, pb+k, nb-k
				if nb == 0 {
					copy(keys[dest:dest+na], tmp[pa:pa+na])
					return
				}
			}
			keys[dest] = tmp[pa]
			dest, pa, na = dest+1, pa+1, na-1
			if na == 1 {
				copy(keys[dest:dest+nb], keys[pb:pb+nb])
				keys[dest+nb] = tmp[pa]
				return
			}
			if acount < minGallop && bcount < minGallop {
				break
			}
		}
		mg++
		ms.minGallop = mg
	}
}

// Merges the run at ssa of length na with the following run at ssb, na above nb
func (ms *mergeState) mergeHi(ssa, na, ssb, nb int) {
	keys := ms.keys
	tmp := append([]any(nil), keys[ssb:ssb+nb]...)
	basea := ssa
	dest, pa, pb := ssb+nb-1, ssa+na-1, nb-1
	keys[dest] = keys[pa]
	dest, pa, na = dest-1, pa-1, na-1
	copyA := func() {
		copy(keys[dest+1-na:dest+1], keys[pa+1-na:pa+1])
		dest, pa = dest-na, pa-na
		keys[dest] = tmp[pb]
	}
	finish := func() {
		if nb > 0 {
			copy(keys[dest-(nb-1):dest+1], tmp[0:nb])
		}
	}
	if na == 0 {
		finish()
		return
	}
	if nb == 1 {
		copyA()
		return
	}
	mg := ms.minGallop
	for {
		acount, bcount := 0, 0
		for {
			if pyLess(tmp[pb], keys[pa]) {
				keys[dest] = keys[pa]
				dest, pa = dest-1, pa-1
				acount++
				bcount = 0
				na--
				if na == 0 {
					finish()
					return
				}
				if acount >= mg {
					break
				}
			} else {
				keys[dest] = tmp[pb]
				dest, pb = dest-1, pb-1
				bcount++
				acount = 0
				nb--
				if nb == 1 {
					copyA()
					return
				}
				if bcount >= mg {
					break
				}
			}
		}
		mg++
		for {
			if mg > 1 {
				mg--
			}
			ms.minGallop = mg
			k := gallopRight(tmp[pb], keys[basea:basea+na], na-1)
			k = na - k
			acount = k
			if k > 0 {
				dest, pa = dest-k, pa-k
				copy(keys[dest+1:dest+1+k], keys[pa+1:pa+1+k])
				na -= k
				if na == 0 {
					finish()
					return
				}
			}
			keys[dest] = tmp[pb]
			dest, pb, nb = dest-1, pb-1, nb-1
			if nb == 1 {
				copyA()
				return
			}
			k = gallopLeft(keys[pa], tmp[0:nb], nb-1)
			k = nb - k
			bcount = k
			if k > 0 {
				dest, pb = dest-k, pb-k
				copy(keys[dest+1:dest+1+k], tmp[pb+1:pb+1+k])
				nb -= k
				if nb == 1 {
					copyA()
					return
				}
				if nb == 0 {
					finish()
					return
				}
			}
			keys[dest] = keys[pa]
			dest, pa, na = dest-1, pa-1, na-1
			if na == 0 {
				finish()
				return
			}
			if acount < minGallop && bcount < minGallop {
				break
			}
		}
		mg++
		ms.minGallop = mg
	}
}

// Merges the pending runs at stack indices i and i plus one
func (ms *mergeState) mergeAt(i int) {
	n := len(ms.pending)
	ssa, na := ms.pending[i].base, ms.pending[i].len
	ssb, nb := ms.pending[i+1].base, ms.pending[i+1].len
	ms.pending[i].len = na + nb
	if i == n-3 {
		ms.pending[i+1] = ms.pending[i+2]
	}
	ms.pending = ms.pending[:n-1]
	k := gallopRight(ms.keys[ssb], ms.keys[ssa:ssa+na], 0)
	ssa += k
	na -= k
	if na == 0 {
		return
	}
	nb = gallopLeft(ms.keys[ssa+na-1], ms.keys[ssb:ssb+nb], nb-1)
	if nb <= 0 {
		return
	}
	if na <= nb {
		ms.mergeLo(ssa, na, ssb, nb)
	} else {
		ms.mergeHi(ssa, na, ssb, nb)
	}
}

// Powersort node level of two adjacent runs
func powerloop(s1, n1, n2, n int) int {
	result := 0
	a := 2*s1 + n1
	b := a + n1 + n2
	for {
		result++
		if a >= n {
			a -= n
			b -= n
		} else if b >= n {
			break
		}
		a <<= 1
		b <<= 1
	}
	return result
}

func (ms *mergeState) foundNewRun(n2 int) {
	if len(ms.pending) == 0 {
		return
	}
	top := ms.pending[len(ms.pending)-1]
	power := powerloop(top.base, top.len, n2, len(ms.keys))
	for len(ms.pending) > 1 && ms.pending[len(ms.pending)-2].power > power {
		ms.mergeAt(len(ms.pending) - 2)
	}
	ms.pending[len(ms.pending)-1].power = power
}

func (ms *mergeState) forceCollapse() {
	for len(ms.pending) > 1 {
		n := len(ms.pending) - 2
		if n > 0 && ms.pending[n-1].len < ms.pending[n+1].len {
			n--
		}
		ms.mergeAt(n)
	}
}
