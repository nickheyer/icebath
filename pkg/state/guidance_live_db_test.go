package state

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuidanceNeverNamesLiveDbRepairBudgetErrorNamesSafeLane(t *testing.T) {
	message := persistentRepairExhaustedError(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	for _, want := range []string{"Manual recovery required", "sessions recover --source", "--inspect-only", "--output recovered-state.db", "do NOT"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q in %q", want, message)
		}
	}
	if strings.Contains(message, ".recover\"`") {
		t.Fatalf("message embeds the live path in a raw sqlite3 command: %q", message)
	}
}
