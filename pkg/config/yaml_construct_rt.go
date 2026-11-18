package config

import (
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	ry "github.com/nickheyer/icebath/pkg/internal/ruamelyaml"
)

// Round trip objects the RoundTripConstructor builds beyond the safe ones
type (
	// TaggedScalar of an unknown tag, hashed by identity
	rtTagged struct {
		value string
		style rune
		tag   string
	}
	// ScalarBoolean of an anchored bool, an int subclass
	rtBool struct{ b bool }
	// TimeStamp of a datetime written with a T separator or an offset
	rtStamp struct {
		v pydatetime.DateTime
		t bool
	}
	// Comment slots the round trip containers attach per key, the four slot lists of CommentedBase.ca.items
	rtComments struct {
		items map[string][]any
	}
)

// Python value a round trip document loads to under RoundTripConstructor, its YAMLError or other exception
func roundtripLoadValue(in ry.Input) (any, error) {
	doc, err := ry.ComposePure(in, true)
	if err != nil {
		return nil, &YAMLError{err}
	}
	emitYAMLWarnings(doc.Warnings)
	if doc.Root == nil {
		return nil, nil
	}
	value, warnings, cerr := constructDocumentMode(doc, true)
	emitYAMLWarnings(warnings)
	if cerr != nil {
		return nil, &YAMLError{cerr}
	}
	return value, nil
}

func rtTypeName(v any) string {
	switch x := v.(type) {
	case *pyList:
		return "CommentedSeq"
	case *pySet:
		return "CommentedSet"
	case *pyDict:
		if x.kind == dictCommentedOrdered {
			return "CommentedOrderedMap"
		}
		return "CommentedMap"
	}
	return pyObjType(v)
}

// Python repr of a TimeStamp, the datetime repr under its own class name
func (s *rtStamp) repr() string {
	return "TimeStamp" + strings.TrimPrefix(s.v.PyRepr(), "datetime.datetime")
}

// Python str of a TimeStamp, isoformat with T or a space
func (s *rtStamp) str() string {
	if !s.t {
		return s.v.PyStr()
	}
	return strings.Replace(s.v.PyStr(), " ", "T", 1)
}

func (t *rtTagged) repr() string {
	style := "None"
	if t.style != 0 {
		style = pyrepr.StrRepr(string(t.style))
	}
	return "TaggedScalar(value=" + pyrepr.StrRepr(t.value) + ", style=" + style + ", tag=Tag(" + pyrepr.StrRepr(t.tag) + "))"
}

// Round trip dispatch of RoundTripConstructor, unknown tags kept as tagged objects
func (c *safeConstructor) constructNonRecursiveRT(n *ry.Node) any {
	tag := n.Tag()
	switch tag {
	case yamlTagPrefix + "null":
		c.constructScalarRT(n)
		return nil
	case yamlTagPrefix + "bool":
		value := c.constructScalar(n)
		low := pystr.Lower(value)
		b, ok := boolValues[low]
		if !ok {
			panic(pyErr("KeyError", pyrepr.StrRepr(low)))
		}
		if n.Anchor != nil {
			return &rtBool{b}
		}
		return b
	case yamlTagPrefix + "int":
		return c.constructIntRT(n)
	case yamlTagPrefix + "float":
		return c.constructFloatRT(n)
	case yamlTagPrefix + "binary":
		return c.constructBinary(n)
	case yamlTagPrefix + "timestamp":
		return c.constructTimestampRT(n)
	case yamlTagPrefix + "omap":
		omap := newPyDict()
		omap.kind = dictCommentedOrdered
		return c.generator(omap, func() { c.fillOmapRT(n, omap) })
	case yamlTagPrefix + "pairs":
		pairs := &pyList{}
		return c.generator(pairs, func() { c.fillPairs(n, pairs) })
	case yamlTagPrefix + "set":
		set := &pySet{index: map[string]int{}, rt: true}
		return c.generator(set, func() { c.constructSetting(n, set) })
	case yamlTagPrefix + "str":
		if n.Handle != nil && *n.Handle != "" {
			return c.constructUnknown(n)
		}
		return c.constructScalarRT(n)
	case yamlTagPrefix + "seq":
		list := &pyList{}
		return c.generator(list, func() { list.items = append(list.items, c.constructRTSequence(n, false)...) })
	case yamlTagPrefix + "map":
		m := newPyDict()
		m.kind = dictCommented
		return c.generator(m, func() { c.constructMappingRT(n, m, true) })
	}
	return c.constructUnknown(n)
}

