package jsonutil

import (
	"cmp"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

type encodeFunc func(e *encoder, v reflect.Value) error

var (
	encoderCache      sync.Map
	fieldCache        sync.Map
	marshalerType     = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
	errorType         = reflect.TypeFor[error]()
	numberType        = reflect.TypeFor[json.Number]()
	objectType        = reflect.TypeFor[Object]()
	objectPtrType     = reflect.TypeFor[*Object]()
	anyMapType        = reflect.TypeFor[map[string]any]()
	anySliceType      = reflect.TypeFor[[]any]()
	stringSliceType   = reflect.TypeFor[[]string]()
	isZeroerType      = reflect.TypeFor[interface{ IsZero() bool }]()
)

func (e *encoder) reflectValue(v reflect.Value) error {
	if !v.IsValid() {
		e.null()
		return nil
	}
	return typeEncoder(v.Type())(e, v)
}

// Cached encoder of a type, a recursive type served through a placeholder that waits for the real one
func typeEncoder(t reflect.Type) encodeFunc {
	if f, ok := encoderCache.Load(t); ok {
		return f.(encodeFunc)
	}
	var (
		wg sync.WaitGroup
		f  encodeFunc
	)
	wg.Add(1)
	fi, loaded := encoderCache.LoadOrStore(t, encodeFunc(func(e *encoder, v reflect.Value) error {
		wg.Wait()
		return f(e, v)
	}))
	if loaded {
		return fi.(encodeFunc)
	}
	f = newTypeEncoder(t, true)
	wg.Done()
	encoderCache.Store(t, f)
	return f
}

func newTypeEncoder(t reflect.Type, allowAddr bool) encodeFunc {
	switch {
	case t == objectPtrType:
		return objectPtrEncoder
	case t == objectType:
		return objectValueEncoder
	case t == anyMapType:
		return anyMapEncoder
	case t == anySliceType:
		return anySliceEncoder
	case t == stringSliceType:
		return stringSliceEncoder
	case t.Kind() == reflect.Interface:
		return interfaceEncoder
	case t.Kind() != reflect.Pointer && allowAddr && reflect.PointerTo(t).Implements(marshalerType):
		return condAddrEncoder(addrMarshalerEncoder, newTypeEncoder(t, false))
	case t.Implements(marshalerType):
		return marshalerEncoder
	case t.Kind() != reflect.Pointer && allowAddr && reflect.PointerTo(t).Implements(textMarshalerType):
		return condAddrEncoder(addrTextMarshalerEncoder, newTypeEncoder(t, false))
	case t.Implements(textMarshalerType):
		return textMarshalerEncoder
	case t.Implements(errorType):
		return errorEncoder
	}
	switch t.Kind() {
	case reflect.Bool:
		return boolEncoder
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return intEncoder
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return uintEncoder
	case reflect.Float32:
		return float32Encoder
	case reflect.Float64:
		return float64Encoder
	case reflect.String:
		if t == numberType {
			return numberEncoder
		}
		return stringEncoder
	case reflect.Struct:
		return newStructEncoder(t)
	case reflect.Map:
		return newMapEncoder(t)
	case reflect.Slice:
		return newSliceEncoder(t)
	case reflect.Array:
		return newArrayEncoder(t)
	case reflect.Pointer:
		return newPtrEncoder(t)
	}
	return convertEncoder
}

func condAddrEncoder(canAddr, otherwise encodeFunc) encodeFunc {
	return func(e *encoder, v reflect.Value) error {
		if v.CanAddr() {
			return canAddr(e, v)
		}
		return otherwise(e, v)
	}
}

func objectPtrEncoder(e *encoder, v reflect.Value) error {
	if v.IsNil() {
		e.null()
		return nil
	}
	o, _ := reflect.TypeAssert[*Object](v)
	return e.object(o)
}

func objectValueEncoder(e *encoder, v reflect.Value) error {
	if v.CanAddr() {
		o, _ := reflect.TypeAssert[*Object](v.Addr())
		return e.object(o)
	}
	o, _ := reflect.TypeAssert[Object](v)
	return e.object(&o)
}

// The shapes the dynamic switch of encoder.value serves, reached here as struct fields, elements and map values
func anyMapEncoder(e *encoder, v reflect.Value) error {
	m, _ := reflect.TypeAssert[map[string]any](v)
	return e.stringMap(m)
}

func anySliceEncoder(e *encoder, v reflect.Value) error {
	a, _ := reflect.TypeAssert[[]any](v)
	return e.array(a)
}

func stringSliceEncoder(e *encoder, v reflect.Value) error {
	a, _ := reflect.TypeAssert[[]string](v)
	e.stringSlice(a)
	return nil
}

func interfaceEncoder(e *encoder, v reflect.Value) error {
	if v.IsNil() {
		e.null()
		return nil
	}
	return e.value(v.Interface())
}

func marshalerEncoder(e *encoder, v reflect.Value) error {
	if v.Kind() == reflect.Pointer && v.IsNil() {
		e.null()
		return nil
	}
	m, ok := reflect.TypeAssert[json.Marshaler](v)
	if !ok {
		e.null()
		return nil
	}
	return e.marshal(v.Type(), m)
}

func addrMarshalerEncoder(e *encoder, v reflect.Value) error {
	m, _ := reflect.TypeAssert[json.Marshaler](v.Addr())
	return e.marshal(v.Type(), m)
}

func (e *encoder) marshal(t reflect.Type, m json.Marshaler) error {
	b, err := m.MarshalJSON()
	if err != nil {
		return &json.MarshalerError{Type: t, Err: err}
	}
	return e.raw(b, t)
}

func textMarshalerEncoder(e *encoder, v reflect.Value) error {
	if v.Kind() == reflect.Pointer && v.IsNil() {
		e.null()
		return nil
	}
	m, ok := reflect.TypeAssert[encoding.TextMarshaler](v)
	if !ok {
		e.null()
		return nil
	}
	return e.text(v.Type(), m)
}

func addrTextMarshalerEncoder(e *encoder, v reflect.Value) error {
	m, _ := reflect.TypeAssert[encoding.TextMarshaler](v.Addr())
	return e.text(v.Type(), m)
}

func (e *encoder) text(t reflect.Type, m encoding.TextMarshaler) error {
	b, err := m.MarshalText()
	if err != nil {
		return fmt.Errorf("json: error calling MarshalText for type %s: %w", t, err)
	}
	e.buf = appendString(e.buf, string(b), e.ascii)
	return nil
}

func errorEncoder(e *encoder, v reflect.Value) error {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		if v.IsNil() {
			e.null()
			return nil
		}
	}
	err, ok := reflect.TypeAssert[error](v)
	if !ok {
		e.null()
		return nil
	}
	e.buf = appendString(e.buf, err.Error(), e.ascii)
	return nil
}

