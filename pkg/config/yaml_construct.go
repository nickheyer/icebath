package config

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	ry "github.com/nickheyer/icebath/pkg/internal/ruamelyaml"
)

// Python objects the safe constructor builds before they become Go values, containers by reference
type (
	pyList struct {
		items []any
		rt    bool
	}
	pyTuple struct {
		items []any
		rtKey bool
	}
	pyDict struct {
		keys, vals []any
		index      map[string]int
		kind       dictKind
	}
	pySet struct {
		keys  []any
		index map[string]int
		rt    bool
	}
	pyNaN  struct{ neg bool }
	pyTime struct {
		v      any
		shared bool
	}
)

// Python class of a dict object, plain, ruamel ordereddict or one of the round trip containers
type dictKind int8

const (
	dictPlain dictKind = iota
	dictOrdered
	dictCommented
	dictCommentedOrdered
	dictCommentedKey
)

func newPyDict() *pyDict { return &pyDict{index: map[string]int{}} }

// Python set a YAML !!set loads to, members in insertion order
type YAMLSet struct{ Items []any }

func (YAMLSet) PyTypeName() string { return "set" }

// Repr of the set with its members in insertion order
func (s YAMLSet) PyRepr() string {
	if len(s.Items) == 0 {
		return "set()"
	}
	parts := make([]string, len(s.Items))
	for i, it := range s.Items {
		parts[i] = pyrepr.Repr(it)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func pyErr(class, msg string) *ry.PyException { return ry.Raise(class, msg) }

// Python type name of an intermediate object for TypeError text
func pyObjType(v any) string {
	switch x := v.(type) {
	case *pyList:
		return "list"
	case *pyTuple:
		return "tuple"
	case *pyDict:
		if x.kind == dictOrdered {
			return "ordereddict"
		}
		return "dict"
	case *pySet:
		return "set"
	}
	return "object"
}

// Type name an unhashable member reports, the round trip container classes by their own names
func unhashableName(v any, rt bool) string {
	if rt {
		return rtTypeName(v)
	}
	return pyObjType(v)
}

// Hash and equality class of a Python key, the TypeError of an unhashable member
func pyHashKey(v any) string {
	switch x := v.(type) {
	case nil:
		return "N"
	case bool:
		if x {
			return "n1"
		}
		return "n0"
	case *big.Int:
		return "n" + x.String()
	case float64:
		if !math.IsInf(x, 0) && x == math.Trunc(x) {
			n, _ := new(big.Float).SetFloat64(x).Int(nil)
			return "n" + n.String()
		}
		return "f" + strconv.FormatFloat(x, 'g', -1, 64)
	case *pyNaN:
		return fmt.Sprintf("nan%p", x)
	case string:
		return "s" + x
	case []byte:
		return "b" + string(x)
	case *pyTime:
		return timeHashKey(x.v)
	case *pyTuple:
		parts := make([]string, len(x.items))
		for i, it := range x.items {
			parts[i] = pyHashKey(it)
		}
		return "(" + strings.Join(parts, "\x01") + ")"
	case *rtBool:
		if x.b {
			return "n1"
		}
		return "n0"
	case *rtStamp:
		return timeHashKey(x.v)
	case *rtTagged:
		return fmt.Sprintf("o%p", x)
	case *pyDict:
		if x.kind == dictCommentedKey {
			parts := make([]string, len(x.keys))
			for i, k := range x.keys {
				parts[i] = "(" + pyHashKey(k) + "\x01" + pyHashKey(x.vals[i]) + ")"
			}
			return "K(" + strings.Join(parts, "\x01") + ")"
		}
	}
	panic(pyErr("TypeError", "unhashable type: '"+unhashableName(v, isRTObject(v))+"'"))
}

// Whether an unhashable object came from the round trip constructor
func isRTObject(v any) bool {
	switch x := v.(type) {
	case *pyDict:
		return x.kind >= dictCommented
	case *pySet:
		return x.rt
	case *pyList:
		return x.rt
	}
	return false
}

// Equality class of a date or datetime, naive values by wall time and aware values by UTC instant
func timeHashKey(v any) string {
	switch t := v.(type) {
	case pydatetime.Date:
		return fmt.Sprintf("d%04d-%02d-%02d", t.Year, t.Month, t.Day)
	case pydatetime.DateTime:
		wall := time.Date(t.Year, time.Month(t.Month), t.Day, t.Hour, t.Minute, t.Second, t.Microsecond*1000, time.UTC)
		if t.Zone == nil {
			return "t" + wall.Format(time.RFC3339Nano)
		}
		return "a" + wall.Add(-t.Zone.Offset).Format(time.RFC3339Nano)
	}
	return "?"
}

// Key class of a dict key lookup, the TypeError a dict raises for an unhashable key
func dictKey(v any) string {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*ry.PyException); ok && e.Class == "TypeError" {
				panic(pyErr("TypeError", "cannot use '"+pyKeyTypeName(v)+"' as a dict key ("+e.Msg+")"))
			}
			panic(r)
		}
	}()
	return pyHashKey(v)
}

