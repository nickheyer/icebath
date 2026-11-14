package roundtrip

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

type missing struct{}

// Absent value sentinel distinct from an explicit null
var Missing any = missing{}

// Comparable map key of a config path
func Key(path ...string) string { return strings.Join(path, "\x00") }

// Path segments of a key built by Key
func Split(key string) []string {
	if key == "" {
		return nil
	}
	return strings.Split(key, "\x00")
}

// Flattened tree keyed by path where an empty mapping or any non mapping value is a leaf
func Leaves(tree any) map[string]any {
	out := map[string]any{}
	var walk func(v any, prefix []string)
	walk = func(v any, prefix []string) {
		if m, ok := v.(map[string]any); ok && len(m) > 0 {
			for k, child := range m {
				walk(child, append(slices.Clone(prefix), k))
			}
			return
		}
		out[Key(prefix...)] = v
	}
	walk(tree, nil)
	return out
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "none"
	case bool:
		return "bool"
	case int, int64, int32, uint64:
		return "int"
	case float64, float32:
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	}
	return 0, false
}

// Python equality, numbers compared across bool int and float and containers element wise
func Equal(a, b any) bool {
	if a == Missing || b == Missing {
		return a == Missing && b == Missing
	}
	if x, ok := number(a); ok {
		y, ok := number(b)
		return ok && x == y
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, has := y[k]
			if !has || !Equal(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

// Equal values of the same Python type
func Same(a, b any) bool { return kind(a) == kind(b) && Equal(a, b) }

// Paths whose leaf differs in value or type between two trees
func ChangedPaths(before, after any) map[string]bool {
	a, b := Leaves(before), Leaves(after)
	out := map[string]bool{}
	for k, av := range a {
		if bv, ok := b[k]; !ok || !Same(av, bv) {
			out[k] = true
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out[k] = true
		}
	}
	return out
}

// Sorted readable form of a path set
func Paths(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, strings.Join(Split(k), "."))
	}
	sort.Strings(out)
	return out
}

// Whether a path is a target, lies below one, or is an ancestor a target write replaced
func Under(path []string, targets [][]string) bool {
	for _, t := range targets {
		n := min(len(path), len(t))
		if slices.Equal(path[:n], t[:n]) {
			return true
		}
	}
	return false
}

// Value at a path of a plain tree, Missing when a hop is absent or not a mapping
func Get(tree any, path []string) any {
	cur := tree
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return Missing
		}
		v, has := m[p]
		if !has {
			return Missing
		}
		cur = v
	}
	return cur
}

// Sets a value at a path of a plain tree replacing any non mapping hop with a mapping
func Set(tree map[string]any, path []string, value any) {
	cur := tree
	for _, p := range path[:len(path)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = value
}

// Deep copy of plain maps and lists
func Clone(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = Clone(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Clone(e)
		}
		return out
	}
	return v
}

var topLevelKey = regexp.MustCompile(`(?m)^([^\s#\-][^:\n]*):`)

// Error naming every top level key the text declares more than once
func NoDuplicateTopLevel(text, ctx string) error {
	seen, dupes := map[string]int{}, []string{}
	for _, m := range topLevelKey.FindAllStringSubmatch(text, -1) {
		seen[m[1]]++
		if seen[m[1]] == 2 {
			dupes = append(dupes, m[1])
		}
	}
	if len(dupes) > 0 {
		sort.Strings(dupes)
		return fmt.Errorf("[%s] duplicated top-level keys %v in:\n%s", ctx, dupes, text)
	}
	return nil
}
