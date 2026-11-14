package plugins

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var coreOverrideSkipDirs = map[string]bool{"tests": true, "test": true, "node_modules": true, ".git": true,
	"__pycache__": true, ".venv": true, "venv": true, "site-packages": true}

// One parsed plugin module, its slash path relative to the plugin root
type parsedSource struct {
	rel  string
	tree *pyast.Node
}

func coreIsTestFile(name string) bool {
	return strings.HasPrefix(name, "test_") || strings.HasSuffix(name, "_test.py") || name == "conftest.py"
}

// Relative slash paths of every plugin .py in pathlib order, skipped dirs and test files left out
func coreOverridePythonFiles(pluginDir string) ([]string, error) {
	paths, err := pypath.Glob(pluginDir, "**/*.py")
	if err != nil {
		return nil, err
	}
	rels := []string{}
	for _, p := range paths {
		rel, err := filepath.Rel(pluginDir, p)
		if err != nil {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		skip := coreIsTestFile(parts[len(parts)-1])
		for _, part := range parts[:len(parts)-1] {
			skip = skip || coreOverrideSkipDirs[part]
		}
		if !skip {
			rels = append(rels, filepath.ToSlash(rel))
		}
	}
	sort.SliceStable(rels, func(i, j int) bool {
		return lessByParts(pystr.SurrogateEscapeDecode([]byte(rels[i])), pystr.SurrogateEscapeDecode([]byte(rels[j])))
	})
	return rels, nil
}

// Parsed plugin modules, unparseable ones dropped while read failures and parser limits propagate
func parsePluginSources(pluginDir string, keep func(rel string) bool) ([]parsedSource, error) {
	rels, err := coreOverridePythonFiles(pluginDir)
	if err != nil {
		return nil, err
	}
	out := []parsedSource{}
	for _, rel := range rels {
		if !keep(rel) {
			continue
		}
		text, err := readTextReplace(filepath.Join(pluginDir, filepath.FromSlash(rel)), true)
		if err != nil {
			return nil, err
		}
		tree, err := pyast.Parse(text)
		if err != nil {
			var syn *pyast.SyntaxError
			if errors.As(err, &syn) {
				continue
			}
			return nil, err
		}
		out = append(out, parsedSource{rel: pystr.SurrogateEscapeDecode([]byte(rel)), tree: tree})
	}
	return out, nil
}

// Dotted module names of a plugin file, its __init__ standing for the package
func modulePartsOf(rel string) []string {
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	parts[len(parts)-1] = name[:len(name)-len(pypath.PathSuffix(name))]
	if parts[len(parts)-1] == "__init__" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// Import closure of __init__.py and dashboard modules, the files that run inside Icebath
func coreRuntimeFiles(files []parsedSource, local map[string]bool) map[string]bool {
	byModule := map[string]map[string]bool{}
	trees := map[string]*pyast.Node{}
	for _, f := range files {
		trees[f.rel] = f.tree
		parts := modulePartsOf(f.rel)
		for start := range parts {
			key := strings.Join(parts[start:], ".")
			if byModule[key] == nil {
				byModule[key] = map[string]bool{}
			}
			byModule[key][f.rel] = true
		}
	}
	resolve := func(rel string, node *pyast.Node) []string {
		var names []string
		level, _ := node.Int("level")
		if node.Is("Import") {
			for _, a := range node.Children("names") {
				n, _ := a.Str("name")
				names = append(names, n)
			}
		} else if node.Is("ImportFrom") {
			module, _ := node.Str("module")
			if level != 0 {
				parent := strings.Split(rel, "/")
				parent = parent[:len(parent)-1]
				base := parent
				if level > 1 {
					end := len(parent) - (level - 1)
					if end < 0 {
						end = max(0, len(parent)+end)
					}
					base = parent[:end]
				}
				prefix := strings.Trim(strings.Join(append(append([]string{}, base...), strings.Split(module, ".")...), "."), ".")
				names = append(names, prefix)
				for _, a := range node.Children("names") {
					n, _ := a.Str("name")
					names = append(names, strings.Trim(prefix+"."+n, "."))
				}
			} else if module != "" {
				names = append(names, module)
				for _, a := range node.Children("names") {
					n, _ := a.Str("name")
					names = append(names, module+"."+n)
				}
			}
		}
		var found []string
		for _, name := range names {
			if local[strings.Split(name, ".")[0]] || (node.Is("ImportFrom") && level != 0) {
				for r := range byModule[name] {
					found = append(found, r)
				}
			}
		}
		return found
	}
	seen := map[string]bool{}
	stack := []string{}
	for _, f := range files {
		parts := strings.Split(f.rel, "/")
		if f.rel == "__init__.py" || (len(parts) == 2 && parts[0] == "dashboard") {
			stack = append(stack, f.rel)
		}
	}
	for len(stack) > 0 {
		rel := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[rel] {
			continue
		}
		seen[rel] = true
		for _, node := range pyast.Walk(trees[rel]) {
			if node.Is("Import", "ImportFrom") {
				for _, r := range resolve(rel, node) {
					if !seen[r] {
						stack = append(stack, r)
					}
				}
			}
		}
	}
	return seen
}

// Module and package names the plugin ships itself, so importing them is local rather than core
func coreLocalNames(pluginDir string) (map[string]bool, error) {
	paths, err := pypath.Glob(pluginDir, "**/*")
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, p := range paths {
		rel, err := filepath.Rel(pluginDir, p)
		if err != nil {
			continue
		}
		skip := false
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			skip = skip || coreOverrideSkipDirs[part]
		}
		if skip {
			continue
		}
		name := pystr.SurrogateEscapeDecode([]byte(filepath.Base(p)))
		if pypath.PathSuffix(name) == ".py" {
			names[pypath.PathStem(name)] = true
		} else if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			names[name] = true
		}
	}
	return names, nil
}

