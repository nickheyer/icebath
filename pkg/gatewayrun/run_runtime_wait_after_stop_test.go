package gatewayrun

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

// Started runner with its control socket and session listener, ready to publish
func startedRuntimeRunner(t *testing.T) *GatewayRunner {
	t.Helper()
	h := shortGatewayHome(t)
	ctx := context.Background()
	if err := gateway.ProcessOwnership.Reserve(ctx, []string{h}, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.ProcessOwnership.Close() })
	r, err := NewGatewayRunner(ctx, gateway.DefaultGatewayConfig(ctx))
	if err != nil {
		t.Fatal(err)
	}
	loop := gateway.NewCallbackLoop()
	loopCtx, stopLoop := context.WithCancel(ctx)
	go loop.Run(loopCtx)
	t.Cleanup(func() { loop.Close(); stopLoop() })
	r.gatewayLoop = loop
	if err := r.initializeGatewayRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	server, err := gateway.NewGatewayControlServer("", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.sessionControlServer = server
	if !server.Start() {
		t.Fatal("the control socket did not start")
	}
	t.Cleanup(server.Stop)
	if err := startGatewayRuntimeAPI(ctx, r); err != nil {
		t.Fatal(err)
	}
	if started, err := r.Start(ctx); err != nil || !started {
		t.Fatalf("runner start %v, %v", started, err)
	}
	t.Cleanup(func() { _ = r.Stop(ctx, false, false, false) })
	return r
}

// Runtime wait bounded to ten seconds
func waitRuntimeBounded(t *testing.T, r *GatewayRunner) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return waitGatewayRuntime(ctx, r)
}

// A runtime wait that first observes the listener after a completed stop ends cleanly, the goroutine scheduled late
func TestRuntimeWaitAfterCompletedStopIsClean(t *testing.T) {
	hermetic.Run(t, "late_waiter", func(t *testing.T) {
		r := startedRuntimeRunner(t)
		if err := publishGatewayRuntimeReady(r); err != nil {
			t.Fatal(err)
		}
		if err := r.Stop(context.Background(), false, false, false); err != nil {
			t.Fatal(err)
		}
		if !sessionAPIDone(r) || r.draining.Load() || !r.shutdownEvent.IsSet() {
			t.Fatalf("stop left listener done %v draining %v shutdown %v", sessionAPIDone(r), r.draining.Load(), r.shutdownEvent.IsSet())
		}
		if err := waitRuntimeBounded(t, r); err != nil {
			t.Fatalf("a wait after the gateway's own stop returned %v", err)
		}
		if state := r.SessionRuntimeDescriptor()["state"]; state == "failed" {
			t.Fatalf("the gateway's own stop marked the runtime %v", state)
		}
	})
}

// A stop that finished before the ready tail leaves the runtime unpublished and the wait clean
func TestReadyTailAfterCompletedStopPublishesNothing(t *testing.T) {
	hermetic.Run(t, "stop_first", func(t *testing.T) {
		r := startedRuntimeRunner(t)
		if err := r.Stop(context.Background(), false, false, false); err != nil {
			t.Fatal(err)
		}
		if err := startGatewayReadyTail(context.Background(), r, false); err != nil {
			t.Fatalf("ready tail after the gateway's own stop returned %v", err)
		}
		if state := r.SessionRuntimeDescriptor()["state"]; state == "ready" {
			t.Fatal("a stopped gateway published ready")
		}
		if err := waitRuntimeBounded(t, r); err != nil {
			t.Fatalf("a wait after the gateway's own stop returned %v", err)
		}
	})
}

// A stop requested during the ready tail tears down only after the tail finished, then the wait ends cleanly
func TestStopDuringReadyTailWaitsForIt(t *testing.T) {
	hermetic.Run(t, "stop_inside", func(t *testing.T) {
		r := startedRuntimeRunner(t)
		previous := publishRuntimeReady
		t.Cleanup(func() { publishRuntimeReady = previous })
		stopped := make(chan error, 1)
		runningInsideTail := false
		publishRuntimeReady = func(r *GatewayRunner) error {
			go func() { stopped <- r.Stop(context.Background(), false, false, false) }()
			for deadline := time.Now().Add(10 * time.Second); r.currentStopTask() == nil && time.Now().Before(deadline); {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond)
			runningInsideTail = r.currentStopTask() != nil && r.running.Load() && !r.draining.Load()
			return publishGatewayRuntimeReady(r)
		}
		if err := startGatewayReadyTail(context.Background(), r, false); err != nil {
			t.Fatal(err)
		}
		if !runningInsideTail {
			t.Fatal("the stop tore the runtime down while the ready tail was publishing")
		}
		if err := waitRuntimeBounded(t, r); err != nil {
			t.Fatalf("a wait after a stop requested inside the tail returned %v", err)
		}
		select {
		case err := <-stopped:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the stop never finished after the ready tail")
		}
	})
}
