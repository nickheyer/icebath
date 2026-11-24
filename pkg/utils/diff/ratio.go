package diffutil

import "slices"

// Similarity of a and b from 0 to 1: twice the elements they share over their combined length, shared runs found longest first on either side of each match; one for two empty sequences
func Ratio[T comparable](a, b []T) float64 {
	if len(a)+len(b) == 0 {
		return 1
	}
	return ratio(newMatcher(b).common(a), len(a)+len(b))
}

// Twice matches over total, one when both sequences are empty
func ratio(matches, total int) float64 {
	if total == 0 {
		return 1
	}
	return 2 * float64(matches) / float64(total)
}

// Positions of one element value inside the indexed sequence, a window of matcher.pos
type span struct{ start, n int }

// Index of one sequence that finds its longest common runs with any other, reusable across many others
type matcher[T comparable] struct {
	b         []T
	index     map[T]span
	pos       []int
	prev, cur []int
	prevSet   []int
	curSet    []int
	stack     [][4]int
}

// Matcher over b with every element's positions listed in ascending order
func newMatcher[T comparable](b []T) *matcher[T] {
	m := &matcher[T]{
		b:     b,
		index: make(map[T]span, len(b)),
		pos:   make([]int, len(b)),
		prev:  make([]int, len(b)+1),
		cur:   make([]int, len(b)+1),
	}
	for _, v := range b {
		s := m.index[v]
		s.n++
		m.index[v] = s
	}
	off := 0
	for v, s := range m.index {
		m.index[v] = span{start: off}
		off += s.n
	}
	for j, v := range b {
		s := m.index[v]
		m.pos[s.start+s.n] = j
		s.n++
		m.index[v] = s
	}
	return m
}

// Elements a shares with the indexed sequence: the longest common run, then the same again on each side of it
func (m *matcher[T]) common(a []T) int {
	total := 0
	m.stack = append(m.stack[:0], [4]int{0, len(a), 0, len(m.b)})
	for len(m.stack) > 0 {
		q := m.stack[len(m.stack)-1]
		m.stack = m.stack[:len(m.stack)-1]
		i, j, k := m.longest(a, q[0], q[1], q[2], q[3])
		if k == 0 {
			continue
		}
		total += k
		if q[0] < i && q[2] < j {
			m.stack = append(m.stack, [4]int{q[0], i, q[2], j})
		}
		if i+k < q[1] && j+k < q[3] {
			m.stack = append(m.stack, [4]int{i + k, q[1], j + k, q[3]})
		}
	}
	return total
}

// Start in a, start in b and length of the longest common run inside a[alo:ahi] and b[blo:bhi], the one ending first in a on ties
func (m *matcher[T]) longest(a []T, alo, ahi, blo, bhi int) (int, int, int) {
	besti, bestj, best := alo, blo, 0
	for i := alo; i < ahi; i++ {
		m.curSet = m.curSet[:0]
		s := m.index[a[i]]
		ps := m.pos[s.start : s.start+s.n]
		if len(ps) > 0 && ps[0] < blo {
			from, _ := slices.BinarySearch(ps, blo)
			ps = ps[from:]
		}
		for _, j := range ps {
			if j >= bhi {
				break
			}
			k := m.prev[j] + 1
			m.cur[j+1] = k
			m.curSet = append(m.curSet, j+1)
			if k > best {
				besti, bestj, best = i-k+1, j-k+1, k
			}
		}
		for _, j := range m.prevSet {
			m.prev[j] = 0
		}
		m.prev, m.cur = m.cur, m.prev
		m.prevSet, m.curSet = m.curSet, m.prevSet
	}
	for _, j := range m.prevSet {
		m.prev[j] = 0
	}
	m.prevSet = m.prevSet[:0]
	return besti, bestj, best
}
