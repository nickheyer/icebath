package approval

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
)

type opcode struct {
	tag            byte
	i1, i2, j1, j2 int
}

type sequenceMatcher struct {
	a, b     []string
	b2j      map[string][]int
	matching [][3]int
}

func newSequenceMatcher(a, b []string) *sequenceMatcher {
	m := &sequenceMatcher{a: a, b: b, b2j: map[string][]int{}}
	for i, elt := range b {
		m.b2j[elt] = append(m.b2j[elt], i)
	}
	if n := len(b); n >= 200 {
		ntest := n/100 + 1
		var popular []string
		for elt, idxs := range m.b2j {
			if len(idxs) > ntest {
				popular = append(popular, elt)
			}
		}
		for _, elt := range popular {
			delete(m.b2j, elt)
		}
	}
	return m
}

func (m *sequenceMatcher) longest(alo, ahi, blo, bhi int) (int, int, int) {
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
	return besti, bestj, bestsize
}

func (m *sequenceMatcher) matchingBlocks() [][3]int {
	if m.matching != nil {
		return m.matching
	}
	type span struct{ alo, ahi, blo, bhi int }
	queue := []span{{0, len(m.a), 0, len(m.b)}}
	var blocks [][3]int
	for len(queue) > 0 {
		s := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		i, j, k := m.longest(s.alo, s.ahi, s.blo, s.bhi)
		if k == 0 {
			continue
		}
		blocks = append(blocks, [3]int{i, j, k})
		if s.alo < i && s.blo < j {
			queue = append(queue, span{s.alo, i, s.blo, j})
		}
		if i+k < s.ahi && j+k < s.bhi {
			queue = append(queue, span{i + k, s.ahi, j + k, s.bhi})
		}
	}
	slices.SortFunc(blocks, func(x, y [3]int) int {
		for k := range 3 {
			if c := cmp.Compare(x[k], y[k]); c != 0 {
				return c
			}
		}
		return 0
	})
	i1, j1, k1 := 0, 0, 0
	var merged [][3]int
	for _, blk := range blocks {
		i2, j2, k2 := blk[0], blk[1], blk[2]
		if i1+k1 == i2 && j1+k1 == j2 {
			k1 += k2
			continue
		}
		if k1 > 0 {
			merged = append(merged, [3]int{i1, j1, k1})
		}
		i1, j1, k1 = i2, j2, k2
	}
	if k1 > 0 {
		merged = append(merged, [3]int{i1, j1, k1})
	}
	merged = append(merged, [3]int{len(m.a), len(m.b), 0})
	m.matching = merged
	return merged
}

func (m *sequenceMatcher) opcodes() []opcode {
	i, j := 0, 0
	var out []opcode
	for _, blk := range m.matchingBlocks() {
		ai, bj, size := blk[0], blk[1], blk[2]
		var tag byte
		switch {
		case i < ai && j < bj:
			tag = 'r'
		case i < ai:
			tag = 'd'
		case j < bj:
			tag = 'i'
		}
		if tag != 0 {
			out = append(out, opcode{tag, i, ai, j, bj})
		}
		i, j = ai+size, bj+size
		if size > 0 {
			out = append(out, opcode{'e', ai, i, bj, j})
		}
	}
	return out
}

func (m *sequenceMatcher) groupedOpcodes(n int) [][]opcode {
	codes := m.opcodes()
	if len(codes) == 0 {
		codes = []opcode{{'e', 0, 1, 0, 1}}
	}
	if codes[0].tag == 'e' {
		c := codes[0]
		codes[0] = opcode{'e', max(c.i1, c.i2-n), c.i2, max(c.j1, c.j2-n), c.j2}
	}
	if last := codes[len(codes)-1]; last.tag == 'e' {
		codes[len(codes)-1] = opcode{'e', last.i1, min(last.i2, last.i1+n), last.j1, min(last.j2, last.j1+n)}
	}
	nn := n + n
	var groups [][]opcode
	var group []opcode
	for _, c := range codes {
		if c.tag == 'e' && c.i2-c.i1 > nn {
			group = append(group, opcode{'e', c.i1, min(c.i2, c.i1+n), c.j1, min(c.j2, c.j1+n)})
			groups = append(groups, group)
			group = nil
			c.i1, c.j1 = max(c.i1, c.i2-n), max(c.j1, c.j2-n)
		}
		group = append(group, c)
	}
	if len(group) > 0 && !(len(group) == 1 && group[0].tag == 'e') {
		groups = append(groups, group)
	}
	return groups
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

func unifiedDiff(a, b []string, fromFile, toFile string) string {
	var out strings.Builder
	started := false
	for _, group := range newSequenceMatcher(a, b).groupedOpcodes(3) {
		if !started {
			started = true
			out.WriteString("--- " + fromFile + "\n")
			out.WriteString("+++ " + toFile + "\n")
		}
		first, last := group[0], group[len(group)-1]
		out.WriteString("@@ -" + unifiedRange(first.i1, last.i2) + " +" + unifiedRange(first.j1, last.j2) + " @@\n")
		for _, c := range group {
			if c.tag == 'e' {
				for _, line := range a[c.i1:c.i2] {
					out.WriteString(" " + line)
				}
				continue
			}
			if c.tag == 'r' || c.tag == 'd' {
				for _, line := range a[c.i1:c.i2] {
					out.WriteString("-" + line)
				}
			}
			if c.tag == 'r' || c.tag == 'i' {
				for _, line := range b[c.j1:c.j2] {
					out.WriteString("+" + line)
				}
			}
		}
	}
	return out.String()
}

func splitLinesKeep(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, w := runeAt(s, i)
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, s[start:i+w])
			start = i + w
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				w = 2
			}
			lines = append(lines, s[start:i+w])
			start = i + w
		}
		i += w
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
