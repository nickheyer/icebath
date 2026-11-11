package pyast

import (
	"math"
	"math/big"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Stack bytes the frames of the code generator hold between a caller and the expressions and statements it visits
const (
	cgModuleBodyDist = 240
	cgStmtFrame      = 256
	cgExprFrame      = 288
	cgJumpFrame      = 112
	cgFuncFrame      = 352
	cgDefaultsFrame  = 96
	cgAnnotFrame     = 80
	cgTypeParams     = 112
	cgBoundFrame     = 80
	cgClassBodyFrame = 144
	cgClassTailFrame = 272
	cgAliasFrame     = 112
	cgAugFrame       = 112
	cgAnnAssignFrame = 96
	cgAnnCheckFrame  = 48
	cgTryFinally     = 96
	cgTryExcept      = 128
	cgTryStar        = 160
	cgWithFrame      = 112
	cgUnwindStack    = 112
	cgUnwindBlock    = 64
	cgPatternFrame   = 208
	cgStackArgs      = 16
	cgStarUnpack     = 96
	cgStoreUnpack    = 80
	cgSubDict        = 112
	cgCallHelper     = 112
	cgSubKwargs      = 96
	cgGenexpCall     = 160
	cgCompFrame      = 352
	cgCompIter       = 48
	cgCompArgs       = 32
	cgCompGenerator  = 128
	cgSliceParts     = 48
	cgSuperArgs      = 96
	maxStaticBlocks  = 21
)

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

// codeUnit is a compiler_unit, the block being generated with its frame block stack
type codeUnit struct {
	ste           *symEntry
	scope         scopeKind
	fblocks       []fblock
	deferred      []*Node
	inInlinedComp int
	argCount      int
}

// codegen walks a module the way codegen.c does and raises the errors the code generator raises
type codegen struct {
	st       *symtable
	filename string
	future   futureInfo
	u        *codeUnit
	units    []*codeUnit
	seen     func(tag byte, d int)
}

// Generates the module in the order of compiler_codegen and returns the first error it raises
func runCodegen(mod *Node, st *symtable, filename string, future futureInfo, seen func(tag byte, d int)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(error)
			if !ok {
				panic(r)
			}
			err = e
		}
	}()
	c := &codegen{st: st, filename: filename, future: future, seen: seen}
	c.enterScope(mod, scopeModule, 0)
	c.body(mod.Children("body"), cgModuleBodyDist)
	c.exitScope()
	return nil
}

func (c *codegen) fail(loc srcLoc, msg string) {
	panic(rangedSyntaxError(msg, loc, c.filename))
}

func (c *codegen) check(d int) {
	if c.seen != nil {
		c.seen('c', d)
	}
	if d >= compileStackLimit {
		panic(stackOverflow(d))
	}
}

