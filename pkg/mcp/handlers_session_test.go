package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Takes the MCP loop down until the test ends
func withoutLoop(t *testing.T) {
	t.Helper()
	mcpMu.Lock()
	prev := mcpLoopState
	mcpLoopState = nil
	mcpMu.Unlock()
	t.Cleanup(func() {
		mcpMu.Lock()
		mcpLoopState = prev
		mcpMu.Unlock()
	})
}

func TestSessionExpiredRetryWaitsForNewSession(t *testing.T) {
	srv, _ := handlerServer(t, "hindsight", func(string, map[string]any) (any, error) { return nil, errors.New("Session terminated") })
	makeHTTP(srv)
	serveReconnects(t, srv, func(string, map[string]any) (any, error) { return textResult("bank ok"), nil })
	key := ServerKey{Name: "hindsight"}
	mcpMu.Lock()
	serverErrorCounts[key] = 7
	toolReadOnlyHints[key] = map[string]bool{"get_bank": true}
	serverBreakerOpenedAt[key] = time.Now().Add(-circuitBreakerCooldown - time.Second)
	mcpMu.Unlock()
	out := decodeResult(t, makeToolHandler("hindsight", "get_bank", 10, false)(context.Background(), toolCall(map[string]any{})))
	mcpMu.Lock()
	count := serverErrorCounts[key]
	_, opened := serverBreakerOpenedAt[key]
	mcpMu.Unlock()
	if out["result"] != "bank ok" || count != 0 || opened {
		t.Fatalf("got %v count %d breaker opened %v", out, count, opened)
	}
}

func TestSessionExpiredHandlerWithoutLoop(t *testing.T) {
	handlerServer(t, "srv-noloop", nil)
	withoutLoop(t)
	expired := errors.New("Invalid or expired session")
	retry := func() (string, error) { return `{"ok": true}`, nil }
	if out, handled := handleSessionExpiredAndRetry(context.Background(), "srv-noloop", expired, retry, "tools/call", false); handled {
		t.Fatalf("read only call without a loop handled %s", out)
	}
	out, handled := handleSessionExpiredAndRetry(context.Background(), "srv-noloop", expired, retry, "tools/call", true)
	if !handled || decodeResult(t, out)["outcome_uncertain"] != true {
		t.Fatalf("write call without a loop %v %s", handled, out)
	}
}

func TestSessionExpiredHandlerWithoutServerRecord(t *testing.T) {
	resetLedgers(t)
	resetHandlerLedgers(t)
	ensureMCPLoop()
	out, handled := handleSessionExpiredAndRetry(context.Background(), "does-not-exist", errors.New("Invalid or expired session"),
		func() (string, error) { return `{"ok": true}`, nil }, "tools/call", false)
	if handled {
		t.Fatalf("missing server handled %s", out)
	}
}

func TestSessionExpiredNonToolHandlersReconnect(t *testing.T) {
	for _, tc := range []struct {
		label   string
		factory func(string, float64) tools.Handler
		method  string
		args    map[string]any
		reply   map[string]any
	}{
		{"list_resources", makeListResourcesHandler, methodResourcesList, map[string]any{}, map[string]any{"resources": []any{}}},
		{"read_resource", makeReadResourceHandler, methodResourcesRead, map[string]any{"uri": "file://foo"}, map[string]any{"contents": []any{}}},
		{"list_prompts", makeListPromptsHandler, methodPromptsList, map[string]any{}, map[string]any{"prompts": []any{}}},
		{"get_prompt", makeGetPromptHandler, methodPromptsGet, map[string]any{"name": "p1"}, map[string]any{"messages": []any{}}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			name := "srv-" + tc.label
			var calls atomic.Int32
			srv, _ := handlerServer(t, name, func(method string, _ map[string]any) (any, error) {
				if method == tc.method {
					calls.Add(1)
				}
				return nil, errors.New("Invalid or expired session")
			})
			var reconnects atomic.Int32
			serveReconnects(t, srv, func(method string, _ map[string]any) (any, error) {
				if method == tc.method {
					calls.Add(1)
				}
				return tc.reply, nil
			}, func() { reconnects.Add(1) })
			out := decodeResult(t, tc.factory(name, 10)(context.Background(), &tools.Call{Args: tc.args, Env: &tools.Env{}}))
			if _, failed := out["error"]; failed || reconnects.Load() != 1 || calls.Load() != 2 {
				t.Fatalf("got %v reconnects %d calls %d", out, reconnects.Load(), calls.Load())
			}
		})
	}
}

