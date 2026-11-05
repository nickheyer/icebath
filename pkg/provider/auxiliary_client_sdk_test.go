package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Recorded request of the SDK test server
type auxSDKSeen struct {
	path, query string
	headers     http.Header
	body        map[string]any
}

// Test server answering each request with the next scripted handler and recording what it saw
func auxSDKServer(t *testing.T, handlers ...http.HandlerFunc) (*httptest.Server, func() []auxSDKSeen) {
	t.Helper()
	var mu sync.Mutex
	var seen []auxSDKSeen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		seen = append(seen, auxSDKSeen{path: r.URL.Path, query: r.URL.RawQuery, headers: r.Header.Clone(), body: body})
		i := len(seen) - 1
		mu.Unlock()
		if i >= len(handlers) {
			i = len(handlers) - 1
		}
		handlers[i](w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []auxSDKSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]auxSDKSeen(nil), seen...)
	}
}

func auxSDKJSON(status int, payload any, headers map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}
}

func auxSDKRetries(n int) *int { return &n }

// Records the SDK backoff instead of sleeping
func auxSDKRecordSleeps(t *testing.T) *[]time.Duration {
	t.Helper()
	var sleeps []time.Duration
	old := auxSDKSleep
	auxSDKSleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	t.Cleanup(func() { auxSDKSleep = old })
	return &sleeps
}

