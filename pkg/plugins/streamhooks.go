package plugins

import (
	"context"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

var streamHookQueueSize = 1024

// One queued observer delivery carrying the enqueuing turn's context
type streamHookItem struct {
	ctx     context.Context
	payload map[string]any
	stop    bool
}

// Bounded queue and worker goroutine serving one observer callback
type consumerDispatcher struct {
	hookName string
	entry    *hookEntry
	events   chan streamHookItem
	done     chan struct{}
}

type streamDispatcherKey struct {
	hook  string
	entry *hookEntry
}

var (
	streamDispatcherMu sync.Mutex
	streamDispatchers  = map[streamDispatcherKey]*consumerDispatcher{}
)

// Queues an item, evicting the oldest pending one once when the queue is full
func putDropOldest(events chan streamHookItem, item streamHookItem) bool {
	select {
	case events <- item:
		return true
	default:
	}
	select {
	case <-events:
	default:
	}
	select {
	case events <- item:
		return true
	default:
		return false
	}
}

// Runs queued observer deliveries until stopped, reporting callback failures once each
func streamHookWorker(d *consumerDispatcher) {
	defer close(d.done)
	for item := range d.events {
		if item.stop {
			return
		}
		payload := copyKwargs(item.payload)
		if _, ok := payload["telemetry_schema_version"]; !ok {
			payload["telemetry_schema_version"] = ObserverSchemaVersion
		}
		if _, err := invokeHookCallback(item.ctx, d.entry, payload); err != nil {
			GetPluginManager(item.ctx).reportHookFailure(d.hookName, d.entry.name, payload, err, "Hook")
		}
	}
}

// Callback entries registered for a hook on the active profile's manager
func registeredStreamCallbacks(ctx context.Context, hookName string) []*hookEntry {
	m := GetPluginManager(ctx)
	if len(m.IterHookCallbacks(hookName)) == 0 {
		return nil
	}
	return m.hookCallbacks(hookName)
}

// Sends the stop marker and waits up to the timeout for the worker to exit
func stopStreamDispatcher(d *consumerDispatcher, timeout time.Duration) {
	putDropOldest(d.events, streamHookItem{stop: true})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-d.done:
	case <-timer.C:
	}
}

// Starts a dispatcher goroutine for one hook callback
func startStreamDispatcher(hookName string, entry *hookEntry) *consumerDispatcher {
	d := &consumerDispatcher{hookName: hookName, entry: entry, events: make(chan streamHookItem, streamHookQueueSize), done: make(chan struct{})}
	go streamHookWorker(d)
	return d
}

// Live dispatcher per registered callback, restarting exited workers and stopping stale ones
func streamDispatchersFor(ctx context.Context, hookName string) []*consumerDispatcher {
	callbacks := registeredStreamCallbacks(ctx, hookName)
	if len(callbacks) == 0 {
		return nil
	}
	live := map[*hookEntry]bool{}
	for _, cb := range callbacks {
		live[cb] = true
	}
	var ready, stale []*consumerDispatcher
	streamDispatcherMu.Lock()
	for key, d := range streamDispatchers {
		if key.hook == hookName && !live[key.entry] {
			stale = append(stale, d)
			delete(streamDispatchers, key)
		}
	}
	for _, cb := range callbacks {
		key := streamDispatcherKey{hook: hookName, entry: cb}
		d := streamDispatchers[key]
		exited := false
		if d != nil {
			select {
			case <-d.done:
				exited = true
			default:
			}
		}
		if d == nil || exited {
			d = startStreamDispatcher(hookName, cb)
			streamDispatchers[key] = d
		}
		ready = append(ready, d)
	}
	streamDispatcherMu.Unlock()
	for _, d := range stale {
		stopStreamDispatcher(d, 200*time.Millisecond)
	}
	return ready
}

// Queues an observer hook for each consumer without running plugin code inline, true when any accepted it
func EnqueuePluginStreamHook(ctx context.Context, hookName string, payload map[string]any) bool {
	queued := false
	item := streamHookItem{ctx: context.WithoutCancel(ctx), payload: copyKwargs(payload)}
	for _, d := range streamDispatchersFor(ctx, hookName) {
		if putDropOldest(d.events, item) {
			queued = true
		} else {
			logDebug("plugin stream hook queue full after drop-oldest: " + hookName + " callback=" + d.entry.name)
		}
	}
	return queued
}

// Reports whether any stream start, delta or end observer is registered
func HasStreamObserverHooks(ctx context.Context) bool {
	for _, name := range []string{"on_stream_start", "on_stream_delta", "on_stream_end"} {
		if len(registeredStreamCallbacks(ctx, name)) > 0 {
			return true
		}
	}
	return false
}

// Reports whether reasoning deltas are opted in and a delta observer is registered
func HasReasoningStreamObserverHooks(ctx context.Context) bool {
	return StreamReasoningDeltasEnabled(ctx) && len(registeredStreamCallbacks(ctx, "on_stream_delta")) > 0
}

// Reports whether plugins.stream_reasoning_deltas opts plugins into reasoning deltas
func StreamReasoningDeltasEnabled(ctx context.Context) bool {
	cfg, err := loadConfig(GetPluginManager(ctx).home)
	if err != nil {
		logDebug("failed to read plugins.stream_reasoning_deltas: " + err.Error())
		return false
	}
	section, _ := cfg["plugins"].(map[string]any)
	return config.PyTruthy(section["stream_reasoning_deltas"])
}

// Stops every stream hook dispatcher, waiting up to the timeout for each
func ShutdownPluginStreamHookDispatcher(timeout time.Duration) {
	streamDispatcherMu.Lock()
	dispatchers := make([]*consumerDispatcher, 0, len(streamDispatchers))
	for _, d := range streamDispatchers {
		dispatchers = append(dispatchers, d)
	}
	streamDispatchers = map[streamDispatcherKey]*consumerDispatcher{}
	streamDispatcherMu.Unlock()
	for _, d := range dispatchers {
		stopStreamDispatcher(d, timeout)
	}
}
