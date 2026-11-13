package config

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Value at a dotted path that could not be read into its field, the rest of the file decoded regardless
type FieldError struct {
	Path string
	Err  error
}

// Path and cause in one line
func (e *FieldError) Error() string {
	if e.Path == "" {
		return e.Err.Error()
	}
	return e.Path + ": " + e.Err.Error()
}

// Underlying cause
func (e *FieldError) Unwrap() error {
	return e.Err
}

// Raised for a value whose shape the field cannot take, such as a list where a number belongs
var ErrShape = errors.New("value has the wrong shape")

// Raised for a scalar the field type cannot read, such as a word where a number belongs
var ErrScalar = errors.New("value cannot be read as the field type")

// Raised for a key written twice in one mapping, the later value winning as in a plain mapping read
var ErrDuplicateKey = errors.New("key written more than once")

// Failure of the whole value rather than of something inside it
func rootError(err error) error {
	return &FieldError{Err: err}
}

// Failure for a node whose kind differs from the kind the field takes
func shapeError(n *yaml.Node, want yaml.Kind) error {
	return rootError(fmt.Errorf("%w: %s where %s belongs", ErrShape, kindName(n), kindWord(want)))
}

// Words for the kind of a node as a user wrote it
func kindName(n *yaml.Node) string {
	n = resolveAlias(n)
	if n == nil {
		return "nothing"
	}
	if n.Kind == yaml.ScalarNode {
		return strconv.Quote(n.Value)
	}
	return kindWord(n.Kind)
}

// Article and noun for a node kind
func kindWord(k yaml.Kind) string {
	switch k {
	case yaml.MappingNode:
		return "a mapping"
	case yaml.SequenceNode:
		return "a list"
	}
	return "a single value"
}

// Error with every failure placed under key, list indexes joined without a dot
func underKey(key string, err error) error {
	if err == nil {
		return nil
	}
	var out []error
	for _, e := range flatErrors(err) {
		var fe *FieldError
		if !errors.As(e, &fe) {
			out = append(out, &FieldError{Path: key, Err: e})
			continue
		}
		path := key
		switch {
		case fe.Path == "":
		case strings.HasPrefix(fe.Path, "["):
			path += fe.Path
		default:
			path += "." + fe.Path
		}
		out = append(out, &FieldError{Path: path, Err: fe.Err})
	}
	return errors.Join(out...)
}

// Leaf errors of a joined error tree
func flatErrors(err error) []error {
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range multi.Unwrap() {
			out = append(out, flatErrors(e)...)
		}
		return out
	}
	return []error{err}
}

// Reports an error that fails the value as a whole, leaving nothing of it decoded
func isRootError(err error) bool {
	if err == nil {
		return false
	}
	for _, e := range flatErrors(err) {
		var fe *FieldError
		if !errors.As(e, &fe) || fe.Path == "" {
			return true
		}
	}
	return false
}

// Value types that keep an explicit null as their own value instead of leaving it to the container
type nullHolder interface {
	holdsNull()
}

// How a field reads values of another type, matching what its readers apply
type coercion string

// Coercions a field may name in its coerce tag, the default being truthy words for booleans and plain number parsing
const (
	coerceDefault    coercion = ""
	coerceBoolOf     coercion = "bool"
	coerceBoolKeep   coercion = "boolkeep"
	coerceStrSet     coercion = "strset"
	coerceApproval   coercion = "approval"
	coerceBoolWords  coercion = "boolwords"
	coerceNotFalse   coercion = "notfalse"
	coerceStrict     coercion = "strict"
	coerceWhole      coercion = "whole"
	coerceNoFloat    coercion = "nofloat"
	coerceExact      coercion = "exact"
	coerceExactInt   coercion = "exactint"
	coerceTruthWords coercion = "truthwords"
	coerceNotOff     coercion = "notoff"
)

// Value types whose entries or items read under the coercion named on the field holding them
type coercedDecoder interface {
	decodeCoerced(n *yaml.Node, c coercion) error
}

// One mapping key of a struct with the field it decodes into
type fieldRef struct {
	index     []int
	omitempty bool
	coerce    coercion
}

