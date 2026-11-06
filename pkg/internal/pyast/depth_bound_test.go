package pyast

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	bTok = iota
	bCall
	bLook
)

type bItem struct {
	kind   int
	tok    int
	callee string
	groups []int
}

type bChain struct {
	items   []bItem
	loop    bool
	invalid bool
	raises  bool
}

type bGraph struct {
	rules   map[string][]bChain
	order   []string
	leaders map[string]string
	frame   map[string]bool
	clears  map[string]bool
}

var bTokIdent = map[string]int{"cENDMARKER": cENDMARKER, "cNAME": cNAME, "cNUMBER": cNUMBER, "cSTRING": cSTRING, "cNEWLINE": cNEWLINE, "cINDENT": cINDENT, "cDEDENT": cDEDENT, "cTYPECOMMENT": cTYPECOMMENT, "cFSTART": cFSTART, "cFMIDDLE": cFMIDDLE, "cFEND": cFEND, "cTSTART": cTSTART, "cTMIDDLE": cTMIDDLE, "cTEND": cTEND}

// Reads every rule function of depth_gen.go into its condition chains of tokens, calls and lookaheads
func loadBGraph(t *testing.T) *bGraph {
	fs := gotoken.NewFileSet()
	f, err := goparser.ParseFile(fs, "depth_gen.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	g := &bGraph{rules: map[string][]bChain{}, leaders: map[string]string{}, frame: map[string]bool{}, clears: map[string]bool{}}
	var funcs []*ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil {
			funcs = append(funcs, fd)
			g.rules[fd.Name.Name] = nil
			g.order = append(g.order, fd.Name.Name)
		}
	}
	calls := 0
	for _, fd := range funcs {
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if s, ok := c.Fun.(*ast.SelectorExpr); ok {
					if _, isRule := g.rules[s.Sel.Name]; isRule {
						calls++
					}
				}
				if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "look" {
					if a, ok := c.Args[1].(*ast.SelectorExpr); ok {
						if _, isRule := g.rules[a.Sel.Name]; isRule {
							calls++
						}
					}
				}
			}
			return true
		})
	}
	seen := 0
	for _, fd := range funcs {
		name := fd.Name.Name
		gid := 0
		var flat func(e ast.Expr, groups []int, out *[]bItem)
		flat = func(e ast.Expr, groups []int, out *[]bItem) {
			switch e := e.(type) {
			case *ast.ParenExpr:
				flat(e.X, groups, out)
			case *ast.UnaryExpr:
				flat(e.X, groups, out)
			case *ast.BinaryExpr:
				if id, ok := e.Y.(*ast.Ident); ok && e.Op == gotoken.LOR && id.Name == "true" {
					gid++
					flat(e.X, append(append([]int{}, groups...), gid), out)
					return
				}
				flat(e.X, groups, out)
				flat(e.Y, groups, out)
			case *ast.CallExpr:
				s := e.Fun.(*ast.SelectorExpr)
				m := s.Sel.Name
				if _, isRule := g.rules[m]; isRule {
					seen++
					*out = append(*out, bItem{kind: bCall, callee: m, groups: groups})
					if strings.HasSuffix(m, "_raw") {
						g.leaders[name] = m
					}
					return
				}
				switch m {
				case "tok", "forced":
					var tt int
					switch a := e.Args[0].(type) {
					case *ast.BasicLit:
						tt, _ = strconv.Atoi(a.Value)
					case *ast.Ident:
						v, ok := bTokIdent[a.Name]
						if !ok {
							t.Fatalf("token ident %s", a.Name)
						}
						tt = v
					}
					*out = append(*out, bItem{kind: bTok, tok: tt, groups: groups})
				case "soft", "anySoft", "nameTok":
					*out = append(*out, bItem{kind: bTok, tok: cNAME, groups: groups})
				case "number":
					*out = append(*out, bItem{kind: bTok, tok: cNUMBER, groups: groups})
				case "stringTok":
					*out = append(*out, bItem{kind: bTok, tok: cSTRING, groups: groups})
				case "look":
					a := e.Args[1].(*ast.SelectorExpr)
					if _, isRule := g.rules[a.Sel.Name]; isRule {
						seen++
						*out = append(*out, bItem{kind: bLook, callee: a.Sel.Name, groups: groups})
					}
				case "lookTok", "lookSoft", "cut", "at", "raise":
				default:
					if !strings.HasPrefix(m, "h") {
						t.Fatalf("%s calls %s in a condition", name, m)
					}
				}
			}
		}
		var walk func(list []ast.Stmt, invalid bool)
		addCond := func(cond ast.Expr, loop, invalid bool, body []ast.Stmt) {
			var items []bItem
			flat(cond, nil, &items)
			raises := false
			if len(body) > 0 {
				if as, ok := body[0].(*ast.AssignStmt); ok {
					if c, ok := as.Rhs[0].(*ast.CallExpr); ok {
						if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "raise" {
							raises = true
						}
					}
				}
			}
			if len(items) > 0 {
				g.rules[name] = append(g.rules[name], bChain{items: items, loop: loop, invalid: invalid, raises: raises})
			}
		}
		walk = func(list []ast.Stmt, invalid bool) {
			for _, s := range list {
				switch s := s.(type) {
				case *ast.IfStmt:
					if sel, ok := s.Cond.(*ast.SelectorExpr); ok && sel.Sel.Name == "invalid" {
						walk(s.Body.List, true)
						continue
					}
					addCond(s.Cond, false, invalid, s.Body.List)
					walk(s.Body.List, invalid)
					if s.Else != nil {
						walk([]ast.Stmt{s.Else}, invalid)
					}
				case *ast.ForStmt:
					if s.Cond != nil {
						addCond(s.Cond, true, invalid, nil)
					}
					walk(s.Body.List, invalid)
				case *ast.BlockStmt:
					walk(s.List, invalid)
				case *ast.AssignStmt:
					if sel, ok := s.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "invalid" {
						if id, ok := s.Rhs[0].(*ast.Ident); ok && id.Name == "false" {
							g.clears[name] = true
						}
					}
					for _, rhs := range s.Rhs {
						addCond(rhs, false, invalid, nil)
					}
				case *ast.ExprStmt:
					if c, ok := s.X.(*ast.CallExpr); ok {
						if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "enter" {
							g.frame[name] = true
						}
					}
				}
			}
		}
		walk(fd.Body.List, false)
	}
	if seen != calls {
		t.Fatalf("chains hold %d of %d rule calls", seen, calls)
	}
	return g
}

