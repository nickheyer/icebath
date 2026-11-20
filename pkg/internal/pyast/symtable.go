package pyast

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	defGlobal    = 1
	defLocal     = 2
	defParam     = 2 << 1
	defNonlocal  = 2 << 2
	symUse       = 2 << 3
	defFreeClass = 2 << 5
	defImport    = 2 << 6
	defAnnot     = 2 << 7
	defCompIter  = 2 << 8
	defTypeParam = 2 << 9
	defCompCell  = 2 << 10
	defBound     = defLocal | defParam | defImport
	scopeOffset  = 12
	scopeMask    = defGlobal | defLocal | defParam | defNonlocal
)

const (
	scopeLocal          = 1
	scopeGlobalExplicit = 2
	scopeGlobalImplicit = 3
	scopeFree           = 4
	scopeCell           = 5
)

type blockKind uint8

const (
	functionBlock blockKind = iota
	classBlock
	moduleBlock
	annotationBlock
	typeAliasBlock
	typeParametersBlock
	typeVariableBlock
)

type compKind uint8

const (
	noComprehension compKind = iota
	listComprehension
	dictComprehension
	setComprehension
	generatorExpression
)

// symDict is the insertion ordered name to flags dict of one block
type symDict struct {
	keys []string
	vals map[string]int
}

func newSymDict() *symDict { return &symDict{vals: map[string]int{}} }

func (d *symDict) get(k string) (int, bool) {
	v, ok := d.vals[k]
	return v, ok
}

func (d *symDict) set(k string, v int) {
	if _, ok := d.vals[k]; !ok {
		d.keys = append(d.keys, k)
	}
	d.vals[k] = v
}

func (d *symDict) del(k string) {
	if _, ok := d.vals[k]; !ok {
		return
	}
	delete(d.vals, k)
	for i, key := range d.keys {
		if key == k {
			d.keys = append(d.keys[:i:i], d.keys[i+1:]...)
			return
		}
	}
}

type nameSet map[string]struct{}

func (s nameSet) has(k string) bool { _, ok := s[k]; return ok }

func (s nameSet) union(o nameSet) {
	for k := range o {
		s[k] = struct{}{}
	}
}

func (s nameSet) clone() nameSet {
	out := make(nameSet, len(s))
	out.union(s)
	return out
}

type symDirective struct {
	name string
	loc  srcLoc
}

// symEntry is a PySTEntryObject, the symbols and flags of one block
type symEntry struct {
	name                      string
	kind                      blockKind
	symbols                   *symDict
	children                  []*symEntry
	directives                []symDirective
	mangledNames              nameSet
	scopeInfo                 string
	loc                       srcLoc
	nested                    bool
	generator                 bool
	coroutine                 bool
	comprehension             compKind
	returnsValue              bool
	needsClassClosure         bool
	compInlined               bool
	compIterTarget            bool
	canSeeClassScope          bool
	compIterExpr              int
	needsClassdict            bool
	hasConditionalAnnotations bool
	inConditionalBlock        bool
	inUnevaluatedAnnotation   bool
	annotationBlock           *symEntry
	hasDocstring              bool
	method                    bool
	varargs                   bool
	varkeywords               bool
	annotationsUsed           bool
}

// Reports whether the block runs as a function as _PyST_IsFunctionLike
func (ste *symEntry) functionLike() bool {
	switch ste.kind {
	case functionBlock, annotationBlock, typeVariableBlock, typeAliasBlock, typeParametersBlock:
		return true
	}
	return false
}

// Flags of a name in the block, zero when the block does not know it
func (ste *symEntry) symbol(name string) int {
	v, _ := ste.symbols.get(name)
	return v
}

// Scope of a name in the analysed block as _PyST_GetScope
func (ste *symEntry) scope(name string) int {
	return (ste.symbol(name) >> scopeOffset) & scopeMask
}

// Keys of the blocks that no single node identifies
type (
	typeParamsKey  struct{ owner *Node }
	annotationsKey struct{ parent *symEntry }
	defaultKey     struct{ param *Node }
)

type symtable struct {
	filename   string
	cur, top   *symEntry
	stack      []*symEntry
	global     *symDict
	private    string
	hasPrivate bool
	future     futureInfo
	blocks     map[any]*symEntry
	seen       func(tag byte, d int)
}

// internalError is a RuntimeError, SystemError or KeyError the C compiler raises on broken bookkeeping
func internalError(class, msg string) *LimitError { return &LimitError{Class: class, Msg: msg} }

func (st *symtable) raise(msg string, loc srcLoc) {
	panic(rangedSyntaxError(msg, loc, st.filename))
}

// Builds the symbol table of a module as _PySymtable_Build, both the collection pass and the analysis pass
func buildSymtable(mod *Node, filename string, future futureInfo, seen func(tag byte, d int)) (st *symtable, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(error)
			if !ok {
				panic(r)
			}
			st, err = nil, e
		}
	}()
	st = &symtable{filename: filename, future: future, blocks: map[any]*symEntry{}, seen: seen}
	st.enterBlock("top", moduleBlock, mod, srcLoc{0, 0, 0, 0})
	st.top = st.cur
	switch mod.Type {
	case "Module":
		body := mod.Children("body")
		if docstringOf(body) {
			st.cur.hasDocstring = true
		}
		st.visitStmts(body, symBuildDist)
	case "Expression":
		st.visitExpr(mod.Child("body"), symBuildDist)
	case "Interactive":
		st.visitStmts(mod.Children("body"), symBuildDist)
	default:
		panic(internalError("RuntimeError", "this compiler does not handle FunctionTypes"))
	}
	st.exitBlock()
	st.analyzeBlock(st.top, nil, nameSet{}, nameSet{}, nameSet{}, nil)
	return st, nil
}

