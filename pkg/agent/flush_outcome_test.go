package agent

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestFlushOutcomeNamesDisabledPersistedAndFailed(t *testing.T) {
	if got := (&AIAgent{persistDisabled: true}).flushMessagesToSessionDB([]map[string]any{{"role": "user", "content": "x"}}); got != state.FlushDisabled {
		t.Fatalf("persistence isolated agent %v", got)
	}
	if got := (&AIAgent{}).flushMessagesToSessionDB([]map[string]any{{"role": "user", "content": "x"}}); got != state.FlushDisabled {
		t.Fatalf("agent without a store %v", got)
	}
	a, db := earlyFlushAgent(t, "flush-outcome")
	if got := a.flushMessagesToSessionDB([]map[string]any{{"role": "user", "content": "kept"}}); got != state.FlushPersisted {
		t.Fatalf("live store %v", got)
	}
	if err := db.EndSession("flush-outcome", "compression"); err != nil {
		t.Fatal(err)
	}
	a.lastPersistenceErrorCause = nil
	if got := a.flushMessagesToSessionDB([]map[string]any{{"role": "user", "content": "lost"}}); got != state.FlushFailed {
		t.Fatalf("closed store %v", got)
	}
	if a.lastPersistenceErrorCause == nil {
		t.Fatal("a failed flush left its cause unclassified")
	}
}

func TestToolTurnFlushStopsOnlyOnAFailedWrite(t *testing.T) {
	disabled := &AIAgent{persistDisabled: true}
	if !flushToolTurn(disabled, &LoopState{Messages: []*state.Message{{Role: "assistant", Content: "x"}}}) {
		t.Fatal("a disabled store stopped the tool round")
	}
	a, db := earlyFlushAgent(t, "tool-turn-outcome")
	s := &LoopState{Messages: []*state.Message{{Role: "user", Content: "go"}}}
	if !flushToolTurn(a, s) {
		t.Fatal("a persisted tool turn stopped the tool round")
	}
	if err := db.EndSession("tool-turn-outcome", "compression"); err != nil {
		t.Fatal(err)
	}
	s.Messages = append(s.Messages, &state.Message{Role: "assistant", Content: "call"})
	if flushToolTurn(a, s) {
		t.Fatal("a failed tool turn write let the tools run")
	}
}

func TestGrownParentAdoptionNeedsAPersistedPreflush(t *testing.T) {
	db := newRecoveryDB(t)
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"one", "two", "three"} {
		if err := appendRecoveryRow(db, "s", c); err != nil {
			t.Fatal(err)
		}
	}
	idx := 0
	disabled := &AIAgent{sessionDB: db, sessionID: "s", persistDisabled: true, persistUserMessageIdx: &idx}
	lease := &compressionLease{a: disabled, db: db, sid: "s"}
	if got, err := adoptGrownDurableParent(t.Context(), disabled, lease, []map[string]any{{"role": "user", "content": "one"}}); err != nil || got != nil {
		t.Fatalf("a disabled preflush adopted %v %v", got, err)
	}
	live := &AIAgent{sessionDB: db, sessionID: "s", sessionDBCreated: true, persistUserMessageIdx: &idx}
	lease = &compressionLease{a: live, db: db, sid: "s"}
	got, err := adoptGrownDurableParent(t.Context(), live, lease, []map[string]any{{"role": "user", "content": "one"}})
	if err != nil || len(got) != 4 {
		t.Fatalf("a persisted preflush did not adopt %v %v", got, err)
	}
}
