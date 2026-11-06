package pyast

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Bytes of C stack CPython 3.14.8 spends per frame of the ast_preprocess.c and Python-ast.c recursions on this host
const (
	foldModFrame     = 128
	foldBodyFrame    = 80
	foldStmtFrame    = 96
	foldExprFrame    = 160
	foldPatternFrame = 80
	foldCompFrame    = 64
	foldArgsFrame    = 64
	foldSmallFrame   = 48
	objFrame         = 64
	objSmallFrame    = 48
	objListFrame     = 64
	// foldLevelMax is the most stack one tree level takes in the fold walk, a statement in a body
	foldLevelMax = foldStmtFrame + foldBodyFrame
)

type stackModel struct{ foldLimit, objLimit, kB int }

// The frozen reference invocation, a script file run whose chains of 52153 additions and 130384 attributes still parse
var cpythonStack = stackModel{foldLimit: 8344720, objLimit: 8344848, kB: 8152}

type recursionStop struct{}

// Applies future checks, the preprocess pass and the ast2obj recursion check ast.parse runs on a parsed tree
func compileAST(mod *Node, o Options, m stackModel, tokens int) error {
	annotations, err := futureFeatures(mod, o.filename())
	if err != nil {
		return err
	}
	levels := 2*tokens + 4
	f := &folder{optimize: max(o.Optimize, 0), checkOnly: o.Optimize <= 0, annotations: annotations, limit: m.foldLimit}
	if (!f.checkOnly || foldModFrame+levels*foldLevelMax >= m.foldLimit) && !recursesWithin(func() { f.mod(mod) }) {
		return &LimitError{Class: "RecursionError", Msg: fmt.Sprintf("Stack overflow (used %d kB) during compilation", m.kB)}
	}
	w := objWalker{limit: m.objLimit}
	if levels*(objFrame+objListFrame) >= m.objLimit && !recursesWithin(func() { w.node(mod, 0) }) {
		return &LimitError{Class: "RecursionError", Msg: fmt.Sprintf("Stack overflow (used %d kB)during  ast construction", m.kB)}
	}
	return nil
}

func recursesWithin(walk func()) (ok bool) {
	defer func() {
		if x := recover(); x != nil {
			if _, stop := x.(recursionStop); !stop {
				panic(x)
			}
			ok = false
		}
	}()
	walk()
	return true
}

type folder struct {
	optimize    int
	checkOnly   bool
	annotations bool
	limit       int
	seen        func(n *Node, d int)
}

// Raises the RecursionError once a frame whose address check sits d bytes down reaches the soft stack limit
func (f *folder) check(d int) {
	if d >= f.limit {
		panic(recursionStop{})
	}
}

func (f *folder) mod(n *Node) {
	switch n.Type {
	case "Module":
		n.Set("body", f.body(n.Children("body"), foldModFrame))
	case "Interactive":
		f.stmts(n.Children("body"), foldModFrame)
	case "Expression":
		f.expr(n.Child("body"), foldModFrame)
	}
}

// Drops or replaces the docstring at optimize 2, folds the statements and keeps a new first string from becoming a docstring
func (f *folder) body(stmts []*Node, d int) []*Node {
	doc := docstringOf(stmts)
	if doc && f.optimize >= 2 {
		if len(stmts) == 1 {
			s := stmts[0]
			pass := NewNode("Pass")
			pass.Lineno, pass.ColOffset, pass.EndLineno, pass.EndColOffset = s.Lineno, s.ColOffset, s.Lineno, s.ColOffset+4
			stmts = []*Node{pass}
		} else {
			stmts = stmts[1:]
		}
		doc = false
	}
	f.stmts(stmts, d+foldBodyFrame)
	if !doc && docstringOf(stmts) {
		s := stmts[0]
		js := NewNode("JoinedStr", []*Node{s.Child("value")})
		js.Lineno, js.ColOffset, js.EndLineno, js.EndColOffset = s.Lineno, s.ColOffset, s.EndLineno, s.EndColOffset
		s.Set("value", js)
	}
	return stmts
}

func (f *folder) stmts(list []*Node, d int) {
	for _, s := range list {
		f.stmt(s, d)
	}
}

func (f *folder) exprs(list []*Node, d int) {
	for _, e := range list {
		if e != nil {
			f.expr(e, d)
		}
	}
}