func (st *symtable) newEntry(name string, kind blockKind, key any, loc srcLoc) *symEntry {
	ste := &symEntry{name: name, kind: kind, loc: loc, symbols: newSymDict()}
	if st.cur != nil && (st.cur.nested || st.cur.functionLike()) {
		ste.nested = true
	}
	if st.cur != nil && st.cur.kind == classBlock && kind == functionBlock {
		ste.method = true
	}
	st.blocks[key] = ste
	return ste
}

func (st *symtable) exitBlock() {
	st.cur = nil
	if n := len(st.stack); n > 0 {
		st.stack = st.stack[:n-1]
		if n > 1 {
			st.cur = st.stack[n-2]
		}
	}
}

func (st *symtable) enterExistingBlock(ste *symEntry, addToChildren bool) {
	st.stack = append(st.stack, ste)
	prev := st.cur
	if prev != nil {
		ste.compIterExpr = prev.compIterExpr
	}
	if prev != nil && prev.mangledNames != nil && ste.kind != classBlock {
		ste.mangledNames = prev.mangledNames
	}
	st.cur = ste
	if st.future.annotations && ste.kind == annotationBlock {
		return
	}
	if ste.kind == moduleBlock {
		st.global = ste.symbols
	}
	if addToChildren && prev != nil {
		prev.children = append(prev.children, ste)
	}
}

func (st *symtable) enterBlock(name string, kind blockKind, key any, loc srcLoc) {
	ste := st.newEntry(name, kind, key, loc)
	st.enterExistingBlock(ste, true)
	if kind == annotationBlock || kind == typeVariableBlock || kind == typeAliasBlock {
		st.addDef(".format", defParam, loc)
		st.addDef(".format", symUse, loc)
	}
}

// Mangles a private name of a class body as _Py_Mangle
func mangleName(private string, hasPrivate bool, ident string) string {
	if !hasPrivate || !strings.HasPrefix(ident, "__") {
		return ident
	}
	if strings.HasSuffix(ident, "__") || strings.Contains(ident, ".") {
		return ident
	}
	stripped := strings.TrimLeft(private, "_")
	if stripped == "" {
		return ident
	}
	return "_" + stripped + ident
}

func (st *symtable) maybeMangle(ste *symEntry, name string) string {
	if ste.mangledNames != nil && !ste.mangledNames.has(name) {
		return name
	}
	return mangleName(st.private, st.hasPrivate, name)
}

func (st *symtable) lookupEntry(ste *symEntry, name string) int {
	return ste.symbol(st.maybeMangle(ste, name))
}

func (st *symtable) lookup(name string) int { return st.lookupEntry(st.cur, name) }

func (st *symtable) addDefHelper(name string, flag int, ste *symEntry, loc srcLoc) {
	mangled := st.maybeMangle(st.cur, name)
	val, ok := ste.symbols.get(mangled)
	if ok {
		if flag&defParam != 0 && val&defParam != 0 {
			st.raise("duplicate argument '"+name+"' in function definition", loc)
		}
		if flag&defTypeParam != 0 && val&defTypeParam != 0 {
			st.raise("duplicate type parameter '"+name+"'", loc)
		}
		val |= flag
	} else {
		val = flag
	}
	if ste.compIterTarget {
		if val&(defGlobal|defNonlocal) != 0 {
			st.raise("comprehension inner loop cannot rebind assignment expression target '"+name+"'", loc)
		}
		val |= defCompIter
	}
	ste.symbols.set(mangled, val)
	if flag&defParam != 0 {
		return
	}
	if flag&defGlobal != 0 {
		v, _ := st.global.get(mangled)
		st.global.set(mangled, v|flag)
	}
}

func (st *symtable) checkName(name string, loc srcLoc, ctx string) {
	if name != "__debug__" {
		return
	}
	switch ctx {
	case "Store":
		st.raise("cannot assign to __debug__", loc)
	case "Del":
		st.raise("cannot delete __debug__", loc)
	}
}

func (st *symtable) checkKeywords(keywords []*Node) {
	for _, k := range keywords {
		if arg, ok := k.Str("arg"); ok {
			st.checkName(arg, locOf(k), "Store")
		}
	}
}

func (st *symtable) addDefCtx(name string, flag int, loc srcLoc, ctx string) {
	if flag&(defParam|defLocal|defImport) != 0 {
		st.checkName(name, loc, ctx)
	}
	if flag&defTypeParam != 0 && st.cur.mangledNames != nil {
		st.cur.mangledNames[name] = struct{}{}
	}
	st.addDefHelper(name, flag, st.cur, loc)
}

func (st *symtable) addDef(name string, flag int, loc srcLoc) {
	ctx := "Store"
	if flag == symUse {
		ctx = "Load"
	}
	st.addDefCtx(name, flag, loc, ctx)
}

func (st *symtable) enterTypeParamBlock(name string, key any, hasDefaults, hasKwdefaults, isClass bool, loc srcLoc) {
	currentKind := st.cur.kind
	st.enterBlock(name, typeParametersBlock, key, loc)
	if currentKind == classBlock {
		st.cur.canSeeClassScope = true
		st.addDef("__classdict__", symUse, loc)
	}
	if isClass {
		st.addDef(".type_params", defLocal, loc)
		st.addDef(".type_params", symUse, loc)
		st.addDef(".generic_base", defLocal, loc)
		st.addDef(".generic_base", symUse, loc)
	}
	if hasDefaults {
		st.addDef(".defaults", defParam, loc)
	}
	if hasKwdefaults {
		st.addDef(".kwdefaults", defParam, loc)
	}
}

func (st *symtable) recordDirective(name string, loc srcLoc) {
	st.cur.directives = append(st.cur.directives, symDirective{st.maybeMangle(st.cur, name), loc})
}

func hasKwonlyDefaults(args *Node) bool {
	defaults := args.Children("kw_defaults")
	for i := range args.Children("kwonlyargs") {
		if i < len(defaults) && defaults[i] != nil {
			return true
		}
	}
	return false
}

