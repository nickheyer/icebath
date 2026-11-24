package pythonutil

import (
	"math"
	"strconv"
)

// Deepest stack of loops, try blocks and with blocks one function may nest
const maxStaticBlocks = 21

type fblockKind uint8

const (
	fbWhileLoop fblockKind = iota
	fbForLoop
	fbTryExcept
	fbFinallyTry
	fbFinallyEnd
	fbWith
	fbAsyncWith
	fbHandlerCleanup
	fbPopValue
	fbExceptionHandler
	fbExceptionGroupHandler
	fbAsyncComprehensionGenerator
	fbStopIteration
	fbInlinedComprehension
)

type fblock struct {
	kind  fblockKind
	loc   srcLoc
	datum []*Node
}

type scopeKind uint8

const (
	scopeModule scopeKind = iota
	scopeClass
	scopeFunction
	scopeAsyncFunction
	scopeLambda
	scopeComprehension
	scopeAnnotations
)

// The block being checked with its stack of loop, try and with blocks
type codeUnit struct {
	ste           *symEntry
	scope         scopeKind
	fblocks       []fblock
	deferred      []*Node
	inInlinedComp int
	argCount      int
}

// Walks a module in code generation order and raises the errors Python's code generator raises, emitting no code
type codegen struct {
	st       *symtable
	lx       *lexer
	filename string
	future   futureInfo
	u        *codeUnit
	units    []*codeUnit
	finals   map[finalWalk]bool
	finalIDs map[*Node]int
}

// One walk of a finally body: its scope, its first statement and the shape of the blocks around it
type finalWalk struct {
	ste   *symEntry
	body  *Node
	shape string
}

// First error the code generator raises for the module, nil when there is none
func runCodegen(mod *Node, st *symtable, lx *lexer, filename string, future futureInfo) (err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(error)
			if !ok {
				panic(r)
			}
			err = e
		}
	}()
	c := &codegen{st: st, lx: lx, filename: filename, future: future}
	c.enterScope(mod, scopeModule, 0)
	c.body(mod.Children("body"))
	c.exitScope()
	return nil
}

func (c *codegen) fail(loc srcLoc, msg string) {
	panic(rangedSyntaxError(msg, loc, c.lx, c.filename))
}

func (c *codegen) enterScope(key any, kind scopeKind, argCount int) {
	ste := c.st.blocks[key]
	if ste == nil {
		panic(internalError("no symbol table entry for a scope"))
	}
	if c.u != nil {
		c.units = append(c.units, c.u)
	}
	c.u = &codeUnit{ste: ste, scope: kind, argCount: argCount}
}

func (c *codegen) exitScope() {
	if n := len(c.units); n > 0 {
		c.u, c.units = c.units[n-1], c.units[:n-1]
		return
	}
	c.u = nil
}

func (c *codegen) pushFBlock(loc srcLoc, kind fblockKind, datum []*Node) {
	if len(c.u.fblocks) >= maxStaticBlocks {
		c.fail(loc, "too many statically nested blocks")
	}
	c.u.fblocks = append(c.u.fblocks, fblock{kind, loc, datum})
}

func (c *codegen) popFBlock() {
	c.u.fblocks = c.u.fblocks[:len(c.u.fblocks)-1]
}

func (c *codegen) stmts(list []*Node) {
	for _, s := range list {
		c.stmt(s)
	}
}

func (c *codegen) exprs(list []*Node) {
	for _, e := range list {
		c.expr(e)
	}
}

// Generates a module or class body, then the annotations of its annotated assignments in their own scope
func (c *codegen) body(stmts []*Node) {
	if len(stmts) == 0 {
		return
	}
	if c.u.ste.hasDocstring {
		stmts = stmts[1:]
	}
	c.stmts(stmts)
	if c.future.annotations || len(c.u.deferred) == 0 {
		return
	}
	deferred := c.u.deferred
	c.enterScope(annotationsKey{c.u.ste}, scopeAnnotations, 1)
	for _, s := range deferred {
		c.expr(s.Child("annotation"))
	}
	c.exitScope()
}

func (c *codegen) defaultArguments(args *Node) {
	c.exprs(args.Children("defaults"))
	defaults := args.Children("kw_defaults")
	for i := range args.Children("kwonlyargs") {
		if i < len(defaults) && defaults[i] != nil {
			c.expr(defaults[i])
		}
	}
}

// Checks an annotation the function evaluates, none under from __future__ import annotations where it stays text
func (c *codegen) argAnnotation(annotation *Node) {
	if annotation == nil || c.future.annotations {
		return
	}
	if annotation.Is("Starred") {
		annotation = annotation.Child("value")
	}
	c.expr(annotation)
}

func (c *codegen) argAnnotations(args []*Node) {
	for _, a := range args {
		c.argAnnotation(a.Child("annotation"))
	}
}

