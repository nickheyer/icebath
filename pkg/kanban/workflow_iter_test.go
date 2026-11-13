package kanban

import "testing"

// Iterating a workflow yields its columns in board order and feeds the CLI status glyph table
func TestWorkflowIterationFeedsStatusIcons(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var keys []string
	for c := range DefaultWorkflow.All() {
		keys = append(keys, c.Key)
		if statusIcons[c.Key] != c.Icon {
			t.Fatalf("glyph of %s is %q", c.Key, statusIcons[c.Key])
		}
	}
	want := DefaultWorkflow.Keys()
	if len(keys) != len(want) || len(statusIcons) != len(want)+1 || statusIcons[Archived] != "—" {
		t.Fatalf("keys %v want %v icons %v", keys, want, statusIcons)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("order %v want %v", keys, want)
		}
	}
}
