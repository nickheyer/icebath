package storage

import (
	"errors"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	yamlNull     = regexp.MustCompile(`^(?:~|[Nn]ull|NULL)?$`)
	yamlBool     = regexp.MustCompile(`^(?:[Tt]rue|TRUE|[Ff]alse|FALSE)$`)
	yamlOct      = regexp.MustCompile(`^0o[0-7]+$`)
	yamlInt      = regexp.MustCompile(`^[-+]?[0-9]+$`)
	yamlHex      = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)
	yamlInfNaN   = regexp.MustCompile(`^(?:[-+]?\.(?:inf|Inf|INF)|\.nan|\.NaN|\.NAN)$`)
	yamlExp      = regexp.MustCompile(`^[-+]?(?:\.[0-9]+|[0-9]+(?:\.[0-9]*)?)[eE][-+]?[0-9]+$`)
	yamlFloat    = regexp.MustCompile(`^[-+]?(?:\.[0-9]+|[0-9]+\.[0-9]*)$`)
	yamlPlainKey = regexp.MustCompile(`^[A-Za-z0-9_./-][A-Za-z0-9 _./-]*$`)
)

var errYAMLAliases = errors.New("Excessive alias count indicates a resource exhaustion attack")

type yamlAnchor struct {
	count      int
	aliasCount int
	res        any
	done       bool
}

// Converts one YAML document to JavaScript values as the yaml package parse does with the core schema
type yamlToJS struct {
	maxAliasCount int
	anchors       map[*yaml.Node]*yamlAnchor
}

// Parses source as the yaml package parse with maxAliasCount does, failing where it throws
func parseYAMLJS(source string, maxAliasCount int) (any, error) {
	dec := yaml.NewDecoder(strings.NewReader(source))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("Source contains multiple documents; please use YAML.parseAllDocuments()")
	}
	c := &yamlToJS{maxAliasCount: maxAliasCount, anchors: map[*yaml.Node]*yamlAnchor{}}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	return c.value(doc.Content[0])
}

func (c *yamlToJS) value(n *yaml.Node) (any, error) {
	if n.Kind == yaml.AliasNode {
		return c.alias(n)
	}
	var data *yamlAnchor
	if n.Anchor != "" {
		data = &yamlAnchor{count: 1}
		c.anchors[n] = data
	}
	var res any
	var err error
	switch n.Kind {
	case yaml.MappingNode:
		res, err = c.mapping(n)
	case yaml.SequenceNode:
		arr := []any{}
		for _, item := range n.Content {
			v, err := c.value(item)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		res = arr
	case yaml.DocumentNode:
		if len(n.Content) > 0 {
			res, err = c.value(n.Content[0])
		}
	default:
		res = yamlScalar(n)
	}
	if err != nil {
		return nil, err
	}
	if data != nil {
		data.res, data.done = res, true
	}
	return res, nil
}

// Resolves an alias to its anchor's value, refusing alias expansion past the configured limit
func (c *yamlToJS) alias(n *yaml.Node) (any, error) {
	found := n.Alias
	if found == nil {
		return nil, errors.New("Unresolved alias (the anchor must be set before the alias): " + n.Value)
	}
	data := c.anchors[found]
	if data == nil || !data.done {
		if _, err := c.value(found); err != nil {
			return nil, err
		}
		data = c.anchors[found]
	}
	if c.maxAliasCount >= 0 {
		data.count++
		if data.aliasCount == 0 {
			data.aliasCount = c.aliasCount(found)
		}
		if data.count*data.aliasCount > c.maxAliasCount {
			return nil, errYAMLAliases
		}
	}
	return data.res, nil
}

func (c *yamlToJS) aliasCount(n *yaml.Node) int {
	switch n.Kind {
	case yaml.AliasNode:
		if a := c.anchors[n.Alias]; a != nil {
			return a.count * a.aliasCount
		}
		return 0
	case yaml.MappingNode, yaml.SequenceNode:
		count := 0
		for _, item := range n.Content {
			if v := c.aliasCount(item); v > count {
				count = v
			}
		}
		return count
	}
	return 1
}

// Builds a plain object, refusing duplicate keys and stringifying keys as JavaScript does
func (c *yamlToJS) mapping(n *yaml.Node) (any, error) {
	obj := NewObject()
	var seen []any
	var seenNodes []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		kn, vn := n.Content[i], n.Content[i+1]
		var keyScalar any
		isScalar := kn.Kind == yaml.ScalarNode
		if isScalar {
			keyScalar = yamlScalar(kn)
		}
		for j, prev := range seenNodes {
			if prev == kn || (isScalar && prev.Kind == yaml.ScalarNode && jsStrictEqual(seen[j], keyScalar)) {
				return nil, errors.New("Map keys must be unique")
			}
		}
		seen, seenNodes = append(seen, keyScalar), append(seenNodes, kn)
		key, err := c.value(kn)
		if err != nil {
			return nil, err
		}
		val, err := c.value(vn)
		if err != nil {
			return nil, err
		}
		obj.Set(jsKeyString(key, kn), val)
	}
	return obj, nil
}