func (c *codegen) functionAnnotations(args, returns *Node) {
	ste := c.st.blocks[args]
	if ste == nil || !ste.annotationsUsed {
		return
	}
	c.enterScope(args, scopeAnnotations, 1)
	c.argAnnotations(args.Children("args"))
	c.argAnnotations(args.Children("posonlyargs"))
	if v := args.Child("vararg"); v != nil {
		c.argAnnotation(v.Child("annotation"))
	}
	c.argAnnotations(args.Children("kwonlyargs"))
	if k := args.Child("kwarg"); k != nil {
		c.argAnnotation(k.Child("annotation"))
	}
	c.argAnnotation(returns)
	c.exitScope()
}

func (c *codegen) typeParamBound(e *Node, key any, allowStarred bool) {
	c.enterScope(key, scopeAnnotations, 1)
	if allowStarred && e.Is("Starred") {
		e = e.Child("value")
	}
	c.expr(e)
	c.exitScope()
}

func (c *codegen) typeParams(list []*Node) {
	seenDefault := false
	for _, tp := range list {
		name, _ := tp.Str("name")
		def := tp.Child("default_value")
		switch tp.Type {
		case "TypeVar":
			if bound := tp.Child("bound"); bound != nil {
				c.typeParamBound(bound, tp, false)
			}
			if def != nil {
				seenDefault = true
				c.typeParamBound(def, defaultKey{tp}, false)
			}
		case "TypeVarTuple":
			if def != nil {
				c.typeParamBound(def, tp, true)
				seenDefault = true
			}
		case "ParamSpec":
			if def != nil {
				c.typeParamBound(def, tp, false)
				seenDefault = true
			}
		}
		if def == nil && seenDefault {
			c.fail(locOf(tp), "non-default type parameter '"+name+"' follows default type parameter")
		}
	}
}

func (c *codegen) function(s *Node, async bool) {
	args := s.Child("args")
	c.exprs(s.Children("decorator_list"))
	c.defaultArguments(args)
	typeParams := s.Children("type_params")
	if len(typeParams) > 0 {
		c.enterScope(typeParamsKey{s}, scopeAnnotations, 0)
		c.typeParams(typeParams)
	}
	c.functionAnnotations(args, s.Child("returns"))
	kind := scopeFunction
	if async {
		kind = scopeAsyncFunction
	}
	c.enterScope(s, kind, len(args.Children("args"))+len(args.Children("posonlyargs")))
	body := s.Children("body")
	if c.u.ste.hasDocstring {
		body = body[1:]
	}
	stopIteration := c.u.ste.coroutine || c.u.ste.generator
	if stopIteration {
		c.pushFBlock(noLocation, fbStopIteration, nil)
	}
	c.stmts(body)
	if stopIteration {
		c.popFBlock()
	}
	c.exitScope()
	if len(typeParams) > 0 {
		c.exitScope()
	}
}

func (c *codegen) class(s *Node) {
	c.exprs(s.Children("decorator_list"))
	typeParams := s.Children("type_params")
	if len(typeParams) > 0 {
		c.enterScope(typeParamsKey{s}, scopeAnnotations, 0)
		c.typeParams(typeParams)
	}
	c.enterScope(s, scopeClass, 0)
	c.body(s.Children("body"))
	c.exitScope()
	c.callHelper(s.Children("bases"), s.Children("keywords"))
	if len(typeParams) > 0 {
		c.exitScope()
	}
}

func isConstantSlice(s *Node) bool {
	if !s.Is("Slice") {
		return false
	}
	for _, f := range []string{"lower", "upper", "step"} {
		if part := s.Child(f); part != nil && !part.Is("Constant") {
			return false
		}
	}
	return true
}

func twoElementSlice(s *Node) bool {
	return s.Is("Slice") && !isConstantSlice(s) && s.Child("step") == nil
}

func (c *codegen) sliceTwoParts(s *Node) {
	if lower := s.Child("lower"); lower != nil {
		c.expr(lower)
	}
	if upper := s.Child("upper"); upper != nil {
		c.expr(upper)
	}
}

func (c *codegen) checkAnnExpr(e *Node) {
	c.expr(e)
}

func (c *codegen) checkAnnSubscr(e *Node) {
	switch e.Type {
	case "Slice":
		for _, f := range []string{"lower", "upper", "step"} {
			if part := e.Child(f); part != nil {
				c.checkAnnExpr(part)
			}
		}
	case "Tuple":
		for _, elt := range e.Children("elts") {
			c.checkAnnSubscr(elt)
		}
	default:
		c.checkAnnExpr(e)
	}
}

