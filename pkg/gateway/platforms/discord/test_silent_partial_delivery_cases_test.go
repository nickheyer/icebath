package discord

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
)

func TestDiscordTransportClassificationConnectionErrorIsTransport(t *testing.T) {
	if !isDiscordTransportError(&net.OpError{Op: "write", Net: "tcp", Err: errors.New("websocket closed")}) {
		t.Fatal("connection error not transport")
	}
	if !isDiscordTransportError(errors.New("Session is closed")) {
		t.Fatal("closed session not transport")
	}
	if !isDiscordTransportError(syscall.ECONNRESET) {
		t.Fatal("connection reset not transport")
	}
}

func TestDiscordTransportClassificationHttpAndTimeoutErrorsAreNotTransport(t *testing.T) {
	if isDiscordTransportError(errors.New("error code: 50013: Missing Permissions")) {
		t.Fatal("permission error treated as transport")
	}
	if isDiscordTransportError(errDiscordReadyTimeout) {
		t.Fatal("timeout treated as transport")
	}
	if isDiscordTransportError(&net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}) {
		t.Fatal("net timeout treated as transport")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestDiscordTransportClassificationSendWithoutClientReportsSendPathDegraded(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newTestAdapter(t, nil)
	result, _ := a.Send(context.Background(), "c1", "hello", "", nil)
	if result.Success || result.Error != "send_path_degraded" || !result.Retryable {
		t.Fatalf("result %+v", result)
	}
}
