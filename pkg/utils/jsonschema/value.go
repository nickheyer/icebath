package jsonschemautil

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"math"
	"math/big"
	"slices"
	"strconv"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Deepest nesting of a schema document the compiler copies
const maxDocumentDepth = 10000

// Value in a shape the validator reads directly: the decoded JSON shapes and Go numbers as they are, a nil *jsonutil.Object as null, any other Go value through its JSON encoding
func plain(v any) any {
	switch x := v.(type) {
	case nil, bool, string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64,
		json.Number, []any, map[string]any:
		return v
	case *jsonutil.Object:
		if x == nil {
			return nil
		}
		return v
	}
	text, err := jsonutil.Encode(v)
	if err != nil {
		return v
	}
	out, err := jsonutil.Decode(text)
	if err != nil {
		return v
	}
	return out
}

// Name of the JSON kind of a value for messages
func kindName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any, *jsonutil.Object:
		return "an object"
	}
	if _, ok := numberOf(v); ok {
		return "a number"
	}
	return fmt.Sprintf("a %T", v)
}

// JSON number: exact in i when it is an integer in the int64 range, else the float f; dec keeps the decimal text of a json.Number or the digits of a uint64 that f only approximates, for exact arithmetic
type number struct {
	i     int64
	f     float64
	exact bool
	dec   string
	// dec spells an integer beyond int64 that no float64 equals, so f alone cannot stand for it in a set of values
	wide bool
}

// Longest decimal text kept for exact arithmetic; longer json.Number text is read as its float64
const maxDecimalText = 1000

// Number a value holds, false for anything that is not a number
func numberOf(v any) (number, bool) {
	switch x := v.(type) {
	case int:
		return number{i: int64(x), exact: true}, true
	case int8:
		return number{i: int64(x), exact: true}, true
	case int16:
		return number{i: int64(x), exact: true}, true
	case int32:
		return number{i: int64(x), exact: true}, true
	case int64:
		return number{i: x, exact: true}, true
	case uint:
		return unsignedNumber(uint64(x)), true
	case uint8:
		return number{i: int64(x), exact: true}, true
	case uint16:
		return number{i: int64(x), exact: true}, true
	case uint32:
		return number{i: int64(x), exact: true}, true
	case uint64:
		return unsignedNumber(x), true
	case float32:
		f, err := strconv.ParseFloat(strconv.FormatFloat(float64(x), 'g', -1, 32), 64)
		if err != nil {
			f = float64(x)
		}
		return floatNumber(f), true
	case float64:
		return floatNumber(x), true
	case json.Number:
		return decimalNumber(string(x))
	}
	return number{}, false
}

// Number a json.Number spells, false for text that is not a number: an integer exact whatever its size, other text as its float64 with the text kept for exact arithmetic
func decimalNumber(text string) (number, bool) {
	i, err := strconv.ParseInt(text, 10, 64)
	if err == nil {
		return number{i: i, exact: true}, true
	}
	if errors.Is(err, strconv.ErrRange) {
		f, _ := strconv.ParseFloat(text, 64)
		n := number{f: f}
		if !math.IsInf(f, 0) && len(text) <= maxDecimalText {
			if b, ok := new(big.Int).SetString(text, 10); ok {
				n.dec = b.String()
				_, acc := new(big.Float).SetInt(b).Float64()
				n.wide = acc != big.Exact
			}
		}
		return n, true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return number{}, false
	}
	n := floatNumber(f)
	if !n.exact && !math.IsInf(f, 0) && len(text) <= maxDecimalText {
		n.dec = text
	}
	return n, true
}

func unsignedNumber(u uint64) number {
	if u <= math.MaxInt64 {
		return number{i: int64(u), exact: true}
	}
	n := number{f: float64(u)}
	if n.f >= 0x1p64 || uint64(n.f) != u {
		n.dec, n.wide = strconv.FormatUint(u, 10), true
	}
	return n
}

// Number of a float, exact when it holds an integer in the int64 range
func floatNumber(f float64) number {
	if f >= -0x1p63 && f < 0x1p63 && f == math.Trunc(f) {
		return number{i: int64(f), f: f, exact: true}
	}
	return number{f: f}
}

func (n number) float() float64 {
	if n.exact {
		return float64(n.i)
	}
	return n.f
}

// Whether the number has no fractional part
func (n number) isInteger() bool {
	return n.exact || !math.IsInf(n.f, 0) && n.f == math.Trunc(n.f)
}

// Order of two numbers, false when either is NaN; one with decimal text is compared exactly, a float read as the shortest decimal that spells it
func (n number) compare(o number) (int, bool) {
	if n.exact && o.exact {
		return cmp.Compare(n.i, o.i), true
	}
	x, y := n.float(), o.float()
	if math.IsNaN(x) || math.IsNaN(y) {
		return 0, false
	}
	if n.dec != "" || o.dec != "" {
		if a, b := n.rat(), o.rat(); a != nil && b != nil {
			return a.Cmp(b), true
		}
	}
	c := cmp.Compare(x, y)
	if c == 0 && n.exact != o.exact {
		// The float lies outside the int64 range and the int64 rounds up to it (float64(MaxInt64) is 2^63), so the int64 is the smaller
		if n.exact {
			return -1, true
		}
		return 1, true
	}
	return c, true
}

