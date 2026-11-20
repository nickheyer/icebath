package telegram

import (
	"context"
	"testing"
)

// Updater with a live polling stop event and running state for the disarm case
type stopEventApp struct {
	*fakePollingApp
	updater *sdkUpdater
}

func (a *stopEventApp) Updater() pollingUpdater { return a.updater }

func TestConflictRetryDropsPendingUpdates(t *testing.T) {
	app := newMockApp()
	app.polling, app.general = nil, nil
	p, _ := newTestLifecycle(t, app)
	if err := p.handlePollingConflict(context.Background(), &pyExc{class: "Conflict", msg: "Conflict: terminated by other getUpdates request"}); err != nil {
		t.Fatal(err)
	}
	if len(app.updater.drops) != 1 || !app.updater.drops[0] {
		t.Fatalf("conflict retry must drop pending updates, got %v", app.updater.drops)
	}
}

func TestConflictRetryProgressDoesNotResetRetryLadder(t *testing.T) {
	app := newMockApp()
	app.polling, app.general = nil, nil
	p, _ := newTestLifecycle(t, app)
	calls := 0
	app.updater.startFn = func(context.Context, bool, func(error)) error {
		calls++
		p.mu.Lock()
		generation := p.generation
		p.mu.Unlock()
		p.recordPollingProgress(context.Background(), generation)
		return nil
	}
	if err := p.handlePollingConflict(context.Background(), &pyExc{class: "Conflict", msg: "Conflict: terminated by other getUpdates request"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || p.conflictCount != 1 || p.conflictRecoveryGeneration != nil || p.isSendPathDegraded() {
		t.Fatalf("calls %d conflicts %d recovery %v degraded %v", calls, p.conflictCount, p.conflictRecoveryGeneration, p.isSendPathDegraded())
	}
}

func TestDisarmSetsPtbStopEvent(t *testing.T) {
	updater := &sdkUpdater{running: true, stopEvent: newPollEvent()}
	p, _ := newTestLifecycle(t, &stopEventApp{fakePollingApp: newMockApp(), updater: updater})
	if updater.stopEvent.isSet() {
		t.Fatal("stop event starts clear")
	}
	p.disarmPTBRetryLoop()
	if !updater.stopEvent.isSet() {
		t.Fatal("disarm must set the polling stop event")
	}
	if !updater.Running() {
		t.Fatal("disarm must leave running untouched")
	}
}
