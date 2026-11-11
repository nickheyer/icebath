package tuiserver

import (
	"sort"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var (
	atExitMu    sync.Mutex
	atExitFuncs []func()
)

// Queues fn for process exit, the atexit.register of the tui gateway modules
func registerAtExit(fn func()) {
	atExitMu.Lock()
	atExitFuncs = append(atExitFuncs, fn)
	atExitMu.Unlock()
}

// Runs and clears the queued exit functions newest first, a panicking one not stopping the rest
func AtExit() {
	atExitMu.Lock()
	funcs := atExitFuncs
	atExitFuncs = nil
	atExitMu.Unlock()
	for i := len(funcs) - 1; i >= 0; i-- {
		func() {
			defer func() { _ = recover() }()
			funcs[i]()
		}()
	}
}

// Keys of a map in sorted order
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Text of a recovered failure, the str of the Python exception
func failureText(failure any) string {
	if err, isError := failure.(error); isError {
		return err.Error()
	}
	return pyrepr.Str(failure)
}
