package config

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestShowPercentFollowsPythonMultiplyAndFormat(t *testing.T) {
	cases := []struct {
		value any
		want  string
		err   string
	}{
		{0.5, "50", ""},
		{int(1), "100", ""},
		{true, "100", ""},
		{math.NaN(), "nan", ""},
		{"0.5", "", "ValueError: Unknown format code 'f' for object of type 'str'"},
		{[]any{1}, "", "TypeError: unsupported format string passed to list.__format__"},
		{nil, "", "TypeError: unsupported operand type(s) for *: 'NoneType' and 'int'"},
		{map[string]any{}, "", "TypeError: unsupported operand type(s) for *: 'dict' and 'int'"},
	}
	for _, c := range cases {
		got, err := showPercent(c.value)
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Fatalf("%#v err %v", c.value, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("%#v got %q err %v", c.value, got, err)
		}
	}
}

func TestCompressionSectionTokenCapUsesPythonInt(t *testing.T) {
	show := func(tt any) (string, error) {
		var buf bytes.Buffer
		err := showPrinter{out: &buf}.compressionSection(map[string]any{"compression": map[string]any{"threshold_tokens": tt}})
		return buf.String(), err
	}
	if out, err := show(" 1_200_000 "); err != nil || !strings.Contains(out, "  Token cap:    1,200,000 tokens (takes lower of ratio vs absolute)") {
		t.Fatalf("%q %v", out, err)
	}
	for _, skipped := range []any{"abc", math.NaN(), int(0), []any{}} {
		if out, err := show(skipped); err != nil || strings.Contains(out, "Token cap") {
			t.Fatalf("%#v printed %q %v", skipped, out, err)
		}
	}
	if _, err := show(math.Inf(1)); err == nil || err.Error() != "OverflowError: cannot convert float infinity to integer" {
		t.Fatalf("infinity err %v", err)
	}
}

func TestOptionalEnvTableValuesAndFillMarkerSpanTables(t *testing.T) {
	other := NewOptionalEnvTable([]NamedEnvVar{{"O", EnvVar{Description: "o"}}})
	other.Arm(func() []NamedEnvVar { return []NamedEnvVar{{"OF", EnvVar{}}} }, nil)
	table := NewOptionalEnvTable([]NamedEnvVar{{"A", EnvVar{Description: "a"}}})
	seen := 0
	table.Arm(func() []NamedEnvVar {
		seen = other.Len()
		return []NamedEnvVar{{"B", EnvVar{Description: "b"}}}
	}, nil)
	values := table.Values()
	if len(values) != 2 || values[0].Description != "a" || values[1].Description != "b" {
		t.Fatalf("values %#v", values)
	}
	if seen != 1 {
		t.Fatalf("a fill read another table with its fill run, saw %d entries", seen)
	}
	if other.Len() != 2 {
		t.Fatal("the other table never ran its own fill")
	}
}
