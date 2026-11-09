package mcp

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Freezes the breaker clock at a value the returned pointer moves
func breakerClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Now()
	prev := managerNow
	managerNow = func() time.Time { return now }
	t.Cleanup(func() { managerNow = prev })
	return &now
}

// Opens the breaker for srv at the frozen clock and moves past the cooldown
func openBreakerPastCooldown(now *time.Time, count int) {
	key := ServerKey{Name: "srv"}
	mcpMu.Lock()
	serverErrorCounts[key] = count
	serverBreakerOpenedAt[key] = *now
	mcpMu.Unlock()
	*now = now.Add(circuitBreakerCooldown + time.Second)
}

func TestCircuitBreakerReopensOnProbeFailure(t *testing.T) {
	var calls atomic.Int32
	handlerServer(t, "srv", func(method string, _ map[string]any) (any, error) {
		if method == methodToolsCall {
			calls.Add(1)
		}
		return nil, errors.New("still broken")
	})
	now := breakerClock(t)
	openBreakerPastCooldown(now, circuitBreakerThreshold)
	h := makeToolHandler("srv", "tool1", 10, false)
	if _, ok := decodeResult(t, h(context.Background(), toolCall(map[string]any{})))["error"]; !ok || calls.Load() != 1 {
		t.Fatalf("probe calls %d", calls.Load())
	}
	out := decodeResult(t, h(context.Background(), toolCall(map[string]any{})))
	if msg, _ := out["error"].(string); !strings.Contains(strings.ToLower(msg), "unreachable") || calls.Load() != 1 {
		t.Fatalf("reopened breaker %v calls %d", out, calls.Load())
	}
}

func TestHalfOpenProbeOnDeadSessionRequestsReconnect(t *testing.T) {
	srv, _ := handlerServer(t, "srv", nil)
	srv.setSession(nil)
	now := breakerClock(t)
	openBreakerPastCooldown(now, circuitBreakerThreshold)
	out := decodeResult(t, makeToolHandler("srv", "tool1", 10, false)(context.Background(), toolCall(map[string]any{})))
	flushLoop(t)
	if msg, _ := out["error"].(string); !strings.Contains(strings.ToLower(msg), "reconnect") || !srv.reconnect.IsSet() {
		t.Fatalf("got %v reconnect set %v", out, srv.reconnect.IsSet())
	}
}

func TestHalfOpenDeadSessionRecoversAfterReconnect(t *testing.T) {
	srv, _ := handlerServer(t, "srv", nil)
	srv.setSession(nil)
	now := breakerClock(t)
	openBreakerPastCooldown(now, circuitBreakerThreshold)
	h := makeToolHandler("srv", "tool1", 10, false)
	if msg, _ := decodeResult(t, h(context.Background(), toolCall(map[string]any{})))["error"].(string); !strings.Contains(strings.ToLower(msg), "reconnect") {
		t.Fatalf("dead session %q", msg)
	}
	live := NewSession(&handlerTransport{in: make(chan *Message, 16), respond: func(string, map[string]any) (any, error) {
		return textResult("ok"), nil
	}}, SessionOptions{Name: "srv"})
	t.Cleanup(func() { live.Close() })
	srv.setSession(live)
	resetServerError(context.Background(), "srv")
	*now = now.Add(circuitBreakerCooldown + time.Second)
	if out := decodeResult(t, h(context.Background(), toolCall(map[string]any{}))); out["result"] != "ok" {
		t.Fatalf("recovered %v", out)
	}
}

func TestCircuitBreakerClearedOnReconnect(t *testing.T) {
	srv, tr := handlerServer(t, "srv", func(string, map[string]any) (any, error) {
		return nil, errors.New("session.call_tool should not be reached in this test")
	})
	serveReconnects(t, srv, func(string, map[string]any) (any, error) { return nil, errors.New("unused") })
	mcpMu.Lock()
	serverErrorCounts[ServerKey{Name: "srv"}] = circuitBreakerThreshold + 2
	serverBreakerOpenedAt[ServerKey{Name: "srv"}] = time.Now()
	mcpMu.Unlock()
	prev := oauthHandle401
	t.Cleanup(func() { oauthHandle401 = prev })
	oauthHandle401 = func(context.Context, string) (bool, error) { return true, nil }
	result, handled := handleAuthErrorAndRetry(context.Background(), "srv", &OAuthFlowError{Msg: "initial"}, func() (string, error) {
		return "", &OAuthFlowError{Msg: "still failing post-reconnect"}
	}, "tools/call test")
	if !handled || decodeResult(t, result)["needs_reauth"] != true {
		t.Fatalf("handled %v result %s", handled, result)
	}
	mcpMu.Lock()
	count := serverErrorCounts[ServerKey{Name: "srv"}]
	mcpMu.Unlock()
	if count >= circuitBreakerThreshold || tr.count(methodToolsCall) != 0 {
		t.Fatalf("count %d threshold %d calls %d", count, circuitBreakerThreshold, tr.count(methodToolsCall))
	}
}