// Mapping keys of a struct type in declaration order with inline fields expanded in place
func structFields(t reflect.Type) ([]string, map[string]fieldRef) {
	var order []string
	refs := map[string]fieldRef{}
	var walk func(t reflect.Type, prefix []int)
	walk = func(t reflect.Type, prefix []int) {
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("yaml")
			name, opts, _ := strings.Cut(tag, ",")
			inline := slices.Contains(strings.Split(opts, ","), "inline")
			if tag == "-" || (!f.IsExported() && !inline) {
				continue
			}
			index := append(slices.Clone(prefix), i)
			if inline {
				walk(f.Type, index)
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			order = append(order, name)
			refs[name] = fieldRef{
				index:     index,
				omitempty: slices.Contains(strings.Split(opts, ","), "omitempty"),
				coerce:    coercion(f.Tag.Get("coerce")),
			}
		}
	}
	walk(t, nil)
	return order, refs
}

// Decodes the named keys of a mapping into the struct v points at and keeps every other key in r
func decodeMapping(n *yaml.Node, v any, r *rest) error {
	return decodeFields(n, v, r, false)
}

// Decodes a user-keyed entry, writing it back in file key order when the entry held nothing before
func decodeEntry(n *yaml.Node, v any, r *rest) error {
	return decodeFields(n, v, r, reflect.ValueOf(v).Elem().IsZero())
}

// Field by field decode of a mapping, each failure reported at its path and kept for writing back
func decodeFields(n *yaml.Node, v any, r *rest, fresh bool) error {
	n, err := resolved(n)
	if err != nil {
		return rootError(err)
	}
	if n == nil || isNull(n) {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return shapeError(n, yaml.MappingNode)
	}
	sv := reflect.ValueOf(v).Elem()
	order, refs := structFields(sv.Type())
	var errs []error
	var fileOrder, added []string
	for _, p := range mappingPairs(n) {
		k := p.key.Value
		fileOrder = append(fileOrder, k)
		ref, known := refs[k]
		if !known {
			added = append(added, k)
			r.set(p.key, p.value)
			continue
		}
		fv := sv.FieldByIndex(ref.index)
		gains := ref.omitempty && isZero(fv)
		hold, err := decodeValue(p.value, fv, ref.coerce)
		if gains && (hold || !isZero(fv)) {
			added = append(added, k)
		}
		if hold || readAsText(p.value, fv) {
			r.hold(k, p.value, snapshot(fv))
		} else {
			r.release(k)
		}
		errs = append(errs, underKey(k, err))
	}
	r.adoptOrder(fileOrder, added, order, fresh)
	return errors.Join(errs...)
}

// Reports a text field that read a scalar of another type, whose file form writing back keeps
func readAsText(n *yaml.Node, fv reflect.Value) bool {
	for fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return false
		}
		fv = fv.Elem()
	}
	if fv.Kind() != reflect.String {
		return false
	}
	n = resolveAlias(n)
	return n != nil && n.Kind == yaml.ScalarNode && !isNull(n) && n.ShortTag() != tagStr
}

// Encoded form of a value, compared at write time to tell whether a held value was replaced since decode
func snapshot(v reflect.Value) []byte {
	n, err := encodeValue(v)
	if err != nil {
		return nil
	}
	b, err := yaml.Marshal(n)
	if err != nil {
		return nil
	}
	return b
}

// Decodes a resolved node into the addressable value rv, hold true when rv cannot represent what the file holds
func decodeValue(n *yaml.Node, rv reflect.Value, c coercion) (hold bool, err error) {
	if rv.Kind() == reflect.Pointer {
		if isNull(n) && rv.Type().Elem().Kind() == reflect.Bool && nullIsFalse(c) {
			rv.Set(reflect.New(rv.Type().Elem()))
			return true, nil
		}
		if isNull(n) {
			rv.SetZero()
			return false, nil
		}
		fresh := rv.IsNil()
		if fresh {
			rv.Set(reflect.New(rv.Type().Elem()))
		}
		hold, err := decodeValue(n, rv.Elem(), c)
		if hold && fresh {
			rv.SetZero()
		}
		return hold, err
	}
	if u, ok := rv.Addr().Interface().(yaml.Unmarshaler); ok {
		if isNull(n) {
			if _, keeps := u.(nullHolder); !keeps {
				return !isMappingValue(rv), nil
			}
		}
		var err error
		if cd, ok := u.(coercedDecoder); ok {
			err = cd.decodeCoerced(n, c)
		} else {
			err = u.UnmarshalYAML(n)
		}
		return isRootError(err), err
	}
	if isNull(n) {
		return true, nullInto(rv, c)
	}
	switch rv.Kind() {
	case reflect.Interface:
		var v any
		err := n.Decode(&v)
		if err == nil {
			rv.Set(reflect.ValueOf(&v).Elem())
		}
		return err != nil, rootIf(err)
	case reflect.Slice:
		return decodeSlice(n, rv, c)
	case reflect.Map:
		return decodeMap(n, rv, c)
	case reflect.Struct:
		var unnamed rest
		err := decodeFields(n, rv.Addr().Interface(), &unnamed, false)
		return isRootError(err) || len(unnamed.pairs) > 0 || len(unnamed.held) > 0, err
	}
	if n.Kind != yaml.ScalarNode {
		return true, shapeError(n, yaml.ScalarNode)
	}
	if err := scalarInto(n, rv, c); err != nil {
		return true, rootError(err)
	}
	return false, nil
}

