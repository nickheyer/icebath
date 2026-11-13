package python

// Token type number of each keyword in the reference grammar
var gramKeywords = map[string]int{"if": 682, "as": 680, "in": 695, "or": 588, "is": 596, "del": 625, "def": 699, "for": 694, "try": 656, "and": 589, "not": 703, "from": 633, "pass": 526, "with": 647, "elif": 687, "else": 686, "None": 623, "True": 622, "raise": 525, "yield": 587, "break": 527, "async": 698, "class": 701, "while": 689, "False": 624, "await": 597, "return": 522, "import": 634, "assert": 532, "global": 529, "except": 677, "lambda": 621, "finally": 673, "continue": 528, "nonlocal": 530}

// Names the reference grammar reads as soft keywords
var gramSoftKeywords = map[string]bool{"_": true, "case": true, "match": true, "type": true}

// Token type number of each operator in the reference grammar
var gramOps = map[string]int{"!": 54, "!=": 28, "%": 24, "%=": 40, "&": 19, "&=": 41, "(": 7, ")": 8, "*": 16, "**": 35, "**=": 46, "*=": 38, "+": 14, "+=": 36, ",": 12, "-": 15, "-=": 37, "->": 51, ".": 23, "...": 52, "/": 17, "//": 47, "//=": 48, "/=": 39, ":": 11, ":=": 53, ";": 13, "<": 20, "<<": 33, "<<=": 44, "<=": 29, "<>": 28, "=": 22, "==": 27, ">": 21, ">=": 30, ">>": 34, ">>=": 45, "@": 49, "@=": 50, "[": 9, "]": 10, "^": 32, "^=": 43, "{": 25, "|": 18, "|=": 42, "}": 26, "~": 31}

// Token type numbers of the reference grammar and the count of memoized rules
const (
	cENDMARKER   = 0
	cNAME        = 1
	cNUMBER      = 2
	cSTRING      = 3
	cNEWLINE     = 4
	cINDENT      = 5
	cDEDENT      = 6
	cTYPECOMMENT = 57
	cFSTART      = 59
	cFMIDDLE     = 60
	cFEND        = 61
	cTSTART      = 62
	cTMIDDLE     = 63
	cTEND        = 64
	cMemoRules   = 29
)

// Replica of the reference parser over token types, counting rule frames against the reference stack bound
type rec struct {
	p         *parser
	types     []int
	pos       int
	level     int
	invalid   bool
	barry     bool
	memo      []int32
	filled    int
	stopped   bool
	overflow  bool
	barryTok  int
	barryStop bool
	eval      bool
}

// Reports whether the reference parser overflows its rule stack on these tokens, skipping sources the proved bound clears
func (p *parser) stackOverflows(failed, eval bool) bool {
	if len(p.toks) == 0 {
		return false
	}
	types := gramTokenTypes(p.toks)
	bound := &passOneBound
	if failed {
		bound = &passTwoBound
	}
	if bound.frames(types) <= maxStack {
		return false
	}
	_, overflow := p.recPasses(types, eval)
	return overflow
}

// Runs pass one and, when it fails without stopping, pass two with the invalid rules as the reference does
func (p *parser) recPasses(types []int, eval bool) (*rec, bool) {
	r := &rec{p: p, types: types, memo: make([]int32, (len(types)+1)*cMemoRules), barryTok: -1, eval: eval}
	ok := r.pass()
	if r.overflow || ok || r.stopped {
		return r, r.overflow
	}
	clear(r.memo)
	r.pos, r.level, r.invalid = 0, 0, true
	r.pass()
	return r, r.overflow
}

func gramTokenTypes(toks []token) []int {
	types := make([]int, len(toks))
	for i := range toks {
		types[i] = gramTokenType(&toks[i])
	}
	return types
}

