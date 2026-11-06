package pyrepr

import (
	"strings"
	"testing"
)

func TestFormatMapMatchesPython(t *testing.T) {
	values := map[string]any{"path": "a.py", "offset": 1, "content_len": 12345, "n": nil, "list": []any{"x", "y"},
		"m": map[string]any{"k": "v"}, "w": 6}
	cases := map[string]string{
		"[read_file] read {path} from line {offset} ({content_len:,} chars)": "[read_file] read a.py from line 1 (12,345 chars)",
		"{{lit}} {n} {list[1]} {m[k]} {path!r}":                              "{lit} None y v 'a.py'",
		"[{path:>{w}}]":                                                      "[  a.py]",
		"{list}":                                                             "['x', 'y']",
	}
	for tmpl, want := range cases {
		got, err := FormatMap(tmpl, values)
		if err != nil || got != want {
			t.Fatalf("%q: got %q, %v want %q", tmpl, got, err, want)
		}
	}
	errs := map[string]string{
		"{missing}":    "KeyError: 'missing'",
		"{}":           "IndexError: Replacement index 0 out of range for positional args tuple",
		"{path.upper}": "AttributeError",
		"x}":           "ValueError: Single '}' encountered in format string",
		"{path":        "ValueError: expected '}' before end of string",
		"{offset:,s}":  "ValueError",
		"{path!x}":     "ValueError: Unknown conversion specifier x",
		"{list[a]}":    "TypeError: list indices must be integers or slices, not str",
	}
	for tmpl, want := range errs {
		_, err := FormatMap(tmpl, values)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("%q: got %v want %q", tmpl, err, want)
		}
	}
}
