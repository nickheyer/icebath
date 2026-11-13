package config

import (
	"bytes"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Node for a Go value built directly, so the nodes values return keep their tags and styles
func encodeValue(rv reflect.Value) (*yaml.Node, error) {
	if !rv.IsValid() || ((rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) && rv.IsNil()) {
		return nullNode(), nil
	}
	if m, ok := rv.Interface().(yaml.Marshaler); ok {
		out, err := m.MarshalYAML()
		if err != nil {
			return nil, err
		}
		switch t := out.(type) {
		case *yaml.Node:
			if t == nil {
				return nullNode(), nil
			}
			return t, nil
		case yaml.Node:
			return &t, nil
		case nil:
			return nullNode(), nil
		}
		return encodeValue(reflect.ValueOf(out))
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		return encodeValue(rv.Elem())
	case reflect.Struct:
		return encodeStruct(rv, rest{})
	case reflect.Map:
		return encodeGoMap(rv)
	case reflect.Slice, reflect.Array:
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq}
		for i := range rv.Len() {
			item, err := encodeValue(rv.Index(i))
			if err != nil {
				return nil, err
			}
			seq.Content = append(seq.Content, item)
		}
		return seq, nil
	}
	return leafNode(rv.Interface())
}

// Mapping of a Go map with its keys sorted by their text, values encoded like any other field
func encodeGoMap(rv reflect.Value) (*yaml.Node, error) {
	type entry struct {
		text       string
		key, value *yaml.Node
	}
	var entries []entry
	iter := rv.MapRange()
	for iter.Next() {
		k, err := encodeValue(iter.Key())
		if err != nil {
			return nil, err
		}
		v, err := encodeValue(iter.Value())
		if err != nil {
			return nil, err
		}
		if iter.Key().Kind() == reflect.String || (k.Kind == yaml.ScalarNode && k.ShortTag() == tagStr) {
			k = keyNode(k.Value)
		}
		entries = append(entries, entry{k.Value, k, v})
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.text, b.text) })
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	for _, e := range entries {
		out.Content = append(out.Content, e.key, e.value)
	}
	return out, nil
}

// Node yaml.v3 writes for a plain Go value, strings quoted wherever YAML 1.1 reads them as another type
func leafNode(v any) (*yaml.Node, error) {
	switch f := reflect.ValueOf(v); f.Kind() {
	case reflect.Float32, reflect.Float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagFloat, Value: floatText(f.Float())}, nil
	}
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	if n.Kind == yaml.ScalarNode && n.Style&fixedStyles == 0 && n.ShortTag() == tagStr {
		if got, _ := resolvePlain(n.Value); got != tagStr {
			n.Style = yaml.DoubleQuotedStyle
		}
	}
	return &n, nil
}

// Mapping of a struct value with the kept keys of r, ordered as the reference writes the section
func encodeMapping(v any, r rest) (any, error) {
	return encodeStruct(reflect.ValueOf(v), r)
}

// Encodes the fields of a struct in declaration order, then the keys the file added, placed in file order
func encodeStruct(sv reflect.Value, r rest) (*yaml.Node, error) {
	order, refs := structFields(sv.Type())
	type entry struct {
		key, value *yaml.Node
		name       string
		fixed      bool
	}
	gained := func(name string) bool { return slices.Contains(r.order, name) }
	var entries []entry
	for _, name := range order {
		ref := refs[name]
		fv := sv.FieldByIndex(ref.index)
		if h, ok := r.heldValue(name); ok && bytes.Equal(snapshot(fv), h.snap) {
			entries = append(entries, entry{strNode(name), emitted(h.node), name, !gained(name)})
			continue
		}
		if ref.omitempty && isZero(fv) {
			continue
		}
		vn, err := encodeValue(fv)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry{strNode(name), vn, name, !gained(name)})
	}
	for i := 0; i+1 < len(r.pairs); i += 2 {
		k := r.pairs[i]
		entries = append(entries, entry{plainCopy(k), emitted(r.pairs[i+1]), k.Value, false})
	}
	rank := func(name string) int {
		if i := slices.Index(r.order, name); i >= 0 {
			return i
		}
		return len(r.order)
	}
	slices.SortStableFunc(entries, func(a, b entry) int {
		switch {
		case r.entry:
			return rank(a.name) - rank(b.name)
		case a.fixed != b.fixed:
			if a.fixed {
				return -1
			}
			return 1
		case a.fixed:
			return 0
		}
		return rank(a.name) - rank(b.name)
	})
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
	for _, e := range entries {
		out.Content = append(out.Content, e.key, e.value)
	}
	return out, nil
}

// Reports a value omitempty leaves out, asking the value itself where it says
func isZero(v reflect.Value) bool {
	kind := v.Kind()
	if (kind == reflect.Pointer || kind == reflect.Interface) && v.IsNil() {
		return true
	}
	if z, ok := v.Interface().(interface{ IsZero() bool }); ok {
		return z.IsZero()
	}
	switch kind {
	case reflect.String:
		return v.Len() == 0
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Struct:
		t := v.Type()
		for i := range v.NumField() {
			if t.Field(i).IsExported() && !isZero(v.Field(i)) {
				return false
			}
		}
		return true
	}
	return false
}
