package wire

import (
	"bytes"
	"encoding/json"
)

// One key of a decoded object, its raw JSON kept only when the typed fields cannot rebuild it
type member struct {
	key string
	raw json.RawMessage
}

// Decoded form of an object, kept so members the caller left alone are written back as read
type shape struct {
	members []member
	layout  layout
	whole   json.RawMessage
}

// Mapping of one known key to typed state, zero being the value an always-present key takes when empty
type field struct {
	key  string
	enc  func(dst []byte, l layout) ([]byte, bool)
	same func() bool
	zero string
}

// Lookup from a key to its position in a field table, negative for an unknown key
func fieldIndex(fields []field) func(key string) int {
	if len(fields) <= 32 {
		return func(key string) int {
			for i := range fields {
				if fields[i].key == key {
					return i
				}
			}
			return -1
		}
	}
	index := make(map[string]int, len(fields))
	for i := len(fields) - 1; i >= 0; i-- {
		index[fields[i].key] = i
	}
	return func(key string) int {
		if i, ok := index[key]; ok {
			return i
		}
		return -1
	}
}

// Reads a valid JSON object into its members, each value still pointing into the returned text
func readShape(data []byte) (*shape, bool) {
	spans, l, uniform := splitObject(data)
	sh := &shape{layout: l, members: make([]member, len(spans))}
	if !uniform {
		sh.whole = bytes.Clone(data)
		data = sh.whole
	}
	for i, sp := range spans {
		sh.members[i] = member{sp.key, data[sp.start:sp.end:sp.end]}
	}
	return sh, uniform
}

// Drops every member value its field rebuilds and copies the rest, reporting whether a known member kept one
func (sh *shape) settle(fields []field) bool {
	kept, find := false, fieldIndex(fields)
	var scratch []byte
	for i := range sh.members {
		mb := &sh.members[i]
		fi := find(mb.key)
		if sh.whole != nil {
			kept = kept || fi >= 0
			continue
		}
		if fi < 0 {
			mb.raw = bytes.Clone(mb.raw)
			continue
		}
		f := &fields[fi]
		out, ok := f.enc(scratch[:0], sh.layout)
		scratch = out
		if (ok && bytes.Equal(out, mb.raw)) || (!ok && f.zero != "" && string(mb.raw) == f.zero) {
			mb.raw = nil
			continue
		}
		mb.raw = bytes.Clone(mb.raw)
		kept = true
	}
	return kept
}

// Visits the entries to write in order, a nil raw asking the visitor to encode the field, reporting any edit
func (sh *shape) walk(fields []field, emit func(key string, raw []byte, f *field) bool) bool {
	touched := sh == nil || sh.whole == nil
	seen, find := make([]bool, len(fields)), fieldIndex(fields)
	if sh != nil {
		for _, mb := range sh.members {
			fi := find(mb.key)
			if fi < 0 {
				emit(mb.key, mb.raw, nil)
				continue
			}
			f := &fields[fi]
			if mb.raw != nil && f.same != nil && f.same() {
				emit(mb.key, mb.raw, nil)
				seen[fi] = true
				continue
			}
			touched = true
			if seen[fi] {
				continue
			}
			seen[fi] = true
			if !emit(mb.key, nil, f) && f.zero != "" {
				emit(mb.key, []byte(f.zero), nil)
			}
		}
	}
	for fi := range fields {
		if seen[fi] {
			continue
		}
		f := &fields[fi]
		if emit(f.key, nil, f) {
			touched = true
		} else if sh == nil && f.zero != "" {
			emit(f.key, []byte(f.zero), nil)
		}
	}
	return touched
}

// Appends the object, replaying untouched members as read and encoding the rest from the fields
func (sh *shape) render(dst []byte, fields []field, ambient layout) []byte {
	l := ambient
	if sh != nil {
		l = sh.layout
	}
	start, count := len(dst), 0
	dst = append(dst, '{')
	touched := sh.walk(fields, func(key string, raw []byte, f *field) bool {
		mark := len(dst)
		if count > 0 {
			dst = l.comma(dst)
		}
		dst = l.colon(appendString(dst, key, l))
		if f == nil {
			dst = append(dst, raw...)
			count++
			return true
		}
		out, ok := f.enc(dst, l)
		if !ok {
			dst = dst[:mark]
			return false
		}
		dst = out
		count++
		return true
	})
	if !touched {
		return append(dst[:start], sh.whole...)
	}
	return append(dst, '}')
}

// Field over a string that is absent when empty
func textField(key string, cur, old *string) field {
	return field{
		key: key,
		enc: func(dst []byte, l layout) ([]byte, bool) {
			if *cur == "" {
				return dst, false
			}
			return appendString(dst, *cur, l), true
		},
		same: func() bool { return *cur == *old },
	}
}

// Field over one raw JSON value written exactly as held
func rawField(key string, cur, old *json.RawMessage) field {
	return field{
		key: key,
		enc: func(dst []byte, l layout) ([]byte, bool) {
			if len(*cur) == 0 {
				return dst, false
			}
			return append(dst, *cur...), true
		},
		same: func() bool { return bytes.Equal(*cur, *old) },
	}
}

// Field over a list of raw JSON values written as an array
func rawsField(key string, cur, old *[]json.RawMessage) field {
	return field{
		key: key,
		enc: func(dst []byte, l layout) ([]byte, bool) {
			if len(*cur) == 0 {
				return dst, false
			}
			return appendRawArray(dst, *cur, l), true
		},
		same: func() bool { return sameRaws(*cur, *old) },
	}
}

// Field over a cache marker pointer
func cacheField(cur, old **CacheMarker) field {
	return field{
		key: "cache_control",
		enc: func(dst []byte, l layout) ([]byte, bool) {
			if *cur == nil {
				return dst, false
			}
			return (*cur).appendJSON(dst, l), true
		},
		same: func() bool { return sameCache(*cur, *old) },
	}
}

// Appends the marker as the ephemeral cache_control object
func (c *CacheMarker) appendJSON(dst []byte, l layout) []byte {
	dst = append(dst, '{')
	dst = l.colon(append(dst, `"type"`...))
	dst = append(dst, `"ephemeral"`...)
	if c.TTL != "" {
		dst = l.comma(dst)
		dst = l.colon(append(dst, `"ttl"`...))
		dst = appendString(dst, c.TTL, l)
	}
	return append(dst, '}')
}

// Marker a cache_control value describes, nil when the value is not an object
func parseCache(raw []byte) *CacheMarker {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	c := &CacheMarker{}
	if ttl, ok := objectMember(raw, "ttl"); ok {
		c.TTL, _ = stringValue(ttl)
	}
	return c
}

// Reports whether two marker pointers describe the same marker
func sameCache(a, b *CacheMarker) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Drops the change checks of a field table built without a decoded snapshot
func withoutSame(fields []field) []field {
	for i := range fields {
		fields[i].same = nil
	}
	return fields
}
