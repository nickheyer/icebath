package python

// Calls visit on n and each node below it in source order, skipping children when visit returns false
func Walk(n Node, visit func(Node) bool) {
	if n == nil {
		return
	}
	stack := []Node{n}
	var kids []Node
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !visit(x) {
			continue
		}
		kids = appendChildren(kids[:0], x, false)
		for i := len(kids) - 1; i >= 0; i-- {
			stack = append(stack, kids[i])
		}
	}
}

// Direct children of a node in the field order of the reference tree, as its generic visitor reaches them
func Children(n Node) []Node {
	return appendChildren(nil, n, true)
}

// Most nodes on one root to leaf path of the tree
func treeDepth(n Node) int {
	if n == nil {
		return 0
	}
	type frame struct {
		n     Node
		depth int
	}
	deepest := 0
	stack := []frame{{n, 1}}
	var kids []Node
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		deepest = max(deepest, f.depth)
		kids = appendChildren(kids[:0], f.n, false)
		for _, k := range kids {
			stack = append(stack, frame{k, f.depth + 1})
		}
	}
	return deepest
}

func addExpr(dst []Node, e Expr) []Node {
	if e != nil {
		dst = append(dst, e)
	}
	return dst
}

func addExprs(dst []Node, es []Expr) []Node {
	for _, e := range es {
		dst = addExpr(dst, e)
	}
	return dst
}

func addStmts(dst []Node, ss []Stmt) []Node {
	for _, s := range ss {
		dst = append(dst, s)
	}
	return dst
}

func addPatterns(dst []Node, ps []Pattern) []Node {
	for _, pt := range ps {
		dst = append(dst, pt)
	}
	return dst
}

func addTypeParams(dst []Node, ts []TypeParam) []Node {
	for _, t := range ts {
		dst = append(dst, t)
	}
	return dst
}

func addArg(dst []Node, a *Arg) []Node {
	if a != nil {
		dst = append(dst, a)
	}
	return dst
}

func addComprehensions(dst []Node, cs []*Comprehension) []Node {
	for _, c := range cs {
		dst = append(dst, c)
	}
	return dst
}

// Appends positional arguments and keywords merged by their start positions
func addArgsAndKeywords(dst []Node, args []Expr, kws []*Keyword) []Node {
	i, j := 0, 0
	for i < len(args) || j < len(kws) {
		if j >= len(kws) || i < len(args) && before(args[i].Position(), kws[j].Pos) {
			dst = append(dst, args[i])
			i++
		} else {
			dst = append(dst, kws[j])
			j++
		}
	}
	return dst
}

func before(a, b Pos) bool {
	return a.Line < b.Line || a.Line == b.Line && a.Col < b.Col
}

// Appends parameters with each default right after the parameter it belongs to
func addArguments(dst []Node, a *Arguments) []Node {
	positional := append(append([]*Arg{}, a.PosOnly...), a.Args...)
	firstDefault := len(positional) - len(a.Defaults)
	for i, arg := range positional {
		dst = append(dst, arg)
		if i >= firstDefault {
			dst = addExpr(dst, a.Defaults[i-firstDefault])
		}
	}
	dst = addArg(dst, a.Vararg)
	for i, arg := range a.KwOnly {
		dst = append(dst, arg)
		if i < len(a.KwDefaults) {
			dst = addExpr(dst, a.KwDefaults[i])
		}
	}
	return addArg(dst, a.Kwarg)
}