// Scalar text of a scalar node, any other node a ConstructorError
func (c *safeConstructor) constructScalarRT(n *ry.Node) string {
	if n.Kind != ry.ScalarNode {
		panic(constructorError("", nil, "expected a scalar node, but found "+n.Kind.ID(), n.Start))
	}
	if n.Style == '>' {
		return strings.ReplaceAll(n.Value, "\a", "")
	}
	return n.Value
}

// Tagged value of an unknown tag, any failure building it turned into the undetermined constructor error
func (c *safeConstructor) constructUnknown(n *ry.Node) any {
	refuse := func() {
		panic(constructorError("", nil, "could not determine a constructor for the tag "+pyrepr.StrRepr(n.Tag()), n.Start))
	}
	guard := func(f func()) {
		defer func() {
			if r := recover(); r != nil {
				switch r.(type) {
				case *ry.MarkedError, *ry.PyException:
					refuse()
				}
				panic(r)
			}
		}()
		f()
	}
	switch n.Kind {
	case ry.MappingNode:
		m := newPyDict()
		m.kind = dictCommented
		return c.generator(m, func() { guard(func() { c.constructMappingRT(n, m, false) }) })
	case ry.ScalarNode:
		var t *rtTagged
		guard(func() { t = &rtTagged{value: c.constructScalarRT(n), style: n.Style, tag: n.Tag()} })
		return t
	case ry.SequenceNode:
		list := &pyList{}
		return c.generator(list, func() { guard(func() { list.items = append(list.items, c.constructSequence(n, false)...) }) })
	}
	refuse()
	return nil
}

// Python int() raising an IndexError for an empty string index the way value_s[0] does
func strIndex(s string, i int) rune {
	r := pystr.Runes(s)
	if i >= len(r) {
		panic(pyErr("IndexError", "string index out of range"))
	}
	return r[i]
}

func (c *safeConstructor) constructIntRT(n *ry.Node) any {
	valueSU := c.constructScalarRT(n)
	valueS := strings.ReplaceAll(valueSU, "_", "")
	sign := big.NewInt(1)
	if first := strIndex(valueS, 0); first == '+' || first == '-' {
		if first == '-' {
			sign = big.NewInt(-1)
		}
		valueS = string(pystr.Runes(valueS)[1:])
	}
	mul := func(v *big.Int) any { return new(big.Int).Mul(sign, v) }
	switch {
	case strings.HasPrefix(valueS, "0b"):
		return mul(yamlIntOf(valueS[2:], 2))
	case strings.HasPrefix(valueS, "0x"):
		return mul(yamlIntOf(valueS[2:], 16))
	case strings.HasPrefix(valueS, "0o"):
		return mul(yamlIntOf(valueS[2:], 8))
	case len(pystr.Runes(valueS)) > 1 && strIndex(valueS, 0) == '0':
		return mul(yamlIntOf(valueS, 8))
	case strings.Contains(valueS, ":"):
		parts := strings.Split(valueS, ":")
		digits := make([]*big.Int, len(parts))
		for i, part := range parts {
			digits[i] = yamlIntOf(part, 10)
		}
		value, base := new(big.Int), big.NewInt(1)
		for i := len(digits) - 1; i >= 0; i-- {
			value.Add(value, new(big.Int).Mul(digits[i], base))
			base.Mul(base, big.NewInt(60))
		}
		return value.Mul(value, sign)
	}
	return mul(yamlIntOf(valueS, 10))
}

