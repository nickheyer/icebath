// Nested configuration document with dotted path navigation, deep merge and default stripping
package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

// Configuration document as nested maps the way YAML decodes it
type Tree map[string]any

// Python exception class and message of a dotted path write that cannot be navigated
type PathError struct {
	Kind string
	Msg  string
}

func (e *PathError) Error() string { return e.Msg }

// Refusal to create a nested key beside a literal dotted sibling it would shadow
type PhantomKeyError struct {
	Part, Path, Shadowed, Escaped string
}

func (e *PhantomKeyError) Error() string {
	return fmt.Sprintf("Refusing to create nested key %s in %s: the mapping already contains a literal key %s that contains a dot. If you meant that key, escape its dots with a backslash (e.g. %s).",
		PyRepr(e.Part), PyRepr(e.Path), PyRepr(e.Shadowed), e.Escaped)
}

// Segments of a dotted path, a backslash before a dot keeping the dot literal
func SplitPath(key string) []string {
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(key); i++ {
		ch := key[i]
		if ch == '\\' && i+1 < len(key) && key[i+1] == '.' {
			cur.WriteByte('.')
			i++
			continue
		}
		if ch == '.' {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	parts = append(parts, cur.String())
	return parts
}

// Longest literal dotted key of the container matching a prefix of parts, with the segments it consumes
func greedyLiteral(container map[string]any, parts []string) (string, int, bool) {
	for n := len(parts); n > 0; n-- {
		key := strings.Join(parts[:n], ".")
		if _, ok := container[key]; ok {
			return key, n, true
		}
	}
	return "", 0, false
}

// First key in mapping order that creating the mapping part would shadow
func phantomSibling(container map[string]any, part string) string {
	prefix := part + "."
	for _, k := range MappingKeys(container) {
		if strings.HasPrefix(k, prefix) {
			return k
		}
	}
	return ""
}

// Position a Python list subscript of the segment addresses, negative counting from the end
func listIndex(segment string, length int) (int, *PathError) {
	n, ok := pyint.Parse(segment)
	if !ok {
		return 0, &PathError{"ValueError", "invalid literal for int() with base 10: " + PyRepr(segment)}
	}
	if !n.IsInt64() {
		return 0, &PathError{"IndexError", "cannot fit 'int' into an index-sized integer"}
	}
	idx := n.Int64()
	if idx < 0 {
		idx += int64(length)
	}
	if idx < 0 || idx >= int64(length) {
		return 0, &PathError{"IndexError", "list index out of range"}
	}
	return int(idx), nil
}

type hop struct {
	container any
	key       any
}

// Walks parts to the container holding the leaf and the key addressing it
func locate(root any, parts []string) ([]hop, any, any, bool) {
	var parents []hop
	cur := root
	i := 0
	for {
		remaining := parts[i:]
		var key any
		consumed := 1
		switch c := cur.(type) {
		case []any:
			idx, err := listIndex(remaining[0], len(c))
			if err != nil {
				return nil, nil, nil, false
			}
			key = idx
		case map[string]any:
			k, n, ok := greedyLiteral(c, remaining)
			if !ok {
				return nil, nil, nil, false
			}
			key, consumed = k, n
		case Tree:
			k, n, ok := greedyLiteral(map[string]any(c), remaining)
			if !ok {
				return nil, nil, nil, false
			}
			key, consumed = k, n
			cur = map[string]any(c)
		default:
			return nil, nil, nil, false
		}
		i += consumed
		if i >= len(parts) {
			return parents, cur, key, true
		}
		parents = append(parents, hop{cur, key})
		cur = index(cur, key)
	}
}

// Child of a container at key
func index(container any, key any) any {
	switch c := container.(type) {
	case []any:
		return c[key.(int)]
	case map[string]any:
		return c[key.(string)]
	}
	return nil
}

// Value at a dotted path and whether it exists
func (t Tree) Lookup(path string) (any, bool) {
	_, container, key, ok := locate(map[string]any(t), SplitPath(path))
	if !ok {
		return nil, false
	}
	return index(container, key), true
}

// Value at a dotted path, nil when absent
func (t Tree) Get(path string) any {
	v, _ := t.Lookup(path)
	return v
}

// Stores value under key, a new key going to the end of the mapping order
func setOrderedKey(m map[string]any, key string, value any) {
	_, had := m[key]
	m[key] = value
	if !had {
		AppendMappingKey(m, key)
	}
}

// Sets the value at a dotted path, creating intermediate maps and refusing to shadow a dotted sibling
func (t Tree) Set(path string, value any) error {
	parts := SplitPath(path)
	var cur any = map[string]any(t)
	i := 0
	for i < len(parts) {
		remaining := parts[i:]
		atLeaf := len(remaining) == 1
		switch c := cur.(type) {
		case []any:
			part := remaining[0]
			idx, err := listIndex(part, len(c))
			if atLeaf {
				if err != nil && err.Kind == "IndexError" && err.Msg == "list index out of range" {
					err.Msg = "list assignment index out of range"
				}
				if err != nil {
					return err
				}
				c[idx] = value
				return nil
			}
			if err != nil && err.Kind == "ValueError" {
				return &PathError{"TypeError", fmt.Sprintf("Cannot navigate into list at key %s: segment %s is not a numeric index", PyRepr(path), PyRepr(part))}
			}
			if err != nil {
				return err
			}
			cur = c[idx]
			i++
		case map[string]any:
			if key, n, ok := greedyLiteral(c, remaining); ok {
				if i+n == len(parts) {
					c[key] = value
					return nil
				}
				switch c[key].(type) {
				case map[string]any, []any:
				default:
					c[key] = map[string]any{}
				}
				cur = c[key]
				i += n
				continue
			}
			part := remaining[0]
			if atLeaf {
				setOrderedKey(c, part, value)
				return nil
			}
			if shadowed := phantomSibling(c, part); shadowed != "" {
				escaped := strings.ReplaceAll(shadowed, ".", `\.`)
				return &PhantomKeyError{Part: part, Path: path, Shadowed: shadowed, Escaped: escaped}
			}
			next := map[string]any{}
			setOrderedKey(c, part, next)
			cur = next
			i++
		default:
			return &PathError{"TypeError", fmt.Sprintf("Cannot navigate into %s at key %s", PyTypeName(cur), PyRepr(path))}
		}
	}
	return nil
}

// Deletes the hop key from its container, storing a shortened list back into the hop above
func deleteChild(hops []hop, j int) {
	switch c := hops[j].container.(type) {
	case map[string]any:
		delete(c, hops[j].key.(string))
	case []any:
		idx := hops[j].key.(int)
		shorter := append(c[:idx:idx], c[idx+1:]...)
		if j > 0 {
			setChild(hops[j-1], shorter)
		}
	}
}

// Removes the value at a dotted path and every map it leaves empty, reporting whether it existed
func (t Tree) Unset(path string) bool {
	parents, container, key, ok := locate(map[string]any(t), SplitPath(path))
	if !ok {
		return false
	}
	hops := append(parents, hop{container, key})
	deleteChild(hops, len(hops)-1)
	cur := container
	for j := len(parents) - 1; j >= 0; j-- {
		if m, isMap := cur.(map[string]any); !isMap || len(m) != 0 {
			break
		}
		deleteChild(hops, j)
		cur = parents[j].container
	}
	return true
}

// Stores value under the hop
func setChild(h hop, value any) {
	switch c := h.container.(type) {
	case []any:
		c[h.key.(int)] = value
	case map[string]any:
		c[h.key.(string)] = value
	}
}

// String at a path, the fallback when absent or not a string
func (t Tree) String(path, fallback string) string {
	switch v := t.Get(path).(type) {
	case string:
		return v
	case nil:
		return fallback
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return fallback
}

// Integer at a path, the fallback when absent or not numeric
func (t Tree) Int(path string, fallback int) int {
	if n, ok := AsInt(t.Get(path)); ok {
		return n
	}
	return fallback
}

// Float at a path, the fallback when absent or not numeric
func (t Tree) Float(path string, fallback float64) float64 {
	if f, ok := AsFloat(t.Get(path)); ok {
		return f
	}
	return fallback
}

// Boolean at a path, the fallback when absent or not boolean like
func (t Tree) Bool(path string, fallback bool) bool {
	if b, ok := AsBool(t.Get(path)); ok {
		return b
	}
	return fallback
}

// String list at a path, nil when absent
func (t Tree) Strings(path string) []string {
	return AsStrings(t.Get(path))
}

// Map at a path, nil when absent or not a map
func (t Tree) Map(path string) map[string]any {
	return AsMap(t.Get(path))
}

// Subtree at a path, an empty tree when absent
func (t Tree) Sub(path string) Tree {
	m := t.Map(path)
	if m == nil {
		return Tree{}
	}
	return Tree(m)
}

// Reports whether a path exists even when its value is null
func (t Tree) Has(path string) bool {
	_, ok := t.Lookup(path)
	return ok
}

// Integer view of a YAML scalar
func AsInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case uint64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i, true
		}
	}
	return 0, false
}

