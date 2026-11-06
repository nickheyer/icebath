package pyjsonschema

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

const (
	schemaURL     = "icebath://schema.json"
	latestMetaURL = "https://json-schema.org/draft/2020-12/schema"
)

//go:embed metaschemas.json
var metaschemaText string

var metaschemas = sync.OnceValues(func() (map[string]any, error) {
	parsed, err := pyjson.Loads(metaschemaText)
	if err != nil {
		return nil, err
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return nil, errors.New("metaschema bundle is not an object")
	}
	return obj.Vals, nil
})

// One error of an iter_errors pass with its Python message, instance path and schema path
type IterError struct {
	Message      string
	Keyword      string
	AbsolutePath []any
	SchemaPath   []any
	order        []int
	parent       any
	instance     any
}

// Schema violation check_schema raises, rendered the way str(SchemaError) renders it
type SchemaError struct {
	Message    string
	Keyword    string
	SchemaPath []any
	Subschema  any
	Path       []any
	Instance   any
}

func (e *SchemaError) Error() string {
	sp := e.SchemaPath
	if len(sp) > 0 {
		sp = sp[:len(sp)-1]
	}
	return e.Message + "\n\nFailed validating " + pyrepr.Repr(e.Keyword) + " in " + formatAsIndex("metaschema", sp) +
		":\n    " + indentPretty(e.Subschema) + "\n\nOn " + formatAsIndex("schema", e.Path) + ":\n    " + indentPretty(e.Instance)
}

// Every error of instance against schema in Python iter_errors order
func IterErrors(instance, schema any) ([]*IterError, error) {
	compiled, err := compile(schema)
	if err != nil {
		return nil, err
	}
	plain, err := decodePlain(instance)
	if err != nil {
		return nil, err
	}
	verr := compiled.Validate(plain)
	if verr == nil {
		return nil, nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return nil, verr
	}
	return collect(ve, instance, newWalker(map[string]any{schemaURL: schema}, schemaURL)), nil
}

// Raises the first metaschema error of schema under the validator its $schema selects
func CheckSchema(schema any) error {
	docs, err := metaschemas()
	if err != nil {
		return err
	}
	metaURL := validatorFor(schema, docs)
	c := jsonschema.NewCompiler()
	c.AssertFormat = true
	c.LoadURL = func(u string) (io.ReadCloser, error) { return nil, fmt.Errorf("remote schema load refused: %s", u) }
	for name := range jsonschema.Formats {
		c.Formats[name] = func(any) bool { return true }
	}
	c.Formats["regex"] = func(v any) bool {
		s, ok := v.(string)
		if !ok {
			return true
		}
		_, err := compilePattern(s)
		return err == nil
	}
	for u, doc := range docs {
		text, err := pyjson.Dumps(doc)
		if err != nil {
			return err
		}
		if err := c.AddResource(u, strings.NewReader(text)); err != nil {
			return err
		}
	}
	meta, err := c.Compile(metaURL)
	if err != nil {
		return err
	}
	plain, err := decodePlain(schema)
	if err != nil {
		return err
	}
	verr := meta.Validate(plain)
	if verr == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return verr
	}
	first := collect(ve, schema, newWalker(docs, metaURL))[0]
	return &SchemaError{Message: first.Message, Keyword: first.Keyword, SchemaPath: first.SchemaPath, Subschema: first.parent, Path: first.AbsolutePath, Instance: first.instance}
}

// Metaschema URL validator_for picks, the latest draft when $schema is absent or unknown
func validatorFor(schema any, docs map[string]any) string {
	declared, ok := at(schema, []string{"$schema"}).(string)
	if !ok {
		return latestMetaURL
	}
	if u, err := url.Parse(declared); err == nil && u.Fragment == "" {
		declared = strings.TrimSuffix(declared, "#")
	}
	if _, known := docs[declared]; known && strings.HasSuffix(declared, "/schema") {
		return declared
	}
	return latestMetaURL
}

// Round trips a decoded value into the plain form the compiled schema validates
func decodePlain(instance any) (any, error) {
	text, err := pyjson.Dumps(instance)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var plain any
	if err := dec.Decode(&plain); err != nil {
		return nil, err
	}
	return plain, nil
}

// Top level errors iter_errors yields, combinators and contains kept whole
func iterLeaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	kw := keyword(e)
	if len(e.Causes) == 0 || weakKeywords[kw] || kw == "contains" || kw == "minContains" {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, iterLeaves(c)...)
	}
	return out
}

