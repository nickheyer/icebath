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
		{"0.5", "", "expected a number, got text"},
		{[]any{1}, "", "expected a number, got a list"},
		{nil, "", "expected a number, got nothing"},
		{map[string]any{}, "", "expected a number, got a mapping"},
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
		seen = len(other.Names())
		return []NamedEnvVar{{"B", EnvVar{Description: "b"}}}
	}, nil)
	items := table.Items()
	if len(items) != 2 || items[0].Entry.Description != "a" || items[1].Entry.Description != "b" {
		t.Fatalf("items %#v", items)
	}
	if seen != 1 {
		t.Fatalf("a fill read another table with its fill run, saw %d entries", seen)
	}
	if len(other.Names()) != 2 {
		t.Fatal("the other table never ran its own fill")
	}
}