func pyKeyTypeName(v any) string {
	switch x := v.(type) {
	case *pyTuple:
		if x.rtKey {
			return "ruamel.yaml.comments.CommentedKeySeq"
		}
	case *pyDict:
		switch x.kind {
		case dictCommentedKey:
			return "ruamel.yaml.comments.CommentedKeyMap"
		case dictOrdered:
			return "ruamel.yaml.compat.ordereddict"
		case dictCommented:
			return "ruamel.yaml.comments.CommentedMap"
		case dictCommentedOrdered:
			return "ruamel.yaml.comments.CommentedOrderedMap"
		}
	case *pySet:
		if x.rt {
			return "ruamel.yaml.comments.CommentedSet"
		}
	case *pyList:
		if x.rt {
			return "ruamel.yaml.comments.CommentedSeq"
		}
	}
	return pyObjType(v)
}

func (d *pyDict) get(k string) (any, bool) {
	i, ok := d.index[k]
	if !ok {
		return nil, false
	}
	return d.vals[i], true
}

// Assigns a value, an equal existing key keeping its original object and position
func (d *pyDict) set(key, val any) {
	k := dictKey(key)
	if i, ok := d.index[k]; ok {
		d.vals[i] = val
		return
	}
	d.index[k] = len(d.keys)
	d.keys = append(d.keys, key)
	d.vals = append(d.vals, val)
}

func (d *pyDict) update(o *pyDict) {
	for i, k := range o.keys {
		d.set(k, o.vals[i])
	}
}

// Python str of an intermediate object, containers by repr with recursion shown as ellipses
func pyObjStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case *pyTime:
		return pyrepr.Str(x.v)
	case *rtBool:
		if x.b {
			return "1"
		}
		return "0"
	case *rtStamp:
		return x.str()
	case *rtTagged:
		return x.value
	}
	return pyObjRepr(v, map[any]bool{})
}

func pyObjRepr(v any, active map[any]bool) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case *big.Int:
		return x.String()
	case float64:
		return pyrepr.Float(x)
	case *pyNaN:
		return "nan"
	case string:
		return pyrepr.StrRepr(x)
	case []byte:
		return pyrepr.BytesRepr(x)
	case *pyTime:
		return pyrepr.Repr(x.v)
	case *rtBool:
		if x.b {
			return "1"
		}
		return "0"
	case *rtStamp:
		return x.repr()
	case *rtTagged:
		return x.repr()
	case *pyList:
		if active[x] {
			return "[...]"
		}
		active[x] = true
		defer delete(active, x)
		return "[" + joinObjRepr(x.items, active) + "]"
	case *pyTuple:
		if active[x] {
			return "(...)"
		}
		active[x] = true
		defer delete(active, x)
		if len(x.items) == 1 {
			return "(" + pyObjRepr(x.items[0], active) + ",)"
		}
		return "(" + joinObjRepr(x.items, active) + ")"
	case *pySet:
		if x.rt {
			return "set(odict_keys([" + joinObjRepr(x.keys, active) + "]))"
		}
		if len(x.keys) == 0 {
			return "set()"
		}
		if active[x] {
			return "set(...)"
		}
		active[x] = true
		defer delete(active, x)
		return "{" + joinObjRepr(x.keys, active) + "}"
	case *pyDict:
		if active[x] {
			return "{...}"
		}
		active[x] = true
		defer delete(active, x)
		parts := make([]string, len(x.keys))
		for i, k := range x.keys {
			parts[i] = pyObjRepr(k, active) + ": " + pyObjRepr(x.vals[i], active)
		}
		body := "{" + strings.Join(parts, ", ") + "}"
		switch x.kind {
		case dictOrdered:
			if len(x.keys) == 0 {
				return "ordereddict()"
			}
			return "ordereddict(" + body + ")"
		case dictCommentedKey:
			return "ordereddict(" + body + ")"
		}
		return body
	}
	return fmt.Sprint(v)
}

func joinObjRepr(items []any, active map[any]bool) string {
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = pyObjRepr(it, active)
	}
	return strings.Join(parts, ", ")
}

// Deferred generator body and the Python frames it runs in
type pendingFill struct {
	cost int
	fill func()
}

// ruamel SafeConstructor state for one document, deferred container fills standing in for its generators
type safeConstructor struct {
	rt          bool
	doc         *ry.Document
	constructed map[*ry.Node]any
	recursive   map[*ry.Node]bool
	pending     []pendingFill
	deep        bool
	frames      int
	flattening  int
	warnings    []ry.Warning
}

func constructorError(context string, contextMark *ry.Mark, problem string, problemMark *ry.Mark) *ry.MarkedError {
	e := &ry.MarkedError{Class: "ConstructorError", ContextMark: contextMark, ProblemMark: problemMark, Problem: &problem}
	if context != "" {
		e.Context = &context
	}
	return e
}

