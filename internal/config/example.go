package config

import (
	"bytes"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Text a config.yaml of one scope is made with when there is none to edit: every setting of the scope a person changes, commented out at
// its default under the titles of its section, with what it does, the values it takes, what unset means, its unit and the variables that
// override it
func (c *Config) Example(global bool) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# icebath settings, every one at its default.\n")
	b.WriteString("# Copy a setting into config.yaml without its leading \"# \", change it in the terminal UI with\n")
	b.WriteString("# /settings, or run: icebath config set <key> <value>\n\n")
	sections := c.Sections()
	titles := map[string]Section{}
	for _, section := range sections {
		titles[section.Key] = section
	}
	for _, section := range sections {
		if c.Global(section.Key) != global {
			continue
		}
		var shown []Setting
		for _, setting := range section.Settings {
			if !setting.Internal {
				shown = append(shown, setting)
			}
		}
		if len(shown) == 0 {
			continue
		}
		var path []string
		for at, ok := section, true; ok; at, ok = titles[at.Parent] {
			path = append([]string{at.Title}, path...)
		}
		heading := strings.Join(path, " › ")
		if section.About != "" {
			heading += ": " + sentence(section.About)
		}
		fmt.Fprintf(&b, "\n# ============================================================\n# %s\n# ============================================================\n", heading)
		for _, setting := range shown {
			b.WriteString("\n")
			for _, line := range setting.described() {
				b.WriteString("# " + line + "\n")
			}
			text, err := snippet(setting.Key, setting.DefaultValue())
			if err != nil {
				return nil, fmt.Errorf("%s: %w", setting.Key, err)
			}
			for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
				b.WriteString("# " + line + "\n")
			}
		}
	}
	return b.Bytes(), nil
}

// Text ending in a full stop
func sentence(text string) string {
	if strings.HasSuffix(text, ".") || strings.HasSuffix(text, "?") || strings.HasSuffix(text, "!") {
		return text
	}
	return text + "."
}

// Lines of what a setting does, as the example of a config.yaml says it above the setting
func (s Setting) described() []string {
	lines := []string{s.Title}
	if s.About != "" {
		lines[0] += ": " + sentence(s.About)
	}
	if s.Options != nil && (s.Kind() == Choice || s.Kind() == List) {
		var values []string
		for _, option := range s.Options() {
			values = append(values, option.Value)
		}
		lead := "One of: "
		if s.Kind() == List {
			lead = "Any of: "
		}
		if len(values) > 0 {
			lines = append(lines, lead+strings.Join(values, ", ")+".")
		}
	}
	if s.Kind() == Duration {
		lines = append(lines, "In "+s.UnitName()+", or a length such as 30s or 5m.")
		if s.Null != "" {
			lines = append(lines, "Unset: "+s.Null+".")
		}
	}
	if len(s.Env) > 0 {
		lines = append(lines, "The "+strings.Join(s.Env, " or ")+" environment variable overrides it.")
	}
	return lines
}

// Nested YAML of one key at a value, ready to paste
func snippet(key string, value any) (string, error) {
	parts := strings.Split(key, ".")
	doc := value
	for i := len(parts) - 1; i >= 0; i-- {
		doc = map[string]any{parts[i]: doc}
	}
	out, err := yaml.Marshal(doc)
	return string(out), err
}