// Float view of a YAML scalar
func AsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// Boolean view of a YAML scalar accepting the usual truthy and falsy spellings
func AsBool(v any) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case int:
		return b != 0, true
	case int64:
		return b != 0, true
	case float64:
		return b != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "1", "true", "yes", "on", "y":
			return true, true
		case "0", "false", "no", "off", "n", "":
			return false, true
		}
	}
	return false, false
}

// Reports whether a value spells a truthy setting
func Truthy(v any) bool {
	b, ok := AsBool(v)
	return ok && b
}

// String list view of a list or a comma separated string
func AsStrings(v any) []string {
	switch l := v.(type) {
	case []any:
		out := make([]string, 0, len(l))
		for _, item := range l {
			if s, ok := item.(string); ok {
				out = append(out, s)
			} else if item != nil {
				out = append(out, fmt.Sprint(item))
			}
		}
		return out
	case []string:
		return append([]string(nil), l...)
	case string:
		if strings.TrimSpace(l) == "" {
			return nil
		}
		var out []string
		for _, part := range strings.Split(l, ",") {
			if s := strings.TrimSpace(part); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Map view of a value
func AsMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case Tree:
		return map[string]any(m)
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprint(k)] = val
		}
		return out
	}
	return nil
}

// Deep copy of a YAML shaped value
func Clone(v any) any {
	switch c := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(c))
		for k, val := range c {
			out[k] = Clone(val)
		}
		copyMeta(out, c)
		return out
	case Tree:
		return Tree(Clone(map[string]any(c)).(map[string]any))
	case []any:
		out := make([]any, len(c))
		for i, val := range c {
			out[i] = Clone(val)
		}
		return out
	case []string:
		return append([]string(nil), c...)
	}
	return v
}

