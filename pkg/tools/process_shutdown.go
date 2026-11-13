package tools

import (
	"errors"
	"sync/atomic"
)

var processShuttingDown atomic.Bool

// BeginProcessShutdown marks the process as exiting so schedulers and turn loops stop starting work
func BeginProcessShutdown() { processShuttingDown.Store(true) }

// ProcessShuttingDown reports that the exit path has begun or that err is a pool refusing work after shutdown
func ProcessShuttingDown(err error) bool {
	return processShuttingDown.Load() || errors.Is(err, ErrPoolShutdown)
}
