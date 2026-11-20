package pyast

import (
	"errors"
	"strings"
	"sync"
)

// Replaces the result with CPython's MemoryError when its parser would overflow, keeping the warnings it emits first
func (p *parser) withCPythonDepth(n *Node, err error) (*Node, error) {
	var le *LimitError
	var se *SyntaxError
	if errors.As(err, &le) || err != nil && !errors.As(err, &se) {
		return n, err
	}
	warns, main, warned := p.warns, p.main, p.warnedEscape
	p.warns, p.main, p.warnedEscape = nil, 0, map[*token]bool{}
	if p.cpythonStackOverflows(err != nil) {
		return nil, &LimitError{Class: "MemoryError", Msg: "Parser stack overflowed - Python source too complex to parse"}
	}
	p.warns, p.main, p.warnedEscape = warns, main, warned
	return n, err
}

type recStop struct{ overflow bool }

type rec struct {
	p       *parser
	types   []int
	pos     int
	level   int
	invalid bool
	barry   bool
	memo    []int32
	filled  int
	peak    int
	// barryTok is the fill when pass one set the barry flag, barryStop marks a pass two stop on !=
	barryTok  int
	barryStop bool
}

// Reports whether CPython's generated parser overflows its 6000 rule stack, skipping sources the proved bound clears
func (p *parser) cpythonStackOverflows(failed bool) bool {
	if len(p.toks) == 0 {
		return false
	}
	types := cpyTokenTypes(p.toks)
	bound := &passOneBound
	if failed {
		bound = &passTwoBound
	}
	if bound.frames(types) <= maxStack {
		return false
	}
	overflow, _ := p.runRecognizer(types)
	return overflow
}

// Runs CPython's two passes on the token types and returns the overflow and the deepest rule stack reached
func (p *parser) runRecognizer(types []int) (overflow bool, peak int) {
	r, overflow := p.recPasses(types)
	return overflow, r.peak
}

// Runs pass one and, when it fails without an error, pass two with the invalid rules as _PyPegen_run_parser
func (p *parser) recPasses(types []int) (*rec, bool) {
	r := &rec{p: p, types: types, memo: memoBuffer((len(types) + 1) * cMemoRules), barryTok: -1}
	defer memoPool.Put(&r.memo)
	ok, stopped, overflow := r.pass()
	if overflow || ok || stopped {
		return r, overflow
	}
	clear(r.memo)
	r.pos, r.level, r.invalid = 0, 0, true
	p.probing++
	defer func() { p.probing-- }()
	_, _, overflow = r.pass()
	return r, overflow
}

var memoPool sync.Pool

// Cleared memo table of n entries, reusing a pooled buffer so large recognizer runs skip allocation
func memoBuffer(n int) []int32 {
	if b, ok := memoPool.Get().(*[]int32); ok && cap(*b) >= n {
		m := (*b)[:n]
		clear(m)
		return m
	}
	return make([]int32, n)
}

func cpyTokenTypes(toks []token) []int {
	types := make([]int, len(toks))
	for i := range toks {
		types[i] = cpyTokenType(&toks[i])
	}
	return types
}

func cpyTokenType(t *token) int {
	switch t.kind {
	case tEnd:
		return cENDMARKER
	case tName:
		if k, ok := cpyKeywords[t.val]; ok {
			return k
		}
		return cNAME
	case tNumber:
		return cNUMBER
	case tString:
		return cSTRING
	case tNewline:
		return cNEWLINE
	case tIndent:
		return cINDENT
	case tDedent:
		return cDEDENT
	case tOp:
		return cpyOps[t.val]
	case tFStart:
		return cFSTART
	case tFMiddle:
		return cFMIDDLE
	case tFEnd:
		return cFEND
	case tTStart:
		return cTSTART
	case tTMiddle:
		return cTMIDDLE
	case tTEnd:
		return cTEND
	case tTypeComment:
		return cTYPECOMMENT
	}
	return -1
}