func (c *safeConstructor) constructFloatRT(n *ry.Node) any {
	valueSO := c.constructScalarRT(n)
	valueS := pystr.Lower(strings.ReplaceAll(valueSO, "_", ""))
	sign := 1.0
	if strIndex(valueS, 0) == '-' {
		sign = -1
	}
	if first := strIndex(valueS, 0); first == '+' || first == '-' {
		valueS = string(pystr.Runes(valueS)[1:])
	}
	switch {
	case valueS == ".inf":
		return sign * math.Inf(1)
	case valueS == ".nan":
		return yamlNaNValue
	case strings.Contains(valueS, ":"):
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
	if strings.Contains(valueS, "e") {
		parts := strings.Split(valueSO, "e")
		if len(parts) != 2 {
			parts = strings.Split(valueSO, "E")
			if len(parts) != 2 {
				if len(parts) > 2 {
					panic(pyErr("ValueError", fmt.Sprintf("too many values to unpack (expected 2, got %d)", len(parts))))
				}
				panic(pyErr("ValueError", fmt.Sprintf("not enough values to unpack (expected 2, got %d)", len(parts))))
			}
		}
		if !strings.Contains(parts[0], ".") {
			c.warnings = append(c.warnings, ry.Warning{Category: "MantissaNoDotYAML1_1Warning",
				Message: fmt.Sprintf(mantissaWarning, valueSO, n.Start.Line, n.Start.Column), Site: rtMantissaSite})
		}
		strIndex(parts[1], 0)
	}
	return floatObj(sign * pyFloatText(valueS))
}

// Module line and source of the MantissaNoDotYAML1_1Warning each constructor raises
var (
	safeMantissaSite = ry.WarningSite{Where: "ruamel/yaml/constructor.py:508", Source: "warnings.warn(MantissaNoDotYAML1_1Warning(node, value_so), stacklevel=1)", Once: true}
	rtMantissaSite   = ry.WarningSite{Where: "ruamel/yaml/constructor.py:1148", Source: "warnings.warn(MantissaNoDotYAML1_1Warning(node, value_so), stacklevel=1)", Once: true}
)

func (c *safeConstructor) constructTimestampRT(n *ry.Node) any {
	v := c.constructTimestamp(n).(*pyTime)
	full := timestampRegexp.FindStringSubmatch(n.Value)
	if full == nil {
		full = timestampRegexp.FindStringSubmatch(n.Value[:len(n.Value)-1])
	}
	group := func(name string) string { return full[timestampRegexp.SubexpIndex(name)] }
	dt, ok := v.v.(pydatetime.DateTime)
	if !ok || (group("t") == "" && group("tz_sign") == "" && group("tz_hour") == "" && group("tz_minute") == "") {
		return v
	}
	return &rtStamp{v: dt, t: group("t") != ""}
}

// Appends key comments the way yaml_key_comment_extend does, slot one and zero of the item
func (rc *rtComments) keyExtend(key string, comment []any) {
	r := rc.items[key]
	if r == nil {
		r = []any{nil, nil, nil, nil}
		rc.items[key] = r
	}
	if r[1] == nil {
		assertCommentList(comment[1])
		r[1] = comment[1]
	} else {
		r[1] = extendList(r[1], comment[0])
	}
	r[0] = comment[0]
}

// Appends value comments the way yaml_value_comment_extend does, slot three and two of the item
func (rc *rtComments) valueExtend(key string, comment []any) {
	r := rc.items[key]
	if r == nil {
		r = []any{nil, nil, nil, nil}
		rc.items[key] = r
	}
	if r[3] == nil {
		assertCommentList(comment[1])
		r[3] = comment[1]
	} else {
		r[3] = extendList(r[3], comment[0])
	}
	r[2] = comment[0]
}

// The isinstance list assertion the comment extenders make on a pre comment slot
func assertCommentList(v any) {
	if v == nil {
		return
	}
	if _, ok := v.([]any); !ok {
		panic(pyErr("AssertionError", ""))
	}
}

// list.extend of a comment slot, the TypeError Python raises for a non iterable argument
func extendList(list any, items any) any {
	l, _ := list.([]any)
	switch x := items.(type) {
	case []any:
		return append(l, x...)
	case string:
		for _, r := range x {
			l = append(l, string(r))
		}
		return l
	case nil:
		panic(pyErr("TypeError", "'NoneType' object is not iterable"))
	}
	panic(pyErr("TypeError", "'CommentToken' object is not iterable"))
}

// Elements of a round trip sequence with each child comment moved onto the sequence
func (c *safeConstructor) constructRTSequence(n *ry.Node, deep bool) []any {
	if n.Kind != ry.SequenceNode {
		panic(constructorError("", nil, "expected a sequence node, but found "+n.Kind.ID(), n.Start))
	}
	rc := &rtComments{items: map[string][]any{}}
	out := make([]any, 0, len(n.Items))
	for idx, child := range n.Items {
		if len(child.Comment) > 0 {
			rc.keyExtend(fmt.Sprint(idx), child.Comment)
			child.Comment = nil
		}
		out = append(out, c.constructObject(child, deep))
	}
	return out
}

// Round trip key, an unhashable sequence or mapping wrapped in its hashable key type
func (c *safeConstructor) rtKey(n *ry.Node, keyNode *ry.Node) any {
	key := c.constructObject(keyNode, true)
	switch k := key.(type) {
	case *pyList:
		return &pyTuple{items: append([]any(nil), k.items...), rtKey: true}
	case *pyDict:
		km := newPyDict()
		km.kind = dictCommentedKey
		for i, kk := range k.keys {
			km.set(kk, k.vals[i])
		}
		return km
	case *pySet:
		panic(constructorError("while constructing a mapping", n.Start, "found unhashable key", keyNode.Start))
	}
	return key
}

// Fills a CommentedMap, merges referenced and added after the own keys, duplicates refused
func (c *safeConstructor) constructMappingRT(n *ry.Node, m *pyDict, deep bool) {
	if n.Kind != ry.MappingNode {
		panic(constructorError("", nil, "expected a mapping node, but found "+n.Kind.ID(), n.Start))
	}
	merged := c.flattenMappingRT(n)
	rc := &rtComments{items: map[string][]any{}}
	var lastKey any
	lastValue := any(rtSentinel{})
	for _, p := range n.Pairs {
		key := c.rtKey(n, p.Key)
		value := c.constructObject(p.Value, deep)
		k := dictKey(key)
		if mk, dup := m.get(k); dup {
			problem := fmt.Sprintf("found duplicate key \"%s\" with value \"%s\" (original value: \"%s\")", pyObjStr(key), pyObjStr(value), pyObjStr(mk))
			panic(&ry.MarkedError{Class: "DuplicateKeyError", Context: strPtr("while constructing a mapping"), ContextMark: n.Start,
				Problem: &problem, ProblemMark: p.Key.Start,
				Note: "\n                    To suppress this check see:\n                        https://yaml.dev/doc/ruamel.yaml/api/#Duplicate_keys\n                    "})
		}
		if kc := p.Key.Comment; len(kc) > 4 && truthyComment(kc[4]) {
			popped := kc[4]
			kc = append(kc[:4:4], kc[5:]...)
			if lastValue == nil {
				kc[0] = popped
				rc.valueExtend(dictKey(lastKey), kc)
			} else {
				kc[2] = popped
				rc.keyExtend(k, kc)
			}
			p.Key.Comment = nil
		}
		if len(p.Key.Comment) > 0 {
			rc.keyExtend(k, p.Key.Comment)
		}
		if len(p.Value.Comment) > 0 {
			rc.valueExtend(k, p.Value.Comment)
		}
		m.set(key, value)
		lastKey, lastValue = key, value
	}
	for _, src := range merged {
		if sm, ok := src.(*pyDict); ok {
			for i, k := range sm.keys {
				if _, has := m.get(dictKey(k)); !has {
					m.set(k, sm.vals[i])
				}
			}
		}
	}
}

type rtSentinel struct{}

func truthyComment(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case []any:
		return len(x) > 0
	case string:
		return x != ""
	}
	return true
}