// Error marked as a failure of the whole value, nil kept nil
func rootIf(err error) error {
	if err == nil {
		return nil
	}
	return rootError(err)
}

// Reports a value that encodes as a mapping, which an explicit null leaves in place as a deep merge does
func isMappingValue(rv reflect.Value) bool {
	n, err := encodeValue(rv)
	return err == nil && n.Kind == yaml.MappingNode
}

// Value an explicit null gives a plain field, the default kept unless the readers turn null into false
func nullInto(rv reflect.Value, c coercion) error {
	if rv.Kind() == reflect.Bool && nullIsFalse(c) {
		rv.SetBool(false)
	}
	return nil
}

// Reports a boolean coercion whose readers read an explicit null as false
func nullIsFalse(c coercion) bool {
	switch c {
	case coerceBoolOf, coerceStrSet, coerceApproval, coerceBoolWords, coerceTruthWords:
		return true
	}
	return false
}

// Decodes a list element by element under the field coercion, failed elements reported at their index and left out
func decodeSlice(n *yaml.Node, rv reflect.Value, c coercion) (bool, error) {
	if n.Kind != yaml.SequenceNode {
		return true, shapeError(n, yaml.SequenceNode)
	}
	out := reflect.MakeSlice(rv.Type(), 0, len(n.Content))
	var errs []error
	failed := false
	for i, item := range n.Content {
		ev := reflect.New(rv.Type().Elem()).Elem()
		hold, err := decodeValue(item, ev, c)
		if err != nil {
			errs = append(errs, underKey("["+strconv.Itoa(i)+"]", err))
		}
		if hold {
			failed = true
			continue
		}
		out = reflect.Append(out, ev)
	}
	rv.Set(out)
	return failed, errors.Join(errs...)
}

// Decodes a mapping entry by entry into a Go map, a failing entry reported at its key and left out
func decodeMap(n *yaml.Node, rv reflect.Value, c coercion) (bool, error) {
	if n.Kind != yaml.MappingNode {
		return true, shapeError(n, yaml.MappingNode)
	}
	if rv.IsNil() {
		rv.Set(reflect.MakeMap(rv.Type()))
	}
	var errs []error
	failed := false
	for _, p := range mappingPairs(n) {
		kv := reflect.New(rv.Type().Key()).Elem()
		if kv.Kind() == reflect.String {
			kv.SetString(p.key.Value)
		} else if hold, err := decodeValue(p.key, kv, coerceDefault); hold || err != nil {
			errs = append(errs, underKey(p.key.Value, err))
			failed = true
			continue
		}
		ev := reflect.New(rv.Type().Elem()).Elem()
		if existing := rv.MapIndex(kv); existing.IsValid() {
			ev.Set(existing)
		}
		hold, err := decodeValue(p.value, ev, c)
		errs = append(errs, underKey(p.key.Value, err))
		if hold {
			failed = true
			continue
		}
		rv.SetMapIndex(kv, ev)
	}
	return failed, errors.Join(errs...)
}

// Sets a bool, integer, float or string field from a resolved scalar the way the key's readers coerce it
func scalarInto(n *yaml.Node, rv reflect.Value, c coercion) error {
	tag := n.ShortTag()
	if (c == coerceExact || c == coerceExactInt) && !exactTag(rv.Kind(), tag, c) {
		return fmt.Errorf("%w: %s is not of the field type", ErrScalar, kindName(n))
	}
	switch rv.Kind() {
	case reflect.Bool:
		b, ok := boolOf(n, tag, c)
		if !ok {
			return fmt.Errorf("%w: %s is not a boolean", ErrScalar, kindName(n))
		}
		rv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v, ok := intOf(n, tag, c)
		if !ok || rv.OverflowInt(v) {
			return fmt.Errorf("%w: %s is not a whole number in range", ErrScalar, kindName(n))
		}
		rv.SetInt(v)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v, ok := intOf(n, tag, c)
		if !ok || v < 0 || rv.OverflowUint(uint64(v)) {
			return fmt.Errorf("%w: %s is not a whole number in range", ErrScalar, kindName(n))
		}
		rv.SetUint(uint64(v))
	case reflect.Float32, reflect.Float64:
		f, ok := floatOf(n, tag, c)
		if !ok {
			return fmt.Errorf("%w: %s is not a number", ErrScalar, kindName(n))
		}
		rv.SetFloat(f)
	case reflect.String:
		rv.SetString(n.Value)
	default:
		return fmt.Errorf("%w: %s", ErrScalar, rv.Type())
	}
	return nil
}