func jsStrictEqual(a, b any) bool {
	if fa, ok := a.(float64); ok {
		fb, ok := b.(float64)
		return ok && fa == fb
	}
	return a == b
}

// Property name a JavaScript object gives a parsed YAML key
func jsKeyString(key any, n *yaml.Node) string {
	switch k := key.(type) {
	case nil:
		return ""
	case string:
		return k
	case bool:
		return strconv.FormatBool(k)
	case float64:
		return jsNumberString(k)
	}
	return yamlFlowText(n)
}

// String conversion of a number as JavaScript String does
func jsNumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return jsNumber(f)
}

// Flow style text the yaml package prints for a collection used as a key
func yamlFlowText(n *yaml.Node) string {
	switch n.Kind {
	case yaml.AliasNode:
		return "*" + n.Value
	case yaml.MappingNode:
		if len(n.Content) == 0 {
			return "{}"
		}
		var parts []string
		for i := 0; i+1 < len(n.Content); i += 2 {
			parts = append(parts, yamlFlowText(n.Content[i])+": "+yamlFlowText(n.Content[i+1]))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	case yaml.SequenceNode:
		if len(n.Content) == 0 {
			return "[]"
		}
		var parts []string
		for _, item := range n.Content {
			parts = append(parts, yamlFlowText(item))
		}
		return "[ " + strings.Join(parts, ", ") + " ]"
	}
	v := yamlScalar(n)
	if s, ok := v.(string); ok {
		if yamlPlainKey.MatchString(s) && strings.TrimSpace(s) == s && yamlResolvePlain(s) == s {
			return s
		}
		return strconv.Quote(s)
	}
	return n.Value
}

// JavaScript value of a scalar under the YAML 1.2 core schema
func yamlScalar(n *yaml.Node) any {
	explicit := n.Style&yaml.TaggedStyle != 0
	if explicit {
		switch n.ShortTag() {
		case "!!str":
			return n.Value
		case "!!null":
			return nil
		case "!!bool":
			if v, ok := yamlResolvePlain(n.Value).(bool); ok {
				return v
			}
			return n.Value
		case "!!int", "!!float":
			if v, ok := yamlResolvePlain(n.Value).(float64); ok {
				return v
			}
			return n.Value
		}
		return n.Value
	}
	if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return n.Value
	}
	return yamlResolvePlain(n.Value)
}

// Core schema resolution of a plain scalar
func yamlResolvePlain(s string) any {
	switch {
	case yamlNull.MatchString(s):
		return nil
	case yamlBool.MatchString(s):
		return s[0] == 't' || s[0] == 'T'
	case yamlOct.MatchString(s):
		return radixFloat(s[2:], 8)
	case yamlInt.MatchString(s):
		v, _ := strconv.ParseFloat(s, 64)
		return v
	case yamlHex.MatchString(s):
		return radixFloat(s[2:], 16)
	case yamlInfNaN.MatchString(s):
		switch {
		case strings.HasSuffix(strings.ToLower(s), "nan"):
			return math.NaN()
		case s[0] == '-':
			return math.Inf(-1)
		}
		return math.Inf(1)
	case yamlExp.MatchString(s), yamlFloat.MatchString(s):
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	return s
}

// Nearest double of an unsigned integer in the given radix as parseInt answers
func radixFloat(digits string, radix int) float64 {
	v, ok := new(big.Int).SetString(digits, radix)
	if !ok {
		return math.NaN()
	}
	f, _ := new(big.Float).SetInt(v).Float64()
	return f
}
