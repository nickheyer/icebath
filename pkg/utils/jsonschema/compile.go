package jsonschemautil

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dlclark/regexp2/v2"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

const (
	boolNone int8 = iota
	boolTrue
	boolFalse
)

// Compiled schema: true, false, or the checks of its keywords in the order they run
type node struct {
	boolean  int8
	unit     *unit
	location string
	// Types the schema's "type" allows, for judging how relevant its errors are
	types    typeSet
	hasTypes bool
	// Holds unevaluatedProperties or unevaluatedItems, which read what the other keywords evaluated
	tracks bool
	checks []check
}

// Check of one keyword: whether the value passes, recording errors in the state and evaluated members in ev
type checkFn func(st *state, v any, ev *evaluated) bool

type check struct {
	keyword string
	fn      checkFn
}

// Schema resource as one compile sees it, with the nodes dynamic references reach through it
type unit struct {
	res     *resource
	root    *node
	dynamic map[string]*node
}

// State of one compile
type compiler struct {
	ctx     context.Context
	cfg     *config
	reg     *registry
	nodes   map[*jsonutil.Object]*node
	units   map[*resource]*unit
	order   []*unit
	regexes map[string]*regexp2.Regexp
	failed  map[string]error
	// A dynamic reference was compiled, so every unit needs its dynamic anchors and recursive root compiled
	dynamic bool
}

func newCompiler(ctx context.Context, cfg *config) *compiler {
	return &compiler{ctx: ctx, cfg: cfg, reg: newRegistry(bundled()), nodes: map[*jsonutil.Object]*node{},
		units: map[*resource]*unit{}, regexes: map[string]*regexp2.Regexp{}, failed: map[string]error{}}
}

// Schema error at rel below the schema at l
func (c *compiler) bad(l loc, rel, format string, args ...any) error {
	return &SchemaError{Location: l.uri() + rel, Message: fmt.Sprintf(format, args...)}
}

func (c *compiler) unit(res *resource) *unit {
	if u := c.units[res]; u != nil {
		return u
	}
	u := &unit{res: res}
	c.units[res] = u
	c.order = append(c.order, u)
	return u
}

// Node of a schema at l, compiled once however many places refer to it
func (c *compiler) compile(v any, l loc) (*node, error) {
	switch x := v.(type) {
	case bool:
		n := &node{boolean: boolFalse, unit: c.unit(l.res), location: l.uri()}
		if x {
			n.boolean = boolTrue
		}
		return n, nil
	case *jsonutil.Object:
		if n := c.nodes[x]; n != nil {
			return n, nil
		}
		if known, ok := c.reg.loc(x); ok {
			l = known
		}
		n := &node{unit: c.unit(l.res), location: l.uri()}
		c.nodes[x] = n
		if err := c.fill(n, x, l); err != nil {
			return nil, err
		}
		return n, nil
	}
	return nil, &SchemaError{Location: l.uri(), Message: "a schema must be an object or a boolean, not " + kindName(v)}
}

// Node of the subschema found at rel below the schema at l
func (c *compiler) sub(v any, l loc, rel string) (*node, error) {
	at := loc{res: l.res, ptr: l.ptr + rel, dialect: l.dialect}
	if o, ok := v.(*jsonutil.Object); ok {
		if _, known := c.reg.loc(o); !known {
			c.reg.crawl(o, at.res, at.ptr, at.dialect)
		}
	}
	return c.compile(v, at)
}

// Compiles the keywords of a schema object, those reading what the others evaluated last
func (c *compiler) fill(n *node, o *jsonutil.Object, l loc) error {
	d := l.dialect
	if d.refAlone {
		if ref, ok := o.Get("$ref"); ok {
			fn, err := c.ref(l, ref, false)
			if err != nil {
				return err
			}
			n.checks = []check{{keyword: "$ref", fn: fn}}
			return nil
		}
	}
	var last []check
	content := false
	for key, value := range o.All() {
		if !d.keywords[key] {
			continue
		}
		if key == "contentEncoding" || key == "contentMediaType" || key == "contentSchema" {
			if content || !c.cfg.content {
				continue
			}
			content = true
		}
		fn, err := c.keyword(n, o, l, key, value)
		if err != nil {
			return err
		}
		if fn == nil {
			continue
		}
		if key == "unevaluatedProperties" || key == "unevaluatedItems" {
			n.tracks = true
			last = append(last, check{keyword: key, fn: fn})
			continue
		}
		n.checks = append(n.checks, check{keyword: key, fn: fn})
	}
	n.checks = append(n.checks, last...)
	return nil
}

