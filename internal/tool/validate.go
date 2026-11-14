package tool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

var schemaLiteralKeys = map[string]bool{"const": true, "default": true, "enum": true, "example": true, "examples": true}

func localValidationSchema(n *jnode) *jnode {
	switch {
	case n.isArr():
		out := newArr()
		for _, item := range n.arr {
			out.arr = append(out.arr, localValidationSchema(item))
		}
		return out
	case !n.isObj():
		return n
	}
	normalized := newObj()
	for i, k := range n.keys {
		if k == "nullable" {
			continue
		}
		if schemaLiteralKeys[k] {
			normalized.set(k, n.vals[i].clone())
		} else {
			normalized.set(k, localValidationSchema(n.vals[i]))
		}
	}
	if nb := n.get("nullable"); nb == nil || nb.kind != jBool || !nb.b {
		return normalized
	}
	t := normalized.get("type")
	if t.isStr() {
		t = newArr(t)
	}
	if t.isArr() {
		hasNull := false
		for _, item := range t.arr {
			if item.strEq("null") {
				hasNull = true
			}
		}
		if !hasNull {
			normalized.set("type", newArr(append(slices.Clone(t.arr), newStr("null"))...))
		}
		return normalized
	}
	return newObj().with("anyOf", newArr(normalized, newObj().with("type", newStr("null"))))
}

func hasExternalRef(n *jnode) bool {
	switch {
	case n.isArr():
		for _, item := range n.arr {
			if hasExternalRef(item) {
				return true
			}
		}
		return false
	case !n.isObj():
		return false
	}
	if ref, ok := n.get("$ref").text(); ok && !strings.HasPrefix(ref, "#") {
		return true
	}
	for i, k := range n.keys {
		if !schemaLiteralKeys[k] && hasExternalRef(n.vals[i]) {
			return true
		}
	}
	return false
}

func validationFailure(message, path, constraint string, params *jnode) Result {
	return Failure(message,
		Field{Key: "path", Value: path},
		Field{Key: "constraint", Value: constraint},
		Field{Key: "parameters", Value: json.RawMessage(params.compact())},
		Field{Key: "hint", Value: "Retry tool_call with 'arguments' matching the parameters schema above."})
}

func (d *Dispatcher) validateDeferred(name string, at Scope, args Args) *Result {
	e, ok := d.r.Lookup(name, at)
	if !ok {
		return nil
	}
	params, err := parseNode(e.Tool.Parameters)
	if err != nil || !params.isObj() {
		return nil
	}
	if req := params.get("required"); req.isArr() {
		var missing []string
		for _, r := range req.arr {
			if s, ok := r.text(); ok {
				if _, present := args[s]; !present {
					missing = append(missing, s)
				}
			}
		}
		if len(missing) > 0 {
			f := validationFailure(fmt.Sprintf("tool_call to '%s' is missing required argument(s): %s. The tool was NOT invoked.", name, strings.Join(missing, ", ")),
				"arguments", "required", params)
			return &f
		}
	}
	vs := localValidationSchema(params)
	if hasExternalRef(vs) {
		slog.Debug("Skipping local deferred-argument validation for " + name + ": external $ref")
		return nil
	}
	candidate := coerceArgs(name, params, args.Clone())
	instance, err := plainJSON(candidate)
	if err != nil {
		slog.Debug(fmt.Sprintf("Deferred-argument encoding failed for %s: %v", name, err))
		return nil
	}
	compiler := jsonschema.NewCompiler()
	compiler.LoadURL = func(s string) (io.ReadCloser, error) {
		return nil, fmt.Errorf("schema %s is not loaded during local validation", s)
	}
	const url = "file:///tool-call-arguments.json"
	if err := compiler.AddResource(url, bytes.NewReader(vs.compact())); err != nil {
		slog.Debug(fmt.Sprintf("validate_deferred_call_args failed for %s: %v", name, err))
		return nil
	}
	schema, err := compiler.Compile(url)
	if err != nil {
		slog.Debug(fmt.Sprintf("validate_deferred_call_args failed for %s: %v", name, err))
		return nil
	}
	err = schema.Validate(instance)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		slog.Debug(fmt.Sprintf("validate_deferred_call_args failed for %s: %v", name, err))
		return nil
	}
	m := errorMapper{root: vs, instance: instance}
	best := bestMatch(m.flatten(ve, nil))
	if best == nil {
		return nil
	}
	path := best.pathText()
	constraint := best.keyword
	if constraint == "" {
		constraint = "schema"
	}
	detail := strings.Join(wideFields(best.message), " ")
	if utf8.RuneCountInString(detail) > 600 {
		detail = runePrefix(detail, 597) + "..."
	}
	f := validationFailure(fmt.Sprintf("tool_call to '%s' failed argument validation at %s (%s): %s. The tool was NOT invoked.", name, path, constraint, detail),
		path, constraint, params)
	return &f
}

