package config

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Reference to a variable inside a value of config.yaml: ${NAME}, or ${env:NAME}
var reference = regexp.MustCompile(`\$\{([^}]+)\}`)

// Start of a reference to a secret store rather than the environment, such as ${bitwarden:item}, which config.yaml can't resolve
var sourced = regexp.MustCompile(`^[a-z][a-z0-9_-]*:`)

// A value that is exactly one reference to a variable, the way a secret is held by reference
var whole = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// Whether the text is exactly one reference to a variable
func Reference(text string) bool {
	return whole.MatchString(text)
}

// Variable a reference reads, empty for one to a secret store
func referenced(body string) string {
	body = strings.TrimSpace(body)
	if name, ok := strings.CutPrefix(body, "env:"); ok {
		return strings.TrimSpace(name)
	}
	if strings.Contains(body, ":") && sourced.MatchString(body) {
		return ""
	}
	return body
}

// Value with every reference in its text replaced by the variable it names, a reference to a variable that isn't set kept as written and
// told once, naming where the variable is looked for
func (c *core) expand(value any, lookup func(name string) (string, bool), where func() string) any {
	switch v := value.(type) {
	case string:
		if !strings.Contains(v, "${") {
			return v
		}
		return reference.ReplaceAllStringFunc(v, func(raw string) string {
			inner := strings.TrimSpace(raw[2 : len(raw)-1])
			name := referenced(inner)
			if name == "" {
				if c.once("reference", raw) {
					slog.Warn(fmt.Sprintf("Config ref %q uses source %q which is not resolvable in config.yaml — external secret sources inject "+
						"env vars at startup, so reference the variable as ${env:NAME} instead", raw, strings.SplitN(inner, ":", 2)[0]))
				}
				return raw
			}
			if found, ok := lookup(name); ok {
				return found
			}
			if strings.HasPrefix(inner, "env:") {
				if place := where(); c.once("unset", raw, place) {
					slog.Warn(fmt.Sprintf("Config ref %q: %s is not set (check %s); keeping the literal placeholder", raw, name, place))
				}
			}
			return raw
		})
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = c.expand(item, lookup, where)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = c.expand(item, lookup, where)
		}
		return out
	}
	return value
}
