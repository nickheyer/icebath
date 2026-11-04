package skills

import (
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	frontmatterEndRe = regexp.MustCompile(`\n---[ \t]*\n`)
	yaml11TrueRe     = regexp.MustCompile(`^(?i:y|yes|true|on)$`)
	yaml11FalseRe    = regexp.MustCompile(`^(?i:n|no|false|off)$`)
	yaml11IntRe      = regexp.MustCompile(`^[-+]?(?:0|[1-9][0-9_]*)$`)
	yaml11FloatRe    = regexp.MustCompile(`^[-+]?(?:[0-9][0-9_]*)?\.[0-9_]*(?:[eE][-+]?[0-9]+)?$`)
)

// Decodes a YAML document into plain maps, slices, and scalars using YAML 1.1 bool and number rules
func yamlLoad(text string) (any, error) {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(text), &node); err != nil {
		return nil, err
	}
	if node.Kind == 0 {
		return nil, nil
	}
	return yamlValue(&node), nil
}

// Converts a yaml node tree into Go values with mapping keys stringified
func yamlValue(n *yaml.Node) any {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil
		}
		return yamlValue(n.Content[0])
	case yaml.MappingNode:
		out := map[string]any{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := yamlScalarString(n.Content[i])
			out[key] = yamlValue(n.Content[i+1])
		}
		return out
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			out = append(out, yamlValue(c))
		}
		return out
	case yaml.AliasNode:
		if n.Alias != nil {
			return yamlValue(n.Alias)
		}
		return nil
	case yaml.ScalarNode:
		return yamlScalar(n)
	}
	return nil
}

// Renders a mapping key as the string YAML 1.1 would print for it
func yamlScalarString(n *yaml.Node) string {
	v := yamlValue(n)
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	return n.Value
}

// Resolves one scalar, quoted scalars staying strings and plain ones following YAML 1.1 types
func yamlScalar(n *yaml.Node) any {
	if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return n.Value
	}
	if n.Tag == "!!str" || n.Tag == "!!bool" || n.Tag == "!!null" {
		v := n.Value
		switch {
		case v == "" || v == "~" || strings.EqualFold(v, "null"):
			return nil
		case yaml11TrueRe.MatchString(v):
			return true
		case yaml11FalseRe.MatchString(v):
			return false
		}
	}
	switch n.Tag {
	case "!!str":
		return n.Value
	case "!!null":
		return nil
	case "!!bool":
		return yaml11TrueRe.MatchString(n.Value)
	case "!!int":
		if i, err := strconv.ParseInt(strings.ReplaceAll(n.Value, "_", ""), 0, 64); err == nil {
			return i
		}
		return n.Value
	case "!!float":
		if f, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64); err == nil {
			return f
		}
		return n.Value
	}
	v := n.Value
	switch {
	case v == "" || v == "~" || strings.EqualFold(v, "null"):
		return nil
	case yaml11TrueRe.MatchString(v):
		return true
	case yaml11FalseRe.MatchString(v):
		return false
	case yaml11IntRe.MatchString(v):
		if i, err := strconv.ParseInt(strings.ReplaceAll(v, "_", ""), 10, 64); err == nil {
			return i
		}
	case yaml11FloatRe.MatchString(v):
		if f, err := strconv.ParseFloat(strings.ReplaceAll(v, "_", ""), 64); err == nil {
			return f
		}
	}
	return v
}

// Splits YAML frontmatter from a markdown body, falling back to key value line splitting on bad YAML
func ParseFrontmatter(content string) (map[string]any, string) {
	content = strings.TrimPrefix(content, "\uFEFF")
	if !strings.HasPrefix(content, "---") {
		return map[string]any{}, content
	}
	loc := frontmatterEndRe.FindStringIndex(content[3:])
	if loc == nil {
		return map[string]any{}, content
	}
	yamlText := content[3 : loc[0]+3]
	body := content[loc[1]+3:]
	fm := map[string]any{}
	parsed, err := yamlLoad(yamlText)
	if err == nil {
		if m, ok := parsed.(map[string]any); ok {
			fm = m
		}
		return fm, body
	}
	for _, line := range strings.Split(strings.TrimSpace(yamlText), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fm[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return fm, body
}

// Frontmatter mapping of a text, empty on any parse failure
func safeFrontmatter(content string) map[string]any {
	fm, _ := ParseFrontmatter(content)
	if fm == nil {
		return map[string]any{}
	}
	return fm
}

// String form of a frontmatter value, empty for nil
func fmString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	return ""
}

// Mapping value of a frontmatter key, nil when absent or not a mapping
func fmMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// List form of a scalar or list frontmatter value, stripped non empty strings only
func fmStringList(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if s := strings.TrimSpace(t); s != "" {
			return []string{s}
		}
		return nil
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s := strings.TrimSpace(fmString(item)); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Reports whether a frontmatter value is truthy the way Python would judge it
func fmTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// Icebath metadata mapping of a frontmatter, metadata.hermes accepted as the upstream alias
func icebathMeta(fm map[string]any) map[string]any {
	meta := fmMap(fm["metadata"])
	if meta == nil {
		return map[string]any{}
	}
	if m := fmMap(meta["icebath"]); m != nil {
		return m
	}
	if m := fmMap(meta["hermes"]); m != nil {
		return m
	}
	return map[string]any{}
}

// Serializes a mapping as YAML text without sorting keys
func yamlDump(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
