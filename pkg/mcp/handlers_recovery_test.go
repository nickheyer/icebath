package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Transport that swallows matching requests without a reply so the call hangs until cancelled
type silentTransport struct {
	*handlerTransport
	drop func(method string) bool
}

func (s *silentTransport) Send(ctx context.Context, msg *Message) error {
	if msg.IsRequest() && s.drop(msg.Method) {
		return nil
	}
	return s.handlerTransport.Send(ctx, msg)
}

// Sets the stdio child pids the death watcher polls
func setChildPIDs(srv *serverTask, pids ...int) {
	srv.mu.Lock()
	srv.stdioChildPIDs = pids
	srv.mu.Unlock()
}

func TestIsAuthErrorDetectsOAuthFlowError(t *testing.T) {
	if !isAuthError(&OAuthFlowError{Msg: "expired"}) {
		t.Fatal("OAuthFlowError not classified as auth")
	}
}

func TestToolHandlerOAuthFlowErrorUnrecoverableNeedsReauth(t *testing.T) {
	handlerServer(t, "srv", func(string, map[string]any) (any, error) { return nil, &OAuthFlowError{Msg: "token expired"} })
	prev := oauthHandle401
	t.Cleanup(func() { oauthHandle401 = prev })
	oauthHandle401 = func(context.Context, string) (bool, error) { return false, nil }
	out := decodeResult(t, makeToolHandler("srv", "tool1", 10, false)(context.Background(), toolCall(map[string]any{"arg": "v"})))
	msg := strings.ToLower(out["error"].(string))
	if out["needs_reauth"] != true || out["server"] != "srv" || !(strings.Contains(msg, "re-auth") || strings.Contains(msg, "reauth")) {
		t.Fatalf("got %v", out)
	}
}

func TestToolHandler401RecoveryRaisingNeedsReauth(t *testing.T) {
	handlerServer(t, "srv", func(string, map[string]any) (any, error) { return nil, &OAuthFlowError{Msg: "token expired"} })
	prev := oauthHandle401
	t.Cleanup(func() { oauthHandle401 = prev })
	oauthHandle401 = func(context.Context, string) (bool, error) { return false, errors.New("token endpoint exploded") }
	out := decodeResult(t, makeToolHandler("srv", "tool1", 10, false)(context.Background(), toolCall(nil)))
	if out["needs_reauth"] != true || serverErrorCounts[ServerKey{Name: "srv"}] != 1 {
		t.Fatalf("got %v strikes %d", out, serverErrorCounts[ServerKey{Name: "srv"}])
	}
}

func TestToolHandlerNonAuthErrorStillGeneric(t *testing.T) {
	handlerServer(t, "srv", func(string, map[string]any) (any, error) { return nil, errors.New("unrelated") })
	out := decodeResult(t, makeToolHandler("srv", "tool1", 10, false)(context.Background(), toolCall(map[string]any{"arg": "v"})))
	if _, reauth := out["needs_reauth"]; reauth || out["error"] == nil || out["error"] == "" {
		t.Fatalf("got %v", out)
	}
}

func TestStdioMidcallChildExitReconnectsWithoutReplay(t *testing.T) {
	var effects atomic.Int32
	srv, tr := handlerServer(t, "srv-midcall", nil)
	setChildPIDs(srv, os.Getpid())
	hanging := &silentTransport{handlerTransport: tr, drop: func(method string) bool {
		if method != methodToolsCall {
			return false
		}
		effects.Add(1)
		setChildPIDs(srv, 1<<30)
		return true
	}}
	sess := NewSession(hanging, SessionOptions{Name: "srv-midcall"})
	t.Cleanup(func() { sess.Close() })
	srv.setSession(sess)
	var swaps atomic.Int32
	serveReconnects(t, srv, func(string, map[string]any) (any, error) {
		effects.Add(1)
		return textResult("ok"), nil
	}, func() {
		swaps.Add(1)
		setChildPIDs(srv, os.Getpid())
	})
	out := decodeResult(t, makeToolHandler("srv-midcall", "tool1", 10, false)(context.Background(), toolCall(map[string]any{})))
	if out["outcome_uncertain"] != true || swaps.Load() != 1 || effects.Load() != 1 {
		t.Fatalf("got %v reconnects %d effects %d", out, swaps.Load(), effects.Load())
	}
}

