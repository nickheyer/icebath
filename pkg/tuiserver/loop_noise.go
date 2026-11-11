package tuiserver

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"syscall"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Python classes of the teardown errors that mean the peer hung up mid write
var benignTeardownErrors = map[string]bool{"ConnectionResetError": true, "ConnectionAbortedError": true, "BrokenPipeError": true}

// Loops that already carry the teardown filter
var loopNoiseFilterInstalled sync.Map

func loopNoiseLog() *slog.Logger { return slog.Default().With("logger", "tui_gateway.loop_noise") }

// Python class of a loop failure, the OSError subclass of its errno when it carries one
func loopExceptionClass(err error) string {
	var errno syscall.Errno
	if name := pyrepr.ExceptionName(err); benignTeardownErrors[name] || !errors.As(err, &errno) {
		return name
	}
	return pypath.OSErrorClass(err)
}

// Python str of a loop failure, the bracketed errno text for an OS error
func loopExceptionText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return pypath.OSErrorText(err)
	}
	return pyrepr.ExceptionMessage(err)
}

// True for a peer hangup raised by the connection lost callback of a transport being torn down
func isBenignTeardown(c gateway.LoopExceptionContext) bool {
	if c.Exception == nil || !benignTeardownErrors[loopExceptionClass(c.Exception)] {
		return false
	}
	const marker = "_call_connection_lost"
	return strings.Contains(c.Callback, marker) || strings.Contains(c.Handle, marker)
}

// Chains the teardown filter ahead of the loop's handler once, peer hangups becoming one debug line
func InstallLoopNoiseFilter(loop *gateway.CallbackLoop) {
	if _, installed := loopNoiseFilterInstalled.LoadOrStore(loop, true); installed {
		return
	}
	previous := loop.ExceptionHandler()
	loop.SetExceptionHandler(func(l *gateway.CallbackLoop, c gateway.LoopExceptionContext) {
		if isBenignTeardown(c) {
			loopNoiseLog().Debug(fmt.Sprintf("ws peer hangup during teardown (suppressed): %s", loopExceptionText(c.Exception)))
			return
		}
		if previous != nil {
			previous(l, c)
			return
		}
		l.DefaultExceptionHandler(c)
	})
}
