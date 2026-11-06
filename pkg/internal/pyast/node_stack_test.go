package pyast

import (
	"math"
	"sort"
	"testing"
)

var cheapest = map[string]string{"expr": "Name", "stmt": "Pass", "pattern": "MatchAs", "excepthandler": "ExceptHandler",
	"type_param": "TypeVar", "type_ignore": "TypeIgnore", "mod": "Module"}

// Builds a node of the class with every required field holding the cheapest node of its type
func minimalNode(typ string) *Node {
	spec := specs[typ]
	vals := make([]any, len(spec.fields))
	for i, f := range spec.fields {
		switch f.kind {
		case kNode:
			if simpleSum(f.typ) {
				vals[i] = NewNode(concrete(f.typ)[0])
			} else {
				vals[i] = minimalNode(concrete(f.typ)[0])
			}
		case kIdent:
			vals[i] = "x"
		case kInt:
			vals[i] = 0
		}
	}
	n := NewNode(typ, vals...)
	n.Lineno, n.EndLineno = 1, 1
	return n
}

// Lists the classes a field of the ASDL type may hold, the cheapest first
func concrete(typ string) []string {
	if s := specs[typ]; s != nil {
		return []string{typ}
	}
	var out []string
	for name, s := range specs {
		if s.base == typ && name != cheapest[typ] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	if c, ok := cheapest[typ]; ok {
		out = append([]string{c}, out...)
	}
	return out
}

func foldChecked(n *Node) bool {
	switch n.spec.base {
	case "stmt", "expr", "pattern":
		return true
	}
	return false
}

// Returns one node of the class per way to fill one node field down to the next stmt, expr or pattern
func variants(typ string) []*Node {
	var out []*Node
	for i, f := range specs[typ].fields {
		if f.kind != kNode && f.kind != kOptNode && f.kind != kNodeList || simpleSum(f.typ) {
			continue
		}
		for _, c := range concrete(f.typ) {
			kids := []*Node{minimalNode(c)}
			if !foldChecked(kids[0]) {
				kids = append(kids, variants(c)...)
			}
			for _, k := range kids {
				n := minimalNode(typ)
				if f.kind == kNodeList {
					n.vals[i] = []*Node{k}
				} else {
					n.vals[i] = k
				}
				out = append(out, n)
			}
		}
	}
	return out
}

// Places a node under the shortest tree a converted module can hold it in
func embed(n *Node) *Node {
	expr := func(e *Node) *Node { return NewNode("Module", []*Node{NewNode("Expr", e)}, nil) }
	switch n.spec.base {
	case "mod":
		return n
	case "stmt":
		return NewNode("Module", []*Node{n}, nil)
	case "expr":
		return expr(n)
	case "pattern":
		return embed(NewNode("match_case", n, nil, []*Node{minimalNode("Pass")}))
	case "excepthandler":
		return NewNode("Module", []*Node{NewNode("Try", []*Node{minimalNode("Pass")}, []*Node{n}, nil, nil)}, nil)
	case "type_param":
		fd := minimalNode("FunctionDef")
		fd.Set("type_params", []*Node{n})
		return NewNode("Module", []*Node{fd}, nil)
	case "type_ignore":
		return NewNode("Module", nil, []*Node{n})
	}
	switch n.Type {
	case "comprehension":
		return expr(NewNode("ListComp", minimalNode("Name"), []*Node{n}))
	case "arguments":
		return expr(NewNode("Lambda", n, minimalNode("Name")))
	case "arg":
		a := minimalNode("arguments")
		a.Set("args", []*Node{n})
		return embed(a)
	case "keyword":
		return expr(NewNode("Call", minimalNode("Name"), nil, []*Node{n}))
	case "alias":
		return NewNode("Module", []*Node{NewNode("Import", []*Node{n})}, nil)
	case "withitem":
		return NewNode("Module", []*Node{NewNode("With", []*Node{n}, []*Node{minimalNode("Pass")}, nil)}, nil)
	case "match_case":
		return NewNode("Module", []*Node{NewNode("Match", minimalNode("Name"), []*Node{n})}, nil)
	}
	panic(n.Type)
}

type depthEdge struct{ from, to string }

// Longest path weights from the roots, failing on a positive cycle, the most a walker leads obj2ast at any class
func longestLead(t *testing.T, start map[string]int, edges map[depthEdge]int) (string, int) {
	pi := map[string]int{}
	for r, v := range start {
		pi[r] = v
	}
	for round := 0; ; round++ {
		changed := false
		for e, w := range edges {
			if v, ok := pi[e.from]; ok {
				if old, seen := pi[e.to]; !seen || v+w > old {
					pi[e.to] = v + w
					changed = true
				}
			}
		}
		if !changed {
			break
		}
		if round > len(specs)+1 {
			t.Fatalf("a positive cycle lets the walker gain on obj2ast without bound")
		}
	}
	worst, at := math.MinInt, ""
	for k, v := range pi {
		if _, root := start[k]; !root && v > worst {
			worst, at = v, k
		}
	}
	return at, worst
}

// Proves preprocess and ast2obj on the AST path stay under their limits whenever obj2ast stays under its own
func TestNodeStackObj2astTripsFirst(t *testing.T) {
	var trees []*Node
	var names []string
	for name, s := range specs {
		if !simpleSum(s.base) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		trees = append(trees, embed(minimalNode(name)))
		for _, v := range variants(name) {
			trees = append(trees, embed(v))
		}
	}
	type seg struct {
		kind  depthEdge
		dw, d int
	}
	foldStart, objStart := map[string]int{}, map[string]int{}
	foldEdges, objEdges := map[depthEdge]int{}, map[depthEdge]int{}
	exact := map[string]int{}
	note := func(m map[depthEdge]int, k depthEdge, w int) {
		if old, ok := m[k]; !ok || w > old {
			m[k] = w
		}
	}
	for _, root := range trees {
		o, a := map[*Node]int{}, map[*Node]int{}
		c := &obj2ast{limit: math.MaxInt, kB: 1, seen: func(x any, d int) { o[x.(*Node)] = d }}
		if _, err := c.sum("mod", root, 0); err != nil {
			t.Fatalf("%s: %v", root.Type, err)
		}
		objWalker{limit: math.MaxInt, seen: func(n *Node, d int) { a[n] = d }}.node(root, 0)
		for opt := 0; opt <= 2; opt++ {
			f := map[*Node]int{}
			(&folder{optimize: opt, checkOnly: opt == 0, limit: math.MaxInt, seen: func(n *Node, d int) { f[n] = d }}).mod(root)
			var walk func(n, anc *Node, path string)
			walk = func(n, anc *Node, path string) {
				for i, fs := range n.spec.fields {
					var kids []*Node
					switch v := n.vals[i].(type) {
					case *Node:
						kids = []*Node{v}
					case []*Node:
						kids = v
					}
					for _, k := range kids {
						if k == nil || simpleSum(k.spec.base) {
							continue
						}
						p := path + "." + fs.name + ">" + k.Type
						next := anc
						if fd, ok := f[k]; ok && foldChecked(k) {
							fa := 0
							if anc != root {
								fa = f[anc]
							}
							w := fd - fa - (o[k] - o[anc])
							key := anc.Type + p
							if old, ok := exact[key]; ok && old != w {
								t.Fatalf("fold lead of %s differs between trees: %d and %d", key, old, w)
							}
							exact[key] = w
							if anc == root {
								note(foldEdges, depthEdge{"^" + root.Type, k.Type}, w)
							} else {
								note(foldEdges, depthEdge{anc.Type, k.Type}, w)
							}
							next, p = k, ""
						}
						walk(k, next, p)
					}
				}
			}
			foldStart["^"+root.Type] = -o[root]
			walk(root, root, "")
		}
		var lead func(n *Node, oc int)
		lead = func(n *Node, oc int) {
			for _, v := range n.vals {
				var kids []*Node
				switch x := v.(type) {
				case *Node:
					kids = []*Node{x}
				case []*Node:
					kids = x
				}
				for _, k := range kids {
					if k == nil || simpleSum(k.spec.base) {
						continue
					}
					ok := o[n]
					if k.spec.attrs || foldChecked(k) || k.spec.base == "excepthandler" || k.spec.base == "type_param" {
						ok = o[k]
					}
					note(objEdges, depthEdge{n.Type, k.Type}, a[k]-a[n]-(ok-oc))
					lead(k, ok)
				}
			}
		}
		objStart[root.Type] = a[root] - o[root]
		lead(root, o[root])
	}
	at, worst := longestLead(t, foldStart, foldEdges)
	if worst > cpythonStack.foldLimit-nodeStack.obj2ast {
		t.Fatalf("preprocess can lead obj2ast by %d bytes at %s, more than the %d its limit allows", worst, at, cpythonStack.foldLimit-nodeStack.obj2ast)
	}
	t.Logf("%d trees, preprocess trails obj2ast by at least %d bytes (at %s)", len(trees), -worst, at)
	at, worst = longestLead(t, objStart, objEdges)
	if worst > cpythonStack.objLimit-nodeStack.obj2ast {
		t.Fatalf("ast2obj can lead obj2ast by %d bytes at %s, more than the %d its limit allows", worst, at, cpythonStack.objLimit-nodeStack.obj2ast)
	}
	t.Logf("ast2obj trails obj2ast by at least %d bytes (at %s)", -worst, at)
}
