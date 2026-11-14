package pyrepr

import (
	"reflect"
	"testing"
)

func TestFormatFieldNamesMatchesFormatterParse(t *testing.T) {
	cases := []struct {
		in    string
		names []string
		err   string
	}{
		{"plain", nil, ""},
		{"{a} and {b!r:>3} {{x}} {}", []string{"a", "b", ""}, ""},
		{"{a[k]:{w}}", []string{"a[k]"}, ""},
		{"{0.x}", []string{"0.x"}, ""},
		{"tail {", nil, "Single '{' encountered in format string"},
		{"x } y", nil, "Single '}' encountered in format string"},
		{"{abc", nil, "expected '}' before end of string"},
		{"{a!}", nil, "end of string while looking for conversion specifier"},
		{"{a!rx}", nil, "expected ':' after conversion specifier"},
	}
	for _, c := range cases {
		got, err := FormatFieldNames(c.in)
		if c.err != "" {
			if err == nil || err.(*FormatError).Msg != c.err {
				t.Fatalf("%q error %v want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.names) {
			t.Fatalf("%q got %#v %v want %#v", c.in, got, err, c.names)
		}
	}
	if _, err := FormatMap("tail {", nil); err == nil || err.(*FormatError).Msg != "Single '{' encountered in format string" {
		t.Fatalf("format_map single brace %v", err)
	}
}