func (c *codegen) annAssign(s *Node) {
	target, value := s.Child("target"), s.Child("value")
	if value != nil {
		c.expr(value)
		c.expr(target)
	}
	switch target.Type {
	case "Name":
		simple, _ := s.Int("simple")
		if simple != 0 && (c.u.scope == scopeModule || c.u.scope == scopeClass) && !c.future.annotations {
			c.u.deferred = append(c.u.deferred, s)
		}
	case "Attribute":
		if value == nil {
			c.checkAnnExpr(target.Child("value"))
		}
	case "Subscript":
		if value == nil {
			c.checkAnnExpr(target.Child("value"))
			c.checkAnnSubscr(target.Child("slice"))
		}
	}
}

func (c *codegen) augAssign(s *Node) {
	target := s.Child("target")
	switch target.Type {
	case "Attribute":
		c.expr(target.Child("value"))
	case "Subscript":
		c.expr(target.Child("value"))
		if slice := target.Child("slice"); twoElementSlice(slice) {
			c.sliceTwoParts(slice)
		} else {
			c.expr(slice)
		}
	}
	c.expr(s.Child("value"))
}

// Unwinds one frame block for a return, break or continue, generating a finally body again
func (c *codegen) unwindFBlock(ploc *srcLoc, info fblock, preserve bool) {
	switch info.kind {
	case fbFinallyTry:
		if preserve {
			c.pushFBlock(*ploc, fbPopValue, nil)
		}
		c.finalBody(info.datum)
		if preserve {
			c.popFBlock()
		}
		*ploc = noLocation
	case fbWith, fbAsyncWith:
		*ploc = noLocation
	}
}

// Unwinds the frame blocks up to the nearest loop when one is wanted, or all of them, for a return, break or continue
func (c *codegen) unwindFBlockStack(ploc *srcLoc, preserve, wantLoop bool) *fblock {
	n := len(c.u.fblocks)
	if n == 0 {
		return nil
	}
	top := c.u.fblocks[n-1]
	if top.kind == fbExceptionGroupHandler {
		c.fail(*ploc, "'break', 'continue' and 'return' cannot appear in an except* block")
	}
	if wantLoop && (top.kind == fbWhileLoop || top.kind == fbForLoop) {
		return &top
	}
	c.popFBlock()
	c.unwindFBlock(ploc, top, preserve)
	loop := c.unwindFBlockStack(ploc, preserve, wantLoop)
	c.u.fblocks = append(c.u.fblocks, top)
	return loop
}

func (c *codegen) tryFinally(s *Node, star bool) {
	final := s.Children("finalbody")
	c.pushFBlock(locOf(s), fbFinallyTry, final)
	switch {
	case len(s.Children("handlers")) == 0:
		c.stmts(s.Children("body"))
	case star:
		c.tryStarExcept(s)
	default:
		c.tryExcept(s)
	}
	c.popFBlock()
	c.finalBody(final)
	c.pushFBlock(noLocation, fbFinallyEnd, nil)
	c.finalBody(final)
	c.popFBlock()
}

// Walks a finally body unless an earlier walk in the same scope under blocks of the same shape already passed it: the code generator emits a finally body once for each way out of its try statement, so finally bodies nested in finally bodies would otherwise cost time exponential in their depth, while a repeated walk can only end the way the first one did
func (c *codegen) finalBody(body []*Node) {
	if len(body) == 0 {
		return
	}
	key := finalWalk{c.u.ste, body[0], c.blockShape()}
	if c.finals[key] {
		return
	}
	c.stmts(body)
	if c.finals == nil {
		c.finals = map[finalWalk]bool{}
	}
	c.finals[key] = true
}

// Block stack of the current unit reduced to what decides how a walk under it ends: the loops a break or continue stops at, the with blocks and finally bodies that drop the location of a later error, the except* handlers that reject a jump, each finally body with the depth below it, and the depth itself
func (c *codegen) blockShape() string {
	b := make([]byte, 0, 32)
	for i, fb := range c.u.fblocks {
		switch fb.kind {
		case fbWhileLoop, fbForLoop:
			b = append(b, 'L')
		case fbWith, fbAsyncWith:
			b = append(b, 'W')
		case fbExceptionGroupHandler:
			b = append(b, 'G')
		case fbFinallyTry:
			b = append(b, 'T')
			b = strconv.AppendInt(b, int64(c.finalID(fb.datum)), 10)
			b = append(b, '@')
			b = strconv.AppendInt(b, int64(i), 10)
		}
	}
	b = append(b, '#')
	return string(strconv.AppendInt(b, int64(len(c.u.fblocks)), 10))
}

// Small number naming a finally body inside block shapes
func (c *codegen) finalID(body []*Node) int {
	if len(body) == 0 {
		return 0
	}
	id, ok := c.finalIDs[body[0]]
	if !ok {
		if c.finalIDs == nil {
			c.finalIDs = map[*Node]int{}
		}
		id = len(c.finalIDs) + 1
		c.finalIDs[body[0]] = id
	}
	return id
}