func (f *folder) optExpr(e *Node, d int) {
	if e != nil {
		f.expr(e, d)
	}
}

// Folds one statement, children in tail position reuse the frame as GCC's sibling calls do
func (f *folder) stmt(n *Node, d int) {
	f.check(d)
	if f.seen != nil {
		f.seen(n, d)
	}
	c := d + foldStmtFrame
	switch n.Type {
	case "FunctionDef", "AsyncFunctionDef":
		f.typeParams(n.Children("type_params"), c)
		f.arguments(n.Child("args"), c)
		n.Set("body", f.body(n.Children("body"), c))
		f.exprs(n.Children("decorator_list"), c)
		if !f.annotations {
			f.optExpr(n.Child("returns"), d)
		}
	case "ClassDef":
		f.typeParams(n.Children("type_params"), c)
		f.exprs(n.Children("bases"), c)
		for _, k := range n.Children("keywords") {
			f.expr(k.Child("value"), c)
		}
		n.Set("body", f.body(n.Children("body"), c))
		f.exprs(n.Children("decorator_list"), c)
	case "Return":
		f.optExpr(n.Child("value"), d)
	case "Delete":
		f.exprs(n.Children("targets"), c)
	case "Assign":
		f.exprs(n.Children("targets"), c)
		f.expr(n.Child("value"), d)
	case "AugAssign":
		f.expr(n.Child("target"), c)
		f.expr(n.Child("value"), d)
	case "AnnAssign":
		f.expr(n.Child("target"), c)
		if !f.annotations {
			f.expr(n.Child("annotation"), c)
		}
		f.optExpr(n.Child("value"), d)
	case "TypeAlias":
		f.expr(n.Child("name"), c)
		f.typeParams(n.Children("type_params"), c)
		f.expr(n.Child("value"), d)
	case "For", "AsyncFor":
		f.expr(n.Child("target"), c)
		f.expr(n.Child("iter"), c)
		f.stmts(n.Children("body"), c)
		f.stmts(n.Children("orelse"), c)
	case "While", "If":
		f.expr(n.Child("test"), c)
		f.stmts(n.Children("body"), c)
		f.stmts(n.Children("orelse"), c)
	case "With", "AsyncWith":
		for _, it := range n.Children("items") {
			f.expr(it.Child("context_expr"), c+foldSmallFrame)
			f.optExpr(it.Child("optional_vars"), c)
		}
		f.stmts(n.Children("body"), c)
	case "Raise":
		f.optExpr(n.Child("exc"), c)
		f.optExpr(n.Child("cause"), d)
	case "Try", "TryStar":
		f.stmts(n.Children("body"), c)
		for _, h := range n.Children("handlers") {
			f.optExpr(h.Child("type"), c+foldSmallFrame)
			f.stmts(h.Children("body"), c+foldSmallFrame)
		}
		f.stmts(n.Children("orelse"), c)
		f.stmts(n.Children("finalbody"), c)
	case "Assert":
		f.expr(n.Child("test"), c)
		f.optExpr(n.Child("msg"), d)
	case "Expr":
		f.expr(n.Child("value"), d)
	case "Match":
		f.expr(n.Child("subject"), c)
		for _, mc := range n.Children("cases") {
			f.pattern(mc.Child("pattern"), c)
			f.optExpr(mc.Child("guard"), c)
			f.stmts(mc.Children("body"), c)
		}
	}
}

// Folds bounds and defaults of type parameters, a default is the tail call of astfold_type_param
func (f *folder) typeParams(list []*Node, d int) {
	for _, tp := range list {
		if tp.Is("TypeVar") {
			f.optExpr(tp.Child("bound"), d+foldSmallFrame)
		}
		f.optExpr(tp.Child("default_value"), d)
	}
}

func (f *folder) arguments(a *Node, d int) {
	c := d + foldArgsFrame
	for _, field := range []string{"posonlyargs", "args", "vararg", "kwonlyargs", "kw_defaults", "kwarg", "defaults"} {
		switch v := a.Get(field).(type) {
		case *Node:
			f.argument(v, field, c)
		case []*Node:
			for _, x := range v {
				f.argument(x, field, c)
			}
		}
	}
}

