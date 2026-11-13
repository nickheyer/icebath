package config

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const mergeTag = "!!merge"

// Keys of a mapping the schema does not name, values named keys could not take, and the file key order
type rest struct {
	pairs []*yaml.Node
	held  []heldValue
	order []string
	entry bool
}

// Value under a named key that its field cannot represent, written back while the field keeps its decoded value
type heldValue struct {
	key  string
	node *yaml.Node
	snap []byte
}

// Adds a key, merging a mapping value into an earlier mapping under the same key
func (r *rest) set(k, v *yaml.Node) {
	v = plainCopy(v)
	for i := 0; i+1 < len(r.pairs); i += 2 {
		if r.pairs[i].Value == k.Value {
			r.pairs[i+1] = mergeNode(r.pairs[i+1], v)
			return
		}
	}
	r.pairs = append(r.pairs, plainCopy(k), v)
}

// Keeps the file value of a named key until its field changes from the value decoded alongside it
func (r *rest) hold(key string, n *yaml.Node, snap []byte) {
	r.release(key)
	r.held = append(r.held, heldValue{key: key, node: plainCopy(n), snap: snap})
}

// Drops the held value of a named key
func (r *rest) release(key string) {
	r.held = slices.DeleteFunc(r.held, func(h heldValue) bool { return h.key == key })
}

// Held value of a named key
func (r rest) heldValue(key string) (heldValue, bool) {
	for _, h := range r.held {
		if h.key == key {
			return h, true
		}
	}
	return heldValue{}, false
}

// Records the file key order writing back needs, all keys of a new entry, or the keys a section gained
func (r *rest) adoptOrder(file, added, fields []string, fresh bool) {
	if fresh || r.entry {
		r.entry = true
		r.order = mergeOrder(r.order, file)
		if slices.Equal(r.order, canonicalOrder(r.order, fields, r.names())) {
			r.order, r.entry = nil, false
		}
		return
	}
	r.order = mergeOrder(r.order, added)
	if slices.Equal(r.order, r.names()) {
		r.order = nil
	}
}

// Order a writer produces for keys without a recorded order, fields in declaration order then unknown keys
func canonicalOrder(keys, fields, unknown []string) []string {
	var out []string
	for _, f := range fields {
		if slices.Contains(keys, f) {
			out = append(out, f)
		}
	}
	for _, k := range unknown {
		if slices.Contains(keys, k) {
			out = append(out, k)
		}
	}
	return out
}

