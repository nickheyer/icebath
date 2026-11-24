package pythonutil

import (
	"errors"
	"math/big"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	precNamedExpr = iota + 1
	precTuple
	precYield
	precTest
	precOr
	precAnd
	precNot
	precCmp
	precExpr
	precBXor
	precBAnd
	precShift
	precArith
	precTerm
	precFactor
	precPower
	precAwait
	precAtom
)

const precBOr = precExpr

func precNext(p int) int {
	if p < precAtom {
		return p + 1
	}
	return p
}

var (
	multiQuotes = []string{"\"\"\"", "'''"}
	allQuotes   = []string{"'", "\"", "\"\"\"", "'''"}
)

type unparser struct {
	out       []byte
	prec      map[*Node]int
	indent    int
	inTryStar bool
}

// Panic value of a node the unparser cannot write
type unparseError struct{ msg string }

// Python source text of a tree or of any node in it, written with the minimal parentheses and the quotes that need fewest escapes
func Unparse(n *Node) (s string, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(unparseError)
			if !ok {
				panic(r)
			}
			s, err = "", errors.New(e.msg)
		}
	}()
	if n == nil {
		return "", errors.New("cannot unparse a nil node")
	}
	u := &unparser{prec: map[*Node]int{}}
	u.traverse(n)
	return string(u.out), nil
}

func (u *unparser) write(s string) { u.out = append(u.out, s...) }

func (u *unparser) fill(text string) {
	if len(u.out) > 0 {
		u.write("\n")
	}
	for range u.indent {
		u.write("    ")
	}
	u.write(text)
}

func (u *unparser) maybeNewline() {
	if len(u.out) > 0 {
		u.write("\n")
	}
}

func (u *unparser) block(body func()) {
	u.write(":")
	u.indent++
	body()
	u.indent--
}

func (u *unparser) delimitIf(start, end string, cond bool, body func()) {
	if cond {
		u.write(start)
	}
	body()
	if cond {
		u.write(end)
	}
}

func (u *unparser) requireParens(p int, n *Node, body func()) {
	u.delimitIf("(", ")", u.getPrec(n) > p, body)
}

func (u *unparser) getPrec(n *Node) int {
	if p, ok := u.prec[n]; ok {
		return p
	}
	return precTest
}

func (u *unparser) setPrec(p int, nodes ...*Node) {
	for _, n := range nodes {
		if n != nil {
			u.prec[n] = p
		}
	}
}

func (u *unparser) interleave(sep string, nodes []*Node) {
	for i, n := range nodes {
		if i > 0 {
			u.write(sep)
		}
		u.traverse(n)
	}
}

func (u *unparser) itemsView(nodes []*Node) {
	if len(nodes) == 1 {
		u.traverse(nodes[0])
		u.write(",")
		return
	}
	u.interleave(", ", nodes)
}

func (u *unparser) traverseAll(nodes []*Node) {
	for _, n := range nodes {
		u.traverse(n)
	}
}

// Text a traversal writes, taken back out of the buffer
func (u *unparser) capture(f func()) string {
	mark := len(u.out)
	f()
	s := string(u.out[mark:])
	u.out = u.out[:mark]
	return s
}

func rawDocstring(n *Node) *Node {
	if !n.Is("AsyncFunctionDef", "FunctionDef", "ClassDef", "Module") {
		return nil
	}
	body := n.Children("body")
	if len(body) < 1 || !body[0].Is("Expr") {
		return nil
	}
	v := body[0].Child("value")
	if _, ok := v.Get("value").(string); v.Is("Constant") && ok {
		return v
	}
	return nil
}

func (u *unparser) docstringAndBody(n *Node) {
	if d := rawDocstring(n); d != nil {
		u.fill("")
		if k, _ := d.Str("kind"); k == "u" {
			u.write("u")
		}
		u.writeStrAvoidingBackslashes(d.Get("value").(string), multiQuotes)
		u.traverseAll(n.Children("body")[1:])
		return
	}
	u.traverseAll(n.Children("body"))
}

