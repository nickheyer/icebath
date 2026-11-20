package agent

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func handoffRows(rows ...map[string]any) *[]*state.Message {
	out := make([]*state.Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, state.MessageFromDict(r))
	}
	return &out
}

func TestSkipGuardRestoresRealUser(t *testing.T) {
	t.Run("restores_pending_user_then_allows_continue", func(t *testing.T) {
		messages := handoffRows(map[string]any{"role": "assistant", "content": "done", "finish_reason": "stop"}, standaloneHandoff())
		if shouldSkipModelCallForReferenceHandoff(messages, "new ask after compaction") {
			t.Fatal("skipped although a real user ask was restorable")
		}
		last := state.MessageToDict((*messages)[len(*messages)-1])
		if last["role"] != "user" || last["content"] != "new ask after compaction" {
			t.Fatalf("last row %v", last)
		}
	})
	t.Run("skips_when_no_real_user_to_restore", func(t *testing.T) {
		messages := handoffRows(map[string]any{"role": "assistant", "content": "Refactor complete.", "finish_reason": "stop"}, standaloneHandoff())
		if !shouldSkipModelCallForReferenceHandoff(messages, nil) {
			t.Fatal("did not skip a handoff only tail")
		}
	})
}

func TestCarrierAlternationRepair(t *testing.T) {
	carrier := compositeHandoff("REAL ASK")
	original := map[string]any{}
	for k, v := range carrier {
		original[k] = v
	}
	fresh := map[string]any{"role": "user", "content": "NEXT ASK"}
	messages := []map[string]any{carrier, fresh}
	repairs, err := state.RepairMessageSequence(&messages)
	if err != nil || repairs != 0 {
		t.Fatalf("repairs %d err %v", repairs, err)
	}
	if len(messages) != 2 || reflect.ValueOf(messages[0]).Pointer() != reflect.ValueOf(carrier).Pointer() || !reflect.DeepEqual(messages[1], fresh) {
		t.Fatalf("messages %v", messages)
	}
	if !reflect.DeepEqual(carrier, original) {
		t.Fatalf("carrier mutated %v", carrier)
	}
}

func TestNoToolCallsWithoutLaterRealUser(t *testing.T) {
	handoff := standaloneHandoff()
	handoff["content"] = state.SummaryPrefix + "\n" + state.HistoricalTaskHeading + "\nUser asked: 'run npm test and fix every failure in the suite'\n\n" + state.SummaryEndMarker
	if IsUserOriginatedTurn(handoff) {
		t.Fatal("historical snapshot counted as user originated")
	}
	if !referenceHandoffWouldDriveNextModelCall([]map[string]any{handoff}) {
		t.Fatal("standalone snapshot would not drive the next call")
	}
}
