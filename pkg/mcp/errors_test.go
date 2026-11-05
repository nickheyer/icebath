package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// taskGroup mimics an anyio TaskGroup wrapper whose own text says nothing
type taskGroup struct{ errs []error }

func (g *taskGroup) Error() string   { return "unhandled errors in a TaskGroup (1 sub-exception)" }
func (g *taskGroup) Unwrap() []error { return g.errs }

func group(errs ...error) error { return &taskGroup{errs: errs} }

// SystemExit mirrors the process exit error a group unwrap must re-raise
type SystemExit struct{ Code int }

func (e *SystemExit) Error() string { return fmt.Sprint(e.Code) }

// chainErr is one link of a hand built cause chain that may loop
type chainErr struct {
	msg  string
	next error
}

func (e *chainErr) Error() string { return e.msg }
func (e *chainErr) Unwrap() error { return e.next }

// causeAndContext carries both an explicit cause and the error it was raised while handling
type causeAndContext struct {
	msg            string
	cause, context error
}

func (e *causeAndContext) Error() string   { return e.msg }
func (e *causeAndContext) Unwrap() []error { return []error{e.cause, e.context} }

type silentErr struct{}

func (silentErr) Error() string { return "" }

func TestErrorsUnwrapExceptionGroup(t *testing.T) {
	plain := errors.New("boom")
	if unwrapExceptionGroup(plain) != plain {
		t.Fatal("plain error must pass through")
	}
	if got := unwrapExceptionGroup(group(syscall.EPIPE)); got != syscall.EPIPE {
		t.Fatalf("single level group root %v", got)
	}
	real := errors.New("real failure")
	if got := unwrapExceptionGroup(group(context.Canceled, group(context.Canceled), real)); got != real {
		t.Fatalf("cancellation noise must be skipped, got %v", got)
	}
	if got := unwrapExceptionGroup(group(context.Canceled)); got != context.Canceled {
		t.Fatalf("an all cancellation group keeps its first member, got %v", got)
	}
	exit := &SystemExit{Code: 2}
	func() {
		defer func() {
			if r := recover(); r != exit {
				t.Fatalf("SystemExit must be re-raised, recovered %v", r)
			}
		}()
		unwrapExceptionGroup(group(errors.New("noise"), group(exit)))
	}()
	if !containsOnlyCancellation(group(context.Canceled, group(fmt.Errorf("wrapped: %w", context.Canceled)))) {
		t.Fatal("nested cancellations only")
	}
	if containsOnlyCancellation(group(context.Canceled, real)) {
		t.Fatal("a real member is not cancellation only")
	}
}

func TestFailureClassification(t *testing.T) {
	transient := []error{
		errors.New("connection refused"), syscall.ECONNRESET, syscall.EPIPE, io.EOF, context.DeadlineExceeded,
		&os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}, errors.New("something odd"), net.ErrClosed,
		&HTTPStatusError{Status: 500, Method: "POST", URL: "https://x/mcp"},
	}
	for _, err := range transient {
		if got := classifyMCPFailure(err); got != "transient" {
			t.Errorf("%v classified %s", err, got)
		}
	}
	permanent := []error{
		group(group(&exec.Error{Name: "cmd", Err: exec.ErrNotFound})),
		&fs.PathError{Op: "fork/exec", Path: "/nope", Err: syscall.ENOENT},
		syscall.ENOENT,
		&NonMCPEndpointError{Msg: "html"}, &InvalidMCPURLError{Msg: "bad"}, &NodeABIMismatchError{Msg: "abi"},
		&HTTPStatusError{Status: 401}, &HTTPStatusError{Status: 403}, &OAuthFlowError{Msg: "expired"},
		&OAuthNonInteractiveError{Msg: "needs login"}, fmt.Errorf("connect: %w", &OAuthRegistrationError{Msg: "dcr"}),
		fmt.Errorf("stdio: %w", errors.Join(&NodeABIMismatchError{Msg: "abi"}, errors.New("Connection closed"))),
	}
	for _, err := range permanent {
		if got := classifyMCPFailure(err); got != "permanent" {
			t.Errorf("%v classified %s", err, got)
		}
	}
	dir := t.TempDir()
	_, err := resolveClientCert("remote", filepath.Join(dir, "missing.pem"), nil)
	if err == nil || !strings.Contains(err.Error(), "MCP server 'remote': client_cert not found at '") || classifyMCPFailure(err) != "permanent" {
		t.Fatalf("missing client cert must be a permanent not found error: %v", err)
	}
}