func unaryOpText(op string) string {
	switch op {
	case "Invert":
		return "~"
	case "Not":
		return "not"
	case "UAdd":
		return "+"
	}
	return "-"
}

func binOpText(op string) string {
	switch op {
	case "Add":
		return "+"
	case "Sub":
		return "-"
	case "Mult":
		return "*"
	case "MatMult":
		return "@"
	case "Div":
		return "/"
	case "Mod":
		return "%"
	case "LShift":
		return "<<"
	case "RShift":
		return ">>"
	case "BitOr":
		return "|"
	case "BitXor":
		return "^"
	case "BitAnd":
		return "&"
	case "FloorDiv":
		return "//"
	}
	return "**"
}

func binOpPrec(op string) int {
	switch op {
	case "+", "-":
		return precArith
	case "<<", ">>":
		return precShift
	case "|":
		return precBOr
	case "^":
		return precBXor
	case "&":
		return precBAnd
	case "**":
		return precPower
	}
	return precTerm
}

func cmpOpText(op string) string {
	switch op {
	case "Eq":
		return "=="
	case "NotEq":
		return "!="
	case "Lt":
		return "<"
	case "LtE":
		return "<="
	case "Gt":
		return ">"
	case "GtE":
		return ">="
	case "Is":
		return "is"
	case "IsNot":
		return "is not"
	case "In":
		return "in"
	}
	return "not in"
}