// Earlier order extended with the keys of a later layer it does not name yet
func mergeOrder(base, more []string) []string {
	out := slices.Clone(base)
	for _, k := range more {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// Kept keys in file order
func (r rest) names() []string {
	var out []string
	for i := 0; i+1 < len(r.pairs); i += 2 {
		out = append(out, r.pairs[i].Value)
	}
	return out
}

// Value under a kept key, nil when the file did not hold it
func (r rest) get(key string) *yaml.Node {
	for i := 0; i+1 < len(r.pairs); i += 2 {
		if r.pairs[i].Value == key {
			return r.pairs[i+1]
		}
	}
	return nil
}

// Copy of the value under a kept key, nil when the file did not hold it
func (r rest) copyOf(key string) *yaml.Node {
	if n := r.get(key); n != nil {
		return plainCopy(n)
	}
	return nil
}

// Kept key and value pairs in the form a writer emits them
func (r rest) emit() []*yaml.Node {
	out := make([]*yaml.Node, 0, len(r.pairs))
	for i := 0; i+1 < len(r.pairs); i += 2 {
		out = append(out, plainCopy(r.pairs[i]), emitted(r.pairs[i+1]))
	}
	return out
}

// Scalar a section was written as in place of its mapping, standing for its enabled switch
type bareForm struct {
	node *yaml.Node
	snap []byte
}

// Decodes a section that may be written as one scalar, apply reading that scalar into the section
func decodeSwitchable(n *yaml.Node, v any, r *rest, b *bareForm, apply func(scalar *yaml.Node)) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n.Kind == yaml.ScalarNode && !isNull(n) {
		apply(n)
		*b = bareForm{node: plainCopy(n), snap: snapshot(reflect.ValueOf(v).Elem())}
		return nil
	}
	*b = bareForm{}
	return decodeMapping(n, v, r)
}

// Writes the scalar a section was read from while the section is unchanged since, the mapping otherwise
func encodeSwitchable(v any, r rest, b bareForm) (any, error) {
	if b.node != nil && bytes.Equal(snapshot(reflect.ValueOf(v)), b.snap) {
		return emitted(b.node), nil
	}
	return encodeMapping(v, r)
}

// Value merged over an earlier one, where a null over a mapping keeps the mapping
func mergeNode(dst, src *yaml.Node) *yaml.Node {
	if dst.Kind != yaml.MappingNode {
		return src
	}
	if isNull(src) {
		return dst
	}
	if src.Kind != yaml.MappingNode {
		return src
	}
	out := &rest{pairs: slices.Clone(dst.Content)}
	for _, p := range mappingPairs(src) {
		out.set(p.key, p.value)
	}
	merged := *dst
	merged.Content = out.pairs
	return &merged
}

// Reports an explicit or empty null scalar
func isNull(n *yaml.Node) bool {
	n = resolveAlias(n)
	return n == nil || (n.Kind == yaml.ScalarNode && n.ShortTag() == tagNull)
}

// Target node at the end of an alias chain
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// Deep copy with aliases replaced, anchors and comments dropped and merge keys folded in, safe to store and encode
func plainCopy(n *yaml.Node) *yaml.Node {
	n = resolveAlias(n)
	if n == nil {
		return nullNode()
	}
	c := *n
	c.Anchor, c.Alias = "", nil
	c.Line, c.Column = 0, 0
	c.HeadComment, c.LineComment, c.FootComment = "", "", ""
	switch {
	case n.Kind == yaml.MappingNode:
		pairs := mappingPairs(n)
		c.Content = make([]*yaml.Node, 0, 2*len(pairs))
		for _, p := range pairs {
			c.Content = append(c.Content, plainCopy(p.key), plainCopy(p.value))
		}
	case len(n.Content) > 0:
		c.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			c.Content[i] = plainCopy(child)
		}
	}
	return &c
}

// Errors for every key written twice in one mapping anywhere below n, each at its dotted path
func duplicateKeys(n *yaml.Node, path string) []error {
	if n == nil {
		return nil
	}
	join := func(k string) string {
		if path == "" || strings.HasPrefix(k, "[") {
			return path + k
		}
		return path + "." + k
	}
	var errs []error
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			errs = append(errs, duplicateKeys(c, path)...)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			errs = append(errs, duplicateKeys(c, join("["+strconv.Itoa(i)+"]"))...)
		}
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind == yaml.ScalarNode && k.ShortTag() == mergeTag {
				continue
			}
			if seen[k.Value] {
				errs = append(errs, &FieldError{Path: join(k.Value), Err: ErrDuplicateKey})
			}
			seen[k.Value] = true
			errs = append(errs, duplicateKeys(n.Content[i+1], join(k.Value))...)
		}
	}
	return errs
}

type pair struct {
	key, value *yaml.Node
}

// Key and value pairs of a mapping with merge keys expanded and own keys overriding merged ones
func mappingPairs(n *yaml.Node) []pair {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var merged, own []pair
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.ShortTag() == mergeTag {
			v = resolveAlias(v)
			switch v.Kind {
			case yaml.MappingNode:
				merged = overlayPairs(merged, mappingPairs(v))
			case yaml.SequenceNode:
				for j := len(v.Content) - 1; j >= 0; j-- {
					merged = overlayPairs(merged, mappingPairs(v.Content[j]))
				}
			}
			continue
		}
		own = append(own, pair{k, v})
	}
	return overlayPairs(merged, own)
}

// Pairs of base with each later pair replacing the base pair of the same key or appended
func overlayPairs(base, over []pair) []pair {
	out := slices.Clone(base)
	for _, p := range over {
		i := slices.IndexFunc(out, func(q pair) bool { return q.key.Value == p.key.Value })
		if i >= 0 {
			out[i] = p
			continue
		}
		out = append(out, p)
	}
	return out
}

// Mapping whose keys users or plugins choose, kept in file order and merged into existing entries on decode
type Map[V any] struct {
	keys []string
	vals map[string]V
	held map[string]*yaml.Node
}