// Attribute chain text ending in the base Name, the parts that are not attributes or names dropped
func astDotted(n *pyast.Node) string {
	parts := []string{}
	for n.Is("Attribute") {
		attr, _ := n.Str("attr")
		parts = append(parts, attr)
		n = n.Child("value")
	}
	if n.Is("Name") {
		id, _ := n.Str("id")
		parts = append(parts, id)
	}
	slices.Reverse(parts)
	return strings.Join(parts, ".")
}

func astCallName(call *pyast.Node) string { return astDotted(call.Child("func")) }

func astStrConstant(n *pyast.Node) (string, bool) {
	if !n.Is("Constant") {
		return "", false
	}
	s, ok := n.Get("value").(string)
	return s, ok
}

// Taint state of the core override pass, shared by every module of the plugin
type coreAnalysis struct {
	coreRoots     map[string]bool
	coreReturners map[string]bool
	paramPatchers map[string]map[int]bool
	aliases       map[string]string
	constants     map[string]string
}

func newCoreAnalysis(roots, local map[string]bool) *coreAnalysis {
	a := &coreAnalysis{coreRoots: map[string]bool{}, coreReturners: map[string]bool{}, paramPatchers: map[string]map[int]bool{},
		aliases: map[string]string{}, constants: map[string]string{}}
	for r := range roots {
		if !local[r] {
			a.coreRoots[r] = true
		}
	}
	return a
}

func (a *coreAnalysis) funcKey(call *pyast.Node) string {
	name := astCallName(call)
	name = name[strings.LastIndex(name, ".")+1:]
	if alias, ok := a.aliases[name]; ok {
		return alias
	}
	return name
}

func (a *coreAnalysis) coreModuleName(name any) bool {
	s, ok := name.(string)
	return ok && a.coreRoots[strings.Split(s, ".")[0]]
}

// Whether an expression evaluates to an Icebath module or a value reached through one
func (a *coreAnalysis) isCore(n *pyast.Node, tainted map[string]bool) bool {
	switch {
	case n.Is("Name"):
		id, _ := n.Str("id")
		return tainted[id]
	case n.Is("Attribute"):
		return a.isCore(n.Child("value"), tainted)
	case n.Is("Subscript"):
		if astDotted(n.Child("value")) == "sys.modules" {
			return a.lookupIsCore(n.Child("slice"))
		}
		return a.isCore(n.Child("value"), tainted)
	case n.Is("Call"):
		name := astCallName(n)
		args := n.Children("args")
		if (name == "importlib.import_module" || name == "import_module" || name == "__import__") && len(args) > 0 {
			return a.lookupIsCore(args[0])
		}
		if name == "sys.modules.get" && len(args) > 0 {
			return a.lookupIsCore(args[0])
		}
		if name == "getattr" && len(args) > 0 {
			return a.isCore(args[0], tainted)
		}
		return a.coreReturners[a.funcKey(n)]
	case n.Is("Tuple", "List", "Set"):
		for _, e := range n.Children("elts") {
			if a.isCore(e, tainted) {
				return true
			}
		}
	}
	return false
}