// Python errors of a validation tree ordered as a depth first walk over schema key order yields them
func collect(root *jsonschema.ValidationError, instance any, w *walker) []*IterError {
	var out []*IterError
	for _, e := range iterLeaves(root) {
		parent, value, order, spath := w.walk(e.KeywordLocation)
		if keyword(e) == "required" {
			loc := pointer(e.InstanceLocation)
			target := at(instance, loc)
			for i, name := range asList(value) {
				if s, ok := name.(string); ok && !hasKey(target, s) {
					out = append(out, &IterError{Message: pyrepr.Repr(s) + " is a required property", Keyword: "required",
						AbsolutePath: typedPath(instance, loc), SchemaPath: spath, order: append(slices.Clone(order), i), parent: parent, instance: target})
				}
			}
			continue
		}
		msg, path := describe(e, instance, w)
		out = append(out, &IterError{Message: msg, Keyword: keyword(e), AbsolutePath: typedPath(instance, path), SchemaPath: spath,
			order: order, parent: parent, instance: at(instance, path)})
	}
	sort.SliceStable(out, func(i, j int) bool { return slices.Compare(out[i].order, out[j].order) < 0 })
	return out
}

// Python message and instance path of one santhosh error
func describe(e *jsonschema.ValidationError, instance any, w *walker) (string, []string) {
	path := pointer(e.InstanceLocation)
	parent, value, _, _ := w.walk(e.KeywordLocation)
	msg := message(e, at(instance, path), value, parent)
	if kw := keyword(e); (kw == "items" || kw == "prefixItems" || kw == "unevaluatedItems") && len(path) > 0 {
		if _, isList := at(instance, path).([]any); !isList {
			path = path[:len(path)-1]
			msg = message(e, at(instance, path), value, parent)
		}
	}
	return msg, path
}

type walker struct {
	docs map[string]any
	root string
}

func newWalker(docs map[string]any, root string) *walker { return &walker{docs: docs, root: root} }

var nameMaps = map[string]bool{"properties": true, "patternProperties": true, "$defs": true, "definitions": true, "dependentSchemas": true, "dependencies": true}

// Follows a keyword location through references, giving the keyword holder, its value, key positions and the Python schema path
func (w *walker) walk(loc string) (parent, value any, order []int, schemaPath []any) {
	node, base := w.docs[w.root], w.root
	segs := pointer(loc)
	isName := false
	for i, seg := range segs {
		if !isName {
			base = rebase(node, base)
		}
		parent = node
		order = append(order, position(node, seg))
		keywordRef := !isName && (seg == "$ref" || seg == "$dynamicRef" || seg == "$recursiveRef")
		if seg != "$ref" || isName {
			if _, isList := node.([]any); isList {
				n, _ := strconv.Atoi(seg)
				schemaPath = append(schemaPath, n)
			} else {
				schemaPath = append(schemaPath, seg)
			}
		}
		next := at(node, []string{seg})
		if keywordRef && i < len(segs)-1 {
			if ref, ok := next.(string); ok && seg == "$ref" {
				node, base = w.resolve(base, ref)
			} else {
				node, base = w.docs[w.root], w.root
			}
			isName = false
			continue
		}
		_, isList := node.([]any)
		isName = !isName && !isList && nameMaps[seg]
		node = next
	}
	return parent, node, order, schemaPath
}

// Base URI after a schema node declares its own id
func rebase(node any, base string) string {
	id, ok := at(node, []string{"$id"}).(string)
	if !ok {
		if id, ok = at(node, []string{"id"}).(string); !ok {
			return base
		}
	}
	b, err := url.Parse(base)
	if err != nil {
		return base
	}
	r, err := b.Parse(id)
	if err != nil {
		return base
	}
	r.Fragment = ""
	return r.String()
}

// Schema node a reference names, by pointer fragment or anchor
func (w *walker) resolve(base, ref string) (any, string) {
	b, err := url.Parse(base)
	if err != nil {
		return nil, base
	}
	r, err := b.Parse(ref)
	if err != nil {
		return nil, base
	}
	frag := r.Fragment
	r.Fragment = ""
	docURL := r.String()
	doc := w.docs[docURL]
	switch {
	case frag == "":
		return doc, docURL
	case strings.HasPrefix(frag, "/"):
		return at(doc, pointer(frag)), docURL
	}
	return findAnchor(doc, frag), docURL
}

// Subschema declaring the named $anchor or $dynamicAnchor
func findAnchor(node any, name string) any {
	switch v := node.(type) {
	case *pyjson.Object:
		if v.Vals["$anchor"] == name || v.Vals["$dynamicAnchor"] == name {
			return v
		}
		for _, k := range v.Keys {
			if found := findAnchor(v.Vals[k], name); found != nil {
				return found
			}
		}
	case []any:
		for _, item := range v {
			if found := findAnchor(item, name); found != nil {
				return found
			}
		}
	}
	return nil
}

// Key position of a segment inside its schema node
func position(node any, seg string) int {
	switch v := node.(type) {
	case *pyjson.Object:
		return slices.Index(v.Keys, seg)
	case []any:
		n, _ := strconv.Atoi(seg)
		return n
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return slices.Index(keys, seg)
	}
	return 0
}