func (c *codegen) enterScope(key any, kind scopeKind, argCount int) {
	ste := c.st.blocks[key]
	if ste == nil {
		panic(internalError("KeyError", "unknown symbol table entry"))
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

func (c *codegen) stmts(list []*Node, d int) {
	for _, s := range list {
		c.stmt(s, d)
	}
}

func (c *codegen) exprs(list []*Node, d int) {
	for _, e := range list {
		c.expr(e, d)
	}
}

// Generates a module or class body, then the annotations of its annotated assignments in their own scope
func (c *codegen) body(stmts []*Node, d int) {
	if len(stmts) == 0 {
		return
	}
	if c.u.ste.hasDocstring {
		stmts = stmts[1:]
	}
	c.stmts(stmts, d)
	if c.future.annotations || len(c.u.deferred) == 0 {
		return
	}
	deferred := c.u.deferred
	c.enterScope(annotationsKey{c.u.ste}, scopeAnnotations, 1)
	for _, s := range deferred {
		c.expr(s.Child("annotation"), d)
	}
	c.exitScope()
}

func (c *codegen) defaultArguments(args *Node, d int) {
	x := d + cgDefaultsFrame
	c.exprs(args.Children("defaults"), x)
	defaults := args.Children("kw_defaults")
	for i := range args.Children("kwonlyargs") {
		if i < len(defaults) && defaults[i] != nil {
			c.expr(defaults[i], x)
		}
	}
}

// Raises the ValueError the annotation unparser raises for an int constant past the digit limit
func (c *codegen) annexpr(annotation *Node) {
	for _, n := range Walk(annotation) {
		if n.Is("Constant") {
			if _, err := ConstantRepr(n.Get("value")); err != nil {
				panic(internalError("ValueError", err.Error()))
			}
		}
	}
}

func (c *codegen) argAnnotation(annotation *Node, d int) {
	if annotation == nil {
		return
	}
	if c.future.annotations {
		c.annexpr(annotation)
		return
	}
	if annotation.Is("Starred") {
		annotation = annotation.Child("value")
	}
	c.expr(annotation, d+cgAnnotFrame)
}

func (c *codegen) argAnnotations(args []*Node, d int) {
	for _, a := range args {
		c.argAnnotation(a.Child("annotation"), d+cgAnnotFrame)
	}
}

func (c *codegen) functionAnnotations(args, returns *Node, d int) {
	ste := c.st.blocks[args]
	if ste == nil || !ste.annotationsUsed {
		return
	}
	c.enterScope(args, scopeAnnotations, 1)
	c.argAnnotations(args.Children("args"), d)
	c.argAnnotations(args.Children("posonlyargs"), d)
	if v := args.Child("vararg"); v != nil {
		c.argAnnotation(v.Child("annotation"), d)
	}
	c.argAnnotations(args.Children("kwonlyargs"), d)
	if k := args.Child("kwarg"); k != nil {
		c.argAnnotation(k.Child("annotation"), d)
	}
	c.argAnnotation(returns, d)
	c.exitScope()
}

func (c *codegen) typeParamBound(e *Node, key any, allowStarred bool, d int) {
	c.enterScope(key, scopeAnnotations, 1)
	if allowStarred && e.Is("Starred") {
		e = e.Child("value")
	}
	c.expr(e, d+cgBoundFrame)
	c.exitScope()
}

func (c *codegen) typeParams(list []*Node, d int) {
	x := d + cgTypeParams
	seenDefault := false
	for _, tp := range list {
		name, _ := tp.Str("name")
		def := tp.Child("default_value")
		switch tp.Type {
		case "TypeVar":
			if bound := tp.Child("bound"); bound != nil {
				c.typeParamBound(bound, tp, false, x)
			}
			if def != nil {
				seenDefault = true
				c.typeParamBound(def, defaultKey{tp}, false, x)
			}
		case "TypeVarTuple":
			if def != nil {
				c.typeParamBound(def, tp, true, x)
				seenDefault = true
			}
		case "ParamSpec":
			if def != nil {
				c.typeParamBound(def, tp, false, x)
				seenDefault = true
			}
		}
		if def == nil && seenDefault {
			c.fail(locOf(tp), "non-default type parameter '"+name+"' follows default type parameter")
		}
	}
}

func (c *codegen) function(s *Node, async bool, d int) {
	f := d + cgFuncFrame
	args := s.Child("args")
	c.exprs(s.Children("decorator_list"), f)
	c.defaultArguments(args, f)
	typeParams := s.Children("type_params")
	if len(typeParams) > 0 {
		c.enterScope(typeParamsKey{s}, scopeAnnotations, 0)
		c.typeParams(typeParams, f)
	}
	c.functionAnnotations(args, s.Child("returns"), f)
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
	c.stmts(body, f)
	if stopIteration {
		c.popFBlock()
	}
	c.exitScope()
	if len(typeParams) > 0 {
		c.exitScope()
	}
}

func (c *codegen) class(s *Node, d int) {
	x := d + cgStmtFrame
	c.exprs(s.Children("decorator_list"), x)
	typeParams := s.Children("type_params")
	if len(typeParams) > 0 {
		c.enterScope(typeParamsKey{s}, scopeAnnotations, 0)
		c.typeParams(typeParams, x)
	}
	c.enterScope(s, scopeClass, 0)
	c.body(s.Children("body"), x+cgClassBodyFrame)
	c.exitScope()
	c.callHelper(s.Children("bases"), s.Children("keywords"), d+cgClassTailFrame)
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

func (c *codegen) sliceTwoParts(s *Node, d int) {
	x := d + cgSliceParts
	if lower := s.Child("lower"); lower != nil {
		c.expr(lower, x)
	}
	if upper := s.Child("upper"); upper != nil {
		c.expr(upper, x)
	}
}

func (c *codegen) checkAnnExpr(e *Node, d int) {
	c.expr(e, d+cgAnnCheckFrame)
}

func (c *codegen) checkAnnSubscr(e *Node, d int) {
	x := d + cgAnnCheckFrame
	switch e.Type {
	case "Slice":
		for _, f := range []string{"lower", "upper", "step"} {
			if part := e.Child(f); part != nil {
				c.checkAnnExpr(part, x)
			}
		}
	case "Tuple":
		for _, elt := range e.Children("elts") {
			c.checkAnnSubscr(elt, x)
		}
	default:
		c.checkAnnExpr(e, d)
	}
}

func (c *codegen) annAssign(s *Node, d int) {
	x := d + cgAnnAssignFrame
	target, value := s.Child("target"), s.Child("value")
	if value != nil {
		c.expr(value, x)
		c.expr(target, x)
	}
	switch target.Type {
	case "Name":
		simple, _ := s.Int("simple")
		if simple != 0 && (c.u.scope == scopeModule || c.u.scope == scopeClass) {
			if c.future.annotations {
				c.annexpr(s.Child("annotation"))
			} else {
				c.u.deferred = append(c.u.deferred, s)
			}
		}
	case "Attribute":
		if value == nil {
			c.checkAnnExpr(target.Child("value"), x)
		}
	case "Subscript":
		if value == nil {
			c.checkAnnExpr(target.Child("value"), x)
			c.checkAnnSubscr(target.Child("slice"), x)
		}
	}
}

func (c *codegen) augAssign(s *Node, d int) {
	x := d + cgAugFrame
	target := s.Child("target")
	switch target.Type {
	case "Attribute":
		c.expr(target.Child("value"), x)
	case "Subscript":
		c.expr(target.Child("value"), x)
		if slice := target.Child("slice"); twoElementSlice(slice) {
			c.sliceTwoParts(slice, x)
		} else {
			c.expr(slice, x)
		}
	}
	c.expr(s.Child("value"), x)
}

// Unwinds one frame block for a return, break or continue, generating a finally body again
func (c *codegen) unwindFBlock(ploc *srcLoc, info fblock, preserve bool, d int) {
	switch info.kind {
	case fbFinallyTry:
		if preserve {
			c.pushFBlock(*ploc, fbPopValue, nil)
		}
		c.stmts(info.datum, d+cgUnwindBlock)
		if preserve {
			c.popFBlock()
		}
		*ploc = noLocation
	case fbWith, fbAsyncWith:
		*ploc = noLocation
	}
}

// Unwinds the frame blocks up to the nearest loop when one is wanted, or all of them, as codegen_unwind_fblock_stack
func (c *codegen) unwindFBlockStack(ploc *srcLoc, preserve, wantLoop bool, d int) *fblock {
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
	x := d + cgUnwindStack
	c.unwindFBlock(ploc, top, preserve, x)
	loop := c.unwindFBlockStack(ploc, preserve, wantLoop, x)
	c.u.fblocks = append(c.u.fblocks, top)
	return loop
}

func (c *codegen) tryFinally(s *Node, star bool, d int) {
	x := d + cgTryFinally
	final := s.Children("finalbody")
	c.pushFBlock(locOf(s), fbFinallyTry, final)
	switch {
	case len(s.Children("handlers")) == 0:
		c.stmts(s.Children("body"), x)
	case star:
		c.tryStarExcept(s, x)
	default:
		c.tryExcept(s, x)
	}
	c.popFBlock()
	c.stmts(final, x)
	c.pushFBlock(noLocation, fbFinallyEnd, nil)
	c.stmts(final, x)
	c.popFBlock()
}

func (c *codegen) tryExcept(s *Node, d int) {
	x := d + cgTryExcept
	c.pushFBlock(locOf(s), fbTryExcept, nil)
	c.stmts(s.Children("body"), x)
	c.popFBlock()
	c.stmts(s.Children("orelse"), x)
	handlers := s.Children("handlers")
	c.pushFBlock(locOf(s), fbExceptionHandler, nil)
	for i, h := range handlers {
		typ := h.Child("type")
		if typ == nil && i < len(handlers)-1 {
			c.fail(locOf(h), "default 'except:' must be last")
		}
		if typ != nil {
			c.expr(typ, x)
		}
		c.pushFBlock(locOf(h), fbHandlerCleanup, nil)
		c.stmts(h.Children("body"), x)
		c.popFBlock()
	}
	c.popFBlock()
}

func (c *codegen) tryStarExcept(s *Node, d int) {
	x := d + cgTryStar
	c.pushFBlock(locOf(s), fbTryExcept, nil)
	c.stmts(s.Children("body"), x)
	c.popFBlock()
	c.pushFBlock(locOf(s), fbExceptionGroupHandler, nil)
	for _, h := range s.Children("handlers") {
		if typ := h.Child("type"); typ != nil {
			c.expr(typ, x)
		}
		c.pushFBlock(locOf(h), fbHandlerCleanup, nil)
		c.stmts(h.Children("body"), x)
		c.popFBlock()
	}
	c.popFBlock()
	c.stmts(s.Children("orelse"), x)
}

func (c *codegen) with(s *Node, pos int, async bool, d int) {
	x := d + cgWithFrame
	items := s.Children("items")
	item := items[pos]
	ctx := item.Child("context_expr")
	c.expr(ctx, x)
	kind := fbWith
	if async {
		kind = fbAsyncWith
	}
	c.pushFBlock(locOf(ctx), kind, nil)
	if v := item.Child("optional_vars"); v != nil {
		c.expr(v, x)
	}
	if pos+1 == len(items) {
		c.stmts(s.Children("body"), x)
	} else {
		c.with(s, pos+1, async, x)
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

func (c *codegen) match(s *Node, d int) {
	c.expr(s.Child("subject"), d)
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
		c.pattern(m.Child("pattern"), pc, d)
		if guard != nil {
			c.jumpIf(guard, d)
		}
		c.stmts(m.Children("body"), d)
	}
	if hasDefault {
		if guard := last.Child("guard"); guard != nil {
			c.jumpIf(guard, d)
		}
		c.stmts(last.Children("body"), d)
	}
}

// Generates one statement as codegen_visit_stmt, d bytes of stack below the reference frame
func (c *codegen) stmt(s *Node, d int) {
	x := d + cgStmtFrame
	switch s.Type {
	case "FunctionDef":
		c.function(s, false, d)
	case "AsyncFunctionDef":
		c.function(s, true, d)
	case "ClassDef":
		c.class(s, d)
	case "TypeAlias":
		t := d + cgAliasFrame
		typeParams := s.Children("type_params")
		if len(typeParams) > 0 {
			c.enterScope(typeParamsKey{s}, scopeAnnotations, 0)
			c.typeParams(typeParams, t)
		}
		c.enterScope(s, scopeAnnotations, 1)
		c.expr(s.Child("value"), t)
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
			c.expr(v, x)
		} else if v != nil {
			loc = locOf(v)
		}
		if v == nil || v.Lineno != s.Lineno {
			loc = locOf(s)
		}
		c.unwindFBlockStack(&loc, preserve, false, x)
	case "Delete":
		c.exprs(s.Children("targets"), x)
	case "Assign":
		c.expr(s.Child("value"), x)
		c.exprs(s.Children("targets"), x)
	case "AugAssign":
		c.augAssign(s, d)
	case "AnnAssign":
		c.annAssign(s, d)
	case "For":
		c.pushFBlock(locOf(s), fbForLoop, nil)
		c.expr(s.Child("iter"), x)
		c.expr(s.Child("target"), x)
		c.stmts(s.Children("body"), x)
		c.popFBlock()
		c.stmts(s.Children("orelse"), x)
	case "AsyncFor":
		c.expr(s.Child("iter"), x)
		c.pushFBlock(locOf(s), fbForLoop, nil)
		c.expr(s.Child("target"), x)
		c.stmts(s.Children("body"), x)
		c.popFBlock()
		c.stmts(s.Children("orelse"), x)
	case "While":
		c.pushFBlock(locOf(s), fbWhileLoop, nil)
		c.jumpIf(s.Child("test"), x)
		c.stmts(s.Children("body"), x)
		c.popFBlock()
		c.stmts(s.Children("orelse"), x)
	case "If":
		c.jumpIf(s.Child("test"), x)
		c.stmts(s.Children("body"), x)
		c.stmts(s.Children("orelse"), x)
	case "Match":
		c.match(s, x)
	case "Raise":
		if exc := s.Child("exc"); exc != nil {
			c.expr(exc, x)
			if cause := s.Child("cause"); cause != nil {
				c.expr(cause, x)
			}
		}
	case "Try", "TryStar":
		star := s.Type == "TryStar"
		switch {
		case len(s.Children("finalbody")) > 0:
			c.tryFinally(s, star, x)
		case star:
			c.tryStarExcept(s, x)
		default:
			c.tryExcept(s, x)
		}
	case "Assert":
		c.jumpIf(s.Child("test"), x)
		if msg := s.Child("msg"); msg != nil {
			c.expr(msg, x)
		}
	case "Expr":
		if v := s.Child("value"); !v.Is("Constant") {
			c.expr(v, x)
		}
	case "Break", "Continue":
		loc := locOf(s)
		origin := loc
		if c.unwindFBlockStack(&loc, false, true, x) == nil {
			if s.Type == "Break" {
				c.fail(origin, "'break' outside loop")
			}
			c.fail(origin, "'continue' not properly in loop")
		}
	case "With":
		c.with(s, 0, false, x)
	case "AsyncWith":
		c.with(s, 0, true, x)
	}
}

// Generates a condition as codegen_jump_if, whose not operand reuses the frame
func (c *codegen) jumpIf(e *Node, d int) {
	x := d + cgJumpFrame
	switch e.Type {
	case "UnaryOp":
		if e.Child("op").Is("Not") {
			c.jumpIf(e.Child("operand"), d)
			return
		}
	case "BoolOp":
		for _, v := range e.Children("values") {
			c.jumpIf(v, x)
		}
		return
	case "IfExp":
		c.jumpIf(e.Child("test"), x)
		c.jumpIf(e.Child("body"), x)
		c.jumpIf(e.Child("orelse"), x)
		return
	case "Compare":
		if len(e.Children("ops")) > 1 {
			c.expr(e.Child("left"), x)
			c.exprs(e.Children("comparators"), x)
			return
		}
	}
	c.expr(e, x)
}

func (c *codegen) validateKeywords(keywords []*Node) {
	for i, key := range keywords {
		arg, ok := key.Str("arg")
		if !ok {
			continue
		}
		for _, other := range keywords[i+1:] {
			if o, ok := other.Str("arg"); ok && o == arg {
				c.fail(locOf(other), "keyword argument repeated: "+arg)
			}
		}
	}
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
func (c *codegen) starUnpack(elts []*Node, d int) {
	for _, elt := range elts {
		if elt.Is("Starred") {
			elt = elt.Child("value")
		}
		c.expr(elt, d)
	}
}

// Generates the arguments of a call or class statement as codegen_call_helper_impl, entered d bytes down
func (c *codegen) callHelper(args, keywords []*Node, d int) {
	c.validateKeywords(keywords)
	x := d + cgCallHelper
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
		c.exprs(args, x)
		for _, k := range keywords {
			c.expr(k.Child("value"), x)
		}
		return
	}
	c.starUnpack(args, x)
	for _, k := range keywords {
		if _, ok := k.Str("arg"); ok {
			c.expr(k.Child("value"), x+cgSubKwargs)
		} else {
			c.expr(k.Child("value"), x)
		}
	}
}

// Reports whether a call through an attribute loads a method as maybe_optimize_method_call decides
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

// Reports whether an attribute of a super() call loads through the super instruction as can_optimize_super_call decides
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

func (c *codegen) comprehensionGenerator(loc srcLoc, e *Node, i int, elt, val *Node, iterOnStack bool, d int) {
	generators := e.Children("generators")
	gen := generators[i]
	x := d + cgCompGenerator
	async, _ := gen.Int("is_async")
	if !iterOnStack && i > 0 {
		iter := gen.Child("iter")
		if elts := iter.Children("elts"); async == 0 && iter.Is("List", "Tuple") && len(elts) == 1 && !elts[0].Is("Starred") {
			iter = elts[0]
		}
		c.expr(iter, x)
	}
	if async != 0 {
		c.pushFBlock(loc, fbAsyncComprehensionGenerator, nil)
	}
	c.expr(gen.Child("target"), x)
	for _, cond := range gen.Children("ifs") {
		c.jumpIf(cond, x)
	}
	if i+1 < len(generators) {
		c.comprehensionGenerator(loc, e, i+1, elt, val, false, x+cgCompArgs)
	} else {
		c.expr(elt, x)
		if val != nil {
			c.expr(val, x)
		}
	}
	if async != 0 {
		c.popFBlock()
	}
}

// Generates a comprehension as codegen_comprehension, inline in the current unit or in a scope of its own
func (c *codegen) comprehension(e *Node, elt, val *Node, d int) {
	entry := c.st.blocks[e]
	if entry == nil {
		panic(internalError("KeyError", "unknown symbol table entry"))
	}
	outermost := e.Children("generators")[0]
	x := d + cgCompFrame
	iterDist := x + cgCompIter
	genDist := x + cgCompArgs
	if !entry.compInlined {
		c.enterScope(e, scopeComprehension, 1)
		c.comprehensionGenerator(locOf(e), e, 0, elt, val, false, genDist)
		c.exitScope()
		c.expr(outermost.Child("iter"), iterDist)
		return
	}
	c.expr(outermost.Child("iter"), iterDist)
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
	c.comprehensionGenerator(locOf(e), e, 0, elt, val, true, genDist)
	if pushed {
		c.popFBlock()
	}
	c.u.inInlinedComp--
}

// Generates one expression as codegen_visit_expr with its recursion check, d bytes of stack below the reference frame
func (c *codegen) expr(e *Node, d int) {
	c.check(d)
	x := d + cgExprFrame
	switch e.Type {
	case "NamedExpr":
		c.expr(e.Child("value"), x)
		c.expr(e.Child("target"), x)
	case "BoolOp":
		c.exprs(e.Children("values"), x)
	case "BinOp":
		c.expr(e.Child("left"), x)
		c.expr(e.Child("right"), x)
	case "UnaryOp":
		c.expr(e.Child("operand"), x)
	case "Lambda":
		args := e.Child("args")
		c.defaultArguments(args, x)
		c.enterScope(e, scopeLambda, len(args.Children("args"))+len(args.Children("posonlyargs")))
		c.expr(e.Child("body"), x)
		c.exitScope()
	case "IfExp":
		c.jumpIf(e.Child("test"), x)
		c.expr(e.Child("body"), x)
		c.expr(e.Child("orelse"), x)
	case "Dict":
		values := e.Children("values")
		for i, key := range e.Children("keys") {
			if key == nil {
				c.expr(values[i], x)
				continue
			}
			c.expr(key, x+cgSubDict)
			c.expr(values[i], x+cgSubDict)
		}
	case "Set":
		c.starUnpack(e.Children("elts"), x+cgStackArgs+cgStarUnpack)
	case "GeneratorExp", "ListComp", "SetComp":
		c.comprehension(e, e.Child("elt"), nil, x+cgStackArgs)
	case "DictComp":
		c.comprehension(e, e.Child("key"), e.Child("value"), x+cgStackArgs)
	case "Yield":
		if !c.u.ste.functionLike() {
			c.fail(locOf(e), "'yield' outside function")
		}
		if v := e.Child("value"); v != nil {
			c.expr(v, x)
		}
	case "YieldFrom":
		if !c.u.ste.functionLike() {
			c.fail(locOf(e), "'yield from' outside function")
		}
		if c.u.scope == scopeAsyncFunction {
			c.fail(locOf(e), "'yield from' inside async function")
		}
		c.expr(e.Child("value"), x)
	case "Await":
		c.expr(e.Child("value"), x)
	case "Compare":
		c.expr(e.Child("left"), x)
		c.exprs(e.Children("comparators"), x)
	case "Call":
		c.call(e, x)
	case "JoinedStr":
		c.exprs(e.Children("values"), x)
	case "TemplateStr":
		for _, v := range e.Children("values") {
			if !v.Is("Interpolation") {
				c.expr(v, x)
			}
		}
		for _, v := range e.Children("values") {
			if v.Is("Interpolation") {
				c.expr(v, x)
			}
		}
	case "FormattedValue", "Interpolation":
		c.expr(e.Child("value"), x)
		if spec := e.Child("format_spec"); spec != nil {
			c.expr(spec, x)
		}
	case "Attribute":
		if e.Child("ctx").Is("Load") && c.superCall(e) {
			c.superArgs(e.Child("value"), x)
			return
		}
		c.expr(e.Child("value"), x)
	case "Subscript":
		c.expr(e.Child("value"), x)
		if slice := e.Child("slice"); twoElementSlice(slice) && !e.Child("ctx").Is("Del") {
			c.sliceTwoParts(slice, x)
		} else {
			c.expr(slice, x)
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
		c.sliceTwoParts(e, x)
		if step := e.Child("step"); step != nil {
			c.expr(step, x)
		}
	case "List", "Tuple":
		elts := e.Children("elts")
		switch e.Child("ctx").Type {
		case "Store":
			c.unpackHelper(locOf(e), elts)
			c.starUnpack(elts, d+cgStoreUnpack)
		case "Load":
			c.starUnpack(elts, x+cgStackArgs+cgStarUnpack)
		default:
			c.exprs(elts, x)
		}
	}
}

// Visits the two arguments of a super() call the way load_args_for_super does
func (c *codegen) superArgs(call *Node, d int) {
	if args := call.Children("args"); len(args) == 2 {
		c.expr(args[0], d+cgSuperArgs)
		c.expr(args[1], d+cgSuperArgs)
	}
}

func (c *codegen) call(e *Node, x int) {
	keywords, args := e.Children("keywords"), e.Children("args")
	c.validateKeywords(keywords)
	fn := e.Child("func")
	if c.methodCall(e) {
		if c.superCall(fn) {
			c.superArgs(fn.Child("value"), x)
		} else {
			c.expr(fn.Child("value"), x)
		}
		c.exprs(args, x)
		for _, k := range keywords {
			c.expr(k.Child("value"), x)
		}
		return
	}
	c.expr(fn, x)
	if fn.Is("Name") && len(args) == 1 && len(keywords) == 0 && args[0].Is("GeneratorExp") {
		id, _ := fn.Str("id")
		if entry := c.st.blocks[args[0]]; entry != nil && !entry.coroutine && (id == "all" || id == "any" || id == "tuple") {
			c.expr(args[0], x+cgGenexpCall)
		}
	}
	c.callHelper(args, keywords, x+cgStackArgs)
}

type patternCtx struct {
	stores           []string
	allowIrrefutable bool
}

func (pc *patternCtx) stored(name string) bool {
	for _, s := range pc.stores {
		if s == name {
			return true
		}
	}
	return false
}

func (c *codegen) patternStore(loc srcLoc, name string, named bool, pc *patternCtx) {
	if !named {
		return
	}
	if pc.stored(name) {
		c.fail(loc, "multiple assignments to name "+pyrepr.StrRepr(name)+" in pattern")
	}
	pc.stores = append(pc.stores, name)
}

func (c *codegen) subpattern(p *Node, pc *patternCtx, d int) {
	allow := pc.allowIrrefutable
	pc.allowIrrefutable = true
	c.pattern(p, pc, d)
	pc.allowIrrefutable = allow
}

// Key of a mapping pattern constant under Python equality, equal numbers of any type sharing one key
func constantKey(v any) (string, bool) {
	number := func(f float64) string {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return pyrepr.Float(f)
		}
		r, _ := new(big.Rat).SetString(big.NewFloat(f).Text('f', -1))
		return r.RatString()
	}
	switch x := v.(type) {
	case nil:
		return "None", true
	case bool:
		if x {
			return "n1", true
		}
		return "n0", true
	case *big.Int:
		return "n" + x.String(), true
	case int:
		return "n" + big.NewInt(int64(x)).String(), true
	case float64:
		if math.IsNaN(x) {
			return "", false
		}
		return "n" + number(x), true
	case Complex:
		if math.IsNaN(x.Real) || math.IsNaN(x.Imag) {
			return "", false
		}
		if x.Imag == 0 {
			return "n" + number(x.Real), true
		}
		return "n" + number(x.Real) + "j" + number(x.Imag), true
	case string:
		return "s" + x, true
	case []byte:
		return "b" + string(x), true
	}
	return "", false
}

// Generates a pattern as codegen_pattern, raising the binding and reachability errors of match statements
func (c *codegen) pattern(p *Node, pc *patternCtx, d int) {
	x := d + cgPatternFrame
	switch p.Type {
	case "MatchValue":
		v := p.Child("value")
		if !v.Is("Constant", "Attribute") {
			c.fail(locOf(p), "patterns may only match literals and attribute lookups")
		}
		c.expr(v, x)
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
					c.subpattern(sub, pc, x)
				}
			}
		default:
			if star >= 1<<8 || (star >= 0 && len(patterns)-star-1 >= math.MaxInt32>>8) {
				c.fail(locOf(p), "too many expressions in star-unpacking sequence pattern")
			}
			for _, sub := range patterns {
				c.subpattern(sub, pc, x)
			}
		}
	case "MatchMapping":
		keys, patterns := p.Children("keys"), p.Children("patterns")
		seen := map[string]bool{}
		for _, key := range keys {
			switch {
			case key.Is("Constant"):
				value := key.Get("value")
				if k, ok := constantKey(value); ok {
					if seen[k] {
						repr, err := ConstantRepr(value)
						if err != nil {
							panic(internalError("ValueError", err.Error()))
						}
						c.fail(locOf(p), "mapping pattern checks duplicate key ("+repr+")")
					}
					seen[k] = true
				}
			case !key.Is("Attribute"):
				c.fail(locOf(p), "mapping pattern keys may only match literals and attribute lookups")
			}
			c.expr(key, x)
		}
		for _, sub := range patterns {
			c.subpattern(sub, pc, x)
		}
		rest, named := p.Str("rest")
		c.patternStore(locOf(p), rest, named, pc)
	case "MatchClass":
		attrs, kwdPatterns := p.Names("kwd_attrs"), p.Children("kwd_patterns")
		for i, attr := range attrs {
			for j := i + 1; j < len(attrs); j++ {
				if attrs[j] == attr {
					c.fail(locOf(kwdPatterns[j]), "attribute name repeated in class pattern: "+attr)
				}
			}
		}
		c.expr(p.Child("cls"), x)
		for _, sub := range append(append([]*Node{}, p.Children("patterns")...), kwdPatterns...) {
			if !wildcardPattern(sub) {
				c.subpattern(sub, pc, x)
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
					c.fail(locOf(p), "name capture "+pyrepr.StrRepr(name)+" makes remaining patterns unreachable")
				}
				c.fail(locOf(p), "wildcard makes remaining patterns unreachable")
			}
			c.patternStore(locOf(p), name, named, pc)
			return
		}
		c.pattern(sub, pc, x)
		c.patternStore(locOf(p), name, named, pc)
	case "MatchOr":
		old := *pc
		var control []string
		alts := p.Children("patterns")
		for i, alt := range alts {
			pc.stores = nil
			pc.allowIrrefutable = i == len(alts)-1 && old.allowIrrefutable
			c.pattern(alt, pc, x)
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
