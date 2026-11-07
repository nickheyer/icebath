package tools

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
)

// ThreadCallback is one per goroutine prompt callback a worker inherits from the goroutine that handed it work
type ThreadCallback struct {
	Get func(ctx context.Context) any
	Set func(ctx context.Context, cb any)
}

var (
	threadCallbacksMu sync.RWMutex
	threadCallbacks   []ThreadCallback
)

// Adds a getter and setter pair every propagated worker installs for its lifetime and clears on exit
func RegisterThreadCallback(cb ThreadCallback) {
	threadCallbacksMu.Lock()
	defer threadCallbacksMu.Unlock()
	threadCallbacks = append(threadCallbacks, cb)
}

// Per goroutine prompt callback pairs a dispatched tool may need, the approval and sudo asker rides the Env copy
func callbackAPI() []ThreadCallback {
	threadCallbacksMu.RLock()
	defer threadCallbacksMu.RUnlock()
	return append([]ThreadCallback{}, threadCallbacks...)
}

type threadLocalsKey struct{}

type threadLocals struct {
	mu   sync.Mutex
	vals map[string]any
}

// Context with empty goroutine local storage, what a freshly started thread sees
func WithThreadLocals(ctx context.Context) context.Context {
	return context.WithValue(ctx, threadLocalsKey{}, &threadLocals{vals: map[string]any{}})
}

// Goroutine local value of name, nil when unset or the goroutine never entered local storage
func ThreadLocalGet(ctx context.Context, name string) any {
	tl, _ := ctx.Value(threadLocalsKey{}).(*threadLocals)
	if tl == nil {
		return nil
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.vals[name]
}

// Stores name in the goroutine local storage, nil deletes it, a no op outside local storage
func ThreadLocalSet(ctx context.Context, name string, v any) {
	tl, _ := ctx.Value(threadLocalsKey{}).(*threadLocals)
	if tl == nil {
		return
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if v == nil {
		delete(tl.vals, name)
		return
	}
	tl.vals[name] = v
}

type threadInstall struct {
	set func(ctx context.Context, cb any)
	cb  any
}

// Snapshots the caller's context vars and prompt callbacks now and returns a worker that runs target under them
func PropagateContextToThread(ctx context.Context, env *Env, target func(ctx context.Context, env *Env) (any, error)) func() (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot := pyctx.CopyContext(ctx)
	base := context.WithoutCancel(ctx)
	var asker Asker
	if env != nil {
		asker = env.Ask
	}
	installs, captured := captureThreadCallbacks(ctx)
	return func() (any, error) {
		workerCtx := WithThreadLocals(pyctx.With(base, snapshot.Copy()))
		var workerEnv *Env
		if env != nil {
			copyEnv := *env
			copyEnv.Ask = nil
			workerEnv = &copyEnv
		}
		if !captured {
			return target(workerCtx, workerEnv)
		}
		if workerEnv != nil {
			workerEnv.Ask = asker
		}
		installThreadCallbacks(workerCtx, installs)
		defer func() {
			if workerEnv != nil {
				workerEnv.Ask = nil
			}
			clearThreadCallbacks(workerCtx, installs)
		}()
		return target(workerCtx, workerEnv)
	}
}

// Parent callbacks paired with their setters, false when a getter panicked so the worker fails closed
func captureThreadCallbacks(ctx context.Context) (installs []threadInstall, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			toolsLogger(nil).Debug("Could not capture parent approval/sudo callbacks", "panic", r)
			installs, ok = nil, false
		}
	}()
	for _, cb := range callbackAPI() {
		installs = append(installs, threadInstall{set: cb.Set, cb: cb.Get(ctx)})
	}
	return installs, true
}

// Installs every captured non nil callback, a panicking setter leaves the rest unset so approval fails closed
func installThreadCallbacks(ctx context.Context, installs []threadInstall) {
	defer func() {
		if r := recover(); r != nil {
			toolsLogger(nil).Debug("Failed to install propagated approval/sudo callbacks; dangerous-command approval will fail closed", "panic", r)
		}
	}()
	for _, in := range installs {
		if in.cb != nil {
			in.set(ctx, in.cb)
		}
	}
}

// Clears every propagated callback slot when the worker exits
func clearThreadCallbacks(ctx context.Context, installs []threadInstall) {
	defer func() {
		if r := recover(); r != nil {
			toolsLogger(nil).Debug("Failed to clear propagated approval/sudo callbacks", "panic", r)
		}
	}()
	for _, in := range installs {
		in.set(ctx, nil)
	}
}