// Python value of a composed document the way SafeConstructor.construct_document builds it
func constructDocument(doc *ry.Document) (any, []ry.Warning, error) {
	return constructDocumentMode(doc, false)
}

// Python value of a composed document under the safe or the round trip constructor
func constructDocumentMode(doc *ry.Document, rt bool) (value any, warnings []ry.Warning, err error) {
	c := &safeConstructor{rt: rt, doc: doc, constructed: map[*ry.Node]any{}, recursive: map[*ry.Node]bool{}}
	defer func() {
		if r := recover(); r != nil {
			warnings = c.warnings
			switch e := r.(type) {
			case *ry.MarkedError:
				err = e
			case *ry.PyException:
				err = e
			default:
				panic(r)
			}
		}
	}()
	data := c.constructObject(doc.Root, false)
	for len(c.pending) > 0 {
		gens := c.pending
		c.pending = nil
		for _, g := range gens {
			c.inFrames(g.cost, g.fill)
		}
	}
	return toGoValue(data, map[any]any{}), c.warnings, nil
}

func (c *safeConstructor) constructObject(n *ry.Node, deep bool) any {
	if v, ok := c.constructed[n]; ok {
		return v
	}
	var oldDeep bool
	if deep {
		oldDeep = c.deep
		c.deep = true
	}
	if c.recursive[n] {
		return nil
	}
	c.recursive[n] = true
	var data any
	c.inFrames(2, func() { data = c.constructNonRecursive(n) })
	c.constructed[n] = data
	delete(c.recursive, n)
	if deep {
		c.deep = oldDeep
	}
	return data
}

const yamlTagPrefix = "tag:yaml.org,2002:"

// Container of a generator constructor, its fill run now under deep construction or deferred
func (c *safeConstructor) generator(data any, cost int, fill func()) any {
	if c.deep {
		c.inFrames(cost, fill)
	} else {
		c.pending = append(c.pending, pendingFill{cost, fill})
	}
	return data
}

// Python frames construct_document may stack, the C or pure safe_load sitting two frames deeper than a round trip load
func (c *safeConstructor) frameLimit() int {
	if c.rt {
		return 995
	}
	return 993
}

// Runs f with cost more Python frames on the stack, the RecursionError CPython raises past its limit
func (c *safeConstructor) inFrames(cost int, f func()) {
	c.frames += cost
	defer func() { c.frames -= cost }()
	if c.frames > c.frameLimit() {
		panic(pyErr("RecursionError", "maximum recursion depth exceeded"))
	}
	f()
}

func (c *safeConstructor) constructNonRecursive(n *ry.Node) any {
	if c.rt {
		return c.constructNonRecursiveRT(n)
	}
	tag := n.Tag()
	switch tag {
	case yamlTagPrefix + "null", yamlTagPrefix + "bool", yamlTagPrefix + "int", yamlTagPrefix + "float", yamlTagPrefix + "binary", yamlTagPrefix + "str":
		var v any
		c.inFrames(3, func() { v = c.constructSafeScalar(n, tag) })
		return v
	case yamlTagPrefix + "timestamp":
		var v any
		c.inFrames(2, func() { v = c.constructTimestamp(n) })
		return v
	}
	return c.constructSafeCollection(n, tag)
}

// Scalar value of one of the safe scalar tags
func (c *safeConstructor) constructSafeScalar(n *ry.Node, tag string) any {
	switch tag {
	case yamlTagPrefix + "null":
		c.constructScalar(n)
		return nil
	case yamlTagPrefix + "bool":
		value := c.constructScalar(n)
		low := pystr.Lower(value)
		b, ok := boolValues[low]
		if !ok {
			panic(pyErr("KeyError", pyrepr.StrRepr(low)))
		}
		return b
	case yamlTagPrefix + "int":
		return c.constructInt(n)
	case yamlTagPrefix + "float":
		return c.constructFloat(n)
	case yamlTagPrefix + "binary":
		return c.constructBinary(n)
	}
	return c.constructScalar(n)
}

// Container of a safe collection tag or the undetermined constructor error
func (c *safeConstructor) constructSafeCollection(n *ry.Node, tag string) any {
	switch tag {
	case yamlTagPrefix + "omap":
		omap := newPyDict()
		omap.kind = dictOrdered
		return c.generator(omap, 1, func() { c.fillOmap(n, omap) })
	case yamlTagPrefix + "pairs":
		pairs := &pyList{}
		return c.generator(pairs, 1, func() { c.fillPairs(n, pairs) })
	case yamlTagPrefix + "set":
		set := &pySet{index: map[string]int{}}
		return c.generator(set, 3, func() {
			value := c.constructMapping(n, false)
			for _, k := range value.keys {
				hk := pyHashKey(k)
				if _, ok := set.index[hk]; !ok {
					set.index[hk] = len(set.keys)
					set.keys = append(set.keys, k)
				}
			}
		})
	case yamlTagPrefix + "seq":
		list := &pyList{}
		return c.generator(list, 2, func() { list.items = append(list.items, c.constructSequence(n, false)...) })
	case yamlTagPrefix + "map":
		m := newPyDict()
		return c.generator(m, 3, func() { m.update(c.constructMapping(n, false)) })
	}
	panic(constructorError("", nil, "could not determine a constructor for the tag "+pyrepr.StrRepr(tag), n.Start))
}

