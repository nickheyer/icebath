package qqbot

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// User agent naming the adapter version, the Go runtime, the OS and the icebath version
func userAgent() string {
	version := updaterecord.BuildVersion()
	if version == "" {
		version = "dev"
	}
	return fmt.Sprintf("QQBotAdapter/%s (Go/%s; %s; Icebath/%s)", adapterVersion,
		strings.TrimPrefix(runtime.Version(), "go"), runtime.GOOS, version)
}

// Trimmed non empty strings of a comma separated string, a list or a scalar
func coerceList(value any) []string {
	var items []string
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		items = strings.Split(v, ",")
	case []string:
		items = v
	case []any:
		for _, item := range v {
			items = append(items, jstr(item))
		}
	default:
		items = []string{jstr(v)}
	}
	var out []string
	for _, item := range items {
		if s := strings.TrimSpace(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Text of a decoded JSON value, numbers in their literal form and nil as empty
func jstr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

// Object of a decoded JSON value, nil for anything else
func jmap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// Array of a decoded JSON value, nil for anything else
func jlist(v any) []any {
	l, _ := v.([]any)
	return l
}

// Integer of a decoded JSON number or numeric string, false when it is neither
func jint(v any) (int64, bool) {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		if f, err := x.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(x), true
	case int:
		return int64(x), true
	case int64:
		return x, true
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// Float of a decoded JSON number or numeric string, the default when it is neither
func jfloat(v any, def float64) float64 {
	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f
		}
	}
	return def
}

// Boolean of a config value, strings read as false only for the usual off words
func jbool(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "", "0", "false", "no", "off":
			return false
		}
		return true
	}
	n, ok := jint(v)
	return !ok || n != 0
}

// Decodes JSON with numbers kept exact
func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// First n code points of text
func cutRunes(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return string(r[:n])
}
