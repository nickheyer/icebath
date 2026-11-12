package agent

import (
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestSplicedTranscriptSurvivesRepairMessageSequence(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := microCompressor(t, "ROLLING SUMMARY")
	messages := microConversation(8)
	for range 3 {
		messages = mustMicro(t, c, messages)
		repairs, err := state.RepairMessageSequence(&messages)
		if err != nil || repairs != 0 {
			t.Fatalf("micro-compacted transcript must already be alternation-valid: %d %v", repairs, err)
		}
		if len(summaryMarkers(messages)) != 1 {
			t.Fatal("marker destroyed by repair pass")
		}
		for _, m := range messages {
			if m["role"] == "user" && !pyTruthy(m[state.CompressedSummaryMetadataKey]) && strings.Contains(fmt.Sprint(m["content"]), "ROLLING SUMMARY") {
				t.Fatal("summary text leaked into a real user message")
			}
		}
	}
}

func TestFreshUserDoesNotMutatePersistedCarrierDict(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	carrier := standaloneHandoff()
	carrier["content"] = carrier["content"].(string) + "\n\nREAL ASK"
	original := maps.Clone(carrier)
	fresh := map[string]any{"role": "user", "content": "NEXT ASK"}
	messages := []map[string]any{carrier, fresh}
	repairs, err := state.RepairMessageSequence(&messages)
	if err != nil || repairs != 0 {
		t.Fatalf("repairs %d err %v", repairs, err)
	}
	if len(messages) != 2 || reflect.ValueOf(messages[0]).Pointer() != reflect.ValueOf(carrier).Pointer() ||
		!reflect.DeepEqual(messages[1], fresh) || !reflect.DeepEqual(carrier, original) {
		t.Fatalf("carrier mutated %v", messages)
	}
}