func TestAuxSDKClientPostsChatCompletionsWithExtras(t *testing.T) {
	srv, seen := auxSDKServer(t, auxSDKJSON(200, map[string]any{"id": "c1", "choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}}}, nil))
	c := newAuxOpenAIClient("sk-test", srv.URL+"/v1", auxOpenAIOptions{
		DefaultHeaders: map[string]string{"X-Default": "d"}, DefaultQuery: map[string]string{"api-version": "1"}, MaxRetries: auxSDKRetries(0),
	})
	if c.BaseURL() != srv.URL+"/v1/" {
		t.Fatalf("base url %q", c.BaseURL())
	}
	out, err := c.CreateChatCompletion(context.Background(), map[string]any{
		"model": "m", "messages": []any{map[string]any{"role": "user", "content": "q"}}, "temperature": nil,
		"extra_body":    map[string]any{"reasoning": map[string]any{"enabled": false}},
		"extra_headers": map[string]any{"X-Extra": "e"}, "extra_query": map[string]any{"q": "v"}, "timeout": 30.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["id"] != "c1" {
		t.Fatalf("out %v", out)
	}
	got := seen()[0]
	if got.path != "/v1/chat/completions" || !strings.Contains(got.query, "api-version=1") || !strings.Contains(got.query, "q=v") {
		t.Fatalf("request %+v", got)
	}
	if got.headers.Get("Authorization") != "Bearer sk-test" || got.headers.Get("X-Default") != "d" || got.headers.Get("X-Extra") != "e" || got.headers.Get("X-Stainless-Retry-Count") != "0" {
		t.Fatalf("headers %v", got.headers)
	}
	if _, ok := got.body["extra_body"]; ok || got.body["reasoning"] == nil || got.body["model"] != "m" {
		t.Fatalf("body %v", got.body)
	}
	if v, ok := got.body["temperature"]; !ok || v != nil {
		t.Fatal("an explicit None goes on the wire as null")
	}
	if _, ok := got.body["timeout"]; ok {
		t.Fatal("timeout is a transport option")
	}
}

func TestAuxSDKCallableKeyIsMintedPerRequest(t *testing.T) {
	srv, seen := auxSDKServer(t, auxSDKJSON(200, map[string]any{"id": "x"}, nil))
	t.Setenv("ICEBATH_HOME", t.TempDir())
	n := 0
	provider := func() (string, error) {
		n++
		return fmt.Sprintf("fresh-jwt-%d", n), nil
	}
	client := auxCreateOpenAIClient(context.Background(), provider, srv.URL+"/openai/v1", auxOpenAIOptions{})
	sdk, ok := client.(*auxOpenAIClient)
	if !ok || sdk.apiKeyProvider == nil || sdk.APIKey() != "" {
		t.Fatal("the callable must reach the SDK client unchanged")
	}
	for i := 0; i < 2; i++ {
		if _, err := client.CreateChatCompletion(context.Background(), map[string]any{"model": "gpt-4o", "messages": []any{}}); err != nil {
			t.Fatal(err)
		}
	}
	if seen()[0].headers.Get("Authorization") != "Bearer fresh-jwt-1" || seen()[1].headers.Get("Authorization") != "Bearer fresh-jwt-2" || sdk.APIKey() != "fresh-jwt-2" {
		t.Fatal("the key must be minted before every request")
	}
}

func TestAuxSDKStatusErrorCarriesTheSDKShape(t *testing.T) {
	body := map[string]any{"error": map[string]any{"message": "slow down", "type": "rate_limit", "code": "rl"}}
	srv, _ := auxSDKServer(t, auxSDKJSON(429, body, map[string]string{"Retry-After": "3"}))
	c := newAuxOpenAIClient("k", srv.URL, auxOpenAIOptions{MaxRetries: auxSDKRetries(0)})
	_, err := c.CreateChatCompletion(context.Background(), map[string]any{"model": "m"})
	var sdk *AuxSDKError
	if !errors.As(err, &sdk) {
		t.Fatalf("err %T %v", err, err)
	}
	want := "Error code: 429 - {'error': {'code': 'rl', 'message': 'slow down', 'type': 'rate_limit'}}"
	if sdk.StatusCode != 429 || sdk.Class != "RateLimitError" || sdk.Message != want || sdk.Code != "rl" || sdk.Type != "rate_limit" {
		t.Fatalf("sdk error %+v", sdk)
	}
	if m, _ := sdk.Body.(map[string]any); m["message"] != "slow down" || sdk.Headers.Get("Retry-After") != "3" {
		t.Fatalf("body %v", sdk.Body)
	}
	if auxExcHTTPStatus(err) != 429 || auxPyExceptionName(err) != "RateLimitError" {
		t.Fatal("classifiers must read the SDK shape")
	}
	srv2, _ := auxSDKServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte(" bad gateway "))
	})
	_, err = newAuxOpenAIClient("k", srv2.URL, auxOpenAIOptions{MaxRetries: auxSDKRetries(0)}).CreateChatCompletion(context.Background(), map[string]any{})
	if !errors.As(err, &sdk) || sdk.Message != "bad gateway" || sdk.Body != "bad gateway" || sdk.Class != "InternalServerError" {
		t.Fatalf("text body error %+v", sdk)
	}
}

func TestAuxSDKRetriesFollowStatusRetryAfterAndShouldRetry(t *testing.T) {
	sleeps := auxSDKRecordSleeps(t)
	srv, seen := auxSDKServer(t,
		auxSDKJSON(500, map[string]any{"error": "boom"}, map[string]string{"retry-after-ms": "1500"}),
		auxSDKJSON(503, map[string]any{"error": "boom"}, nil),
		auxSDKJSON(200, map[string]any{"id": "ok"}, nil))
	out, err := newAuxOpenAIClient("k", srv.URL, auxOpenAIOptions{MaxRetries: auxSDKRetries(2)}).CreateChatCompletion(context.Background(), map[string]any{})
	if err != nil || out.(map[string]any)["id"] != "ok" {
		t.Fatalf("out %v err %v", out, err)
	}
	if len(*sleeps) != 2 || (*sleeps)[0] != 1500*time.Millisecond || (*sleeps)[1] < 750*time.Millisecond || (*sleeps)[1] > time.Second {
		t.Fatalf("sleeps %v", *sleeps)
	}
	if seen()[2].headers.Get("X-Stainless-Retry-Count") != "2" {
		t.Fatal("retry count header must count retries")
	}
	srv2, seen2 := auxSDKServer(t, auxSDKJSON(500, map[string]any{}, map[string]string{"x-should-retry": "false"}))
	_, err = newAuxOpenAIClient("k", srv2.URL, auxOpenAIOptions{MaxRetries: auxSDKRetries(2)}).CreateChatCompletion(context.Background(), map[string]any{})
	if auxExcHTTPStatus(err) != 500 || len(seen2()) != 1 {
		t.Fatal("x-should-retry false must stop retries")
	}
	srv3, seen3 := auxSDKServer(t, auxSDKJSON(400, map[string]any{}, nil))
	_, _ = newAuxOpenAIClient("k", srv3.URL, auxOpenAIOptions{MaxRetries: auxSDKRetries(2)}).CreateChatCompletion(context.Background(), map[string]any{})
	if len(seen3()) != 1 {
		t.Fatal("a 400 is never retried")
	}
}

func TestAuxSDKConnectionAndTimeoutErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close()
	_, err := newAuxOpenAIClient("k", dead, auxOpenAIOptions{MaxRetries: auxSDKRetries(0)}).CreateChatCompletion(context.Background(), map[string]any{})
	var sdk *AuxSDKError
	if !errors.As(err, &sdk) || sdk.Class != "APIConnectionError" || sdk.Message != "Connection error." {
		t.Fatalf("err %v", err)
	}
	release := make(chan struct{})
	defer close(release)
	slow, _ := auxSDKServer(t, func(http.ResponseWriter, *http.Request) { <-release })
	_, err = newAuxOpenAIClient("k", slow.URL, auxOpenAIOptions{MaxRetries: auxSDKRetries(0)}).CreateChatCompletion(context.Background(), map[string]any{"timeout": 0.1})
	if !errors.As(err, &sdk) || sdk.Class != "APITimeoutError" || sdk.Message != "Request timed out." || errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestAuxSDKNonJSONBodyIsReturnedAsText(t *testing.T) {
	srv, _ := auxSDKServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("plain text")) })
	out, err := newAuxOpenAIClient("k", srv.URL, auxOpenAIOptions{}).CreateChatCompletion(context.Background(), map[string]any{})
	if err != nil || out != "plain text" {
		t.Fatalf("out %v err %v", out, err)
	}
}

