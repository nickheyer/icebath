package config

import (
	"reflect"
	"testing"
)

// Stripped documents keep the mapping order of the input like the dict comprehension upstream
func TestStripDefaultsKeepsMappingOrder(t *testing.T) {
	inner := map[string]any{"b": 2, "a": 1, "same": true}
	SetMappingKeys(inner, []string{"b", "a", "same"})
	doc := map[string]any{"zeta": inner, "alpha": 1, "omega": 9}
	SetMappingKeys(doc, []string{"zeta", "alpha", "omega"})
	out := StripDefaults(doc, map[string]any{"alpha": 1, "zeta": map[string]any{"same": true}}, nil)
	if got := MappingKeys(out); !reflect.DeepEqual(got, []string{"zeta", "omega"}) {
		t.Fatalf("root order %v", got)
	}
	if got := MappingKeys(out["zeta"].(map[string]any)); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Fatalf("nested order %v", got)
	}
}
