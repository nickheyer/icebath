package types

import (
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var rustDebuggerType = reflect.TypeFor[rustDebugger]()

// RustDebug renders a data model value as Rust Debug formatting with {:?} writes it
func RustDebug(v any) string {
	var b strings.Builder
	writeRustDebug(&b, reflect.ValueOf(v), false)
	return b.String()
}

// RustJSONDebug renders a JSON value as serde_json Value Debug writes it
func RustJSONDebug(v any) string {
	var b strings.Builder
	writeJSONDebug(&b, v)
	return b.String()
}

func writeJSONDebug(b *strings.Builder, v any) {
	switch x := normalizeJSON(v).(type) {
	case nil:
		b.WriteString("Null")
	case bool:
		b.WriteString("Bool(" + strconv.FormatBool(x) + ")")
	case string:
		b.WriteString("String(" + RustStrDebug(x) + ")")
	case []any:
		b.WriteString("Array [")
		for i, e := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writeJSONDebug(b, e)
		}
		b.WriteByte(']')
	case map[string]any:
		b.WriteString("Object {")
		for i, k := range sortedMapKeys(x) {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(RustStrDebug(k))
			b.WriteString(": ")
			writeJSONDebug(b, x[k])
		}
		b.WriteByte('}')
	default:
		if n, ok := classifyNumber(x); ok {
			b.WriteString("Number(" + n.String() + ")")
			return
		}
		b.WriteString(RustStrDebug(RustDebug(x)))
	}
}

// Writes a reflected value as Rust Debug, treating empty interfaces as JSON values
func writeRustDebug(b *strings.Builder, rv reflect.Value, jsonContext bool) {
	if !rv.IsValid() {
		if jsonContext {
			b.WriteString("Null")
		} else {
			b.WriteString("None")
		}
		return
	}
	t := rv.Type()
	if t.Implements(rustDebuggerType) && !(rv.Kind() == reflect.Interface && rv.IsNil()) && !(rv.Kind() == reflect.Pointer && rv.IsNil()) {
		rv.Interface().(rustDebugger).rustDebug(b)
		return
	}
	switch t {
	case uuidType:
		b.WriteString(rv.Interface().(uuid.UUID).String())
		return
	case timeType:
		writeChronoDebug(b, rv.Interface().(time.Time))
		return
	}
	switch rv.Kind() {
	case reflect.Interface:
		if t.NumMethod() == 0 {
			writeJSONDebug(b, nilOrInterface(rv))
			return
		}
		if rv.IsNil() {
			b.WriteString("None")
			return
		}
		writeRustDebug(b, rv.Elem(), false)
	case reflect.Pointer:
		if rv.IsNil() {
			b.WriteString("None")
			return
		}
		writeRustDebug(b, rv.Elem(), false)
	case reflect.Bool:
		b.WriteString(strconv.FormatBool(rv.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(rv.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(strconv.FormatUint(rv.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		b.WriteString(RustF64Debug(rv.Float()))
	case reflect.String:
		b.WriteString(RustStrDebug(rv.String()))
	case reflect.Slice, reflect.Array:
		b.WriteByte('[')
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRustDebug(b, rv.Index(i), false)
		}
		b.WriteByte(']')
	case reflect.Map:
		keys, _ := sortedReflectKeys(rv)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writeRustDebug(b, k.value, false)
			b.WriteString(": ")
			writeRustDebug(b, rv.MapIndex(k.value), false)
		}
		b.WriteByte('}')
	case reflect.Struct:
		writeStructDebug(b, rv)
	default:
		b.WriteString(rv.String())
	}
}

// Writes a struct as Name { field: value, ... } with Option wrapping for optional fields
func writeStructDebug(b *strings.Builder, rv reflect.Value) {
	m := metaOf(rv.Type())
	b.WriteString(m.rustName)
	wrote := false
	for i := range m.fields {
		f := &m.fields[i]
		if f.rustHidden {
			continue
		}
		if !wrote {
			b.WriteString(" { ")
		} else {
			b.WriteString(", ")
		}
		wrote = true
		b.WriteString(f.rustName)
		b.WriteString(": ")
		fv := rv.FieldByIndex(f.index)
		writeFieldDebug(b, f, fv, rv)
	}
	if wrote {
		b.WriteString(" }")
	}
}

func writeFieldDebug(b *strings.Builder, f *fieldMeta, fv reflect.Value, parent reflect.Value) {
	if !f.option {
		writeRustDebug(b, fv, fv.Kind() == reflect.Interface)
		return
	}
	if f.isNone(fv, parent) {
		b.WriteString("None")
		return
	}
	b.WriteString("Some(")
	switch fv.Kind() {
	case reflect.Pointer:
		writeRustDebug(b, fv.Elem(), false)
	default:
		writeRustDebug(b, fv, fv.Kind() == reflect.Interface)
	}
	b.WriteByte(')')
}

// Writes a UTC time as chrono DateTime<Utc> Debug writes it
func writeChronoDebug(b *strings.Builder, t time.Time) {
	s := formatRFC3339(t, true, false)
	b.WriteString(s)
}

// Writes Some(value) or None for an optional Debug value
func writeOptionDebug(b *strings.Builder, present bool, value any) {
	if !present {
		b.WriteString("None")
		return
	}
	b.WriteString("Some(")
	writeRustDebug(b, reflect.ValueOf(value), false)
	b.WriteByte(')')
}
