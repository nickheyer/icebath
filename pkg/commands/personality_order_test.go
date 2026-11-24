package commands

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Merged personalities keep the dict order of the merge, an overridden builtin staying in its builtin slot
func TestAvailablePersonalitiesKeepDictOrder(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	user := map[string]any{"Zeta": "z", "alpha": "a", "helpful": "h", "none": "ignored", "beta": "b"}
	config.SetMappingKeys(user, []string{"Zeta", "alpha", "helpful", "none", "beta"})
	merged := AvailablePersonalities(config.Tree{"personalities": user})
	want := append(append([]string{}, builtinPersonalityOrder...), "zeta", "alpha", "beta")
	if got := PersonalityNames(merged); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v", got)
	}
	if merged["zeta"] != "z" || merged["helpful"] != "h" || merged["none"] != nil {
		t.Fatalf("merged = %v", merged)
	}
}