func TestStdioSDKFirstTransportCloseMidcallIsUncertainWithoutReplay(t *testing.T) {
	var effects atomic.Int32
	srv, _ := handlerServer(t, "srv-sdk-first", func(string, map[string]any) (any, error) {
		effects.Add(1)
		return nil, io.ErrClosedPipe
	})
	setChildPIDs(srv, os.Getpid())
	var swaps atomic.Int32
	serveReconnects(t, srv, func(string, map[string]any) (any, error) {
		effects.Add(1)
		return textResult("ok"), nil
	}, func() { swaps.Add(1) })
	out := decodeResult(t, makeToolHandler("srv-sdk-first", "tool1", 10, false)(context.Background(), toolCall(map[string]any{})))
	if out["outcome_uncertain"] != true || swaps.Load() != 1 || effects.Load() != 1 {
		t.Fatalf("got %v reconnects %d effects %d", out, swaps.Load(), effects.Load())
	}
}

// Shortens the respawn wait for one test
func shortRespawnWait(t *testing.T) {
	t.Helper()
	prev := stdioRespawnWait
	stdioRespawnWait = time.Second
	t.Cleanup(func() { stdioRespawnWait = prev })
}

func TestStdioDeadChildNeverReturningIsNotReportedAsTimeout(t *testing.T) {
	shortRespawnWait(t)
	srv, tr := handlerServer(t, "srv-gone", func(string, map[string]any) (any, error) { return textResult("ok"), nil })
	setChildPIDs(srv, 1<<30)
	out := decodeResult(t, makeToolHandler("srv-gone", "tool1", 300, false)(context.Background(), toolCall(map[string]any{})))
	message, _ := out["error"].(string)
	if !strings.Contains(message, "exited") {
		t.Fatalf("got %v", out)
	}
	for _, forbidden := range []string{"TimeoutError", "300s", "timed out"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("%q in %q", forbidden, message)
		}
	}
	flushLoop(t)
	if !srv.reconnect.IsSet() || tr.count(methodToolsCall) != 0 || serverErrorCounts[ServerKey{Name: "srv-gone"}] != 1 {
		t.Fatalf("reconnect %v calls %d strikes %d", srv.reconnect.IsSet(), tr.count(methodToolsCall), serverErrorCounts[ServerKey{Name: "srv-gone"}])
	}
}

func TestStdioChildDyingAgainAfterRespawnDoesNotHotCycle(t *testing.T) {
	shortRespawnWait(t)
	var called atomic.Int32
	respond := func(method string, _ map[string]any) (any, error) {
		if method == methodToolsCall {
			called.Add(1)
		}
		return textResult("ok"), nil
	}
	srv, _ := handlerServer(t, "srv-flap", respond)
	setChildPIDs(srv, 1<<30)
	var swaps atomic.Int32
	serveReconnects(t, srv, respond, func() { swaps.Add(1) })
	raw := makeToolHandler("srv-flap", "tool1", 10, false)(context.Background(), toolCall(map[string]any{}))
	time.Sleep(20 * time.Millisecond)
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out["error"] == nil {
		t.Fatalf("got %s", raw)
	}
	if swaps.Load() != 1 || called.Load() != 0 || serverErrorCounts[ServerKey{Name: "srv-flap"}] != 1 {
		t.Fatalf("respawns %d calls %d strikes %d", swaps.Load(), called.Load(), serverErrorCounts[ServerKey{Name: "srv-flap"}])
	}
}