func plainJSON(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return decodeLoose(string(raw))
}

type pathSeg struct {
	key   string
	index int
	isIdx bool
}

type pyError struct {
	keyword  string
	path     []pathSeg
	base     int
	instance any
	schema   *jnode
	message  string
	context  []*pyError
	order    []int
}

var identifierPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (e *pyError) pathText() string {
	var b strings.Builder
	b.WriteString("arguments")
	for _, s := range e.path {
		switch {
		case s.isIdx:
			fmt.Fprintf(&b, "[%d]", s.index)
		case identifierPath.MatchString(s.key):
			b.WriteString("." + s.key)
		default:
			b.WriteString("[" + string(nodeOf(s.key).compact()) + "]")
		}
	}
	return b.String()
}

type relevance struct {
	depth      int
	notWeak    bool
	typeMisfit bool
}

func (e *pyError) relevance() relevance {
	return relevance{depth: -(len(e.path) - e.base), notWeak: e.keyword != "anyOf" && e.keyword != "oneOf", typeMisfit: !e.matchesType()}
}

func (a relevance) less(b relevance) bool {
	if a.depth != b.depth {
		return a.depth < b.depth
	}
	if a.notWeak != b.notWeak {
		return !a.notWeak
	}
	return !a.typeMisfit && b.typeMisfit
}

func bestMatch(errs []*pyError) *pyError {
	if len(errs) == 0 {
		return nil
	}
	best := errs[0]
	for _, e := range errs[1:] {
		if best.relevance().less(e.relevance()) {
			best = e
		}
	}
	for len(best.context) > 0 {
		ctx := slices.Clone(best.context)
		sort.SliceStable(ctx, func(i, j int) bool { return ctx[i].relevance().less(ctx[j].relevance()) })
		if len(ctx) >= 2 && ctx[0].relevance() == ctx[1].relevance() {
			return best
		}
		best = ctx[0]
	}
	return best
}

func instanceIs(v any, kind string) bool {
	switch kind {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		r, ok := new(big.Rat).SetString(string(n))
		return ok && r.IsInt()
	}
	return false
}

func (e *pyError) matchesType() bool {
	t := e.schema.get("type")
	if s, ok := t.text(); ok {
		return instanceIs(e.instance, s)
	}
	if t.isArr() {
		for _, item := range t.arr {
			if s, ok := item.text(); ok && instanceIs(e.instance, s) {
				return true
			}
		}
	}
	return false
}

type errorMapper struct {
	root     *jnode
	instance any
}

func pointerTokens(p string) []string {
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[i+1:]
	}
	if p == "" || p == "/" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return parts
}

var (
	nameMapKeywords   = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true, "dependencies": true}
	indexKeywords     = map[string]bool{"anyOf": true, "oneOf": true, "allOf": true, "prefixItems": true}
	referenceKeywords = map[string]bool{"$ref": true, "$dynamicRef": true, "$recursiveRef": true}
)

type schemaWalk struct {
	keyword string
	parent  *jnode
	value   *jnode
	order   []int
}

func (m errorMapper) refTarget(node *jnode, key string) *jnode {
	ref, ok := node.get(key).text()
	if !ok || !strings.HasPrefix(ref, "#") {
		return nil
	}
	target := m.root
	for _, tok := range pointerTokens(percentDecode(ref)) {
		switch {
		case target.isObj():
			target = target.get(tok)
		case target.isArr():
			i, ok := pointerIndex(tok)
			if !ok || i >= len(target.arr) {
				return nil
			}
			target = target.arr[i]
		default:
			return nil
		}
		if target == nil {
			return nil
		}
	}
	return target
}

func (m errorMapper) walk(location string) schemaWalk {
	tokens := pointerTokens(location)
	node := m.root
	var w schemaWalk
	state := "schema"
	for _, tok := range tokens {
		parent := node
		var next *jnode
		pos := -1
		if node.isObj() {
			pos = node.index(tok)
			next = node.get(tok)
		} else if node.isArr() {
			if i, ok := pointerIndex(tok); ok && i < len(node.arr) {
				pos, next = i, node.arr[i]
			}
		}
		w.order = append(w.order, pos)
		switch state {
		case "schema":
			w.keyword, w.parent = tok, parent
			switch {
			case nameMapKeywords[tok]:
				state = "name"
			case indexKeywords[tok]:
				state = "index"
			case referenceKeywords[tok]:
				next = m.refTarget(parent, tok)
			case tok == "items" && next.isArr():
				state = "index"
			}
		default:
			w.keyword = ""
			state = "schema"
		}
		node = next
		w.value = next
	}
	if state != "schema" {
		w.keyword = ""
	}
	if w.keyword == "" {
		w.parent = node
	}
	return w
}