func boolEncoder(e *encoder, v reflect.Value) error {
	e.buf = strconv.AppendBool(e.buf, v.Bool())
	return nil
}

func intEncoder(e *encoder, v reflect.Value) error {
	e.buf = strconv.AppendInt(e.buf, v.Int(), 10)
	return nil
}

func uintEncoder(e *encoder, v reflect.Value) error {
	e.buf = strconv.AppendUint(e.buf, v.Uint(), 10)
	return nil
}

func float32Encoder(e *encoder, v reflect.Value) error {
	e.buf = appendFloat(e.buf, v.Float(), 32)
	return nil
}

func float64Encoder(e *encoder, v reflect.Value) error {
	e.buf = appendFloat(e.buf, v.Float(), 64)
	return nil
}

func numberEncoder(e *encoder, v reflect.Value) error { return e.number(v.String()) }

func stringEncoder(e *encoder, v reflect.Value) error {
	e.buf = appendString(e.buf, v.String(), e.ascii)
	return nil
}

func convertEncoder(e *encoder, v reflect.Value) error { return e.converted(v) }

func newPtrEncoder(t reflect.Type) encodeFunc {
	elem := typeEncoder(t.Elem())
	return func(e *encoder, v reflect.Value) error {
		if v.IsNil() {
			e.null()
			return nil
		}
		var id visit
		if e.ptrs++; e.ptrs+e.depth > cycleDepth {
			id = visit{ptr: uintptr(v.UnsafePointer()), n: -1, typ: t}
			if _, seen := e.seen[id]; seen {
				return &json.UnsupportedValueError{Value: v, Str: "encountered a cycle via " + t.String()}
			}
			if e.seen == nil {
				e.seen = map[visit]struct{}{}
			}
			e.seen[id] = struct{}{}
		}
		err := elem(e, v.Elem())
		if id.ptr != 0 {
			delete(e.seen, id)
		}
		e.ptrs--
		return err
	}
}