func (st *symtable) checkImportFrom(s *Node) {
	fut := st.future.loc
	module, ok := s.Str("module")
	level, _ := s.Int("level")
	if ok && level == 0 && module == "__future__" &&
		(s.Lineno > fut.lineno || (s.Lineno == fut.endLineno && s.ColOffset > fut.endCol)) {
		st.raise("from __future__ imports must occur at the beginning of the file", locOf(s))
	}
}

// Stack bytes between the frame of Py_CompileStringObject and the callers of the recursion checked functions of compile()
const (
	// compileStackLimit is the distance at which a recursion check of the frozen reference invocation raises
	compileStackLimit = 8344912
	symBuildDist      = 224
	symStmtFrame      = 112
	symExprFrame      = 128
	symPatternFrame   = 96
	symCompFrame      = 112
	symHandlerFrame   = 64
	symWithItemFrame  = 32
	symAnnotsFrame    = 96
	symArgAnnotsFrame = 48
	symTypeParamFrame = 48
	symBoundFrame     = 96
)

// Builds the RecursionError Py_EnterRecursiveCall raises at a check whose caller sits d bytes below the reference frame
func stackOverflow(d int) *LimitError {
	return &LimitError{Class: "RecursionError", Msg: fmt.Sprintf("Stack overflow (used %d kB) during compilation", 8152+(d-compileStackLimit+16)/1024)}
}

// Runs the recursion check of a visit function entered with its caller d bytes down the stack
func (st *symtable) check(tag byte, d int) {
	if st.seen != nil {
		st.seen(tag, d)
	}
	if d >= compileStackLimit {
		panic(stackOverflow(d))
	}
}

func (st *symtable) visitStmts(list []*Node, d int) {
	for _, s := range list {
		st.visitStmt(s, d)
	}
}

func (st *symtable) visitExprs(list []*Node, d int) {
	for _, e := range list {
		st.visitExpr(e, d)
	}
}

func (st *symtable) visitExprsWithNull(list []*Node, d int) {
	for _, e := range list {
		if e != nil {
			st.visitExpr(e, d)
		}
	}
}

// Runs a body as a conditional block, restoring the flag of the block afterwards
func (st *symtable) conditional(body func()) {
	ste := st.cur
	saved := ste.inConditionalBlock
	ste.inConditionalBlock = true
	body()
	ste.inConditionalBlock = saved
}

func (st *symtable) visitFunction(s *Node, async bool, c int) {
	name, _ := s.Str("name")
	args := s.Child("args")
	st.addDef(name, defLocal, locOf(s))
	st.visitExprs(args.Children("defaults"), c)
	st.visitExprsWithNull(args.Children("kw_defaults"), c)
	st.visitExprs(s.Children("decorator_list"), c)
	typeParams := s.Children("type_params")
	if len(typeParams) > 0 {
		st.enterTypeParamBlock(name, typeParamsKey{s}, true, hasKwonlyDefaults(args), false, locOf(s))
		for _, tp := range typeParams {
			st.visitTypeParam(tp, c)
		}
	}
	ste := st.newEntry(name, functionBlock, s, locOf(s))
	if docstringOf(s.Children("body")) {
		ste.hasDocstring = true
	}
	st.visitAnnotations(s, args, s.Child("returns"), c)
	st.enterExistingBlock(ste, true)
	if async {
		st.cur.coroutine = true
	}
	st.visitArguments(args)
	st.visitStmts(s.Children("body"), c)
	st.exitBlock()
	if len(typeParams) > 0 {
		st.exitBlock()
	}
}

