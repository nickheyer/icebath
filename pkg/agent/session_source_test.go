package agent

import (
	"testing"
)

func TestSessionSourceSets(t *testing.T) {
	for _, src := range []string{"cli", "oneshot"} {
		if !CLIFamilySources[src] {
			t.Fatalf("cli family lacks %s", src)
		}
	}
	if len(CLIFamilySources) != 2 || len(DaybreakSources) != 2 {
		t.Fatalf("set sizes %d %d", len(CLIFamilySources), len(DaybreakSources))
	}
	for _, src := range []string{"tui", "cli"} {
		if !DaybreakSources[src] {
			t.Fatalf("%s missing", src)
		}
	}
	if DaybreakSources["acp"] || DaybreakSources["telegram"] {
		t.Fatal("acp or telegram misplaced")
	}
}
