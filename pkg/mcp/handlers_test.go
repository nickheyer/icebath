package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// handlerTransport answers each request through respond and records the methods it saw
type handlerTransport struct {
	mu      sync.Mutex
	in      chan *Message
	once    sync.Once
	methods []string
	respond func(method string, params map[string]any) (any, error)
}

func (f *handlerTransport) Send(ctx context.Context, msg *Message) error {
	if !msg.IsRequest() {
		return nil
	}
	var params map[string]any
	_ = json.Unmarshal(msg.Params, &params)
	f.mu.Lock()
	f.methods = append(f.methods, msg.Method)
	f.mu.Unlock()
	reply, err := f.respond(msg.Method, params)
	if err != nil {
		return err
	}
	resp, _ := NewResponse(msg.ID, reply)
	f.in <- resp
	return nil
}

func (f *handlerTransport) Receive() <-chan *Message { return f.in }
func (f *handlerTransport) Err() error               { return nil }
func (f *handlerTransport) Label() string            { return "handlers" }
func (f *handlerTransport) Close() error {
	f.once.Do(func() { close(f.in) })
	return nil
}

// Number of requests of one method the fake server saw
func (f *handlerTransport) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range f.methods {
		if m == method {
			n++
		}
	}
	return n
}

// Marks the task as a remote HTTP server
func makeHTTP(srv *serverTask) {
	srv.mu.Lock()
	srv.config = map[string]any{"url": "https://mcp.example/mcp"}
	srv.mu.Unlock()
}

// Registered server task over a scripted session with the loop running and fast readiness polls
func handlerServer(t *testing.T, name string, respond func(method string, params map[string]any) (any, error)) (*serverTask, *handlerTransport) {
	t.Helper()
	resetLedgers(t)
	resetHandlerLedgers(t)
	ensureMCPLoop()
	prevPoll := readinessPoll
	readinessPoll = func() { time.Sleep(5 * time.Millisecond) }
	t.Cleanup(func() { readinessPoll = prevPoll })
	tr := &handlerTransport{in: make(chan *Message, 16), respond: respond}
	sess := NewSession(tr, SessionOptions{Name: name})
	t.Cleanup(func() { sess.Close() })
	srv := newServerTask(name, &tools.Env{})
	srv.mu.Lock()
	srv.session = sess
	srv.mu.Unlock()
	srv.ready.Set()
	mcpMu.Lock()
	mcpServers[ServerKey{Name: name}] = srv
	mcpMu.Unlock()
	return srv, tr
}

// Clears the breaker, trust and cost ledgers the handlers read
func resetHandlerLedgers(t *testing.T) {
	t.Helper()
	clear := func() {
		mcpMu.Lock()
		defer mcpMu.Unlock()
		for k := range serverErrorCounts {
			delete(serverErrorCounts, k)
		}
		for k := range serverBreakerOpenedAt {
			delete(serverBreakerOpenedAt, k)
		}
		for k := range serverErrorsAllApplication {
			delete(serverErrorsAllApplication, k)
		}
		for k := range toolReadOnlyHints {
			delete(toolReadOnlyHints, k)
		}
		for k := range serverCostPolicies {
			delete(serverCostPolicies, k)
		}
		for k := range serverTrustLevels {
			delete(serverTrustLevels, k)
		}
	}
	clear()
	t.Cleanup(clear)
}

// Swaps the session whenever the reconnect event fires, standing in for the run loop
func serveReconnects(t *testing.T, srv *serverTask, respond func(method string, params map[string]any) (any, error), onSwap ...func()) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			if !srv.reconnect.IsSet() {
				continue
			}
			srv.reconnect.Clear()
			for _, fn := range onSwap {
				fn()
			}
			tr := &handlerTransport{in: make(chan *Message, 16), respond: respond}
			sess := NewSession(tr, SessionOptions{Name: srv.name})
			t.Cleanup(func() { sess.Close() })
			srv.setSession(sess)
			srv.ready.Set()
		}
	}()
}

// Text tool result
func textResult(s string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
}

// Decoded handler JSON
func decodeResult(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("not JSON %q: %v", s, err)
	}
	return m
}