type bKey struct {
	name string
	inv  bool
}

type bNode struct {
	bKey
	b bool
}

type bArc struct {
	from, to bNode
	leaf     bool
	kb, ki   int
	ql, qg   []int
}

func bWithin(p, c bItem) bool {
	if len(p.groups) > len(c.groups) {
		return false
	}
	for i := range p.groups {
		if p.groups[i] != c.groups[i] {
			return false
		}
	}
	return true
}

func bCreditTok(t int) bool {
	return !recOpener(t) && !recCloser(t) && t != cINDENT && t != cDEDENT && t != cNEWLINE
}

// Builds the call arcs of one pass with leader copies and their bracket, indent and token credits
func (g *bGraph) arcs(t *testing.T, pass2 bool) []bArc {
	var keys []bKey
	for _, name := range g.order {
		keys = append(keys, bKey{name, false})
		if pass2 {
			keys = append(keys, bKey{name, true})
		}
	}
	chains := func(k bKey) []bChain {
		var out []bChain
		for _, c := range g.rules[k.name] {
			if k.inv || !c.invalid {
				out = append(out, c)
			}
		}
		return out
	}
	callee := func(k bKey, it bItem) bKey { return bKey{it.callee, k.inv && !g.clears[k.name]} }
	nullable, nr, ne := map[bKey]bool{}, map[bKey]bool{}, map[bKey]bool{}
	nl := func(k bKey, it bItem) bool {
		return it.kind == bTok && it.tok == cNEWLINE || it.kind == bCall && nr[callee(k, it)]
	}
	for changed := true; changed; {
		changed = false
		for _, name := range keys {
			for _, c := range chains(name) {
				empty := !c.loop && !c.raises
				seenNL, hasNL, hasCall := false, false, false
				for _, it := range c.items {
					if len(it.groups) == 0 && (it.kind == bTok || it.kind == bCall && !nullable[callee(name, it)]) {
						empty = false
					}
					if it.kind != bTok {
						hasCall = true
						if seenNL || ne[callee(name, it)] {
							if !ne[name] {
								ne[name], changed = true, true
							}
						}
					}
					if nl(name, it) {
						seenNL, hasNL = true, true
					}
				}
				if c.loop && hasNL && hasCall && !ne[name] {
					ne[name], changed = true, true
				}
				if (empty || strings.HasPrefix(name.name, "_loop0_")) && !nullable[name] {
					nullable[name], changed = true, true
				}
				if hasNL && !c.raises && !nr[name] {
					nr[name], changed = true, true
				}
			}
		}
	}
	const unknown = 1 << 20
	minB, minI := map[bKey]int{}, map[bKey]int{}
	for _, k := range keys {
		minB[k], minI[k] = unknown, unknown
	}
	for changed := true; changed; {
		changed = false
		for _, k := range keys {
			for _, ch := range chains(k) {
				if ch.raises || ch.loop {
					continue
				}
				nb, ni := 0, 0
				for _, p := range ch.items {
					def := len(p.groups) == 0
					switch {
					case p.kind == bCall && def:
						nb += min(minB[callee(k, p)], unknown)
						ni += min(minI[callee(k, p)], unknown)
					case p.kind == bCall:
						nb += min(minB[callee(k, p)], 0)
						ni += min(minI[callee(k, p)], 0)
					case p.kind != bTok:
					case recOpener(p.tok) && def:
						nb++
					case recCloser(p.tok):
						nb--
					case p.tok == cINDENT && def:
						ni++
					case p.tok == cDEDENT:
						ni--
					}
				}
				nb, ni = max(min(nb, unknown), -1), max(min(ni, unknown), -1)
				if nb < minB[k] {
					minB[k], changed = nb, true
				}
				if ni < minI[k] {
					minI[k], changed = ni, true
				}
			}
			if strings.HasPrefix(k.name, "_loop0_") || strings.HasPrefix(k.name, "_loop1_") {
				if minB[k] > 0 || minI[k] > 0 {
					minB[k], minI[k], changed = 0, 0, true
				}
			}
		}
	}
	childB := func(k bKey) int {
		if v := minB[k]; v > 0 && v < unknown {
			return v
		}
		return 0
	}
	childI := func(k bKey) int {
		if v := minI[k]; v > 0 && v < unknown {
			return v
		}
		return 0
	}
	type edge struct {
		from, to          bKey
		defSame, possSame bool
		kb, ki            int
		ql, qg            []int
	}
	var es []edge
	adj := map[string][]string{}
	keyName := func(k bKey) string { return fmt.Sprint(k.name, " ", k.inv) }
	byName := map[string]bKey{}
	for _, k := range keys {
		byName[keyName(k)] = k
	}
	for _, name := range keys {
		for _, ch := range chains(name) {
			for j, c := range ch.items {
				if c.kind == bTok {
					continue
				}
				e := edge{from: name, to: callee(name, c), defSame: true, possSame: true}
				lastNL := -1
				for i := 0; i < j; i++ {
					p := ch.items[i]
					def := bWithin(p, c)
					if p.kind != bLook {
						e.defSame = false
					}
					if def && (p.kind == bTok || p.kind == bCall && !nullable[callee(name, p)]) {
						e.possSame = false
					}
					if nl(name, p) {
						lastNL = i
					}
					if p.kind == bCall && def {
						e.kb += childB(callee(name, p))
						e.ki += childI(callee(name, p))
					}
					if p.kind != bTok {
						continue
					}
					switch {
					case recOpener(p.tok) && def:
						e.kb++
					case recCloser(p.tok):
						e.kb--
					case p.tok == cINDENT && def:
						e.ki++
					case p.tok == cDEDENT:
						e.ki--
					case bCreditTok(p.tok) && def:
						e.qg = append(e.qg, p.tok)
						if i > lastNL && !ne[e.to] {
							e.ql = append(e.ql, p.tok)
						}
					}
				}
				if e.kb < 0 || e.ki < 0 {
					t.Errorf("%v calls %s after an unmatched closer", name, c.callee)
				}
				es = append(es, e)
				if e.possSame {
					adj[keyName(name)] = append(adj[keyName(name)], keyName(e.to))
				}
			}
		}
	}
	negB, negI := map[bKey]bool{}, map[bKey]bool{}
	for changed := true; changed; {
		changed = false
		for _, name := range keys {
			for _, ch := range chains(name) {
				if ch.raises {
					continue
				}
				kb, ki := 0, 0
				for _, p := range ch.items {
					switch {
					case p.kind == bCall && negB[callee(name, p)]:
						kb = -1 << 20
					case p.kind == bCall && negI[callee(name, p)]:
						ki = -1 << 20
					case p.kind != bTok:
					case recOpener(p.tok) && len(p.groups) == 0:
						kb++
					case recCloser(p.tok):
						kb--
					case p.tok == cINDENT && len(p.groups) == 0:
						ki++
					case p.tok == cDEDENT:
						ki--
					}
				}
				if kb < 0 && !negB[name] {
					negB[name], changed = true, true
				}
				if ki < 0 && !negI[name] {
					negI[name], changed = true, true
				}
			}
		}
	}
	for _, name := range keys {
		for _, ch := range chains(name) {
			after := false
			for _, p := range ch.items {
				if p.kind != bTok && after {
					t.Errorf("%v enters %s after a child that may close an outer bracket or block", name, p.callee)
				}
				if p.kind == bCall && (negB[callee(name, p)] || negI[callee(name, p)]) {
					after = true
				}
			}
			if ch.loop && after {
				t.Errorf("%v loops over a child that may close an outer bracket or block", name)
			}
		}
	}
	for _, s := range []string{"file_rule", "eval_rule", "interactive_rule", "func_type_rule"} {
		if negB[bKey{s, pass2}] || negI[bKey{s, pass2}] {
			t.Errorf("start rule %s may close more than it opens", s)
		}
	}
	leaderOf := map[bKey]bKey{}
	for _, comp := range bSCC(adj) {
		for _, ln := range comp {
			l := byName[ln]
			if _, ok := g.leaders[l.name]; !ok {
				continue
			}
			for _, mn := range comp {
				m := byName[mn]
				if prev, ok := leaderOf[m]; ok && prev != l {
					t.Errorf("%v sits in the left recursion of %v and %v", m, prev, l)
				}
				leaderOf[m] = l
			}
		}
	}
	var out []bArc
	for _, e := range es {
		srcs := []bNode{{e.from, false}}
		if l, ok := leaderOf[e.from]; ok && l != e.from {
			srcs = append(srcs, bNode{e.from, true})
		}
		for _, src := range srcs {
			a := bArc{from: src, to: bNode{e.to, false}, kb: e.kb, ki: e.ki, ql: e.ql, qg: e.qg}
			l, inS := leaderOf[e.from]
			if inS && leaderOf[e.to] == l && e.defSame && (src.b || l == e.from) {
				if e.to == l {
					a.leaf = true
				} else {
					a.to.b = true
				}
			}
			out = append(out, a)
		}
	}
	return out
}

