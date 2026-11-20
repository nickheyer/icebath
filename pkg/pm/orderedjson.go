package pm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// OrderedObject is a JSON object that keeps its key order across a load and save
type OrderedObject struct {
	Keys   []string
	Values map[string]any
}

// Empty ordered object
func NewOrderedObject() *OrderedObject { return &OrderedObject{Values: map[string]any{}} }

// Value of a key
func (o *OrderedObject) Get(key string) (any, bool) {
	v, ok := o.Values[key]
	return v, ok
}

// Sets a key, appending it when new
func (o *OrderedObject) Set(key string, value any) {
	if _, ok := o.Values[key]; !ok {
		o.Keys = append(o.Keys, key)
	}
	o.Values[key] = value
}

// Encodes the object with its keys in their recorded order
func (o *OrderedObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.Keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := encodeNoHTML(k)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		value, err := encodeNoHTML(o.Values[k])
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Compact JSON of a value with angle brackets and ampersands left literal as Python writes them
func encodeNoHTML(v any) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// Decodes JSON with every object as an ordered object and numbers kept textual
func loadOrderedJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(pyJSONSource(trimBOM(data))))
	dec.UseNumber()
	v, err := decodeOrdered(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("extra data after JSON value")
	}
	return v, nil
}

// Decodes one value from the token stream
func decodeOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := NewOrderedObject()
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyTok.(string)
				value, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				obj.Set(key, value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			list := []any{}
			for dec.More() {
				value, err := decodeOrdered(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	}
	return normalizePyJSON(tok), nil
}