// Swaps the trust consent prompt for one recording its requests and answering answer
func fakeConsent(t *testing.T, answer string) *[]tools.ElicitationOptions {
	t.Helper()
	var asked []tools.ElicitationOptions
	var mu sync.Mutex
	prev := requestTrustConsent
	requestTrustConsent = func(ctx context.Context, env *tools.Env, message, description string, o tools.ElicitationOptions) string {
		mu.Lock()
		defer mu.Unlock()
		o.Title = message
		asked = append(asked, o)
		if answer == "panic" {
			panic("approval backend exploded")
		}
		return answer
	}
	t.Cleanup(func() { requestTrustConsent = prev })
	return &asked
}

func toolCall(args map[string]any) *tools.Call {
	return &tools.Call{Name: "mcp__srv__write", Args: args, Env: &tools.Env{}}
}

func TestToolHandlerSuccessfulCall(t *testing.T) {
	_, tr := handlerServer(t, "srv", func(method string, params map[string]any) (any, error) {
		return textResult("hello from mcp"), nil
	})
	out := makeToolHandler("srv", "greet", 30, false)(context.Background(), toolCall(map[string]any{"name": "x"}))
	if got := decodeResult(t, out)["result"]; got != "hello from mcp" {
		t.Fatalf("result %v from %s", got, out)
	}
	if tr.count(methodToolsCall) != 1 || serverErrorCounts[ServerKey{Name: "srv"}] != 0 {
		t.Fatalf("calls %d errors %d", tr.count(methodToolsCall), serverErrorCounts[ServerKey{Name: "srv"}])
	}
}

func TestToolHandlerNotConnectedStrikes(t *testing.T) {
	resetLedgers(t)
	resetHandlerLedgers(t)
	out := makeToolHandler("ghost", "greet", 0.01, false)(context.Background(), toolCall(nil))
	if !strings.Contains(decodeResult(t, out)["error"].(string), "MCP server 'ghost' is not connected") {
		t.Fatalf("got %s", out)
	}
	if serverErrorCounts[ServerKey{Name: "ghost"}] != 1 {
		t.Fatalf("breaker strike missing")
	}
}

func TestCheckFunction(t *testing.T) {
	resetLedgers(t)
	check := makeCheckFn("srv")
	t.Run("test_disconnected_returns_false", func(t *testing.T) {
		if check(&tools.Env{}) {
			t.Fatal("disconnected server reported available")
		}
	})
	handlerServer(t, "srv", nil)
	if !makeCheckFn("srv")(&tools.Env{}) {
		t.Fatal("connected server reported unavailable")
	}
	mcpMu.Lock()
	delete(mcpServers, ServerKey{Name: "srv"})
	lazyServerConfigs[ServerKey{Name: "srv"}] = map[string]any{"command": "x"}
	mcpMu.Unlock()
	if !check(&tools.Env{}) {
		t.Fatal("lazy server reported unavailable")
	}
	t.Run("test_recycled_stdio_server_remains_available_for_lazy_reconnect", func(t *testing.T) {
		recycled := newServerTask("srv", &tools.Env{})
		recycled.mu.Lock()
		recycled.config = map[string]any{"command": "npx"}
		recycled.recycledReason = "idle_timeout_seconds"
		recycled.mu.Unlock()
		mcpMu.Lock()
		delete(lazyServerConfigs, ServerKey{Name: "srv"})
		mcpServers[ServerKey{Name: "srv"}] = recycled
		mcpMu.Unlock()
		if !check(&tools.Env{}) {
			t.Fatal("recycled stdio server reported unavailable")
		}
	})
}