// Scalar text of a node, a mapping answering with its value key entry as SafeConstructor.construct_scalar does
func (c *safeConstructor) constructScalar(n *ry.Node) string {
	if c.rt {
		return c.constructScalarRT(n)
	}
	if n.Kind == ry.MappingNode {
		for _, p := range n.Pairs {
			if p.Key.Tag() == yamlTagPrefix+"value" {
				return c.constructScalar(p.Value)
			}
		}
	}
	if n.Kind != ry.ScalarNode {
		panic(constructorError("", nil, "expected a scalar node, but found "+n.Kind.ID(), n.Start))
	}
	return n.Value
}

func (c *safeConstructor) constructSequence(n *ry.Node, deep bool) []any {
	if n.Kind != ry.SequenceNode {
		panic(constructorError("", nil, "expected a sequence node, but found "+n.Kind.ID(), n.Start))
	}
	out := make([]any, 0, len(n.Items))
	for _, child := range n.Items {
		out = append(out, c.constructObject(child, deep))
	}
	return out
}

// Applies the merge key feature, merged pairs placed ahead of the mapping's own pairs
func (c *safeConstructor) flattenMapping(n *ry.Node) {
	c.flattening++
	defer func() { c.flattening-- }()
	if c.flattening > 990 {
		panic(pyErr("RecursionError", "maximum recursion depth exceeded"))
	}
	var merge []ry.Pair
	index := 0
	for index < len(n.Pairs) {
		key, value := n.Pairs[index].Key, n.Pairs[index].Value
		switch key.Tag() {
		case yamlTagPrefix + "merge":
			if len(merge) > 0 {
				e := &ry.MarkedError{Class: "DuplicateKeyError", Context: strPtr("while constructing a mapping"), ContextMark: n.Start,
					Problem: strPtr(`found duplicate merge key "<<"`), ProblemMark: key.Start,
					Note: "                        Duplicate merge keys are never allowed, not even when\n                        `.allow_duplicate_keys` is set to True\n                        "}
				panic(e)
			}
			n.Pairs = append(n.Pairs[:index:index], n.Pairs[index+1:]...)
			switch value.Kind {
			case ry.MappingNode:
				c.flattenMapping(value)
				merge = append(merge, value.Pairs...)
			case ry.SequenceNode:
				var submerge [][]ry.Pair
				for _, sub := range value.Items {
					if sub.Kind != ry.MappingNode {
						panic(constructorError("while constructing a mapping", n.Start, "expected a mapping for merging, but found "+sub.Kind.ID(), sub.Start))
					}
					c.flattenMapping(sub)
					submerge = append(submerge, sub.Pairs)
				}
				for i := len(submerge) - 1; i >= 0; i-- {
					merge = append(merge, submerge[i]...)
				}
			default:
				panic(constructorError("while constructing a mapping", n.Start, "expected a mapping or list of mappings for merging, but found "+value.Kind.ID(), value.Start))
			}
		case yamlTagPrefix + "value":
			key.SetTag(yamlTagPrefix + "str")
			index++
		default:
			index++
		}
	}
	if len(merge) > 0 {
		n.Merge = append([]ry.Pair(nil), merge...)
		n.Merged = true
		n.Pairs = append(append([]ry.Pair(nil), merge...), n.Pairs...)
	}
}

func strPtr(s string) *string { return &s }