func (u *unparser) traverse(n *Node) {
	switch n.Type {
	case "Module":
		u.docstringAndBody(n)
	case "Expression":
		u.traverse(n.Child("body"))
	case "Expr":
		u.fill("")
		u.setPrec(precYield, n.Child("value"))
		u.traverse(n.Child("value"))
	case "NamedExpr":
		u.requireParens(precNamedExpr, n, func() {
			u.setPrec(precAtom, n.Child("target"), n.Child("value"))
			u.traverse(n.Child("target"))
			u.write(" := ")
			u.traverse(n.Child("value"))
		})
	case "Import":
		u.fill("import ")
		u.interleave(", ", n.Children("names"))
	case "ImportFrom":
		u.fill("from ")
		level, _ := n.Int("level")
		u.write(strings.Repeat(".", level))
		if m, ok := n.Str("module"); ok && m != "" {
			u.write(m)
		}
		u.write(" import ")
		u.interleave(", ", n.Children("names"))
	case "Assign":
		u.fill("")
		for _, t := range n.Children("targets") {
			u.setPrec(precTuple, t)
			u.traverse(t)
			u.write(" = ")
		}
		u.traverse(n.Child("value"))
	case "AugAssign":
		u.fill("")
		u.traverse(n.Child("target"))
		u.write(" " + binOpText(n.Child("op").Type) + "= ")
		u.traverse(n.Child("value"))
	case "AnnAssign":
		u.fill("")
		simple, _ := n.Int("simple")
		u.delimitIf("(", ")", simple == 0 && n.Child("target").Is("Name"), func() { u.traverse(n.Child("target")) })
		u.write(": ")
		u.traverse(n.Child("annotation"))
		if v := n.Child("value"); v != nil {
			u.write(" = ")
			u.traverse(v)
		}
	case "Return":
		u.fill("return")
		if v := n.Child("value"); v != nil {
			u.write(" ")
			u.traverse(v)
		}
	case "Pass":
		u.fill("pass")
	case "Break":
		u.fill("break")
	case "Continue":
		u.fill("continue")
	case "Delete":
		u.fill("del ")
		u.interleave(", ", n.Children("targets"))
	case "Assert":
		u.fill("assert ")
		u.traverse(n.Child("test"))
		if m := n.Child("msg"); m != nil {
			u.write(", ")
			u.traverse(m)
		}
	case "Global", "Nonlocal":
		u.fill(strings.ToLower(n.Type) + " ")
		u.write(strings.Join(n.Names("names"), ", "))
	case "Await":
		u.requireParens(precAwait, n, func() {
			u.write("await")
			if v := n.Child("value"); v != nil {
				u.write(" ")
				u.setPrec(precAtom, v)
				u.traverse(v)
			}
		})
	case "Yield":
		u.requireParens(precYield, n, func() {
			u.write("yield")
			if v := n.Child("value"); v != nil {
				u.write(" ")
				u.setPrec(precAtom, v)
				u.traverse(v)
			}
		})
	case "YieldFrom":
		u.requireParens(precYield, n, func() {
			u.write("yield from ")
			v := n.Child("value")
			if v == nil {
				panic(unparseError{"yield from node has no value"})
			}
			u.setPrec(precAtom, v)
			u.traverse(v)
		})
	case "Raise":
		u.fill("raise")
		exc, cause := n.Child("exc"), n.Child("cause")
		if exc == nil {
			if cause != nil {
				panic(unparseError{"raise node has a cause without an exception"})
			}
			return
		}
		u.write(" ")
		u.traverse(exc)
		if cause != nil {
			u.write(" from ")
			u.traverse(cause)
		}
	case "Try", "TryStar":
		prev := u.inTryStar
		u.inTryStar = n.Type == "TryStar"
		u.fill("try")
		u.block(func() { u.traverseAll(n.Children("body")) })
		u.traverseAll(n.Children("handlers"))
		if o := n.Children("orelse"); len(o) > 0 {
			u.fill("else")
			u.block(func() { u.traverseAll(o) })
		}
		if f := n.Children("finalbody"); len(f) > 0 {
			u.fill("finally")
			u.block(func() { u.traverseAll(f) })
		}
		u.inTryStar = prev
	case "ExceptHandler":
		if u.inTryStar {
			u.fill("except*")
		} else {
			u.fill("except")
		}
		if t := n.Child("type"); t != nil {
			u.write(" ")
			u.traverse(t)
		}
		if name, ok := n.Str("name"); ok && name != "" {
			u.write(" as ")
			u.write(name)
		}
		u.block(func() { u.traverseAll(n.Children("body")) })
	case "ClassDef":
		u.maybeNewline()
		for _, d := range n.Children("decorator_list") {
			u.fill("@")
			u.traverse(d)
		}
		name, _ := n.Str("name")
		u.fill("class " + name)
		u.typeParams(n.Children("type_params"))
		bases, kws := n.Children("bases"), n.Children("keywords")
		u.delimitIf("(", ")", len(bases) > 0 || len(kws) > 0, func() {
			u.interleave(", ", append(slices.Clip(bases), kws...))
		})
		u.block(func() { u.docstringAndBody(n) })
	case "FunctionDef", "AsyncFunctionDef":
		u.maybeNewline()
		for _, d := range n.Children("decorator_list") {
			u.fill("@")
			u.traverse(d)
		}
		prefix := "def"
		if n.Type == "AsyncFunctionDef" {
			prefix = "async def"
		}
		name, _ := n.Str("name")
		u.fill(prefix + " " + name)
		u.typeParams(n.Children("type_params"))
		u.write("(")
		u.traverse(n.Child("args"))
		u.write(")")
		if r := n.Child("returns"); r != nil {
			u.write(" -> ")
			u.traverse(r)
		}
		u.block(func() { u.docstringAndBody(n) })
	case "TypeVar":
		name, _ := n.Str("name")
		u.write(name)
		if b := n.Child("bound"); b != nil {
			u.write(": ")
			u.traverse(b)
		}
		u.defaultValue(n)
	case "TypeVarTuple":
		name, _ := n.Str("name")
		u.write("*" + name)
		u.defaultValue(n)
	case "ParamSpec":
		name, _ := n.Str("name")
		u.write("**" + name)
		u.defaultValue(n)
	case "TypeAlias":
		u.fill("type ")
		u.traverse(n.Child("name"))
		u.typeParams(n.Children("type_params"))
		u.write(" = ")
		u.traverse(n.Child("value"))
	case "For", "AsyncFor":
		if n.Type == "For" {
			u.fill("for ")
		} else {
			u.fill("async for ")
		}
		u.setPrec(precTuple, n.Child("target"))
		u.traverse(n.Child("target"))
		u.write(" in ")
		u.traverse(n.Child("iter"))
		u.block(func() { u.traverseAll(n.Children("body")) })
		if o := n.Children("orelse"); len(o) > 0 {
			u.fill("else")
			u.block(func() { u.traverseAll(o) })
		}
	case "If":
		u.fill("if ")
		u.traverse(n.Child("test"))
		u.block(func() { u.traverseAll(n.Children("body")) })
		for o := n.Children("orelse"); len(o) == 1 && o[0].Is("If"); o = n.Children("orelse") {
			n = o[0]
			u.fill("elif ")
			u.traverse(n.Child("test"))
			u.block(func() { u.traverseAll(n.Children("body")) })
		}
		if o := n.Children("orelse"); len(o) > 0 {
			u.fill("else")
			u.block(func() { u.traverseAll(o) })
		}
	case "While":
		u.fill("while ")
		u.traverse(n.Child("test"))
		u.block(func() { u.traverseAll(n.Children("body")) })
		if o := n.Children("orelse"); len(o) > 0 {
			u.fill("else")
			u.block(func() { u.traverseAll(o) })
		}
	case "With", "AsyncWith":
		if n.Type == "With" {
			u.fill("with ")
		} else {
			u.fill("async with ")
		}
		u.interleave(", ", n.Children("items"))
		u.block(func() { u.traverseAll(n.Children("body")) })
	case "JoinedStr":
		u.writeFTString(n.Children("values"), "f")
	case "TemplateStr":
		u.writeFTString(n.Children("values"), "t")
	case "FormattedValue":
		u.writeInterpolation(n, false)
	case "Interpolation":
		_, hasStr := n.Get("str").(string)
		u.writeInterpolation(n, hasStr)
	case "Name":
		id, _ := n.Str("id")
		u.write(id)
	case "Constant":
		v := n.Get("value")
		if _, ok := v.(Ellipsis); !ok {
			if k, _ := n.Str("kind"); k == "u" {
				u.write("u")
			}
		}
		u.out = appendLiteral(u.out, v)
	case "List":
		u.write("[")
		u.interleave(", ", n.Children("elts"))
		u.write("]")
	case "ListComp", "GeneratorExp", "SetComp":
		open, close := "[", "]"
		switch n.Type {
		case "GeneratorExp":
			open, close = "(", ")"
		case "SetComp":
			open, close = "{", "}"
		}
		u.write(open)
		u.traverse(n.Child("elt"))
		u.traverseAll(n.Children("generators"))
		u.write(close)
	case "DictComp":
		u.write("{")
		u.traverse(n.Child("key"))
		u.write(": ")
		u.traverse(n.Child("value"))
		u.traverseAll(n.Children("generators"))
		u.write("}")
	case "comprehension":
		if a, _ := n.Int("is_async"); a != 0 {
			u.write(" async for ")
		} else {
			u.write(" for ")
		}
		u.setPrec(precTuple, n.Child("target"))
		u.traverse(n.Child("target"))
		u.write(" in ")
		u.setPrec(precNext(precTest), n.Child("iter"))
		u.setPrec(precNext(precTest), n.Children("ifs")...)
		u.traverse(n.Child("iter"))
		for _, c := range n.Children("ifs") {
			u.write(" if ")
			u.traverse(c)
		}
	case "IfExp":
		u.requireParens(precTest, n, func() {
			u.setPrec(precNext(precTest), n.Child("body"), n.Child("test"))
			u.traverse(n.Child("body"))
			u.write(" if ")
			u.traverse(n.Child("test"))
			u.write(" else ")
			u.setPrec(precTest, n.Child("orelse"))
			u.traverse(n.Child("orelse"))
		})
	case "Set":
		if elts := n.Children("elts"); len(elts) > 0 {
			u.write("{")
			u.interleave(", ", elts)
			u.write("}")
		} else {
			u.write("{*()}")
		}
	case "Dict":
		u.write("{")
		keys, vals := n.Children("keys"), n.Children("values")
		for i := range min(len(keys), len(vals)) {
			if i > 0 {
				u.write(", ")
			}
			if keys[i] == nil {
				u.write("**")
				u.setPrec(precExpr, vals[i])
				u.traverse(vals[i])
			} else {
				u.traverse(keys[i])
				u.write(": ")
				u.traverse(vals[i])
			}
		}
		u.write("}")
	case "Tuple":
		elts := n.Children("elts")
		u.delimitIf("(", ")", len(elts) == 0 || u.getPrec(n) > precTuple, func() { u.itemsView(elts) })
	case "UnaryOp":
		op := unaryOpText(n.Child("op").Type)
		p := precFactor
		if op == "not" {
			p = precNot
		}
		u.requireParens(p, n, func() {
			u.write(op)
			if p != precFactor {
				u.write(" ")
			}
			u.setPrec(p, n.Child("operand"))
			u.traverse(n.Child("operand"))
		})
	case "BinOp":
		op := binOpText(n.Child("op").Type)
		p := binOpPrec(op)
		u.requireParens(p, n, func() {
			left, right := p, precNext(p)
			if op == "**" {
				left, right = precNext(p), p
			}
			u.setPrec(left, n.Child("left"))
			u.traverse(n.Child("left"))
			u.write(" " + op + " ")
			u.setPrec(right, n.Child("right"))
			u.traverse(n.Child("right"))
		})
	case "Compare":
		u.requireParens(precCmp, n, func() {
			u.setPrec(precNext(precCmp), n.Child("left"))
			u.setPrec(precNext(precCmp), n.Children("comparators")...)
			u.traverse(n.Child("left"))
			ops, comps := n.Children("ops"), n.Children("comparators")
			for i := range min(len(ops), len(comps)) {
				u.write(" " + cmpOpText(ops[i].Type) + " ")
				u.traverse(comps[i])
			}
		})
	case "BoolOp":
		op := "and"
		p := precAnd
		if n.Child("op").Is("Or") {
			op, p = "or", precOr
		}
		u.requireParens(p, n, func() {
			level := p
			for i, v := range n.Children("values") {
				if i > 0 {
					u.write(" " + op + " ")
				}
				level = precNext(level)
				u.setPrec(level, v)
				u.traverse(v)
			}
		})
	case "Attribute":
		v := n.Child("value")
		u.setPrec(precAtom, v)
		u.traverse(v)
		if v.Is("Constant") && isIntConstant(v.Get("value")) {
			u.write(" ")
		}
		attr, _ := n.Str("attr")
		u.write("." + attr)
	case "Call":
		u.setPrec(precAtom, n.Child("func"))
		u.traverse(n.Child("func"))
		u.write("(")
		u.interleave(", ", append(slices.Clip(n.Children("args")), n.Children("keywords")...))
		u.write(")")
	case "Subscript":
		u.setPrec(precAtom, n.Child("value"))
		u.traverse(n.Child("value"))
		u.write("[")
		if s := n.Child("slice"); s.Is("Tuple") && len(s.Children("elts")) > 0 {
			u.itemsView(s.Children("elts"))
		} else {
			u.traverse(s)
		}
		u.write("]")
	case "Starred":
		u.write("*")
		u.setPrec(precExpr, n.Child("value"))
		u.traverse(n.Child("value"))
	case "Slice":
		if l := n.Child("lower"); l != nil {
			u.traverse(l)
		}
		u.write(":")
		if up := n.Child("upper"); up != nil {
			u.traverse(up)
		}
		if st := n.Child("step"); st != nil {
			u.write(":")
			u.traverse(st)
		}
	case "Match":
		u.fill("match ")
		u.traverse(n.Child("subject"))
		u.block(func() { u.traverseAll(n.Children("cases")) })
	case "arg":
		name, _ := n.Str("arg")
		u.write(name)
		if a := n.Child("annotation"); a != nil {
			u.write(": ")
			u.traverse(a)
		}
	case "arguments":
		u.arguments(n)
	case "keyword":
		if name, ok := n.Str("arg"); ok {
			u.write(name)
			u.write("=")
		} else {
			u.write("**")
		}
		u.traverse(n.Child("value"))
	case "Lambda":
		u.requireParens(precTest, n, func() {
			u.write("lambda ")
			mark := len(u.out)
			u.traverse(n.Child("args"))
			if len(u.out) == mark {
				u.out = u.out[:mark-1]
			}
			u.write(": ")
			u.setPrec(precTest, n.Child("body"))
			u.traverse(n.Child("body"))
		})
	case "alias":
		name, _ := n.Str("name")
		u.write(name)
		if as, ok := n.Str("asname"); ok && as != "" {
			u.write(" as " + as)
		}
	case "withitem":
		u.traverse(n.Child("context_expr"))
		if v := n.Child("optional_vars"); v != nil {
			u.write(" as ")
			u.traverse(v)
		}
	case "match_case":
		u.fill("case ")
		u.traverse(n.Child("pattern"))
		if g := n.Child("guard"); g != nil {
			u.write(" if ")
			u.traverse(g)
		}
		u.block(func() { u.traverseAll(n.Children("body")) })
	case "MatchValue":
		u.traverse(n.Child("value"))
	case "MatchSingleton":
		u.out = appendLiteral(u.out, n.Get("value"))
	case "MatchSequence":
		u.write("[")
		u.interleave(", ", n.Children("patterns"))
		u.write("]")
	case "MatchStar":
		name, ok := n.Str("name")
		if !ok {
			name = "_"
		}
		u.write("*" + name)
	case "MatchMapping":
		u.write("{")
		keys, pats := n.Children("keys"), n.Children("patterns")
		if len(keys) != len(pats) {
			panic(unparseError{"mapping pattern has a different number of keys and patterns"})
		}
		for i := range keys {
			if i > 0 {
				u.write(", ")
			}
			u.traverse(keys[i])
			u.write(": ")
			u.traverse(pats[i])
		}
		if rest, ok := n.Str("rest"); ok {
			if len(keys) > 0 {
				u.write(", ")
			}
			u.write("**" + rest)
		}
		u.write("}")
	case "MatchClass":
		u.setPrec(precAtom, n.Child("cls"))
		u.traverse(n.Child("cls"))
		u.write("(")
		pats := n.Children("patterns")
		u.interleave(", ", pats)
		attrs, kwPats := n.Names("kwd_attrs"), n.Children("kwd_patterns")
		if len(attrs) > 0 {
			if len(attrs) != len(kwPats) {
				panic(unparseError{"class pattern has a different number of keyword names and patterns"})
			}
			if len(pats) > 0 {
				u.write(", ")
			}
			for i, a := range attrs {
				if i > 0 {
					u.write(", ")
				}
				u.write(a + "=")
				u.traverse(kwPats[i])
			}
		}
		u.write(")")
	case "MatchAs":
		name, hasName := n.Str("name")
		pat := n.Child("pattern")
		switch {
		case !hasName:
			u.write("_")
		case pat == nil:
			u.write(name)
		default:
			u.requireParens(precTest, n, func() {
				u.setPrec(precBOr, pat)
				u.traverse(pat)
				u.write(" as " + name)
			})
		}
	case "MatchOr":
		u.requireParens(precBOr, n, func() {
			u.setPrec(precNext(precBOr), n.Children("patterns")...)
			u.interleave(" | ", n.Children("patterns"))
		})
	default:
		panic(unparseError{"cannot unparse a " + n.Type + " node on its own"})
	}
}