// Reports a scalar type the readers of an exact field take, numbers of either type unless only integers count
func exactTag(k reflect.Kind, tag string, c coercion) bool {
	switch k {
	case reflect.Bool:
		return tag == tagBool
	case reflect.String:
		return tag == tagStr
	}
	return tag == tagInt || (tag == tagFloat && c != coerceExactInt)
}

// Boolean a scalar reads as, booleans and numbers by value and words by the coercion of the key
func boolOf(n *yaml.Node, tag string, c coercion) (bool, bool) {
	switch {
	case tag == tagBool:
		return n.Value == "true", true
	case c == coerceTruthWords:
		switch strings.ToLower(strings.TrimSpace(n.Value)) {
		case "1", "true", "yes", "on":
			return tag == tagStr, true
		}
		return false, true
	case (c == coerceNotFalse || c == coerceApproval) && (tag == tagInt || tag == tagFloat):
		return false, c == coerceApproval
	case c == coerceNotFalse || ((c == coerceBoolWords || c == coerceNotOff) && tag != tagInt && tag != tagFloat):
		switch strings.ToLower(strings.TrimSpace(n.Value)) {
		case "0", "false", "no", "off":
			return false, true
		}
		return c != coerceNotOff || !strings.EqualFold(strings.TrimSpace(n.Value), "disabled"), true
	case c == coerceApproval:
		switch strings.ToLower(strings.TrimSpace(n.Value)) {
		case "on", "true", "yes", "1", "approve", "enabled":
			return true, true
		}
		return false, true
	}
	switch tag {
	case tagInt:
		v, ok := constructInt(n.Value)
		if !ok {
			return false, false
		}
		if c == coerceStrSet {
			return v.IsInt64() && v.Int64() == 1, true
		}
		return v.Sign() != 0, c != coerceStrict
	case tagFloat:
		f, ok := constructFloat(n.Value)
		return f != 0 && c != coerceStrSet, ok && c != coerceStrict
	}
	switch c {
	case coerceBoolOf, coerceBoolKeep:
		return n.Value != "", true
	case coerceStrSet:
		switch strings.ToLower(n.Value) {
		case "true", "1", "yes":
			return true, true
		}
		return false, true
	case coerceStrict:
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(n.Value)) {
	case "1", "true", "yes", "on":
		return true, true
	}
	return false, true
}

// Whole number a scalar reads as, floats truncated toward zero and numeric text parsed
func intOf(n *yaml.Node, tag string, c coercion) (int64, bool) {
	switch tag {
	case tagInt:
		v, ok := constructInt(n.Value)
		return v.Int64(), ok && v.IsInt64()
	case tagFloat:
		f, ok := constructFloat(n.Value)
		if !ok || c == coerceNoFloat || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) >= 1<<63 {
			return 0, false
		}
		if c == coerceWhole && f != math.Trunc(f) {
			return 0, false
		}
		return int64(f), true
	case tagBool:
		rejects := c == coerceStrict || c == coerceNoFloat || c == coerceWhole
		if n.Value == "true" {
			return 1, !rejects
		}
		return 0, !rejects
	}
	v, err := strconv.ParseInt(strings.TrimSpace(n.Value), 10, 64)
	return v, err == nil
}

// Number a scalar reads as, booleans as one and zero and numeric text parsed
func floatOf(n *yaml.Node, tag string, c coercion) (float64, bool) {
	switch tag {
	case tagInt:
		v, ok := constructInt(n.Value)
		if !ok {
			return 0, false
		}
		f, _ := new(big.Float).SetInt(v).Float64()
		return f, true
	case tagFloat:
		return constructFloat(n.Value)
	case tagBool:
		if n.Value == "true" {
			return 1, c != coerceStrict
		}
		return 0, c != coerceStrict
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(n.Value), 64)
	return f, err == nil || errors.Is(err, strconv.ErrRange)
}