// Appends the direct children of a node in field order or else in source order
func appendChildren(dst []Node, n Node, fieldOrder bool) []Node {
	if fieldOrder {
		if out, ok := appendFieldOrder(dst, n); ok {
			return out
		}
	}
	switch x := n.(type) {
	case *Module:
		return addStmts(dst, x.Body)
	case *FunctionDef:
		dst = addExprs(dst, x.Decorators)
		dst = addTypeParams(dst, x.TypeParams)
		dst = append(dst, x.Args)
		dst = addExpr(dst, x.Returns)
		return addStmts(dst, x.Body)
	case *ClassDef:
		dst = addExprs(dst, x.Decorators)
		dst = addTypeParams(dst, x.TypeParams)
		dst = addArgsAndKeywords(dst, x.Bases, x.Keywords)
		return addStmts(dst, x.Body)
	case *Return:
		return addExpr(dst, x.Value)
	case *Delete:
		return addExprs(dst, x.Targets)
	case *Assign:
		dst = addExprs(dst, x.Targets)
		return addExpr(dst, x.Value)
	case *TypeAlias:
		dst = append(dst, x.Name)
		dst = addTypeParams(dst, x.TypeParams)
		return addExpr(dst, x.Value)
	case *AugAssign:
		dst = addExpr(dst, x.Target)
		return addExpr(dst, x.Value)
	case *AnnAssign:
		dst = addExpr(dst, x.Target)
		dst = addExpr(dst, x.Annotation)
		return addExpr(dst, x.Value)
	case *For:
		dst = addExpr(dst, x.Target)
		dst = addExpr(dst, x.Iter)
		dst = addStmts(dst, x.Body)
		return addStmts(dst, x.Orelse)
	case *While:
		dst = addExpr(dst, x.Test)
		dst = addStmts(dst, x.Body)
		return addStmts(dst, x.Orelse)
	case *If:
		dst = addExpr(dst, x.Test)
		dst = addStmts(dst, x.Body)
		return addStmts(dst, x.Orelse)
	case *With:
		for _, it := range x.Items {
			dst = append(dst, it)
		}
		return addStmts(dst, x.Body)
	case *Match:
		dst = addExpr(dst, x.Subject)
		for _, c := range x.Cases {
			dst = append(dst, c)
		}
		return dst
	case *Raise:
		dst = addExpr(dst, x.Exc)
		return addExpr(dst, x.Cause)
	case *Try:
		dst = addStmts(dst, x.Body)
		for _, h := range x.Handlers {
			dst = append(dst, h)
		}
		dst = addStmts(dst, x.Orelse)
		return addStmts(dst, x.Finalbody)
	case *Assert:
		dst = addExpr(dst, x.Test)
		return addExpr(dst, x.Msg)
	case *Import:
		for _, a := range x.Names {
			dst = append(dst, a)
		}
		return dst
	case *ImportFrom:
		for _, a := range x.Names {
			dst = append(dst, a)
		}
		return dst
	case *ExprStmt:
		return addExpr(dst, x.Value)
	case *BoolOp:
		return addExprs(dst, x.Values)
	case *NamedExpr:
		dst = addExpr(dst, x.Target)
		return addExpr(dst, x.Value)
	case *BinOp:
		dst = addExpr(dst, x.Left)
		return addExpr(dst, x.Right)
	case *UnaryOp:
		return addExpr(dst, x.Operand)
	case *Lambda:
		dst = append(dst, x.Args)
		return addExpr(dst, x.Body)
	case *IfExp:
		dst = addExpr(dst, x.Body)
		dst = addExpr(dst, x.Test)
		return addExpr(dst, x.Orelse)
	case *Dict:
		for i := range x.Values {
			if i < len(x.Keys) {
				dst = addExpr(dst, x.Keys[i])
			}
			dst = addExpr(dst, x.Values[i])
		}
		return dst
	case *Set:
		return addExprs(dst, x.Elts)
	case *ListComp:
		dst = addExpr(dst, x.Elt)
		return addComprehensions(dst, x.Generators)
	case *SetComp:
		dst = addExpr(dst, x.Elt)
		return addComprehensions(dst, x.Generators)
	case *GeneratorExp:
		dst = addExpr(dst, x.Elt)
		return addComprehensions(dst, x.Generators)
	case *DictComp:
		dst = addExpr(dst, x.Key)
		dst = addExpr(dst, x.Value)
		return addComprehensions(dst, x.Generators)
	case *Await:
		return addExpr(dst, x.Value)
	case *Yield:
		return addExpr(dst, x.Value)
	case *YieldFrom:
		return addExpr(dst, x.Value)
	case *Compare:
		dst = addExpr(dst, x.Left)
		return addExprs(dst, x.Comparators)
	case *Call:
		dst = addExpr(dst, x.Func)
		return addArgsAndKeywords(dst, x.Args, x.Keywords)
	case *FormattedValue:
		dst = addExpr(dst, x.Value)
		return addExpr(dst, x.FormatSpec)
	case *Interpolation:
		dst = addExpr(dst, x.Value)
		return addExpr(dst, x.FormatSpec)
	case *JoinedStr:
		return addExprs(dst, x.Values)
	case *TemplateStr:
		return addExprs(dst, x.Values)
	case *Attribute:
		return addExpr(dst, x.Value)
	case *Subscript:
		dst = addExpr(dst, x.Value)
		return addExpr(dst, x.Slice)
	case *Starred:
		return addExpr(dst, x.Value)
	case *List:
		return addExprs(dst, x.Elts)
	case *Tuple:
		return addExprs(dst, x.Elts)
	case *Slice:
		dst = addExpr(dst, x.Lower)
		dst = addExpr(dst, x.Upper)
		return addExpr(dst, x.Step)
	case *MatchValue:
		return addExpr(dst, x.Value)
	case *MatchSequence:
		return addPatterns(dst, x.Patterns)
	case *MatchMapping:
		for i := range x.Patterns {
			if i < len(x.Keys) {
				dst = addExpr(dst, x.Keys[i])
			}
			dst = append(dst, x.Patterns[i])
		}
		return dst
	case *MatchClass:
		dst = addExpr(dst, x.Cls)
		dst = addPatterns(dst, x.Patterns)
		return addPatterns(dst, x.KwdPatterns)
	case *MatchAs:
		if x.Pattern != nil {
			dst = append(dst, x.Pattern)
		}
		return dst
	case *MatchOr:
		return addPatterns(dst, x.Patterns)
	case *Comprehension:
		dst = addExpr(dst, x.Target)
		dst = addExpr(dst, x.Iter)
		return addExprs(dst, x.Ifs)
	case *ExceptHandler:
		dst = addExpr(dst, x.Type)
		return addStmts(dst, x.Body)
	case *Arguments:
		return addArguments(dst, x)
	case *Arg:
		return addExpr(dst, x.Annotation)
	case *Keyword:
		return addExpr(dst, x.Value)
	case *WithItem:
		dst = addExpr(dst, x.ContextExpr)
		return addExpr(dst, x.OptionalVars)
	case *MatchCase:
		dst = append(dst, x.Pattern)
		dst = addExpr(dst, x.Guard)
		return addStmts(dst, x.Body)
	case *TypeVar:
		dst = addExpr(dst, x.Bound)
		return addExpr(dst, x.Default)
	case *ParamSpec:
		return addExpr(dst, x.Default)
	case *TypeVarTuple:
		return addExpr(dst, x.Default)
	}
	return dst
}

