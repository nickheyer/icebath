package tools

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
)

// Per worker interrupt signaling so stopping one session never kills another session's tools

// WorkerID identifies one tool execution worker the way a thread ident does in hermes
type WorkerID int64

var (
	interruptMu        sync.Mutex
	interruptedWorkers = map[WorkerID]bool{}
	interruptReasons   = map[WorkerID]string{}
	yieldWorkers       = map[WorkerID]bool{}
	nextWorkerID       atomic.Int64
	debugInterrupt     = truthyEnvValue(os.Getenv("ICEBATH_DEBUG_INTERRUPT"))
)

type workerIDKey struct{}

type actingForKey struct{}

// NewWorkerID allocates a fresh worker identity
func NewWorkerID() WorkerID {
	return WorkerID(nextWorkerID.Add(1))
}

// WithWorker binds the executing worker identity to the context
func WithWorker(ctx context.Context, id WorkerID) context.Context {
	return context.WithValue(ctx, workerIDKey{}, id)
}

// WorkerOf returns the context's worker identity or zero
func WorkerOf(ctx context.Context) WorkerID {
	if ctx == nil {
		return 0
	}
	id, _ := ctx.Value(workerIDKey{}).(WorkerID)
	return id
}

// WithActingFor marks a deadline worker as acting for the tool worker that spawned it
func WithActingFor(ctx context.Context, id WorkerID) context.Context {
	return context.WithValue(ctx, actingForKey{}, id)
}

func actingForOf(ctx context.Context) WorkerID {
	if ctx == nil {
		return 0
	}
	id, _ := ctx.Value(actingForKey{}).(WorkerID)
	return id
}

// SetInterrupt sets or clears the interrupt for a worker with an optional user safe reason
func SetInterrupt(id WorkerID, active bool, reason string) {
	interruptMu.Lock()
	if active {
		interruptedWorkers[id] = true
		if reason != "" {
			interruptReasons[id] = reason
		} else {
			delete(interruptReasons, id)
		}
	} else {
		delete(interruptedWorkers, id)
		delete(interruptReasons, id)
		delete(yieldWorkers, id)
	}
	var snapshot []WorkerID
	if debugInterrupt {
		for w := range interruptedWorkers {
			snapshot = append(snapshot, w)
		}
	}
	interruptMu.Unlock()
	if debugInterrupt {
		slog.Info("[interrupt-debug] set_interrupt", "active", active, "target", id, "current_set", snapshot)
	}
}

// IsInterrupted reports whether the context's worker or the worker it acts for was interrupted
func IsInterrupted(ctx context.Context) bool {
	return IsWorkerInterrupted(WorkerOf(ctx)) || IsWorkerInterrupted(actingForOf(ctx))
}

// IsWorkerInterrupted reports whether a worker has its interrupt bit set, zero never does
func IsWorkerInterrupted(id WorkerID) bool {
	if id == 0 {
		return false
	}
	interruptMu.Lock()
	defer interruptMu.Unlock()
	return interruptedWorkers[id]
}

// RequestYield asks the worker to hand its foreground command to the background instead of killing it
func RequestYield(id WorkerID) {
	interruptMu.Lock()
	yieldWorkers[id] = true
	interruptMu.Unlock()
}

// IsWorkerYieldRequested reports whether a yield is pending for the worker
func IsWorkerYieldRequested(id WorkerID) bool {
	if id == 0 {
		return false
	}
	interruptMu.Lock()
	defer interruptMu.Unlock()
	return yieldWorkers[id]
}

// ConsumeYield atomically takes the pending yield for the worker
func ConsumeYield(id WorkerID) bool {
	if id == 0 {
		return false
	}
	interruptMu.Lock()
	defer interruptMu.Unlock()
	if yieldWorkers[id] {
		delete(yieldWorkers, id)
		return true
	}
	return false
}

// RunIfNotInterrupted runs a state transition atomically against the context worker's interruption
func RunIfNotInterrupted(ctx context.Context, callback func()) bool {
	id := WorkerOf(ctx)
	interruptMu.Lock()
	defer interruptMu.Unlock()
	if interruptedWorkers[id] {
		return false
	}
	callback()
	return true
}

// InterruptReason returns the user safe interrupt cause for the context worker
func InterruptReason(ctx context.Context) string {
	interruptMu.Lock()
	defer interruptMu.Unlock()
	return interruptReasons[WorkerOf(ctx)]
}

// ClearCurrentInterrupt clears the interrupt bit on the context worker right before an approved spawn
func ClearCurrentInterrupt(ctx context.Context) {
	SetInterrupt(WorkerOf(ctx), false, "")
}
