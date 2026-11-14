package approval

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	asWordBoundary = iota
	asNotWordBoundary
	asPyEnd
	asPrevSpace
	asNextUploadQuote
	asNextNotWordDash
	asNextTargetEnd
	asPrevNotDigitDot
	asNextNotDigitDot
	asPrevNotWordDotDash
	asNextNotWordDotDash
	asNotDashDashEnd
	asNotSpaceDashDashEnd
	asPrevNotEmoji
	asNextNotEmoji
	assertionCount
)

func emojiRange(r rune) bool {
	return (r >= 0x1f000 && r <= 0x1faff) || (r >= 0x2600 && r <= 0x27bf)
}

func pyEnd(s string, i int) bool {
	return i == len(s) || (i == len(s)-1 && s[i] == '\n')
}

func wordAt(s string, i int) bool {
	r, w := runeAt(s, i)
	return w > 0 && pyWord(r)
}

func dashDashEnd(s string, i int) bool {
	return strings.HasPrefix(s[i:], "--") && (spaceAt(s, i+2) || pyEnd(s, i+2))
}

func assertAt(k int, s string, i int) bool {
	prev := runeBefore(s, i)
	next, w := runeAt(s, i)
	switch k {
	case asWordBoundary:
		return (prev >= 0 && pyWord(prev)) != (w > 0 && pyWord(next))
	case asNotWordBoundary:
		return (prev >= 0 && pyWord(prev)) == (w > 0 && pyWord(next))
	case asPyEnd:
		return pyEnd(s, i)
	case asPrevSpace:
		return prev < 0 || pySpace(prev)
	case asNextUploadQuote:
		return w > 0 && strings.ContainsRune(`@"'$`, next)
	case asNextNotWordDash:
		return !(w > 0 && (pyWord(next) || next == '-'))
	case asNextTargetEnd:
		return (w > 0 && (pySpace(next) || strings.ContainsRune(`;&|<>"'`, next))) || pyEnd(s, i)
	case asPrevNotDigitDot:
		return !(prev >= 0 && (unicode.Is(unicode.Nd, prev) || prev == '.'))
	case asNextNotDigitDot:
		return !(w > 0 && (unicode.Is(unicode.Nd, next) || next == '.'))
	case asPrevNotWordDotDash:
		return !(prev >= 0 && (pyWord(prev) || prev == '.' || prev == '-'))
	case asNextNotWordDotDash:
		return !(w > 0 && (pyWord(next) || next == '.' || next == '-'))
	case asNotDashDashEnd:
		return !dashDashEnd(s, i)
	case asNotSpaceDashDashEnd:
		return !(w > 0 && pySpace(next) && dashDashEnd(s, i+w))
	case asPrevNotEmoji:
		return !(prev >= 0 && (emojiRange(prev) || prev == 0xfe0f))
	case asNextNotEmoji:
		return !(w > 0 && emojiRange(next))
	}
	panic(fmt.Sprintf("approval assertion %d has no predicate", k))
}

type machine struct {
	prog    *syntax.Prog
	asserts [][]int
	plain   [][]rune
}

func (m *machine) split() {
	m.asserts = make([][]int, len(m.prog.Inst))
	m.plain = make([][]rune, len(m.prog.Inst))
	for pc := range m.prog.Inst {
		inst := &m.prog.Inst[pc]
		if inst.Op != syntax.InstRune && inst.Op != syntax.InstRune1 {
			continue
		}
		ranges := inst.Rune
		if len(ranges) == 1 {
			ranges = []rune{ranges[0], ranges[0]}
		}
		var asserts []int
		var plain []rune
		for k := 0; k+1 < len(ranges); k += 2 {
			lo, hi := ranges[k], ranges[k+1]
			if lo >= phBase && hi < phBase+assertionCount {
				for r := lo; r <= hi; r++ {
					asserts = append(asserts, int(r-phBase))
				}
				continue
			}
			plain = append(plain, lo, hi)
		}
		if len(asserts) > 0 {
			m.asserts[pc], m.plain[pc] = asserts, plain
		}
	}
}

