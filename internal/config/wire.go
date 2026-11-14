package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"math/big"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const maxSafeInteger = 1<<53 - 1

func DocumentOf(root *yaml.Node) *YAMLDocument {
	return &YAMLDocument{root: plainCopy(root)}
}

func ValueNode(v any) (*yaml.Node, error) {
	return nodeFor(v)
}

func (s *Store) KnownRootKeys() ([]string, error) {
	defaults, err := s.defaults()
	if err != nil {
		return nil, err
	}
	return knownTopLevelKeys(defaults), nil
}

func wireRejected(message string) error {
	return &RejectedValueError{Message: message, Code: "config_value_invalid"}
}

func ToWire(n *yaml.Node, path []string, shown func([]string) string) (*yaml.Node, error) {
	where := func(p []string) string {
		if len(p) == 0 {
			return "<root>"
		}
		return shown(p)
	}
	n = resolveAlias(n)
	if n == nil || isNull(n) {
		return nullNode(), nil
	}
	switch n.Kind {
	case yaml.SequenceNode:
		out := &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq}
		for _, it := range n.Content {
			v, err := ToWire(it, path, shown)
			if err != nil {
				return nil, err
			}
			out.Content = append(out.Content, v)
		}
		return out, nil
	case yaml.MappingNode:
		if n.ShortTag() == "!!set" {
			return nil, wireRejected(fmt.Sprintf("%s: a set value cannot be stored in Remote Config", where(path)))
		}
		out := emptyMapping()
		seen := map[string]bool{}
		for _, p := range mappingPairs(n) {
			key, err := wireKey(p.key, path, where)
			if err != nil {
				return nil, err
			}
			sub := append(append([]string(nil), path...), key)
			if seen[key] {
				return nil, wireRejected(fmt.Sprintf("%s: two keys become the same string key %s", shown(sub), quoted(key)))
			}
			seen[key] = true
			v, err := ToWire(p.value, sub, shown)
			if err != nil {
				return nil, err
			}
			out.Content = append(out.Content, keyNode(key), v)
		}
		return out, nil
	}
	tag, text := scalarKind(n)
	switch tag {
	case tagStr:
		return keyNode(text), nil
	case tagBool:
		v, _ := resolveTagged(tagBool, text)
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagBool, Value: v}, nil
	case tagInt:
		i, ok := constructInt(text)
		if !ok {
			break
		}
		if i.IsInt64() && i.Int64() >= -maxSafeInteger && i.Int64() <= maxSafeInteger {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagInt, Value: i.String()}, nil
		}
		slog.Warn(fmt.Sprintf("config path %s: integer %s is outside ±(2^53-1); sent as a decimal string", where(path), i.String()))
		return keyNode(i.String()), nil
	case tagFloat:
		f, ok := constructFloat(text)
		if !ok {
			break
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, wireRejected(fmt.Sprintf("%s: NaN/Infinity cannot be stored in Remote Config", where(path)))
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagFloat, Value: floatText(f)}, nil
	case "!!timestamp":
		return keyNode(strings.Replace(timestampWord(text), " ", "T", 1)), nil
	}
	return nil, wireRejected(fmt.Sprintf("%s: a %s value cannot be stored in Remote Config", where(path), typeName(n)))
}

func wireKey(k *yaml.Node, path []string, where func([]string) string) (string, error) {
	k = resolveAlias(k)
	if k != nil && k.Kind == yaml.ScalarNode {
		tag, text := scalarKind(k)
		switch tag {
		case tagStr:
			return text, nil
		case tagInt:
			if i, ok := constructInt(text); ok {
				return i.String(), nil
			}
		}
	}
	return "", wireRejected(fmt.Sprintf("%s: a %s mapping key (%s) cannot be stored in Remote Config; quote it in YAML",
		where(path), typeName(k), literalText(k)))
}

func WireNode(data []byte) (*yaml.Node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := wireValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data after the JSON value", ErrShape)
	}
	return n, nil
}

func wireValue(dec *json.Decoder) (*yaml.Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			out := &yaml.Node{Kind: yaml.SequenceNode, Tag: tagSeq}
			for dec.More() {
				v, err := wireValue(dec)
				if err != nil {
					return nil, err
				}
				out.Content = append(out.Content, v)
			}
			_, err := dec.Token()
			return out, err
		}
		out := emptyMapping()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := kt.(string)
			v, err := wireValue(dec)
			if err != nil {
				return nil, err
			}
			putKey(out, key, v)
		}
		_, err := dec.Token()
		return out, err
	case string:
		return keyNode(t), nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagBool, Value: fmt.Sprint(t)}, nil
	case json.Number:
		text := t.String()
		if !strings.ContainsAny(text, ".eE") {
			if i, ok := new(big.Int).SetString(text, 10); ok {
				return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagInt, Value: i.String()}, nil
			}
		}
		f, err := t.Float64()
		if err != nil && !math.IsInf(f, 0) {
			return nil, err
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tagFloat, Value: floatText(f)}, nil
	}
	return nullNode(), nil
}

type readScopeKey struct{}

type ReadBases struct {
	mu    sync.Mutex
	bases map[any]any
}

func ReadScope(ctx context.Context) context.Context {
	fresh := &ReadBases{bases: map[any]any{}}
	if parent := ReadScopeOf(ctx); parent != nil {
		parent.mu.Lock()
		maps.Copy(fresh.bases, parent.bases)
		parent.mu.Unlock()
	}
	return context.WithValue(ctx, readScopeKey{}, fresh)
}

func ReadScopeOf(ctx context.Context) *ReadBases {
	r, _ := ctx.Value(readScopeKey{}).(*ReadBases)
	return r
}

func (r *ReadBases) Get(key any) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.bases[key]
	return v, ok
}

func (r *ReadBases) Put(key, value any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bases[key] = value
}

func (r *ReadBases) Empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bases) == 0
}