func auxSDKSSE(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			w.(http.Flusher).Flush()
		}
	}
}

func TestAuxSDKStreamYieldsChunksUntilDone(t *testing.T) {
	srv, seen := auxSDKServer(t, auxSDKSSE(": keepalive\n\n", "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\r\n\r\n", "data: [DONE]\n\n", "data: {\"after\":true}\n\n"))
	out, err := newAuxOpenAIClient("k", srv.URL, auxOpenAIOptions{}).CreateChatCompletion(context.Background(), map[string]any{"stream": true})
	if err != nil {
		t.Fatal(err)
	}
	if seen()[0].body["stream"] != true {
		t.Fatal("stream flag must stay in the body")
	}
	stream := out.(AuxChatStream)
	var text string
	for {
		chunk, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		text += chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"].(string)
	}
	if text != "ab" {
		t.Fatalf("text %q", text)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatal("a finished stream stays finished")
	}
}

func TestAuxSDKStreamRaisesErrorEventsAndDropsUnterminatedTail(t *testing.T) {
	srv, _ := auxSDKServer(t, auxSDKSSE("data: {\"error\":{\"message\":\"quota gone\",\"code\":\"insufficient_quota\"}}\n\n"))
	out, _ := newAuxOpenAIClient("k", srv.URL, auxOpenAIOptions{}).CreateChatCompletion(context.Background(), map[string]any{"stream": true})
	_, err := out.(AuxChatStream).Next(context.Background())
	var sdk *AuxSDKError
	if !errors.As(err, &sdk) || sdk.Message != "quota gone" || sdk.Class != "APIError" || sdk.Code != "insufficient_quota" {
		t.Fatalf("err %v", err)
	}
	srv2, _ := auxSDKServer(t, auxSDKSSE("data: {\"error\":{}}\n\n", "data: {\"id\":1}"))
	out, _ = newAuxOpenAIClient("k", srv2.URL, auxOpenAIOptions{}).CreateChatCompletion(context.Background(), map[string]any{"stream": true})
	stream := out.(AuxChatStream)
	if chunk, err := stream.Next(context.Background()); err != nil || chunk["error"] == nil {
		t.Fatalf("a falsy error object is an ordinary chunk, got %v %v", chunk, err)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("an unterminated trailing event is dropped, got %v", err)
	}
}

func TestAuxSDKStreamPinShutdownCutsABlockedRead(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv, _ := auxSDKServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":1}\n\n")
		w.(http.Flusher).Flush()
		<-release
	})
	out, err := newAuxOpenAIClient("k", srv.URL, auxOpenAIOptions{}).CreateChatCompletion(context.Background(), map[string]any{"stream": true})
	if err != nil {
		t.Fatal(err)
	}
	s := out.(*auxSDKStream)
	released := false
	shutdown := s.auxPinAttempt(func() { released = true })
	if _, err := s.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(100*time.Millisecond, shutdown)
	start := time.Now()
	_, err = s.Next(context.Background())
	if err == nil || errors.Is(err, io.EOF) || time.Since(start) > 5*time.Second {
		t.Fatalf("shutdown must cut the blocked read, got %v", err)
	}
	if !released {
		t.Fatal("the pin release runs when the stream lets its connection go")
	}
	shutdown()
}

