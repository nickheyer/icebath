package pyjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// JSON value with objects as Object in wire key order and numbers kept as json.Number text
func LoadsOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := orderedValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON value")
	}
	return v, nil
}

func orderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		obj := NewObject()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, _ := kt.(string)
			val, err := orderedValue(dec)
			if err != nil {
				return nil, err
			}
			obj.Set(key, val)
		}
		_, err := dec.Token()
		return obj, err
	case '[':
		list := []any{}
		for dec.More() {
			val, err := orderedValue(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, val)
		}
		_, err := dec.Token()
		return list, err
	}
	return nil, fmt.Errorf("unexpected JSON delimiter %v", delim)
}
