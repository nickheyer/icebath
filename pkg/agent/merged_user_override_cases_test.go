package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Agent persisting the current turn user row at index one with the override
func agentPersisting(override any) *AIAgent {
	idx := 1
	return &AIAgent{persistUserMessageIdx: &idx, persistUserMessageOverride: override}
}

func TestReplayFlushRowKeepsPrefixAndExactApiSidecar(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	wire := "[2026-04-13T17:02:06+0200] can you also run the smoke tests"
	clean, _ := gateway.StripLeadingMessageTimestamps(wire, time.Local)
	messages := []map[string]any{
		{"role": "assistant", "content": "previous response"},
		{"role": "user", "content": "please deploy build 42 to staging"},
		{"role": "user", "content": wire},
	}
	if repairs, err := state.RepairMessageSequence(&messages); err != nil || repairs != 1 {
		t.Fatalf("repairs %d err %v", repairs, err)
	}
	mergedWire := messages[1]["content"]
	if EstimateMessagesTokensRough(ctx, messages[1:2]) != EstimateMessagesTokensRough(ctx, []map[string]any{{"role": "user", "content": mergedWire}}) {
		t.Fatal("the recorded prefix must not be priced")
	}
	a := agentPersisting(clean)
	expected := "please deploy build 42 to staging\n\ncan you also run the smoke tests"
	row := dbFlushRow(a, messages[1], true)
	if row["content"] != expected || row["api_content"] != mergedWire {
		t.Fatalf("row %v", row)
	}
	a.applyPersistUserMessageOverride(messages)
	if messages[1]["content"] != expected {
		t.Fatalf("override gave %v", messages[1]["content"])
	}
}

func TestEmptyGatewayTimestampTurnPreservesAllUnansweredRowsOnLiveAndReplay(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	clean, _ := gateway.StripLeadingMessageTimestamps("[2026-04-13T17:02:06+0200]", time.Local)
	if clean != "" {
		t.Fatalf("clean %q", clean)
	}
	for _, prior := range [][]string{{"request R"}, {"request R", "request U"}} {
		messages := []map[string]any{{"role": "assistant", "content": "previous response"}}
		for _, text := range prior {
			messages = append(messages, map[string]any{"role": "user", "content": text})
		}
		messages = append(messages, map[string]any{"role": "user", "content": clean})
		if repairs, err := state.RepairMessageSequence(&messages); err != nil || repairs != len(prior) {
			t.Fatalf("repairs %d err %v", repairs, err)
		}
		expected := strings.Join(prior, "\n\n")
		if messages[1]["content"] != expected {
			t.Fatalf("merged %v", messages[1]["content"])
		}
		a := agentPersisting(clean)
		a.applyPersistUserMessageOverride(messages)
		if messages[1]["content"] != expected {
			t.Fatalf("override gave %v", messages[1]["content"])
		}
		if row := dbFlushRow(a, messages[1], true); row["content"] != expected {
			t.Fatalf("row %v", row["content"])
		}
		a.applyPersistUserMessageOverride(messages)
		if messages[1]["content"] != expected || dbFlushRow(a, messages[1], true)["content"] != expected {
			t.Fatalf("second override gave %v", messages[1]["content"])
		}
		if got := dbFlushRow(agentPersisting("later"), messages[1], true)["content"]; got != expected+"\n\nlater" {
			t.Fatalf("later override row %v", got)
		}
	}
}
