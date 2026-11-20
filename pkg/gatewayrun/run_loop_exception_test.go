package gatewayrun

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

type TimedOut struct{ msg string }

func (e *TimedOut) Error() string { return e.msg }

type NetworkError struct{ msg string }

func (e *NetworkError) Error() string { return e.msg }

type ConnectError struct{ msg string }

func (e *ConnectError) Error() string { return e.msg }

type ReadTimeout struct{ msg string }

func (e *ReadTimeout) Error() string { return e.msg }

type PoolTimeout struct{ msg string }

func (e *PoolTimeout) Error() string { return e.msg }

type ClientConnectorError struct{ msg string }

func (e *ClientConnectorError) Error() string { return e.msg }

type SomeUnrelatedBug struct{ msg string }

func (e *SomeUnrelatedBug) Error() string { return e.msg }

func TestTransientClassifierMatchesKnownNetworkErrors(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, exc := range []error{&TimedOut{"boom"}, &NetworkError{"boom"}, &ConnectError{"boom"}, &ReadTimeout{"boom"},
		&PoolTimeout{"boom"}, &ClientConnectorError{"boom"}} {
		if !isTransientNetworkError(exc) {
			t.Fatalf("%T not classified transient", exc)
		}
	}
}

func TestHandlerDelegatesUnknownErrorsToDefault(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	loop := gateway.NewCallbackLoop()
	gatewayLoopExceptionHandler(loop, gateway.LoopExceptionContext{Message: "Something else broke", Exception: &SomeUnrelatedBug{"real bug"}})
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "Something else broke: real bug") {
		t.Fatalf("default handler not reached: %q", out)
	}
	if strings.Contains(out, "swallowed") {
		t.Fatalf("unknown error swallowed: %q", out)
	}
}