func (m errorMapper) instanceAt(location string) ([]pathSeg, any) {
	var path []pathSeg
	cur := m.instance
	for _, tok := range pointerTokens(location) {
		switch t := cur.(type) {
		case []any:
			i, _ := strconv.Atoi(tok)
			path = append(path, pathSeg{index: i, isIdx: true})
			if i >= 0 && i < len(t) {
				cur = t[i]
			} else {
				cur = nil
			}
		case map[string]any:
			path = append(path, pathSeg{key: tok})
			cur = t[tok]
		default:
			path = append(path, pathSeg{key: tok})
			cur = nil
		}
	}
	return path, cur
}

func show(v any) string {
	return nodeOf(v).spaced()
}

func showNode(n *jnode) string {
	if n == nil {
		return "null"
	}
	return n.spaced()
}

func (m errorMapper) flatten(e *jsonschema.ValidationError, parent *pyError) []*pyError {
	w := m.walk(e.KeywordLocation)
	if len(e.Causes) == 0 {
		return m.leaves(e, w, parent)
	}
	if w.keyword == "anyOf" || w.keyword == "oneOf" {
		pe := m.newError(e, w, parent)
		pe.message = show(pe.instance) + " is not valid under any of the given schemas"
		for _, c := range e.Causes {
			pe.context = append(pe.context, m.flatten(c, pe)...)
		}
		return []*pyError{pe}
	}
	var out []*pyError
	for _, c := range e.Causes {
		out = append(out, m.flatten(c, parent)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return orderLess(out[i], out[j]) })
	return out
}

func orderLess(a, b *pyError) bool {
	if c := slices.Compare(a.order, b.order); c != 0 {
		return c < 0
	}
	for i := 0; i < len(a.path) && i < len(b.path); i++ {
		x, y := a.path[i], b.path[i]
		if x.isIdx && y.isIdx && x.index != y.index {
			return x.index < y.index
		}
	}
	return false
}

