// Package diff compares sequences the way Python's difflib does: matching blocks and opcodes of a sequence matcher, the similarity
// ratio of two texts, and unified diffs of their lines
package diff

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Run of size equal elements starting at I in a and J in b
type Match struct{ I, J, Size int }

// Change turning a[I1:I2] into b[J1:J2]: equal, replace, delete or insert
type Opcode struct {
	Tag            string
	I1, I2, J1, J2 int
}

// Finds the longest matching blocks of two sequences, elements of b that are junk or too popular left out of the index
type Matcher[T comparable] struct {
	a, b    []T
	b2j     map[T][]int
	junk    map[T]bool
	blocks  []Match
	opcodes []Opcode
}

// Matcher over a and b; isjunk may be nil, and autojunk drops elements of b more popular than one percent past 200 of them
func New[T comparable](isjunk func(T) bool, a, b []T, autojunk bool) *Matcher[T] {
	m := &Matcher[T]{a: a, b: b, b2j: map[T][]int{}, junk: map[T]bool{}}
	for i, elt := range b {
		m.b2j[elt] = append(m.b2j[elt], i)
	}
	if isjunk != nil {
		for elt := range m.b2j {
			if isjunk(elt) {
				m.junk[elt] = true
				delete(m.b2j, elt)
			}
		}
	}
	if n := len(b); autojunk && n >= 200 {
		limit := n/100 + 1
		for elt, at := range m.b2j {
			if len(at) > limit {
				delete(m.b2j, elt)
			}
		}
	}
	return m
}

// Matcher over the characters of two texts
func Runes(a, b string) *Matcher[rune] {
	return New[rune](nil, []rune(a), []rune(b), true)
}

// Similarity of two texts from zero to one, twice the characters they share over the characters of both
func Ratio(a, b string) float64 {
	return Runes(a, b).Ratio()
}

func (m *Matcher[T]) longest(alo, ahi, blo, bhi int) Match {
	a, b := m.a, m.b
	besti, bestj, size := alo, blo, 0
	lengths := map[int]int{}
	for i := alo; i < ahi; i++ {
		next := map[int]int{}
		for _, j := range m.b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := lengths[j-1] + 1
			next[j] = k
			if k > size {
				besti, bestj, size = i-k+1, j-k+1, k
			}
		}
		lengths = next
	}
	for besti > alo && bestj > blo && !m.junk[b[bestj-1]] && a[besti-1] == b[bestj-1] {
		besti, bestj, size = besti-1, bestj-1, size+1
	}
	for besti+size < ahi && bestj+size < bhi && !m.junk[b[bestj+size]] && a[besti+size] == b[bestj+size] {
		size++
	}
	for besti > alo && bestj > blo && m.junk[b[bestj-1]] && a[besti-1] == b[bestj-1] {
		besti, bestj, size = besti-1, bestj-1, size+1
	}
	for besti+size < ahi && bestj+size < bhi && m.junk[b[bestj+size]] && a[besti+size] == b[bestj+size] {
		size++
	}
	return Match{besti, bestj, size}
}

// Matching blocks in order, adjacent ones joined, ending with a block of size zero at the ends of both
func (m *Matcher[T]) Blocks() []Match {
	if m.blocks != nil {
		return m.blocks
	}
	queue := [][4]int{{0, len(m.a), 0, len(m.b)}}
	var found []Match
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		x := m.longest(q[0], q[1], q[2], q[3])
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
	slices.SortFunc(found, func(p, q Match) int {
		if p.I != q.I {
			return p.I - q.I
		}
		if p.J != q.J {
			return p.J - q.J
		}
		return p.Size - q.Size
	})
	var joined []Match
	run := Match{}
	for _, block := range found {
		if run.I+run.Size == block.I && run.J+run.Size == block.J {
			run.Size += block.Size
			continue
		}
		if run.Size > 0 {
			joined = append(joined, run)
		}
		run = block
	}
	if run.Size > 0 {
		joined = append(joined, run)
	}
	m.blocks = append(joined, Match{len(m.a), len(m.b), 0})
	return m.blocks
}

// Changes turning a into b, equal runs included
func (m *Matcher[T]) Opcodes() []Opcode {
	if m.opcodes != nil {
		return m.opcodes
	}
	i, j := 0, 0
	codes := []Opcode{}
	for _, block := range m.Blocks() {
		tag := ""
		switch {
		case i < block.I && j < block.J:
			tag = "replace"
		case i < block.I:
			tag = "delete"
		case j < block.J:
			tag = "insert"
		}
		if tag != "" {
			codes = append(codes, Opcode{tag, i, block.I, j, block.J})
		}
		i, j = block.I+block.Size, block.J+block.Size
		if block.Size > 0 {
			codes = append(codes, Opcode{"equal", block.I, i, block.J, j})
		}
	}
	m.opcodes = codes
	return codes
}

