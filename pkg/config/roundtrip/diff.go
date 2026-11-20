package roundtrip

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type match struct{ i, j, size int }

// Edit operation over line ranges with tag r, d, i or e
type Opcode struct {
	Tag            byte
	I1, I2, J1, J2 int
}

// Longest common block of a[alo:ahi] and b[blo:bhi] the way difflib finds it without junk
func longestMatch(a []string, b2j map[string][]int, alo, ahi, blo, bhi int) match {
	best := match{alo, blo, 0}
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		next := map[int]int{}
		for _, j := range b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			next[j] = k
			if k > best.size {
				best = match{i - k + 1, j - k + 1, k}
			}
		}
		j2len = next
	}
	return best
}

// Opcodes of difflib.SequenceMatcher(None, a, b, autojunk=False)
func Opcodes(a, b []string) []Opcode {
	b2j := map[string][]int{}
	for j, line := range b {
		b2j[line] = append(b2j[line], j)
	}
	var blocks []match
	queue := [][4]int{{0, len(a), 0, len(b)}}
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		m := longestMatch(a, b2j, q[0], q[1], q[2], q[3])
		if m.size == 0 {
			continue
		}
		blocks = append(blocks, m)
		if q[0] < m.i && q[2] < m.j {
			queue = append(queue, [4]int{q[0], m.i, q[2], m.j})
		}
		if m.i+m.size < q[1] && m.j+m.size < q[3] {
			queue = append(queue, [4]int{m.i + m.size, q[1], m.j + m.size, q[3]})
		}
	}
	sort.Slice(blocks, func(x, y int) bool {
		if blocks[x].i != blocks[y].i {
			return blocks[x].i < blocks[y].i
		}
		return blocks[x].j < blocks[y].j
	})
	var merged []match
	for _, m := range blocks {
		if n := len(merged); n > 0 && merged[n-1].i+merged[n-1].size == m.i && merged[n-1].j+merged[n-1].size == m.j {
			merged[n-1].size += m.size
			continue
		}
		merged = append(merged, m)
	}
	merged = append(merged, match{len(a), len(b), 0})
	var out []Opcode
	i, j := 0, 0
	for _, m := range merged {
		var tag byte
		switch {
		case i < m.i && j < m.j:
			tag = 'r'
		case i < m.i:
			tag = 'd'
		case j < m.j:
			tag = 'i'
		}
		if tag != 0 {
			out = append(out, Opcode{tag, i, m.i, j, m.j})
		}
		i, j = m.i+m.size, m.j+m.size
		if m.size > 0 {
			out = append(out, Opcode{'e', m.i, i, m.j, j})
		}
	}
	return out
}

// Lines of a text the way str.splitlines cuts a newline terminated file
func Lines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

var (
	nullToken  = regexp.MustCompile(`^(\s*[^#\n]*?:)\s+(?:null|~)(\s+#.*)?$`)
	colonSpace = regexp.MustCompile(`:\s+#`)
)

// Line with an untouched null spelled as an empty value, the only cosmetic rewrite allowed
func CanonLine(line string) string {
	if m := nullToken.FindStringSubmatch(line); m != nil {
		if m[2] != "" {
			return m[1] + " " + strings.TrimSpace(m[2])
		}
		return m[1]
	}
	return colonSpace.ReplaceAllString(line, ": #")
}

// Indices of before lines replaced or deleted and the blocks of inserted after lines
func LineDiff(before, after string) (map[int]bool, [][]string) {
	var a, b []string
	for _, l := range Lines(before) {
		a = append(a, CanonLine(l))
	}
	for _, l := range Lines(after) {
		b = append(b, CanonLine(l))
	}
	touched := map[int]bool{}
	var inserted [][]string
	for _, op := range Opcodes(a, b) {
		if op.Tag == 'r' || op.Tag == 'd' {
			for i := op.I1; i < op.I2; i++ {
				touched[i] = true
			}
		}
		if (op.Tag == 'r' || op.Tag == 'i') && op.J2 > op.J1 {
			inserted = append(inserted, b[op.J1:op.J2])
		}
	}
	return touched, inserted
}

// Line oriented diff of two texts for failure messages
func UDiff(before, after string) string {
	a, b := Lines(before), Lines(after)
	var out strings.Builder
	for _, op := range Opcodes(a, b) {
		if op.Tag == 'e' {
			continue
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", op.I1+1, op.I2-op.I1, op.J1+1, op.J2-op.J1)
		for _, l := range a[op.I1:op.I2] {
			out.WriteString("-" + l + "\n")
		}
		for _, l := range b[op.J1:op.J2] {
			out.WriteString("+" + l + "\n")
		}
	}
	return out.String()
}

// Count of lines across inserted blocks
func InsertedLines(blocks [][]string) int {
	n := 0
	for _, b := range blocks {
		n += len(b)
	}
	return n
}

// Error unless bytes outside the target lines stay identical and a new key lands as one block
func OneKeyWrite(ctx, before, after string, targets [][]string, targetLines map[int]bool, allowInsert bool) error {
	touched, inserted := LineDiff(before, after)
	var stray []int
	for i := range touched {
		if !targetLines[i] {
			stray = append(stray, i)
		}
	}
	if len(stray) > 0 {
		sort.Ints(stray)
		return fmt.Errorf("[%s] a write to %v rewrote untouched lines %v:\n%s", ctx, targets, stray, UDiff(before, after))
	}
	if !allowInsert && InsertedLines(inserted) > max(len(targetLines), 1) {
		return fmt.Errorf("[%s] a value change inserted extra lines:\n%s", ctx, UDiff(before, after))
	}
	if allowInsert && len(inserted) > 1 {
		return fmt.Errorf("[%s] new key split across blocks:\n%s", ctx, UDiff(before, after))
	}
	return NoDuplicateTopLevel(after, ctx)
}