func TestUtilityHandlers(t *testing.T) {
	handlerServer(t, "srv", func(method string, params map[string]any) (any, error) {
		switch method {
		case methodResourcesList:
			return map[string]any{"resources": []any{map[string]any{"uri": "file:///a.txt", "name": "a", "mimeType": "text/plain"},
				map[string]any{"uri": "file:///b", "name": "b", "description": "bee"}}}, nil
		case methodPromptsGet:
			return map[string]any{"description": "greeting", "messages": []any{
				map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "hi " + params["arguments"].(map[string]any)["who"].(string)}},
				map[string]any{"role": "assistant", "content": map[string]any{"type": "image", "data": "AAA", "mimeType": "image/png"}}}}, nil
		case methodPromptsList:
			return map[string]any{"prompts": []any{map[string]any{"name": "p", "arguments": []any{map[string]any{"name": "who", "required": true}, map[string]any{"name": "tone"}}}}}, nil
		}
		return map[string]any{"contents": []any{map[string]any{"uri": "file:///a.txt", "text": "body"}}}, nil
	})
	ctx := context.Background()
	t.Run("test_list_resources_success", func(t *testing.T) {
		out := makeListResourcesHandler("srv", 30)(ctx, &tools.Call{Args: map[string]any{}, Env: &tools.Env{}})
		if want := `{"resources": [{"uri": "file:///a.txt", "name": "a", "mimeType": "text/plain"}, {"uri": "file:///b", "name": "b", "description": "bee"}]}`; out != want {
			t.Fatalf("list_resources %s", out)
		}
	})
	t.Run("test_get_prompt_success", func(t *testing.T) {
		out := makeGetPromptHandler("srv", 30)(ctx, &tools.Call{Args: map[string]any{"name": "p", "arguments": map[string]any{"who": "bob"}}, Env: &tools.Env{}})
		if want := `{"messages": [{"role": "user", "content": "hi bob"}, {"role": "assistant", "content": "type='image' data='AAA' mime_type='image/png' annotations=None meta=None"}], "description": "greeting"}`; out != want {
			t.Fatalf("get_prompt %s", out)
		}
	})
	out := makeListPromptsHandler("srv", 30)(ctx, &tools.Call{Args: map[string]any{}, Env: &tools.Env{}})
	if want := `{"prompts": [{"name": "p", "arguments": [{"name": "who", "required": true}, {"name": "tone", "required": null}]}]}`; out != want {
		t.Fatalf("list_prompts %s", out)
	}
	out = makeReadResourceHandler("srv", 30)(ctx, &tools.Call{Args: map[string]any{"uri": "file:///a.txt"}, Env: &tools.Env{}})
	if out != `{"result": "body"}` {
		t.Fatalf("read_resource %s", out)
	}
	out = makeReadResourceHandler("srv", 30)(ctx, &tools.Call{Args: map[string]any{}, Env: &tools.Env{}})
	if out != `{"error": "Missing required parameter 'uri'"}` {
		t.Fatalf("missing uri %s", out)
	}
	out = makeGetPromptHandler("srv", 30)(ctx, &tools.Call{Args: map[string]any{"name": "p", "arguments": map[string]any{"who": 3.0}}, Env: &tools.Env{}})
	if !strings.Contains(out, "MCP call failed: ValidationError: 1 validation error for GetPromptRequestParams") {
		t.Fatalf("non string prompt argument %s", out)
	}
}

func TestTrustGateUntrustedWriteAsksAndRuns(t *testing.T) {
	_, tr := handlerServer(t, "srv", func(string, map[string]any) (any, error) { return textResult("done"), nil })
	serverTrustLevels[ServerKey{Name: "srv"}] = TrustUntrusted
	asked := fakeConsent(t, "accept")
	out := makeToolHandler("srv", "write", 30, false)(context.Background(), toolCall(map[string]any{"path": "/x", "db_password": "hunter2"}))
	if decodeResult(t, out)["result"] != "done" || len(*asked) != 1 || tr.count(methodToolsCall) != 1 {
		t.Fatalf("out %s asked %d calls %d", out, len(*asked), tr.count(methodToolsCall))
	}
	o := (*asked)[0]
	if o.RememberKey != "mcp:srv/write" || o.Surface != "mcp-trust/srv" {
		t.Fatalf("options %+v", o)
	}
	if !strings.Contains(o.Title, "MCP tool 'write' on UNTRUSTED server 'srv' wants to run.") ||
		!strings.Contains(o.Title, "\n\nArguments:\n{\n  \"db_password\": \"***\",\n  \"path\": \"/x\"\n}") || strings.Contains(o.Title, "hunter2") {
		t.Fatalf("message %q", o.Title)
	}
}

func TestTrustGateDenyBlocksBeforeTransport(t *testing.T) {
	_, tr := handlerServer(t, "srv", func(string, map[string]any) (any, error) { return textResult("done"), nil })
	serverTrustLevels[ServerKey{Name: "srv"}] = TrustUntrusted
	fakeConsent(t, "decline")
	out := makeToolHandler("srv", "write", 30, false)(context.Background(), toolCall(nil))
	if !strings.Contains(out, "The user did not approve running write-capable MCP tool 'write' on untrusted server 'srv'") || tr.count(methodToolsCall) != 0 {
		t.Fatalf("out %s calls %d", out, tr.count(methodToolsCall))
	}
}