// Runs the start rule of the mode as _PyPegen_parse does and classifies how the pass ended
func (r *rec) pass() (ok, stopped, overflow bool) {
	defer func() {
		if x := recover(); x != nil {
			s, isStop := x.(recStop)
			if !isStop {
				panic(x)
			}
			ok, stopped, overflow = false, true, s.overflow
		}
	}()
	switch r.p.mode {
	case "eval":
		return r.eval_rule(), false, false
	case "single":
		return r.interactive_rule(), false, false
	case "func_type":
		return r.func_type_rule(), false, false
	}
	return r.file_rule(), false, false
}

func (r *rec) enter() {
	if r.level == maxStack {
		panic(recStop{overflow: true})
	}
	r.level++
	r.peak = max(r.peak, r.level)
}

func (r *rec) raise() bool { panic(recStop{}) }

// Requests the current token as _PyPegen_fill_token does, a tokenizer error ends the pass
func (r *rec) fillCur() {
	if r.pos < r.filled {
		return
	}
	r.fillNew()
	r.filled = r.pos + 1
}

func (r *rec) fillNew() {
	defer func() {
		if x := recover(); x != nil {
			if _, isErr := x.(*SyntaxError); isErr {
				panic(recStop{})
			}
			panic(x)
		}
	}()
	r.p.tk(r.pos)
}

// Type of the current token, filling it first when the pass has not fetched it yet
func (r *rec) typ() int {
	if i := r.pos; i < r.filled && i < len(r.types) {
		return r.types[i]
	}
	return r.typFill()
}

func (r *rec) typFill() int {
	r.fillCur()
	if r.pos >= len(r.types) {
		return cENDMARKER
	}
	return r.types[r.pos]
}

func (r *rec) memoGet(rule int) (ok, hit bool) {
	if r.pos >= r.filled {
		r.fillCur()
	}
	v := r.memo[r.pos*cMemoRules+rule]
	switch {
	case v > 0:
		r.pos = int(v) - 1
		return true, true
	case v < 0:
		r.pos = int(-v) - 1
		return false, true
	}
	return false, false
}

func (r *rec) memoPut(mark, rule int, ok bool, end int) {
	v := int32(end + 1)
	if !ok {
		v = -v
	}
	r.memo[mark*cMemoRules+rule] = v
}

func (r *rec) tok(t int) bool {
	if r.typ() != t {
		return false
	}
	r.pos++
	return true
}

func (r *rec) forced(t int) bool {
	if r.tok(t) {
		return true
	}
	return r.raise()
}

func (r *rec) soft(kw string) bool {
	if r.typ() != cNAME || r.p.toks[r.pos].val != kw {
		return false
	}
	r.pos++
	return true
}

func (r *rec) anySoft() bool {
	if r.typ() != cNAME || !cpySoftKeywords[r.p.toks[r.pos].val] {
		return false
	}
	r.pos++
	return true
}

// Matches NUMBER and converts it, a conversion error ends the pass as _PyPegen_number_token does
func (r *rec) number() bool {
	if r.typ() != cNUMBER {
		return false
	}
	t := &r.p.toks[r.pos]
	r.pos++
	if r.sub(func() { r.p.numberValue(t) }) {
		return r.raise()
	}
	return true
}

func (r *rec) nameTok() bool   { return r.tok(cNAME) }
func (r *rec) stringTok() bool { return r.tok(cSTRING) }

func (r *rec) look(positive bool, f func() bool) bool {
	m := r.pos
	ok := f()
	r.pos = m
	return ok == positive
}

func (r *rec) lookTok(positive bool, t int) bool {
	m := r.pos
	ok := r.tok(t)
	r.pos = m
	return ok == positive
}

func (r *rec) lookSoft(positive bool, kw string) bool {
	m := r.pos
	ok := r.soft(kw)
	r.pos = m
	return ok == positive
}

func (r *rec) cut(c *bool) bool {
	*c = true
	return true
}

func (r *rec) at(s *int) bool {
	*s = r.pos
	return true
}

