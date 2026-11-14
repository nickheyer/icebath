package botmode

import "testing"

// Sender waiter outlives one re-offered delivery past the desktop deliver deadline
func TestWaiterOutlivesTheDesktopDeliverDeadline(t *testing.T) {
	liveHold := TurnWaitSecondsFallback + TurnAttemptTimeoutSeconds*TurnMaxAttempts
	desktopBudget := liveHold + DesktopDeliverSettlementMarginSeconds
	if DesktopDeliverTimeoutSeconds != desktopBudget {
		t.Fatalf("DesktopDeliverTimeoutSeconds = %d, want %d", DesktopDeliverTimeoutSeconds, desktopBudget)
	}
	if !(ReofferAfterSeconds > desktopBudget && desktopBudget > liveHold) {
		t.Fatalf("want ReofferAfterSeconds %d > desktop budget %d > live hold %d", ReofferAfterSeconds, desktopBudget, liveHold)
	}
	if !(ReplyWaitSeconds > ReofferAfterSeconds+desktopBudget) {
		t.Fatalf("ReplyWaitSeconds %d must exceed %d", ReplyWaitSeconds, ReofferAfterSeconds+desktopBudget)
	}
}
