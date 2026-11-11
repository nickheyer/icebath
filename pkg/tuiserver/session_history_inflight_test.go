package tuiserver

import (
	"errors"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session record of the failed turn retention cases with no turn in flight
func retentionSession() *tuigateway.Session {
	return tuigateway.NewSession(map[string]any{"image_counter": 0, "cols": 80, "slash_worker": nil, "show_reasoning": false,
		"tool_progress_mode": "all", "inflight_turn": nil})
}

func TestFailInflightTurnRetainsPartialAndError(t *testing.T) {
	session := retentionSession()
	startInflightTurn(session, "do the thing", "", nil)
	appendInflightDelta(session, "partial answer")
	failInflightTurn(session, errors.New("provider exploded"), nil)
	snapshot := inflightSnapshot(session)
	if snapshot == nil {
		t.Fatal("no snapshot")
	}
	want := map[string]any{"user": "do the thing", "assistant": "partial answer", "streaming": false,
		"error": "provider exploded", "status": "error", "recoverable": true}
	for key, value := range want {
		if snapshot[key] != value {
			t.Fatalf("%s = %v in %v", key, snapshot[key], snapshot)
		}
	}
}

func TestSnapshotReturnedForErrorOnlyTurn(t *testing.T) {
	session := retentionSession()
	failInflightTurn(session, "agent initialization failed", nil)
	snapshot := inflightSnapshot(session)
	if snapshot == nil || snapshot["error"] != "agent initialization failed" {
		t.Fatalf("snapshot %v", snapshot)
	}
}

func TestHealthySnapshotCarriesNoErrorKeys(t *testing.T) {
	session := retentionSession()
	startInflightTurn(session, "hi", "", nil)
	appendInflightDelta(session, "hello")
	if got := inflightSnapshot(session); !reflect.DeepEqual(got, map[string]any{"assistant": "hello", "streaming": true, "user": "hi"}) {
		t.Fatalf("snapshot %v", got)
	}
}

func TestSessionRedirectRecordsCorrectionWithoutErasingPrompt(t *testing.T) {
	session := tuigateway.NewSession(nil)
	startInflightTurn(session, "remove the session counts", "", nil)
	appendInflightDelta(session, "Moving.")
	recordInflightCorrection(session, "hurry up")
	recordInflightCorrection(session, "and the worktree ones")
	snapshot := inflightSnapshot(session)
	if snapshot == nil || snapshot["user"] != "remove the session counts" {
		t.Fatalf("snapshot %v", snapshot)
	}
	if !reflect.DeepEqual(snapshot["corrections"], []any{"hurry up", "and the worktree ones"}) {
		t.Fatalf("corrections %v", snapshot["corrections"])
	}
}

func TestInflightSnapshotCarriesArrivalOrderOffsets(t *testing.T) {
	session := tuigateway.NewSession(nil)
	startInflightTurn(session, "remove the session counts", "", nil)
	appendInflightDelta(session, "Moving.")
	recordInflightCorrection(session, "hurry up")
	appendInflightDelta(session, "Still.")
	recordInflightCorrection(session, "and the worktree ones")
	appendInflightDelta(session, "Done soon.")
	snapshot := inflightSnapshot(session)
	if snapshot == nil || !reflect.DeepEqual(snapshot["corrections"], []any{"hurry up", "and the worktree ones"}) {
		t.Fatalf("snapshot %v", snapshot)
	}
	if !reflect.DeepEqual(snapshot["correction_offsets"], []any{len("Moving."), len("Moving.Still.")}) {
		t.Fatalf("offsets %v", snapshot["correction_offsets"])
	}
}

func TestNewTurnDoesNotInheritPriorTurnCorrections(t *testing.T) {
	session := tuigateway.NewSession(nil)
	startInflightTurn(session, "first prompt", "", nil)
	recordInflightCorrection(session, "first correction")
	startInflightTurn(session, "second prompt", "", nil)
	snapshot := inflightSnapshot(session)
	if snapshot == nil || snapshot["user"] != "second prompt" {
		t.Fatalf("snapshot %v", snapshot)
	}
	if _, has := snapshot["corrections"]; has {
		t.Fatalf("inherited corrections %v", snapshot)
	}
}
