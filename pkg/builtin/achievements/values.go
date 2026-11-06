package achievements

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

type stringSet map[string]struct{}

// Python set of strings built from the given members
func newSet(items ...string) stringSet {
	s := stringSet{}
	for _, it := range items {
		s[it] = struct{}{}
	}
	return s
}

// Members in code point order as sorted renders them
func (s stringSet) sorted() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Ordered dict built from alternating keys and values
func obj(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Value under key or nil, the dict.get analogue
func get(o *pyjson.Object, key string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(key)
	return v
}

// Value under key or def when the key is absent
func getOr(o *pyjson.Object, key string, def any) any {
	if o != nil {
		if v, ok := o.Get(key); ok {
			return v
		}
	}
	return def
}

// Python truthiness including string sets
func truthy(v any) bool {
	if s, ok := v.(stringSet); ok {
		return len(s) > 0
	}
	return pyrepr.Truthy(v)
}

// First truthy value or the last one, the Python or chain
func or(vals ...any) any {
	for _, v := range vals[:len(vals)-1] {
		if truthy(v) {
			return v
		}
	}
	return vals[len(vals)-1]
}

// Later dicts layered over earlier ones keeping first insertion order, the {**a, **b} analogue
func merge(objs ...*pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, o := range objs {
		if o == nil {
			continue
		}
		for _, k := range o.Keys {
			out.Set(k, o.Vals[k])
		}
	}
	return out
}

// Python int of a value as an int64 with the int conversion error
func intOf(v any) (int64, error) {
	if _, ok := v.(stringSet); ok {
		return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not 'set'")
	}
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("Python int too large to convert to C long")
	}
	return n.Int64(), nil
}

// Python int(v or 0)
func intOr0(v any) (int64, error) {
	if !truthy(v) {
		return 0, nil
	}
	return intOf(v)
}

// Number of a value for ordering comparisons, zero for values that are not numbers
func numKey(v any) float64 {
	f, err := pyint.Float(v)
	if err != nil {
		return 0
	}
	return f
}

// Python iteration members of a value, failing on objects that are not iterable
func iterate(v any) ([]any, error) {
	switch t := v.(type) {
	case nil:
		return nil, fmt.Errorf("'NoneType' object is not iterable")
	case []any:
		return t, nil
	case *pyjson.Object:
		out := make([]any, 0, len(t.Keys))
		for _, k := range t.Keys {
			out = append(out, k)
		}
		return out, nil
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case stringSet:
		out := []any{}
		for _, k := range t.sorted() {
			out = append(out, k)
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(v))
}

// Sets rendered as sorted lists and dicts and lists copied recursively
func jsonSafe(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range t.Keys {
			out.Set(k, jsonSafe(t.Vals[k]))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = jsonSafe(x)
		}
		return out
	case []*pyjson.Object:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = jsonSafe(x)
		}
		return out
	case stringSet:
		out := []any{}
		for _, k := range t.sorted() {
			out = append(out, k)
		}
		return out
	}
	return v
}

// Python re pattern compiled with Python semantics, panicking on a pattern re rejects
func mustRE(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Pattern compiled with re.IGNORECASE
func ci(pattern string) *regexp2.Regexp { return mustRE("(?i)" + pattern) }

// Whether the pattern matches anywhere, the re.search analogue
func search(re *regexp2.Regexp, s string) bool {
	ok, _ := re.MatchString(s)
	return ok
}

// Every non overlapping match text, the re.findall analogue for a pattern without groups
func findAll(re *regexp2.Regexp, s string) []string {
	var out []string
	m, _ := re.FindStringMatch(s)
	for m != nil {
		out = append(out, m.String())
		m, _ = re.FindNextMatch(m)
	}
	return out
}

// Text of a file read as utf-8-sig with universal newlines, failing on undecodable bytes
func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("'utf-8' codec can't decode %s", path)
	}
	s := strings.TrimPrefix(string(b), "\xef\xbb\xbf")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n"), nil
}

// Writes text as utf-8 with the platform line separator, the Path.write_text analogue
func writeText(path, s string) error {
	if runtime.GOOS == "windows" {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	return os.WriteFile(path, []byte(s), 0o666)
}

// Removes a file, treating a missing file as success
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