func TestErrorsAuthDetection(t *testing.T) {
	cases := map[error]bool{
		&OAuthFlowError{Msg: "expired"}: true, &OAuthTokenError{Msg: "t"}: true, &OAuthRegistrationError{Msg: "r"}: true,
		&OAuthNonInteractiveError{Msg: "n"}: true, &HTTPStatusError{Status: 401}: true, &HTTPStatusError{Status: 403}: false,
		errors.New("401 Unauthorized"): false, fmt.Errorf("wrapped: %w", &HTTPStatusError{Status: 401}): true,
	}
	for err, want := range cases {
		if got := isAuthError(err); got != want {
			t.Errorf("isAuthError(%v) = %v", err, got)
		}
	}
	if isAuthError(nil) {
		t.Fatal("nil is not an auth error")
	}
}

func TestErrorsAuthDetailAndCallFailedMessage(t *testing.T) {
	reg := &OAuthRegistrationError{Msg: "MCP OAuth 'gmail': this provider does not support automatic client registration ... add it under config.yaml mcp_servers.<name>.oauth (client_id, client_secret), then run `icebath mcp login gmail`."}
	detail := authErrorDetail(reg)
	if !strings.HasPrefix(detail, " ") || !strings.Contains(detail, "config.yaml") || !strings.Contains(detail, "client_id") {
		t.Fatalf("registration detail %q", detail)
	}
	if !strings.Contains(authErrorDetail(group(&OAuthRegistrationError{Msg: "config.yaml guidance"})), "config.yaml") {
		t.Fatal("detail must unwrap the group")
	}
	if authErrorDetail(errors.New("nope")) != "" || authErrorDetail(group(context.DeadlineExceeded)) != "" {
		t.Fatal("non registration errors carry no detail")
	}
	msg := mcpCallFailedMessage(group(&OAuthRegistrationError{Msg: "registration guidance text"}))
	if !strings.Contains(msg, "OAuthRegistrationError") || !strings.Contains(msg, "registration guidance text") || strings.Contains(msg, "TaskGroup") {
		t.Fatalf("call failed message %q", msg)
	}
	if msg := mcpCallFailedMessage(silentErr{}); !strings.HasPrefix(msg, "MCP call failed: silentErr: ") || strings.HasSuffix(msg, ": ") {
		t.Fatalf("empty message must still carry diagnostics: %q", msg)
	}
	if msg := mcpCallFailedMessage(errors.New("token sk-abcdefghijklmnopqrstuvwxyz0123456789 leaked")); strings.Contains(msg, "abcdefghijklmnop") {
		t.Fatalf("call failed message must be sanitized: %q", msg)
	}
}

