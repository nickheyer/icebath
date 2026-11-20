package lsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

// JSON decoder of the test runtime, integer literals as int64 and fractions as float64
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, &JSONDecodeError{Msg: err.Error()}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &JSONDecodeError{Msg: "Extra data"}
	}
	return convertNumbers(v), nil
}

// Replaces json.Number leaves with int64, float64, or a json.Number kept for integers past int64
func convertNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return n
			}
			return t
		}
		f, _ := strconv.ParseFloat(s, 64)
		return f
	case map[string]any:
		for k, e := range t {
			t[k] = convertNumbers(e)
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = convertNumbers(e)
		}
		return t
	}
	return v
}
