package commands

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Merged personalities keep the dict order of the merge, an overridden builtin staying in its builtin slot
func TestAvailablePersonalitiesKeepDictOrder(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	user := map[string]any{"Zeta": "z", "alpha": "a", "helpful": "h"}
	config.SetMappingKeys(user, []string{"Zeta", "alpha", "helpful"})
	agentUser := map[string]any{"beta": "b", "none": "ignored", "zeta": "z2"}
	config.SetMappingKeys(agentUser, []string{"beta", "none", "zeta"})
	cfg := config.Tree{"personalities": user, "agent": map[string]any{"personalities": agentUser}}
	merged := AvailablePersonalities(cfg)
	want := append(append([]string{}, builtinPersonalityOrder...), "zeta", "alpha", "beta")
	if got := PersonalityNames(merged); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v", got)
	}
	if merged["zeta"] != "z2" || merged["helpful"] != "h" {
		t.Fatalf("merged = %v", merged)
	}
}