func TestErrorsInvalidURL(t *testing.T) {
	for _, url := range []string{"http://localhost:3000/mcp", "https://example.com/mcp", "https://context7.liam.com/mcp",
		"http://127.0.0.1:8080", "https://api.example.com:443/v1/mcp?session=abc", "http://[::1]:9000/mcp", "https://host.example.com"} {
		got, err := validatedRemoteMCPURL("test", url)
		if err != nil || got != url {
			t.Errorf("%s rejected: %v", url, err)
		}
	}
	if got, err := validatedRemoteMCPURL("test", "  https://example.com/mcp  "); err != nil || got != "https://example.com/mcp" {
		t.Fatalf("strip %q %v", got, err)
	}
	bad := map[string]any{
		"context7":        nil,
		"my-weird-server": "not a url at all",
	}
	for name, url := range bad {
		err := validateRemoteMCPURL(name, url)
		var inv *InvalidMCPURLError
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
	cases := []struct {
		url  any
		text string
	}{
		{nil, "Invalid MCP URL for 's': expected a string, got NoneType"},
		{map[string]any{}, "Invalid MCP URL for 's': expected a string, got dict"},
		{"   ", "Invalid MCP URL for 's': empty url"},
		{"ftp://example.com", "Invalid MCP URL for 's': scheme must be http or https, got 'ftp' ('ftp://example.com')"},
		{"not a url at all", "Invalid MCP URL for 's': scheme must be http or https, got '' ('not a url at all')"},
		{"http://", "Invalid MCP URL for 's': missing host ('http://')"},
		{"http://:8080", "Invalid MCP URL for 's': missing hostname ('http://:8080')"},
		{"http://[::1", "Invalid MCP URL for 's': 'http://[::1' (Invalid IPv6 URL)"},
	}
	for _, c := range cases {
		if err := validateRemoteMCPURL("s", c.url); err == nil || err.Error() != c.text {
			t.Errorf("%v: %v", c.url, err)
		}
	}
	if classifyMCPFailure(validateRemoteMCPURL("s", "ftp://x")) != "permanent" {
		t.Fatal("invalid URL is permanent")
	}
}

func TestErrorsFormatConnectError(t *testing.T) {
	first := &chainErr{msg: "first failure"}
	second := &chainErr{msg: "second failure", next: first}
	first.next = second
	if got := formatConnectError(first); got != "first failure; second failure" {
		t.Fatalf("cyclic chain %q", got)
	}
	bottom := &causeAndContext{msg: "spawn failed", cause: &fs.PathError{Op: "fork/exec", Path: "/opt/homebrew/bin/removed-mcp-server", Err: syscall.ENOENT}}
	var current error = bottom
	for i := 0; i < 1010; i++ {
		current = &chainErr{msg: "stdio startup failed", next: current}
	}
	bottom.context = current
	if got := formatConnectError(current); got != "missing executable '/opt/homebrew/bin/removed-mcp-server'" {
		t.Fatalf("deep cyclic chain %q", got)
	}
	npx := formatConnectError(group(&exec.Error{Name: "npx", Err: exec.ErrNotFound}))
	if !strings.HasPrefix(npx, "missing executable 'npx' (ensure Node.js is installed") || !strings.Contains(npx, "mcp_servers.<name>.env.PATH") {
		t.Fatalf("npx hint %q", npx)
	}
	if got := formatConnectError(&fileNotFoundError{msg: "[Errno 2] No such file or directory: 'uvx'"}); got != "missing executable 'uvx'" {
		t.Fatalf("quoted missing file %q", got)
	}
	abi := &NodeABIMismatchError{Msg: "native addon built for a different Node.js"}
	if got := formatConnectError(fmt.Errorf("conn: %w", errors.Join(errors.New("Connection closed"), abi))); got != abi.Msg {
		t.Fatalf("abi error is the whole story, got %q", got)
	}
	got := formatConnectError(group(errors.New("a"), silentErr{}, errors.New("b"), errors.New("c"), errors.New("a")))
	if got != "a; silentErr; b" {
		t.Fatalf("flattened %q", got)
	}
	if got := formatConnectError(fmt.Errorf("handshake: %w", errors.New("Connection closed"))); got != "handshake: Connection closed" {
		t.Fatalf("wrapped text is not repeated, got %q", got)
	}
}

func TestErrorsSessionExpired(t *testing.T) {
	for _, err := range []error{
		errors.New("Invalid or expired session"), errors.New("HTTP 404: Session not found"), ErrSessionExpired,
		fmt.Errorf("call failed: %w", io.EOF), &chainErr{msg: "wrapped", next: silentErrEOF{}}, errors.New("transport is closed"),
		group(errors.New("Connection closed")), syscall.EPIPE, net.ErrClosed,
	} {
		if !isSessionExpiredError(err) {
			t.Errorf("%v must look like a session expiry", err)
		}
	}
	for _, err := range []error{
		errors.New("permission denied"), &HTTPStatusError{Status: 401}, nil,
		fmt.Errorf("session expired: %w", ErrInterrupted), group(errors.New("connection closed"), ErrInterrupted),
	} {
		if isSessionExpiredError(err) {
			t.Errorf("%v must not look like a session expiry", err)
		}
	}
}

type silentErrEOF struct{}

func (silentErrEOF) Error() string { return "" }
func (silentErrEOF) Unwrap() error { return io.EOF }

func TestErrorsStreamableHTTPRejection(t *testing.T) {
	for _, status := range []int{400, 405, 406, 411} {
		if !isStreamableHTTPRejection(group(&HTTPStatusError{Status: status, Method: "POST", URL: "https://x/mcp"})) {
			t.Errorf("%d must be a rejection", status)
		}
	}
	for _, err := range []error{&HTTPStatusError{Status: 401}, &HTTPStatusError{Status: 500}, context.DeadlineExceeded,
		&RPCError{Code: -32603, Message: "boom"}} {
		if isStreamableHTTPRejection(err) {
			t.Errorf("%v must not be a rejection", err)
		}
	}
	opaque := &RPCError{Code: -32603, Message: "Server returned an error response"}
	if !isStreamableHTTPRejection(group(opaque)) {
		t.Fatal("opaque internal error is a rejection")
	}
	rej := HTTPRejection{Status: 400, Method: "POST", URL: "https://x/mcp", Body: `{"code":-32020,"message":"Unsupported MCP-Protocol-Version"}`}
	if got := describeHTTPFailure(group(opaque), rej); got != opaque.Error()+` (HTTP 400 from POST https://x/mcp: {"code":-32020,"message":"Unsupported MCP-Protocol-Version"})` {
		t.Fatalf("described %q", got)
	}
	if got := describeHTTPFailure(opaque, HTTPRejection{}); got != opaque.Error() {
		t.Fatalf("no recorded rejection %q", got)
	}
	plain := errors.New("connect refused")
	if got := describeHTTPFailure(group(plain), rej); got != "connect refused" {
		t.Fatalf("non opaque root keeps its text %q", got)
	}
	if e := newNonMCPEndpointError("html page"); e.Error() != "html page" || classifyMCPFailure(e) != "permanent" {
		t.Fatal("non MCP endpoint error")
	}
}

func TestErrorsConnectionErrorIsATransientBarrier(t *testing.T) {
	wrapped := &mcpConnectionError{msg: "MCP server 's': Streamable HTTP connect failed (HTTP 401)", cause: &HTTPStatusError{Status: 401}}
	if classifyMCPFailure(wrapped) != "transient" || isAuthError(wrapped) {
		t.Fatal("a connection error is transient and never an auth error, whatever it wraps")
	}
	if classifyMCPFailure(group(wrapped)) != "transient" {
		t.Fatal("grouped connection error")
	}
	abi := &NodeABIMismatchError{Msg: "native addon built for a different Node.js"}
	reraised := &mcpConnectionError{msg: abi.Msg, cause: errors.Join(abi, errors.New("Connection closed"))}
	if classifyMCPFailure(reraised) != "permanent" || formatConnectError(reraised) != abi.Msg {
		t.Fatal("a wrapper re-raising a node ABI error classifies as that error")
	}
	if !isAuthError(fmt.Errorf("call: %w", &OAuthRegistrationError{Msg: "r"})) {
		t.Fatal("plain wrappers still expose the auth error")
	}
}
