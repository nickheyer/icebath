package tools

import (
	"fmt"
	"sort"
)

// One matching block of size elements starting at i in a and j in b
type diffMatch struct{ i, j, size int }

// One opcode turning the a range i1 to i2 into the b range j1 to j2
type diffOpcode struct {
	tag            string
	i1, i2, j1, j2 int
}

// Port of Python difflib.SequenceMatcher with junk and autojunk popularity purging
type sequenceMatcher[T comparable] struct {
	a, b           []T
	isjunk         func(T) bool
	autojunk       bool
	b2j            map[T][]int
	bjunk          map[T]bool
	matchingBlocks []diffMatch
	opcodes        []diffOpcode
}

// Builds a matcher over a and b the way SequenceMatcher(isjunk, a, b, autojunk) does
func newSequenceMatcher[T comparable](isjunk func(T) bool, a, b []T, autojunk bool) *sequenceMatcher[T] {
	m := &sequenceMatcher[T]{a: a, b: b, isjunk: isjunk, autojunk: autojunk}
	m.chainB()
	return m
}

// Matcher over the characters of two strings with default autojunk
func newStringMatcher(a, b string) *sequenceMatcher[rune] {
	return newSequenceMatcher[rune](nil, wtf8Runes(a), wtf8Runes(b), true)
}

// Indexes b, then purges junk and, past 200 elements, elements above one percent popularity
func (m *sequenceMatcher[T]) chainB() {
	b := m.b
	m.b2j = map[T][]int{}
	for i, elt := range b {
		m.b2j[elt] = append(m.b2j[elt], i)
	}
	m.bjunk = map[T]bool{}
	if m.isjunk != nil {
		for elt := range m.b2j {
			if m.isjunk(elt) {
				m.bjunk[elt] = true
			}
		}
		for elt := range m.bjunk {
			delete(m.b2j, elt)
		}
	}
	popular := map[T]bool{}
	n := len(b)
	if m.autojunk && n >= 200 {
		ntest := n/100 + 1
		for elt, idxs := range m.b2j {
			if len(idxs) > ntest {
				popular[elt] = true
			}
		}
		for elt := range popular {
			delete(m.b2j, elt)
		}
	}
}

// Longest junk free block inside the given a and b ranges, extended by adjacent junk
func (m *sequenceMatcher[T]) findLongestMatch(alo, ahi, blo, bhi int) diffMatch {
	a, b := m.a, m.b
	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range m.b2j[a[i]] {
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
	for besti > alo && bestj > blo && !m.bjunk[b[bestj-1]] && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && !m.bjunk[b[bestj+bestsize]] && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	for besti > alo && bestj > blo && m.bjunk[b[bestj-1]] && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && m.bjunk[b[bestj+bestsize]] && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	return diffMatch{besti, bestj, bestsize}
}

// Non adjacent matching blocks in order, terminated by the zero size sentinel
func (m *sequenceMatcher[T]) getMatchingBlocks() []diffMatch {
	if m.matchingBlocks != nil {
		return m.matchingBlocks
	}
	la, lb := len(m.a), len(m.b)
	queue := [][4]int{{0, la, 0, lb}}
	var blocks []diffMatch
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		alo, ahi, blo, bhi := q[0], q[1], q[2], q[3]
		x := m.findLongestMatch(alo, ahi, blo, bhi)
		i, j, k := x.i, x.j, x.size
		if k > 0 {
			blocks = append(blocks, x)
			if alo < i && blo < j {
				queue = append(queue, [4]int{alo, i, blo, j})
			}
			if i+k < ahi && j+k < bhi {
				queue = append(queue, [4]int{i + k, ahi, j + k, bhi})
			}
		}
	}
	sort.Slice(blocks, func(p, q int) bool {
		if blocks[p].i != blocks[q].i {
			return blocks[p].i < blocks[q].i
		}
		if blocks[p].j != blocks[q].j {
			return blocks[p].j < blocks[q].j
		}
		return blocks[p].size < blocks[q].size
	})
	i1, j1, k1 := 0, 0, 0
	var nonAdjacent []diffMatch
	for _, blk := range blocks {
		if i1+k1 == blk.i && j1+k1 == blk.j {
			k1 += blk.size
			continue
		}
		if k1 > 0 {
			nonAdjacent = append(nonAdjacent, diffMatch{i1, j1, k1})
		}
		i1, j1, k1 = blk.i, blk.j, blk.size
	}
	if k1 > 0 {
		nonAdjacent = append(nonAdjacent, diffMatch{i1, j1, k1})
	}
	nonAdjacent = append(nonAdjacent, diffMatch{la, lb, 0})
	m.matchingBlocks = nonAdjacent
	return nonAdjacent
}