func (c *codegen) tryExcept(s *Node) {
	c.pushFBlock(locOf(s), fbTryExcept, nil)
	c.stmts(s.Children("body"))
	c.popFBlock()
	c.stmts(s.Children("orelse"))
	handlers := s.Children("handlers")
	c.pushFBlock(locOf(s), fbExceptionHandler, nil)
	for i, h := range handlers {
		typ := h.Child("type")
		if typ == nil && i < len(handlers)-1 {
			c.fail(locOf(h), "default 'except:' must be last")
		}
		if typ != nil {
			c.expr(typ)
		}
		c.pushFBlock(locOf(h), fbHandlerCleanup, nil)
		c.stmts(h.Children("body"))
		c.popFBlock()
	}
	c.popFBlock()
}

func (c *codegen) tryStarExcept(s *Node) {
	c.pushFBlock(locOf(s), fbTryExcept, nil)
	c.stmts(s.Children("body"))
	c.popFBlock()
	c.pushFBlock(locOf(s), fbExceptionGroupHandler, nil)
	for _, h := range s.Children("handlers") {
		if typ := h.Child("type"); typ != nil {
			c.expr(typ)
		}
		c.pushFBlock(locOf(h), fbHandlerCleanup, nil)
		c.stmts(h.Children("body"))
		c.popFBlock()
	}
	c.popFBlock()
	c.stmts(s.Children("orelse"))
}

func (c *codegen) with(s *Node, pos int, async bool) {
	items := s.Children("items")
	item := items[pos]
	ctx := item.Child("context_expr")
	c.expr(ctx)
	kind := fbWith
	if async {
		kind = fbAsyncWith
	}
	c.pushFBlock(locOf(ctx), kind, nil)
	if v := item.Child("optional_vars"); v != nil {
		c.expr(v)
	}
	if pos+1 == len(items) {
		c.stmts(s.Children("body"))
	} else {
		c.with(s, pos+1, async)
	}
	c.popFBlock()
}

func wildcardPattern(p *Node) bool {
	if !p.Is("MatchAs") {
		return false
	}
	_, named := p.Str("name")
	return !named
}

func (c *codegen) match(s *Node) {
	c.expr(s.Child("subject"))
	cases := s.Children("cases")
	last := cases[len(cases)-1]
	hasDefault := wildcardPattern(last.Child("pattern")) && len(cases) > 1
	n := len(cases)
	if hasDefault {
		n--
	}
	for i := 0; i < n; i++ {
		m := cases[i]
		guard := m.Child("guard")
		pc := &patternCtx{allowIrrefutable: guard != nil || i == len(cases)-1}
		c.pattern(m.Child("pattern"), pc)
		if guard != nil {
			c.jumpIf(guard)
		}
		c.stmts(m.Children("body"))
	}
	if hasDefault {
		if guard := last.Child("guard"); guard != nil {
			c.jumpIf(guard)
		}
		c.stmts(last.Children("body"))
	}
}

