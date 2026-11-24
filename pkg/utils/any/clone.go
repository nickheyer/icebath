package anyutil

import (
	"encoding/json"
	"reflect"
	"unsafe"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Deep copy of a value through maps, slices, arrays, pointers, objects and exported struct fields, keeping shared references shared and cycles intact
func Clone[T any](v T) T {
	var c cloner
	out, _ := c.value(any(v)).(T)
	return out
}

type memoKey struct {
	ptr unsafe.Pointer
	typ reflect.Type
	n   int
}

// One deep copy in progress, remembering each reference already copied
type cloner struct{ memo map[memoKey]any }

func (c *cloner) remember(key memoKey, v any) {
	if c.memo == nil {
		c.memo = make(map[memoKey]any)
	}
	c.memo[key] = v
}

var (
	plainMapType   = reflect.TypeFor[map[string]any]()
	plainSliceType = reflect.TypeFor[[]any]()
	objectType     = reflect.TypeFor[*jsonutil.Object]()
)

func (c *cloner) value(v any) any {
	switch x := v.(type) {
	case nil, bool, string, int, int64, int32, uint64, float64, float32, json.Number:
		return v
	case map[string]any:
		return c.plainMap(x)
	case []any:
		return c.plainSlice(x)
	case *jsonutil.Object:
		return c.object(x)
	}
	return c.reflectValue(reflect.ValueOf(v)).Interface()
}

func (c *cloner) plainMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	key := memoKey{reflect.ValueOf(m).UnsafePointer(), plainMapType, -1}
	if done, ok := c.memo[key]; ok {
		return done.(map[string]any)
	}
	out := make(map[string]any, len(m))
	c.remember(key, out)
	for k, v := range m {
		out[k] = c.value(v)
	}
	return out
}

func (c *cloner) plainSlice(s []any) []any {
	if s == nil {
		return nil
	}
	if len(s) == 0 {
		return []any{}
	}
	key := memoKey{unsafe.Pointer(unsafe.SliceData(s)), plainSliceType, len(s)}
	if done, ok := c.memo[key]; ok {
		return done.([]any)
	}
	out := make([]any, len(s))
	c.remember(key, out)
	for i, v := range s {
		out[i] = c.value(v)
	}
	return out
}

// Copy of an object built entry by entry so that its key order and inner bookkeeping stay its own
func (c *cloner) object(o *jsonutil.Object) *jsonutil.Object {
	if o == nil {
		return nil
	}
	key := memoKey{unsafe.Pointer(o), objectType, -1}
	if done, ok := c.memo[key]; ok {
		return done.(*jsonutil.Object)
	}
	out := jsonutil.NewObject()
	c.remember(key, out)
	for k, v := range o.All() {
		out.Set(k, c.value(v))
	}
	return out
}

func (c *cloner) reflectValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(c.reflectValue(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		if v.Type() == plainMapType && v.CanInterface() {
			return reflect.ValueOf(c.plainMap(v.Interface().(map[string]any)))
		}
		key := memoKey{v.UnsafePointer(), v.Type(), -1}
		if done, ok := c.memo[key]; ok {
			return reflect.ValueOf(done)
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		c.remember(key, out.Interface())
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(it.Key(), c.reflectValue(it.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		if v.Type() == plainSliceType && v.CanInterface() {
			return reflect.ValueOf(c.plainSlice(v.Interface().([]any)))
		}
		n := v.Len()
		key := memoKey{v.UnsafePointer(), v.Type(), n}
		if done, ok := c.memo[key]; ok && n > 0 {
			return reflect.ValueOf(done)
		}
		out := reflect.MakeSlice(v.Type(), n, n)
		if n == 0 {
			return out
		}
		c.remember(key, out.Interface())
		if flat(v.Type().Elem().Kind()) {
			reflect.Copy(out, v)
			return out
		}
		for i := range n {
			out.Index(i).Set(c.reflectValue(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		if flat(v.Type().Elem().Kind()) {
			out.Set(v)
			return out
		}
		for i := range v.Len() {
			out.Index(i).Set(c.reflectValue(v.Index(i)))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		if v.Type() == objectType && v.CanInterface() {
			return reflect.ValueOf(c.object(v.Interface().(*jsonutil.Object)))
		}
		key := memoKey{v.UnsafePointer(), v.Type(), -1}
		if done, ok := c.memo[key]; ok {
			return reflect.ValueOf(done)
		}
		out := reflect.New(v.Type().Elem())
		c.remember(key, out.Interface())
		out.Elem().Set(c.reflectValue(v.Elem()))
		return out
	case reflect.Struct:
		if v.Type() == objectType.Elem() && v.CanInterface() {
			o, _ := reflect.TypeAssert[jsonutil.Object](v)
			return reflect.ValueOf(c.object(&o)).Elem()
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if field := out.Field(i); field.CanSet() {
				field.Set(c.reflectValue(v.Field(i)))
			}
		}
		return out
	}
	return v
}

// Whether values of a kind hold no references, so copying them copies everything
func flat(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return true
	}
	return false
}