// Check of one keyword of a schema object in its dialect, nil when the keyword asserts nothing here
func (c *compiler) keyword(n *node, o *jsonutil.Object, l loc, key string, value any) (checkFn, error) {
	d := l.dialect
	switch key {
	case "$ref":
		return c.ref(l, value, false)
	case "$dynamicRef":
		return c.ref(l, value, true)
	case "$recursiveRef":
		return c.recursiveRef(l, value)
	case "type":
		return c.typeCheck(n, l, value)
	case "enum":
		return c.enum(l, value)
	case "const":
		return constCheck(value), nil
	case "allOf":
		return c.allOf(l, key, value)
	case "anyOf":
		return c.anyOf(l, value)
	case "oneOf":
		return c.oneOf(l, value)
	case "not":
		return c.not(l, value)
	case "if":
		return c.ifThenElse(l, o, value)
	case "format":
		return c.format(l, value), nil
	case "contentEncoding", "contentMediaType", "contentSchema":
		return c.content(l, o)
	case "multipleOf", "divisibleBy":
		return c.multipleOf(l, key, value)
	case "minimum", "maximum":
		if d.draft <= Draft4 {
			return c.legacyBound(l, o, key, value)
		}
		return c.bound(l, key, value)
	case "exclusiveMinimum", "exclusiveMaximum":
		return c.bound(l, key, value)
	case "minLength", "maxLength":
		return c.length(l, key, value)
	case "pattern":
		return c.pattern(l, value)
	case "minItems", "maxItems":
		return c.itemCount(l, key, value)
	case "uniqueItems":
		return uniqueItems(value), nil
	case "items":
		if d.draft == Draft202012 {
			return c.items(l, o, value)
		}
		return c.legacyItems(l, value)
	case "prefixItems":
		return c.prefixItems(l, value)
	case "additionalItems":
		return c.additionalItems(l, o, value)
	case "contains":
		if d.draft >= Draft201909 {
			return c.contains(l, o, value)
		}
		return c.containsAny(l, value)
	case "unevaluatedItems":
		return c.unevaluatedItems(l, value)
	case "properties":
		return c.properties(l, value, d.draft == Draft3)
	case "patternProperties":
		return c.patternProperties(l, value)
	case "additionalProperties":
		return c.additionalProperties(l, o, value)
	case "propertyNames":
		return c.propertyNames(l, value)
	case "required":
		return c.required(l, value)
	case "minProperties", "maxProperties":
		return c.propertyCount(l, key, value)
	case "dependencies":
		return c.dependencies(l, value, d.draft == Draft3)
	case "dependentRequired":
		return c.dependentRequired(l, value)
	case "dependentSchemas":
		return c.dependentSchemas(l, value)
	case "unevaluatedProperties":
		return c.unevaluatedProperties(l, value)
	case "disallow":
		return c.disallow(l, value)
	case "extends":
		return c.extends(l, value)
	}
	return nil, nil
}

// Pattern compiled once per compile
func (c *compiler) regex(l loc, rel, pattern string) (*regexp2.Regexp, error) {
	if re := c.regexes[pattern]; re != nil {
		return re, nil
	}
	re, err := compilePattern(pattern, c.cfg.timeout)
	if err != nil {
		return nil, &SchemaError{Location: l.uri() + rel,
			Message: quote(pattern, schemaQuoteLimit) + " is not a valid regular expression: " + err.Error(), Err: err}
	}
	c.regexes[pattern] = re
	return re, nil
}

// Target of a reference made from the schema at l with its location, and for a dynamic reference that lands on a dynamic anchor the anchor's name
func (c *compiler) resolve(l loc, ref string, dynamic bool) (any, loc, string, error) {
	base, frag := splitFragment(resolveURI(l.res.uri, ref))
	res, err := c.resource(normalizeURI(base), l.dialect)
	if err != nil {
		if errors.Is(err, ErrUnresolvable) {
			return nil, loc{}, "", &SchemaError{Location: l.uri(),
				Message: "reference " + strconv.Quote(ref) + " names no document the schema contains or was given", Err: err}
		}
		return nil, loc{}, "", &SchemaError{Location: l.uri(),
			Message: "reference " + strconv.Quote(ref) + " cannot be loaded: " + err.Error(), Err: err}
	}
	frag = urlutil.Unescape(frag)
	if frag == "" || frag[0] == '/' {
		v, at, ok := c.pointer(res, frag)
		if !ok {
			return nil, loc{}, "", &SchemaError{Location: l.uri(),
				Message: "reference " + strconv.Quote(ref) + " points to nothing in " + displayURI(res.uri), Err: ErrUnresolvable}
		}
		return v, at, "", nil
	}
	v, ok := res.anchors[frag]
	if !ok {
		return nil, loc{}, "", &SchemaError{Location: l.uri(),
			Message: "reference " + strconv.Quote(ref) + " names an anchor " + displayURI(res.uri) + " does not declare", Err: ErrUnresolvable}
	}
	at, known := c.reg.loc(v.(*jsonutil.Object))
	if !known {
		at = loc{res: res, dialect: res.dialect}
	}
	name := ""
	if dynamic && res.dynamic[frag] == v {
		name = frag
	}
	return v, at, name, nil
}

