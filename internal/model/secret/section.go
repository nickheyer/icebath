package secret

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
)

// Raw config mapping read with the loose rules hand edited YAML needs
type section struct{ node *yaml.Node }

// Section view of a raw value, empty when the value is unset or no mapping
func sectionOf(raw config.Raw) section {
	return section{node: mappingOf(raw.Node())}
}

// The node itself when it is a mapping, documents and aliases followed, nil otherwise
func mappingOf(n *yaml.Node) *yaml.Node {
	for n != nil && (n.Kind == yaml.DocumentNode || n.Kind == yaml.AliasNode) {
		if n.Kind == yaml.AliasNode {
			n = n.Alias
		} else if len(n.Content) > 0 {
			n = n.Content[0]
		} else {
			return nil
		}
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	return n
}

// Value node under key, nil when the section lacks it
func (s section) child(key string) *yaml.Node {
	if s.node == nil {
		return nil
	}
	for i := 0; i+1 < len(s.node.Content); i += 2 {
		if s.node.Content[i].Value == key {
			return s.node.Content[i+1]
		}
	}
	return nil
}

// Scalar under key with its YAML 1.1 type, false for a missing key or a collection
func (s section) scalar(key string) (config.Scalar, bool) {
	n := s.child(key)
	if n == nil || n.Kind != yaml.ScalarNode {
		return config.Scalar{}, false
	}
	var v config.Scalar
	if n.Decode(&v) != nil {
		return config.Scalar{}, false
	}
	return v, true
}

// Reports a value a person would read as switched on, def when the key is missing
func (s section) truthy(key string, def bool) bool {
	n := s.child(key)
	if n == nil {
		return def
	}
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		return len(n.Content) > 0
	}
	v, ok := s.scalar(key)
	if !ok {
		return false
	}
	switch v.Kind() {
	case config.KindBool:
		on, _ := v.AsBool()
		return on
	case config.KindInt, config.KindFloat:
		f, _ := v.AsFloat()
		return f != 0
	case config.KindString:
		return v.String() != ""
	}
	return false
}

// Text of the scalar under key, empty for a missing, null, false, zero or collection value
func (s section) text(key string) string {
	v, ok := s.scalar(key)
	if !ok || !s.truthy(key, false) {
		return ""
	}
	if v.Kind() == config.KindBool {
		return "True"
	}
	return v.String()
}

// Number under key, def when the key is missing or its value is no number
func (s section) float(key string, def float64) float64 {
	v, ok := s.scalar(key)
	if !ok {
		return def
	}
	switch v.Kind() {
	case config.KindBool:
		if on, _ := v.AsBool(); on {
			return 1
		}
		return 0
	case config.KindInt, config.KindFloat:
		f, _ := v.AsFloat()
		return f
	case config.KindString:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v.String()), 64); err == nil {
			return f
		}
	}
	return def
}

// Mapping under key as a section, empty when the value is anything else
func (s section) sub(key string) section {
	return section{node: mappingOf(s.child(key))}
}

// Strings of the sequence under key, entries of any other type left out, nil when the value is no sequence
func (s section) stringList(key string) ([]string, bool) {
	n := s.child(key)
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil, false
	}
	var out []string
	for _, item := range n.Content {
		if item.Kind != yaml.ScalarNode {
			continue
		}
		var v config.Scalar
		if item.Decode(&v) == nil && v.Kind() == config.KindString {
			out = append(out, v.String())
		}
	}
	return out, true
}

// One pair of a mapping, the value text set only when the value is a string
type binding struct {
	key      string
	value    string
	isString bool
}

// Bindings of the mapping under key in file order
func (s section) bindings(key string) []binding {
	m := mappingOf(s.child(key))
	if m == nil {
		return nil
	}
	var out []binding
	for i := 0; i+1 < len(m.Content); i += 2 {
		b := binding{key: m.Content[i].Value}
		if value := m.Content[i+1]; value.Kind == yaml.ScalarNode {
			var v config.Scalar
			if value.Decode(&v) == nil && v.Kind() == config.KindString {
				b.value, b.isString = v.String(), true
			}
		}
		out = append(out, b)
	}
	return out
}