// Exact value of the number as written in decimal, nil for NaN and the infinities
func (n number) rat() *big.Rat {
	if n.exact {
		return new(big.Rat).SetInt64(n.i)
	}
	if math.IsNaN(n.f) || math.IsInf(n.f, 0) {
		return nil
	}
	text := n.dec
	if text == "" {
		text = strconv.FormatFloat(n.f, 'g', -1, 64)
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil
	}
	return r
}

// Whether the number is a whole multiple of d, whose exact value is dr, both compared as the decimals they are written as
func (n number) multipleOf(d number, dr *big.Rat) bool {
	if n.exact && d.exact {
		return d.i != 0 && n.i%d.i == 0
	}
	r := n.rat()
	if r == nil || dr == nil || dr.Sign() == 0 {
		return false
	}
	return new(big.Rat).Quo(r, dr).IsInt()
}

// Map key two numbers share exactly when they are equal
type numberKey struct {
	i     int64
	f     float64
	exact bool
	dec   string
}

func (n number) key() numberKey {
	switch {
	case n.exact:
		return numberKey{i: n.i, exact: true}
	case n.wide:
		return numberKey{f: n.f, dec: n.dec}
	}
	return numberKey{f: n.f}
}

// Read access to a JSON object held as *jsonutil.Object or map[string]any
type object struct {
	o *jsonutil.Object
	m map[string]any
}

func objectOf(v any) (object, bool) {
	switch x := v.(type) {
	case *jsonutil.Object:
		return object{o: x}, x != nil
	case map[string]any:
		return object{m: x}, true
	}
	return object{}, false
}

func (x object) len() int {
	if x.o != nil {
		return x.o.Len()
	}
	return len(x.m)
}

func (x object) get(key string) (any, bool) {
	if x.o != nil {
		return x.o.Get(key)
	}
	v, ok := x.m[key]
	return v, ok
}

// Keys in document order, a plain map's in sorted order; the slice must not be modified
func (x object) keys() []string {
	if x.o != nil {
		return x.o.Keys()
	}
	return slices.Sorted(maps.Keys(x.m))
}

// Members in document order, a plain map's in key order
func (x object) all() iter.Seq2[string, any] {
	if x.o != nil {
		return x.o.All()
	}
	return func(yield func(string, any) bool) {
		for _, k := range slices.Sorted(maps.Keys(x.m)) {
			if !yield(k, x.m[k]) {
				return
			}
		}
	}
}

// Set of JSON values: strings, numbers, booleans and null hashed, arrays and objects by their canonical key, a container no key spells compared one by one
type valueSet struct {
	null, yes, no bool
	strs          map[string]struct{}
	nums          map[numberKey]struct{}
	keys          map[string]struct{}
	containers    []any
}

func (s *valueSet) add(v any) {
	v = plain(v)
	switch x := v.(type) {
	case nil:
		s.null = true
		return
	case bool:
		if x {
			s.yes = true
		} else {
			s.no = true
		}
		return
	case string:
		if s.strs == nil {
			s.strs = map[string]struct{}{}
		}
		s.strs[x] = struct{}{}
		return
	}
	if n, ok := numberOf(v); ok {
		if !n.exact && math.IsNaN(n.f) {
			return
		}
		if s.nums == nil {
			s.nums = map[numberKey]struct{}{}
		}
		s.nums[n.key()] = struct{}{}
		return
	}
	s.containers = append(s.containers, v)
	if k, ok := appendValueKey(nil, v, 0); ok {
		if s.keys == nil {
			s.keys = map[string]struct{}{}
		}
		s.keys[string(k)] = struct{}{}
	}
}

func (s *valueSet) has(v any) bool {
	switch x := v.(type) {
	case nil:
		return s.null
	case bool:
		if x {
			return s.yes
		}
		return s.no
	case string:
		_, ok := s.strs[x]
		return ok
	}
	if n, ok := numberOf(v); ok {
		_, hit := s.nums[n.key()]
		return hit
	}
	if len(s.containers) == 0 {
		return false
	}
	if k, ok := appendValueKey(nil, v, 0); ok {
		_, hit := s.keys[string(k)]
		return hit
	}
	for _, o := range s.containers {
		if anyutil.Equal(v, o) {
			return true
		}
	}
	return false
}

// Item of an array at its position
type positioned struct {
	at int
	v  any
}

