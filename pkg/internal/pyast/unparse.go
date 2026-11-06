package pyast

import (
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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

const infStr = "1e309"

type unparser struct {
	out       []string
	prec      map[*Node]int
	ignores   map[int]string
	indent    int
	inTryStar bool
	// inInteractive joins top level simple statements with semicolons as visit_Interactive does
	inInteractive bool
}

// UnparseError is the ValueError ast.unparse raises for a node it cannot render
type UnparseError struct{ Msg string }

func (e *UnparseError) Error() string { return e.Msg }

// Unparse renders a tree back to source text exactly as ast.unparse
func Unparse(n *Node) (s string, err error) {
	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case *UnparseError:
				s, err = "", e
			case *IntDigitsError:
				s, err = "", e
			default:
				panic(r)
			}
		}
	}()
	u := &unparser{prec: map[*Node]int{}, ignores: map[int]string{}}
	return u.visit(n), nil
}

func (u *unparser) visit(n *Node) string {
	u.out = nil
	u.traverse(n)
	return strings.Join(u.out, "")
}

func (u *unparser) write(s ...string) { u.out = append(u.out, s...) }

func (u *unparser) fill(text string) {
	if u.inInteractive && u.indent == 0 {
		if len(u.out) > 0 {
			u.write("; ")
		}
		u.write(text)
		return
	}
	u.fillBlock(text)
}

// Starts a new indented line even at the top level of interactive input, as fill with allow_semicolon False
func (u *unparser) fillBlock(text string) {
	if len(u.out) > 0 {
		u.write("\n")
	}
	u.write(strings.Repeat("    ", u.indent) + text)
}

func (u *unparser) maybeNewline() {
	if len(u.out) > 0 {
		u.write("\n")
	}
}

func (u *unparser) block(extra string, body func()) {
	u.write(":")
	if extra != "" {
		u.write(extra)
	}
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

func (u *unparser) interleave(sep string, nodes []*Node, f func(*Node)) {
	for i, n := range nodes {
		if i > 0 {
			u.write(sep)
		}
		f(n)
	}
}

func (u *unparser) itemsView(nodes []*Node) {
	if len(nodes) == 1 {
		u.traverse(nodes[0])
		u.write(",")
		return
	}
	u.interleave(", ", nodes, u.traverse)
}

func (u *unparser) traverseAll(nodes []*Node) {
	for _, n := range nodes {
		u.traverse(n)
	}
}

func (u *unparser) typeComment(n *Node) string {
	if c, ok := u.ignores[n.Lineno]; ok && c != "" {
		return " # type: " + c
	}
	if c, ok := n.Str("type_comment"); ok {
		return " # type: " + c
	}
	return ""
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
		u.fillBlock("")
		if k, _ := d.Str("kind"); k == "u" {
			u.write("u")
		}
		u.writeStrAvoidingBackslashes(d.Get("value").(string), multiQuotes)
		u.traverseAll(n.Children("body")[1:])
		return
	}
	u.traverseAll(n.Children("body"))
}

var unaryOps = map[string]string{"Invert": "~", "Not": "not", "UAdd": "+", "USub": "-"}

var binOpText = map[string]string{"Add": "+", "Sub": "-", "Mult": "*", "MatMult": "@", "Div": "/", "Mod": "%", "LShift": "<<", "RShift": ">>", "BitOr": "|", "BitXor": "^", "BitAnd": "&", "FloorDiv": "//", "Pow": "**"}

var binOpPrec = map[string]int{"+": precArith, "-": precArith, "*": precTerm, "@": precTerm, "/": precTerm, "%": precTerm, "<<": precShift, ">>": precShift, "|": precBOr, "^": precBXor, "&": precBAnd, "//": precTerm, "**": precPower}

var cmpOpText = map[string]string{"Eq": "==", "NotEq": "!=", "Lt": "<", "LtE": "<=", "Gt": ">", "GtE": ">=", "Is": "is", "IsNot": "is not", "In": "in", "NotIn": "not in"}

