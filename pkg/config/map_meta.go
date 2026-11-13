package config

import (
	"reflect"
	"runtime"
	"sort"
	"sync"
	"unsafe"
	"weak"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"gopkg.in/yaml.v3"
)

// Key order and failed read marker carried by one live map, dropped once the map is collected
type mapMeta struct {
	id     uintptr
	owner  weak.Pointer[byte]
	keys   []string
	failed error
	pyKeys map[string]any
}

var (
	mapMetaMu sync.Mutex
	mapMetas  = map[uintptr]*mapMeta{}
	heapSink  struct {
		on bool
		m  map[string]any
	}
)

// Runtime identity of a map, nil for a nil map
func mapIdentity(m map[string]any) unsafe.Pointer {
	if m == nil {
		return nil
	}
	return reflect.ValueOf(m).UnsafePointer()
}

// Metadata recorded for a live map, nil when none was recorded or the address was reused
func lookupMeta(m map[string]any) *mapMeta {
	p := mapIdentity(m)
	if p == nil {
		return nil
	}
	mapMetaMu.Lock()
	defer mapMetaMu.Unlock()
	meta := mapMetas[uintptr(p)]
	if meta == nil || meta.owner.Value() != (*byte)(p) {
		return nil
	}
	return meta
}

// Fresh metadata attached to a map, replacing anything a dead map left at the same address
func attachMeta(m map[string]any) *mapMeta {
	if heapSink.on {
		heapSink.m = m
	}
	p := mapIdentity(m)
	if p == nil {
		return nil
	}
	mapMetaMu.Lock()
	defer mapMetaMu.Unlock()
	if meta := mapMetas[uintptr(p)]; meta != nil && meta.owner.Value() == (*byte)(p) {
		return meta
	}
	meta := &mapMeta{id: uintptr(p), owner: weak.Make((*byte)(p))}
	mapMetas[uintptr(p)] = meta
	runtime.AddCleanup((*byte)(p), func(m *mapMeta) {
		mapMetaMu.Lock()
		if mapMetas[m.id] == m {
			delete(mapMetas, m.id)
		}
		mapMetaMu.Unlock()
	}, meta)
	return meta
}

// Keys of a mapping in file or insertion order, keys without a recorded position following in sorted order
func MappingKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	seen := make(map[string]bool, len(m))
	if meta := lookupMeta(m); meta != nil {
		mapMetaMu.Lock()
		recorded := append([]string(nil), meta.keys...)
		mapMetaMu.Unlock()
		for _, k := range recorded {
			if _, ok := m[k]; ok && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	rest := make([]string, 0, len(m)-len(out))
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// Records the key order of a mapping so later readers iterate it the way the file or builder did
func SetMappingKeys(m map[string]any, keys []string) {
	if meta := attachMeta(m); meta != nil {
		mapMetaMu.Lock()
		meta.keys = append([]string(nil), keys...)
		mapMetaMu.Unlock()
	}
}

// Moves a key to the end of the mapping order the way a Python dict insert of a new key does
func AppendMappingKey(m map[string]any, key string) {
	keys := MappingKeys(m)
	out := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		if k != key {
			out = append(out, k)
		}
	}
	SetMappingKeys(m, append(out, key))
}

// Marks a mapping as the fail open fallback of an unreadable config, carrying the read error
func markFailedRead(m map[string]any, err error) {
	if meta := attachMeta(m); meta != nil {
		mapMetaMu.Lock()
		meta.failed = err
		mapMetaMu.Unlock()
	}
}

// Read error of a mapping served as the fallback for an unreadable config, nil for ordinary data
func FailedReadError(m map[string]any) error {
	meta := lookupMeta(m)
	if meta == nil {
		return nil
	}
	mapMetaMu.Lock()
	defer mapMetaMu.Unlock()
	return meta.failed
}

// Metadata of src copied onto dst, the way a deep copy keeps a dict subclass and its order
func copyMeta(dst, src map[string]any) {
	meta := lookupMeta(src)
	if meta == nil {
		return
	}
	mapMetaMu.Lock()
	keys, failed := append([]string(nil), meta.keys...), meta.failed
	pyKeys := make(map[string]any, len(meta.pyKeys))
	for k, v := range meta.pyKeys {
		pyKeys[k] = v
	}
	mapMetaMu.Unlock()
	SetMappingKeys(dst, keys)
	if failed != nil {
		markFailedRead(dst, failed)
	}
	for k, v := range pyKeys {
		SetMappingPyKey(dst, k, v)
	}
}

// Records that a string key of a mapping stands for a non str Python key such as True, 1 or None
func SetMappingPyKey(m map[string]any, key string, pyKey any) {
	if meta := attachMeta(m); meta != nil {
		mapMetaMu.Lock()
		if meta.pyKeys == nil {
			meta.pyKeys = map[string]any{}
		}
		meta.pyKeys[key] = pyKey
		mapMetaMu.Unlock()
	}
}

// Non str Python key a string key of a mapping stands for, false for an ordinary str key
func MappingPyKey(m map[string]any, key string) (any, bool) {
	meta := lookupMeta(m)
	if meta == nil {
		return nil, false
	}
	mapMetaMu.Lock()
	defer mapMetaMu.Unlock()
	v, ok := meta.pyKeys[key]
	return v, ok
}

// Map key under which a non str Python key is stored, never equal to a key a YAML str produces
func PyKeyString(pyKey any) string { return "\x00" + pyrepr.Repr(pyKey) }

// Records the file order of every mapping in a decoded document from the node it was decoded from
func recordNodeOrder(node *yaml.Node, value any) {
	for node != nil && (node.Kind == yaml.DocumentNode || node.Kind == yaml.AliasNode) {
		if node.Kind == yaml.DocumentNode {
			if len(node.Content) == 0 {
				return
			}
			node = node.Content[0]
		} else {
			node = node.Alias
		}
	}
	if node == nil {
		return
	}
	switch v := value.(type) {
	case map[string]any:
		if node.Kind != yaml.MappingNode {
			return
		}
		keys := make([]string, 0, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			k := node.Content[i]
			if k.Kind == yaml.ScalarNode && k.Tag == "!!merge" {
				continue
			}
			child, ok := v[k.Value]
			if !ok {
				continue
			}
			keys = append(keys, k.Value)
			recordNodeOrder(node.Content[i+1], child)
		}
		SetMappingKeys(v, keys)
	case []any:
		if node.Kind != yaml.SequenceNode {
			return
		}
		for i := 0; i < len(v) && i < len(node.Content); i++ {
			recordNodeOrder(node.Content[i], v[i])
		}
	}
}
