package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// JSON text decoded with numbers kept as written, panicking on bad input
func gemJS(text string) any {
	v, err := geminiJSONLoads(text)
	if err != nil {
		panic("bad test JSON: " + err.Error() + ": " + text)
	}
	return v
}

// JSON object text decoded to a map
func gemMap(text string) map[string]any { return gemJS(text).(map[string]any) }

// JSON array text decoded to a list
func gemList(text string) []any { return gemJS(text).([]any) }

// Canonical JSON text of a value with sorted keys
func gemCanon(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %#v: %v", v, err)
	}
	var back any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, _ := json.Marshal(back)
	return string(out)
}

// Fails unless a value equals the JSON text
func gemEq(t *testing.T, got any, want string) {
	t.Helper()
	if g, w := gemCanon(t, got), gemCanon(t, gemJS(want)); g != w {
		t.Fatalf("mismatch\n got: %s\nwant: %s", g, w)
	}
}

// Nested map lookup by keys and list indexes
func gemAt(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			switch l := v.(type) {
			case []any:
				if k < 0 {
					k += len(l)
				}
				v = l[k]
			case []map[string]any:
				if k < 0 {
					k += len(l)
				}
				v = l[k]
			default:
				return nil
			}
		}
	}
	return v
}

// Roles of built contents in order
func gemRoles(contents []map[string]any) []string {
	out := make([]string, 0, len(contents))
	for _, c := range contents {
		out = append(out, c["role"].(string))
	}
	return out
}
