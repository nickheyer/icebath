package state

import "testing"

func TestWindowsNeverArmsTheGuard(t *testing.T) {
	if lockGuardSupported() {
		t.Fatal("guard armed on Windows")
	}
	if held := lockGuardHold("state.db", nil); len(held) != 0 {
		t.Fatalf("held = %v", held)
	}
	lockGuardRelease(lockGuardHeld{})
}