// URI of a resource for messages, the root document named as such when it has none
func displayURI(uri string) string {
	if uri == "" {
		return "the schema"
	}
	return uri
}

// Value a JSON pointer names inside a resource and its location, crawling it when no crawl reached it yet
func (c *compiler) pointer(res *resource, ptr string) (any, loc, bool) {
	cur := res.root
	at := loc{res: res, dialect: res.dialect}
	if o, ok := cur.(*jsonutil.Object); ok {
		if known, ok := c.reg.loc(o); ok {
			at = known
		}
	}
	rel := ""
	if ptr != "" {
		for token := range strings.SplitSeq(ptr[1:], "/") {
			token = unescapeToken(token)
			switch x := cur.(type) {
			case *jsonutil.Object:
				v, ok := x.Get(token)
				if !ok {
					return nil, loc{}, false
				}
				cur = v
			case []any:
				i, ok := arrayIndex(token, len(x))
				if !ok {
					return nil, loc{}, false
				}
				cur = x[i]
			default:
				return nil, loc{}, false
			}
			rel += "/" + escapeToken(token)
			if o, ok := cur.(*jsonutil.Object); ok {
				if known, ok := c.reg.loc(o); ok {
					at, rel = known, ""
				}
			}
		}
	}
	if rel == "" {
		return cur, at, true
	}
	target := loc{res: at.res, ptr: at.ptr + rel, dialect: at.dialect}
	if o, ok := cur.(*jsonutil.Object); ok {
		c.reg.crawl(o, target.res, target.ptr, target.dialect)
		if known, ok := c.reg.loc(o); ok {
			target = known
		}
	}
	return cur, target, true
}

// Array position a pointer token names, false for anything but a decimal without leading zeros below n
func arrayIndex(token string, n int) (int, bool) {
	if token == "" || len(token) > 1 && token[0] == '0' {
		return 0, false
	}
	i, err := strconv.Atoi(token)
	if err != nil || i < 0 || i >= n {
		return 0, false
	}
	return i, true
}

// Resource registered under uri, loaded through the loaders the first time it is asked for; d reads a loaded document that names no dialect
func (c *compiler) resource(uri string, d *dialect) (*resource, error) {
	if res := c.reg.resource(uri); res != nil {
		return res, nil
	}
	if err, failed := c.failed[uri]; failed {
		return nil, err
	}
	res, err := c.load(uri, d)
	if err != nil {
		c.failed[uri] = err
		return nil, err
	}
	return res, nil
}

func (c *compiler) load(uri string, d *dialect) (*resource, error) {
	if uri == "" {
		return nil, ErrUnresolvable
	}
	for _, loader := range c.cfg.loaders {
		doc, err := loader(c.ctx, uri)
		if errors.Is(err, ErrUnsupportedURI) {
			continue
		}
		if err != nil {
			return nil, err
		}
		norm, err := normalize(doc)
		if err != nil {
			return nil, err
		}
		return c.reg.addDocument(uri, norm, d), nil
	}
	return nil, ErrUnresolvable
}

// Compiles what dynamic references may reach at validation time: every dynamic anchor and recursive root of every unit
func (c *compiler) finish() error {
	if !c.dynamic {
		return nil
	}
	for i := 0; i < len(c.order); i++ {
		u := c.order[i]
		if u.res.recursive && u.root == nil {
			at := loc{res: u.res, dialect: u.res.dialect}
			n, err := c.compile(u.res.root, at)
			if err != nil {
				return err
			}
			u.root = n
		}
		for name, v := range u.res.dynamic {
			if u.dynamic[name] != nil {
				continue
			}
			o, ok := v.(*jsonutil.Object)
			if !ok {
				continue
			}
			at, known := c.reg.loc(o)
			if !known {
				at = loc{res: u.res, dialect: u.res.dialect}
			}
			n, err := c.compile(o, at)
			if err != nil {
				return err
			}
			if u.dynamic == nil {
				u.dynamic = map[string]*node{}
			}
			u.dynamic[name] = n
		}
	}
	return nil
}