// Instance path with list positions as ints the way absolute_path holds them
func typedPath(doc any, path []string) []any {
	out := make([]any, 0, len(path))
	for _, seg := range path {
		if _, isList := doc.([]any); isList {
			n, _ := strconv.Atoi(seg)
			out = append(out, n)
		} else {
			out = append(out, seg)
		}
		doc = at(doc, []string{seg})
	}
	return out
}

func formatAsIndex(container string, indices []any) string {
	if len(indices) == 0 {
		return container
	}
	parts := make([]string, len(indices))
	for i, idx := range indices {
		parts[i] = pyrepr.Repr(idx)
	}
	return container + "[" + strings.Join(parts, "][") + "]"
}

func indentPretty(v any) string { return strings.ReplaceAll(Pformat(v), "\n", "\n    ") }

// Text pprint.pformat gives a value at width 72 with dict insertion order kept
func Pformat(v any) string {
	var b bytes.Buffer
	pformatValue(&b, v, 0, 0, 0)
	return b.String()
}

func pformatValue(b *bytes.Buffer, v any, indent, allowance, level int) {
	rep := pyrepr.Repr(v)
	if utf8.RuneCountInString(rep) > 72-indent-allowance {
		switch x := v.(type) {
		case *pyjson.Object:
			pformatDict(b, x.Keys, func(k string) any { return x.Vals[k] }, indent, allowance, level+1)
			return
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			pformatDict(b, keys, func(k string) any { return x[k] }, indent, allowance, level+1)
			return
		case []any:
			pformatList(b, x, indent, allowance, level+1)
			return
		case string:
			pformatStr(b, x, indent, allowance, level+1)
			return
		}
	}
	b.WriteString(rep)
}

func pformatDict(b *bytes.Buffer, keys []string, val func(string) any, indent, allowance, level int) {
	b.WriteByte('{')
	allowance++
	indent++
	for i, k := range keys {
		last := i == len(keys)-1
		rep := pyrepr.Repr(k)
		b.WriteString(rep + ": ")
		a := 1
		if last {
			a = allowance
		}
		pformatValue(b, val(k), indent+utf8.RuneCountInString(rep)+2, a, level)
		if !last {
			b.WriteString(",\n" + strings.Repeat(" ", indent))
		}
	}
	b.WriteByte('}')
}

func pformatList(b *bytes.Buffer, items []any, indent, allowance, level int) {
	b.WriteByte('[')
	allowance++
	indent++
	for i, item := range items {
		last := i == len(items)-1
		if i > 0 {
			b.WriteString(",\n" + strings.Repeat(" ", indent))
		}
		a := 1
		if last {
			a = allowance
		}
		pformatValue(b, item, indent, a, level)
	}
	b.WriteByte(']')
}

// Long string split into word chunks, parenthesized at the top level the way pprint wraps it
func pformatStr(b *bytes.Buffer, s string, indent, allowance, level int) {
	lines := pystr.Splitlines(s, true)
	if level == 1 {
		indent++
		allowance++
	}
	maxWidth := 72 - indent
	maxWidth1 := maxWidth
	var chunks []string
	rep := ""
	for i, line := range lines {
		rep = pyrepr.Repr(line)
		if i == len(lines)-1 {
			maxWidth1 -= allowance
		}
		if utf8.RuneCountInString(rep) <= maxWidth1 {
			chunks = append(chunks, rep)
			continue
		}
		parts := wordParts(line)
		maxWidth2 := maxWidth
		current := ""
		for j, part := range parts {
			candidate := current + part
			if j == len(parts)-1 && i == len(lines)-1 {
				maxWidth2 -= allowance
			}
			if utf8.RuneCountInString(pyrepr.Repr(candidate)) > maxWidth2 {
				if current != "" {
					chunks = append(chunks, pyrepr.Repr(current))
				}
				current = part
			} else {
				current = candidate
			}
		}
		if current != "" {
			chunks = append(chunks, pyrepr.Repr(current))
		}
	}
	if len(chunks) == 1 {
		b.WriteString(rep)
		return
	}
	if level == 1 {
		b.WriteByte('(')
	}
	for i, c := range chunks {
		if i > 0 {
			b.WriteString("\n" + strings.Repeat(" ", indent))
		}
		b.WriteString(c)
	}
	if level == 1 {
		b.WriteByte(')')
	}
}

// Alternating non space then space runs re.findall of \S*\s* yields without its empty tail
func wordParts(line string) []string {
	var parts []string
	rs := []rune(line)
	for i := 0; i < len(rs); {
		j := i
		for j < len(rs) && !pystr.IsSpace(rs[j]) {
			j++
		}
		for j < len(rs) && pystr.IsSpace(rs[j]) {
			j++
		}
		parts = append(parts, string(rs[i:j]))
		i = j
	}
	return parts
}