func inRanges(ranges []rune, r rune) bool {
	for k := 0; k+1 < len(ranges); k += 2 {
		if r >= ranges[k] && r <= ranges[k+1] {
			return true
		}
	}
	return false
}

func compileMachine(src string, lookarounds ...string) *machine {
	pairs := make([]string, 0, len(lookarounds))
	for k := 0; k+1 < len(lookarounds); k += 2 {
		if !strings.Contains(src, lookarounds[k]) {
			panic(fmt.Sprintf("approval pattern lacks the lookaround %q", lookarounds[k]))
		}
		pairs = append(pairs, lookarounds[k], lookarounds[k+1])
	}
	pattern := translate(strings.NewReplacer(pairs...).Replace(src), true, true)
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		panic(fmt.Sprintf("approval pattern %q: %v", src, err))
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		panic(fmt.Sprintf("approval pattern %q: %v", src, err))
	}
	m := &machine{prog: prog}
	m.split()
	return m
}

type stateSet struct {
	dense  []uint32
	sparse []uint32
}

func newStateSet(n int) *stateSet {
	return &stateSet{dense: make([]uint32, 0, n), sparse: make([]uint32, n)}
}

func (q *stateSet) has(pc uint32) bool {
	k := q.sparse[pc]
	return int(k) < len(q.dense) && q.dense[k] == pc
}

func (q *stateSet) insert(pc uint32) {
	q.sparse[pc] = uint32(len(q.dense))
	q.dense = append(q.dense, pc)
}

func (m *machine) closure(q *stateSet, pc0 uint32, s string, pos int, stack []uint32) ([]uint32, bool) {
	stack = append(stack[:0], pc0)
	for len(stack) > 0 {
		pc := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if q.has(pc) {
			continue
		}
		q.insert(pc)
		inst := &m.prog.Inst[pc]
		switch inst.Op {
		case syntax.InstMatch:
			return stack, true
		case syntax.InstAlt, syntax.InstAltMatch:
			stack = append(stack, inst.Arg, inst.Out)
		case syntax.InstCapture, syntax.InstNop:
			stack = append(stack, inst.Out)
		case syntax.InstEmptyWidth:
			if syntax.EmptyOp(inst.Arg)&^syntax.EmptyOpContext(runeBefore(s, pos), nextRune(s, pos)) == 0 {
				stack = append(stack, inst.Out)
			}
		case syntax.InstRune, syntax.InstRune1:
			for _, k := range m.asserts[pc] {
				if assertAt(k, s, pos) {
					stack = append(stack, inst.Out)
					break
				}
			}
		}
	}
	return stack, false
}

func nextRune(s string, i int) rune {
	r, w := runeAt(s, i)
	if w == 0 {
		return -1
	}
	return r
}

func (m *machine) search(s string) bool {
	n := len(m.prog.Inst)
	cur, next := newStateSet(n), newStateSet(n)
	var stack []uint32
	var hit bool
	for pos := 0; ; {
		if stack, hit = m.closure(cur, uint32(m.prog.Start), s, pos, stack); hit {
			return true
		}
		if pos >= len(s) {
			return false
		}
		r, w := utf8.DecodeRuneInString(s[pos:])
		next.dense = next.dense[:0]
		for _, pc := range cur.dense {
			inst := &m.prog.Inst[pc]
			ok := false
			switch inst.Op {
			case syntax.InstRuneAny:
				ok = true
			case syntax.InstRuneAnyNotNL:
				ok = r != '\n'
			case syntax.InstRune, syntax.InstRune1:
				if m.asserts[pc] == nil {
					ok = inst.MatchRune(r)
				} else {
					ok = inRanges(m.plain[pc], r)
				}
			}
			if ok {
				if stack, hit = m.closure(next, inst.Out, s, pos+w, stack); hit {
					return true
				}
			}
		}
		cur, next = next, cur
		pos += w
	}
}

type matcher interface {
	MatchString(s string) bool
}

func wideRe(src string, ci bool) *regexp.Regexp {
	return regexp.MustCompile(translate(src, false, ci))
}

func (m *machine) MatchString(s string) bool { return m.search(s) }
