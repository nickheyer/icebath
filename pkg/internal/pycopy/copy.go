package pycopy

import (
	"reflect"
	"unsafe"
)

type memoKey struct {
	ptr unsafe.Pointer
	typ reflect.Type
	n   int
}

// Recursive copy of maps, slices, arrays, pointers and structs keeping shared references shared like copy.deepcopy
func DeepCopy(v any) any {
	if v == nil {
		return nil
	}
	memo := map[memoKey]reflect.Value{}
	return deepCopyValue(reflect.ValueOf(v), memo).Interface()
}

// Typed form of DeepCopy for callers that hold a concrete value
func Of[T any](v T) T {
	out, _ := DeepCopy(v).(T)
	return out
}

// One level of the reflect walk behind DeepCopy, consulting the memo for reference kinds
func deepCopyValue(v reflect.Value, memo map[memoKey]reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(deepCopyValue(v.Elem(), memo))
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		key := memoKey{v.UnsafePointer(), v.Type(), -1}
		if done, ok := memo[key]; ok {
			return done
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		memo[key] = out
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), deepCopyValue(iter.Value(), memo))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		key := memoKey{v.UnsafePointer(), v.Type(), v.Len()}
		if done, ok := memo[key]; ok && v.Len() > 0 {
			return done
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		if v.Len() > 0 {
			memo[key] = out
		}
		for i := range v.Len() {
			out.Index(i).Set(deepCopyValue(v.Index(i), memo))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			out.Index(i).Set(deepCopyValue(v.Index(i), memo))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		key := memoKey{v.UnsafePointer(), v.Type(), -1}
		if done, ok := memo[key]; ok {
			return done
		}
		out := reflect.New(v.Type().Elem())
		memo[key] = out
		out.Elem().Set(deepCopyValue(v.Elem(), memo))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if out.Field(i).CanSet() {
				out.Field(i).Set(deepCopyValue(v.Field(i), memo))
			}
		}
		return out
	}
	return v
}