// Whether a module name expression names Icebath, through a literal, a module constant or an f-string head
func (a *coreAnalysis) lookupIsCore(arg *pyast.Node) bool {
	if arg.Is("Name") {
		id, _ := arg.Str("id")
		if v, ok := a.constants[id]; ok {
			return a.coreModuleName(v)
		}
		return false
	}
	if arg.Is("JoinedStr") {
		if values := arg.Children("values"); len(values) > 0 && values[0].Is("Constant") {
			head := pyrepr.Str(values[0].Get("value"))
			return strings.Contains(head, ".") && a.coreModuleName(head)
		}
	}
	return arg.Is("Constant") && a.coreModuleName(arg.Get("value"))
}

// Names bound to core values in one scope, iterated to a fixpoint of at most four rounds
func (a *coreAnalysis) scopeTaint(body []*pyast.Node, inherited map[string]bool) map[string]bool {
	tainted := map[string]bool{}
	for k := range inherited {
		tainted[k] = true
	}
	var nodes []*pyast.Node
	for _, stmt := range body {
		nodes = append(nodes, walkScope(stmt)...)
	}
	for range 4 {
		before := len(tainted)
		for _, node := range nodes {
			switch {
			case node.Is("Import"):
				for _, alias := range node.Children("names") {
					name, _ := alias.Str("name")
					if a.coreModuleName(name) {
						bound := name
						if as, ok := alias.Str("asname"); ok {
							bound = as
						}
						tainted[strings.Split(bound, ".")[0]] = true
					}
				}
			case node.Is("ImportFrom"):
				level, _ := node.Int("level")
				if level == 0 && a.coreModuleName(node.Get("module")) {
					for _, alias := range node.Children("names") {
						bound, ok := alias.Str("asname")
						if !ok {
							bound, _ = alias.Str("name")
						}
						tainted[bound] = true
					}
				}
			case node.Is("Assign", "AnnAssign"):
				if value := node.Child("value"); value != nil && a.isCore(value, tainted) {
					targets := node.Children("targets")
					if node.Is("AnnAssign") {
						targets = []*pyast.Node{node.Child("target")}
					}
					for name := range boundNames(targets) {
						tainted[name] = true
					}
				}
			case node.Is("For", "AsyncFor", "comprehension"):
				if a.isCore(node.Child("iter"), tainted) {
					for name := range boundNames([]*pyast.Node{node.Child("target")}) {
						tainted[name] = true
					}
				}
			}
		}
		if len(tainted) == before {
			break
		}
	}
	return tainted
}