// Copy of the tree
func (t Tree) Clone() Tree {
	return Clone(t).(Tree)
}

// Override merged into base, maps recursing and a null over a map section ignored
func DeepMerge(base, override map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(override))
	order := MappingKeys(base)
	for k, v := range base {
		result[k] = v
	}
	for _, k := range MappingKeys(override) {
		v := override[k]
		if _, had := result[k]; !had {
			order = append(order, k)
		}
		existing, overDict := result[k].(map[string]any)
		if overDict {
			if vm, ok := v.(map[string]any); ok {
				result[k] = DeepMerge(existing, vm)
				continue
			}
			if v == nil {
				continue
			}
		}
		result[k] = v
	}
	SetMappingKeys(result, order)
	return result
}

// Leaf paths present in a raw document
func ExplicitPaths(doc map[string]any) map[string]bool {
	paths := map[string]bool{}
	var walk func(v any, path []string)
	walk = func(v any, path []string) {
		if m, ok := v.(map[string]any); ok {
			for k, child := range m {
				walk(child, append(append([]string(nil), path...), k))
			}
			return
		}
		if len(path) > 0 {
			paths[joinPath(path)] = true
		}
	}
	walk(doc, nil)
	return paths
}

// Dotted path with literal dots escaped
func joinPath(parts []string) string {
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = strings.ReplaceAll(p, ".", `\.`)
	}
	return strings.Join(escaped, ".")
}

// Document without values equal to the defaults, preserved paths kept even when equal
func StripDefaults(doc, defaults map[string]any, preserve map[string]bool) map[string]any {
	keep := map[string]bool{"_config_version": true}
	for k := range preserve {
		keep[k] = true
	}
	stripped, dropped := stripValue(doc, defaults, nil, keep)
	if dropped {
		return map[string]any{}
	}
	return stripped.(map[string]any)
}

func stripValue(value, def any, path []string, keep map[string]bool) (any, bool) {
	if len(path) > 0 && keep[joinPath(path)] {
		return Clone(value), false
	}
	if m, ok := value.(map[string]any); ok && len(m) > 0 {
		defMap, _ := def.(map[string]any)
		out := map[string]any{}
		var order []string
		for _, k := range MappingKeys(m) {
			child, dropped := stripValue(m[k], defMap[k], append(append([]string(nil), path...), k), keep)
			if !dropped {
				out[k] = child
				order = append(order, k)
			}
		}
		if len(out) == 0 {
			return nil, true
		}
		SetMappingKeys(out, order)
		return out, false
	}
	if pyEq(value, def) {
		return nil, true
	}
	return Clone(value), false
}

// Reports whether two YAML shaped values are equal, numbers compared by value
func Equal(a, b any) bool {
	if af, ok := AsFloat(a); ok {
		if bf, ok := AsFloat(b); ok {
			_, as := a.(string)
			_, bs := b.(string)
			if !as && !bs {
				return af == bf
			}
		}
	}
	return reflect.DeepEqual(a, b)
}

// Sorted keys of a map
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
