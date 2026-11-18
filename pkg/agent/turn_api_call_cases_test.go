package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runs the plain interrupt handler over an agent that streamed the given partial text
func interruptWithPartial(t *testing.T, streamed string) ([]*state.Message, APIInterruptVerdict) {
	t.Helper()
	caseHome(t)
	a := bareSteerAgent()
	a.quietMode = true
	a.setCurrentStreamedAssistantText(streamed)
	messages := rowsToMessages([]map[string]any{{"role": "user", "content": "start"}})
	verdict, err := a.handleAPIInterrupt(&TurnRetryState{}, nil, &messages, nil, apiCallWallNow(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return messages, verdict
}

func TestRepetitionDominatedPartialIsNotKeptAsTheInterruptedRow(t *testing.T) {
	messages, verdict := interruptWithPartial(t, strings.Repeat("I. ", 1941))
	last := messages[len(messages)-1]
	if last.Role != "assistant" || last.Content != "" || last.DisplayKind != "hidden" || last.APIContent != InterruptedPlaceholder {
		t.Fatalf("interrupted row %+v", last)
	}
	final, _ := verdict.FinalResponse.(string)
	if final != provider.RepetitionLoopInterrupted || strings.Contains(final, "I. I. I.") {
		t.Fatalf("final %q", final)
	}
}

func TestDistinctBatchRowsAreNotMistakenForALoop(t *testing.T) {
	var lines []string
	for i := range 12 {
		lines = append(lines, fmt.Sprintf("INSERT INTO users (id, name, email, created_at) VALUES (%d, 'user%d', 'user%d@example.com', NOW());", i, i, i))
	}
	rows := strings.Join(lines, "\n")
	messages, verdict := interruptWithPartial(t, rows)
	last := messages[len(messages)-1]
	if last.Role != "assistant" || last.Content != rows || verdict.FinalResponse != rows {
		t.Fatalf("interrupted row %+v final %v", last, verdict.FinalResponse)
	}
}

func TestOrdinaryPartialIsKeptAsTheInterruptedRow(t *testing.T) {
	messages, verdict := interruptWithPartial(t, "Visible draft.")
	last := messages[len(messages)-1]
	if last.Role != "assistant" || last.Content != "Visible draft." || !reflect.DeepEqual(last.DisplayMetadata, map[string]any{"interrupted": true}) ||
		verdict.FinalResponse != "Visible draft." {
		t.Fatalf("interrupted row %+v final %v", last, verdict.FinalResponse)
	}
}

func guardJWT(t *testing.T, tier string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"account_tier": tier})
	if err != nil {
		t.Fatal(err)
	}
	seg := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return seg([]byte(`{"alg":"none"}`)) + "." + seg(raw) + ".sig"
}