// Collects the definitions and uses of one statement as symtable_visit_stmt, d bytes of stack below the reference frame
func (st *symtable) visitStmt(s *Node, d int) {
	st.check('s', d)
	c := d + symStmtFrame
	switch s.Type {
	case "FunctionDef":
		st.visitFunction(s, false, c)
	case "AsyncFunctionDef":
		st.visitFunction(s, true, c)
	case "ClassDef":
		name, _ := s.Str("name")
		st.addDef(name, defLocal, locOf(s))
		st.visitExprs(s.Children("decorator_list"), c)
		private, hasPrivate := st.private, st.hasPrivate
		typeParams := s.Children("type_params")
		if len(typeParams) > 0 {
			st.enterTypeParamBlock(name, typeParamsKey{s}, false, false, true, locOf(s))
			st.private, st.hasPrivate = name, true
			st.cur.mangledNames = nameSet{}
			for _, tp := range typeParams {
				st.visitTypeParam(tp, c)
			}
		}
		st.visitExprs(s.Children("bases"), c)
		st.checkKeywords(s.Children("keywords"))
		for _, k := range s.Children("keywords") {
			st.visitExpr(k.Child("value"), c)
		}
		st.enterBlock(name, classBlock, s, locOf(s))
		st.private, st.hasPrivate = name, true
		if len(typeParams) > 0 {
			st.addDef("__type_params__", defLocal, locOf(s))
			st.addDef(".type_params", symUse, locOf(s))
		}
		if docstringOf(s.Children("body")) {
			st.cur.hasDocstring = true
		}
		st.visitStmts(s.Children("body"), c)
		st.exitBlock()
		if len(typeParams) > 0 {
			st.exitBlock()
		}
		st.private, st.hasPrivate = private, hasPrivate
	case "TypeAlias":
		target := s.Child("name")
		st.visitExpr(target, c)
		name, _ := target.Str("id")
		inClass := st.cur.kind == classBlock
		typeParams := s.Children("type_params")
		if len(typeParams) > 0 {
			st.enterTypeParamBlock(name, typeParamsKey{s}, false, false, false, locOf(s))
			for _, tp := range typeParams {
				st.visitTypeParam(tp, c)
			}
		}
		st.enterBlock(name, typeAliasBlock, s, locOf(s))
		st.cur.canSeeClassScope = inClass
		if inClass {
			st.addDef("__classdict__", symUse, locOf(s.Child("value")))
		}
		st.visitExpr(s.Child("value"), c)
		st.exitBlock()
		if len(typeParams) > 0 {
			st.exitBlock()
		}
	case "Return":
		if v := s.Child("value"); v != nil {
			st.visitExpr(v, c)
			st.cur.returnsValue = true
		}
	case "Delete":
		st.visitExprs(s.Children("targets"), c)
	case "Assign":
		st.visitExprs(s.Children("targets"), c)
		st.visitExpr(s.Child("value"), d)
	case "AnnAssign":
		st.cur.annotationsUsed = true
		target := s.Child("target")
		simple, _ := s.Int("simple")
		if target.Is("Name") {
			id, _ := target.Str("id")
			cur := st.lookup(id)
			if cur&(defGlobal|defNonlocal) != 0 && st.cur.symbols != st.global && simple != 0 {
				if cur&defGlobal != 0 {
					st.raise("annotated name '"+id+"' can't be global", locOf(s))
				}
				st.raise("annotated name '"+id+"' can't be nonlocal", locOf(s))
			}
			if simple != 0 {
				st.addDef(id, defAnnot|defLocal, locOf(target))
			} else if s.Child("value") != nil {
				st.addDef(id, defLocal, locOf(target))
			}
		} else {
			st.visitExpr(target, c)
		}
		st.visitAnnotation(s.Child("annotation"), annotationsKey{st.cur}, c)
		if v := s.Child("value"); v != nil {
			st.visitExpr(v, d)
		}
	case "AugAssign":
		st.visitExpr(s.Child("target"), c)
		st.visitExpr(s.Child("value"), d)
	case "For":
		st.visitExpr(s.Child("target"), c)
		st.visitExpr(s.Child("iter"), c)
		st.conditional(func() {
			st.visitStmts(s.Children("body"), c)
			st.visitStmts(s.Children("orelse"), c)
		})
	case "While", "If":
		st.visitExpr(s.Child("test"), c)
		st.conditional(func() {
			st.visitStmts(s.Children("body"), c)
			st.visitStmts(s.Children("orelse"), c)
		})
	case "Match":
		st.visitExpr(s.Child("subject"), c)
		st.conditional(func() {
			for _, m := range s.Children("cases") {
				st.visitPattern(m.Child("pattern"), c)
				if g := m.Child("guard"); g != nil {
					st.visitExpr(g, c)
				}
				st.visitStmts(m.Children("body"), c)
			}
		})
	case "Raise":
		if exc := s.Child("exc"); exc != nil {
			st.visitExpr(exc, c)
			if cause := s.Child("cause"); cause != nil {
				st.visitExpr(cause, d)
			}
		}
	case "Try", "TryStar":
		st.conditional(func() {
			st.visitStmts(s.Children("body"), c)
			h := c + symHandlerFrame
			for _, handler := range s.Children("handlers") {
				if t := handler.Child("type"); t != nil {
					st.visitExpr(t, h)
				}
				if name, ok := handler.Str("name"); ok {
					st.addDef(name, defLocal, locOf(handler))
				}
				st.visitStmts(handler.Children("body"), h)
			}
			st.visitStmts(s.Children("orelse"), c)
			st.visitStmts(s.Children("finalbody"), c)
		})
	case "Assert":
		st.visitExpr(s.Child("test"), c)
		if msg := s.Child("msg"); msg != nil {
			st.visitExpr(msg, d)
		}
	case "Import":
		for _, a := range s.Children("names") {
			st.visitAlias(a)
		}
	case "ImportFrom":
		for _, a := range s.Children("names") {
			st.visitAlias(a)
		}
		st.checkImportFrom(s)
	case "Global", "Nonlocal":
		global := s.Type == "Global"
		word, flag := "nonlocal", defNonlocal
		if global {
			word, flag = "global", defGlobal
		}
		for _, name := range s.Names("names") {
			cur := st.lookup(name)
			if cur&(defParam|defLocal|symUse|defAnnot) != 0 {
				var msg string
				switch {
				case cur&defParam != 0:
					msg = "name '" + name + "' is parameter and " + word
				case cur&symUse != 0:
					msg = "name '" + name + "' is used prior to " + word + " declaration"
				case cur&defAnnot != 0:
					msg = "annotated name '" + name + "' can't be " + word
				default:
					msg = "name '" + name + "' is assigned to before " + word + " declaration"
				}
				st.raise(msg, locOf(s))
			}
			st.addDef(name, flag, locOf(s))
			st.recordDirective(name, locOf(s))
		}
	case "Expr":
		st.visitExpr(s.Child("value"), d)
	case "Pass", "Break", "Continue":
	case "With", "AsyncWith":
		if s.Type == "AsyncWith" {
			st.raiseIfNotCoroutine("'async with' outside async function", locOf(s))
		}
		st.conditional(func() {
			for _, item := range s.Children("items") {
				st.visitExpr(item.Child("context_expr"), c+symWithItemFrame)
				if v := item.Child("optional_vars"); v != nil {
					st.visitExpr(v, c)
				}
			}
			st.visitStmts(s.Children("body"), c)
		})
	case "AsyncFor":
		st.raiseIfNotCoroutine("'async for' outside async function", locOf(s))
		st.visitExpr(s.Child("target"), c)
		st.visitExpr(s.Child("iter"), c)
		st.conditional(func() {
			st.visitStmts(s.Children("body"), c)
			st.visitStmts(s.Children("orelse"), c)
		})
	}
}

func (st *symtable) raiseIfNotCoroutine(msg string, loc srcLoc) {
	if !st.cur.coroutine {
		st.raise(msg, loc)
	}
}