// Checks one statement in code generation order
func (c *codegen) stmt(s *Node) {
	switch s.Type {
	case "FunctionDef":
		c.function(s, false)
	case "AsyncFunctionDef":
		c.function(s, true)
	case "ClassDef":
		c.class(s)
	case "TypeAlias":
		typeParams := s.Children("type_params")
		if len(typeParams) > 0 {
			c.enterScope(typeParamsKey{s}, scopeAnnotations, 0)
			c.typeParams(typeParams)
		}
		c.enterScope(s, scopeAnnotations, 1)
		c.expr(s.Child("value"))
		c.exitScope()
		if len(typeParams) > 0 {
			c.exitScope()
		}
	case "Return":
		loc := locOf(s)
		v := s.Child("value")
		preserve := v != nil && !v.Is("Constant")
		ste := c.u.ste
		if !ste.functionLike() {
			c.fail(loc, "'return' outside function")
		}
		if v != nil && ste.coroutine && ste.generator {
			c.fail(loc, "'return' with value in async generator")
		}
		if preserve {
			c.expr(v)
		} else if v != nil {
			loc = locOf(v)
		}
		if v == nil || v.Line != s.Line {
			loc = locOf(s)
		}
		c.unwindFBlockStack(&loc, preserve, false)
	case "Delete":
		c.exprs(s.Children("targets"))
	case "Assign":
		c.expr(s.Child("value"))
		c.exprs(s.Children("targets"))
	case "AugAssign":
		c.augAssign(s)
	case "AnnAssign":
		c.annAssign(s)
	case "For":
		c.pushFBlock(locOf(s), fbForLoop, nil)
		c.expr(s.Child("iter"))
		c.expr(s.Child("target"))
		c.stmts(s.Children("body"))
		c.popFBlock()
		c.stmts(s.Children("orelse"))
	case "AsyncFor":
		c.expr(s.Child("iter"))
		c.pushFBlock(locOf(s), fbForLoop, nil)
		c.expr(s.Child("target"))
		c.stmts(s.Children("body"))
		c.popFBlock()
		c.stmts(s.Children("orelse"))
	case "While":
		c.pushFBlock(locOf(s), fbWhileLoop, nil)
		c.jumpIf(s.Child("test"))
		c.stmts(s.Children("body"))
		c.popFBlock()
		c.stmts(s.Children("orelse"))
	case "If":
		c.jumpIf(s.Child("test"))
		c.stmts(s.Children("body"))
		c.stmts(s.Children("orelse"))
	case "Match":
		c.match(s)
	case "Raise":
		if exc := s.Child("exc"); exc != nil {
			c.expr(exc)
			if cause := s.Child("cause"); cause != nil {
				c.expr(cause)
			}
		}
	case "Try", "TryStar":
		star := s.Type == "TryStar"
		switch {
		case len(s.Children("finalbody")) > 0:
			c.tryFinally(s, star)
		case star:
			c.tryStarExcept(s)
		default:
			c.tryExcept(s)
		}
	case "Assert":
		c.jumpIf(s.Child("test"))
		if msg := s.Child("msg"); msg != nil {
			c.expr(msg)
		}
	case "Expr":
		if v := s.Child("value"); !v.Is("Constant") {
			c.expr(v)
		}
	case "Break", "Continue":
		loc := locOf(s)
		origin := loc
		if c.unwindFBlockStack(&loc, false, true) == nil {
			if s.Type == "Break" {
				c.fail(origin, "'break' outside loop")
			}
			c.fail(origin, "'continue' not properly in loop")
		}
	case "With":
		c.with(s, 0, false)
	case "AsyncWith":
		c.with(s, 0, true)
	}
}

// Checks a condition the way the code generator walks one, through not, and, or and conditional expressions
func (c *codegen) jumpIf(e *Node) {
	switch e.Type {
	case "UnaryOp":
		if e.Child("op").Is("Not") {
			c.jumpIf(e.Child("operand"))
			return
		}
	case "BoolOp":
		for _, v := range e.Children("values") {
			c.jumpIf(v)
		}
		return
	case "IfExp":
		c.jumpIf(e.Child("test"))
		c.jumpIf(e.Child("body"))
		c.jumpIf(e.Child("orelse"))
		return
	case "Compare":
		if len(e.Children("ops")) > 1 {
			c.expr(e.Child("left"))
			c.exprs(e.Children("comparators"))
			return
		}
	}
	c.expr(e)
}

func (c *codegen) validateKeywords(keywords []*Node) {
	if j := firstRepeat(len(keywords), func(i int) (string, bool) { return keywords[i].Str("arg") }); j >= 0 {
		arg, _ := keywords[j].Str("arg")
		c.fail(locOf(keywords[j]), "keyword argument repeated: "+arg)
	}
}

// Index of the repeated name Python reports first among n named items, the second use of the name whose first use comes earliest, -1 when no name repeats; items without a name are skipped
func firstRepeat(n int, name func(int) (string, bool)) int {
	if n < 2 {
		return -1
	}
	if n <= 16 {
		for i := range n {
			a, ok := name(i)
			if !ok {
				continue
			}
			for j := i + 1; j < n; j++ {
				if b, ok := name(j); ok && a == b {
					return j
				}
			}
		}
		return -1
	}
	first := make(map[string]int, n)
	earliest, at := n, -1
	for j := range n {
		s, ok := name(j)
		if !ok {
			continue
		}
		i, seen := first[s]
		if !seen {
			first[s] = j
			continue
		}
		if i < earliest {
			earliest, at = i, j
		}
	}
	return at
}

func (c *codegen) unpackHelper(loc srcLoc, elts []*Node) {
	seenStar := false
	for i, elt := range elts {
		if !elt.Is("Starred") {
			continue
		}
		if seenStar {
			c.fail(loc, "multiple starred expressions in assignment")
		}
		if i >= 1<<8 || len(elts)-i-1 >= math.MaxInt32>>8 {
			c.fail(loc, "too many expressions in star-unpacking assignment")
		}
		seenStar = true
	}
}

// Visits the elements of a display or call, a starred element by its value
func (c *codegen) starUnpack(elts []*Node) {
	for _, elt := range elts {
		if elt.Is("Starred") {
			elt = elt.Child("value")
		}
		c.expr(elt)
	}
}

