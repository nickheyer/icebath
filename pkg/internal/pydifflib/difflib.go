package pydifflib

import (
	"fmt"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// One matching block of size elements starting at I in a and J in b
type Match struct{ I, J, Size int }

// Port of difflib.SequenceMatcher without a junk predicate, with autojunk popularity purging
type SequenceMatcher[T comparable] struct {
	a, b     []T
	autojunk bool
	b2j      map[T][]int
	blocks   []Match
}

// Matcher over a and b the way SequenceMatcher(None, a, b, autojunk) builds it
func NewSequenceMatcher[T comparable](a, b []T, autojunk bool) *SequenceMatcher[T] {
	m := &SequenceMatcher[T]{a: a, b: b, autojunk: autojunk}
	m.chainB()
	return m
}

// Indexes b and, past 200 elements under autojunk, drops elements above one percent popularity
func (m *SequenceMatcher[T]) chainB() {
	m.b2j = map[T][]int{}
	for i, elt := range m.b {
		m.b2j[elt] = append(m.b2j[elt], i)
	}
	if n := len(m.b); m.autojunk && n >= 200 {
		ntest := n/100 + 1
		for elt, idxs := range m.b2j {
			if len(idxs) > ntest {
				delete(m.b2j, elt)
			}
		}
	}
}

// Longest block inside the given ranges, earliest in a then in b on ties
func (m *SequenceMatcher[T]) findLongestMatch(alo, ahi, blo, bhi int) Match {
	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range m.b2j[m.a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	for besti > alo && bestj > blo && m.a[besti-1] == m.b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && m.a[besti+bestsize] == m.b[bestj+bestsize] {
		bestsize++
	}
	return Match{besti, bestj, bestsize}
}

// Non adjacent matching blocks in order, ending with the zero size sentinel
func (m *SequenceMatcher[T]) MatchingBlocks() []Match {
	if m.blocks != nil {
		return m.blocks
	}
	la, lb := len(m.a), len(m.b)
	queue := [][4]int{{0, la, 0, lb}}
	var found []Match
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		x := m.findLongestMatch(q[0], q[1], q[2], q[3])
		if x.Size == 0 {
			continue
		}
		found = append(found, x)
		if q[0] < x.I && q[2] < x.J {
			queue = append(queue, [4]int{q[0], x.I, q[2], x.J})
		}
		if x.I+x.Size < q[1] && x.J+x.Size < q[3] {
			queue = append(queue, [4]int{x.I + x.Size, q[1], x.J + x.Size, q[3]})
		}
	}
	sort.Slice(found, func(p, q int) bool {
		if found[p].I != found[q].I {
			return found[p].I < found[q].I
		}
		return found[p].J < found[q].J
	})
	var out []Match
	i1, j1, k1 := 0, 0, 0
	for _, blk := range found {
		if i1+k1 == blk.I && j1+k1 == blk.J {
			k1 += blk.Size
			continue
		}
		if k1 > 0 {
			out = append(out, Match{i1, j1, k1})
		}
		i1, j1, k1 = blk.I, blk.J, blk.Size
	}
	if k1 > 0 {
		out = append(out, Match{i1, j1, k1})
	}
	m.blocks = append(out, Match{la, lb, 0})
	return m.blocks
}

// Twice the matched elements over the combined length, one for two empty sequences
func (m *SequenceMatcher[T]) Ratio() float64 {
	matches := 0
	for _, blk := range m.MatchingBlocks() {
		matches += blk.Size
	}
	if total := len(m.a) + len(m.b); total > 0 {
		return 2.0 * float64(matches) / float64(total)
	}
	return 1.0
}

// Best n possibilities scoring at least cutoff against word, highest score then greatest text first
func GetCloseMatches(word string, possibilities []string, n int, cutoff float64) []string {
	if n <= 0 {
		panic(fmt.Sprintf("n must be > 0: %d", n))
	}
	if cutoff < 0.0 || cutoff > 1.0 {
		panic(fmt.Sprintf("cutoff must be in [0.0, 1.0]: %s", pyrepr.Float(cutoff)))
	}
	type scored struct {
		score float64
		text  string
	}
	wordRunes := pystr.Runes(word)
	var results []scored
	for _, x := range possibilities {
		if score := NewSequenceMatcher(pystr.Runes(x), wordRunes, true).Ratio(); score >= cutoff {
			results = append(results, scored{score, x})
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].text > results[j].text
	})
	if len(results) > n {
		results = results[:n]
	}
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.text)
	}
	return out
}