func newSliceEncoder(t reflect.Type) encodeFunc {
	if t.Elem().Kind() == reflect.Uint8 {
		p := reflect.PointerTo(t.Elem())
		if !p.Implements(marshalerType) && !p.Implements(textMarshalerType) {
			return bytesEncoder
		}
	}
	elem := typeEncoder(t.Elem())
	return func(e *encoder, v reflect.Value) error {
		n := v.Len()
		if n == 0 {
			e.buf = append(e.buf, "[]"...)
			return nil
		}
		id, err := e.openValue('[', v)
		if err != nil {
			return err
		}
		for i := range n {
			e.item(i)
			if err := elem(e, v.Index(i)); err != nil {
				return err
			}
		}
		e.close(']', id)
		return nil
	}
}

func bytesEncoder(e *encoder, v reflect.Value) error {
	e.buf = append(e.buf, '"')
	e.buf = base64.StdEncoding.AppendEncode(e.buf, v.Bytes())
	e.buf = append(e.buf, '"')
	return nil
}

func newArrayEncoder(t reflect.Type) encodeFunc {
	elem := typeEncoder(t.Elem())
	return func(e *encoder, v reflect.Value) error {
		n := v.Len()
		if n == 0 {
			e.buf = append(e.buf, "[]"...)
			return nil
		}
		e.depth++
		e.buf = append(e.buf, '[')
		for i := range n {
			e.item(i)
			if err := elem(e, v.Index(i)); err != nil {
				return err
			}
		}
		e.close(']', visit{})
		return nil
	}
}

// Text of a map key, nil when the key type has no text form
func mapKeyFunc(t reflect.Type) func(reflect.Value) (string, error) {
	switch {
	case t.Kind() == reflect.String:
		return func(k reflect.Value) (string, error) { return k.String(), nil }
	case t.Kind() == reflect.Interface:
		return interfaceKey
	case t.Implements(textMarshalerType):
		return textKey
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return func(k reflect.Value) (string, error) { return strconv.FormatInt(k.Int(), 10), nil }
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return func(k reflect.Value) (string, error) { return strconv.FormatUint(k.Uint(), 10), nil }
	case reflect.Float32:
		return func(k reflect.Value) (string, error) { return string(appendFloat(nil, k.Float(), 32)), nil }
	case reflect.Float64:
		return func(k reflect.Value) (string, error) { return string(appendFloat(nil, k.Float(), 64)), nil }
	case reflect.Bool:
		return func(k reflect.Value) (string, error) { return strconv.FormatBool(k.Bool()), nil }
	}
	return nil
}

func textKey(k reflect.Value) (string, error) {
	if k.Kind() == reflect.Pointer && k.IsNil() {
		return "", nil
	}
	m, ok := reflect.TypeAssert[encoding.TextMarshaler](k)
	if !ok {
		return "", nil
	}
	b, err := m.MarshalText()
	if err != nil {
		return "", fmt.Errorf("json: error calling MarshalText for type %s: %w", k.Type(), err)
	}
	return string(b), nil
}

// Text of a map key held in an interface, by the rules for its dynamic type
func interfaceKey(k reflect.Value) (string, error) {
	if k.IsNil() {
		return "null", nil
	}
	k = k.Elem()
	keyOf := mapKeyFunc(k.Type())
	if keyOf == nil {
		return "", &json.UnsupportedTypeError{Type: k.Type()}
	}
	return keyOf(k)
}

func newMapEncoder(t reflect.Type) encodeFunc {
	keyOf := mapKeyFunc(t.Key())
	if keyOf == nil {
		return convertEncoder
	}
	elem := typeEncoder(t.Elem())
	type entry struct {
		key string
		val reflect.Value
	}
	return func(e *encoder, v reflect.Value) error {
		if v.Len() == 0 {
			e.buf = append(e.buf, "{}"...)
			return nil
		}
		entries := make([]entry, 0, v.Len())
		for it := v.MapRange(); it.Next(); {
			k, err := keyOf(it.Key())
			if err != nil {
				if _, unusable := err.(*json.UnsupportedTypeError); unusable && e.convert != nil {
					return e.converted(v)
				}
				return err
			}
			entries = append(entries, entry{k, it.Value()})
		}
		slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })
		id, err := e.openValue('{', v)
		if err != nil {
			return err
		}
		for i, en := range entries {
			e.item(i)
			e.key(en.key)
			if err := elem(e, en.val); err != nil {
				return err
			}
		}
		e.close('}', id)
		return nil
	}
}