// Dict of a mapping node with merges flattened, duplicate keys refused unless a merge is present
func (c *safeConstructor) constructMapping(n *ry.Node, deep bool) *pyDict {
	if n.Kind == ry.MappingNode {
		c.flattenMapping(n)
	}
	if n.Kind != ry.MappingNode {
		panic(constructorError("", nil, "expected a mapping node, but found "+n.Kind.ID(), n.Start))
	}
	total := newPyDict()
	type todo struct {
		pairs []ry.Pair
		check bool
	}
	var work []todo
	if n.Merged {
		work = []todo{{n.Merge, false}, {n.Pairs, false}}
	} else {
		work = []todo{{n.Pairs, true}}
	}
	for _, w := range work {
		mapping := newPyDict()
		for _, p := range w.pairs {
			key := c.constructObject(p.Key, true)
			if l, ok := key.(*pyList); ok {
				key = &pyTuple{items: append([]any(nil), l.items...)}
			}
			switch key.(type) {
			case *pyDict, *pySet:
				panic(constructorError("while constructing a mapping", n.Start, "found unhashable key", p.Key.Start))
			}
			value := c.constructObject(p.Value, deep)
			if w.check {
				k := dictKey(key)
				if mk, dup := mapping.get(k); dup {
					problem := fmt.Sprintf("found duplicate key \"%s\" with value \"%s\" (original value: \"%s\")", pyObjStr(key), pyObjStr(value), pyObjStr(mk))
					panic(&ry.MarkedError{Class: "DuplicateKeyError", Context: strPtr("while constructing a mapping"), ContextMark: n.Start,
						Problem: &problem, ProblemMark: p.Key.Start,
						Note: "\n                    To suppress this check see:\n                        https://yaml.dev/doc/ruamel.yaml/api/#Duplicate_keys\n                    "})
				}
				mapping.set(key, value)
			} else {
				mapping.set(key, value)
			}
		}
		total.update(mapping)
	}
	return total
}

var boolValues = map[string]bool{"yes": true, "no": false, "y": true, "n": false, "true": true, "false": false, "on": true, "off": false}

// Python int() of text in a base, the ValueError texts CPython gives for bad literals and over long decimals
func yamlIntOf(text string, base int) *big.Int {
	if base == 10 {
		digits := 0
		for _, r := range text {
			if r != '_' && !pystr.IsSpace(r) && r != '+' && r != '-' {
				digits++
			}
		}
		if n, ok := pyint.ParseBase(text, base); ok {
			if digits > 4300 {
				panic(pyErr("ValueError", fmt.Sprintf("Exceeds the limit (4300 digits) for integer string conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit", digits)))
			}
			return n
		}
	} else if n, ok := pyint.ParseBase(text, base); ok {
		return n
	}
	r := []rune(pyrepr.StrRepr(text))
	if len(r) > 200 {
		r = r[:200]
	}
	panic(pyErr("ValueError", fmt.Sprintf("invalid literal for int() with base %d: %s", base, string(r))))
}

func strIndex0(s string) rune {
	if s == "" {
		panic(pyErr("IndexError", "string index out of range"))
	}
	return pystr.Runes(s)[0]
}

func (c *safeConstructor) constructInt(n *ry.Node) any {
	valueS := strings.ReplaceAll(c.constructScalar(n), "_", "")
	sign := big.NewInt(1)
	if strIndex0(valueS) == '-' {
		sign = big.NewInt(-1)
	}
	if first := strIndex0(valueS); first == '+' || first == '-' {
		valueS = string(pystr.Runes(valueS)[1:])
	}
	switch {
	case valueS == "0":
		return big.NewInt(0)
	case strings.HasPrefix(valueS, "0b"):
		return new(big.Int).Mul(sign, yamlIntOf(valueS[2:], 2))
	case strings.HasPrefix(valueS, "0x"):
		return new(big.Int).Mul(sign, yamlIntOf(valueS[2:], 16))
	case strings.HasPrefix(valueS, "0o"):
		return new(big.Int).Mul(sign, yamlIntOf(valueS[2:], 8))
	case c.doc.IsYAML11() && strIndex0(valueS) == '0':
		return new(big.Int).Mul(sign, yamlIntOf(valueS, 8))
	case c.doc.IsYAML11() && strings.Contains(valueS, ":"):
		parts := strings.Split(valueS, ":")
		value, base := new(big.Int), big.NewInt(1)
		digits := make([]*big.Int, len(parts))
		for i, part := range parts {
			digits[i] = yamlIntOf(part, 10)
		}
		for i := len(digits) - 1; i >= 0; i-- {
			value.Add(value, new(big.Int).Mul(digits[i], base))
			base.Mul(base, big.NewInt(60))
		}
		return value.Mul(value, sign)
	}
	return new(big.Int).Mul(sign, yamlIntOf(valueS, 10))
}

// Python float() of a str, the ValueError naming the text
func pyFloatText(text string) float64 {
	f, err := pyint.ParseFloat(text)
	if err != nil {
		panic(pyErr("ValueError", err.Error()))
	}
	return f
}

const mantissaWarning = `
In YAML 1.1 floating point values should have a dot ('.') in their mantissa.
See the Floating-Point Language-Independent Type for YAML™ Version 1.1 specification
( http://yaml.org/type/float.html ). This dot is not required for JSON nor for YAML 1.2

Correct your float: "%s" on line: %d, column: %d

or alternatively include the following in your code:

  import warnings
  warnings.simplefilter('ignore', ruamel.yaml.error.MantissaNoDotYAML1_1Warning)

`

