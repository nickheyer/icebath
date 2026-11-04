package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
)

func TestDeviceFlowErrorCopy(t *testing.T) {
	err := DeviceFlowError("expired_token", "code expired", "icebath portal")
	want := "The sign-in code expired before it was approved in the browser. Run `icebath portal` to get a new code.\n  Details: expired_token: code expired"
	if err.Message != want || err.OAuthErrorCode != "expired_token" {
		t.Fatalf("known code %q", err.Message)
	}
	if !IsDeviceCodeExpired(err) {
		t.Fatal("expired_token copy must read as a device code expiry")
	}
	err = DeviceFlowError("weird_code", "server said no", "icebath auth add xai-oauth")
	if !strings.HasPrefix(err.Message, "Sign-in did not complete: server said no. Run `icebath auth add xai-oauth` to try again.\n  Details: weird_code: server said no") {
		t.Fatalf("unknown code %q", err.Message)
	}
	err = DeviceFlowError("", "", "")
	if err.Message != "Sign-in did not complete: the server rejected the request. Run `icebath portal` to try again." {
		t.Fatalf("empty code %q", err.Message)
	}
	if DeviceFlowError("invalid_client", "nope", "icebath portal").Message[:len("The server did not recognize this copy of Icebath.")] != "The server did not recognize this copy of Icebath." {
		t.Fatal("invalid_client copy")
	}
}

func TestErrorClassifiers(t *testing.T) {
	if !IsNetworkError(&url.Error{Op: "Post", URL: "https://p", Err: errors.New("dial tcp: connection refused")}) {
		t.Fatal("url errors are network errors")
	}
	if !IsNetworkError(&net.DNSError{Err: "no such host", Name: "p"}) || !IsNetworkError(context.DeadlineExceeded) {
		t.Fatal("dns and deadline errors are network errors")
	}
	if IsNetworkError(&SignInCopyError{Message: "x"}) || IsNetworkError(&DeviceCodeExpiredError{Message: "x"}) || IsNetworkError(nousErr("x", "y", true)) || IsNetworkError(errors.New("bug")) {
		t.Fatal("copy, expiry, auth, and plain errors are not network errors")
	}
	if !IsDeviceCodeExpired(&DeviceCodeExpiredError{Message: "x"}) || !IsDeviceCodeExpired(xaiErr("x", "timeout", false)) || !IsDeviceCodeExpired(nousErr("x", DeviceCodeExpiredCode, false)) {
		t.Fatal("device code expiry shapes")
	}
	if IsDeviceCodeExpired(nousErr("x", "access_denied", false)) || IsDeviceCodeExpired(nil) {
		t.Fatal("other codes are not expiry")
	}
	if !IsCancelled(context.Canceled) || !IsCancelled(io.EOF) || !IsCancelled(&SignInCancelledError{ExitCode: 130, HasCode: true}) || !IsCancelled(&SignInCancelledError{}) || !IsCancelled(&DeviceFlowCancelledError{}) {
		t.Fatal("cancellation shapes")
	}
	if IsCancelled(&SignInCancelledError{ExitCode: 1, HasCode: true}) || IsCancelled(errors.New("x")) || IsCancelled(nil) {
		t.Fatal("non cancellations")
	}
}

func TestSignInFailureLines(t *testing.T) {
	lines := SignInFailureLines(context.Canceled, "", "")
	if len(lines) != 1 || lines[0] != "Sign-in was cancelled. Run `icebath portal` when you want to try again." {
		t.Fatalf("cancelled %v", lines)
	}
	lines = SignInFailureLines(&url.Error{Op: "Post", URL: "https://p", Err: errors.New("connection refused")}, "portal.nousresearch.com", "icebath portal")
	if len(lines) != 2 || lines[0] != "Could not sign in: Icebath could not reach portal.nousresearch.com. Check your internet connection or proxy, then run `icebath portal` again." || !strings.HasPrefix(lines[1], "  Details: Post \"https://p\": connection refused") {
		t.Fatalf("network %v", lines)
	}
	lines = SignInFailureLines(errors.New("boom"), "", "icebath auth add xai-oauth")
	if len(lines) != 2 || lines[0] != "Could not sign in. Run `icebath auth add xai-oauth` to try again, or `icebath model` to pick a different provider." || lines[1] != "  Details: boom" {
		t.Fatalf("other %v", lines)
	}
	lines = SignInFailureLines(DeviceFlowError("access_denied", "declined", "icebath portal"), "", "")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "Sign-in was declined in the browser.") || lines[1] != "  Details: access_denied: declined" {
		t.Fatalf("copy %v", lines)
	}
}

func TestProviderSetupFailureLines(t *testing.T) {
	nothingSaved := "Your provider settings were not changed. Continue the wizard now and run `icebath model` afterwards to try again."
	lines := ProviderSetupFailureLines(DeviceFlowError("access_denied", "declined", "icebath portal"), "")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "Could not finish connecting a provider: sign-in was declined in the browser.") || lines[1] != nothingSaved || lines[2] != "  Details: access_denied: declined" {
		t.Fatalf("copy %v", lines)
	}
	lines = ProviderSetupFailureLines(context.Canceled, "")
	if len(lines) != 2 || lines[0] != "Could not finish connecting a provider (sign-in was cancelled)." {
		t.Fatalf("cancelled %v", lines)
	}
	lines = ProviderSetupFailureLines(&net.DNSError{Err: "no such host", Name: "p"}, "icebath setup")
	if len(lines) != 3 || lines[0] != "Could not finish connecting a provider (no internet connection, or the provider could not be reached)." || !strings.Contains(lines[1], "`icebath setup`") {
		t.Fatalf("network %v", lines)
	}
	lines = ProviderSetupFailureLines(errors.New("boom"), "")
	if lines[0] != "Could not finish connecting a provider (something went wrong while talking to the provider)." || lines[2] != "  Details: boom" {
		t.Fatalf("other %v", lines)
	}
}
