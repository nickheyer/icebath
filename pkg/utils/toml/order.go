package tomlutil

import (
	"slices"
	"strconv"

	"github.com/pelletier/go-toml/v2/unstable"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Path separators of the key order map: one before each key's length and one between it and the key, one before each array index
const (
	keyMark   = "\x00"
	indexMark = "\x01"
)

// Path of member key of the table at parent, the key's length written first so that no key text, marks included, can spell another path
func keyPath(parent, key string) string {
	return parent + keyMark + strconv.Itoa(len(key)) + keyMark + key
}

// Path of item i of the array at parent
func indexPath(parent string, i int) string {
	return parent + indexMark + strconv.Itoa(i)
}

// First-appearance order of the keys of every table in a document already known to be valid
type orderRecorder struct {
	keys   map[string][]string
	seen   map[string]bool
	tables map[string]int
}

// Key order of every table keyed by its path, each table of an array of tables and each element of an array indexed on its own
func keyOrder(data []byte) map[string][]string {
	r := &orderRecorder{keys: map[string][]string{}, seen: map[string]bool{}, tables: map[string]int{}}
	var p unstable.Parser
	p.Reset(data)
	current := ""
	for p.NextExpression() {
		e := p.Expression()
		switch e.Kind {
		case unstable.Table:
			current = ""
			keys := e.Key()
			for keys.Next() {
				current = r.descend(current, string(keys.Node().Data))
			}
		case unstable.ArrayTable:
			current = r.arrayTable(e)
		case unstable.KeyValue:
			r.keyValue(current, e)
		}
	}
	return r.keys
}

// Path of key under parent, noting the key in parent's order the first time it appears
func (r *orderRecorder) record(parent, key string) string {
	path := keyPath(parent, key)
	if !r.seen[path] {
		r.seen[path] = true
		r.keys[parent] = append(r.keys[parent], key)
	}
	return path
}

// Path of key under parent, the last table of it when key names an array of tables
func (r *orderRecorder) descend(parent, key string) string {
	path := r.record(parent, key)
	if n := r.tables[path]; n > 0 {
		path = indexPath(path, n-1)
	}
	return path
}

// Path of the table a [[header]] opens, a new element of its array
func (r *orderRecorder) arrayTable(e *unstable.Node) string {
	var parts []string
	keys := e.Key()
	for keys.Next() {
		parts = append(parts, string(keys.Node().Data))
	}
	current := ""
	for _, part := range parts[:len(parts)-1] {
		current = r.descend(current, part)
	}
	path := r.record(current, parts[len(parts)-1])
	r.tables[path]++
	return indexPath(path, r.tables[path]-1)
}

// Keys of one key-value expression under base, a dotted key and inline tables included
func (r *orderRecorder) keyValue(base string, kv *unstable.Node) {
	path := base
	keys := kv.Key()
	for keys.Next() {
		path = r.descend(path, string(keys.Node().Data))
	}
	r.value(path, kv.Value())
}

func (r *orderRecorder) value(path string, v *unstable.Node) {
	switch v.Kind {
	case unstable.InlineTable:
		children := v.Children()
		for children.Next() {
			if child := children.Node(); child.Kind == unstable.KeyValue {
				r.keyValue(path, child)
			}
		}
	case unstable.Array:
		children := v.Children()
		for i := 0; children.Next(); i++ {
			r.value(indexPath(path, i), children.Node())
		}
	}
}

// Decoded value with every table rebuilt as an ordered object in the recorded order, keys the order lacks following in sorted order
func ordered(v any, path string, order map[string][]string) any {
	switch t := v.(type) {
	case map[string]any:
		obj := jsonutil.NewObject()
		for _, k := range order[path] {
			if child, ok := t[k]; ok {
				obj.Set(k, ordered(child, keyPath(path, k), order))
			}
		}
		if obj.Len() < len(t) {
			rest := make([]string, 0, len(t)-obj.Len())
			for k := range t {
				if !obj.Has(k) {
					rest = append(rest, k)
				}
			}
			slices.Sort(rest)
			for _, k := range rest {
				obj.Set(k, ordered(t[k], keyPath(path, k), order))
			}
		}
		return obj
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = ordered(item, indexPath(path, i), order)
		}
		return out
	}
	return v
}
