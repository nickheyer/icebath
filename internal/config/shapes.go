package config

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Key accepting several scalar types, kept with its YAML 1.1 type and quoting so saving writes back the same
type Scalar struct {
	tag   string
	value string
	style yaml.Style
}

// YAML type of a scalar value
type ScalarKind int

// YAML scalar types
const (
	KindNull ScalarKind = iota
	KindBool
	KindInt
	KindFloat
	KindString
)

// Builds a string scalar
func Text(s string) Scalar {
	return Scalar{tag: tagStr, value: s}
}

// Builds a boolean scalar
func Bool(v bool) Scalar {
	return Scalar{tag: tagBool, value: strconv.FormatBool(v)}
}

// Builds an integer scalar
func Int(v int) Scalar {
	return Scalar{tag: tagInt, value: strconv.Itoa(v)}
}

// Builds a floating point scalar
func Float(v float64) Scalar {
	return Scalar{tag: tagFloat, value: floatText(v)}
}

// Keeps an explicit null as a null value
func (Scalar) holdsNull() {}

// Keeps any scalar with its YAML 1.1 type, null included
func (s *Scalar) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n == nil {
		*s = Scalar{tag: tagNull, value: "null"}
		return nil
	}
	if n.Kind != yaml.ScalarNode {
		return shapeError(n, yaml.ScalarNode)
	}
	*s = Scalar{tag: n.ShortTag(), value: n.Value, style: chosenStyle(n)}
	return nil
}

// Quoting or block style the user picked, none when the text needs quoting anyway to keep its type
func chosenStyle(n *yaml.Node) yaml.Style {
	style := n.Style & (yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle)
	if got, _ := resolvePlain(n.Value); style != 0 && (got != tagStr || n.Value == "") {
		return 0
	}
	return style
}

// Writes the value with the type and quoting it was read with
func (s Scalar) MarshalYAML() (any, error) {
	if s.tag == "" {
		return nil, nil
	}
	return emitted(&yaml.Node{Kind: yaml.ScalarNode, Tag: s.tag, Value: s.value, Style: s.style}), nil
}

// Reports an unset value so omitempty drops it
func (s Scalar) IsZero() bool {
	return s.tag == ""
}

// YAML type of the value
func (s Scalar) Kind() ScalarKind {
	switch s.tag {
	case tagBool:
		return KindBool
	case tagInt:
		return KindInt
	case tagFloat:
		return KindFloat
	case "", tagNull:
		return KindNull
	}
	return KindString
}

// Value as written in the file, booleans as true or false
func (s Scalar) String() string {
	if s.Kind() == KindNull {
		return ""
	}
	return s.value
}

// Boolean value, ok false for any other type
func (s Scalar) AsBool() (v, ok bool) {
	if s.Kind() != KindBool {
		return false, false
	}
	return s.value == "true", true
}

// Integer value or a whole float, ok false for any other type
func (s Scalar) AsInt() (int, bool) {
	switch s.Kind() {
	case KindInt:
		v, ok := constructInt(s.value)
		if !ok || !v.IsInt64() || v.Int64() != int64(int(v.Int64())) {
			return 0, false
		}
		return int(v.Int64()), true
	case KindFloat:
		f, ok := s.AsFloat()
		if ok && f == math.Trunc(f) && math.Abs(f) < math.MaxInt {
			return int(f), true
		}
	}
	return 0, false
}

// Number of either numeric type, ok false for any other type
func (s Scalar) AsFloat() (float64, bool) {
	switch s.Kind() {
	case KindInt:
		v, ok := constructInt(s.value)
		if !ok {
			return 0, false
		}
		f, _ := strconv.ParseFloat(v.String(), 64)
		return f, true
	case KindFloat:
		return constructFloat(s.value)
	}
	return 0, false
}

// Reports a string equal to word, ignoring case and surrounding space
func (s Scalar) Is(word string) bool {
	return s.Kind() == KindString && strings.EqualFold(strings.TrimSpace(s.value), word)
}

// Reads booleans and the words true yes on 1 or false no off 0, ok false for anything else
func (s Scalar) Truth() (on, ok bool) {
	if v, isBool := s.AsBool(); isBool {
		return v, true
	}
	switch strings.ToLower(strings.TrimSpace(s.String())) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}

// One string or a list of strings, written back in the form read with numbers kept as numbers
type StringList struct {
	Text   string
	Items  []string
	IsList bool
	typed  map[int]string
}

// Builds the list form
func ListOf(items ...string) StringList {
	if items == nil {
		items = []string{}
	}
	return StringList{Items: items, IsList: true}
}

// Accepts a scalar or a list of scalars, remembering which ones the file wrote as another type than text
func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	switch n.Kind {
	case yaml.ScalarNode:
		*s = StringList{Text: n.Value, typed: typedAt(nil, 0, n)}
		return nil
	case yaml.SequenceNode:
		items, typed, err := scalarItems(n)
		if err != nil {
			return err
		}
		*s = StringList{Items: items, IsList: true, typed: typed}
		return nil
	}
	return shapeError(n, yaml.SequenceNode)
}

// Texts of a list of scalars with the positions written as another type than text, failing on a nested value
func scalarItems(n *yaml.Node) ([]string, map[int]string, error) {
	items := []string{}
	var typed map[int]string
	for i, it := range n.Content {
		if it.Kind != yaml.ScalarNode {
			return nil, nil, rootError(fmt.Errorf("%w: item %d is %s where a single value belongs", ErrShape, i, kindWord(it.Kind)))
		}
		items = append(items, scalarText(it))
		typed = typedAt(typed, i, it)
	}
	return items, typed, nil
}