// Opcodes in groups of changes with up to n equal elements of context around them
func (m *Matcher[T]) Grouped(n int) [][]Opcode {
	codes := slices.Clone(m.Opcodes())
	if len(codes) == 0 {
		codes = []Opcode{{"equal", 0, 1, 0, 1}}
	}
	if c := codes[0]; c.Tag == "equal" {
		codes[0] = Opcode{c.Tag, max(c.I1, c.I2-n), c.I2, max(c.J1, c.J2-n), c.J2}
	}
	if last := len(codes) - 1; codes[last].Tag == "equal" {
		c := codes[last]
		codes[last] = Opcode{c.Tag, c.I1, min(c.I2, c.I1+n), c.J1, min(c.J2, c.J1+n)}
	}
	var groups [][]Opcode
	var group []Opcode
	for _, c := range codes {
		i1, j1 := c.I1, c.J1
		if c.Tag == "equal" && c.I2-c.I1 > 2*n {
			group = append(group, Opcode{c.Tag, i1, min(c.I2, i1+n), j1, min(c.J2, j1+n)})
			groups = append(groups, group)
			group = nil
			i1, j1 = max(i1, c.I2-n), max(j1, c.J2-n)
		}
		group = append(group, Opcode{c.Tag, i1, c.I2, j1, c.J2})
	}
	if len(group) > 0 && !(len(group) == 1 && group[0].Tag == "equal") {
		groups = append(groups, group)
	}
	return groups
}

// Similarity from zero to one, twice the matched elements over the elements of both
func (m *Matcher[T]) Ratio() float64 {
	matched := 0
	for _, block := range m.Blocks() {
		matched += block.Size
	}
	if total := len(m.a) + len(m.b); total > 0 {
		return 2 * float64(matched) / float64(total)
	}
	return 1
}

// Range of a hunk header, start,length with the forms difflib prints for one line and for none
func span(start, stop int) string {
	first, length := start+1, stop-start
	switch length {
	case 1:
		return fmt.Sprint(first)
	case 0:
		first--
	}
	return fmt.Sprintf("%d,%d", first, length)
}

// Unified diff of lines that keep their own line ends, under the two file names with n lines of context, empty when they are equal
func Unified(a, b []string, from, to string, n int) string {
	var out strings.Builder
	for _, group := range New[string](nil, a, b, true).Grouped(n) {
		if out.Len() == 0 {
			out.WriteString("--- " + from + "\n+++ " + to + "\n")
		}
		first, last := group[0], group[len(group)-1]
		out.WriteString("@@ -" + span(first.I1, last.I2) + " +" + span(first.J1, last.J2) + " @@\n")
		for _, c := range group {
			if c.Tag == "equal" {
				for _, line := range a[c.I1:c.I2] {
					out.WriteString(" " + line)
				}
				continue
			}
			if c.Tag == "replace" || c.Tag == "delete" {
				for _, line := range a[c.I1:c.I2] {
					out.WriteString("-" + line)
				}
			}
			if c.Tag == "replace" || c.Tag == "insert" {
				for _, line := range b[c.J1:c.J2] {
					out.WriteString("+" + line)
				}
			}
		}
	}
	return out.String()
}

// Unified diff of two texts under a/ and b/ names of the file
func Files(before, after, name string) string {
	return Unified(Lines(before), Lines(after), "a/"+name, "b/"+name, 3)
}

// Lines of a text each with its line end, at every break a text editor knows: \n, \r\n, \r, vertical tab, form feed, the file, group and
// record separators, next line and the Unicode line and paragraph separators
func Lines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		end := -1
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			end = i + size
		case '\r':
			end = i + 1
			if i+1 < len(s) && s[i+1] == '\n' {
				end = i + 2
			}
		}
		if end >= 0 {
			out = append(out, s[start:end])
			start, i = end, end
			continue
		}
		i += size
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Lines of a text without their line ends
func Split(s string) []string {
	lines := Lines(s)
	for i, line := range lines {
		if strings.HasSuffix(line, "\r\n") {
			lines[i] = line[:len(line)-2]
			continue
		}
		switch r, size := utf8.DecodeLastRuneInString(line); r {
		case '\n', '\r', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			lines[i] = line[:len(line)-size]
		}
	}
	return lines
}
