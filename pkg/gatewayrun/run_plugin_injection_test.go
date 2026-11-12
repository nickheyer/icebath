package gatewayrun

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Gateway loop stand in recording scheduled callbacks, refusing them when reject is set
type recordingLoop struct {
	mu     sync.Mutex
	closed bool
	reject error
	calls  int
}

func (l *recordingLoop) CallSoonThreadsafe(func()) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.reject
}

func (l *recordingLoop) CallLater(time.Duration, func()) (gateway.LoopTimer, error) {
	return nil, errors.New("not scheduled")
}

func (l *recordingLoop) IsClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *recordingLoop) IsRunning() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.closed
}

// Bare runner with an empty session store, running and not draining
func injectionRunner(t *testing.T) *GatewayRunner {
	ctx := context.Background()
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(ctx)
	r.sessionStore = gateway.NewSessionStore(ctx, filepath.Join(home.Dir(ctx), "sessions"), r.config, nil)
	t.Cleanup(r.sessionStore.CloseAllDBHandles)
	r.asyncSessionStore = gateway.NewAsyncSessionStore(r.sessionStore)
	r.running.Store(true)
	r.draining.Store(false)
	return r
}

func injectKwargs() map[string]any {
	return map[string]any{"session_key": "key", "content": "wake up", "plugin_id": "notify-plugin"}
}

func TestSchedulerRejectsStoppedOrClosedGateway(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	r := injectionRunner(t)
	loop := &recordingLoop{}
	r.gatewayLoop = loop
	r.running.Store(false)
	if accepted, err := r.schedulePluginMessageInjection(ctx, injectKwargs()); err != nil || accepted {
		t.Fatalf("stopped gateway accepted %v %v", accepted, err)
	}
	if loop.calls != 0 {
		t.Fatal("stopped gateway scheduled a callback")
	}
	r.running.Store(true)
	r.gatewayLoop = nil
	if accepted, err := r.schedulePluginMessageInjection(ctx, injectKwargs()); err != nil || accepted {
		t.Fatalf("loopless gateway accepted %v %v", accepted, err)
	}
	r.gatewayLoop = loop
	loop.closed = true
	if accepted, err := r.schedulePluginMessageInjection(ctx, injectKwargs()); err != nil || accepted {
		t.Fatalf("closed loop accepted %v %v", accepted, err)
	}
	if loop.calls != 0 {
		t.Fatal("closed loop scheduled a callback")
	}
}

func TestSchedulerRejectsSubmissionFailure(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	r := injectionRunner(t)
	r.gatewayLoop = &recordingLoop{reject: gateway.ErrLoopClosed}
	if accepted, err := r.schedulePluginMessageInjection(context.Background(), injectKwargs()); err != nil || accepted {
		t.Fatalf("refused submission accepted %v %v", accepted, err)
	}
}

func TestInstallAndClearGatewayInjectorPreservesNewerOwner(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	r := injectionRunner(t)
	manager := plugins.GetPluginManager(ctx)
	r.installPluginMessageInjector()
	if !manager.HasGatewayMessageInjector() {
		t.Fatal("install did not publish the injector")
	}
	r.clearPluginMessageInjector()
	if manager.HasGatewayMessageInjector() {
		t.Fatal("clear left the injector")
	}
	r.installPluginMessageInjector()
	newerOwner := &struct{ name string }{"newer"}
	var calls []map[string]any
	manager.SetGatewayMessageInjector(newerOwner, func(_ context.Context, kwargs map[string]any) bool {
		calls = append(calls, kwargs)
		return true
	})
	r.clearPluginMessageInjector()
	t.Cleanup(func() { plugins.ClearPublishedGatewayMessageHost(newerOwner) })
	if !manager.HasGatewayMessageInjector() {
		t.Fatal("clear dropped the newer owner's injector")
	}
	if !manager.InjectGatewayMessage(ctx, map[string]any{"value": "kept"}) {
		t.Fatal("newer injector refused")
	}
	if len(calls) != 1 || calls[0]["value"] != "kept" {
		t.Fatalf("newer injector calls %v", calls)
	}
}
