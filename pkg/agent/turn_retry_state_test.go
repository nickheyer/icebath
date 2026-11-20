package agent

import "testing"

func TestTurnRetryStateStartsClear(t *testing.T) {
	var s TurnRetryState
	if s != (TurnRetryState{}) || s.AutoRecoveryCyclesUsed != 0 {
		t.Fatal("fresh state is not all guards unset")
	}
	s.HasRetried429 = true
	if s == (TurnRetryState{}) {
		t.Fatal("guard did not stick")
	}
}