// Replace, delete, insert and equal opcodes that turn a into b
func (m *sequenceMatcher[T]) getOpcodes() []diffOpcode {
	if m.opcodes != nil {
		return m.opcodes
	}
	i, j := 0, 0
	answer := []diffOpcode{}
	for _, blk := range m.getMatchingBlocks() {
		tag := ""
		switch {
		case i < blk.i && j < blk.j:
			tag = "replace"
		case i < blk.i:
			tag = "delete"
		case j < blk.j:
			tag = "insert"
		}
		if tag != "" {
			answer = append(answer, diffOpcode{tag, i, blk.i, j, blk.j})
		}
		i, j = blk.i+blk.size, blk.j+blk.size
		if blk.size > 0 {
			answer = append(answer, diffOpcode{"equal", blk.i, i, blk.j, j})
		}
	}
	m.opcodes = answer
	return answer
}

// Opcode groups with up to n lines of context, split at long unchanged ranges
func (m *sequenceMatcher[T]) getGroupedOpcodes(n int) [][]diffOpcode {
	codes := append([]diffOpcode(nil), m.getOpcodes()...)
	if len(codes) == 0 {
		codes = []diffOpcode{{"equal", 0, 1, 0, 1}}
	}
	if codes[0].tag == "equal" {
		c := codes[0]
		codes[0] = diffOpcode{c.tag, max(c.i1, c.i2-n), c.i2, max(c.j1, c.j2-n), c.j2}
	}
	if last := len(codes) - 1; codes[last].tag == "equal" {
		c := codes[last]
		codes[last] = diffOpcode{c.tag, c.i1, min(c.i2, c.i1+n), c.j1, min(c.j2, c.j1+n)}
	}
	nn := n + n
	var groups [][]diffOpcode
	var group []diffOpcode
	for _, c := range codes {
		i1, j1 := c.i1, c.j1
		if c.tag == "equal" && c.i2-c.i1 > nn {
			group = append(group, diffOpcode{c.tag, i1, min(c.i2, i1+n), j1, min(c.j2, j1+n)})
			groups = append(groups, group)
			group = nil
			i1, j1 = max(i1, c.i2-n), max(j1, c.j2-n)
		}
		group = append(group, diffOpcode{c.tag, i1, c.i2, j1, c.j2})
	}
	if len(group) > 0 && !(len(group) == 1 && group[0].tag == "equal") {
		groups = append(groups, group)
	}
	return groups
}

func calculateRatio(matches, length int) float64 {
	if length > 0 {
		return 2.0 * float64(matches) / float64(length)
	}
	return 1.0
}

// Similarity in zero to one as twice the matched elements over the total length
func (m *sequenceMatcher[T]) ratio() float64 {
	matches := 0
	for _, blk := range m.getMatchingBlocks() {
		matches += blk.size
	}
	return calculateRatio(matches, len(m.a)+len(m.b))
}

// Similarity ratio of two strings compared by character
func stringRatio(a, b string) float64 { return newStringMatcher(a, b).ratio() }

// Unified range as start,length with the single line and empty forms difflib prints
func formatRangeUnified(start, stop int) string {
	beginning := start + 1
	length := stop - start
	if length == 1 {
		return fmt.Sprint(beginning)
	}
	if length == 0 {
		beginning--
	}
	return fmt.Sprintf("%d,%d", beginning, length)
}

// Port of difflib.unified_diff over lines that keep their own terminators
func unifiedDiff(a, b []string, fromfile, tofile string, n int) []string {
	var out []string
	started := false
	for _, group := range newSequenceMatcher[string](nil, a, b, true).getGroupedOpcodes(n) {
		if !started {
			started = true
			out = append(out, "--- "+fromfile+"\n", "+++ "+tofile+"\n")
		}
		first, last := group[0], group[len(group)-1]
		out = append(out, "@@ -"+formatRangeUnified(first.i1, last.i2)+" +"+formatRangeUnified(first.j1, last.j2)+" @@\n")
		for _, c := range group {
			if c.tag == "equal" {
				for _, line := range a[c.i1:c.i2] {
					out = append(out, " "+line)
				}
				continue
			}
			if c.tag == "replace" || c.tag == "delete" {
				for _, line := range a[c.i1:c.i2] {
					out = append(out, "-"+line)
				}
			}
			if c.tag == "replace" || c.tag == "insert" {
				for _, line := range b[c.j1:c.j2] {
					out = append(out, "+"+line)
				}
			}
		}
	}
	return out
}