func (c *safeConstructor) constructFloat(n *ry.Node) any {
	valueSO := c.constructScalar(n)
	valueS := pystr.Lower(strings.ReplaceAll(valueSO, "_", ""))
	sign := 1.0
	if strIndex0(valueS) == '-' {
		sign = -1
	}
	if first := strIndex0(valueS); first == '+' || first == '-' {
		valueS = string(pystr.Runes(valueS)[1:])
	}
	switch {
	case valueS == ".inf":
		return sign * math.Inf(1)
	case valueS == ".nan":
		return yamlNaNValue
	case !c.doc.IsYAML12() && strings.Contains(valueS, ":"):
		parts := strings.Split(valueS, ":")
		digits := make([]float64, len(parts))
		for i, part := range parts {
			digits[i] = pyFloatText(part)
		}
		value, base := 0.0, 1.0
		for i := len(digits) - 1; i >= 0; i-- {
			value += digits[i] * base
			base *= 60
		}
		return floatObj(sign * value)
	}
	if !c.doc.IsYAML12() && strings.Contains(valueS, "e") {
		parts := strings.Split(valueS, "e")
		if len(parts) != 2 {
			panic(pyErr("ValueError", fmt.Sprintf("too many values to unpack (expected 2, got %d)", len(parts))))
		}
		if !strings.Contains(parts[0], ".") {
			c.warnings = append(c.warnings, ry.Warning{Category: "MantissaNoDotYAML1_1Warning",
				Message: fmt.Sprintf(mantissaWarning, valueSO, n.Start.Line, n.Start.Column), Site: safeMantissaSite})
		}
	}
	return floatObj(sign * pyFloatText(valueS))
}

// Float object, a NaN getting its own identity as a fresh Python float does
func floatObj(f float64) any {
	if math.IsNaN(f) {
		return &pyNaN{neg: math.Signbit(f)}
	}
	return f
}

// Escape a UnicodeError message gives one character, x for Latin-1, u for the BMP and U beyond
func unicodeErrorChar(r rune) string {
	switch {
	case r < 0x100:
		return fmt.Sprintf("\\x%02x", r)
	case r < 0x10000:
		return fmt.Sprintf("\\u%04x", r)
	}
	return fmt.Sprintf("\\U%08x", r)
}

// The one NaN object SafeConstructor.nan_value is, every .nan scalar loading to it
var yamlNaNValue = &pyNaN{neg: true}

// Python base64.decodebytes, binascii.a2b_base64 in its non strict mode
func a2bBase64(data []byte) ([]byte, string) {
	var out []byte
	quadPos, pads := 0, 0
	var leftchar byte
	for _, ch := range data {
		if ch == '=' {
			pads++
			continue
		}
		v := base64Value(ch)
		if v < 0 {
			continue
		}
		this := byte(v)
		pads = 0
		switch quadPos {
		case 0:
			quadPos, leftchar = 1, this
		case 1:
			quadPos = 2
			out = append(out, leftchar<<2|this>>4)
			leftchar = this & 0x0f
		case 2:
			quadPos = 3
			out = append(out, leftchar<<4|this>>2)
			leftchar = this & 0x03
		case 3:
			quadPos = 0
			out = append(out, leftchar<<6|this)
			leftchar = 0
		}
	}
	if quadPos == 1 {
		return nil, fmt.Sprintf("Invalid base64-encoded string: number of data characters (%d) cannot be 1 more than a multiple of 4", len(out)/3*4+1)
	}
	if quadPos != 0 && quadPos+pads < 4 {
		return nil, "Incorrect padding"
	}
	if out == nil {
		out = []byte{}
	}
	return out, ""
}

func base64Value(ch byte) int {
	switch {
	case ch >= 'A' && ch <= 'Z':
		return int(ch - 'A')
	case ch >= 'a' && ch <= 'z':
		return int(ch-'a') + 26
	case ch >= '0' && ch <= '9':
		return int(ch-'0') + 52
	case ch == '+':
		return 62
	case ch == '/':
		return 63
	}
	return -1
}

func (c *safeConstructor) constructBinary(n *ry.Node) any {
	text := c.constructScalar(n)
	runes := pystr.Runes(text)
	for i, r := range runes {
		if r >= 0x80 {
			j := i
			for j < len(runes) && runes[j] >= 0x80 {
				j++
			}
			var msg string
			if j-i == 1 {
				msg = fmt.Sprintf("'ascii' codec can't encode character '%s' in position %d: ordinal not in range(128)", unicodeErrorChar(runes[i]), i)
			} else {
				msg = fmt.Sprintf("'ascii' codec can't encode characters in position %d-%d: ordinal not in range(128)", i, j-1)
			}
			panic(constructorError("", nil, "failed to convert base64 data into ascii: "+msg, n.Start))
		}
	}
	out, problem := a2bBase64([]byte(text))
	if problem != "" {
		panic(constructorError("", nil, "failed to decode base64 data: "+problem, n.Start))
	}
	return out
}

