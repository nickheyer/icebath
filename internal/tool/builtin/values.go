package builtin

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/model/wire"
)

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		return err != nil || f != 0
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case []any:
		return len(t) > 0
	case []string:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func pyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		return string(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	raw, err := fileutil.EncodeJSON(v, fileutil.JSONStyle{})
	if err != nil {
		return ""
	}
	return string(raw)
}

func pyTypeName(v any) string {
	switch t := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			return "float"
		}
		return "int"
	case float64:
		return "float"
	case int, int64:
		return "int"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return "object"
}

func pyInt(v any) (int64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case json.Number:
		if n, err := strconv.ParseInt(string(t), 10, 64); err == nil {
			return n, true
		}
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil || math.IsNaN(f) || f > 9e18 || f < -9e18 {
			return 0, false
		}
		return int64(f), true
	case float64:
		if math.IsNaN(t) || t > 9e18 || t < -9e18 {
			return 0, false
		}
		return int64(t), true
	case int:
		return int64(t), true
	case int64:
		return t, true
	case string:
		if !isInt(t) {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(strip(t), "_", ""), 10, 64)
		return n, err == nil
	}
	return 0, false
}

type object struct {
	members []wire.Member
	err     error
}

func (o *object) set(key string, v any) {
	raw, err := encodeValue(v)
	if err != nil {
		if o.err == nil {
			o.err = err
		}
		return
	}
	for i := range o.members {
		if o.members[i].Key == key {
			o.members[i].Value = raw
			return
		}
	}
	o.members = append(o.members, wire.Member{Key: key, Value: raw})
}

func (o *object) get(key string) (json.RawMessage, bool) {
	for _, m := range o.members {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

func (o *object) json() (json.RawMessage, error) {
	if o.err != nil {
		return nil, o.err
	}
	return wire.Object(o.members), nil
}

func encodeValue(v any) (json.RawMessage, error) {
	switch t := v.(type) {
	case json.RawMessage:
		return t, nil
	case *object:
		return t.json()
	case []*object:
		items := make([]json.RawMessage, len(t))
		for i, o := range t {
			raw, err := o.json()
			if err != nil {
				return nil, err
			}
			items[i] = raw
		}
		return encodeValue(items)
	}
	raw, err := fileutil.EncodeJSON(v, fileutil.JSONStyle{Tight: true})
	if err != nil {
		return nil, err
	}
	return raw, nil
}