func (u *unparser) defaultValue(n *Node) {
	if d := n.Child("default_value"); d != nil {
		u.write(" = ")
		u.traverse(d)
	}
}

func (u *unparser) typeParams(params []*Node) {
	if len(params) > 0 {
		u.write("[")
		u.interleave(", ", params)
		u.write("]")
	}
}

func (u *unparser) arguments(n *Node) {
	first := true
	sep := func() {
		if first {
			first = false
		} else {
			u.write(", ")
		}
	}
	posonly := n.Children("posonlyargs")
	all := append(slices.Clip(posonly), n.Children("args")...)
	defs := n.Children("defaults")
	for i, a := range all {
		sep()
		u.traverse(a)
		if di := i - (len(all) - len(defs)); di >= 0 && defs[di] != nil {
			u.write("=")
			u.traverse(defs[di])
		}
		if i+1 == len(posonly) {
			u.write(", /")
		}
	}
	vararg, kwonly := n.Child("vararg"), n.Children("kwonlyargs")
	if vararg != nil || len(kwonly) > 0 {
		sep()
		u.write("*")
		if vararg != nil {
			name, _ := vararg.Str("arg")
			u.write(name)
			if a := vararg.Child("annotation"); a != nil {
				u.write(": ")
				u.traverse(a)
			}
		}
	}
	kwDefs := n.Children("kw_defaults")
	for i, a := range kwonly {
		u.write(", ")
		u.traverse(a)
		if i < len(kwDefs) && kwDefs[i] != nil {
			u.write("=")
			u.traverse(kwDefs[i])
		}
	}
	if kw := n.Child("kwarg"); kw != nil {
		sep()
		name, _ := kw.Str("arg")
		u.write("**" + name)
		if a := kw.Child("annotation"); a != nil {
			u.write(": ")
			u.traverse(a)
		}
	}
}

