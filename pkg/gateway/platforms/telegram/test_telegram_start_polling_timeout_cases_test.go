package telegram

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNetworkLadderStartPollingHangDoesNotWedge(t *testing.T) {
	app := newMockApp()
	app.updater.running = false
	app.polling = nil
	app.updater.startFn = func(context.Context, bool, func(error)) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	bounded, _ := newTestLifecycle(t, app)
	bounded.timings.updaterStartTimeout = 0.05
	finished := make(chan struct{})
	go func() {
		bounded.handlePollingNetworkError(context.Background(), &pyExc{class: "Exception", msg: "net down"})
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("a hung start_polling wedged the ladder")
	}
}

func TestInitialConnectSucceedsOnCurrentGenerationProgress(t *testing.T) {
	app := newMockApp()
	p, _ := newTestLifecycle(t, app)
	p.timings.initialProgressTimeout = 5
	app.updater.startFn = func(context.Context, bool, func(error)) error {
		p.mu.Lock()
		generation := p.generation
		p.mu.Unlock()
		p.recordPollingProgress(context.Background(), generation)
		return nil
	}
	ok, err := p.startPollingResilient(context.Background(), true, nil, true)
	if !ok || err != nil || p.isSendPathDegraded() || p.verifierTask != nil {
		t.Fatalf("ok %v err %v degraded %v verifier %v", ok, err, p.isSendPathDegraded(), p.verifierTask)
	}
}

func TestInitialConnectPollingErrorFailsFastNotBackground(t *testing.T) {
	app := newMockApp()
	p, host := newTestLifecycle(t, app)
	p.timings.initialProgressTimeout = 30
	var recovery []error
	app.updater.startFn = func(_ context.Context, _ bool, cb func(error)) error {
		go cb(&osError{msg: "getUpdates failed"})
		return nil
	}
	ok, err := p.startPollingResilient(context.Background(), true, func(e error) { recovery = append(recovery, e) }, true)
	if ok || err == nil || className(err) != "OSError" || !strings.Contains(err.Error(), "errored before first getUpdates") {
		t.Fatalf("want OSError errored before first getUpdates, got %v %v", ok, err)
	}
	if len(recovery) != 0 || len(host.pendingTasks()) != 0 {
		t.Fatal("a cold start polling error must not schedule background recovery")
	}
}
