package pyrepr

import "reflect"

// Python is operator, pointer identity for reference kinds, both nil alike and equality for plain values
func Is(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	switch va.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Slice:
		return va.Pointer() == vb.Pointer()
	}
	return va.Type().Comparable() && a == b
}
