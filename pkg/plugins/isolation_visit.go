package plugins

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyast"
)

// Module and callable pairs that mutate an Icebath registry when plugin code calls them
var isolationDirectRegistryCalls = map[[2]string]bool{
	{"tools.registry", "register"}:            true,
	{"tools.registry", "deregister"}:          true,
	{"gateway.platform_registry", "register"}: true,
	{"providers", "register_provider"}:        true,
}

// Collects ctx method calls, Icebath module aliases and writes through those aliases
type isolationSourceVisitor struct {
	rel           string
	modelProvider bool
	aliases       map[string]string
	visit         *isolationVisit
}

// Parses one plugin source and audits it, nil visit plus the message when it is not valid Python
func isolationVisitSource(rel, source string, modelProvider bool) (*isolationVisit, string, error) {
	tree, err := pyast.Parse(source)
	if err != nil {
		var syn *pyast.SyntaxError
		if errors.As(err, &syn) {
			return nil, syn.Msg, nil
		}
		return nil, "", err
	}
	v := &isolationSourceVisitor{rel: rel, modelProvider: modelProvider, aliases: map[string]string{},
		visit: &isolationVisit{Reasons: []string{}, Notes: []string{}, CtxMethods: map[string]bool{}}}
	v.walk(tree)
	return v.visit, "", nil
}

func (v *isolationSourceVisitor) icebath(module string) bool {
	return IcebathPackages[strings.Split(module, ".")[0]]
}

// Dispatches on the node class like ast.NodeVisitor, visiting children in field order
func (v *isolationSourceVisitor) walk(n *pyast.Node) {
	switch n.Type {
	case "Import":
		for _, alias := range n.Children("names") {
			name, _ := alias.Str("name")
			if !v.icebath(name) {
				continue
			}
			top := strings.Split(name, ".")[0]
			if asname, ok := alias.Str("asname"); ok && asname != "" {
				v.aliases[asname] = name
			} else {
				v.aliases[top] = top
			}
		}
		return
	case "ImportFrom":
		module, _ := n.Str("module")
		if level, _ := n.Int("level"); level == 0 && module != "" && v.icebath(module) {
			for _, alias := range n.Children("names") {
				name, _ := alias.Str("name")
				key := name
				if asname, ok := alias.Str("asname"); ok && asname != "" {
					key = asname
				}
				v.aliases[key] = module + "." + name
			}
		}
		return
	case "Assign":
		for _, target := range n.Children("targets") {
			v.flagWrite(target, n.Lineno)
		}
	case "AugAssign":
		v.flagWrite(n.Child("target"), n.Lineno)
	case "Call":
		v.call(n)
	case "ClassDef":
		if v.modelProvider {
			for _, item := range n.Children("body") {
				if fname, _ := item.Str("name"); item.Is("FunctionDef", "AsyncFunctionDef") && fname == "create_client" {
					cname, _ := n.Str("name")
					v.visit.Reasons = append(v.visit.Reasons, fmt.Sprintf("%s:%d %s.create_client() returns a live SDK "+
						"client (model-provider profiles cross the host as data + methods)", v.rel, n.Lineno, cname))
					break
				}
			}
		}
	}
	for _, c := range pyast.IterChildNodes(n) {
		v.walk(c)
	}
}

// Dotted Icebath path a Name or Attribute chain resolves to through the import aliases
func (v *isolationSourceVisitor) resolve(n *pyast.Node) (string, bool) {
	switch {
	case n.Is("Name"):
		id, _ := n.Str("id")
		target, ok := v.aliases[id]
		return target, ok && target != ""
	case n.Is("Attribute"):
		base, ok := v.resolve(n.Child("value"))
		if !ok {
			return "", false
		}
		attr, _ := n.Str("attr")
		return base + "." + attr, true
	}
	return "", false
}

func (v *isolationSourceVisitor) flagWrite(target *pyast.Node, line int) {
	if !target.Is("Attribute") {
		return
	}
	if owner, ok := v.resolve(target.Child("value")); ok {
		attr, _ := target.Str("attr")
		v.visit.Reasons = append(v.visit.Reasons, fmt.Sprintf("%s:%d patches %s.%s (an Icebath module "+
			"attribute; the host's copy is not the one Icebath runs)", v.rel, line, owner, attr))
	}
}

// Records setattr patches, unsupported and skipped ctx methods, degraded hooks and direct registry calls
func (v *isolationSourceVisitor) call(n *pyast.Node) {
	fn := n.Child("func")
	args := n.Children("args")
	if id, _ := fn.Str("id"); fn.Is("Name") && id == "setattr" && len(args) > 0 {
		if owner, ok := v.resolve(args[0]); ok {
			v.visit.Reasons = append(v.visit.Reasons, fmt.Sprintf("%s:%d setattr() on %s (patches an Icebath module)", v.rel, n.Lineno, owner))
		}
	}
	switch {
	case fn.Is("Attribute"):
		name, _ := fn.Str("attr")
		if why, ok := HostUnsupportedCtxMethods[name]; ok {
			v.visit.Reasons = append(v.visit.Reasons, fmt.Sprintf("%s:%d ctx.%s(): %s", v.rel, n.Lineno, name, why))
		} else if why, ok := HostSkippedCtxMethods[name]; ok {
			v.visit.Notes = append(v.visit.Notes, fmt.Sprintf("ctx.%s() is skipped in the host (%s)", name, why))
		}
		switch name {
		case "subscribe", "dispatch_tool", "inject_message", "emit":
			v.visit.CtxMethods[name] = true
		default:
			if strings.HasPrefix(name, "register_") {
				v.visit.CtxMethods[name] = true
			}
		}
		if name == "register_hook" && len(args) > 0 && args[0].Is("Constant") {
			if hook, ok := args[0].Get("value").(string); ok {
				if why, degraded := HostDegradedHooks[hook]; degraded {
					v.visit.Notes = append(v.visit.Notes, fmt.Sprintf("hook %s %s; those fields are placeholders in the host", hook, why))
				}
			}
		}
		target, ok := v.resolve(fn)
		v.flagRegistryCall(target, ok, n.Lineno)
	case fn.Is("Name"):
		id, _ := fn.Str("id")
		target, ok := v.aliases[id]
		v.flagRegistryCall(target, ok && target != "", n.Lineno)
	}
}

func (v *isolationSourceVisitor) flagRegistryCall(target string, ok bool, line int) {
	if !ok {
		return
	}
	module, attr := "", target
	if i := strings.LastIndex(target, "."); i >= 0 {
		module, attr = target[:i], target[i+1:]
	}
	if v.modelProvider && module == "providers" && attr == "register_provider" {
		return
	}
	if isolationDirectRegistryCalls[[2]string{module, attr}] ||
		(strings.HasSuffix(module, "_registry") && (attr == "register" || attr == "register_provider")) {
		v.visit.Reasons = append(v.visit.Reasons, fmt.Sprintf("%s:%d calls %s() directly instead of "+
			"through ctx (registers in the host, not in Icebath)", v.rel, line, target))
	}
}