// Merge sources of a round trip mapping, each constructed deep, the merge keys removed from the node
func (c *safeConstructor) flattenMappingRT(n *ry.Node) []any {
	constructed := func(v *ry.Node) any { return c.constructObject(v, true) }
	var merged []any
	index := 0
	for index < len(n.Pairs) {
		key, value := n.Pairs[index].Key, n.Pairs[index].Value
		switch key.Tag() {
		case yamlTagPrefix + "merge":
			if len(merged) > 0 {
				panic(&ry.MarkedError{Class: "DuplicateKeyError", Context: strPtr("while constructing a mapping"), ContextMark: n.Start,
					Problem: strPtr(`found duplicate merge key "<<"`), ProblemMark: key.Start,
					Note: "                        Duplicate merge keys are never allowed, not even when\n                        `.allow_duplicate_keys` is set to True\n                        "})
			}
			n.Pairs = append(n.Pairs[:index:index], n.Pairs[index+1:]...)
			switch value.Kind {
			case ry.MappingNode:
				merged = append(merged, constructed(value))
			case ry.SequenceNode:
				constructed(value)
				for _, sub := range value.Items {
					if sub.Kind != ry.MappingNode {
						panic(constructorError("while constructing a mapping", n.Start, "expected a mapping for merging, but found "+sub.Kind.ID(), sub.Start))
					}
					merged = append(merged, constructed(sub))
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
	return merged
}

// Fills a CommentedSet from a mapping node, duplicate members refused
func (c *safeConstructor) constructSetting(n *ry.Node, set *pySet) {
	if n.Kind != ry.MappingNode {
		panic(constructorError("", nil, "expected a mapping node, but found "+n.Kind.ID(), n.Start))
	}
	rc := &rtComments{items: map[string][]any{}}
	for _, p := range n.Pairs {
		key := c.constructObject(p.Key, true)
		if l, ok := key.(*pyList); ok {
			key = &pyTuple{items: append([]any(nil), l.items...)}
		}
		switch key.(type) {
		case *pyDict, *pySet:
			panic(constructorError("while constructing a mapping", n.Start, "found unhashable key", p.Key.Start))
		}
		c.constructObject(p.Value, false)
		k := dictKey(key)
		if _, dup := set.index[k]; dup {
			problem := fmt.Sprintf("found duplicate key \"%s\"", pyObjStr(key))
			panic(&ry.MarkedError{Class: "DuplicateKeyError", Context: strPtr("while constructing a set"), ContextMark: n.Start,
				Problem: &problem, ProblemMark: p.Key.Start,
				Note: "\n                    To suppress this check see:\n                        https://yaml.dev/doc/ruamel.yaml/api/#Duplicate_keys\n                    "})
		}
		if len(p.Key.Comment) > 0 {
			rc.keyExtend(k, p.Key.Comment)
		}
		if len(p.Value.Comment) > 0 {
			rc.valueExtend(k, p.Value.Comment)
		}
		set.index[k] = len(set.keys)
		set.keys = append(set.keys, key)
	}
}

func (c *safeConstructor) fillOmapRT(n *ry.Node, omap *pyDict) {
	const ctx = "while constructing an ordered map"
	if n.Kind != ry.SequenceNode {
		panic(constructorError(ctx, n.Start, "expected a sequence, but found "+n.Kind.ID(), n.Start))
	}
	rc := &rtComments{items: map[string][]any{}}
	for _, sub := range n.Items {
		if sub.Kind != ry.MappingNode {
			panic(constructorError(ctx, n.Start, "expected a mapping of length 1, but found "+sub.Kind.ID(), sub.Start))
		}
		if len(sub.Pairs) != 1 {
			panic(constructorError(ctx, n.Start, fmt.Sprintf("expected a single mapping item, but found %d items", len(sub.Pairs)), sub.Start))
		}
		keyNode, valueNode := sub.Pairs[0].Key, sub.Pairs[0].Value
		key := c.constructObject(keyNode, false)
		k := dictKey(key)
		if _, dup := omap.get(k); dup {
			panic(pyErr("AssertionError", ""))
		}
		value := c.constructObject(valueNode, false)
		if len(keyNode.Comment) > 0 {
			rc.keyExtend(k, keyNode.Comment)
		}
		if len(sub.Comment) > 0 {
			rc.keyExtend(k, sub.Comment)
		}
		if len(valueNode.Comment) > 0 {
			rc.valueExtend(k, valueNode.Comment)
		}
		omap.set(key, value)
	}
}
