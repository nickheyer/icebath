package sqliteutil

import (
	"log/slog"
	"runtime"
	"sync"
)

// Go callbacks of one kind keyed by the database handle SQLite hands back to the trampoline as user data
type registry[F any] struct {
	m sync.Map
}

// One installed callback with the cleanup that drops it once its driver connection is garbage collected
type hook[F any] struct {
	fn      F
	db      uintptr
	cleanup runtime.Cleanup
}

// Callback installed for the database handle
func (r *registry[F]) get(db uintptr) (F, bool) {
	if v, ok := r.m.Load(db); ok {
		return v.(*hook[F]).fn, true
	}
	var zero F
	return zero, false
}

// Installs fn for the connection, replacing what it had; a connection closed without removing it frees the entry when it is collected
func (r *registry[F]) set(h handle, fn F) {
	e := &hook[F]{fn: fn, db: h.db}
	e.cleanup = runtime.AddCleanup((*byte)(h.owner), r.forget, e)
	if old, loaded := r.m.Swap(h.db, e); loaded {
		old.(*hook[F]).cleanup.Stop()
	}
}

// Removes the callback of the connection
func (r *registry[F]) remove(h handle) {
	if old, loaded := r.m.LoadAndDelete(h.db); loaded {
		old.(*hook[F]).cleanup.Stop()
	}
	runtime.KeepAlive(h.owner)
}

// Drops an entry whose connection was collected, unless a newer connection at the same handle address replaced it
func (r *registry[F]) forget(e *hook[F]) { r.m.CompareAndDelete(e.db, e) }

// Logs a panic a callback raised inside SQLite, where unwinding would leave the connection's state broken
func logPanic(what string, value any) {
	slog.Error("sqlite "+what+" panicked", "panic", value)
}