func (m errorMapper) newError(e *jsonschema.ValidationError, w schemaWalk, parent *pyError) *pyError {
	path, inst := m.instanceAt(e.InstanceLocation)
	base := 0
	if parent != nil {
		base = len(parent.path)
	}
	return &pyError{keyword: w.keyword, path: path, base: base, instance: inst, schema: w.parent, order: w.order}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func joinShown(items []string) string {
	shown := make([]string, len(items))
	for i, s := range items {
		shown[i] = show(s)
	}
	return strings.Join(shown, ", ")
}

func intOfNode(n *jnode) int {
	if n == nil || n.kind != jNum {
		return -1
	}
	i, err := strconv.Atoi(n.num)
	if err != nil {
		return -1
	}
	return i
}

func (m errorMapper) extras(obj map[string]any, schema *jnode) []string {
	props := schema.get("properties")
	patterns := schema.get("patternProperties")
	var out []string
	for k := range obj {
		if props.has(k) {
			continue
		}
		matched := false
		if patterns.isObj() {
			for _, p := range patterns.keys {
				re, err := regexp2.Compile(p, regexp2.None)
				if err != nil {
					continue
				}
				re.MatchTimeout = time.Second
				if ok, err := re.MatchString(k); err == nil && ok {
					matched = true
					break
				}
			}
		}
		if !matched {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

var messageIndexes = regexp.MustCompile(`\d+`)

func (m errorMapper) leaves(e *jsonschema.ValidationError, w schemaWalk, parent *pyError) []*pyError {
	pe := m.newError(e, w, parent)
	inst := show(pe.instance)
	kw := w.value
	switch w.keyword {
	case "":
		if e.KeywordLocation == "" {
			pe.message = e.Message
		} else {
			pe.message = "False schema does not allow " + inst
		}
	case "type":
		var types []string
		if s, ok := kw.text(); ok {
			types = []string{s}
		} else if kw.isArr() {
			for _, t := range kw.arr {
				if s, ok := t.text(); ok {
					types = append(types, s)
				}
			}
		}
		pe.message = inst + " is not of type " + joinShown(types)
	case "required":
		obj, _ := pe.instance.(map[string]any)
		var out []*pyError
		if kw.isArr() {
			for _, r := range kw.arr {
				s, ok := r.text()
				if !ok {
					continue
				}
				if _, present := obj[s]; present {
					continue
				}
				c := *pe
				c.message = show(s) + " is a required property"
				out = append(out, &c)
			}
		}
		if len(out) > 0 {
			return out
		}
		pe.message = e.Message
	case "enum":
		pe.message = inst + " is not one of " + showNode(kw)
	case "const":
		pe.message = showNode(kw) + " was expected"
	case "minimum":
		pe.message = inst + " is less than the minimum of " + showNode(kw)
	case "maximum":
		pe.message = inst + " is greater than the maximum of " + showNode(kw)
	case "exclusiveMinimum":
		pe.message = inst + " is less than or equal to the minimum of " + showNode(kw)
	case "exclusiveMaximum":
		pe.message = inst + " is greater than or equal to the maximum of " + showNode(kw)
	case "multipleOf":
		pe.message = inst + " is not a multiple of " + showNode(kw)
	case "minLength", "minItems":
		pe.message = inst + plural(intOfNode(kw), " should be non-empty", " is too short")
	case "maxLength", "maxItems":
		if intOfNode(kw) == 0 {
			pe.message = inst + " is expected to be empty"
		} else {
			pe.message = inst + " is too long"
		}
	case "minProperties":
		pe.message = inst + plural(intOfNode(kw), " should be non-empty", " does not have enough properties")
	case "maxProperties":
		if intOfNode(kw) == 0 {
			pe.message = inst + " is expected to be empty"
		} else {
			pe.message = inst + " has too many properties"
		}
	case "uniqueItems":
		pe.message = inst + " has non-unique elements"
	case "pattern":
		pe.message = inst + " does not match " + showNode(kw)
	case "format":
		pe.message = inst + " is not a " + showNode(kw)
	case "additionalProperties", "unevaluatedProperties":
		obj, _ := pe.instance.(map[string]any)
		extras := m.extras(obj, w.parent)
		patterns := w.parent.get("patternProperties")
		switch {
		case w.keyword == "additionalProperties" && patterns.isObj() && len(patterns.keys) > 0:
			keys := slices.Clone(patterns.keys)
			sort.Strings(keys)
			pe.message = fmt.Sprintf("%s %s not match any of the regexes: %s", joinShown(extras), plural(len(extras), "does", "do"), joinShown(keys))
		case w.keyword == "additionalProperties":
			pe.message = fmt.Sprintf("Additional properties are not allowed (%s %s unexpected)", joinShown(extras), plural(len(extras), "was", "were"))
		default:
			pe.message = fmt.Sprintf("Unevaluated properties are not allowed (%s %s unexpected)", joinShown(extras), plural(len(extras), "was", "were"))
		}
	case "items", "additionalItems", "unevaluatedItems":
		list, _ := pe.instance.([]any)
		prefix := 0
		if p := w.parent.get("prefixItems"); p.isArr() {
			prefix = len(p.arr)
		} else if p := w.parent.get("items"); p.isArr() {
			prefix = len(p.arr)
		}
		var rest []any
		if prefix < len(list) {
			rest = list[prefix:]
		}
		shown := make([]string, len(rest))
		for i, r := range rest {
			shown[i] = show(r)
		}
		switch w.keyword {
		case "items":
			pe.message = fmt.Sprintf("Expected at most %d item%s but found %d extra: %s", prefix, plural(prefix, "", "s"), len(rest), show(rest))
		case "additionalItems":
			pe.message = fmt.Sprintf("Additional items are not allowed (%s %s unexpected)", strings.Join(shown, ", "), plural(len(rest), "was", "were"))
		default:
			pe.message = fmt.Sprintf("Unevaluated items are not allowed (%s %s unexpected)", strings.Join(shown, ", "), plural(len(rest), "was", "were"))
		}
	case "dependentRequired", "dependencies":
		obj, _ := pe.instance.(map[string]any)
		var out []*pyError
		for i, prop := range kw.keys {
			if _, present := obj[prop]; !present || !kw.vals[i].isArr() {
				continue
			}
			for _, dep := range kw.vals[i].arr {
				s, ok := dep.text()
				if !ok {
					continue
				}
				if _, present := obj[s]; present {
					continue
				}
				c := *pe
				c.message = show(s) + " is a dependency of " + show(prop)
				out = append(out, &c)
			}
		}
		if len(out) > 0 {
			return out
		}
		pe.message = e.Message
	case "anyOf":
		pe.message = inst + " is not valid under any of the given schemas"
	case "oneOf":
		idx := messageIndexes.FindAllString(e.Message, -1)
		var shown []string
		for _, s := range idx {
			if i, err := strconv.Atoi(s); err == nil && kw.isArr() && i < len(kw.arr) {
				shown = append(shown, showNode(kw.arr[i]))
			}
		}
		if len(shown) == 0 {
			pe.message = inst + " is not valid under any of the given schemas"
		} else {
			pe.message = inst + " is valid under each of " + strings.Join(shown, ", ")
		}
	case "not":
		pe.message = inst + " should not be valid under " + showNode(kw)
	case "contains":
		pe.message = inst + " does not contain items matching the given schema"
	case "minContains":
		pe.message = fmt.Sprintf("Too few items match the given schema (expected at least %s)", showNode(kw))
	case "maxContains":
		pe.message = fmt.Sprintf("Too many items match the given schema (expected at most %s)", showNode(kw))
	default:
		pe.message = e.Message
	}
	return []*pyError{pe}
}
