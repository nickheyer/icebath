package tools

import (
	"errors"
	"strings"
	"sync/atomic"
)

// Shared shutdown predicate for background goroutines that can outlive process teardown

var processShuttingDown atomic.Bool

// BeginInterpreterShutdown marks the process as finalizing so background schedulers stop producing noise
func BeginInterpreterShutdown() { processShuttingDown.Store(true) }

// InterpreterShuttingDown reports finalization, treating a pool scheduling error as the shutdown signal
func InterpreterShuttingDown(err error) bool {
	if processShuttingDown.Load() {
		return true
	}
	if err == nil {
		return false
	}
	return errors.Is(err, ErrPoolShutdown) || strings.Contains(strings.ToLower(err.Error()), "cannot schedule new futures")
}