// Positions of the first item equal to an earlier one and of that earlier one, false when every item differs
func firstDuplicate(items []any) (int, int, bool) {
	null, yes, no := -1, -1, -1
	var strs map[string]int
	var nums map[numberKey]int
	var keys map[string]int
	// Every array and object so far, and those of them no key spells
	var containers, loose []positioned
	var buf []byte
	seen := func(at *int, j int) (int, bool) {
		if *at >= 0 {
			return *at, true
		}
		*at = j
		return 0, false
	}
	for j, raw := range items {
		v := plain(raw)
		switch x := v.(type) {
		case nil:
			if i, dup := seen(&null, j); dup {
				return i, j, true
			}
			continue
		case bool:
			at := &no
			if x {
				at = &yes
			}
			if i, dup := seen(at, j); dup {
				return i, j, true
			}
			continue
		case string:
			if i, dup := strs[x]; dup {
				return i, j, true
			}
			if strs == nil {
				strs = map[string]int{}
			}
			strs[x] = j
			continue
		}
		if n, ok := numberOf(v); ok {
			if !n.exact && math.IsNaN(n.f) {
				continue
			}
			k := n.key()
			if i, dup := nums[k]; dup {
				return i, j, true
			}
			if nums == nil {
				nums = map[numberKey]int{}
			}
			nums[k] = j
			continue
		}
		var keyed bool
		buf, keyed = appendValueKey(buf[:0], v, 0)
		if keyed {
			if i, dup := keys[string(buf)]; dup {
				return i, j, true
			}
		}
		against := loose
		if !keyed {
			against = containers
		}
		for _, p := range against {
			if anyutil.Equal(p.v, v) {
				return p.at, j, true
			}
		}
		if keyed {
			if keys == nil {
				keys = map[string]int{}
			}
			keys[string(buf)] = j
		} else {
			loose = append(loose, positioned{at: j, v: v})
		}
		containers = append(containers, positioned{at: j, v: v})
	}
	return 0, 0, false
}

// buf with a canonical key of a value appended: two values get the same key exactly when they are equal, numbers by value and objects whatever their key order; false for a value no key spells (NaN, which equals nothing, a Go type outside the JSON shapes, nesting past maxValueDepth)
func appendValueKey(buf []byte, v any, depth int) ([]byte, bool) {
	if depth > maxValueDepth {
		return buf, false
	}
	switch x := v.(type) {
	case nil:
		return append(buf, 'n'), true
	case bool:
		if x {
			return append(buf, 't'), true
		}
		return append(buf, 'f'), true
	case string:
		return appendText(append(buf, 's'), x), true
	case []any:
		buf = append(buf, '[')
		for _, item := range x {
			var ok bool
			if buf, ok = appendValueKey(buf, item, depth+1); !ok {
				return buf, false
			}
		}
		return append(buf, ']'), true
	case *jsonutil.Object, map[string]any:
		obj, ok := objectOf(v)
		if !ok {
			return buf, false
		}
		keys := obj.keys()
		if obj.o != nil && !slices.IsSorted(keys) {
			keys = slices.Sorted(slices.Values(keys))
		}
		buf = append(buf, '{')
		for _, k := range keys {
			val, _ := obj.get(k)
			buf = appendText(buf, k)
			if buf, ok = appendValueKey(buf, val, depth+1); !ok {
				return buf, false
			}
		}
		return append(buf, '}'), true
	}
	n, ok := numberOf(v)
	if !ok || !n.exact && math.IsNaN(n.f) {
		return buf, false
	}
	switch k := n.key(); {
	case k.exact:
		buf = strconv.AppendInt(append(buf, 'i'), k.i, 10)
	case k.dec != "":
		return appendText(append(buf, 'w'), k.dec), true
	default:
		buf = strconv.AppendUint(append(buf, 'd'), math.Float64bits(k.f), 16)
	}
	return append(buf, ';'), true
}

// Deep copy of a schema document: objects as *jsonutil.Object with plain maps in sorted key order, numbers as int64 or float64 or, when only decimal text holds their exact value, json.Number; other Go values through their JSON encoding
func normalize(v any) (any, error) { return normalizeAt(v, 0) }

func normalizeAt(v any, depth int) (any, error) {
	if depth > maxDocumentDepth {
		return nil, errors.New("the schema nests too deeply")
	}
	switch x := v.(type) {
	case nil, bool, string, int64, float64:
		return v, nil
	case *jsonutil.Object:
		if x == nil {
			return nil, nil
		}
		out := jsonutil.NewObject()
		for k, val := range x.All() {
			nv, err := normalizeAt(val, depth+1)
			if err != nil {
				return nil, err
			}
			out.Set(k, nv)
		}
		return out, nil
	case map[string]any:
		out := jsonutil.NewObject()
		for _, k := range slices.Sorted(maps.Keys(x)) {
			nv, err := normalizeAt(x[k], depth+1)
			if err != nil {
				return nil, err
			}
			out.Set(k, nv)
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			nv, err := normalizeAt(item, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	}
	if n, ok := numberOf(v); ok {
		switch {
		case n.exact:
			return n.i, nil
		case n.dec != "":
			return json.Number(n.dec), nil
		}
		return n.f, nil
	}
	text, err := jsonutil.Encode(v)
	if err != nil {
		return nil, err
	}
	decoded, err := jsonutil.Decode(text)
	if err != nil {
		return nil, err
	}
	return normalizeAt(decoded, depth+1)
}
