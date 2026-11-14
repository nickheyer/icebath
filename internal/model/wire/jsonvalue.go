package wire

import (
	"encoding/json"
	"strconv"
)

// What a JSON value is
type Kind uint8

const (
	KindNone Kind = iota
	KindNull
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
)

// One value inside JSON text validated when it was parsed, the zero value standing for an absent one
type Value struct {
	raw []byte
}

// One member of an object value
type Field struct {
	Key   string
	Value Value
}

// Validates JSON text once so every value reached from it can be read without checking again
func ParseValue(data []byte) (Value, error) {
	data = trimJSON(data)
	if len(data) == 0 || !json.Valid(data) {
		return Value{}, ErrInvalidJSON
	}
	return Value{raw: data}, nil
}

// Text of the value exactly as written, nil when absent
func (v Value) Raw() json.RawMessage { return v.raw }

// Reports whether the value is present
func (v Value) Exists() bool { return len(v.raw) > 0 }

// What the value is
func (v Value) Kind() Kind {
	if len(v.raw) == 0 {
		return KindNone
	}
	switch v.raw[0] {
	case 'n':
		return KindNull
	case 't', 'f':
		return KindBool
	case '"':
		return KindString
	case '[':
		return KindArray
	case '{':
		return KindObject
	}
	return KindNumber
}

// Reports whether the value is an object
func (v Value) IsObject() bool { return v.Kind() == KindObject }

// Reports whether the value is an array
func (v Value) IsArray() bool { return v.Kind() == KindArray }

// Reports whether the value is a string
func (v Value) IsString() bool { return v.Kind() == KindString }

// Member of an object under a key, the last one when the key repeats, absent for anything else
func (v Value) Get(key string) Value {
	if v.Kind() != KindObject {
		return Value{}
	}
	raw, _ := objectMember(v.raw, key)
	return Value{raw: raw}
}

// Value reached by following keys through nested objects
func (v Value) Path(keys ...string) Value {
	for _, key := range keys {
		v = v.Get(key)
	}
	return v
}

// Members of an object in written order, nil for anything else
func (v Value) Fields() []Field {
	if v.Kind() != KindObject {
		return nil
	}
	spans, _, _ := splitObject(v.raw)
	out := make([]Field, len(spans))
	for i, sp := range spans {
		out[i] = Field{sp.key, Value{raw: v.raw[sp.start:sp.end:sp.end]}}
	}
	return out
}

// Elements of an array in order, nil for anything else
func (v Value) Elems() []Value {
	if v.Kind() != KindArray {
		return nil
	}
	spans := splitArray(v.raw)
	out := make([]Value, len(spans))
	for i, sp := range spans {
		out[i] = Value{raw: v.raw[sp.start:sp.end:sp.end]}
	}
	return out
}

// String the value holds, false for anything that is not a string
func (v Value) Str() (string, bool) { return stringValue(v.raw) }

// String the value holds, empty for anything that is not a string
func (v Value) Text() string {
	s, _ := stringValue(v.raw)
	return s
}

// String as itself and any other present value as its JSON text
func (v Value) Display() string { return valueText(v.raw) }

// Whole number the value holds, a number with a fraction cut toward zero, false for anything else
func (v Value) Int() (int64, bool) {
	if v.Kind() != KindNumber {
		return 0, false
	}
	if n, err := strconv.ParseInt(string(v.raw), 10, 64); err == nil {
		return n, true
	}
	f, err := strconv.ParseFloat(string(v.raw), 64)
	if err != nil || f > 9.2e18 || f < -9.2e18 {
		return 0, false
	}
	return int64(f), true
}

// Number the value holds, false for anything else
func (v Value) Float() (float64, bool) {
	if v.Kind() != KindNumber {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(v.raw), 64)
	return f, err == nil
}

// Boolean the value holds, false for anything else
func (v Value) Bool() (value, ok bool) {
	if v.Kind() != KindBool {
		return false, false
	}
	return v.raw[0] == 't', true
}

// Reports whether the value is the literal true
func (v Value) True() bool { return len(v.raw) > 0 && v.raw[0] == 't' }

// Reports whether the value is the literal false
func (v Value) False() bool { return len(v.raw) > 0 && v.raw[0] == 'f' }

// Reports whether the value counts as present, null, false, zero and empty values not counting
func (v Value) Truthy() bool { return truthy(v.raw) }

// JSON string holding s, written as UTF-8 with only the escapes JSON requires
func Quote(s string) json.RawMessage { return appendString(nil, s, 0) }

// JSON array holding the items exactly as given
func Array(items ...json.RawMessage) json.RawMessage {
	return appendRawArray(nil, items, 0)
}

// JSON array of strings
func Strings(items []string) json.RawMessage {
	return appendArray(nil, len(items), 0, func(dst []byte, i int) []byte { return appendString(dst, items[i], 0) })
}

// JSON number for a whole number
func Int(n int64) json.RawMessage { return strconv.AppendInt(nil, n, 10) }

// JSON number for a float in its shortest form that reads back exactly
func Float(f float64) json.RawMessage { return appendFloat(nil, f) }

// JSON literal for a boolean
func Bool(b bool) json.RawMessage {
	if b {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}

// JSON object under construction, members kept in insertion order and written exactly as given
type Dict struct {
	members []Member
}

// Builder holding the members of an object value in written order, a repeated key keeping its last value
func DictOf(v Value) *Dict {
	d := &Dict{}
	for _, f := range v.Fields() {
		d.Set(f.Key, f.Value.raw)
	}
	return d
}

// Position of a key among the members, negative when absent
func (d *Dict) index(key string) int {
	for i := range d.members {
		if d.members[i].Key == key {
			return i
		}
	}
	return -1
}

// Stores raw JSON under a key, in place when the key exists and at the end otherwise
func (d *Dict) Set(key string, value json.RawMessage) *Dict {
	if i := d.index(key); i >= 0 {
		d.members[i].Value = value
		return d
	}
	d.members = append(d.members, Member{key, value})
	return d
}

// Stores a string under a key
func (d *Dict) SetString(key, s string) *Dict { return d.Set(key, Quote(s)) }

// Stores a whole number under a key
func (d *Dict) SetInt(key string, n int) *Dict { return d.Set(key, Int(int64(n))) }

// Stores a boolean under a key
func (d *Dict) SetBool(key string, b bool) *Dict { return d.Set(key, Bool(b)) }

// Stores a float under a key
func (d *Dict) SetFloat(key string, f float64) *Dict { return d.Set(key, Float(f)) }

// Removes a key, reporting whether it was present
func (d *Dict) Delete(key string) bool {
	i := d.index(key)
	if i < 0 {
		return false
	}
	d.members = append(d.members[:i], d.members[i+1:]...)
	return true
}

// Value stored under a key, absent when the key is missing
func (d *Dict) Get(key string) Value {
	if i := d.index(key); i >= 0 {
		return Value{raw: trimJSON(d.members[i].Value)}
	}
	return Value{}
}

// Reports whether a key is present
func (d *Dict) Has(key string) bool { return d.index(key) >= 0 }

// Number of members
func (d *Dict) Len() int { return len(d.members) }

// Entries in order, shared with the builder
func (d *Dict) Members() []Member { return d.members }

// Compact object text holding the members in order
func (d *Dict) JSON() json.RawMessage { return Object(d.members) }

// The object as a value that can be read back
func (d *Dict) Value() Value { return Value{raw: d.JSON()} }
