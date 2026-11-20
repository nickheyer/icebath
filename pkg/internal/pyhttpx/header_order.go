package pyhttpx

import (
	"reflect"
	"runtime"
	"sort"
	"sync"
	"unsafe"
	"weak"
)

// Declaration order recorded for one live header map, dropped once the map is collected
type headerOrder struct {
	id    uintptr
	owner weak.Pointer[byte]
	keys  []string
}

var (
	headerOrdersMu sync.Mutex
	headerOrders   = map[uintptr]*headerOrder{}
)

// Runtime identity of a map, nil for a nil map
func headerMapIdentity(m map[string]string) unsafe.Pointer {
	if m == nil {
		return nil
	}
	return reflect.ValueOf(m).UnsafePointer()
}

// Records the order a Python dict holding these headers would iterate them in
func SetHeaderOrder(m map[string]string, keys []string) {
	p := headerMapIdentity(m)
	if p == nil {
		return
	}
	headerOrdersMu.Lock()
	defer headerOrdersMu.Unlock()
	if o := headerOrders[uintptr(p)]; o != nil && o.owner.Value() == (*byte)(p) {
		o.keys = append([]string(nil), keys...)
		return
	}
	o := &headerOrder{id: uintptr(p), owner: weak.Make((*byte)(p)), keys: append([]string(nil), keys...)}
	headerOrders[uintptr(p)] = o
	runtime.AddCleanup((*byte)(p), func(o *headerOrder) {
		headerOrdersMu.Lock()
		if headerOrders[o.id] == o {
			delete(headerOrders, o.id)
		}
		headerOrdersMu.Unlock()
	}, o)
}

// Keys of a header map in recorded order, keys added since following in sorted order the way a later dict insert lands last
func HeaderOrder(m map[string]string) []string {
	out := make([]string, 0, len(m))
	seen := make(map[string]bool, len(m))
	if p := headerMapIdentity(m); p != nil {
		headerOrdersMu.Lock()
		var recorded []string
		if o := headerOrders[uintptr(p)]; o != nil && o.owner.Value() == (*byte)(p) {
			recorded = append(recorded, o.keys...)
		}
		headerOrdersMu.Unlock()
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

// Header map built from the lines in order, a repeated name keeping its first slot and last value, its order recorded
func OrderedHeaderMap(lines ...HeaderLine) map[string]string {
	m := make(map[string]string, len(lines))
	keys := make([]string, 0, len(lines))
	for _, l := range lines {
		if _, ok := m[l.Name]; !ok {
			keys = append(keys, l.Name)
		}
		m[l.Name] = l.Value
	}
	SetHeaderOrder(m, keys)
	return m
}

// Dict merge of the maps left to right, later values winning in the earlier slot and new keys appended, its order recorded
func MergeHeaderMaps(maps ...map[string]string) map[string]string {
	var lines []HeaderLine
	for _, m := range maps {
		for _, k := range HeaderOrder(m) {
			lines = append(lines, HeaderLine{Name: k, Value: m[k]})
		}
	}
	return OrderedHeaderMap(lines...)
}
