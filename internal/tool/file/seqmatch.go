package file

import (
	"sort"
	"strconv"
	"strings"
)

type block struct {
	i, j, size int
}

type opTag uint8

const (
	opEqual opTag = iota
	opReplace
	opDelete
	opInsert
)

type opcode struct {
	tag            opTag
	i1, i2, j1, j2 int
}

type matcher[T comparable] struct {
	a, b       []T
	b2j        map[T][]int
	rowA, rowB []int
	blocks     []block
	solved     bool
}

func newMatcher[T comparable](a, b []T) *matcher[T] {
	m := &matcher[T]{a: a, b: b, b2j: make(map[T][]int)}
	for i, elt := range b {
		m.b2j[elt] = append(m.b2j[elt], i)
	}
	if n := len(b); n >= 200 {
		popular := n/100 + 1
		for elt, idxs := range m.b2j {
			if len(idxs) > popular {
				delete(m.b2j, elt)
			}
		}
	}
	m.rowA = make([]int, len(b)+1)
	m.rowB = make([]int, len(b)+1)
	return m
}

func (m *matcher[T]) longest(alo, ahi, blo, bhi int) block {
	a, b := m.a, m.b
	besti, bestj, bestsize := alo, blo, 0
	prev, cur := m.rowA, m.rowB
	var prevTouched, curTouched []int
	for i := alo; i < ahi; i++ {
		for _, j := range m.b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := prev[j] + 1
			cur[j+1] = k
			curTouched = append(curTouched, j+1)
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		for _, t := range prevTouched {
			prev[t] = 0
		}
		prevTouched = prevTouched[:0]
		prev, cur = cur, prev
		prevTouched, curTouched = curTouched, prevTouched
	}
	for _, t := range prevTouched {
		prev[t] = 0
	}
	for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	return block{besti, bestj, bestsize}
}

func (m *matcher[T]) matchingBlocks() []block {
	if m.solved {
		return m.blocks
	}
	la, lb := len(m.a), len(m.b)
	queue := [][4]int{{0, la, 0, lb}}
	var found []block
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		alo, ahi, blo, bhi := q[0], q[1], q[2], q[3]
		x := m.longest(alo, ahi, blo, bhi)
		if x.size == 0 {
			continue
		}
		found = append(found, x)
		if alo < x.i && blo < x.j {
			queue = append(queue, [4]int{alo, x.i, blo, x.j})
		}
		if x.i+x.size < ahi && x.j+x.size < bhi {
			queue = append(queue, [4]int{x.i + x.size, ahi, x.j + x.size, bhi})
		}
	}
	sort.Slice(found, func(p, q int) bool {
		if found[p].i != found[q].i {
			return found[p].i < found[q].i
		}
		if found[p].j != found[q].j {
			return found[p].j < found[q].j
		}
		return found[p].size < found[q].size
	})
	var merged []block
	i1, j1, k1 := 0, 0, 0
	for _, x := range found {
		if i1+k1 == x.i && j1+k1 == x.j {
			k1 += x.size
			continue
		}
		if k1 > 0 {
			merged = append(merged, block{i1, j1, k1})
		}
		i1, j1, k1 = x.i, x.j, x.size
	}
	if k1 > 0 {
		merged = append(merged, block{i1, j1, k1})
	}
	merged = append(merged, block{la, lb, 0})
	m.blocks, m.solved = merged, true
	return merged
}

func (m *matcher[T]) opcodes() []opcode {
	var out []opcode
	i, j := 0, 0
	for _, x := range m.matchingBlocks() {
		switch {
		case i < x.i && j < x.j:
			out = append(out, opcode{opReplace, i, x.i, j, x.j})
		case i < x.i:
			out = append(out, opcode{opDelete, i, x.i, j, x.j})
		case j < x.j:
			out = append(out, opcode{opInsert, i, x.i, j, x.j})
		}
		i, j = x.i+x.size, x.j+x.size
		if x.size > 0 {
			out = append(out, opcode{opEqual, x.i, i, x.j, j})
		}
	}
	return out
}

func (m *matcher[T]) groupedOpcodes(n int) [][]opcode {
	codes := m.opcodes()
	if len(codes) == 0 {
		codes = []opcode{{opEqual, 0, 1, 0, 1}}
	}
	if c := codes[0]; c.tag == opEqual {
		codes[0] = opcode{c.tag, max(c.i1, c.i2-n), c.i2, max(c.j1, c.j2-n), c.j2}
	}
	if c := codes[len(codes)-1]; c.tag == opEqual {
		codes[len(codes)-1] = opcode{c.tag, c.i1, min(c.i2, c.i1+n), c.j1, min(c.j2, c.j1+n)}
	}
	var groups [][]opcode
	var group []opcode
	for _, c := range codes {
		i1, j1 := c.i1, c.j1
		if c.tag == opEqual && c.i2-c.i1 > n+n {
			group = append(group, opcode{c.tag, i1, min(c.i2, i1+n), j1, min(c.j2, j1+n)})
			groups = append(groups, group)
			group = nil
			i1, j1 = max(i1, c.i2-n), max(j1, c.j2-n)
		}
		group = append(group, opcode{c.tag, i1, c.i2, j1, c.j2})
	}
	if len(group) > 0 && !(len(group) == 1 && group[0].tag == opEqual) {
		groups = append(groups, group)
	}
	return groups
}

func (m *matcher[T]) ratio() float64 {
	matches := 0
	for _, x := range m.matchingBlocks() {
		matches += x.size
	}
	total := len(m.a) + len(m.b)
	if total == 0 {
		return 1.0
	}
	return 2.0 * float64(matches) / float64(total)
}

func similarity(a, b string) float64 {
	return newMatcher(decodeRunes(a), decodeRunes(b)).ratio()
}

func similarAtLeast(a, b string, threshold float64) bool {
	if a == b {
		return 1.0 >= threshold
	}
	ra, rb := decodeRunes(a), decodeRunes(b)
	total := len(ra) + len(rb)
	if total > 0 && 2.0*float64(min(len(ra), len(rb)))/float64(total) < threshold {
		return false
	}
	return newMatcher(ra, rb).ratio() >= threshold
}

func unifiedRange(start, stop int) string {
	beginning := start + 1
	length := stop - start
	if length == 1 {
		return strconv.Itoa(beginning)
	}
	if length == 0 {
		beginning--
	}
	return strconv.Itoa(beginning) + "," + strconv.Itoa(length)
}

func unifiedDiff(old, updated []string, fromFile, toFile string) string {
	var out strings.Builder
	started := false
	for _, group := range newMatcher(old, updated).groupedOpcodes(3) {
		if !started {
			started = true
			out.WriteString("--- " + fromFile + "\n")
			out.WriteString("+++ " + toFile + "\n")
		}
		first, last := group[0], group[len(group)-1]
		out.WriteString("@@ -" + unifiedRange(first.i1, last.i2) + " +" + unifiedRange(first.j1, last.j2) + " @@\n")
		for _, c := range group {
			if c.tag == opEqual {
				for _, line := range old[c.i1:c.i2] {
					out.WriteString(" " + line)
				}
				continue
			}
			if c.tag == opReplace || c.tag == opDelete {
				for _, line := range old[c.i1:c.i2] {
					out.WriteString("-" + line)
				}
			}
			if c.tag == opReplace || c.tag == opInsert {
				for _, line := range updated[c.j1:c.j2] {
					out.WriteString("+" + line)
				}
			}
		}
	}
	return out.String()
}

func diffText(old, updated, fromFile, toFile string) string {
	return unifiedDiff(splitLines(old, true), splitLines(updated, true), fromFile, toFile)
}