// Finds the scope a named expression target of a comprehension binds in as symtable_extend_namedexpr_scope
func (st *symtable) extendNamedExprScope(e *Node) {
	target, _ := e.Str("id")
	for i := len(st.stack) - 1; i >= 0; i-- {
		ste := st.stack[i]
		if ste.comprehension != noComprehension {
			in := st.lookupEntry(ste, target)
			if in&defCompIter != 0 && in&defLocal != 0 {
				st.raise("assignment expression cannot rebind comprehension iteration variable '"+target+"'", locOf(e))
			}
			continue
		}
		switch ste.kind {
		case functionBlock:
			if st.lookupEntry(ste, target)&defGlobal != 0 {
				st.addDef(target, defGlobal, locOf(e))
			} else {
				st.addDef(target, defNonlocal, locOf(e))
			}
			st.recordDirective(target, locOf(e))
			st.addDefHelper(target, defLocal, ste, locOf(e))
			return
		case moduleBlock:
			st.addDef(target, defGlobal, locOf(e))
			st.recordDirective(target, locOf(e))
			st.addDefHelper(target, defGlobal, ste, locOf(e))
			return
		case classBlock:
			st.raise("assignment expression within a comprehension cannot be used in a class body", locOf(e))
		case typeParametersBlock:
			st.raise("assignment expression within a comprehension cannot be used within the definition of a generic", locOf(e))
		case typeAliasBlock:
			st.raise("assignment expression within a comprehension cannot be used in a type alias", locOf(e))
		case typeVariableBlock:
			st.raise("assignment expression within a comprehension cannot be used in a TypeVar bound", locOf(e))
		}
	}
	panic(internalError("SystemError", "named expression target found no enclosing scope"))
}

// Collects the definitions and uses of one expression as symtable_visit_expr, a child in tail position runs in the same frame
func (st *symtable) visitExpr(e *Node, d int) {
	st.check('e', d)
	st.exprBody(e, d)
}

// Body of symtable_visit_expr after its recursion check, looping over the children GCC visits by tail recursion
func (st *symtable) exprBody(e *Node, d int) {
	c := d + symExprFrame
	switch e.Type {
	case "NamedExpr":
		st.raiseIfAnnotationBlock("named expression", e)
		if st.cur.compIterExpr > 0 {
			st.raise("assignment expression cannot be used in a comprehension iterable expression", locOf(e))
		}
		if st.cur.comprehension != noComprehension {
			st.extendNamedExprScope(e.Child("target"))
		}
		st.visitExpr(e.Child("value"), c)
		st.exprBody(e.Child("target"), d)
	case "BoolOp":
		st.visitExprs(e.Children("values"), c)
	case "BinOp":
		st.visitExpr(e.Child("left"), c)
		st.exprBody(e.Child("right"), d)
	case "UnaryOp":
		st.exprBody(e.Child("operand"), d)
	case "Lambda":
		args := e.Child("args")
		st.visitExprs(args.Children("defaults"), c)
		st.visitExprsWithNull(args.Children("kw_defaults"), c)
		st.enterBlock("lambda", functionBlock, e, locOf(e))
		st.visitArguments(args)
		st.visitExpr(e.Child("body"), c)
		st.exitBlock()
	case "IfExp":
		st.visitExpr(e.Child("test"), c)
		st.visitExpr(e.Child("body"), c)
		st.exprBody(e.Child("orelse"), d)
	case "Dict":
		st.visitExprsWithNull(e.Children("keys"), c)
		st.visitExprs(e.Children("values"), c)
	case "Set":
		st.visitExprs(e.Children("elts"), c)
	case "GeneratorExp":
		st.handleComprehension(e, "genexpr", e.Child("elt"), nil, d+symCompFrame)
	case "ListComp":
		st.handleComprehension(e, "listcomp", e.Child("elt"), nil, d+symCompFrame)
	case "SetComp":
		st.handleComprehension(e, "setcomp", e.Child("elt"), nil, d+symCompFrame)
	case "DictComp":
		st.handleComprehension(e, "dictcomp", e.Child("key"), e.Child("value"), d+symCompFrame)
	case "Yield", "YieldFrom":
		st.raiseIfAnnotationBlock("yield expression", e)
		if v := e.Child("value"); v != nil {
			st.visitExpr(v, c)
		}
		st.cur.generator = true
		if st.cur.comprehension != noComprehension {
			st.raiseIfComprehensionBlock(e)
		}
	case "Await":
		st.raiseIfAnnotationBlock("await expression", e)
		if !st.cur.functionLike() {
			st.raise("'await' outside function", locOf(e))
		}
		if !(st.cur.kind == functionBlock && st.cur.coroutine) && st.cur.comprehension == noComprehension {
			st.raise("'await' outside async function", locOf(e))
		}
		st.visitExpr(e.Child("value"), c)
		st.cur.coroutine = true
	case "Compare":
		st.visitExpr(e.Child("left"), c)
		st.visitExprs(e.Children("comparators"), c)
	case "Call":
		st.visitExpr(e.Child("func"), c)
		st.visitExprs(e.Children("args"), c)
		st.checkKeywords(e.Children("keywords"))
		for _, k := range e.Children("keywords") {
			if k != nil {
				st.visitExpr(k.Child("value"), c)
			}
		}
	case "FormattedValue", "Interpolation":
		st.visitExpr(e.Child("value"), c)
		if spec := e.Child("format_spec"); spec != nil {
			st.exprBody(spec, d)
		}
	case "JoinedStr", "TemplateStr":
		st.visitExprs(e.Children("values"), c)
	case "Constant":
	case "Attribute":
		attr, _ := e.Str("attr")
		st.checkName(attr, locOf(e), e.Child("ctx").Type)
		st.exprBody(e.Child("value"), d)
	case "Subscript":
		st.visitExpr(e.Child("value"), c)
		st.exprBody(e.Child("slice"), d)
	case "Starred":
		st.exprBody(e.Child("value"), d)
	case "Slice":
		if lower := e.Child("lower"); lower != nil {
			st.visitExpr(lower, c)
		}
		if upper := e.Child("upper"); upper != nil {
			st.visitExpr(upper, c)
		}
		if step := e.Child("step"); step != nil {
			st.exprBody(step, d)
		}
	case "Name":
		if st.cur.inUnevaluatedAnnotation {
			return
		}
		id, _ := e.Str("id")
		ctx := e.Child("ctx").Type
		flag := defLocal
		if ctx == "Load" {
			flag = symUse
		}
		st.addDefCtx(id, flag, locOf(e), ctx)
		if ctx == "Load" && st.cur.functionLike() && id == "super" {
			st.addDef("__class__", symUse, locOf(e))
		}
	case "List", "Tuple":
		st.visitExprs(e.Children("elts"), c)
	}
}