// Builds a mapping from keys and values given in order
func MapOf[V any](keys []string, vals []V) Map[V] {
	var m Map[V]
	for i, k := range keys {
		m.Set(k, vals[i])
	}
	return m
}

// Entry under k and whether it exists with a value of the entry type
func (m Map[V]) Get(k string) (V, bool) {
	v, ok := m.vals[k]
	return v, ok
}

// Stores v under k, keeping the position of an existing key
func (m *Map[V]) Set(k string, v V) {
	if m.vals == nil {
		m.vals = map[string]V{}
	}
	if !slices.Contains(m.keys, k) {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
	delete(m.held, k)
}

// Removes the entry under k
func (m *Map[V]) Delete(k string) {
	delete(m.vals, k)
	delete(m.held, k)
	m.keys = slices.DeleteFunc(m.keys, func(s string) bool { return s == k })
}

// Names of the entries holding a value of the entry type, in file order
func (m Map[V]) Keys() []string {
	var out []string
	for _, k := range m.keys {
		if _, ok := m.vals[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// Number of entries holding a value of the entry type
func (m Map[V]) Len() int {
	return len(m.vals)
}

// Names whose file value the entry type cannot represent, such as null or a wrong shape, in file order
func (m Map[V]) Unreadable() []string {
	var out []string
	for _, k := range m.keys {
		if _, ok := m.held[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// Reports an empty mapping so omitempty drops it
func (m Map[V]) IsZero() bool {
	return len(m.keys) == 0
}

// Merges each entry over an existing entry of the same name or appends it, null leaving a mapping entry
func (m *Map[V]) UnmarshalYAML(n *yaml.Node) error {
	return m.decodeCoerced(n, coerceDefault)
}

// Merges the entries with each value read under the coercion of the field holding the mapping
func (m *Map[V]) decodeCoerced(n *yaml.Node, c coercion) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if isNull(n) {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return shapeError(n, yaml.MappingNode)
	}
	var errs []error
	for _, p := range mappingPairs(n) {
		k := p.key.Value
		v, ok := m.vals[k]
		rv := reflect.ValueOf(&v).Elem()
		if ok && isNull(p.value) && isMappingValue(rv) {
			continue
		}
		hold, err := decodeValue(p.value, rv, c)
		if isNull(p.value) && !isNullHolder(rv) {
			hold = true
		}
		errs = append(errs, underKey(k, err))
		if hold {
			m.keep(k, p.value)
			continue
		}
		m.Set(k, v)
	}
	return errors.Join(errs...)
}

// Reports an entry type that keeps an explicit null as its own value
func isNullHolder(rv reflect.Value) bool {
	if rv.Kind() == reflect.Pointer {
		return true
	}
	_, ok := rv.Addr().Interface().(nullHolder)
	return ok
}

// Keeps a value the entry type cannot represent for writing back, the entry unreadable until replaced
func (m *Map[V]) keep(k string, n *yaml.Node) {
	if m.held == nil {
		m.held = map[string]*yaml.Node{}
	}
	if !slices.Contains(m.keys, k) {
		m.keys = append(m.keys, k)
	}
	delete(m.vals, k)
	m.held[k] = plainCopy(n)
}

// Writes the entries in order, unreadable ones exactly as read
func (m Map[V]) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	for _, k := range m.keys {
		if h, ok := m.held[k]; ok {
			n.Content = append(n.Content, keyNode(k), emitted(h))
			continue
		}
		v, err := encodeValue(reflect.ValueOf(m.vals[k]))
		if err != nil {
			return nil, err
		}
		n.Content = append(n.Content, keyNode(k), v)
	}
	return n, nil
}

// Mapping key node, its text read back as written since keys are never retyped
func keyNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagStr, Value: s}
}

// String scalar node, quoted wherever YAML 1.1 would read its text as another type
func strNode(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: tagStr, Value: s}
	if got, _ := resolvePlain(s); got != tagStr {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

// Text of a scalar node, empty for null and for collections
func scalarText(n *yaml.Node) string {
	n = resolveAlias(n)
	if n == nil || n.Kind != yaml.ScalarNode || n.ShortTag() == tagNull {
		return ""
	}
	return n.Value
}