func gramTokenType(t *token) int {
	switch t.kind {
	case tEnd:
		return cENDMARKER
	case tName:
		if k, ok := gramKeywords[t.val]; ok {
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
		return gramOps[t.val]
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
	}
	return -1
}

// Runs the start rule of the input kind and reports success, the stopped and overflow flags telling how it ended
func (r *rec) pass() bool {
	var ok bool
	if r.eval {
		ok = r.evalRule()
	} else {
		ok = r.fileRule()
	}
	return ok && !r.stopped
}

// Pushes one rule frame, stopping the pass with an overflow when the frame would pass the bound
func (r *rec) enter() bool {
	if r.stopped {
		return false
	}
	if r.level == maxStack {
		r.stopped, r.overflow = true, true
		return false
	}
	r.level++
	return true
}

// Ends the pass as a raised syntax error ends the reference parser
func (r *rec) raise() bool {
	r.stopped = true
	return false
}

// Requests the current token, a tokenizer error or unclosed bracket there ending the pass
func (r *rec) fillCur() {
	if r.stopped || r.pos < r.filled {
		return
	}
	toks, lx := r.p.toks, r.p.lx
	i := r.pos
	if i >= len(toks) {
		if lx.err != nil {
			r.stopped = true
			return
		}
		i = len(toks) - 1
	}
	if toks[i].kind == tError {
		r.stopped = true
		return
	}
	r.filled = r.pos + 1
}

// Type of the current token, filling it first when the pass has not fetched it yet
func (r *rec) typ() int {
	if i := r.pos; i < r.filled && i < len(r.types) {
		return r.types[i]
	}
	r.fillCur()
	if r.pos >= len(r.types) {
		return cENDMARKER
	}
	return r.types[r.pos]
}

func (r *rec) memoGet(rule int) (ok, hit bool) {
	if r.stopped {
		return false, true
	}
	if r.pos >= r.filled {
		if r.fillCur(); r.stopped {
			return false, true
		}
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
	if r.stopped || r.typ() != t || r.stopped {
		return false
	}
	r.pos++
	return true
}

// Token that must follow, its absence raising as the double ampersand grammar operator does
func (r *rec) forced(t int) bool {
	if r.tok(t) {
		return true
	}
	return r.raise()
}

func (r *rec) soft(kw string) bool {
	if r.stopped || r.typ() != cNAME || r.stopped || r.p.toks[r.pos].val != kw {
		return false
	}
	r.pos++
	return true
}

func (r *rec) anySoft() bool {
	if r.stopped || r.typ() != cNAME || r.stopped || !gramSoftKeywords[r.p.toks[r.pos].val] {
		return false
	}
	r.pos++
	return true
}

// Matches a numeric literal token and converts it, a conversion error ending the pass
func (r *rec) number() bool {
	if r.stopped || r.typ() != cNUMBER || r.stopped {
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
	if r.stopped {
		return false
	}
	m := r.pos
	ok := f()
	r.pos = m
	return !r.stopped && ok == positive
}

func (r *rec) lookTok(positive bool, t int) bool {
	if r.stopped {
		return false
	}
	m := r.pos
	ok := r.tok(t)
	r.pos = m
	return !r.stopped && ok == positive
}

func (r *rec) lookSoft(positive bool, kw string) bool {
	if r.stopped {
		return false
	}
	m := r.pos
	ok := r.soft(kw)
	r.pos = m
	return !r.stopped && ok == positive
}

func (r *rec) cut(c *bool) bool {
	*c = true
	return true
}

func (r *rec) at(s *int) bool {
	*s = r.pos
	return true
}

// Runs a parser step at saved parser state and reports whether it raised or failed
func (r *rec) sub(f func()) bool {
	p := r.p
	pos, fill, level, cond, barry, perr, failed := p.pos, p.fill, p.level, p.inCondition, p.barry, p.err, p.failed
	p.barry, p.err, p.failed = r.barry, nil, false
	f()
	raised := p.err != nil || p.failed
	p.pos, p.fill, p.level, p.inCondition, p.barry, p.err, p.failed = pos, fill, level, cond, barry, perr, failed
	return raised
}

// Fails quietly on <> without the barry_as_FLUFL future and raises on != with it
func (r *rec) hBarry(i int) bool {
	ne := r.p.toks[i].val == "<>"
	if r.barry && !ne {
		r.barryStop = r.invalid
		return r.raise()
	}
	return r.barry || !ne
}

// Sets the barry_as_FLUFL flag for an absolute from __future__ import naming it, recording the fill at that moment
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

// Raises when the star expressions between start and end hold an invalid target of the kind
func (r *rec) hInvalidTarget(kind, start, end int) bool {
	var bad Expr
	if r.sub(func() {
		r.p.pos = start
		if e := r.p.starExpressions(); !r.p.bad() && r.p.pos == end {
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

// Raises unless the number token is imaginary exactly when an imaginary number is required
func (r *rec) hEnsure(imaginary bool, i int) bool {
	v := r.p.toks[i].val
	if (v[len(v)-1] == 'j' || v[len(v)-1] == 'J') != imaginary {
		return r.raise()
	}
	return true
}

// Raises when the string literals from start fail to decode or concatenate
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
	v := normIdent(r.p.toks[name].val)
	if !r.adjacent(bang, name) || (v != "s" && v != "r" && v != "a") {
		return r.raise()
	}
	return true
}

// Frames the reference parser can stack per open bracket, open block and credited token
type recBound struct {
	base, bracket, indent, local, global int
	localToks, globalToks                map[int]bool
}

// Pass one bound, covering every parse that ends in a tree because the reference pass one then succeeds
var passOneBound = recBound{base: 55, bracket: 39, indent: 8, local: 8, global: 1,
	localToks:  map[int]bool{14: true, 15: true, 16: true, 31: true, 35: true, 621: true, 686: true, 703: true},
	globalToks: map[int]bool{687: true}}

// Bound over both passes with the invalid rules, used when the parse ended in a syntax error
var passTwoBound = recBound{base: 62, bracket: 43, indent: 10, local: 14, global: 1,
	localToks:  map[int]bool{cNAME: true, 14: true, 15: true, 16: true, 31: true, 35: true, 621: true, 686: true, 703: true},
	globalToks: map[int]bool{687: true}}

func recOpener(t int) bool { return t == 7 || t == 9 || t == 25 || t == cFSTART || t == cTSTART }
func recCloser(t int) bool { return t == 8 || t == 10 || t == 26 || t == cFEND || t == cTEND }

// Deepest rule stack these tokens can reach, line local tokens reset at each newline
func (b *recBound) frames(types []int) int {
	var localTok, globalTok [1024]bool
	for t := range b.localToks {
		localTok[t] = true
	}
	for t := range b.globalToks {
		globalTok[t] = true
	}
	depth, indent, local, global, worst := 0, 0, 0, 0, 0
	for _, t := range types {
		switch {
		case recOpener(t):
			depth++
		case recCloser(t):
			depth--
		case t == cINDENT:
			indent++
		case t == cDEDENT:
			indent--
		case t == cNEWLINE:
			local = 0
		}
		if uint(t) < uint(len(localTok)) {
			if localTok[t] {
				local++
			}
			if globalTok[t] {
				global++
			}
		}
		worst = max(worst, b.bracket*depth+b.indent*indent+b.local*local+b.global*global)
	}
	return b.base + worst
}

// Reports whether a barry_as_FLUFL future import can change how the reference lexes or parses the tokens
func (p *parser) barryRelevant() bool {
	return p.lx.barryAt != -1 || p.lx.barryTok >= 0 || p.barryTok >= 0
}

// After a failed parse, records the pass one barry fill and returns the != error the reference pass two raises
func (p *parser) barryErrorPass(eval bool) error {
	pos, fill, level, barry, perr, failed := p.pos, p.fill, p.level, p.barry, p.err, p.failed
	defer func() {
		p.pos, p.fill, p.level, p.barry, p.err, p.failed = pos, fill, level, barry, perr, failed
	}()
	r, overflow := p.recPasses(gramTokenTypes(p.toks), eval)
	p.recBarryTok, p.barryChecked = r.barryTok, true
	if overflow || !r.barryStop {
		return nil
	}
	p.fill, p.err, p.failed = r.filled, nil, false
	p.raiseOnToken(ClassSyntax, &p.toks[r.filled-1], "with Barry as BDFL, use '<>' instead of '!='")
	se, ok := p.err.(*SyntaxError)
	if !ok {
		return p.err
	}
	return p.fullTokenize(se)
}

// Reports whether lexing with <> enabled from token a instead of b changes a token, -1 meaning never
func barryLexDiffers(toks []token, a, b int) bool {
	if a == b {
		return false
	}
	if a < 0 || b >= 0 && b < a {
		a, b = b, a
	}
	if b < 0 {
		b = len(toks)
	}
	for i := max(a, 0); i < b && i < len(toks); i++ {
		t := &toks[i]
		if t.kind != tOp {
			continue
		}
		if t.val == "<>" {
			return true
		}
		if t.val == "<" && i+1 < len(toks) {
			if n := &toks[i+1]; n.kind == tOp && n.val == ">" && n.l == t.el && n.c == t.ec {
				return true
			}
		}
	}
	return false
}