func (st *symtable) visitTypeParamBoundOrDefault(e *Node, name string, tp *Node, key any, scopeInfo string, d int) {
	if name == "__classdict__" {
		st.raise("reserved name '"+name+"' cannot be used for type parameter", locOf(tp))
	}
	if e == nil {
		return
	}
	inClass := st.cur.canSeeClassScope
	st.enterBlock(name, typeVariableBlock, key, locOf(e))
	st.cur.canSeeClassScope = inClass
	if inClass {
		st.addDef("__classdict__", symUse, locOf(e))
	}
	st.cur.scopeInfo = scopeInfo
	st.visitExpr(e, d+symBoundFrame)
	st.exitBlock()
}

func (st *symtable) visitTypeParam(tp *Node, d int) {
	st.check('t', d)
	c := d + symTypeParamFrame
	name, _ := tp.Str("name")
	st.addDef(name, defTypeParam|defLocal, locOf(tp))
	switch tp.Type {
	case "TypeVar":
		info := ""
		if bound := tp.Child("bound"); bound != nil {
			info = "a TypeVar bound"
			if bound.Is("Tuple") {
				info = "a TypeVar constraint"
			}
		}
		st.visitTypeParamBoundOrDefault(tp.Child("bound"), name, tp, tp, info, c)
		st.visitTypeParamBoundOrDefault(tp.Child("default_value"), name, tp, defaultKey{tp}, "a TypeVar default", d)
	case "TypeVarTuple":
		st.visitTypeParamBoundOrDefault(tp.Child("default_value"), name, tp, tp, "a TypeVarTuple default", d)
	case "ParamSpec":
		st.visitTypeParamBoundOrDefault(tp.Child("default_value"), name, tp, tp, "a ParamSpec default", d)
	}
}

func (st *symtable) visitPatterns(list []*Node, d int) {
	for _, p := range list {
		st.visitPattern(p, d)
	}
}

func (st *symtable) visitPattern(p *Node, d int) {
	st.check('p', d)
	c := d + symPatternFrame
	switch p.Type {
	case "MatchValue":
		st.visitExpr(p.Child("value"), d)
	case "MatchSingleton":
	case "MatchSequence", "MatchOr":
		st.visitPatterns(p.Children("patterns"), c)
	case "MatchStar":
		if name, ok := p.Str("name"); ok {
			st.addDef(name, defLocal, locOf(p))
		}
	case "MatchMapping":
		st.visitExprs(p.Children("keys"), c)
		st.visitPatterns(p.Children("patterns"), c)
		if rest, ok := p.Str("rest"); ok {
			st.addDef(rest, defLocal, locOf(p))
		}
	case "MatchClass":
		st.visitExpr(p.Child("cls"), c)
		st.visitPatterns(p.Children("patterns"), c)
		kwdPatterns := p.Children("kwd_patterns")
		for i, attr := range p.Names("kwd_attrs") {
			st.checkName(attr, locOf(kwdPatterns[i]), "Store")
		}
		st.visitPatterns(kwdPatterns, c)
	case "MatchAs":
		if sub := p.Child("pattern"); sub != nil {
			st.visitPattern(sub, c)
		}
		if name, ok := p.Str("name"); ok {
			st.addDef(name, defLocal, locOf(p))
		}
	}
}

func (st *symtable) visitParams(args []*Node) {
	for _, a := range args {
		name, _ := a.Str("arg")
		st.addDef(name, defParam, locOf(a))
	}
}

// Visits one annotation of an annotated assignment inside the annotation block of the current block
func (st *symtable) visitAnnotation(annotation *Node, key any, d int) {
	unevaluated := st.cur.kind == functionBlock
	if ((st.cur.kind == classBlock && st.cur.inConditionalBlock) || st.cur.kind == moduleBlock) && !st.cur.hasConditionalAnnotations {
		st.cur.hasConditionalAnnotations = true
		st.addDef("__conditional_annotations__", symUse, locOf(annotation))
	}
	parent := st.cur
	if parent.annotationBlock == nil {
		currentKind := parent.kind
		st.enterBlock("__annotate__", annotationBlock, key, locOf(annotation))
		parent.annotationBlock = st.cur
		if currentKind == classBlock && !st.future.annotations {
			st.cur.canSeeClassScope = true
			parent.needsClassdict = true
			st.addDef("__classdict__", symUse, locOf(annotation))
		}
	} else {
		st.enterExistingBlock(parent.annotationBlock, false)
	}
	if unevaluated {
		st.cur.inUnevaluatedAnnotation = true
	}
	st.visitExpr(annotation, d)
	if unevaluated {
		st.cur.inUnevaluatedAnnotation = false
	}
	st.exitBlock()
}

func (st *symtable) visitArgAnnotations(args []*Node, d int) {
	for _, a := range args {
		if ann := a.Child("annotation"); ann != nil {
			st.cur.annotationsUsed = true
			st.visitExpr(ann, d+symArgAnnotsFrame)
		}
	}
}

// Visits the parameter and return annotations of a function inside their own annotation block
func (st *symtable) visitAnnotations(o, a, returns *Node, d int) {
	c := d + symAnnotsFrame
	inClass := st.cur.canSeeClassScope
	currentKind := st.cur.kind
	st.enterBlock("__annotate__", annotationBlock, a, locOf(o))
	if inClass || currentKind == classBlock {
		st.cur.canSeeClassScope = true
		st.addDef("__classdict__", symUse, locOf(o))
	}
	st.visitArgAnnotations(a.Children("posonlyargs"), c)
	st.visitArgAnnotations(a.Children("args"), c)
	for _, f := range []string{"vararg", "kwarg"} {
		if arg := a.Child(f); arg != nil {
			if ann := arg.Child("annotation"); ann != nil {
				st.cur.annotationsUsed = true
				st.visitExpr(ann, c)
			}
		}
	}
	st.visitArgAnnotations(a.Children("kwonlyargs"), c)
	if returns != nil {
		st.cur.annotationsUsed = true
		st.visitExpr(returns, c)
	}
	st.exitBlock()
}