var timestampRegexp = regexp.MustCompile(`^(?P<year>[0-9][0-9][0-9][0-9])-(?P<month>[0-9][0-9]?)-(?P<day>[0-9][0-9]?)(?:((?P<t>[Tt])|[ \t]+)(?P<hour>[0-9][0-9]?):(?P<minute>[0-9][0-9]):(?P<second>[0-9][0-9])(?:\.(?P<fraction>[0-9]*))?(?:[ \t]*(?P<tz>Z|(?P<tz_sign>[-+])(?P<tz_hour>[0-9][0-9]?)(?::(?P<tz_minute>[0-9][0-9]))?))?)?$`)

func (c *safeConstructor) constructTimestamp(n *ry.Node) any {
	var m []string
	if n.Kind == ry.ScalarNode {
		m = timestampRegexp.FindStringSubmatch(n.Value)
		if m == nil && strings.HasSuffix(n.Value, "\n") {
			m = timestampRegexp.FindStringSubmatch(n.Value[:len(n.Value)-1])
		}
	}
	if m == nil {
		panic(constructorError("", nil, `failed to construct timestamp from "`+nodeValueStr(n)+`"`, n.Start))
	}
	group := func(name string) (string, bool) {
		i := timestampRegexp.SubexpIndex(name)
		full := timestampRegexp.FindStringSubmatchIndex(n.Value)
		if full == nil {
			full = timestampRegexp.FindStringSubmatchIndex(n.Value[:len(n.Value)-1])
		}
		return m[i], full[2*i] >= 0
	}
	v := createTimestamp(group)
	return &pyTime{v: v, shared: n.Anchor != nil}
}