// Typed positions extended with i when the scalar is not text
func typedAt(m map[int]string, i int, n *yaml.Node) map[int]string {
	if tag := n.ShortTag(); tag != tagStr {
		if m == nil {
			m = map[int]string{}
		}
		m[i] = tag
	}
	return m
}

// Writes the form that was read, values still reading as the type they were read with keeping that type
func (s StringList) MarshalYAML() (any, error) {
	item := func(i int, v string) *yaml.Node {
		if tag, ok := s.typed[i]; ok {
			if got, text := resolvePlain(v); got == tag && text == v {
				return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
			}
		}
		return strNode(v)
	}
	if !s.IsList {
		return item(0, s.Text), nil
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq}
	for i, it := range s.Items {
		seq.Content = append(seq.Content, item(i, it))
	}
	return seq, nil
}

// Reports an empty value so omitempty drops it
func (s StringList) IsZero() bool {
	return s.Text == "" && len(s.Items) == 0 && !s.IsList
}

// Items the value lists, a text holding a bracketed list read as that list, a non-text scalar listing nothing
func (s StringList) Names() []string {
	if s.IsList {
		return slices.Clone(s.Items)
	}
	if _, typed := s.typed[0]; typed {
		return nil
	}
	if text := strings.TrimSpace(s.Text); strings.HasPrefix(text, "[") {
		var doc yaml.Node
		if yaml.Unmarshal([]byte(text), &doc) == nil && len(doc.Content) == 1 && doc.Content[0].Kind == yaml.SequenceNode {
			if items, _, err := scalarItems(doc.Content[0]); err == nil {
				return items
			}
		}
	}
	return []string{s.Text}
}

// List items, or the string cut at sep, with blanks dropped
func (s StringList) Split(sep string) []string {
	var out []string
	add := func(it string) {
		if it = strings.TrimSpace(it); it != "" {
			out = append(out, it)
		}
	}
	if s.IsList {
		for _, it := range s.Items {
			add(it)
		}
		return out
	}
	for part := range strings.SplitSeq(s.Text, sep) {
		add(part)
	}
	return out
}

// Prompt gate that is a word such as auto, a forced boolean, or a list of model name substrings
type ModelGate struct {
	Bool     bool
	Word     string
	Patterns []string
	Shape    GateShape
	list     *yaml.Node
}

// Form a gate value took in the file
type GateShape int

// Forms of a gate value
const (
	GateWord GateShape = iota
	GateBool
	GateList
)

// Default gate word
const GateAuto = "auto"

// Gate set to the default word
func AutoGate() ModelGate {
	return ModelGate{Word: GateAuto}
}

// Accepts a boolean, a word, or a list of model name substrings, items of other types kept but never matched
func (g *ModelGate) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	switch {
	case n.Kind == yaml.SequenceNode:
		out := ModelGate{Patterns: gatePatterns(n), Shape: GateList}
		if len(out.Patterns) != len(n.Content) {
			out.list = plainCopy(n)
		}
		*g = out
		return nil
	case n.Kind == yaml.ScalarNode && n.ShortTag() == tagBool:
		*g = ModelGate{Bool: n.Value == "true", Shape: GateBool}
		return nil
	case n.Kind == yaml.ScalarNode:
		*g = ModelGate{Word: n.Value, Shape: GateWord}
		return nil
	}
	return shapeError(n, yaml.SequenceNode)
}

// Writes the form that was read
func (g ModelGate) MarshalYAML() (any, error) {
	switch g.Shape {
	case GateBool:
		return g.Bool, nil
	case GateList:
		if g.list != nil && slices.Equal(g.Patterns, gatePatterns(g.list)) {
			return emitted(g.list), nil
		}
		if g.Patterns == nil {
			return []string{}, nil
		}
		return g.Patterns, nil
	}
	return strNode(g.Word), nil
}

// String items of a gate list, the only items a gate matches model names against
func gatePatterns(n *yaml.Node) []string {
	out := []string{}
	for _, it := range n.Content {
		if it.Kind == yaml.ScalarNode && it.ShortTag() == tagStr {
			out = append(out, it.Value)
		}
	}
	return out
}

// Value of any shape kept exactly as written, a mapping merged over an earlier mapping on decode
type Raw struct {
	node *yaml.Node
}

// Value holding a copy of n read under YAML 1.1 typing
func RawOf(n *yaml.Node) (Raw, error) {
	r, err := resolved(n)
	if err != nil {
		return Raw{}, err
	}
	return Raw{node: plainCopy(r)}, nil
}

// Keeps an explicit null as a null value
func (Raw) holdsNull() {}

// Keeps the value, merging a mapping into the mapping already held
func (r *Raw) UnmarshalYAML(n *yaml.Node) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if r.node != nil {
		r.node = mergeNode(r.node, plainCopy(n))
		return nil
	}
	r.node = plainCopy(n)
	return nil
}

// Writes the value exactly as read
func (r Raw) MarshalYAML() (any, error) {
	if r.node == nil {
		return nil, nil
	}
	return emitted(r.node), nil
}

// Reports an unset value so omitempty drops it
func (r Raw) IsZero() bool {
	return r.node == nil
}

// Copy of the held node under YAML 1.1 typing, nil when unset
func (r Raw) Node() *yaml.Node {
	if r.node == nil {
		return nil
	}
	return plainCopy(r.node)
}

// Decodes the held value into the value v points at with the schema coercions, leaving it untouched when unset
func (r Raw) Decode(v any) error {
	if r.node == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return rootError(ErrScalar)
	}
	_, err := decodeValue(r.node, rv.Elem(), coerceDefault)
	return err
}