// Checks the arguments of a call or class statement
func (c *codegen) callHelper(args, keywords []*Node) {
	c.validateKeywords(keywords)
	simple := len(args)+len(keywords)*2 <= 30
	for _, a := range args {
		if a.Is("Starred") {
			simple = false
		}
	}
	for _, k := range keywords {
		if _, ok := k.Str("arg"); !ok {
			simple = false
		}
	}
	if simple {
		c.exprs(args)
		for _, k := range keywords {
			c.expr(k.Child("value"))
		}
		return
	}
	c.starUnpack(args)
	for _, k := range keywords {
		c.expr(k.Child("value"))
	}
}

// Whether a call through an attribute is compiled as a method call
func (c *codegen) methodCall(e *Node) bool {
	meth := e.Child("func")
	if !meth.Is("Attribute") || !meth.Child("ctx").Is("Load") {
		return false
	}
	if v := meth.Child("value"); v.Is("Name") {
		id, _ := v.Str("id")
		if c.st.top.symbol(id)&defImport != 0 {
			return false
		}
	}
	args, keywords := e.Children("args"), e.Children("keywords")
	extra := 0
	if len(keywords) != 0 {
		extra = 1
	}
	if len(args)+len(keywords)+extra >= 30 {
		return false
	}
	for _, a := range args {
		if a.Is("Starred") {
			return false
		}
	}
	for _, k := range keywords {
		if _, ok := k.Str("arg"); !ok {
			return false
		}
	}
	return true
}

// Whether an attribute of a super() call is compiled as a direct super attribute load
func (c *codegen) superCall(attr *Node) bool {
	e := attr.Child("value")
	if !e.Is("Call") || !e.Child("func").Is("Name") {
		return false
	}
	if id, _ := e.Child("func").Str("id"); id != "super" {
		return false
	}
	if name, _ := attr.Str("attr"); name == "__class__" || len(e.Children("keywords")) != 0 {
		return false
	}
	if c.u.ste.scope("super") != scopeGlobalImplicit || c.st.top.scope("super") != 0 {
		return false
	}
	args := e.Children("args")
	if len(args) == 2 {
		return !args[0].Is("Starred") && !args[1].Is("Starred")
	}
	if len(args) != 0 || c.u.argCount == 0 {
		return false
	}
	if c.u.scope == scopeClass {
		return false
	}
	return c.u.ste.scope("__class__") == scopeFree
}

func (c *codegen) comprehensionGenerator(loc srcLoc, e *Node, i int, elt, val *Node, iterOnStack bool) {
	generators := e.Children("generators")
	gen := generators[i]
	async, _ := gen.Int("is_async")
	if !iterOnStack && i > 0 {
		iter := gen.Child("iter")
		if elts := iter.Children("elts"); async == 0 && iter.Is("List", "Tuple") && len(elts) == 1 && !elts[0].Is("Starred") {
			iter = elts[0]
		}
		c.expr(iter)
	}
	if async != 0 {
		c.pushFBlock(loc, fbAsyncComprehensionGenerator, nil)
	}
	c.expr(gen.Child("target"))
	for _, cond := range gen.Children("ifs") {
		c.jumpIf(cond)
	}
	if i+1 < len(generators) {
		c.comprehensionGenerator(loc, e, i+1, elt, val, false)
	} else {
		c.expr(elt)
		if val != nil {
			c.expr(val)
		}
	}
	if async != 0 {
		c.popFBlock()
	}
}

// Checks a comprehension, inline in the current unit or in a scope of its own
func (c *codegen) comprehension(e *Node, elt, val *Node) {
	entry := c.st.blocks[e]
	if entry == nil {
		panic(internalError("no symbol table entry for a comprehension"))
	}
	outermost := e.Children("generators")[0]
	if !entry.compInlined {
		c.enterScope(e, scopeComprehension, 1)
		c.comprehensionGenerator(locOf(e), e, 0, elt, val, false)
		c.exitScope()
		c.expr(outermost.Child("iter"))
		return
	}
	c.expr(outermost.Child("iter"))
	c.u.inInlinedComp++
	pushed := false
	for _, name := range entry.symbols.keys {
		if symbol := entry.symbols.vals[name]; symbol&defLocal != 0 && symbol&defNonlocal == 0 {
			pushed = true
		}
	}
	if pushed {
		c.pushFBlock(locOf(e), fbInlinedComprehension, nil)
	}
	c.comprehensionGenerator(locOf(e), e, 0, elt, val, true)
	if pushed {
		c.popFBlock()
	}
	c.u.inInlinedComp--
}