// Rebind targets one node performs on core values, as the text the finding reports
func (a *coreAnalysis) rebinds(node *pyast.Node, tainted map[string]bool) []string {
	var hits []string
	var targets []*pyast.Node
	switch {
	case node.Is("Assign", "Delete"):
		targets = node.Children("targets")
	case node.Is("AugAssign", "AnnAssign"):
		targets = []*pyast.Node{node.Child("target")}
	}
	for _, target := range targets {
		items := []*pyast.Node{target}
		if target.Is("Tuple", "List") {
			items = target.Children("elts")
		}
		for _, t := range items {
			switch {
			case t.Is("Attribute") && a.isCore(t.Child("value"), tainted):
				hit := astDotted(t)
				if hit == "" {
					hit, _ = t.Str("attr")
				}
				hits = append(hits, hit)
			case t.Is("Subscript") && astDotted(t.Child("value")) == "sys.modules":
				if s := t.Child("slice"); s.Is("Constant") && a.coreModuleName(s.Get("value")) {
					hits = append(hits, "sys.modules["+pyrepr.Repr(s.Get("value"))+"]")
				}
			case t.Is("Subscript") && t.Child("value").Is("Attribute") && a.isCore(t.Child("value").Child("value"), tainted):
				hit := astDotted(t.Child("value"))
				if hit == "" {
					hit, _ = t.Child("value").Str("attr")
				}
				hits = append(hits, hit+"[...]")
			}
		}
	}
	if node.Is("Call") {
		name := astCallName(node)
		args := node.Children("args")
		dottedOr := func(n *pyast.Node) string {
			if d := astDotted(n); d != "" {
				return d
			}
			return "<core>"
		}
		switch {
		case (name == "setattr" || name == "delattr") && len(args) > 0 && a.isCore(args[0], tainted):
			hits = append(hits, name+"("+dottedOr(args[0])+", ...)")
		case strings.HasSuffix(name, "patch.object") && len(args) > 0 && a.isCore(args[0], tainted):
			hits = append(hits, "patch.object("+astDotted(args[0])+", ...)")
		case name[strings.LastIndex(name, ".")+1:] == "patch" && len(args) > 0 && args[0].Is("Constant") && a.coreModuleName(args[0].Get("value")):
			hits = append(hits, "patch("+pyrepr.Repr(args[0].Get("value"))+")")
		default:
			indexes := []int{}
			for i := range a.paramPatchers[a.funcKey(node)] {
				indexes = append(indexes, i)
			}
			sort.Ints(indexes)
			for _, i := range indexes {
				if i < len(args) && a.isCore(args[i], tainted) {
					hits = append(hits, name+"("+dottedOr(args[i])+", ...)")
				}
			}
		}
	}
	return hits
}

// Nodes of one scope in stack order, never entering nested function, class or lambda nodes
func walkScope(stmt *pyast.Node) []*pyast.Node {
	var out []*pyast.Node
	stack := []*pyast.Node{stmt}
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		out = append(out, node)
		for _, child := range pyast.IterChildNodes(node) {
			if !child.Is("FunctionDef", "AsyncFunctionDef", "ClassDef", "Lambda") {
				stack = append(stack, child)
			}
		}
	}
	return out
}

// Plain names an assignment target list binds, tuples and lists unpacked
func boundNames(targets []*pyast.Node) map[string]bool {
	names := map[string]bool{}
	for _, t := range targets {
		if t.Is("Name") {
			id, _ := t.Str("id")
			names[id] = true
		} else if t.Is("Tuple", "List") {
			for n := range boundNames(t.Children("elts")) {
				names[n] = true
			}
		}
	}
	return names
}

type coreFunction struct {
	node     *pyast.Node
	isMethod bool
}

// Every function of a module with whether it is called as a bound method, nested ones included
func coreFunctions(tree *pyast.Node) []coreFunction {
	var out []coreFunction
	var visit func(n *pyast.Node, inClass bool)
	visit = func(n *pyast.Node, inClass bool) {
		for _, child := range pyast.IterChildNodes(n) {
			switch {
			case child.Is("FunctionDef", "AsyncFunctionDef"):
				static := false
				for _, d := range child.Children("decorator_list") {
					static = static || astDotted(d) == "staticmethod"
				}
				out = append(out, coreFunction{child, inClass && !static})
				visit(child, false)
			case child.Is("ClassDef"):
				visit(child, true)
			default:
				visit(child, inClass)
			}
		}
	}
	visit(tree, false)
	return out
}

func coreFunctionName(f *pyast.Node) string {
	name, _ := f.Str("name")
	return name
}