func (u *unparser) traverse(n *Node) {
	switch n.Type {
	case "Module":
		u.ignores = map[int]string{}
		for _, ig := range n.Children("type_ignores") {
			tag, _ := ig.Str("tag")
			line, _ := ig.Int("lineno")
			u.ignores[line] = "ignore" + tag
		}
		u.docstringAndBody(n)
		u.ignores = map[int]string{}
	case "Interactive":
		u.inInteractive = true
		u.docstringAndBody(n)
		u.inInteractive = false
	case "Expression":
		u.traverse(n.Child("body"))
	case "FunctionType":
		u.write("(")
		u.interleave(", ", n.Children("argtypes"), u.traverse)
		u.write(")")
		u.write(" -> ")
		u.traverse(n.Child("returns"))
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
		u.interleave(", ", n.Children("names"), u.traverse)
	case "ImportFrom":
		u.fill("from ")
		level, _ := n.Int("level")
		u.write(strings.Repeat(".", level))
		if m, ok := n.Str("module"); ok && m != "" {
			u.write(m)
		}
		u.write(" import ")
		u.interleave(", ", n.Children("names"), u.traverse)
	case "Assign":
		u.fill("")
		for _, t := range n.Children("targets") {
			u.setPrec(precTuple, t)
			u.traverse(t)
			u.write(" = ")
		}
		u.traverse(n.Child("value"))
		if c := u.typeComment(n); c != "" {
			u.write(c)
		}
	case "AugAssign":
		u.fill("")
		u.traverse(n.Child("target"))
		u.write(" " + binOpText[n.Child("op").Type] + "= ")
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
		u.interleave(", ", n.Children("targets"), u.traverse)
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
				panic(&UnparseError{"Node can't be used without a value attribute."})
			}
			u.setPrec(precAtom, v)
			u.traverse(v)
		})
	case "Raise":
		u.fill("raise")
		exc, cause := n.Child("exc"), n.Child("cause")
		if exc == nil {
			if cause != nil {
				panic(&UnparseError{"Node can't use cause without an exception."})
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
		u.fillBlock("try")
		u.block("", func() { u.traverseAll(n.Children("body")) })
		u.traverseAll(n.Children("handlers"))
		if o := n.Children("orelse"); len(o) > 0 {
			u.fillBlock("else")
			u.block("", func() { u.traverseAll(o) })
		}
		if f := n.Children("finalbody"); len(f) > 0 {
			u.fillBlock("finally")
			u.block("", func() { u.traverseAll(f) })
		}
		u.inTryStar = prev
	case "ExceptHandler":
		if u.inTryStar {
			u.fillBlock("except*")
		} else {
			u.fillBlock("except")
		}
		if t := n.Child("type"); t != nil {
			u.write(" ")
			u.traverse(t)
		}
		if name, ok := n.Str("name"); ok && name != "" {
			u.write(" as ")
			u.write(name)
		}
		u.block("", func() { u.traverseAll(n.Children("body")) })
	case "ClassDef":
		u.maybeNewline()
		for _, d := range n.Children("decorator_list") {
			u.fillBlock("@")
			u.traverse(d)
		}
		name, _ := n.Str("name")
		u.fillBlock("class " + name)
		u.typeParams(n.Children("type_params"))
		bases, kws := n.Children("bases"), n.Children("keywords")
		u.delimitIf("(", ")", len(bases) > 0 || len(kws) > 0, func() {
			u.interleave(", ", append(append([]*Node{}, bases...), kws...), u.traverse)
		})
		u.block("", func() { u.docstringAndBody(n) })
	case "FunctionDef", "AsyncFunctionDef":
		u.maybeNewline()
		for _, d := range n.Children("decorator_list") {
			u.fillBlock("@")
			u.traverse(d)
		}
		prefix := "def"
		if n.Type == "AsyncFunctionDef" {
			prefix = "async def"
		}
		name, _ := n.Str("name")
		u.fillBlock(prefix + " " + name)
		u.typeParams(n.Children("type_params"))
		u.write("(")
		u.traverse(n.Child("args"))
		u.write(")")
		if r := n.Child("returns"); r != nil {
			u.write(" -> ")
			u.traverse(r)
		}
		u.block(u.typeComment(n), func() { u.docstringAndBody(n) })
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
			u.fillBlock("for ")
		} else {
			u.fillBlock("async for ")
		}
		u.setPrec(precTuple, n.Child("target"))
		u.traverse(n.Child("target"))
		u.write(" in ")
		u.traverse(n.Child("iter"))
		u.block(u.typeComment(n), func() { u.traverseAll(n.Children("body")) })
		if o := n.Children("orelse"); len(o) > 0 {
			u.fillBlock("else")
			u.block("", func() { u.traverseAll(o) })
		}
	case "If":
		u.fillBlock("if ")
		u.traverse(n.Child("test"))
		u.block("", func() { u.traverseAll(n.Children("body")) })
		for o := n.Children("orelse"); len(o) == 1 && o[0].Is("If"); o = n.Children("orelse") {
			n = o[0]
			u.fillBlock("elif ")
			u.traverse(n.Child("test"))
			u.block("", func() { u.traverseAll(n.Children("body")) })
		}
		if o := n.Children("orelse"); len(o) > 0 {
			u.fillBlock("else")
			u.block("", func() { u.traverseAll(o) })
		}
	case "While":
		u.fillBlock("while ")
		u.traverse(n.Child("test"))
		u.block("", func() { u.traverseAll(n.Children("body")) })
		if o := n.Children("orelse"); len(o) > 0 {
			u.fillBlock("else")
			u.block("", func() { u.traverseAll(o) })
		}
	case "With", "AsyncWith":
		if n.Type == "With" {
			u.fillBlock("with ")
		} else {
			u.fillBlock("async with ")
		}
		u.interleave(", ", n.Children("items"), u.traverse)
		u.block(u.typeComment(n), func() { u.traverseAll(n.Children("body")) })
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
		if _, ok := v.(Ellipsis); ok {
			u.write("...")
			return
		}
		if t, ok := v.(pyrepr.Tuple); ok {
			u.write("(")
			if len(t) == 1 {
				u.writeConstant(t[0])
				u.write(",")
			} else {
				for i, e := range t {
					if i > 0 {
						u.write(", ")
					}
					u.writeConstant(e)
				}
			}
			u.write(")")
			return
		}
		if k, _ := n.Str("kind"); k == "u" {
			u.write("u")
		}
		u.writeConstant(v)
	case "List":
		u.write("[")
		u.interleave(", ", n.Children("elts"), u.traverse)
		u.write("]")
	case "ListComp", "GeneratorExp", "SetComp":
		delims := map[string][2]string{"ListComp": {"[", "]"}, "GeneratorExp": {"(", ")"}, "SetComp": {"{", "}"}}[n.Type]
		u.write(delims[0])
		u.traverse(n.Child("elt"))
		u.traverseAll(n.Children("generators"))
		u.write(delims[1])
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
		u.setPrec(precNext(precTest), append([]*Node{n.Child("iter")}, n.Children("ifs")...)...)
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
			u.interleave(", ", elts, u.traverse)
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
		op := unaryOps[n.Child("op").Type]
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
		op := binOpText[n.Child("op").Type]
		p := binOpPrec[op]
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
			u.setPrec(precNext(precCmp), append([]*Node{n.Child("left")}, n.Children("comparators")...)...)
			u.traverse(n.Child("left"))
			ops, comps := n.Children("ops"), n.Children("comparators")
			for i := range min(len(ops), len(comps)) {
				u.write(" " + cmpOpText[ops[i].Type] + " ")
				u.traverse(comps[i])
			}
		})
	case "BoolOp":
		op := "and"
		p := precAnd
		if n.Child("op").Type == "Or" {
			op, p = "or", precOr
		}
		u.requireParens(p, n, func() {
			level := p
			u.interleave(" "+op+" ", n.Children("values"), func(v *Node) {
				level = precNext(level)
				u.setPrec(level, v)
				u.traverse(v)
			})
		})
	case "Attribute":
		v := n.Child("value")
		u.setPrec(precAtom, v)
		u.traverse(v)
		if _, isInt := v.Get("value").(*big.Int); v.Is("Constant") && isInt {
			u.write(" ")
		}
		attr, _ := n.Str("attr")
		u.write("." + attr)
	case "Call":
		u.setPrec(precAtom, n.Child("func"))
		u.traverse(n.Child("func"))
		u.write("(")
		u.interleave(", ", append(append([]*Node{}, n.Children("args")...), n.Children("keywords")...), u.traverse)
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
		u.fillBlock("match ")
		u.traverse(n.Child("subject"))
		u.block("", func() { u.traverseAll(n.Children("cases")) })
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
			u.write(name, "=")
		} else {
			u.write("**")
		}
		u.traverse(n.Child("value"))
	case "Lambda":
		u.requireParens(precTest, n, func() {
			u.write("lambda")
			saved := u.out
			u.out = nil
			u.traverse(n.Child("args"))
			buf := u.out
			u.out = saved
			if len(buf) > 0 {
				u.write(" ")
				u.write(buf...)
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
		u.fillBlock("case ")
		u.traverse(n.Child("pattern"))
		if g := n.Child("guard"); g != nil {
			u.write(" if ")
			u.traverse(g)
		}
		u.block("", func() { u.traverseAll(n.Children("body")) })
	case "MatchValue":
		u.traverse(n.Child("value"))
	case "MatchSingleton":
		u.writeConstant(n.Get("value"))
	case "MatchSequence":
		u.write("[")
		u.interleave(", ", n.Children("patterns"), u.traverse)
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
			panic(&UnparseError{fmt.Sprintf("zip() argument 2 is %s than argument 1", map[bool]string{true: "shorter", false: "longer"}[len(pats) < len(keys)])})
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
		u.interleave(", ", pats, u.traverse)
		attrs, kwPats := n.Names("kwd_attrs"), n.Children("kwd_patterns")
		if len(attrs) > 0 {
			if len(attrs) != len(kwPats) {
				panic(&UnparseError{fmt.Sprintf("zip() argument 2 is %s than argument 1", map[bool]string{true: "shorter", false: "longer"}[len(kwPats) < len(attrs)])})
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
			u.interleave(" | ", n.Children("patterns"), u.traverse)
		})
	default:
		panic(&UnparseError{"Unexpected node " + n.Type})
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
		u.interleave(", ", params, u.traverse)
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
	all := append(append([]*Node{}, posonly...), n.Children("args")...)
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

func (u *unparser) writeConstant(v any) {
	switch x := v.(type) {
	case float64, Complex:
		s, _ := ConstantRepr(x)
		s = strings.ReplaceAll(s, "inf", infStr)
		s = strings.ReplaceAll(s, "nan", "("+infStr+"-"+infStr+")")
		u.write(s)
		return
	}
	s, err := ConstantRepr(v)
	if err != nil {
		panic(err)
	}
	u.write(s)
}

// Escapes a character the way unicode_escape encodes backslashes and non printable characters
func escapeChar(r rune, special bool) string {
	if !special && (r == '\n' || r == '\t') {
		return string(r)
	}
	if r == '\\' {
		return `\\`
	}
	if pystr.IsPrintable(r) {
		return pystr.FromRunes([]rune{r})
	}
	switch r {
	case '\t':
		return `\t`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	}
	switch {
	case r < 0x100:
		return fmt.Sprintf(`\x%02x`, r)
	case r < 0x10000:
		return fmt.Sprintf(`\u%04x`, r)
	}
	return fmt.Sprintf(`\U%08x`, r)
}

// String literal text and usable quote styles minimizing escapes, as _str_literal_helper
func strLiteralHelper(s string, quotes []string, special bool) (string, []string) {
	var b strings.Builder
	for _, r := range pystr.Runes(s) {
		b.WriteString(escapeChar(r, special))
	}
	escaped := b.String()
	possible := append([]string{}, quotes...)
	if strings.Contains(escaped, "\n") {
		possible = slices.DeleteFunc(possible, func(q string) bool { return !slices.Contains(multiQuotes, q) })
	}
	possible = slices.DeleteFunc(possible, func(q string) bool { return strings.Contains(escaped, q) })
	if len(possible) == 0 {
		r := pyrepr.StrRepr(s)
		quote := string(r[0])
		for _, q := range quotes {
			if strings.Contains(q, string(r[0])) {
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
	u.write(qs[0] + text + qs[0])
}

type ftPart struct {
	text     string
	constant bool
}

func (u *unparser) ftstringHelper(parts []ftPart) {
	var newParts []string
	quotes := append([]string{}, allQuotes...)
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
		if nq := slices.DeleteFunc(append([]string{}, quotes...), func(q string) bool { return strings.Contains(value, q) }); len(nq) > 0 {
			quotes = nq
		}
		newParts = append(newParts, value)
	}
	if fallback {
		quotes = []string{"'''"}
		newParts = nil
		for _, p := range parts {
			value := p.text
			if p.constant {
				r := pyrepr.StrRepr("\"" + value)
				value = r[2 : len(r)-1]
			}
			newParts = append(newParts, value)
		}
	}
	u.write(quotes[0] + strings.Join(newParts, "") + quotes[0])
}

func (u *unparser) writeFTString(values []*Node, prefix string) {
	u.write(prefix)
	var parts []ftPart
	for _, v := range values {
		saved := u.out
		u.out = nil
		u.ftInner(v, false)
		buf := strings.Join(u.out, "")
		u.out = saved
		parts = append(parts, ftPart{buf, v.Is("Constant")})
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
			r, _ := n.Repr()
			panic(&UnparseError{"Unexpected node inside JoinedStr, " + r})
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
		r, _ := n.Repr()
		panic(&UnparseError{"Unexpected node inside JoinedStr, " + r})
	}
}

func (u *unparser) writeInterpolation(n *Node, useStr bool) {
	u.write("{")
	var expr string
	if useStr {
		expr, _ = n.Get("str").(string)
	} else {
		inner := &unparser{prec: map[*Node]int{}, ignores: map[int]string{}}
		inner.setPrec(precNext(precTest), n.Child("value"))
		expr = inner.visit(n.Child("value"))
	}
	if strings.HasPrefix(expr, "{") {
		u.write(" ")
	}
	u.write(expr)
	if conv, _ := n.Int("conversion"); conv != -1 {
		u.write("!" + string(rune(conv)))
	}
	if spec := n.Child("format_spec"); spec != nil {
		u.write(":")
		u.ftInner(spec, true)
	}
	u.write("}")
}