// Checks one expression in code generation order
func (c *codegen) expr(e *Node) {
	switch e.Type {
	case "NamedExpr":
		c.expr(e.Child("value"))
		c.expr(e.Child("target"))
	case "BoolOp":
		c.exprs(e.Children("values"))
	case "BinOp":
		c.expr(e.Child("left"))
		c.expr(e.Child("right"))
	case "UnaryOp":
		c.expr(e.Child("operand"))
	case "Lambda":
		args := e.Child("args")
		c.defaultArguments(args)
		c.enterScope(e, scopeLambda, len(args.Children("args"))+len(args.Children("posonlyargs")))
		c.expr(e.Child("body"))
		c.exitScope()
	case "IfExp":
		c.jumpIf(e.Child("test"))
		c.expr(e.Child("body"))
		c.expr(e.Child("orelse"))
	case "Dict":
		values := e.Children("values")
		for i, key := range e.Children("keys") {
			if key == nil {
				c.expr(values[i])
				continue
			}
			c.expr(key)
			c.expr(values[i])
		}
	case "Set":
		c.starUnpack(e.Children("elts"))
	case "GeneratorExp", "ListComp", "SetComp":
		c.comprehension(e, e.Child("elt"), nil)
	case "DictComp":
		c.comprehension(e, e.Child("key"), e.Child("value"))
	case "Yield":
		if !c.u.ste.functionLike() {
			c.fail(locOf(e), "'yield' outside function")
		}
		if v := e.Child("value"); v != nil {
			c.expr(v)
		}
	case "YieldFrom":
		if !c.u.ste.functionLike() {
			c.fail(locOf(e), "'yield from' outside function")
		}
		if c.u.scope == scopeAsyncFunction {
			c.fail(locOf(e), "'yield from' inside async function")
		}
		c.expr(e.Child("value"))
	case "Await":
		c.expr(e.Child("value"))
	case "Compare":
		c.expr(e.Child("left"))
		c.exprs(e.Children("comparators"))
	case "Call":
		c.call(e)
	case "JoinedStr":
		c.exprs(e.Children("values"))
	case "TemplateStr":
		for _, v := range e.Children("values") {
			if !v.Is("Interpolation") {
				c.expr(v)
			}
		}
		for _, v := range e.Children("values") {
			if v.Is("Interpolation") {
				c.expr(v)
			}
		}
	case "FormattedValue", "Interpolation":
		c.expr(e.Child("value"))
		if spec := e.Child("format_spec"); spec != nil {
			c.expr(spec)
		}
	case "Attribute":
		if e.Child("ctx").Is("Load") && c.superCall(e) {
			c.superArgs(e.Child("value"))
			return
		}
		c.expr(e.Child("value"))
	case "Subscript":
		c.expr(e.Child("value"))
		if slice := e.Child("slice"); twoElementSlice(slice) && !e.Child("ctx").Is("Del") {
			c.sliceTwoParts(slice)
		} else {
			c.expr(slice)
		}
	case "Starred":
		if e.Child("ctx").Is("Store") {
			c.fail(locOf(e), "starred assignment target must be in a list or tuple")
		}
		c.fail(locOf(e), "can't use starred expression here")
	case "Slice":
		if isConstantSlice(e) {
			return
		}
		c.sliceTwoParts(e)
		if step := e.Child("step"); step != nil {
			c.expr(step)
		}
	case "List", "Tuple":
		elts := e.Children("elts")
		switch e.Child("ctx").Type {
		case "Store":
			c.unpackHelper(locOf(e), elts)
			c.starUnpack(elts)
		case "Load":
			c.starUnpack(elts)
		default:
			c.exprs(elts)
		}
	}
}

// Visits the two arguments of a super() call the way load_args_for_super does
func (c *codegen) superArgs(call *Node) {
	if args := call.Children("args"); len(args) == 2 {
		c.expr(args[0])
		c.expr(args[1])
	}
}

func (c *codegen) call(e *Node) {
	keywords, args := e.Children("keywords"), e.Children("args")
	c.validateKeywords(keywords)
	fn := e.Child("func")
	if c.methodCall(e) {
		if c.superCall(fn) {
			c.superArgs(fn.Child("value"))
		} else {
			c.expr(fn.Child("value"))
		}
		c.exprs(args)
		for _, k := range keywords {
			c.expr(k.Child("value"))
		}
		return
	}
	c.expr(fn)
	if fn.Is("Name") && len(args) == 1 && len(keywords) == 0 && args[0].Is("GeneratorExp") {
		id, _ := fn.Str("id")
		if entry := c.st.blocks[args[0]]; entry != nil && !entry.coroutine && (id == "all" || id == "any" || id == "tuple") {
			c.expr(args[0])
		}
	}
	c.callHelper(args, keywords)
}

// Names a pattern binds so far, in binding order and as a set, and whether an irrefutable pattern may appear
type patternCtx struct {
	stores           []string
	bound            map[string]struct{}
	allowIrrefutable bool
}

func (pc *patternCtx) stored(name string) bool {
	_, ok := pc.bound[name]
	return ok
}

