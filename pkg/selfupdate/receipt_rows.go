package selfupdate

import (
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// True for a decoded JSON object
func isRowDict(v any) bool {
	_, _, ok := rowLookup(v, "")
	return ok
}

// Value under key of a decoded JSON object, with key presence and whether v is an object
func rowLookup(v any, key string) (value any, present, isDict bool) {
	switch row := v.(type) {
	case map[string]any:
		if row == nil {
			return nil, false, false
		}
		value, present = row[key]
		return value, present, true
	case *pyjson.Object:
		if row == nil {
			return nil, false, false
		}
		value, present = row.Get(key)
		return value, present, true
	}
	return nil, false, false
}

// Value under key of a row, nil when the row is not an object or lacks the key
func rowValue(v any, key string) any {
	value, _, _ := rowLookup(v, key)
	return value
}

// Value under key of a row, the fallback when the key is absent
func rowValueOr(v any, key string, fallback any) any {
	if value, present, _ := rowLookup(v, key); present {
		return value
	}
	return fallback
}

// Elements of a truthy list, none for a falsy value, an error for any other value
func rowsOf(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("receipt rows are %s, expected a list", errchain.Kind(v))
	}
	return items, nil
}

// Integer of a value whose Python type is exactly int
func exactInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

// Text of a missing mapping key
func pyKeyErrorText(key string) string { return "missing " + key }

// Value of key in a JSON object, the KeyError or TypeError text when it is absent or not subscriptable
func pyDictItem(holder any, key string) (any, error) {
	obj, ok := holder.(*pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("record is %s where a mapping holding %s was expected", errchain.Kind(holder), key)
	}
	value, present := obj.Get(key)
	if !present {
		return nil, fmt.Errorf("%s", pyKeyErrorText(key))
	}
	return value, nil
}

// Reads a request file as utf-8-sig JSON
func readRequestFile(path string) (any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Items of a JSON value iterated the way a Python for loop does, a TypeError for a non iterable
func pyIterable(value any) ([]any, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case []any:
		return v, nil
	case string:
		out := make([]any, 0, len(v))
		for _, r := range v {
			out = append(out, string(r))
		}
		return out, nil
	case *pyjson.Object:
		out := make([]any, 0, v.Len())
		for _, key := range v.Keys {
			out = append(out, key)
		}
		return out, nil
	}
	return nil, fmt.Errorf("record value is %s where a list was expected", errchain.Kind(value))
}
