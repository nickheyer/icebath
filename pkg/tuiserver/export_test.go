package tuiserver

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Registered handler of a method for the external test package
var LookupMethod = lookupMethod

// Points the launch store at db for an external test case
func UseSharedDB(t *testing.T, db *state.DB) { useSharedDB(t, db) }

// Registers a handler for one external case
func SwapMethod(t *testing.T, name string, fn Handler) { swapMethod(t, name, fn) }

// Successful response of an external case handler
var OK = ok

// Replaces the pool dispatched method set for one external case
func SwapLongHandlers(t *testing.T, names ...string) {
	longHandlersMu.Lock()
	prev := longHandlers
	longHandlers = setOf(names...)
	longHandlersMu.Unlock()
	t.Cleanup(func() {
		longHandlersMu.Lock()
		longHandlers = prev
		longHandlersMu.Unlock()
	})
}

// Installs a pool of the given size for one external case and hands back its submit
func SwapRPCPool(t *testing.T, workers int) func(fn func()) {
	prev := rpcPool
	pool := newRPCWorkerPool(workers)
	rpcPool = pool
	t.Cleanup(func() {
		pool.Shutdown(false)
		rpcPool = prev
	})
	return func(fn func()) {
		if err := pool.Submit(fn, nil); err != nil {
			t.Fatal(err)
		}
	}
}