func (st *symtable) visitArguments(a *Node) {
	st.visitParams(a.Children("posonlyargs"))
	st.visitParams(a.Children("args"))
	st.visitParams(a.Children("kwonlyargs"))
	if v := a.Child("vararg"); v != nil {
		name, _ := v.Str("arg")
		st.addDef(name, defParam, locOf(v))
		st.cur.varargs = true
	}
	if k := a.Child("kwarg"); k != nil {
		name, _ := k.Str("arg")
		st.addDef(name, defParam, locOf(k))
		st.cur.varkeywords = true
	}
}

func (st *symtable) visitAlias(a *Node) {
	name, _ := a.Str("name")
	if asname, ok := a.Str("asname"); ok {
		name = asname
	}
	store := name
	if dot := strings.IndexByte(name, '.'); dot != -1 {
		store = name[:dot]
	}
	if name != "*" {
		st.addDef(store, defImport, locOf(a))
		return
	}
	if st.cur.kind != moduleBlock {
		st.raise("import * only allowed at module level", locOf(a))
	}
}

func (st *symtable) visitComprehension(lc *Node, d int) {
	st.cur.compIterTarget = true
	st.visitExpr(lc.Child("target"), d)
	st.cur.compIterTarget = false
	st.cur.compIterExpr++
	st.visitExpr(lc.Child("iter"), d)
	st.cur.compIterExpr--
	st.visitExprs(lc.Children("ifs"), d)
	if async, _ := lc.Int("is_async"); async != 0 {
		st.cur.coroutine = true
	}
}

// Visits a comprehension, its first iterable in the enclosing block and the rest in a block of its own
func (st *symtable) handleComprehension(e *Node, scopeName string, elt, value *Node, d int) {
	isGenerator := e.Type == "GeneratorExp"
	generators := e.Children("generators")
	outermost := generators[0]
	st.cur.compIterExpr++
	st.visitExpr(outermost.Child("iter"), d)
	st.cur.compIterExpr--
	st.enterBlock(scopeName, functionBlock, e, locOf(e))
	switch e.Type {
	case "ListComp":
		st.cur.comprehension = listComprehension
	case "SetComp":
		st.cur.comprehension = setComprehension
	case "DictComp":
		st.cur.comprehension = dictComprehension
	default:
		st.cur.comprehension = generatorExpression
	}
	if async, _ := outermost.Int("is_async"); async != 0 {
		st.cur.coroutine = true
	}
	st.addDef(".0", defParam, st.cur.loc)
	st.cur.compIterTarget = true
	st.visitExpr(outermost.Child("target"), d)
	st.cur.compIterTarget = false
	st.visitExprs(outermost.Children("ifs"), d)
	for _, g := range generators[1:] {
		st.visitComprehension(g, d)
	}
	if value != nil {
		st.visitExpr(value, d)
	}
	st.visitExpr(elt, d)
	st.cur.generator = isGenerator
	isAsync := st.cur.coroutine && !isGenerator
	st.exitBlock()
	if isAsync && !(st.cur.kind == functionBlock && st.cur.coroutine) && st.cur.comprehension == noComprehension {
		st.raise("asynchronous comprehension outside of an asynchronous function", locOf(e))
	}
	if isAsync {
		st.cur.coroutine = true
	}
}

func (st *symtable) raiseIfAnnotationBlock(name string, e *Node) {
	switch st.cur.kind {
	case annotationBlock:
		st.raise(name+" cannot be used within an annotation", locOf(e))
	case typeVariableBlock:
		st.raise(name+" cannot be used within "+st.cur.scopeInfo, locOf(e))
	case typeAliasBlock:
		st.raise(name+" cannot be used within a type alias", locOf(e))
	case typeParametersBlock:
		st.raise(name+" cannot be used within the definition of a generic", locOf(e))
	}
}

func (st *symtable) raiseIfComprehensionBlock(e *Node) {
	msg := "'yield' inside generator expression"
	switch st.cur.comprehension {
	case listComprehension:
		msg = "'yield' inside list comprehension"
	case setComprehension:
		msg = "'yield' inside set comprehension"
	case dictComprehension:
		msg = "'yield' inside dict comprehension"
	}
	st.raise(msg, locOf(e))
}

func (st *symtable) errorAtDirective(ste *symEntry, name, msg string) {
	for _, d := range ste.directives {
		if d.name == name {
			st.raise(msg, d.loc)
		}
	}
	panic(internalError("RuntimeError", "BUG: internal directive bookkeeping broken"))
}

// Decides the scope of one name of a block from its flags and the enclosing bindings as analyze_name
func (st *symtable) analyzeName(ste *symEntry, scopes map[string]int, name string, flags int, bound, local, free, global, typeParams nameSet, classEntry *symEntry) {
	if flags&defGlobal != 0 {
		if flags&defNonlocal != 0 {
			st.errorAtDirective(ste, name, "name '"+name+"' is nonlocal and global")
		}
		scopes[name] = scopeGlobalExplicit
		global[name] = struct{}{}
		if bound != nil {
			delete(bound, name)
		}
		return
	}
	if flags&defNonlocal != 0 {
		if bound == nil {
			st.errorAtDirective(ste, name, "nonlocal declaration not allowed at module level")
		}
		if !bound.has(name) {
			st.errorAtDirective(ste, name, "no binding for nonlocal '"+name+"' found")
		}
		if typeParams.has(name) {
			st.errorAtDirective(ste, name, "nonlocal binding not allowed for type parameter '"+name+"'")
		}
		scopes[name] = scopeFree
		free[name] = struct{}{}
		return
	}
	if flags&defBound != 0 {
		scopes[name] = scopeLocal
		local[name] = struct{}{}
		delete(global, name)
		if flags&defTypeParam != 0 {
			typeParams[name] = struct{}{}
		} else {
			delete(typeParams, name)
		}
		return
	}
	if classEntry != nil {
		classFlags := classEntry.symbol(name)
		if classFlags&defGlobal != 0 {
			scopes[name] = scopeGlobalExplicit
			return
		}
		if classFlags&defBound != 0 && classFlags&defNonlocal == 0 {
			scopes[name] = scopeGlobalImplicit
			return
		}
	}
	if bound != nil && bound.has(name) {
		scopes[name] = scopeFree
		free[name] = struct{}{}
		return
	}
	scopes[name] = scopeGlobalImplicit
}

