package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Failure whose text reaches the model as it is, a backend having worded it for the model
type Verbatim string

func (v Verbatim) Error() string { return string(v) }

// What the model reads of a failed call to a backend: a Verbatim failure as it is, any other after the backend and what it was doing
func Failure(title, kind string, err error) string {
	if text, ok := errors.AsType[Verbatim](err); ok {
		return string(text)
	}
	return fmt.Sprintf("%s %s failed: %v", title, kind, err)
}

// Kinds of JSON value a reply is read by
const (
	KindList    = "a list"
	KindMapping = "a mapping"
)

// Kind of a JSON value, as a failure names what it got
func Kind(data []byte) string {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return "nothing"
	}
	switch data[0] {
	case 'n':
		return "nothing"
	case 't', 'f':
		return "true or false"
	case '"':
		return "text"
	case '[':
		return KindList
	case '{':
		return KindMapping
	}
	return "a number"
}

// Whether a JSON value holds something: not null, false or zero, nor empty text, list or object
func Holds(data []byte) bool {
	data = bytes.TrimSpace(data)
	switch {
	case len(data) == 0:
		return false
	case data[0] == '"':
		return len(data) > 2
	case data[0] == '[' || data[0] == '{':
		return len(bytes.TrimSpace(data[1:len(data)-1])) > 0
	case data[0] == 't':
		return true
	case data[0] == 'f' || data[0] == 'n':
		return false
	}
	n, _ := strconv.ParseFloat(string(data), 64)
	return n != 0
}

// Decodes a JSON object into v, failing with what it got when the value is something else
func Object(data []byte, v any) error {
	if got := Kind(data); got != KindMapping {
		return fmt.Errorf("expected a mapping, got %s", got)
	}
	return json.Unmarshal(data, v)
}

// Texts of a JSON list, or of a lone value, the items holding nothing left out
type Texts []string

func (t *Texts) UnmarshalJSON(data []byte) error {
	items := []json.RawMessage{data}
	if Kind(data) == KindList {
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
	}
	*t = nil
	for _, item := range items {
		if !Holds(item) {
			continue
		}
		var value Text
		if err := json.Unmarshal(item, &value); err != nil {
			return err
		}
		*t = append(*t, value.String())
	}
	return nil
}