func (pc *patternCtx) store(name string) {
	if pc.bound == nil {
		pc.bound = map[string]struct{}{}
	}
	pc.bound[name] = struct{}{}
	pc.stores = append(pc.stores, name)
}

func (c *codegen) patternStore(loc srcLoc, name string, named bool, pc *patternCtx) {
	if !named {
		return
	}
	if pc.stored(name) {
		c.fail(loc, "multiple assignments to name "+Quote(name)+" in pattern")
	}
	pc.store(name)
}

func (c *codegen) subpattern(p *Node, pc *patternCtx) {
	allow := pc.allowIrrefutable
	pc.allowIrrefutable = true
	c.pattern(p, pc)
	pc.allowIrrefutable = allow
}

// Checks a match pattern, raising the binding and reachability errors of match statements
func (c *codegen) pattern(p *Node, pc *patternCtx) {
	switch p.Type {
	case "MatchValue":
		v := p.Child("value")
		if !v.Is("Constant", "Attribute") {
			c.fail(locOf(p), "patterns may only match literals and attribute lookups")
		}
		c.expr(v)
	case "MatchSequence":
		patterns := p.Children("patterns")
		star, onlyWildcard, starWildcard := -1, true, false
		for i, sub := range patterns {
			if sub.Is("MatchStar") {
				if star >= 0 {
					c.fail(locOf(p), "multiple starred names in sequence pattern")
				}
				_, named := sub.Str("name")
				starWildcard = !named
				onlyWildcard = onlyWildcard && starWildcard
				star = i
				continue
			}
			onlyWildcard = onlyWildcard && wildcardPattern(sub)
		}
		switch {
		case onlyWildcard:
		case starWildcard:
			for i, sub := range patterns {
				if i != star && !wildcardPattern(sub) {
					c.subpattern(sub, pc)
				}
			}
		default:
			if star >= 1<<8 || (star >= 0 && len(patterns)-star-1 >= math.MaxInt32>>8) {
				c.fail(locOf(p), "too many expressions in star-unpacking sequence pattern")
			}
			for _, sub := range patterns {
				c.subpattern(sub, pc)
			}
		}
	case "MatchMapping":
		keys, patterns := p.Children("keys"), p.Children("patterns")
		seen := map[string]bool{}
		for _, key := range keys {
			switch {
			case key.Is("Constant"):
				value := key.Get("value")
				if k, ok := literalKey(value); ok {
					if seen[k] {
						c.fail(locOf(p), "mapping pattern checks duplicate key ("+constantRepr(value)+")")
					}
					seen[k] = true
				}
			case !key.Is("Attribute"):
				c.fail(locOf(p), "mapping pattern keys may only match literals and attribute lookups")
			}
			c.expr(key)
		}
		for _, sub := range patterns {
			c.subpattern(sub, pc)
		}
		rest, named := p.Str("rest")
		c.patternStore(locOf(p), rest, named, pc)
	case "MatchClass":
		attrs, kwdPatterns := p.Names("kwd_attrs"), p.Children("kwd_patterns")
		if j := firstRepeat(len(attrs), func(i int) (string, bool) { return attrs[i], true }); j >= 0 {
			c.fail(locOf(kwdPatterns[j]), "attribute name repeated in class pattern: "+attrs[j])
		}
		c.expr(p.Child("cls"))
		for _, sub := range append(append([]*Node{}, p.Children("patterns")...), kwdPatterns...) {
			if !wildcardPattern(sub) {
				c.subpattern(sub, pc)
			}
		}
	case "MatchStar":
		name, named := p.Str("name")
		c.patternStore(locOf(p), name, named, pc)
	case "MatchAs":
		name, named := p.Str("name")
		sub := p.Child("pattern")
		if sub == nil {
			if !pc.allowIrrefutable {
				if named {
					c.fail(locOf(p), "name capture "+Quote(name)+" makes remaining patterns unreachable")
				}
				c.fail(locOf(p), "wildcard makes remaining patterns unreachable")
			}
			c.patternStore(locOf(p), name, named, pc)
			return
		}
		c.pattern(sub, pc)
		c.patternStore(locOf(p), name, named, pc)
	case "MatchOr":
		old := *pc
		var control []string
		alts := p.Children("patterns")
		for i, alt := range alts {
			pc.stores, pc.bound = nil, nil
			pc.allowIrrefutable = i == len(alts)-1 && old.allowIrrefutable
			c.pattern(alt, pc)
			if i == 0 {
				control = pc.stores
				continue
			}
			if len(pc.stores) != len(control) {
				c.fail(locOf(p), "alternative patterns bind different names")
			}
			for _, name := range control {
				if !pc.stored(name) {
					c.fail(locOf(p), "alternative patterns bind different names")
				}
			}
		}
		*pc = old
		for _, name := range control {
			c.patternStore(locOf(p), name, true, pc)
		}
	}
}
