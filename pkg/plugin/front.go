// Plugins, skills, agents, hooks, servers and styles in the Claude, Codex and portable plugin formats, discovered,
// installed and rendered for the harness
package plugin

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Frontmatter of a markdown file as parsed, with the body that followed it
type Front struct {
	Fields map[string]any
	Body   string
	// The opening marker was the first line, so the fields were read
	Present bool
	// The YAML did not parse, so every field is unset
	Err error
}

// Splits a markdown file into frontmatter and body, the fields read only when the first line opens them
func ParseFront(text string) Front {
	text = strings.TrimPrefix(text, "\ufeff")
	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r\n") != "---" {
		return Front{Body: text, Fields: map[string]any{}}
	}
	for i := 1; i < len(lines); i++ {
		if t := strings.TrimRight(lines[i], "\r\n"); t == "---" || t == "..." {
			raw := strings.Join(lines[1:i], "")
			body := strings.Join(lines[i+1:], "")
			f := Front{Fields: map[string]any{}, Body: body, Present: true}
			var out map[string]any
			if err := yaml.Unmarshal([]byte(raw), &out); err != nil {
				f.Err = err
				return f
			}
			if out != nil {
				f.Fields = out
			}
			return f
		}
	}
	return Front{Body: text, Fields: map[string]any{}}
}

// String value of a field, numbers and booleans spelled out, empty for a missing or structured one
func (f Front) String(key string) string {
	v, ok := f.Fields[key]
	if !ok || v == nil {
		return ""
	}
	return scalar(v)
}

// Text of a scalar, empty for a map or list
func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// Boolean value of a field, the default while missing, taking yes, no, on, off, 1 and 0 in any case
func (f Front) Bool(key string, def bool) bool {
	v, ok := f.Fields[key]
	if !ok || v == nil {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	switch strings.ToLower(strings.TrimSpace(scalar(v))) {
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	}
	return def
}

// Integer value of a field, zero while missing or not a whole number
func (f Front) Int(key string) int {
	v, ok := f.Fields[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		if t == float64(int(t)) {
			return int(t)
		}
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(scalar(v)))
	if err != nil {
		return 0
	}
	return n
}

// Words of a field given as a list, or as a string split on commas and spaces
func (f Front) List(key string) []string {
	v, ok := f.Fields[key]
	if !ok || v == nil {
		return nil
	}
	return words(v)
}

// Words of a list or of a string split on commas and spaces, blanks dropped
func words(v any) []string {
	var out []string
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			out = append(out, strings.TrimSpace(scalar(item)))
		}
	default:
		for _, w := range strings.FieldsFunc(scalar(v), func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
			out = append(out, strings.TrimSpace(w))
		}
	}
	kept := out[:0]
	for _, w := range out {
		if w != "" {
			kept = append(kept, w)
		}
	}
	return kept
}

// Rule patterns of a field, split on commas only so a pattern like Bash(git commit *) keeps its spaces
func (f Front) Rules(key string) []string {
	v, ok := f.Fields[key]
	if !ok || v == nil {
		return nil
	}
	var out []string
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			out = append(out, strings.TrimSpace(scalar(item)))
		}
	default:
		out = splitRules(scalar(v))
	}
	kept := out[:0]
	for _, w := range out {
		if w != "" {
			kept = append(kept, w)
		}
	}
	return kept
}

// Splits rule text on commas and on spaces outside parentheses
func splitRules(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case r == '(':
			depth++
			cur.WriteRune(r)
		case r == ')':
			if depth > 0 {
				depth--
			}
			cur.WriteRune(r)
		case (r == ',' || r == ' ' || r == '\n' || r == '\t') && depth == 0:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// Map value of a field, nil while missing or not a map
func (f Front) Map(key string) map[string]any {
	v, ok := f.Fields[key]
	if !ok || v == nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// Reports whether the field is set
func (f Front) Has(key string) bool {
	_, ok := f.Fields[key]
	return ok
}

// Keys set that the known list does not name, sorted
func (f Front) Unknown(known ...string) []string {
	var out []string
	for k := range f.Fields {
		found := false
		for _, name := range known {
			if name == k {
				found = true
				break
			}
		}
		if !found {
			out = append(out, k)
		}
	}
	sortStrings(out)
	return out
}