// Findings formatted target, relative path and line for every runtime rebind of Icebath core
func coreOverrideFindings(pluginDir string) ([]string, error) {
	parsed, err := parsePluginSources(pluginDir, func(string) bool { return true })
	if err != nil {
		return nil, err
	}
	local, err := coreLocalNames(pluginDir)
	if err != nil {
		return nil, err
	}
	runtimeFiles := coreRuntimeFiles(parsed, local)
	var trees []parsedSource
	for _, f := range parsed {
		if runtimeFiles[f.rel] {
			trees = append(trees, f)
		}
	}
	a := newCoreAnalysis(coreTables().topLevel, local)
	for _, f := range trees {
		for _, node := range pyast.Walk(f.tree) {
			if !node.Is("ImportFrom") {
				continue
			}
			level, _ := node.Int("level")
			module, _ := node.Str("module")
			if level != 0 || local[strings.Split(module, ".")[0]] {
				for _, alias := range node.Children("names") {
					if as, ok := alias.Str("asname"); ok && as != "" {
						a.aliases[as], _ = alias.Str("name")
					}
				}
			}
		}
		for _, node := range f.tree.Children("body") {
			if s, ok := astStrConstant(node.Child("value")); node.Is("Assign") && ok {
				for n := range boundNames(node.Children("targets")) {
					a.constants[n] = s
				}
			}
		}
	}
	size := func() [2]int {
		total := 0
		for _, v := range a.paramPatchers {
			total += len(v)
		}
		return [2]int{len(a.coreReturners), total}
	}
	for range 4 {
		before := size()
		for _, f := range trees {
			moduleTaint := a.scopeTaint(f.tree.Children("body"), map[string]bool{})
			for _, fn := range coreFunctions(f.tree) {
				body := fn.node.Children("body")
				taint := a.scopeTaint(body, moduleTaint)
				for _, n := range walkBody(body) {
					if n.Is("Return") && n.Child("value") != nil && a.isCore(n.Child("value"), taint) {
						a.coreReturners[coreFunctionName(fn.node)] = true
						break
					}
				}
				args := fn.node.Child("args")
				params := append(append([]*pyast.Node{}, args.Children("posonlyargs")...), args.Children("args")...)
				offset := 0
				if fn.isMethod {
					offset = 1
				}
				for index := 0; index+offset < len(params); index++ {
					param, _ := params[index+offset].Str("arg")
					for _, n := range walkBody(body) {
						if len(a.rebinds(n, map[string]bool{param: true})) > 0 {
							name := coreFunctionName(fn.node)
							if a.paramPatchers[name] == nil {
								a.paramPatchers[name] = map[int]bool{}
							}
							a.paramPatchers[name][index] = true
							break
						}
					}
				}
			}
		}
		if size() == before {
			break
		}
	}
	type finding struct {
		rel    string
		line   int
		target string
	}
	found := map[finding]bool{}
	for _, f := range trees {
		moduleTaint := a.scopeTaint(f.tree.Children("body"), map[string]bool{})
		bodies := [][]*pyast.Node{f.tree.Children("body")}
		taints := []map[string]bool{moduleTaint}
		for _, fn := range coreFunctions(f.tree) {
			bodies = append(bodies, fn.node.Children("body"))
			taints = append(taints, a.scopeTaint(fn.node.Children("body"), moduleTaint))
		}
		for i, body := range bodies {
			for _, node := range walkBody(body) {
				for _, target := range a.rebinds(node, taints[i]) {
					found[finding{f.rel, node.Lineno, target}] = true
				}
			}
		}
	}
	list := make([]finding, 0, len(found))
	for k := range found {
		list = append(list, k)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].rel != list[j].rel {
			return list[i].rel < list[j].rel
		}
		if list[i].line != list[j].line {
			return list[i].line < list[j].line
		}
		return list[i].target < list[j].target
	})
	out := make([]string, len(list))
	for i, f := range list {
		out[i] = fmt.Sprintf("%s (%s:%d)", f.target, f.rel, f.line)
	}
	return out, nil
}

func walkBody(body []*pyast.Node) []*pyast.Node {
	var out []*pyast.Node
	for _, stmt := range body {
		out = append(out, walkScope(stmt)...)
	}
	return out
}

// Fails the report when the plugin's Python rebinds Icebath core at runtime
func checkCoreOverride(report *ValidationReport, pluginDir string) error {
	hits, err := coreOverrideFindings(pluginDir)
	if err != nil {
		return err
	}
	detail := "no runtime rebinds of Icebath core"
	if len(hits) > 0 {
		detail = "rebinds Icebath core at runtime (use a public hook, middleware or provider profile): " +
			strings.Join(hits[:min(8, len(hits))], "; ")
		if len(hits) > 8 {
			detail += fmt.Sprintf(" (+%d more)", len(hits)-8)
		}
	}
	report.add("no core override", len(hits) == 0, detail)
	return nil
}