// One encodable struct field with its tag settings and the index path that reaches it
type field struct {
	name      string
	quoted    string
	quotedASC string
	tag       bool
	index     []int
	typ       reflect.Type
	omitEmpty bool
	omitZero  bool
	isZero    func(reflect.Value) bool
	asString  bool
	// Under asString, a pointer to the field's type has MarshalJSON or MarshalText, which wins when the value is addressable
	addrMarshal bool
	encoder     encodeFunc
}

func newStructEncoder(t reflect.Type) encodeFunc {
	fields := cachedFields(t)
	return func(e *encoder, v reflect.Value) error {
		written := 0
	next:
		for i := range fields {
			f := &fields[i]
			fv := v
			for _, idx := range f.index {
				if fv.Kind() == reflect.Pointer {
					if fv.IsNil() {
						continue next
					}
					fv = fv.Elem()
				}
				fv = fv.Field(idx)
			}
			if f.omitEmpty && isEmptyValue(fv) {
				continue
			}
			if f.omitZero && (f.isZero == nil && fv.IsZero() || f.isZero != nil && f.isZero(fv)) {
				continue
			}
			if written == 0 {
				e.depth++
				e.buf = append(e.buf, '{')
			}
			e.item(written)
			written++
			if e.ascii {
				e.buf = append(e.buf, f.quotedASC...)
			} else {
				e.buf = append(e.buf, f.quoted...)
			}
			e.colon()
			var err error
			if f.asString {
				err = e.quotedScalar(fv, f.addrMarshal)
			} else {
				err = f.encoder(e, fv)
			}
			if err != nil {
				return err
			}
		}
		if written == 0 {
			e.buf = append(e.buf, "{}"...)
			return nil
		}
		e.close('}', visit{})
		return nil
	}
}

// Field value under the ",string" tag option: a scalar inside a JSON string, an addressable value whose pointer marshals itself written by its marshaler
func (e *encoder) quotedScalar(v reflect.Value, addrMarshal bool) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			e.null()
			return nil
		}
		v = v.Elem()
	}
	if addrMarshal && v.CanAddr() {
		return typeEncoder(v.Type())(e, v)
	}
	switch v.Kind() {
	case reflect.String:
		if v.Type() == numberType {
			s := v.String()
			if s == "" {
				s = "0"
			}
			if !validNumber(s) {
				return fmt.Errorf("json: invalid number literal %q", s)
			}
			e.buf = append(append(append(e.buf, '"'), s...), '"')
			return nil
		}
		e.buf = appendString(e.buf, string(appendString(nil, v.String(), e.ascii)), e.ascii)
		return nil
	case reflect.Float32, reflect.Float64:
		size := 64
		if v.Kind() == reflect.Float32 {
			size = 32
		}
		text := appendFloat(nil, v.Float(), size)
		if string(text) == "null" {
			e.null()
			return nil
		}
		e.buf = append(append(append(e.buf, '"'), text...), '"')
		return nil
	}
	e.buf = append(e.buf, '"')
	err := typeEncoder(v.Type())(e, v)
	e.buf = append(e.buf, '"')
	return err
}

func isEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}

func cachedFields(t reflect.Type) []field {
	if f, ok := fieldCache.Load(t); ok {
		return f.([]field)
	}
	f, _ := fieldCache.LoadOrStore(t, typeFields(t))
	return f.([]field)
}

func validTag(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c) && !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return false
		}
	}
	return true
}

func tagHas(opts, name string) bool {
	for opt := range strings.SplitSeq(opts, ",") {
		if opt == name {
			return true
		}
	}
	return false
}

func typeByIndex(t reflect.Type, index []int) reflect.Type {
	for _, i := range index {
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		t = t.Field(i).Type
	}
	return t
}