func TestTrustGateReadOnlyAndFullTrustSkipPrompt(t *testing.T) {
	handlerServer(t, "srv", func(string, map[string]any) (any, error) { return textResult("ok"), nil })
	key := ServerKey{Name: "srv"}
	serverTrustLevels[key] = TrustUntrusted
	toolReadOnlyHints[key] = map[string]bool{"read": true}
	asked := fakeConsent(t, "decline")
	if out := makeToolHandler("srv", "read", 30, false)(context.Background(), toolCall(nil)); decodeResult(t, out)["result"] != "ok" {
		t.Fatalf("read only %s", out)
	}
	serverTrustLevels[key] = TrustFull
	if out := makeToolHandler("srv", "write", 30, false)(context.Background(), toolCall(nil)); decodeResult(t, out)["result"] != "ok" {
		t.Fatalf("full trust %s", out)
	}
	if len(*asked) != 0 {
		t.Fatalf("prompted %d times", len(*asked))
	}
}

func TestTrustGateApprovalFailureFailsClosed(t *testing.T) {
	_, tr := handlerServer(t, "srv", func(string, map[string]any) (any, error) { return textResult("ok"), nil })
	serverTrustLevels[ServerKey{Name: "srv"}] = TrustUntrusted
	fakeConsent(t, "panic")
	out := makeToolHandler("srv", "write", 30, false)(context.Background(), toolCall(nil))
	if !strings.Contains(out, "was blocked: the approval system was unavailable (fail-closed).") || tr.count(methodToolsCall) != 0 {
		t.Fatalf("out %s", out)
	}
}

func TestTrustGateArgumentsBoundedAndEscaped(t *testing.T) {
	got := formatTrustGateArguments(&tools.Env{}, map[string]any{"a": "x\u202ey", "b": strings.Repeat("z", 6000)}, "")
	if !strings.Contains(got, `x\u202ey`) || !strings.HasSuffix(got, trustGateArgumentsTruncationMarker) || len([]rune(got)) > trustGateArgumentsMaxChars {
		t.Fatalf("formatted %d runes tail %q", len([]rune(got)), got[len(got)-60:])
	}
}

// Opens the breaker of srv with three application errors, returning the handler, transport and a clock advance
func openBreakerByToolErrors(t *testing.T) (tools.Handler, *handlerTransport, func(time.Duration), string) {
	t.Helper()
	_, tr := handlerServer(t, "srv", func(string, map[string]any) (any, error) {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "bad args"}}, "isError": true}, nil
	})
	now := time.Now()
	prev := managerNow
	managerNow = func() time.Time { return now }
	t.Cleanup(func() { managerNow = prev })
	h := makeToolHandler("srv", "greet", 30, false)
	for i := 0; i < 3; i++ {
		h(context.Background(), toolCall(nil))
	}
	out := h(context.Background(), toolCall(nil))
	return h, tr, func(d time.Duration) { now = now.Add(d) }, out
}

// test_mcp_circuit_breaker.py test_breaker_opened_by_tool_errors_says_rejected_not_unreachable
func TestBreakerOpenedByToolErrorsSaysRejectedNotUnreachable(t *testing.T) {
	_, tr, _, out := openBreakerByToolErrors(t)
	if !strings.Contains(out, "MCP server 'srv' rejected the last 3 calls (it is reachable") || strings.Contains(out, "unreachable") || !strings.Contains(out, "Paused for ~60s") || tr.count(methodToolsCall) != 3 {
		t.Fatalf("open breaker %s calls %d", out, tr.count(methodToolsCall))
	}
}

// test_mcp_circuit_breaker.py test_circuit_breaker_half_opens_after_cooldown
func TestCircuitBreakerHalfOpensAfterCooldown(t *testing.T) {
	h, tr, advance, _ := openBreakerByToolErrors(t)
	if tr.count(methodToolsCall) != 3 {
		t.Fatalf("open breaker still forwards calls %d", tr.count(methodToolsCall))
	}
	advance(61 * time.Second)
	h(context.Background(), toolCall(nil))
	if tr.count(methodToolsCall) != 4 {
		t.Fatal("half open breaker did not probe")
	}
}

func TestCircuitBreakerTransportFailuresReadUnreachable(t *testing.T) {
	handlerServer(t, "srv", func(string, map[string]any) (any, error) { return textResult("ok"), nil })
	for i := 0; i < 3; i++ {
		bumpServerError(context.Background(), "srv", false)
	}
	out := makeToolHandler("srv", "greet", 30, false)(context.Background(), toolCall(nil))
	if !strings.Contains(out, "MCP server 'srv' is unreachable after 3 consecutive failures.") {
		t.Fatalf("got %s", out)
	}
	resetServerError(context.Background(), "srv")
	if out := makeToolHandler("srv", "greet", 30, false)(context.Background(), toolCall(nil)); decodeResult(t, out)["result"] != "ok" {
		t.Fatalf("reset breaker %s", out)
	}
}

