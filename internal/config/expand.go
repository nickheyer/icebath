package config

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Environment reference grammar of config strings, a dollar brace reference with a bare or env prefixed name
var envRef = regexp.MustCompile(`\$\{([^}]+)\}`)

// Source prefix of a secret reference body such as bitwarden or vault followed by a colon
var secretSource = regexp.MustCompile(`^[a-z][a-z0-9_-]*:`)

// Reports a reference body naming a source other than the environment
func nonEnvSecretRef(ref string) bool {
	return strings.Contains(ref, ":") && secretSource.MatchString(ref)
}

// Variable a reference body reads, ok false for another source or an empty env reference
func envRefName(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if name, ok := strings.CutPrefix(ref, "env:"); ok {
		name = strings.TrimSpace(name)
		return name, name != ""
	}
	if nonEnvSecretRef(ref) {
		return "", false
	}
	return ref, true
}

// Value of a variable an env reference reads, through the secret scope of ctx when one is installed
func (s *Store) lookupEnv(ctx context.Context, name string) (string, bool) {
	return s.env(ctx, name)
}

// Text with every resolvable reference replaced by its variable, unresolved ones kept as written
func (s *Store) expandString(ctx context.Context, text string) string {
	return envRef.ReplaceAllStringFunc(text, func(raw string) string {
		inner := strings.TrimSpace(raw[2 : len(raw)-1])
		name, ok := envRefName(inner)
		if !ok {
			if !strings.HasPrefix(inner, "env:") && nonEnvSecretRef(inner) {
				slog.Warn(fmt.Sprintf("Config ref %s uses source %s which is not resolvable in config.yaml — external secret sources "+
					"inject env vars at startup, so reference the variable as ${env:NAME} instead", quoted(raw), quoted(strings.SplitN(inner, ":", 2)[0])))
			}
			return raw
		}
		if v, set := s.lookupEnv(ctx, name); set {
			return v
		}
		if strings.HasPrefix(inner, "env:") {
			slog.Warn(fmt.Sprintf("Config ref %s: %s is not set (check ~/.icebath/.env); keeping the literal placeholder", quoted(raw), name))
		}
		return raw
	})
}

// Copy of a value tree with references expanded in every string value, keys and other scalars untouched
func (s *Store) expandTree(ctx context.Context, n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	out := plainCopy(n)
	var walk func(n *yaml.Node, key bool)
	walk = func(n *yaml.Node, key bool) {
		switch n.Kind {
		case yaml.ScalarNode:
			if key || !isStringNode(n) || !strings.Contains(n.Value, "${") {
				return
			}
			if expanded := s.expandString(ctx, n.Value); expanded != n.Value {
				n.Tag, n.Value = tagStr, expanded
				if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle|yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle) == 0 {
					n.Style = yaml.DoubleQuotedStyle
				}
			}
		default:
			for i, c := range n.Content {
				walk(c, n.Kind == yaml.MappingNode && i%2 == 0)
			}
		}
	}
	walk(out, false)
	return out
}

// Value one referenced variable had, unset recorded as such
type envReading struct {
	value string
	set   bool
}

// Records the current value of every variable the string values below n reference
func (s *Store) envSnapshot(ctx context.Context, n *yaml.Node, out map[string]envReading) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if !isStringNode(n) {
			return
		}
		for _, m := range envRef.FindAllStringSubmatch(n.Value, -1) {
			if name, ok := envRefName(m[1]); ok {
				v, set := s.lookupEnv(ctx, name)
				out[name] = envReading{v, set}
			}
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			s.envSnapshot(ctx, n.Content[i], out)
		}
	default:
		for _, c := range n.Content {
			s.envSnapshot(ctx, c, out)
		}
	}
}

// Reports whether every recorded variable still holds the value it had
func (s *Store) envUnchanged(ctx context.Context, snap map[string]envReading) bool {
	for name, was := range snap {
		v, set := s.lookupEnv(ctx, name)
		if (envReading{v, set}) != was {
			return false
		}
	}
	return true
}

// Items keyed by name, nil unless every item is a mapping whose text name no other item shares
func itemsByUniqueName(items *yaml.Node) map[string]*yaml.Node {
	if !isSequence(items) {
		return nil
	}
	out := map[string]*yaml.Node{}
	for _, it := range items.Content {
		name := childOf(it, "name")
		if !isMapping(it) || !isStringNode(name) {
			return nil
		}
		if _, dup := out[name.Value]; dup {
			return nil
		}
		out[name.Value] = it
	}
	return out
}

// Current with the reference templates of raw put back wherever the value is otherwise unchanged
func (s *Store) preserveEnvTemplates(ctx context.Context, current, raw, loaded *yaml.Node) *yaml.Node {
	switch {
	case current == nil:
		return nil
	case isStringNode(current) && isStringNode(raw) && envRef.MatchString(raw.Value):
		if current.Value == raw.Value || (isStringNode(loaded) && current.Value == loaded.Value) || s.expandString(ctx, raw.Value) == current.Value {
			return plainCopy(raw)
		}
		return current
	case isMapping(current) && isMapping(raw):
		out := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap, Style: current.Style}
		for i := 0; i+1 < len(current.Content); i += 2 {
			k := current.Content[i].Value
			var l *yaml.Node
			if isMapping(loaded) {
				l = childOf(loaded, k)
			}
			out.Content = append(out.Content, plainCopy(current.Content[i]), s.preserveEnvTemplates(ctx, current.Content[i+1], childOf(raw, k), l))
		}
		return out
	case isSequence(current) && isSequence(raw):
		out := &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq, Style: current.Style}
		byName, rawByName, loadedByName := itemsByUniqueName(current), itemsByUniqueName(raw), itemsByUniqueName(loaded)
		for i, it := range current.Content {
			var r, l *yaml.Node
			if byName != nil && rawByName != nil {
				name := childOf(it, "name").Value
				r = rawByName[name]
				if loadedByName != nil {
					l = loadedByName[name]
				}
			} else {
				if i < len(raw.Content) {
					r = raw.Content[i]
				}
				if isSequence(loaded) && i < len(loaded.Content) {
					l = loaded.Content[i]
				}
			}
			out.Content = append(out.Content, s.preserveEnvTemplates(ctx, it, r, l))
		}
		return out
	}
	return current
}