// IsZero method of a field type for the omitzero option, nil when the type has none
func zeroFunc(t reflect.Type) func(reflect.Value) bool {
	zero := func(v reflect.Value) bool {
		z, _ := reflect.TypeAssert[interface{ IsZero() bool }](v)
		return z == nil || z.IsZero()
	}
	switch {
	case t.Kind() == reflect.Interface && t.Implements(isZeroerType):
		return func(v reflect.Value) bool {
			return v.IsNil() || (v.Elem().Kind() == reflect.Pointer && v.Elem().IsNil()) || zero(v)
		}
	case t.Kind() == reflect.Pointer && t.Implements(isZeroerType):
		return func(v reflect.Value) bool { return v.IsNil() || zero(v) }
	case t.Implements(isZeroerType):
		return zero
	case reflect.PointerTo(t).Implements(isZeroerType):
		return func(v reflect.Value) bool {
			if !v.CanAddr() {
				boxed := reflect.New(v.Type()).Elem()
				boxed.Set(v)
				v = boxed
			}
			return zero(v.Addr())
		}
	}
	return nil
}

// Visible fields of a struct type under the Go embedding rules, json tag names winning over plain ones
func typeFields(t reflect.Type) []field {
	current := []field{}
	next := []field{{typ: t}}
	var count, nextCount map[reflect.Type]int
	visited := map[reflect.Type]bool{}
	var fields []field
	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, map[reflect.Type]int{}
		for _, f := range current {
			if visited[f.typ] {
				continue
			}
			visited[f.typ] = true
			for i := range f.typ.NumField() {
				sf := f.typ.Field(i)
				if sf.Anonymous {
					st := sf.Type
					if st.Kind() == reflect.Pointer {
						st = st.Elem()
					}
					if !sf.IsExported() && st.Kind() != reflect.Struct {
						continue
					}
				} else if !sf.IsExported() {
					continue
				}
				tag := sf.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, opts, _ := strings.Cut(tag, ",")
				if !validTag(name) {
					name = ""
				}
				index := make([]int, len(f.index)+1)
				copy(index, f.index)
				index[len(f.index)] = i
				ft := sf.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				asString := false
				if tagHas(opts, "string") && !sf.Type.Implements(errorType) && !ft.Implements(marshalerType) && !ft.Implements(textMarshalerType) {
					switch ft.Kind() {
					case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
						reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
						reflect.Float32, reflect.Float64, reflect.String:
						asString = true
					}
				}
				addrMarshal := asString && (reflect.PointerTo(ft).Implements(marshalerType) || reflect.PointerTo(ft).Implements(textMarshalerType))
				if name != "" || !sf.Anonymous || ft.Kind() != reflect.Struct {
					tagged := name != ""
					if name == "" {
						name = sf.Name
					}
					fld := field{name: name, tag: tagged, index: index, typ: ft, asString: asString, addrMarshal: addrMarshal,
						omitEmpty: tagHas(opts, "omitempty"), omitZero: tagHas(opts, "omitzero")}
					if fld.omitZero {
						fld.isZero = zeroFunc(sf.Type)
					}
					fields = append(fields, fld)
					if count[f.typ] > 1 {
						fields = append(fields, fields[len(fields)-1])
					}
					continue
				}
				nextCount[ft]++
				if nextCount[ft] == 1 {
					next = append(next, field{name: ft.Name(), index: index, typ: ft})
				}
			}
		}
	}
	slices.SortFunc(fields, func(a, b field) int {
		if c := strings.Compare(a.name, b.name); c != 0 {
			return c
		}
		if c := cmp.Compare(len(a.index), len(b.index)); c != 0 {
			return c
		}
		if a.tag != b.tag {
			if a.tag {
				return -1
			}
			return 1
		}
		return slices.Compare(a.index, b.index)
	})
	out := fields[:0]
	for advance, i := 0, 0; i < len(fields); i += advance {
		name := fields[i].name
		for advance = 1; i+advance < len(fields) && fields[i+advance].name == name; advance++ {
		}
		group := fields[i : i+advance]
		if len(group) > 1 && len(group[0].index) == len(group[1].index) && group[0].tag == group[1].tag {
			continue
		}
		out = append(out, group[0])
	}
	fields = out
	slices.SortFunc(fields, func(a, b field) int { return slices.Compare(a.index, b.index) })
	for i := range fields {
		f := &fields[i]
		f.quoted = string(appendString(nil, f.name, false))
		f.quotedASC = string(appendString(nil, f.name, true))
		f.encoder = typeEncoder(typeByIndex(t, f.index))
	}
	return fields
}
