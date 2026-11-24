package provider

import (
	"maps"
	"net/http"
	"slices"
	"strings"
)

// One header field of a request
type headerField struct{ Name, Value string }

// Header holding the fields, a later field replacing an earlier one of the same name
func headerOf(fields ...headerField) http.Header {
	h := http.Header{}
	setFields(h, fields)
	return h
}

// Sets every field on the header, replacing what it held under the same name
func setFields(h http.Header, fields []headerField) {
	for _, f := range fields {
		h.Set(f.Name, f.Value)
	}
}

// Fields of a header sorted by name, the values of a repeated name joined by a comma and a space
func fieldsOf(h http.Header) []headerField {
	out := make([]headerField, 0, len(h))
	for _, name := range slices.Sorted(maps.Keys(h)) {
		out = append(out, headerField{Name: name, Value: strings.Join(h[name], ", ")})
	}
	return out
}

// String map of the fields, a later field replacing an earlier one of the same name
func fieldMap(fields ...headerField) map[string]string {
	m := make(map[string]string, len(fields))
	for _, f := range fields {
		m[f.Name] = f.Value
	}
	return m
}

// The request carrying exactly this header
func withHeader(req *http.Request, h http.Header) *http.Request {
	req.Header = h.Clone()
	return req
}
