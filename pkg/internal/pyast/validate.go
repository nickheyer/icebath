package pyast

import (
	"math/big"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Bytes of C stack the Python/ast.c validators take in CPython 3.14.8 on this host, the others inlined
const (
	valStmtFrame  = 64
	valExprFrame  = 48
	valConstFrame = 48
	valPatFrame   = 48
	valPatsFrame  = 48
	valExprsFrame = 48
	valSmallFrame = 32
)

type validator struct{ limit, kB int }

// Raises the RecursionError Py_EnterRecursiveCall gives once a validator frame reaches the soft limit
func (v *validator) enter(d int) error {
	if d >= v.limit {
		return pyErr("RecursionError", "Stack overflow (used %d kB) during compilation", v.kB)
	}
	return nil
}

// Checks the location attributes of a node as VALIDATE_POSITIONS
func validPositions(n *Node) error {
	if n.Lineno > n.EndLineno {
		return pyErr("ValueError", "AST node line range (%d, %d) is not valid", n.Lineno, n.EndLineno)
	}
	if n.Lineno < 0 && n.EndLineno != n.Lineno || n.ColOffset < 0 && n.ColOffset != n.EndColOffset {
		return pyErr("ValueError", "AST node column range (%d, %d) for line range (%d, %d) is not valid", n.ColOffset, n.EndColOffset, n.Lineno, n.EndLineno)
	}
	if n.Lineno == n.EndLineno && n.ColOffset > n.EndColOffset {
		return pyErr("ValueError", "line %d, column %d-%d is not a valid range", n.Lineno, n.ColOffset, n.EndColOffset)
	}
	return nil
}

func validName(name any) error {
	s, _ := name.(string)
	switch s {
	case "None", "True", "False":
		return pyErr("ValueError", "identifier field can't represent '%s' constant", s)
	}
	return nil
}

func validCapture(name any) error {
	if s, _ := name.(string); s == "_" {
		return pyErr("ValueError", "can't capture name '_' in patterns")
	}
	return validName(name)
}

func nonEmpty(n int, what, owner string) error {
	if n == 0 {
		return pyErr("ValueError", "empty %s on %s", what, owner)
	}
	return nil
}

// Validates a converted module as _PyAST_Validate
func validateMod(mod *Node) error {
	v := &validator{limit: nodeStack.validate, kB: nodeStack.kB}
	switch mod.Type {
	case "Module", "Interactive":
		return v.stmts(mod.Children("body"), 0)
	case "Expression":
		return v.expr(mod.Child("body"), "Load", 0)
	}
	if err := v.exprs(mod.Children("argtypes"), "Load", false, 0); err != nil {
		return err
	}
	return v.expr(mod.Child("returns"), "Load", 0)
}

// Validates a statement list inlined into its caller, a None entry rejected
func (v *validator) stmts(list []*Node, d int) error {
	for _, s := range list {
		if s == nil {
			return pyErr("ValueError", "None disallowed in statement list")
		}
		if err := v.stmt(s, d); err != nil {
			return err
		}
	}
	return nil
}

func (v *validator) body(list []*Node, owner string, d int) error {
	if err := nonEmpty(len(list), "body", owner); err != nil {
		return err
	}
	return v.stmts(list, d)
}

// Validates an expression list called as its own frame, a None entry allowed only where null_ok
func (v *validator) exprsCall(list []*Node, ctx string, nullOK bool, p int) error {
	return v.exprs(list, ctx, nullOK, p+valExprsFrame)
}

func (v *validator) exprs(list []*Node, ctx string, nullOK bool, d int) error {
	for _, e := range list {
		if e == nil {
			if !nullOK {
				return pyErr("ValueError", "None disallowed in expression list")
			}
			continue
		}
		if err := v.expr(e, ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (v *validator) optExpr(e *Node, d int) error {
	if e == nil {
		return nil
	}
	return v.expr(e, "Load", d)
}

// Validates arguments in its own frame with validate_args frames for the positional and keyword lists
func (v *validator) arguments(a *Node, p int) error {
	d := p + valSmallFrame
	args := func(list []*Node) error {
		for _, x := range list {
			if err := validPositions(x); err != nil {
				return err
			}
			if err := v.optExpr(x.Child("annotation"), d+valSmallFrame); err != nil {
				return err
			}
		}
		return nil
	}
	if err := args(a.Children("posonlyargs")); err != nil {
		return err
	}
	if err := args(a.Children("args")); err != nil {
		return err
	}
	if va := a.Child("vararg"); va != nil {
		if err := v.optExpr(va.Child("annotation"), d); err != nil {
			return err
		}
	}
	if err := args(a.Children("kwonlyargs")); err != nil {
		return err
	}
	if kw := a.Child("kwarg"); kw != nil {
		if err := v.optExpr(kw.Child("annotation"), d); err != nil {
			return err
		}
	}
	if len(a.Children("defaults")) > len(a.Children("posonlyargs"))+len(a.Children("args")) {
		return pyErr("ValueError", "more positional defaults than args on arguments")
	}
	if len(a.Children("kw_defaults")) != len(a.Children("kwonlyargs")) {
		return pyErr("ValueError", "length of kwonlyargs is not the same as kw_defaults on arguments")
	}
	if err := v.exprsCall(a.Children("defaults"), "Load", false, d); err != nil {
		return err
	}
	return v.exprsCall(a.Children("kw_defaults"), "Load", true, p)
}

func (v *validator) keywords(list []*Node, p int) error {
	for _, k := range list {
		if err := v.expr(k.Child("value"), "Load", p+valSmallFrame); err != nil {
			return err
		}
	}
	return nil
}

func (v *validator) comprehension(gens []*Node, d int) error {
	if len(gens) == 0 {
		return pyErr("ValueError", "comprehension with no generators")
	}
	for _, g := range gens {
		if err := v.expr(g.Child("target"), "Store", d); err != nil {
			return err
		}
		if err := v.expr(g.Child("iter"), "Load", d); err != nil {
			return err
		}
		if err := v.exprsCall(g.Children("ifs"), "Load", false, d); err != nil {
			return err
		}
	}
	return nil
}

func (v *validator) typeParams(list []*Node, d int) error {
	for _, tp := range list {
		if tp == nil {
			continue
		}
		if err := validPositions(tp); err != nil {
			return err
		}
		if err := validName(tp.Get("name")); err != nil {
			return err
		}
		if tp.Type == "TypeVar" {
			if err := v.optExpr(tp.Child("bound"), d); err != nil {
				return err
			}
		}
		if err := v.optExpr(tp.Child("default_value"), d); err != nil {
			return err
		}
	}
	return nil
}

func (v *validator) withItems(s *Node, d int) error {
	items := s.Children("items")
	if err := nonEmpty(len(items), "items", s.Type); err != nil {
		return err
	}
	for _, it := range items {
		if err := v.expr(it.Child("context_expr"), "Load", d); err != nil {
			return err
		}
		if ov := it.Child("optional_vars"); ov != nil {
			if err := v.expr(ov, "Store", d); err != nil {
				return err
			}
		}
	}
	return v.body(s.Children("body"), s.Type, d)
}

// Validates a Try or TryStar, positions of handlers checked only for Try as CPython does
func (v *validator) try(s *Node, p, d int) error {
	if err := v.body(s.Children("body"), s.Type, d); err != nil {
		return err
	}
	handlers, final, orelse := s.Children("handlers"), s.Children("finalbody"), s.Children("orelse")
	if len(handlers) == 0 && len(final) == 0 {
		return pyErr("ValueError", "%s has neither except handlers nor finalbody", s.Type)
	}
	if len(handlers) == 0 && len(orelse) > 0 {
		return pyErr("ValueError", "%s has orelse but no except handlers", s.Type)
	}
	for _, h := range handlers {
		if h == nil {
			nullEntry("validate_stmt " + s.Type + " handlers")
		}
		if s.Type == "Try" {
			if err := validPositions(h); err != nil {
				return err
			}
		}
		if t := h.Child("type"); t != nil {
			if err := v.expr(t, "Load", d); err != nil {
				return err
			}
		}
		if s.Type == "Try" && h.Get("name") != nil {
			if err := validName(h.Get("name")); err != nil {
				return err
			}
		}
		if err := v.body(h.Children("body"), "ExceptHandler", d); err != nil {
			return err
		}
	}
	if len(final) > 0 {
		if err := v.stmts(final, d); err != nil {
			return err
		}
	}
	if len(orelse) > 0 {
		return v.stmts(orelse, d)
	}
	return nil
}

// Validates a statement as validate_stmt, tail calls and the Match subject, patterns and guards reusing the caller depth p
func (v *validator) stmt(s *Node, p int) error {
	if err := validPositions(s); err != nil {
		return err
	}
	d := p + valStmtFrame
	if err := v.enter(d); err != nil {
		return err
	}
	switch s.Type {
	case "FunctionDef", "AsyncFunctionDef", "ClassDef":
		if err := v.body(s.Children("body"), s.Type, d); err != nil {
			return err
		}
		if err := validName(s.Get("name")); err != nil {
			return err
		}
		if err := v.typeParams(s.Children("type_params"), d); err != nil {
			return err
		}
		if s.Type == "ClassDef" {
			if err := v.exprsCall(s.Children("bases"), "Load", false, d); err != nil {
				return err
			}
			if err := v.keywords(s.Children("keywords"), d); err != nil {
				return err
			}
			return v.exprsCall(s.Children("decorator_list"), "Load", false, p)
		}
		if err := v.arguments(s.Child("args"), d); err != nil {
			return err
		}
		if err := v.exprsCall(s.Children("decorator_list"), "Load", false, d); err != nil {
			return err
		}
		return v.optExpr(s.Child("returns"), d)
	case "Return":
		return v.optExpr(s.Child("value"), d)
	case "Delete", "Assign":
		ctx := "Store"
		if s.Type == "Delete" {
			ctx = "Del"
		}
		targets := s.Children("targets")
		if err := nonEmpty(len(targets), "targets", s.Type); err != nil {
			return err
		}
		if s.Type == "Delete" {
			return v.exprsCall(targets, ctx, false, p)
		}
		if err := v.exprsCall(targets, ctx, false, d); err != nil {
			return err
		}
		return v.expr(s.Child("value"), "Load", d)
	case "AugAssign":
		if err := v.expr(s.Child("target"), "Store", d); err != nil {
			return err
		}
		return v.expr(s.Child("value"), "Load", d)
	case "AnnAssign":
		if !s.Child("target").Is("Name") && s.Get("simple") != 0 {
			return pyErr("TypeError", "AnnAssign with simple non-Name target")
		}
		if err := v.expr(s.Child("target"), "Store", d); err != nil {
			return err
		}
		if err := v.optExpr(s.Child("value"), d); err != nil {
			return err
		}
		return v.expr(s.Child("annotation"), "Load", d)
	case "TypeAlias":
		if !s.Child("name").Is("Name") {
			return pyErr("TypeError", "TypeAlias with non-Name name")
		}
		if err := v.expr(s.Child("name"), "Store", d); err != nil {
			return err
		}
		if err := v.typeParams(s.Children("type_params"), d); err != nil {
			return err
		}
		return v.expr(s.Child("value"), "Load", d)
	case "For", "AsyncFor":
		if err := v.expr(s.Child("target"), "Store", d); err != nil {
			return err
		}
		if err := v.expr(s.Child("iter"), "Load", d); err != nil {
			return err
		}
		if err := v.body(s.Children("body"), s.Type, d); err != nil {
			return err
		}
		return v.stmts(s.Children("orelse"), d)
	case "While", "If":
		if err := v.expr(s.Child("test"), "Load", d); err != nil {
			return err
		}
		if err := v.body(s.Children("body"), s.Type, d); err != nil {
			return err
		}
		return v.stmts(s.Children("orelse"), d)
	case "With", "AsyncWith":
		return v.withItems(s, d)
	case "Match":
		if err := v.expr(s.Child("subject"), "Load", p); err != nil {
			return err
		}
		cases := s.Children("cases")
		if err := nonEmpty(len(cases), "cases", "Match"); err != nil {
			return err
		}
		for _, m := range cases {
			if err := v.pattern(m.Child("pattern"), false, p); err != nil {
				return err
			}
			if err := v.optExpr(m.Child("guard"), p); err != nil {
				return err
			}
			if err := v.body(m.Children("body"), "match_case", d); err != nil {
				return err
			}
		}
		return nil
	case "Raise":
		if exc := s.Child("exc"); exc != nil {
			if err := v.expr(exc, "Load", d); err != nil {
				return err
			}
			return v.optExpr(s.Child("cause"), d)
		}
		if s.Child("cause") != nil {
			return pyErr("ValueError", "Raise with cause but no exception")
		}
		return nil
	case "Try", "TryStar":
		return v.try(s, p, d)
	case "Assert":
		if err := v.expr(s.Child("test"), "Load", d); err != nil {
			return err
		}
		return v.optExpr(s.Child("msg"), d)
	case "Import", "ImportFrom":
		if s.Type == "ImportFrom" && s.Get("level").(int) < 0 {
			return pyErr("ValueError", "Negative ImportFrom level")
		}
		names := s.Children("names")
		if err := nonEmpty(len(names), "names", s.Type); err != nil {
			return err
		}
		for _, a := range names {
			if err := validName(a.Get("name")); err != nil {
				return err
			}
			if as := a.Get("asname"); as != nil {
				if err := validName(as); err != nil {
					return err
				}
			}
		}
		return nil
	case "Global", "Nonlocal":
		names, _ := listItems(s.Get("names"))
		return nonEmpty(len(names), "names", s.Type)
	case "Expr":
		return v.expr(s.Child("value"), "Load", p)
	}
	return nil
}

// Dereferences a NULL list entry the way CPython 3.14.8 does, which ends the process with a segmentation fault
func nullEntry(where string) {
	panic("pyast: NULL entry dereferenced in " + where + ", CPython 3.14.8 crashes with SIGSEGV here")
}

// Validates an expression as validate_expr, a lone returned call reusing the caller depth p as GCC tail calls it
func (v *validator) expr(e *Node, ctx string, p int) error {
	if err := validPositions(e); err != nil {
		return err
	}
	d := p + valExprFrame
	if err := v.enter(d); err != nil {
		return err
	}
	actual := ""
	switch e.Type {
	case "Name":
		if err := validName(e.Get("id")); err != nil {
			return err
		}
		actual = e.Child("ctx").Type
	case "Attribute", "Subscript", "Starred", "List", "Tuple":
		actual = e.Child("ctx").Type
	default:
		if ctx != "Load" {
			return pyErr("ValueError", "expression which can't be assigned to in %s context", ctx)
		}
	}
	if actual != "" && actual != ctx {
		return pyErr("ValueError", "expression must have %s context but has %s instead", ctx, actual)
	}
	switch e.Type {
	case "BoolOp":
		if len(e.Children("values")) < 2 {
			return pyErr("ValueError", "BoolOp with less than 2 values")
		}
		return v.exprsCall(e.Children("values"), "Load", false, p)
	case "BinOp":
		if err := v.expr(e.Child("left"), "Load", d); err != nil {
			return err
		}
		return v.expr(e.Child("right"), "Load", d)
	case "UnaryOp":
		return v.expr(e.Child("operand"), "Load", p)
	case "Lambda":
		if err := v.arguments(e.Child("args"), d); err != nil {
			return err
		}
		return v.expr(e.Child("body"), "Load", d)
	case "IfExp":
		if err := v.expr(e.Child("test"), "Load", d); err != nil {
			return err
		}
		if err := v.expr(e.Child("body"), "Load", d); err != nil {
			return err
		}
		return v.expr(e.Child("orelse"), "Load", d)
	case "Dict":
		if len(e.Children("keys")) != len(e.Children("values")) {
			return pyErr("ValueError", "Dict doesn't have the same number of keys as values")
		}
		if err := v.exprsCall(e.Children("keys"), "Load", true, d); err != nil {
			return err
		}
		return v.exprsCall(e.Children("values"), "Load", false, p)
	case "Set":
		return v.exprsCall(e.Children("elts"), "Load", false, p)
	case "ListComp", "SetComp", "GeneratorExp":
		if err := v.comprehension(e.Children("generators"), d); err != nil {
			return err
		}
		return v.expr(e.Child("elt"), "Load", d)
	case "DictComp":
		if err := v.comprehension(e.Children("generators"), d); err != nil {
			return err
		}
		if err := v.expr(e.Child("key"), "Load", d); err != nil {
			return err
		}
		return v.expr(e.Child("value"), "Load", d)
	case "Yield":
		return v.optExpr(e.Child("value"), d)
	case "YieldFrom", "Await":
		return v.expr(e.Child("value"), "Load", p)
	case "Compare":
		comps := e.Children("comparators")
		if len(comps) == 0 {
			return pyErr("ValueError", "Compare with no comparators")
		}
		if len(comps) != len(e.Children("ops")) {
			return pyErr("ValueError", "Compare has a different number of comparators and operands")
		}
		if err := v.exprsCall(comps, "Load", false, d); err != nil {
			return err
		}
		return v.expr(e.Child("left"), "Load", d)
	case "Call":
		if err := v.expr(e.Child("func"), "Load", d); err != nil {
			return err
		}
		if err := v.exprsCall(e.Children("args"), "Load", false, d); err != nil {
			return err
		}
		return v.keywords(e.Children("keywords"), p)
	case "Constant":
		return v.constant(e.Get("value"), d)
	case "JoinedStr", "TemplateStr":
		return v.exprsCall(e.Children("values"), "Load", false, p)
	case "FormattedValue", "Interpolation":
		if err := v.expr(e.Child("value"), "Load", d); err != nil {
			return err
		}
		if fs := e.Child("format_spec"); fs != nil {
			return v.expr(fs, "Load", p)
		}
		return nil
	case "Attribute":
		return v.expr(e.Child("value"), "Load", p)
	case "Subscript":
		if err := v.expr(e.Child("slice"), "Load", d); err != nil {
			return err
		}
		return v.expr(e.Child("value"), "Load", d)
	case "Starred":
		return v.expr(e.Child("value"), ctx, p)
	case "Slice":
		if err := v.optExpr(e.Child("lower"), d); err != nil {
			return err
		}
		if err := v.optExpr(e.Child("upper"), d); err != nil {
			return err
		}
		return v.optExpr(e.Child("step"), d)
	case "List", "Tuple":
		return v.exprsCall(e.Children("elts"), ctx, false, p)
	case "NamedExpr":
		if !e.Child("target").Is("Name") {
			return pyErr("TypeError", "NamedExpr target must be a Name")
		}
		return v.expr(e.Child("value"), "Load", p)
	}
	return nil
}

// Accepts the constant types validate_constant allows, recursing into tuples and frozensets in their own frames
func (v *validator) constant(c any, p int) error {
	var items []any
	switch x := c.(type) {
	case nil, Ellipsis, bool, string, []byte, float64, Complex, int, *big.Int:
		return nil
	case pyrepr.Tuple:
		items = x
	case FrozenSet:
		items = x
	default:
		if _, ok := pyrepr.Int(c); ok {
			return nil
		}
		return pyErr("TypeError", "got an invalid type in Constant: %s", typeName(c))
	}
	d := p + valConstFrame
	if err := v.enter(d); err != nil {
		return err
	}
	for _, it := range items {
		if err := v.constant(it, d); err != nil {
			return err
		}
	}
	return nil
}

func literalNumber(e *Node, real, imag bool) bool {
	switch e.Get("value").(type) {
	case float64, int, *big.Int:
		return real
	case Complex:
		return imag
	}
	return false
}

func literalNegative(e *Node, real, imag bool) bool {
	if !e.Child("op").Is("USub") {
		return false
	}
	op := e.Child("operand")
	return op.Is("Constant") && literalNumber(op, real, imag)
}

func literalComplex(e *Node) bool {
	if !e.Child("op").Is("Add", "Sub") {
		return false
	}
	left, right := e.Child("left"), e.Child("right")
	switch {
	case left.Is("Constant"):
		if !literalNumber(left, true, false) {
			return false
		}
	case left.Is("UnaryOp"):
		if !literalNegative(left, true, false) {
			return false
		}
	default:
		return false
	}
	return right.Is("Constant") && literalNumber(right, false, true)
}

// Validates a literal pattern value as validate_pattern_match_value
func (v *validator) matchValue(e *Node, d int) error {
	if err := v.expr(e, "Load", d); err != nil {
		return err
	}
	switch e.Type {
	case "Constant":
		if err := v.expr(e, "Load", d); err != nil {
			return err
		}
		switch e.Get("value").(type) {
		case int, *big.Int, float64, []byte, Complex, string:
			return nil
		}
		return pyErr("ValueError", "unexpected constant inside of a literal pattern")
	case "Attribute", "JoinedStr", "TemplateStr":
		return nil
	case "UnaryOp":
		if literalNegative(e, true, true) {
			return nil
		}
	case "BinOp":
		if literalComplex(e) {
			return nil
		}
	}
	return pyErr("ValueError", "patterns may only match literals and attribute lookups")
}

func (v *validator) patterns(list []*Node, starOK bool, d int) error {
	for _, p := range list {
		if p == nil {
			nullEntry("validate_patterns")
		}
		if err := v.pattern(p, starOK, d); err != nil {
			return err
		}
	}
	return nil
}

// Validates a pattern as validate_pattern, MatchValue and MatchAs tail calls reusing the caller depth p
func (v *validator) pattern(pt *Node, starOK bool, p int) error {
	if err := validPositions(pt); err != nil {
		return err
	}
	d := p + valPatFrame
	if err := v.enter(d); err != nil {
		return err
	}
	switch pt.Type {
	case "MatchValue":
		return v.matchValue(pt.Child("value"), p)
	case "MatchSingleton":
		switch pt.Get("value").(type) {
		case nil, bool:
			return nil
		}
		return pyErr("ValueError", "MatchSingleton can only contain True, False and None")
	case "MatchSequence":
		return v.patterns(pt.Children("patterns"), true, d)
	case "MatchMapping":
		keys := pt.Children("keys")
		if len(keys) != len(pt.Children("patterns")) {
			return pyErr("ValueError", "MatchMapping doesn't have the same number of keys as patterns")
		}
		if rest := pt.Get("rest"); rest != nil {
			if err := validCapture(rest); err != nil {
				return err
			}
		}
		for _, k := range keys {
			if k == nil {
				nullEntry("validate_pattern MatchMapping keys")
			}
			if k.Type == "Constant" {
				switch k.Get("value").(type) {
				case nil, bool:
					continue
				}
			}
			if err := v.matchValue(k, d); err != nil {
				return err
			}
		}
		return v.patterns(pt.Children("patterns"), false, d)
	case "MatchClass":
		attrs, _ := listItems(pt.Get("kwd_attrs"))
		if len(attrs) != len(pt.Children("kwd_patterns")) {
			return pyErr("ValueError", "MatchClass doesn't have the same number of keyword attributes as patterns")
		}
		if err := v.expr(pt.Child("cls"), "Load", d); err != nil {
			return err
		}
		for cls := pt.Child("cls"); !cls.Is("Name"); cls = cls.Child("value") {
			if !cls.Is("Attribute") {
				return pyErr("ValueError", "MatchClass cls field can only contain Name or Attribute nodes.")
			}
		}
		for _, a := range attrs {
			if a == nil {
				nullEntry("validate_name")
			}
			if err := validName(a); err != nil {
				return err
			}
		}
		if err := v.patterns(pt.Children("patterns"), false, d+valPatsFrame); err != nil {
			return err
		}
		return v.patterns(pt.Children("kwd_patterns"), false, d)
	case "MatchStar":
		if !starOK {
			return pyErr("ValueError", "can't use MatchStar here")
		}
		if name := pt.Get("name"); name != nil {
			return validCapture(name)
		}
		return nil
	case "MatchAs":
		name := pt.Get("name")
		if name != nil {
			if err := validCapture(name); err != nil {
				return err
			}
		}
		if pt.Child("pattern") == nil {
			return nil
		}
		if name == nil {
			return pyErr("ValueError", "MatchAs must specify a target name if a pattern is given")
		}
		return v.pattern(pt.Child("pattern"), false, p)
	case "MatchOr":
		if len(pt.Children("patterns")) < 2 {
			return pyErr("ValueError", "MatchOr requires at least 2 patterns")
		}
		return v.patterns(pt.Children("patterns"), false, d)
	}
	return nil
}