func TestToolIsReadOnlyFailsSafe(t *testing.T) {
	resetHandlerLedgers(t)
	ctx := context.Background()
	if toolIsReadOnly(ctx, "no-such-server", "tool") {
		t.Fatal("unknown server classified read only")
	}
	mcpMu.Lock()
	toolReadOnlyHints[ServerKey{Name: "known"}] = map[string]bool{"reader": true, "writer": false}
	mcpMu.Unlock()
	if !toolIsReadOnly(ctx, "known", "reader") || toolIsReadOnly(ctx, "known", "writer") || toolIsReadOnly(ctx, "known", "unlisted") {
		t.Fatal("hints misread")
	}
}

func TestSessionExpiredHandlerRebuildsConfiguredServerTransport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		route  string
	}{
		{"stdio", map[string]any{"command": "librarian-mcp"}, "stdio"},
		{"http", map[string]any{"url": "https://neo4j.example.test/mcp", "skip_preflight": true}, "http"},
	} {
		for _, applicationError := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/application_error=%v", tc.name, applicationError), func(t *testing.T) {
				isolateHome(t)
				resetHandlerLedgers(t)
				fastServerTimings(t)
				startTestLoop(t)
				var mu sync.Mutex
				var routes []string
				var configs []map[string]any
				var sessions []*Session
				var calls atomic.Int32
				respond := func(method string, _ map[string]any) (any, error) {
					if method != methodToolsCall {
						return map[string]any{}, nil
					}
					if calls.Add(1) == 1 {
						return nil, io.ErrClosedPipe
					}
					return map[string]any{"content": []any{map[string]any{"type": "text", "text": "reconnected"}}, "isError": applicationError}, nil
				}
				s, _ := runnerTask(t, "resumed", tc.config, func(s *serverTask, ctx context.Context, _ int) (string, error) {
					cfg := s.currentConfig()
					route := "stdio"
					if _, ok := cfg["url"]; ok {
						route = "http"
					}
					sess := NewSession(&handlerTransport{in: make(chan *Message, 16), respond: respond}, SessionOptions{Name: "resumed"})
					mu.Lock()
					routes = append(routes, route)
					configs = append(configs, maps.Clone(cfg))
					sessions = append(sessions, sess)
					mu.Unlock()
					s.setSession(sess)
					fakeConnected(s, false)
					return s.waitForLifecycleEvent(ctx, nil)
				})
				mcpMu.Lock()
				mcpServers[ServerKey{Name: "resumed"}] = s
				toolReadOnlyHints[ServerKey{Name: "resumed"}] = map[string]bool{"health": true}
				mcpMu.Unlock()
				if err := s.start(context.Background(), s.rendered); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.shutdown(context.Background()) })
				out := decodeResult(t, makeToolHandler("resumed", "health", 10, false)(context.Background(), toolCall(map[string]any{})))
				mcpMu.Lock()
				strikes := serverErrorCounts[ServerKey{Name: "resumed"}]
				mcpMu.Unlock()
				if tc.route == "stdio" {
					if out["outcome_uncertain"] != true || calls.Load() != 1 {
						t.Fatalf("stdio %v calls %d", out, calls.Load())
					}
				} else {
					wantKey, wantStrikes := "result", 0
					if applicationError {
						wantKey, wantStrikes = "error", 1
					}
					if len(out) != 1 || out[wantKey] != "reconnected" || strikes != wantStrikes || calls.Load() != 2 {
						t.Fatalf("http %v strikes %d calls %d", out, strikes, calls.Load())
					}
				}
				mu.Lock()
				defer mu.Unlock()
				if !reflect.DeepEqual(routes, []string{tc.route, tc.route}) || !reflect.DeepEqual(configs, []map[string]any{tc.config, tc.config}) ||
					len(sessions) != 2 || sessions[0] == sessions[1] {
					t.Fatalf("routes %v configs %v sessions %d", routes, configs, len(sessions))
				}
			})
		}
	}
}
