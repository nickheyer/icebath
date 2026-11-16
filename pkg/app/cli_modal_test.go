package app

import (
	"testing"
	"time"
)

// A CLI whose paint gates are both set adversarially, a pending resize and a recent throttled paint
func newRealPaintCLI(t *testing.T) (*CLI, *fakeTUIApp) {
	t.Helper()
	c, app := newModalTestCLI(t, nil)
	c.resizeRecoveryPending = true
	now := monotonic()
	c.lastInvalidate = &now
	return c, app
}

func TestModalPaintNowPaintNowBypassesThrottleAndResizeGuard(t *testing.T) {
	c, app := newRealPaintCLI(t)
	_ = c.invalidate()
	if app.invalidations() != 0 {
		t.Fatal("a throttled invalidate painted under both gates")
	}
	c.paintNow()
	if app.invalidations() == 0 {
		t.Fatal("paint_now did not paint")
	}
}

func TestClearOverlaysForInterruptInterruptUnblocksThreadBlockedOnApproval(t *testing.T) {
	c, _ := newModalTestCLI(t, nil)
	approvalQ := make(chan string, 1)
	c.approvalState = &approvalModalState{response: approvalQ}
	results := make(chan string, 1)
	go func() {
		select {
		case v := <-approvalQ:
			results <- v
		case <-time.After(2 * time.Second):
			results <- "<timeout>"
		}
	}()
	time.Sleep(50 * time.Millisecond)
	c.clearActiveOverlaysForInterrupt()
	if got := <-results; got != "deny" {
		t.Fatalf("worker got %q", got)
	}
}