// Appends a character escaped the way the unicode_escape codec writes backslashes and non printable characters, newlines and tabs kept unless special
func appendEscapedChar(b []byte, r rune, special bool) []byte {
	if !special && (r == '\n' || r == '\t') {
		return append(b, byte(r))
	}
	if r == '\\' {
		return append(b, `\\`...)
	}
	if isPrintable(r) {
		return utf8.AppendRune(b, r)
	}
	switch r {
	case '\t':
		return append(b, `\t`...)
	case '\n':
		return append(b, `\n`...)
	case '\r':
		return append(b, `\r`...)
	}
	switch {
	case r < 0x100:
		return appendHexEscape(b, 'x', r, 2)
	case r < 0x10000:
		return appendHexEscape(b, 'u', r, 4)
	}
	return appendHexEscape(b, 'U', r, 8)
}

// String literal text and the usable quote styles that need the fewest escapes
func strLiteralHelper(s string, quotes []string, special bool) (string, []string) {
	buf := make([]byte, 0, len(s))
	for _, r := range s {
		buf = appendEscapedChar(buf, r, special)
	}
	escaped := string(buf)
	possible := slices.Clone(quotes)
	if strings.Contains(escaped, "\n") {
		possible = slices.DeleteFunc(possible, func(q string) bool { return !slices.Contains(multiQuotes, q) })
	}
	possible = slices.DeleteFunc(possible, func(q string) bool { return strings.Contains(escaped, q) })
	if len(possible) == 0 {
		r := Quote(s)
		quote := r[:1]
		for _, q := range quotes {
			if strings.Contains(q, r[:1]) {
				quote = q
				break
			}
		}
		return r[1 : len(r)-1], []string{quote}
	}
	if escaped != "" {
		last := escaped[len(escaped)-1]
		slices.SortStableFunc(possible, func(a, b string) int {
			ka, kb := a[0] == last, b[0] == last
			switch {
			case ka == kb:
				return 0
			case !ka:
				return -1
			}
			return 1
		})
		if possible[0][0] == last {
			escaped = escaped[:len(escaped)-1] + `\` + escaped[len(escaped)-1:]
		}
	}
	return escaped, possible
}

func (u *unparser) writeStrAvoidingBackslashes(s string, quotes []string) {
	text, qs := strLiteralHelper(s, quotes, false)
	u.write(qs[0])
	u.write(text)
	u.write(qs[0])
}

type ftPart struct {
	text     string
	constant bool
}

func (u *unparser) ftstringHelper(parts []ftPart) {
	newParts := make([]string, 0, len(parts))
	quotes := slices.Clone(allQuotes)
	fallback := false
	for _, p := range parts {
		value := p.text
		if p.constant {
			v, nq := strLiteralHelper(value, quotes, true)
			disjoint := true
			for _, q := range nq {
				if slices.Contains(quotes, q) {
					disjoint = false
				}
			}
			if disjoint {
				fallback = true
				break
			}
			value, quotes = v, nq
		} else if strings.Contains(value, "\n") {
			quotes = slices.DeleteFunc(quotes, func(q string) bool { return !slices.Contains(multiQuotes, q) })
		}
		if nq := slices.DeleteFunc(slices.Clone(quotes), func(q string) bool { return strings.Contains(value, q) }); len(nq) > 0 {
			quotes = nq
		}
		newParts = append(newParts, value)
	}
	if fallback {
		quotes = []string{"'''"}
		newParts = newParts[:0]
		for _, p := range parts {
			value := p.text
			if p.constant {
				r := Quote("\"" + value)
				value = r[2 : len(r)-1]
			}
			newParts = append(newParts, value)
		}
	}
	u.write(quotes[0])
	for _, p := range newParts {
		u.write(p)
	}
	u.write(quotes[0])
}

func (u *unparser) writeFTString(values []*Node, prefix string) {
	u.write(prefix)
	parts := make([]ftPart, 0, len(values))
	for _, v := range values {
		text := u.capture(func() { u.ftInner(v, false) })
		parts = append(parts, ftPart{text, v.Is("Constant")})
	}
	u.ftstringHelper(parts)
}

func (u *unparser) ftInner(n *Node, formatSpec bool) {
	switch {
	case n.Is("JoinedStr"):
		for _, v := range n.Children("values") {
			u.ftInner(v, formatSpec)
		}
	case n.Is("Constant"):
		s, ok := n.Get("value").(string)
		if !ok {
			panic(unparseError{"f-string holds a constant that is not a string"})
		}
		s = strings.ReplaceAll(strings.ReplaceAll(s, "{", "{{"), "}", "}}")
		if formatSpec {
			s = strings.ReplaceAll(s, `\`, `\\`)
			s = strings.ReplaceAll(s, "'", `\'`)
			s = strings.ReplaceAll(s, `"`, `\"`)
			s = strings.ReplaceAll(s, "\n", `\n`)
		}
		u.write(s)
	case n.Is("FormattedValue"):
		u.writeInterpolation(n, false)
	case n.Is("Interpolation"):
		_, hasStr := n.Get("str").(string)
		u.writeInterpolation(n, hasStr)
	default:
		panic(unparseError{"f-string holds a " + n.Type + " node"})
	}
}

func (u *unparser) writeInterpolation(n *Node, useStr bool) {
	u.write("{")
	var expr string
	if useStr {
		expr, _ = n.Get("str").(string)
	} else {
		inner := &unparser{prec: map[*Node]int{}}
		inner.setPrec(precNext(precTest), n.Child("value"))
		inner.traverse(n.Child("value"))
		expr = string(inner.out)
	}
	if strings.HasPrefix(expr, "{") {
		u.write(" ")
	}
	u.write(expr)
	if conv, _ := n.Int("conversion"); conv != -1 {
		u.write("!")
		u.out = utf8.AppendRune(u.out, rune(conv))
	}
	if spec := n.Child("format_spec"); spec != nil {
		u.write(":")
		u.ftInner(spec, true)
	}
	u.write("}")
}

// Whether a constant is an integer, booleans included
func isIntConstant(v any) bool {
	switch v.(type) {
	case int64, *big.Int, bool:
		return true
	}
	return false
}
