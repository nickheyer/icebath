package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Active profile context whose manager is reset and whose stream dispatchers stop at cleanup
func streamProfile(t *testing.T, cfg string) (context.Context, *Manager) {
	t.Helper()
	h := t.TempDir()
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ResetPluginManagersForTests()
	t.Cleanup(func() {
		ShutdownPluginStreamHookDispatcher(time.Second)
		ResetPluginManagersForTests()
	})
	ctx := homeCtx(h)
	return ctx, GetPluginManager(ctx)
}

// Receives one payload or fails after two seconds
func recv(t *testing.T, ch chan map[string]any) map[string]any {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("observer never ran")
	}
	return nil
}

func TestStreamDeltaPluginHookIsQueuedOffTokenPath(t *testing.T) {
	ctx, m := streamProfile(t, "")
	release, got := make(chan struct{}), make(chan map[string]any, 4)
	addHook(m, "on_stream_delta", func(_ context.Context, p map[string]any) (any, error) {
		<-release
		got <- p
		return nil, nil
	})
	t0 := time.Now()
	if !EnqueuePluginStreamHook(ctx, "on_stream_delta", map[string]any{"text": "hi"}) || time.Since(t0) > time.Second {
		t.Fatal("enqueue blocked or refused")
	}
	close(release)
	p := recv(t, got)
	if p["text"] != "hi" || p["telemetry_schema_version"] != ObserverSchemaVersion {
		t.Fatalf("payload %v", p)
	}
}

func TestStreamDeltaPluginHookErrorDoesNotBreakStreamingAndWarnsOnce(t *testing.T) {
	ctx, m := streamProfile(t, "")
	logs := captureLogs(t)
	calls := make(chan map[string]any, 8)
	addHook(m, "on_stream_delta", func(_ context.Context, p map[string]any) (any, error) {
		calls <- p
		return nil, &runtimeError{"observer broke"}
	})
	for range 3 {
		if !EnqueuePluginStreamHook(ctx, "on_stream_delta", map[string]any{"text": "x"}) {
			t.Fatal("enqueue refused")
		}
	}
	for range 3 {
		recv(t, calls)
	}
	ShutdownPluginStreamHookDispatcher(time.Second)
	if strings.Count(logs.String(), "level=WARN") != 1 || strings.Count(logs.String(), "raised again") != 2 {
		t.Fatalf("logs %s", logs.String())
	}
}

func TestStreamHookQueueDropsOldestPendingEventWhenFull(t *testing.T) {
	saved := streamHookQueueSize
	streamHookQueueSize = 2
	t.Cleanup(func() { streamHookQueueSize = saved })
	ctx, m := streamProfile(t, "")
	release, started, got := make(chan struct{}), make(chan struct{}, 1), make(chan map[string]any, 8)
	addHook(m, "on_stream_delta", func(_ context.Context, p map[string]any) (any, error) {
		if p["n"] == 0 {
			started <- struct{}{}
			<-release
		}
		got <- p
		return nil, nil
	})
	EnqueuePluginStreamHook(ctx, "on_stream_delta", map[string]any{"n": 0})
	<-started
	for n := 1; n <= 3; n++ {
		if !EnqueuePluginStreamHook(ctx, "on_stream_delta", map[string]any{"n": n}) {
			t.Fatal("enqueue refused")
		}
	}
	close(release)
	var seen []any
	for range 3 {
		seen = append(seen, recv(t, got)["n"])
	}
	if seen[0] != 0 || seen[1] != 2 || seen[2] != 3 {
		t.Fatalf("seen %v", seen)
	}
}

func TestStreamHookQueueIsolatedPerConsumer(t *testing.T) {
	ctx, m := streamProfile(t, "")
	release, fast := make(chan struct{}), make(chan map[string]any, 4)
	defer close(release)
	addHook(m, "on_stream_delta", func(context.Context, map[string]any) (any, error) { <-release; return nil, nil })
	addHook(m, "on_stream_delta", func(_ context.Context, p map[string]any) (any, error) { fast <- p; return nil, nil })
	EnqueuePluginStreamHook(ctx, "on_stream_delta", map[string]any{"text": "a"})
	EnqueuePluginStreamHook(ctx, "on_stream_delta", map[string]any{"text": "b"})
	if recv(t, fast)["text"] != "a" || recv(t, fast)["text"] != "b" {
		t.Fatal("fast consumer starved")
	}
}

func TestReasoningStreamDeltaPluginHookIsOptIn(t *testing.T) {
	ctx, m := streamProfile(t, "")
	addHook(m, "on_stream_delta", func(context.Context, map[string]any) (any, error) { return nil, nil })
	if StreamReasoningDeltasEnabled(ctx) || HasReasoningStreamObserverHooks(ctx) {
		t.Fatal("reasoning deltas on without opt in")
	}
	ctx2, m2 := streamProfile(t, "plugins:\n  stream_reasoning_deltas: true\n")
	if !StreamReasoningDeltasEnabled(ctx2) || HasReasoningStreamObserverHooks(ctx2) {
		t.Fatal("opt in without observer")
	}
	addHook(m2, "on_stream_delta", func(context.Context, map[string]any) (any, error) { return nil, nil })
	if !HasReasoningStreamObserverHooks(ctx2) {
		t.Fatal("opted in observer not reported")
	}
}

func TestInterimAndLifecycleHooksQueuedAndConsumerCount(t *testing.T) {
	ctx, m := streamProfile(t, "")
	var mu sync.Mutex
	got := make(chan map[string]any, 8)
	addHook(m, "on_interim_message", func(_ context.Context, p map[string]any) (any, error) { got <- p; return nil, nil })
	if HasStreamObserverHooks(ctx) {
		t.Fatal("interim observer counted as stream consumer")
	}
	if !EnqueuePluginStreamHook(ctx, "on_interim_message", map[string]any{"text": "thinking"}) || recv(t, got)["text"] != "thinking" {
		t.Fatal("interim not delivered")
	}
	for _, name := range []string{"on_stream_start", "on_stream_end"} {
		addHook(m, name, func(_ context.Context, p map[string]any) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			got <- p
			return nil, nil
		})
		if !HasStreamObserverHooks(ctx) {
			t.Fatalf("%s not counted", name)
		}
		if !EnqueuePluginStreamHook(ctx, name, map[string]any{"hook": name}) || recv(t, got)["hook"] != name {
			t.Fatalf("%s not delivered", name)
		}
	}
	if EnqueuePluginStreamHook(ctx, "on_stream_delta", map[string]any{}) {
		t.Fatal("queued with no consumer")
	}
}