func TestAuxSDKStreamHonorsTheCallersContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv, _ := auxSDKServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-release
	})
	out, _ := newAuxOpenAIClient("k", srv.URL, auxOpenAIOptions{}).CreateChatCompletion(context.Background(), map[string]any{"stream": true})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := out.(AuxChatStream).Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}

func TestCreateOpenAIClientRoutesViaEnvProxy(t *testing.T) {
	keepaliveNoProxyEnv(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7897")
	client := auxCreateOpenAIClient(context.Background(), "test-key", "https://litellm.internal.example.com/v1", auxOpenAIOptions{})
	sdk := client.(*auxOpenAIClient)
	ct := keepaliveMounts(t, sdk.httpClient)
	for scheme, rt := range ct.mounts {
		tr, ok := rt.(*http.Transport)
		if !ok || tr.Proxy == nil {
			t.Fatalf("%s mount %T is not proxy backed", scheme, rt)
		}
		u, _ := tr.Proxy(&http.Request{})
		if u == nil || u.String() != "http://127.0.0.1:7897" {
			t.Fatalf("proxy %v", u)
		}
	}
	if sdk.maxRetries != 0 {
		t.Fatal("auxiliary clients turn SDK retries off")
	}
	_ = client.Close()
}

func TestMissingBootstrapHelperDegradesInsteadOfRaising(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	old := auxBuildKeepaliveHTTPClientFn
	auxBuildKeepaliveHTTPClientFn = func(string, bool, TLSVerify) *http.Client { return nil }
	defer func() { auxBuildKeepaliveHTTPClientFn = old }()
	for i := 0; i < 2; i++ {
		if auxOpenAIHTTPClient(context.Background(), "https://api.example.com/v1", false) != nil {
			t.Fatal("a failed keepalive build injects nothing")
		}
	}
	sdk := auxCreateOpenAIClient(context.Background(), "k", "https://api.example.com/v1", auxOpenAIOptions{}).(*auxOpenAIClient)
	if sdk.httpClient != auxSDKDefaultHTTPClient() || sdk.ownsHTTP || sdk.timeout != auxSDKDefaultTimeout {
		t.Fatal("the SDK default client takes over")
	}
}

func TestHealthyBootstrapStillInjectsKeepaliveClient(t *testing.T) {
	keepaliveNoProxyEnv(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	hc := auxOpenAIHTTPClient(context.Background(), "https://api.example.com/v1", false)
	if hc == nil {
		t.Fatal("keepalive client missing")
	}
	keepaliveMounts(t, hc)
	sdk := auxCreateOpenAIClient(context.Background(), "k", "https://api.example.com/v1", auxOpenAIOptions{}).(*auxOpenAIClient)
	if !sdk.ownsHTTP || sdk.hasTimeout {
		t.Fatal("the keepalive client and its unbounded read timeout are injected")
	}
}

// Dispatch and provider response hooks installed by the owner reach the protected worker goroutine
func TestTimingHooksPropagateToProtectedCallWorkerThread(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	record := func(name string) func() error {
		return func() error {
			mu.Lock()
			seen = append(seen, name)
			mu.Unlock()
			return nil
		}
	}
	ctx := AuxThreadLocalHook(context.Background(), AuxDispatchLocal, record("dispatch"))
	ctx = AuxThreadLocalHook(ctx, AuxProviderResponseLocal, record("response"))
	ctx = AuxInterruptProtection(ctx, true, AuxCancelFunc(func() (bool, error) { return false, nil }), nil)
	result, err := auxRunProtectedSyncProviderCall(ctx, func(ctx context.Context, _ map[string]any) (any, error) {
		auxNotifyAuxDispatch(ctx)
		auxNotifyAuxProviderResponse(ctx)
		return "ok", nil
	}, map[string]any{})
	mu.Lock()
	defer mu.Unlock()
	if err != nil || result != "ok" || strings.Join(seen, ",") != "dispatch,response" {
		t.Fatalf("result %v err %v seen %v", result, err, seen)
	}
}
