package gatewayrun

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Stands a listener server in for the dashboard builder for one case
func fakeGatewayListener(t *testing.T) {
	t.Helper()
	prev, prevDist := BuildGatewayListener, gateway.WebappRendererBuild.PackagedDist
	gateway.WebappRendererBuild.PackagedDist = func() string { return "" }
	BuildGatewayListener = func(context.Context, gateway.GatewayAPIRunner, *gateway.ListenerState, *gateway.WebappMount, string, int) (gateway.GatewayListenerServer, error) {
		return &fakeListenerServer{mount: &fakeDashboardMount{}, exit: make(chan struct{})}, nil
	}
	t.Cleanup(func() { BuildGatewayListener, gateway.WebappRendererBuild.PackagedDist = prev, prevDist })
}

// Short home for a case that binds the control socket, removed with the case
func shortGatewayHome(t *testing.T) string {
	t.Helper()
	h, err := os.MkdirTemp("", "gwh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(h) })
	if err := os.Chmod(h, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func TestListenerFailureStopsBootstrapBackgroundThreads(t *testing.T) {
	shortGatewayHome(t)
	fakeGatewayListener(t)
	prevRestore, prevStart, prevPublish := RestoreUserProxyEnv, startCronAndHousekeeping, publishRuntimeReady
	t.Cleanup(func() {
		RestoreUserProxyEnv, startCronAndHousekeeping, publishRuntimeReady = prevRestore, prevStart, prevPublish
	})
	RestoreUserProxyEnv = func(map[string]string) {}
	var owned []cronHousekeeping
	startCronAndHousekeeping = func(ctx context.Context, r *GatewayRunner) cronHousekeeping {
		ch := startGatewayStartCronAndHousekeeping(ctx, r)
		owned = append(owned, ch)
		return ch
	}
	publishRuntimeReady = func(r *GatewayRunner) error {
		if err := publishGatewayRuntimeReady(r); err != nil {
			return err
		}
		r.sessionAPI.server.RequestExit()
		return nil
	}
	type outcome struct {
		success bool
		err     error
	}
	done := make(chan outcome, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		success, err := StartGateway(ctx, StartGatewayOptions{Config: gateway.DefaultGatewayConfig(ctx), Verbosity: new(int)})
		done <- outcome{success, err}
	}()
	var result outcome
	select {
	case result = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the gateway start did not end after its listener died")
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "session API stopped unexpectedly") {
		t.Fatalf("start returned %v, %v", result.success, result.err)
	}
	if len(owned) != 1 {
		t.Fatalf("cron and housekeeping started %d times", len(owned))
	}
	if !owned[0].stop.IsSet() {
		t.Fatal("listener failure skipped background-writer shutdown")
	}
	if owned[0].cronThread.IsAlive() || owned[0].housekeeping.alive() {
		t.Fatal("a background thread outlived the failed start")
	}
}

func TestRuntimeWaitObservesListenerExit(t *testing.T) {
	for _, listenerFailure := range []bool{false, true} {
		name := "runner_stop"
		if listenerFailure {
			name = "listener_failure"
		}
		t.Run(name, func(t *testing.T) {
			h := shortGatewayHome(t)
			fakeGatewayListener(t)
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
			t.Cleanup(func() {
				_ = r.Stop(ctx, false, false, false)
				server.Stop()
			})
			if err := startGatewayRuntimeAPI(ctx, r); err != nil {
				t.Fatal(err)
			}
			if started, err := r.Start(ctx); err != nil || !started {
				t.Fatalf("runner start %v, %v", started, err)
			}
			if err := publishGatewayRuntimeReady(r); err != nil {
				t.Fatal(err)
			}
			waitCtx, cancelWait := context.WithCancel(ctx)
			defer cancelWait()
			waiter := make(chan error, 1)
			go func() { waiter <- waitGatewayRuntime(waitCtx, r) }()
			if listenerFailure {
				r.sessionAPI.server.RequestExit()
			} else if err := r.Stop(ctx, false, false, false); err != nil {
				t.Fatal(err)
			}
			var waitErr error
			select {
			case waitErr = <-waiter:
			case <-time.After(10 * time.Second):
				t.Fatal("the runtime wait did not observe the exit")
			}
			if !listenerFailure {
				if waitErr != nil {
					t.Fatalf("clean stop returned %v", waitErr)
				}
				return
			}
			if waitErr == nil || !strings.Contains(waitErr.Error(), "session API stopped unexpectedly") {
				t.Fatalf("listener failure returned %v", waitErr)
			}
			if r.SessionRuntimeDescriptor()["state"] == "ready" {
				t.Fatal("a gateway whose listener died still reads ready")
			}
			if capabilities, _ := r.SessionRuntimeDescriptor()["capabilities"].([]any); len(capabilities) != 0 {
				t.Fatalf("capabilities left after the listener died: %v", capabilities)
			}
		})
	}
}