// Returns the strongly connected groups of the call graph in Tarjan order
func bSCC(adj map[string][]string) [][]string {
	idx, low, on := map[string]int{}, map[string]int{}, map[string]bool{}
	var stack []string
	var out [][]string
	n := 0
	var visit func(v string)
	visit = func(v string) {
		idx[v], low[v] = n, n
		n++
		stack = append(stack, v)
		on[v] = true
		for _, w := range adj[v] {
			if _, ok := idx[w]; !ok {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if on[w] {
				low[v] = min(low[v], idx[w])
			}
		}
		if low[v] == idx[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				on[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			out = append(out, comp)
		}
	}
	var keys []string
	for v := range adj {
		keys = append(keys, v)
	}
	sort.Strings(keys)
	for _, v := range keys {
		if _, ok := idx[v]; !ok {
			visit(v)
		}
	}
	return out
}

type bParams struct {
	beta, delta, gl, gg int
	ql, qg              map[int]bool
}

// Returns the deepest frame weight of every node, false when some call cycle gains frames
func bSolve(arcs []bArc, frame map[string]bool, pr bParams) (map[bNode]int, bool) {
	id := map[bNode]int{}
	var nodes []bNode
	index := func(n bNode) int {
		i, ok := id[n]
		if !ok {
			i = len(nodes)
			id[n] = i
			nodes = append(nodes, n)
		}
		return i
	}
	from, to, gain := make([]int, len(arcs)), make([]int, len(arcs)), make([]int, len(arcs))
	for k, a := range arcs {
		from[k], to[k] = index(a.from), index(a.to)
		gain[k] = -(pr.beta*a.kb + pr.delta*a.ki)
		for _, tt := range a.ql {
			if pr.ql[tt] {
				gain[k] -= pr.gl
			}
		}
		for _, tt := range a.qg {
			if pr.qg[tt] {
				gain[k] -= pr.gg
			}
		}
		if a.leaf {
			to[k] = -1
		}
	}
	f := make([]int, len(nodes))
	for i, n := range nodes {
		if frame[n.bKey.name] {
			f[i] = 1
		}
	}
	w := append([]int{}, f...)
	ok := false
	for it := 0; it <= len(w)+1 && !ok; it++ {
		ok = true
		for k := range arcs {
			sub := 1
			if to[k] >= 0 {
				sub = w[to[k]]
			}
			if c := f[from[k]] + sub + gain[k]; c > w[from[k]] {
				w[from[k]], ok = c, false
			}
		}
	}
	out := map[bNode]int{}
	for i, n := range nodes {
		out[n] = w[i]
	}
	return out, ok
}

// Finds the smallest token credit sets and per unit costs that leave no cycle gaining frames
func bDerive(t *testing.T, g *bGraph, pass2 bool) recBound {
	const huge = 100000
	arcs := g.arcs(t, pass2)
	all := map[int]bool{}
	for _, a := range arcs {
		for _, tt := range append(append([]int{}, a.ql...), a.qg...) {
			all[tt] = true
		}
	}
	pr := bParams{huge, huge, huge, huge, map[int]bool{}, map[int]bool{}}
	for tt := range all {
		pr.ql[tt], pr.qg[tt] = true, true
	}
	if _, ok := bSolve(arcs, g.frame, pr); !ok {
		t.Fatalf("pass two %v has a call cycle that consumes no credited token", pass2)
	}
	var cand []int
	for tt := range all {
		cand = append(cand, tt)
	}
	sort.Ints(cand)
	pref := []int{cNAME, cNUMBER, cSTRING, 12, 23, 22, 11, 16, 13, 49, 51, 53, cFMIDDLE, cTMIDDLE, 14, 15}
	for i := len(pref) - 1; i >= 0; i-- {
		for k, v := range cand {
			if v == pref[i] {
				cand = append([]int{v}, append(cand[:k:k], cand[k+1:]...)...)
				break
			}
		}
	}
	for _, set := range []map[int]bool{pr.qg, pr.ql} {
		for _, tt := range cand {
			delete(set, tt)
			if _, ok := bSolve(arcs, g.frame, pr); !ok {
				set[tt] = true
			}
		}
	}
	var mins [4]int
	for i, field := range []*int{&pr.beta, &pr.delta, &pr.gl, &pr.gg} {
		lo, hi := 0, huge
		for lo+1 < hi {
			*field = (lo + hi) / 2
			if _, ok := bSolve(arcs, g.frame, pr); ok {
				hi = *field
			} else {
				lo = *field
			}
		}
		mins[i], *field = hi, huge
	}
	pr.beta, pr.delta, pr.gl, pr.gg = mins[0], mins[1], mins[2], mins[3]
	for {
		if _, ok := bSolve(arcs, g.frame, pr); ok {
			break
		}
		pr.beta, pr.delta, pr.gl, pr.gg = pr.beta+1, pr.delta+1, pr.gl+1, pr.gg+1
	}
	w, _ := bSolve(arcs, g.frame, pr)
	b := recBound{bracket: pr.beta, indent: pr.delta, local: pr.gl, global: pr.gg, localToks: pr.ql, globalToks: pr.qg}
	for _, s := range []string{"file_rule", "eval_rule", "interactive_rule", "func_type_rule"} {
		b.base = max(b.base, w[bNode{bKey{s, pass2}, false}])
	}
	return b
}

// The bounds the recognizer skip uses are the smallest ones a cycle analysis of depth_gen.go proves sound
func TestRecognizerBoundProvedFromDepthGen(t *testing.T) {
	g := loadBGraph(t)
	for i, want := range []recBound{passOneBound, passTwoBound} {
		if got := bDerive(t, g, i == 1); !reflect.DeepEqual(got, want) {
			t.Errorf("pass %d derived %+v, depth_bound.go holds %+v", i+1, got, want)
		}
	}
}

// Every recorded deep context and parse case stays within the bound of the pass it can reach
func TestRecognizerBoundCoversObservedStacks(t *testing.T) {
	var rows [][]any
	if err := json.Unmarshal(readGzip(t, "depth_limits.json.gz"), &rows); err != nil {
		t.Fatal(err)
	}
	var srcs []string
	for i, r := range rows {
		if i%8 == 0 {
			ctx, pre, leaf, n := r[0].(string), r[1].(string), r[2].(string), int(r[3].(float64))
			srcs = append(srcs, strings.ReplaceAll(ctx, "§", strings.Repeat(pre, n)+leaf))
		}
	}
	var cases []struct {
		Src *string `json:"src"`
	}
	if err := json.Unmarshal(readGzip(t, "cases.json.gz"), &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Src != nil {
			srcs = append(srcs, *c.Src)
		}
	}
	checked := 0
	for _, src := range srcs {
		buf := translateNewlines([]byte(src))
		lx := tokenizeWith(buf, false, false)
		p := &parser{lx: lx, toks: lx.toks, parens: map[*Node]bool{}, filename: "<unknown>", fv: latestMinor, mode: "exec"}
		_, err := p.run(p.file)
		var se *SyntaxError
		if err != nil && !errors.As(err, &se) || len(p.toks) == 0 {
			continue
		}
		types := make([]int, len(p.toks))
		for i := range p.toks {
			types[i] = cpyTokenType(&p.toks[i])
		}
		bound := passOneBound.frames(types)
		if err != nil {
			bound = passTwoBound.frames(types)
		}
		overflow, peak := p.runRecognizer(types)
		if overflow && bound <= maxStack || peak > bound {
			t.Errorf("peak %d overflow %v above bound %d for %.120q", peak, overflow, bound, src)
		}
		checked++
	}
	if checked < 300 {
		t.Fatalf("only %d sources checked", checked)
	}
}