// Children of the node kinds whose reference field order differs from source order, false for every other kind
func appendFieldOrder(dst []Node, n Node) ([]Node, bool) {
	switch x := n.(type) {
	case *FunctionDef:
		dst = append(dst, x.Args)
		dst = addStmts(dst, x.Body)
		dst = addExprs(dst, x.Decorators)
		dst = addExpr(dst, x.Returns)
		return addTypeParams(dst, x.TypeParams), true
	case *ClassDef:
		dst = addExprs(dst, x.Bases)
		for _, k := range x.Keywords {
			dst = append(dst, k)
		}
		dst = addStmts(dst, x.Body)
		dst = addExprs(dst, x.Decorators)
		return addTypeParams(dst, x.TypeParams), true
	case *IfExp:
		dst = addExpr(dst, x.Test)
		dst = addExpr(dst, x.Body)
		return addExpr(dst, x.Orelse), true
	case *Dict:
		dst = addExprs(dst, x.Keys)
		return addExprs(dst, x.Values), true
	case *Call:
		dst = addExpr(dst, x.Func)
		dst = addExprs(dst, x.Args)
		for _, k := range x.Keywords {
			dst = append(dst, k)
		}
		return dst, true
	case *Arguments:
		for _, a := range x.PosOnly {
			dst = append(dst, a)
		}
		for _, a := range x.Args {
			dst = append(dst, a)
		}
		dst = addArg(dst, x.Vararg)
		for _, a := range x.KwOnly {
			dst = append(dst, a)
		}
		dst = addExprs(dst, x.KwDefaults)
		dst = addArg(dst, x.Kwarg)
		return addExprs(dst, x.Defaults), true
	case *MatchMapping:
		dst = addExprs(dst, x.Keys)
		return addPatterns(dst, x.Patterns), true
	}
	return dst, false
}
