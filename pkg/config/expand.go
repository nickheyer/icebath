package config

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

var envRefRe = regexp.MustCompile(`\$\{([^}]+)\}`)
var secretSourceRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*:`)

// Resolves an environment variable name to its value, false when unset
type Lookup func(name string) (string, bool)

// Variable name a reference body reads, empty for a non env source or a blank env source
func envRefName(body string) string {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "env:") {
		return strings.TrimSpace(body[len("env:"):])
	}
	if strings.Contains(body, ":") && secretSourceRe.MatchString(body) {
		return ""
	}
	return body
}

// Value with every ${VAR} and ${env:VAR} reference replaced, unresolved references kept verbatim
func ExpandString(s string, lookup Lookup) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return envRefRe.ReplaceAllStringFunc(s, func(raw string) string {
		inner := strings.TrimSpace(raw[2 : len(raw)-1])
		name := envRefName(inner)
		if name == "" {
			if !strings.HasPrefix(inner, "env:") && strings.Contains(inner, ":") && secretSourceRe.MatchString(inner) {
				slog.Warn(fmt.Sprintf("Config ref %s uses source %s which is not resolvable in config.yaml — external secret sources "+
					"inject env vars at startup, so reference the variable as ${env:NAME} instead",
					PyRepr(raw), PyRepr(strings.SplitN(inner, ":", 2)[0])))
			}
			return raw
		}
		if v, ok := lookup(name); ok {
			return v
		}
		if strings.HasPrefix(inner, "env:") {
			slog.Warn(fmt.Sprintf("Config ref %s: %s is not set (check ~/.icebath/.env); keeping the literal placeholder", PyRepr(raw), name))
		}
		return raw
	})
}

// Document with every string value expanded, keys and non strings untouched
func Expand(v any, lookup Lookup) any {
	switch c := v.(type) {
	case string:
		return ExpandString(c, lookup)
	case map[string]any:
		out := make(map[string]any, len(c))
		for k, val := range c {
			out[k] = Expand(val, lookup)
		}
		SetMappingKeys(out, MappingKeys(c))
		return out
	case Tree:
		return Tree(Expand(map[string]any(c), lookup).(map[string]any))
	case []any:
		out := make([]any, len(c))
		for i, val := range c {
			out[i] = Expand(val, lookup)
		}
		return out
	}
	return v
}

// Names referenced by the document mapped to the values the expansion saw
func RefSnapshot(v any, lookup Lookup) map[string]*string {
	snap := map[string]*string{}
	var walk func(any)
	walk = func(x any) {
		switch c := x.(type) {
		case string:
			for _, m := range envRefRe.FindAllStringSubmatch(c, -1) {
				name := envRefName(m[1])
				if name == "" {
					continue
				}
				if val, ok := lookup(name); ok {
					v := val
					snap[name] = &v
				} else {
					snap[name] = nil
				}
			}
		case map[string]any:
			for _, val := range c {
				walk(val)
			}
		case Tree:
			walk(map[string]any(c))
		case []any:
			for _, val := range c {
				walk(val)
			}
		}
	}
	walk(v)
	return snap
}

// Reports whether every referenced name still resolves to the snapshot value
func SnapshotCurrent(snap map[string]*string, lookup Lookup) bool {
	for name, was := range snap {
		now, ok := lookup(name)
		if was == nil {
			if ok {
				return false
			}
			continue
		}
		if !ok || now != *was {
			return false
		}
	}
	return true
}