func sessionExpiredReadOnlyReplays(t *testing.T) {
	srv, _ := handlerServer(t, "srv", func(string, map[string]any) (any, error) { return nil, ErrSessionExpired })
	makeHTTP(srv)
	toolReadOnlyHints[ServerKey{Name: "srv"}] = map[string]bool{"read": true}
	serveReconnects(t, srv, func(string, map[string]any) (any, error) { return textResult("fresh"), nil })
	out := makeToolHandler("srv", "read", 30, false)(context.Background(), toolCall(nil))
	if decodeResult(t, out)["result"] != "fresh" {
		t.Fatalf("got %s", out)
	}
}

func TestSessionExpiredRetryOnlyForReadOnlyTools(t *testing.T) {
	t.Run("write_is_not_replayed", sessionExpiredWriteIsNotReplayed)
	t.Run("read_only_replays", sessionExpiredReadOnlyReplays)
}

func sessionExpiredWriteIsNotReplayed(t *testing.T) {
	srv, _ := handlerServer(t, "srv", func(string, map[string]any) (any, error) { return nil, ErrSessionExpired })
	makeHTTP(srv)
	calls := 0
	var mu sync.Mutex
	serveReconnects(t, srv, func(string, map[string]any) (any, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return textResult("fresh"), nil
	})
	out := decodeResult(t, makeToolHandler("srv", "write", 30, false)(context.Background(), toolCall(nil)))
	mu.Lock()
	defer mu.Unlock()
	if out["outcome_uncertain"] != true || !strings.Contains(out["error"].(string), "The connection has been re-established.") || calls != 0 {
		t.Fatalf("got %v replays %d", out, calls)
	}
}

func TestToolHandler401RecoversAndRetries(t *testing.T) {
	srv, _ := handlerServer(t, "srv", func(string, map[string]any) (any, error) { return nil, &HTTPStatusError{Status: 401} })
	makeHTTP(srv)
	serveReconnects(t, srv, func(string, map[string]any) (any, error) { return textResult("authed"), nil })
	prev := oauthHandle401
	t.Cleanup(func() { oauthHandle401 = prev })
	oauthHandle401 = func(context.Context, string) (bool, error) { return true, nil }
	if out := makeToolHandler("srv", "greet", 30, false)(context.Background(), toolCall(nil)); decodeResult(t, out)["result"] != "authed" {
		t.Fatalf("got %s", out)
	}
}

func TestCallToolHandlerReturnsNeedsReauthOnUnrecoverable401(t *testing.T) {
	handlerServer(t, "srv", func(string, map[string]any) (any, error) { return nil, &HTTPStatusError{Status: 401} })
	prev := oauthHandle401
	t.Cleanup(func() { oauthHandle401 = prev })
	oauthHandle401 = func(context.Context, string) (bool, error) { return false, nil }
	out := decodeResult(t, makeToolHandler("srv", "greet", 30, false)(context.Background(), toolCall(nil)))
	if out["needs_reauth"] != true || out["server"] != "srv" || !strings.Contains(out["error"].(string), "Run `icebath mcp login srv`") {
		t.Fatalf("got %v", out)
	}
	if serverErrorCounts[ServerKey{Name: "srv"}] != 1 {
		t.Fatal("needs_reauth did not strike")
	}
}

func TestPrecallDeadChildrenRespawnAndRetry(t *testing.T) {
	srv, tr := handlerServer(t, "srv", func(string, map[string]any) (any, error) { return textResult("never"), nil })
	srv.mu.Lock()
	srv.stdioChildPIDs = []int{1 << 30}
	srv.mu.Unlock()
	serveReconnects(t, srv, func(string, map[string]any) (any, error) { return textResult("respawned"), nil }, func() {
		srv.mu.Lock()
		srv.stdioChildPIDs = nil
		srv.mu.Unlock()
	})
	out := makeToolHandler("srv", "greet", 30, false)(context.Background(), toolCall(nil))
	if decodeResult(t, out)["result"] != "respawned" || tr.count(methodToolsCall) != 0 {
		t.Fatalf("got %s old calls %d", out, tr.count(methodToolsCall))
	}
}
