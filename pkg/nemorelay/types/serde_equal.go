package types

import (
	"reflect"
	"time"
)

// serdeEqualer types compare themselves the way their Rust PartialEq does
type serdeEqualer interface {
	serdeEqual(other any) bool
}

var serdeEqType = reflect.TypeFor[serdeEqualer]()

// Equal compares data model values the way Rust PartialEq does, JSON values with serde_json equality
func Equal(a, b any) bool {
	if a == nil || b == nil {
		return JSONEqual(a, b)
	}
	return equalValues(reflect.ValueOf(a), reflect.ValueOf(b))
}

func equalValues(a, b reflect.Value) bool {
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	if a.Type() != b.Type() {
		if a.Kind() == reflect.Interface || b.Kind() == reflect.Interface {
			return equalValues(unwrapInterface(a), unwrapInterface(b))
		}
		return false
	}
	t := a.Type()
	if t.Implements(serdeEqType) && a.Kind() != reflect.Interface {
		return a.Interface().(serdeEqualer).serdeEqual(b.Interface())
	}
	switch t {
	case timeType:
		return a.Interface().(time.Time).Equal(b.Interface().(time.Time))
	}
	switch a.Kind() {
	case reflect.Interface:
		if a.IsNil() || b.IsNil() {
			if t.NumMethod() == 0 {
				return JSONEqual(nilOrInterface(a), nilOrInterface(b))
			}
			return a.IsNil() == b.IsNil()
		}
		if t.NumMethod() == 0 {
			return JSONEqual(a.Interface(), b.Interface())
		}
		return equalValues(a.Elem(), b.Elem())
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return equalValues(a.Elem(), b.Elem())
	case reflect.Slice:
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !equalValues(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Array:
		for i := 0; i < a.Len(); i++ {
			if !equalValues(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Map:
		if a.Len() != b.Len() {
			return false
		}
		iter := a.MapRange()
		for iter.Next() {
			bv := b.MapIndex(iter.Key())
			if !bv.IsValid() || !equalValues(iter.Value(), bv) {
				return false
			}
		}
		return true
	case reflect.Struct:
		return equalStructs(a, b)
	case reflect.Float32, reflect.Float64:
		return a.Float() == b.Float()
	}
	if a.Type().Comparable() {
		return a.Equal(b)
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

func unwrapInterface(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Interface && !v.IsNil() {
		v = v.Elem()
	}
	return v
}

func nilOrInterface(v reflect.Value) any {
	if v.IsNil() {
		return nil
	}
	return v.Interface()
}

// Compares struct fields honoring option presence rules from serde metadata
func equalStructs(a, b reflect.Value) bool {
	m := metaOf(a.Type())
	for i := range m.fields {
		f := &m.fields[i]
		if f.rustHidden && f.serdeSkip && f.rustName == f.name {
			continue
		}
		av, bv := a.FieldByIndex(f.index), b.FieldByIndex(f.index)
		if f.option && (av.Kind() == reflect.Slice || av.Kind() == reflect.Map || av.Kind() == reflect.Interface) {
			an, bn := f.isNone(av, a), f.isNone(bv, b)
			if f.skipNull {
				an = an || (av.Kind() == reflect.Interface && av.IsNil())
				bn = bn || (bv.Kind() == reflect.Interface && bv.IsNil())
			}
			if an != bn {
				return false
			}
			if an {
				continue
			}
		}
		if !equalValues(av, bv) {
			return false
		}
	}
	for i := 0; i < a.NumField(); i++ {
		sf := a.Type().Field(i)
		if sf.IsExported() {
			continue
		}
		if !equalUnexported(a.Field(i), b.Field(i)) {
			return false
		}
	}
	return true
}

func equalUnexported(a, b reflect.Value) bool {
	if a.Type().Comparable() {
		return a.Equal(b)
	}
	return true
}