func isFreeInAnyChild(entry *symEntry, key string) bool {
	for _, child := range entry.children {
		if child.scope(key) == scopeFree {
			return true
		}
	}
	return false
}

// Merges the symbols of an inlined comprehension into the block that runs it as inline_comprehension
func inlineComprehension(ste, comp *symEntry, scopes map[string]int, compFree, inlinedCells nameSet) {
	var remove []string
	for _, k := range append([]string(nil), comp.symbols.keys...) {
		compFlags := comp.symbols.vals[k]
		if compFlags&defParam != 0 {
			continue
		}
		scope := (compFlags >> scopeOffset) & scopeMask
		onlyFlags := compFlags & ((1 << scopeOffset) - 1)
		if scope == scopeCell || onlyFlags&defCompCell != 0 {
			inlinedCells[k] = struct{}{}
		}
		existing, exists := ste.symbols.get(k)
		if scope == scopeFree && ste.kind == classBlock && (k == "__class__" || k == "__classdict__" || k == "__conditional_annotations__") {
			scope = scopeGlobalImplicit
			if !isFreeInAnyChild(comp, k) {
				delete(compFree, k)
			}
			remove = append(remove, k)
		}
		if !exists {
			ste.symbols.set(k, onlyFlags)
			scopes[k] = scope
		} else if existing&defBound != 0 && ste.kind != classBlock {
			if !isFreeInAnyChild(comp, k) {
				delete(compFree, k)
			}
		}
	}
	for _, k := range remove {
		comp.symbols.del(k)
	}
}

func sortedNames(s nameSet) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Records the decided scopes in the symbols of a block and adds the unresolved free names of its children
func updateSymbols(symbols *symDict, scopes map[string]int, bound, free, inlinedCells nameSet, classFlag bool) {
	for _, name := range symbols.keys {
		flags := symbols.vals[name]
		if inlinedCells.has(name) {
			flags |= defCompCell
		}
		scope, ok := scopes[name]
		if !ok {
			panic(internalError("KeyError", strconv.Quote(name)))
		}
		symbols.vals[name] = flags | scope<<scopeOffset
	}
	for _, name := range sortedNames(free) {
		if v, ok := symbols.get(name); ok {
			if classFlag {
				symbols.set(name, v|defFreeClass)
			}
			continue
		}
		if bound != nil && !bound.has(name) {
			continue
		}
		symbols.set(name, scopeFree<<scopeOffset)
	}
}

// Decides the scope of every name of a block and its children as analyze_block
func (st *symtable) analyzeBlock(ste *symEntry, bound, free, global, typeParams nameSet, classEntry *symEntry) {
	local := nameSet{}
	scopes := map[string]int{}
	newGlobal, newFree, newBound, inlinedCells := nameSet{}, nameSet{}, nameSet{}, nameSet{}
	if ste.kind == classBlock {
		newGlobal.union(global)
		if bound != nil {
			newBound.union(bound)
		}
	}
	for _, name := range append([]string(nil), ste.symbols.keys...) {
		st.analyzeName(ste, scopes, name, ste.symbols.vals[name], bound, local, free, global, typeParams, classEntry)
	}
	if ste.kind != classBlock {
		if ste.functionLike() {
			newBound.union(local)
		}
		if bound != nil {
			newBound.union(bound)
		}
		newGlobal.union(global)
	} else {
		newBound["__class__"] = struct{}{}
		newBound["__classdict__"] = struct{}{}
		newBound["__conditional_annotations__"] = struct{}{}
	}
	for _, entry := range ste.children {
		var newClassEntry *symEntry
		if entry.canSeeClassScope {
			if ste.kind == classBlock {
				newClassEntry = ste
			} else if classEntry != nil {
				newClassEntry = classEntry
			}
		}
		inlineComp := entry.comprehension != noComprehension && !entry.generator && !ste.canSeeClassScope
		childFree := newFree.clone()
		st.analyzeBlock(entry, newBound.clone(), childFree, newGlobal.clone(), typeParams.clone(), newClassEntry)
		if inlineComp {
			inlineComprehension(ste, entry, scopes, childFree, inlinedCells)
			entry.compInlined = true
		}
		newFree.union(childFree)
	}
	for i := len(ste.children) - 1; i >= 0; i-- {
		if entry := ste.children[i]; entry.compInlined {
			spliced := append([]*symEntry(nil), ste.children[:i]...)
			spliced = append(spliced, entry.children...)
			ste.children = append(spliced, ste.children[i+1:]...)
		}
	}
	if ste.functionLike() {
		for name, scope := range scopes {
			if scope == scopeLocal && (newFree.has(name) || inlinedCells.has(name)) {
				scopes[name] = scopeCell
				delete(newFree, name)
			}
		}
	} else if ste.kind == classBlock {
		if newFree.has("__class__") {
			delete(newFree, "__class__")
			ste.needsClassClosure = true
		}
		if newFree.has("__classdict__") {
			delete(newFree, "__classdict__")
			ste.needsClassdict = true
		}
		if newFree.has("__conditional_annotations__") {
			delete(newFree, "__conditional_annotations__")
			ste.hasConditionalAnnotations = true
		}
	}
	updateSymbols(ste.symbols, scopes, bound, newFree, inlinedCells, ste.kind == classBlock || ste.canSeeClassScope)
	free.union(newFree)
}
