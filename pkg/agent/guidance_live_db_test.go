package agent

import (
	"context"
	"strings"
	"testing"
)

const liveDBSalvageCommand = `sqlite3 ~/.icebath/state.db ".recover"`

func TestGuidanceNeverNamesLiveDbRunAgentCorruptExplanation(t *testing.T) {
	explanation := formatTurnCompletionExplanation(context.Background(), "session_persistence_failed", "corrupt", "", "")
	if strings.Contains(explanation, liveDBSalvageCommand) || !strings.Contains(explanation, "icebath sessions recover --source") ||
		!strings.Contains(explanation, "--inspect-only") || !strings.Contains(explanation, "--output recovered-state.db") ||
		!strings.Contains(explanation, ".recover") {
		t.Fatalf("explanation %q", explanation)
	}
}