func (f *folder) argument(x *Node, field string, d int) {
	switch {
	case x == nil:
	case field == "kw_defaults" || field == "defaults":
		f.expr(x, d)
	case !f.annotations:
		f.optExpr(x.Child("annotation"), d)
	}
}

// Folds one expression, the operand of a tail position loops in the same frame as GCC's tail recursion does
func (f *folder) expr(n *Node, d int) {
	f.check(d)
	if f.seen != nil {
		f.seen(n, d)
	}
	c := d + foldExprFrame
	switch n.Type {
	case "BoolOp":
		f.exprs(n.Children("values"), c)
	case "BinOp":
		f.expr(n.Child("left"), c)
		f.expr(n.Child("right"), c)
		f.foldBinop(n)
	case "UnaryOp":
		f.expr(n.Child("operand"), d)
	case "Lambda":
		f.arguments(n.Child("args"), c)
		f.expr(n.Child("body"), d)
	case "IfExp":
		f.expr(n.Child("test"), c)
		f.expr(n.Child("body"), c)
		f.expr(n.Child("orelse"), d)
	case "Dict":
		f.exprs(n.Children("keys"), c)
		f.exprs(n.Children("values"), c)
	case "Set", "List", "Tuple":
		f.exprs(n.Children("elts"), c)
	case "ListComp", "SetComp", "GeneratorExp", "DictComp":
		if n.Type == "DictComp" {
			f.expr(n.Child("key"), c)
			f.expr(n.Child("value"), c)
		} else {
			f.expr(n.Child("elt"), c)
		}
		for _, g := range n.Children("generators") {
			f.expr(g.Child("target"), c+foldCompFrame)
			f.expr(g.Child("iter"), c+foldCompFrame)
			f.exprs(g.Children("ifs"), c+foldCompFrame)
		}
	case "Await", "YieldFrom", "Yield", "Attribute", "Starred", "NamedExpr":
		f.optExpr(n.Child("value"), d)
	case "Compare":
		f.expr(n.Child("left"), c)
		f.exprs(n.Children("comparators"), c)
	case "Call":
		f.expr(n.Child("func"), c)
		f.exprs(n.Children("args"), c)
		for _, k := range n.Children("keywords") {
			f.expr(k.Child("value"), c)
		}
	case "FormattedValue", "Interpolation":
		f.expr(n.Child("value"), c)
		f.optExpr(n.Child("format_spec"), d)
	case "JoinedStr", "TemplateStr":
		f.exprs(n.Children("values"), c)
	case "Subscript":
		f.expr(n.Child("value"), c)
		f.expr(n.Child("slice"), d)
	case "Slice":
		f.optExpr(n.Child("lower"), c)
		f.optExpr(n.Child("upper"), c)
		f.optExpr(n.Child("step"), d)
	case "Name":
		if !f.checkOnly && n.Child("ctx").Is("Load") {
			if id, _ := n.Str("id"); id == "__debug__" {
				n.becomeConstant(f.optimize == 0)
			}
		}
	}
}

// Folds patterns, whose recursion check runs one frame down inside an outlined Py_EnterRecursiveCall clone
func (f *folder) pattern(n *Node, d int) {
	f.check(d + foldPatternFrame)
	if f.seen != nil {
		f.seen(n, d+foldPatternFrame)
	}
	c := d + foldPatternFrame
	switch n.Type {
	case "MatchValue":
		f.foldMatchConst(n.Child("value"))
	case "MatchSequence", "MatchOr":
		f.patterns(n.Children("patterns"), c)
	case "MatchMapping":
		for _, k := range n.Children("keys") {
			f.foldMatchConst(k)
		}
		f.patterns(n.Children("patterns"), c)
	case "MatchClass":
		f.expr(n.Child("cls"), c)
		f.patterns(n.Children("patterns"), c)
		f.patterns(n.Children("kwd_patterns"), c)
	case "MatchAs":
		if p := n.Child("pattern"); p != nil {
			f.pattern(p, d)
		}
	}
}

func (f *folder) patterns(list []*Node, d int) {
	for _, p := range list {
		f.pattern(p, d)
	}
}

// Turns the node into a Constant without kind keeping its location as make_const does
func (n *Node) becomeConstant(v any) {
	n.Type, n.spec, n.vals = "Constant", specs["Constant"], []any{v, nil}
}