// Python str of a node value, a collection printing its child node reprs
func nodeValueStr(n *ry.Node) string {
	switch n.Kind {
	case ry.SequenceNode:
		parts := make([]string, len(n.Items))
		for i, it := range n.Items {
			parts[i] = nodeRepr(it)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case ry.MappingNode:
		parts := make([]string, len(n.Pairs))
		for i, p := range n.Pairs {
			parts[i] = "(" + nodeRepr(p.Key) + ", " + nodeRepr(p.Value) + ")"
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return n.Value
}

// Repr of a ruamel node, the class, tag, anchor and value
func nodeRepr(n *ry.Node) string {
	class := map[ry.NodeKind]string{ry.ScalarNode: "ScalarNode", ry.SequenceNode: "SequenceNode", ry.MappingNode: "MappingNode"}[n.Kind]
	value := pyrepr.StrRepr(n.Value)
	if n.Kind != ry.ScalarNode {
		value = nodeValueStr(n)
	}
	if n.Anchor != nil {
		return class + "(tag=" + pyrepr.StrRepr(n.Tag()) + ", anchor=" + pyrepr.StrRepr(*n.Anchor) + ", value=" + value + ")"
	}
	return class + "(tag=" + pyrepr.StrRepr(n.Tag()) + ", value=" + value + ")"
}

// Python datetime.date argument check raising the shared CPython 3.14 messages
func checkDateArgs(year, month, day int) {
	if msg := pydatetime.DateArgsError(year, month, day); msg != "" {
		panic(pyErr("ValueError", msg))
	}
}

// Date or datetime ruamel's create_timestamp builds from the matched groups
func createTimestamp(group func(string) (string, bool)) any {
	atoi := func(name string) int {
		s, _ := group(name)
		n, _ := strconv.Atoi(s)
		return n
	}
	year, month, day := atoi("year"), atoi("month"), atoi("day")
	if _, ok := group("hour"); !ok {
		checkDateArgs(year, month, day)
		return pydatetime.Date{Year: year, Month: month, Day: day}
	}
	hour, minute, second := atoi("hour"), atoi("minute"), atoi("second")
	frac, fraction := 0, 0
	if f, _ := group("fraction"); f != "" {
		fracS := f
		if len(fracS) > 6 {
			fracS = fracS[:6]
		}
		for len(fracS) < 6 {
			fracS += "0"
		}
		frac, _ = strconv.Atoi(fracS)
		if len(f) > 6 && f[6]-'0' > 4 {
			frac++
		}
		if frac <= 999999 {
			fraction = frac
		}
	}
	var zone *pydatetime.FixedOffset
	tz, _ := group("tz")
	if sign, ok := group("tz_sign"); ok && sign != "" {
		minutes := atoi("tz_hour") * 60
		if tm, _ := group("tz_minute"); tm != "" {
			minutes += atoi("tz_minute")
		}
		if sign == "-" {
			minutes = -minutes
		}
		off := time.Duration(minutes) * time.Minute
		if off <= -24*time.Hour || off >= 24*time.Hour {
			panic(pyErr("ValueError", "offset must be a timedelta strictly between -timedelta(hours=24) and timedelta(hours=24), not "+pydatetime.TimedeltaRepr(off)+"."))
		}
		zone = &pydatetime.FixedOffset{Offset: off, Name: tz}
	} else if tz == "Z" {
		zone = &pydatetime.FixedOffset{Name: tz}
	}
	checkDateArgs(year, month, day)
	if msg := pydatetime.TimeArgsError(hour, minute, second); msg != "" {
		panic(pyErr("ValueError", msg))
	}
	dt := pydatetime.DateTime{Year: year, Month: month, Day: day, Hour: hour, Minute: minute, Second: second, Microsecond: fraction, Zone: zone}
	if frac > 999999 {
		t := time.Date(year, time.Month(month), day, hour, minute, second, fraction*1000, time.UTC).Add(time.Second)
		if t.Year() > 9999 {
			panic(pyErr("OverflowError", "date value out of range"))
		}
		dt.Year, dt.Month, dt.Day, dt.Hour, dt.Minute, dt.Second = t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second()
	}
	return dt
}

func (c *safeConstructor) fillOmap(n *ry.Node, omap *pyDict) {
	const ctx = "while constructing an ordered map"
	if n.Kind != ry.SequenceNode {
		panic(constructorError(ctx, n.Start, "expected a sequence, but found "+n.Kind.ID(), n.Start))
	}
	for _, sub := range n.Items {
		if sub.Kind != ry.MappingNode {
			panic(constructorError(ctx, n.Start, "expected a mapping of length 1, but found "+sub.Kind.ID(), sub.Start))
		}
		if len(sub.Pairs) != 1 {
			panic(constructorError(ctx, n.Start, fmt.Sprintf("expected a single mapping item, but found %d items", len(sub.Pairs)), sub.Start))
		}
		key := c.constructObject(sub.Pairs[0].Key, false)
		if _, dup := omap.get(dictKey(key)); dup {
			panic(pyErr("AssertionError", ""))
		}
		value := c.constructObject(sub.Pairs[0].Value, false)
		omap.set(key, value)
	}
}

func (c *safeConstructor) fillPairs(n *ry.Node, pairs *pyList) {
	const ctx = "while constructing pairs"
	if n.Kind != ry.SequenceNode {
		panic(constructorError(ctx, n.Start, "expected a sequence, but found "+n.Kind.ID(), n.Start))
	}
	for _, sub := range n.Items {
		if sub.Kind != ry.MappingNode {
			panic(constructorError(ctx, n.Start, "expected a mapping of length 1, but found "+sub.Kind.ID(), sub.Start))
		}
		if len(sub.Pairs) != 1 {
			panic(constructorError(ctx, n.Start, fmt.Sprintf("expected a single mapping item, but found %d items", len(sub.Pairs)), sub.Start))
		}
		key := c.constructObject(sub.Pairs[0].Key, false)
		value := c.constructObject(sub.Pairs[0].Value, false)
		pairs.items = append(pairs.items, &pyTuple{items: []any{key, value}})
	}
}

// Go value of a constructed Python object, shared and recursive containers kept shared
func toGoValue(v any, memo map[any]any) any {
	switch x := v.(type) {
	case *big.Int:
		return pyint.Native(x)
	case *pyNaN:
		if x.neg {
			return math.Copysign(math.NaN(), -1)
		}
		return math.NaN()
	case *pyTime:
		if got, ok := memo[x]; ok {
			return got
		}
		var out any = x.v
		if x.shared {
			switch t := x.v.(type) {
			case pydatetime.Date:
				out = &t
			case pydatetime.DateTime:
				out = &t
			}
		}
		memo[x] = out
		return out
	case *pyList:
		if got, ok := memo[x]; ok {
			return got
		}
		out := make([]any, len(x.items))
		memo[x] = out
		for i, it := range x.items {
			out[i] = toGoValue(it, memo)
		}
		return out
	case *pyTuple:
		if got, ok := memo[x]; ok {
			return got
		}
		out := make(pyrepr.Tuple, len(x.items))
		memo[x] = out
		for i, it := range x.items {
			out[i] = toGoValue(it, memo)
		}
		return out
	case *pySet:
		if got, ok := memo[x]; ok {
			return got
		}
		out := &YAMLSet{Items: make([]any, len(x.keys))}
		memo[x] = out
		for i, it := range x.keys {
			out.Items[i] = toGoValue(it, memo)
		}
		return out
	case *pyDict:
		if got, ok := memo[x]; ok {
			return got
		}
		out := make(map[string]any, len(x.keys))
		memo[x] = out
		order := make([]string, 0, len(x.keys))
		for i, k := range x.keys {
			key, isStr := k.(string)
			gk := toGoValue(k, memo)
			if !isStr {
				key = PyKeyString(gk)
				SetMappingPyKey(out, key, gk)
			}
			order = append(order, key)
			out[key] = toGoValue(x.vals[i], memo)
		}
		SetMappingKeys(out, order)
		if x.kind == dictOrdered {
			SetMappingClass(out, "ordereddict")
		}
		return out
	case *rtBool:
		return x.b
	case *rtStamp:
		return x.v
	case *rtTagged:
		return x.value
	}
	return v
}