// Runs a Go parser step at saved state and reports whether it raised a SyntaxError
func (r *rec) sub(f func()) (raised bool) {
	p := r.p
	pos, fill, seen, level, cond, barry := p.pos, p.fill, p.seen, p.level, p.inCondition, p.barry
	p.barry = r.barry
	defer func() {
		p.pos, p.fill, p.seen, p.level, p.inCondition, p.barry = pos, fill, seen, level, cond, barry
		if x := recover(); x != nil {
			switch x.(type) {
			case *SyntaxError, *LimitError, failSignal:
				raised = true
			default:
				panic(x)
			}
		}
	}()
	f()
	return false
}

func (r *rec) hVersion(minor int) bool {
	if r.p.fv < minor {
		return r.raise()
	}
	return true
}

// Fails silently on <> without the barry_as_FLUFL future and raises on != with it, as _PyPegen_check_barry_as_flufl
func (r *rec) hBarry(i int) bool {
	ne := r.p.toks[i].val == "<>"
	if r.barry && !ne {
		r.barryStop = r.invalid
		return r.raise()
	}
	return r.barry || !ne
}

// Sets the barry_as_FLUFL flag for an absolute from __future__ import naming it
func (r *rec) hFuture(start int) bool {
	t := r.p.toks
	if start+2 >= len(t) || t[start+1].kind != tName || normIdent(t[start+1].val) != "__future__" || t[start+2].val != "import" {
		return true
	}
	for j := start + 3; j < r.pos; j++ {
		if t[j].kind == tName && normIdent(t[j].val) == "barry_as_FLUFL" && t[j-1].val != "as" && !r.barry {
			r.barry, r.barryTok = true, r.filled
		}
	}
	return true
}

func (r *rec) adjacent(i, j int) bool {
	a, b := &r.p.toks[i], &r.p.toks[j]
	return a.el == b.l && a.ec == b.c
}

func (r *rec) hAdjacent(i, j int) bool {
	if r.adjacent(i, j) {
		return r.raise()
	}
	return false
}

func (r *rec) legacyName(start, end int) bool {
	t := &r.p.toks[start]
	return end-start == 1 && r.types[start] == cNAME && (t.val == "print" || t.val == "exec")
}

func (r *rec) hLegacy(start, end int) bool {
	if r.legacyName(start, end) {
		return r.raise()
	}
	return false
}

// Raises the forgot a comma error unless the expression is print or exec or ends outside brackets
func (r *rec) hForgotComma(start, end int) bool {
	if r.legacyName(start, end) || r.p.toks[r.pos-1].level == 0 {
		return false
	}
	return r.raise()
}

func (r *rec) hInvalidTarget(kind, start, end int) bool {
	var bad *Node
	if r.sub(func() {
		r.p.pos = start
		if e := r.p.starExpressions(); r.p.pos == end {
			bad = invalidTarget(e, kind)
		}
	}) || bad != nil {
		return r.raise()
	}
	return false
}

// Raises when the call arguments before a bare generator hold more than one positional argument
func (r *rec) hGenexpCall(start, end int) bool {
	t, depth, n, item := r.p.toks, 0, 0, true
	for j := start; j < end; j++ {
		if depth == 0 && item {
			kw := t[j].kind == tOp && t[j].val == "**" || t[j].kind == tName && j+1 < end && t[j+1].kind == tOp && t[j+1].val == "="
			if !kw {
				n++
			}
			item = false
		}
		if t[j].kind == tOp {
			switch t[j].val {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			case ",":
				item = depth == 0
			}
		}
	}
	if n <= 1 {
		return false
	}
	return r.raise()
}

func (r *rec) hEnsure(imaginary bool, i int) bool {
	v := r.p.toks[i].val
	if (strings.HasSuffix(v, "j") || strings.HasSuffix(v, "J")) != imaginary {
		return r.raise()
	}
	return true
}

func (r *rec) hStrings(start int) bool {
	if r.sub(func() {
		r.p.pos = start
		r.p.strings()
	}) {
		return r.raise()
	}
	return true
}

// Raises unless the conversion name directly follows the exclamation mark and is s, r or a
func (r *rec) hConversion(bang, name int) bool {
	v := r.p.identifier(r.p.toks[name].val)
	if !r.adjacent(bang, name) || (v != "s" && v != "r" && v != "a") {
		return r.raise()
	}
	return true
}