// Folds negative and complex number literals of match values and mapping keys
func (f *folder) foldMatchConst(n *Node) {
	if f.checkOnly {
		return
	}
	switch n.Type {
	case "UnaryOp":
		if operand := n.Child("operand"); n.Child("op").Is("USub") && operand.Is("Constant") {
			if v, ok := numberNegative(operand.Get("value")); ok {
				n.becomeConstant(v)
			}
		}
	case "BinOp":
		op := n.Child("op")
		if right := n.Child("right"); op.Is("Add", "Sub") && right.Is("Constant") {
			f.foldMatchConst(n.Child("left"))
			if left := n.Child("left"); left.Is("Constant") {
				if v, ok := numberAddSub(left.Get("value"), right.Get("value"), op.Is("Sub")); ok {
					n.becomeConstant(v)
				}
			}
		}
	}
}

func numberNegative(v any) (any, bool) {
	switch x := v.(type) {
	case *big.Int:
		return new(big.Int).Neg(x), true
	case float64:
		return -x, true
	case Complex:
		return Complex{-x.Real, -x.Imag}, true
	}
	return nil, false
}

// Converts an int or float to a double as PyLong_AsDouble does, failing past the float range
func realDouble(v any) (float64, bool) {
	switch x := v.(type) {
	case *big.Int:
		f, acc := new(big.Float).SetInt(x).Float64()
		if (f > 1.7976931348623157e308 || f < -1.7976931348623157e308) && acc != big.Exact {
			return 0, false
		}
		return f, true
	case float64:
		return x, true
	}
	return 0, false
}

// Adds or subtracts numbers with the 3.14 mixed real and complex rules, false where Python raises
func numberAddSub(l, r any, sub bool) (any, bool) {
	sign := 1.0
	if sub {
		sign = -1
	}
	lc, lIsC := l.(Complex)
	rc, rIsC := r.(Complex)
	switch {
	case lIsC && rIsC:
		return Complex{lc.Real + sign*rc.Real, lc.Imag + sign*rc.Imag}, true
	case rIsC:
		x, ok := realDouble(l)
		if !ok {
			return nil, false
		}
		if sub {
			return Complex{x - rc.Real, -rc.Imag}, true
		}
		return Complex{x + rc.Real, rc.Imag}, true
	case lIsC:
		y, ok := realDouble(r)
		if !ok {
			return nil, false
		}
		if sub {
			return Complex{lc.Real - y, lc.Imag}, true
		}
		return Complex{lc.Real + y, lc.Imag}, true
	}
	li, lInt := l.(*big.Int)
	ri, rInt := r.(*big.Int)
	if lInt && rInt {
		if sub {
			return new(big.Int).Sub(li, ri), true
		}
		return new(big.Int).Add(li, ri), true
	}
	x, okx := realDouble(l)
	y, oky := realDouble(r)
	if !okx || !oky {
		return nil, false
	}
	if sub {
		return x - y, true
	}
	return x + y, true
}

// Rewrites str % tuple without starred items into a JoinedStr when every unit is a simple s, r or a format
func (f *folder) foldBinop(n *Node) {
	if f.checkOnly {
		return
	}
	lhs, rhs := n.Child("left"), n.Child("right")
	if !lhs.Is("Constant") || !n.Child("op").Is("Mod") || !rhs.Is("Tuple") {
		return
	}
	format, ok := lhs.Get("value").(string)
	if !ok {
		return
	}
	elts := rhs.Children("elts")
	for _, e := range elts {
		if e.Is("Starred") {
			return
		}
	}
	fmtRunes := pystr.Runes(format)
	pos, cnt := 0, 0
	var seq []*Node
	for {
		if lit := formatLiteral(fmtRunes, &pos); lit != nil {
			seq = append(seq, lit)
		}
		if pos >= len(fmtRunes) {
			break
		}
		if cnt >= len(elts) {
			return
		}
		pos++
		unit := formatUnit(fmtRunes, &pos, elts[cnt])
		cnt++
		if unit == nil {
			return
		}
		seq = append(seq, unit)
	}
	if cnt < len(elts) {
		return
	}
	n.Type, n.spec, n.vals = "JoinedStr", specs["JoinedStr"], []any{seq}
}

