package tuiserver

import (
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Repr of the connection lost handle asyncio floods the log with
const fakeConnectionLostCallback = "<Handle _ProactorBasePipeTransport._call_connection_lost(None)>"

func TestBenignTeardownMatchesResetInConnectionLost(t *testing.T) {
	c := gateway.LoopExceptionContext{Exception: syscall.ECONNRESET, Handle: fakeConnectionLostCallback}
	if !isBenignTeardown(c) {
		t.Fatal("a reset in the connection lost handle must be benign")
	}
}

func TestBenignTeardownMatchesAbortedAndBrokenPipe(t *testing.T) {
	for _, exc := range []error{syscall.ECONNABORTED, &pyrepr.FormatError{Class: "BrokenPipeError", Msg: "epipe"}} {
		c := gateway.LoopExceptionContext{Exception: exc, Callback: fakeConnectionLostCallback}
		if !isBenignTeardown(c) {
			t.Fatalf("%v in the connection lost callback must be benign", exc)
		}
	}
}

func TestResetOutsideConnectionLostIsNotSuppressed(t *testing.T) {
	c := gateway.LoopExceptionContext{Exception: &pyrepr.FormatError{Class: "ConnectionResetError", Msg: "reset in a real handler"},
		Handle: "<Handle some_other_handler()>"}
	if isBenignTeardown(c) {
		t.Fatal("a reset outside the teardown path must reach the default handler")
	}
}

func TestUnrelatedExceptionIsNotSuppressed(t *testing.T) {
	c := gateway.LoopExceptionContext{Exception: &pyrepr.FormatError{Class: "ValueError", Msg: "boom"}, Handle: fakeConnectionLostCallback}
	if isBenignTeardown(c) {
		t.Fatal("an unrelated exception must not be suppressed")
	}
}

func TestNoExceptionIsNotSuppressed(t *testing.T) {
	if isBenignTeardown(gateway.LoopExceptionContext{Message: "loop warning, no exc"}) {
		t.Fatal("a context with no exception must not be suppressed")
	}
}

func TestInstallSuppressesFloodAndForwardsRealErrors(t *testing.T) {
	loop := gateway.NewCallbackLoop()
	defer loop.Close()
	defer loop.SetExceptionHandler(nil)
	var forwarded []gateway.LoopExceptionContext
	loop.SetExceptionHandler(func(_ *gateway.CallbackLoop, c gateway.LoopExceptionContext) { forwarded = append(forwarded, c) })

	InstallLoopNoiseFilter(loop)
	InstallLoopNoiseFilter(loop)

	loop.CallExceptionHandler(gateway.LoopExceptionContext{Exception: syscall.ECONNRESET, Handle: fakeConnectionLostCallback})
	if len(forwarded) != 0 {
		t.Fatalf("benign teardown flood was forwarded: %v", forwarded)
	}

	realCtx := gateway.LoopExceptionContext{Exception: &pyrepr.FormatError{Class: "RuntimeError", Msg: "genuine loop bug"}}
	loop.CallExceptionHandler(realCtx)
	if len(forwarded) != 1 || forwarded[0] != realCtx {
		t.Fatalf("genuine loop error not forwarded unchanged once: %v", forwarded)
	}
}
