package config

import (
	"testing"
)

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