func unlocatedConstant(s string) *Node {
	c := NewNode("Constant", s, nil)
	c.Lineno, c.ColOffset, c.EndLineno, c.EndColOffset = -1, -1, -1, -1
	return c
}

// Takes the text up to the next lone percent with doubled percents collapsed as parse_literal does
func formatLiteral(fmt []rune, pos *int) *Node {
	start, p := *pos, *pos
	percents := false
	for p < len(fmt) {
		if fmt[p] != '%' {
			p++
		} else if p+1 < len(fmt) && fmt[p+1] == '%' {
			percents = true
			p += 2
		} else {
			break
		}
	}
	*pos = p
	if p == start {
		return nil
	}
	s := pystr.FromRunes(fmt[start:p])
	if percents {
		s = strings.ReplaceAll(s, "%%", "%")
	}
	return unlocatedConstant(s)
}

// Parses flags, a width and a precision of at most two digits and the conversion character of one unit
func simpleFormatArg(fmt []rune, pos *int) (spec rune, ljust bool, width, prec int, ok bool) {
	p := *pos
	width, prec = -1, -1
	next := func() (rune, bool) {
		if p >= len(fmt) {
			return 0, false
		}
		p++
		return fmt[p-1], true
	}
	ch, more := next()
	for more && strings.ContainsRune("-+ #0", ch) {
		if ch == '-' {
			ljust = true
		}
		ch, more = next()
	}
	if !more {
		return 0, false, 0, 0, false
	}
	digits := func(dst *int) bool {
		*dst = 0
		for n := 0; '0' <= ch && ch <= '9'; {
			*dst = *dst*10 + int(ch-'0')
			if ch, more = next(); !more {
				return false
			}
			if n++; n >= 3 {
				return false
			}
		}
		return true
	}
	if '0' <= ch && ch <= '9' && !digits(&width) {
		return 0, false, 0, 0, false
	}
	if ch == '.' {
		if ch, more = next(); !more {
			return 0, false, 0, 0, false
		}
		prec = 0
		if '0' <= ch && ch <= '9' && !digits(&prec) {
			return 0, false, 0, 0, false
		}
	}
	*pos = p
	return ch, ljust, width, prec, true
}

// Builds the FormattedValue of an s, r or a unit with its alignment, width and precision spec
func formatUnit(fmt []rune, pos *int, arg *Node) *Node {
	spec, ljust, width, prec, ok := simpleFormatArg(fmt, pos)
	if !ok || (spec != 's' && spec != 'r' && spec != 'a') {
		return nil
	}
	var buf strings.Builder
	if !ljust && width > 0 {
		buf.WriteByte('>')
	}
	if width >= 0 {
		buf.WriteString(strconv.Itoa(width))
	}
	if prec >= 0 {
		buf.WriteString("." + strconv.Itoa(prec))
	}
	var formatSpec *Node
	if buf.Len() > 0 {
		formatSpec = unlocatedConstant(buf.String())
	}
	fv := NewNode("FormattedValue", arg, int(spec), formatSpec)
	fv.Lineno, fv.ColOffset, fv.EndLineno, fv.EndColOffset = arg.Lineno, arg.ColOffset, arg.EndLineno, arg.EndColOffset
	return fv
}

type objWalker struct {
	limit int
	seen  func(n *Node, d int)
}

// Checks the ast2obj recursion over the final tree, each check one helper frame below its function
func (w objWalker) node(n *Node, d int) {
	size := objFrame
	switch n.spec.base {
	case "mod", "stmt", "expr":
	default:
		size = objSmallFrame
	}
	if d+size >= w.limit {
		panic(recursionStop{})
	}
	if w.seen != nil {
		w.seen(n, d+size)
	}
	c := d + size
	for i, fs := range n.spec.fields {
		switch fs.kind {
		case kNode, kOptNode:
			if ch, _ := n.vals[i].(*Node); objChecked(ch) {
				w.node(ch, c)
			}
		case kNodeList:
			for _, ch := range n.vals[i].([]*Node) {
				if objChecked(ch) {
					w.node(ch, c+objListFrame)
				}
			}
		}
	}
}

// Reports whether ast2obj converts the child through a recursion checked function
func objChecked(n *Node) bool {
	if n == nil {
		return false
	}
	switch n.spec.base {
	case "expr_context", "boolop", "operator", "unaryop", "cmpop":
		return false
	}
	return true
}
