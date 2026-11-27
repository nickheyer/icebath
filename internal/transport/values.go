package transport

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Text a decoded JSON value holds, empty for anything but a string
func Text(v any) string {
	s, _ := v.(string)
	return s
}

// Copy of JSON shaped data that shares nothing with it; values of other Go types in it come back as JSON decodes them
func DeepCopy(v any) any {
	switch t := v.(type) {
	case nil, string, bool, float64, float32, int, int64, int32, uint, uint64, uint32, json.Number:
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, value := range t {
			out[k] = DeepCopy(value)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, value := range t {
			out[i] = DeepCopy(value)
		}
		return out
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return v
	}
	return out
}

// What a wire kept of a message under its carrier key, the zero value when it kept nothing there
func Carried[T any](m *sessionsv1.Message, key string) T {
	var kept T
	if raw := m.GetCarriers()[key]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &kept)
	}
	return kept
}

// Address of a path under the base URL, the query the base carries, such as Azure's api-version, kept on it
func Endpoint(base, path string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", fmt.Errorf("The Base URL %s can't be read: %w", base, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("The Base URL %s must be a full address, such as https://host/v1.", base)
	}
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	return parsed.String(), nil
}
