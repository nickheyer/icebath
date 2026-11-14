package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Per client response hook lists standing in for the SDK client event hooks
type fakeResponseHooks struct {
	mu    sync.Mutex
	hooks map[any]map[string]func(*http.Response)
}

func installFakeResponseHooks(t *testing.T) *fakeResponseHooks {
	t.Helper()
	f := &fakeResponseHooks{hooks: map[any]map[string]func(*http.Response){}}
	prev := addClientResponseHook
	addClientResponseHook = func(client any, mark string, hook func(*http.Response)) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.hooks[client] == nil {
			f.hooks[client] = map[string]func(*http.Response){}
		}
		if _, set := f.hooks[client][mark]; !set {
			f.hooks[client][mark] = hook
		}
		return true
	}
	t.Cleanup(func() { addClientResponseHook = prev })
	return f
}

func (f *fakeResponseHooks) fire(client any, status int, header http.Header) {
	f.mu.Lock()
	hooks := f.hooks[client]
	f.mu.Unlock()
	for _, hook := range hooks {
		hook(&http.Response{StatusCode: status, Header: header})
	}
}

// Fake SDK constructor recording every construction with the http client it was handed
type fakeOpenAI struct {
	kwargs     map[string]any
	httpClient any
	closed     bool
}

func (f *fakeOpenAI) Close() error { f.closed = true; return nil }

func installFakeOpenAI(t *testing.T) *[]*fakeOpenAI {
	t.Helper()
	var constructed []*fakeOpenAI
	prev := openAIClientConstructor
	openAIClientConstructor = func(kwargs map[string]any) (any, error) {
		c := &fakeOpenAI{kwargs: kwargs, httpClient: kwargs["http_client"]}
		constructed = append(constructed, c)
		return c, nil
	}
	t.Cleanup(func() { openAIClientConstructor = prev })
	return &constructed
}

func lifecycleAgent(t *testing.T) *AIAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := &AIAgent{provider: "openrouter", model: "test/model", apiKey: "test-key"}
	a.setBaseURL("https://openrouter.ai/api/v1")
	a.clientKwargs = map[string]any{"api_key": "test-key-value", "base_url": "https://api.example.com/v1"}
	return a
}

func TestServedModelHookCapturesLitellmHeaderAndClearsWhenAbsent(t *testing.T) {
	hooks := installFakeResponseHooks(t)
	a := &AIAgent{model: "hermes-router"}
	client := &fakeOpenAI{}
	installServedModelCapture(a, client)
	installServedModelCapture(a, client)
	if n := len(hooks.hooks[client]); n != 1 {
		t.Fatalf("hooks %d", n)
	}
	hooks.fire(client, 200, http.Header{"X-Litellm-Model-Id": {"gpt-4o-2024-11-20"}})
	if got := resultModelFields(a); !reflect.DeepEqual(got, map[string]any{"requested_model": "hermes-router", "served_model": "gpt-4o-2024-11-20"}) {
		t.Fatalf("fields %v", got)
	}
	hooks.fire(client, 500, http.Header{"X-Litellm-Model-Id": {"ignored"}})
	if got := resultModelFields(a)["served_model"]; got != "gpt-4o-2024-11-20" {
		t.Fatalf("error response overwrote %v", got)
	}
	hooks.fire(client, 200, http.Header{})
	if got := resultModelFields(a); !reflect.DeepEqual(got, map[string]any{"requested_model": "hermes-router", "served_model": nil}) {
		t.Fatalf("fields %v", got)
	}
	a.fallbackActivated = true
	a.primaryRuntime = map[string]any{"model": "gpt-5.6-sol"}
	a.model = "qwen/qwen3.8-max"
	if got := resultModelFields(a); !reflect.DeepEqual(got, map[string]any{"requested_model": "gpt-5.6-sol", "served_model": "qwen/qwen3.8-max"}) {
		t.Fatalf("fields %v", got)
	}
	if got := servedModelFromHeaders(http.Header{"X-Litellm-Model-Api-Base": {" https://up "}}); got == nil || *got != "https://up" {
		t.Fatalf("api base header %v", got)
	}
}

// Real SDK client over an httptest proxy records the routing header once per response and clears it when absent
func TestServedModelCaptureOverRealSDKClientRoundTrips(t *testing.T) {
	var servedMu sync.Mutex
	served := "gpt-4o-2024-11-20"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servedMu.Lock()
		value := served
		servedMu.Unlock()
		if value != "" {
			w.Header().Set("x-litellm-model-id", value)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","object":"chat.completion","created":0,"model":"hermes-router","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	var calls sync.Map
	prev := addClientResponseHook
	addClientResponseHook = func(client any, mark string, hook func(*http.Response)) bool {
		return provider.AddOpenAIClientResponseHook(client, mark, func(resp *http.Response) {
			n, _ := calls.LoadOrStore(resp, new(int))
			*n.(*int)++
			hook(resp)
		})
	}
	t.Cleanup(func() { addClientResponseHook = prev })
	zero := 0
	client := provider.NewOpenAIClient("x", srv.URL+"/v1", provider.OpenAIClientOptions{MaxRetries: &zero, HTTPClient: srv.Client()})
	defer client.Close()
	a := &AIAgent{model: "hermes-router"}
	installServedModelCapture(a, client)
	installServedModelCapture(a, client)
	chat := func() {
		t.Helper()
		if _, err := client.CreateChatCompletion(context.Background(), map[string]any{"model": "hermes-router", "messages": []map[string]any{{"role": "user", "content": "hi"}}}); err != nil {
			t.Fatal(err)
		}
	}
	chat()
	if got := resultModelFields(a); !reflect.DeepEqual(got, map[string]any{"requested_model": "hermes-router", "served_model": "gpt-4o-2024-11-20"}) {
		t.Fatalf("fields %v", got)
	}
	servedMu.Lock()
	served = ""
	servedMu.Unlock()
	chat()
	if got := resultModelFields(a); !reflect.DeepEqual(got, map[string]any{"requested_model": "hermes-router", "served_model": nil}) {
		t.Fatalf("fields %v", got)
	}
	responses := 0
	calls.Range(func(_, n any) bool {
		responses++
		if *n.(*int) != 1 {
			t.Fatalf("hook ran %d times for one response", *n.(*int))
		}
		return true
	})
	if responses != 2 {
		t.Fatalf("hooked responses %d", responses)
	}
	a.fallbackActivated = true
	a.primaryRuntime = map[string]any{"model": "gpt-5.6-sol"}
	a.model = "qwen/qwen3.8-max"
	if got := resultModelFields(a); !reflect.DeepEqual(got, map[string]any{"requested_model": "gpt-5.6-sol", "served_model": "qwen/qwen3.8-max"}) {
		t.Fatalf("fields %v", got)
	}
}

func TestSecondCreateDoesNotWrapClosedTransportFromFirst(t *testing.T) {
	installFakeResponseHooks(t)
	constructed := installFakeOpenAI(t)
	a := lifecycleAgent(t)
	ctx := context.Background()
	clientA, err := a.createOpenAIClient(ctx, a.clientKwargs, "initial", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = clientA.(*fakeOpenAI).Close()
	clientB, err := a.createOpenAIClient(ctx, a.clientKwargs, "rebuild", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(*constructed) != 2 || (*constructed)[0] != clientA || (*constructed)[1] != clientB {
		t.Fatalf("constructions %d", len(*constructed))
	}
	hcA, hcB := (*constructed)[0].httpClient, (*constructed)[1].httpClient
	if hcA != nil && hcB != nil && hcA == hcB {
		t.Fatal("same http_client handed to two sequential constructions")
	}
	if _, leaked := a.clientKwargs["http_client"]; leaked {
		t.Fatal("http_client injected into the stored kwargs")
	}
	if (*constructed)[1].kwargs["max_retries"] != 0 {
		t.Fatalf("max_retries %v", (*constructed)[1].kwargs["max_retries"])
	}
}

func TestReplacePrimaryOpenAIClientSurvivesRepeatedRebuilds(t *testing.T) {
	installFakeResponseHooks(t)
	constructed := installFakeOpenAI(t)
	a := lifecycleAgent(t)
	prevSweep := forceCloseTCPSockets
	forceCloseTCPSockets = func(any) int { return 0 }
	t.Cleanup(func() { forceCloseTCPSockets = prevSweep })
	seed, err := a.createOpenAIClient(context.Background(), a.clientKwargs, "seed", true)
	if err != nil {
		t.Fatal(err)
	}
	a.client = seed
	for _, label := range []string{"rebuild_1", "rebuild_2", "rebuild_3"} {
		if !a.replacePrimaryOpenAIClient(label) {
			t.Fatalf("rebuild %s returned false", label)
		}
	}
	if len(*constructed) != 4 || a.client != (*constructed)[3] {
		t.Fatalf("constructions %d", len(*constructed))
	}
	seen := map[any]bool{}
	for _, c := range *constructed {
		if c.closed {
			t.Fatal("retired shared client was hard closed")
		}
		if c.httpClient != nil {
			if seen[c.httpClient] {
				t.Fatal("http_client reused across rebuilds")
			}
			seen[c.httpClient] = true
		}
	}
}

func TestGeminiAliasesPreserveNativeEndpointSelection(t *testing.T) {
	installFakeResponseHooks(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	profile, ok := provider.GetProviderProfile(ctx, "gemini")
	if !ok {
		t.Fatal("gemini profile missing")
	}
	for _, name := range append([]string{profile.Name}, profile.Aliases...) {
		for base, want := range map[string]string{"https://generativelanguage.googleapis.com/v1beta": "*provider.geminiNativeClient", "https://example.invalid/v1": "*provider.auxOpenAIClient"} {
			a := &AIAgent{provider: name, model: "gemini-flash-latest"}
			client, err := a.createOpenAIClient(ctx, map[string]any{"api_key": "test-key", "base_url": base}, "fallback", false)
			if err != nil {
				t.Fatalf("%s %s: %v", name, base, err)
			}
			if got := fmt.Sprintf("%T", client); got != want {
				t.Fatalf("%s %s: got %s want %s", name, base, got, want)
			}
			closeClientQuietly(client)
		}
	}
}

type closedFlag struct{ closed bool }

func (c closedFlag) IsClosed() bool { return c.closed }

type duckClient struct {
	outer *bool
	inner any
}

func (d duckClient) IsClosed() bool  { return d.outer != nil && *d.outer }
func (d duckClient) HTTPClient() any { return d.inner }

type innerOnlyClient struct{ inner any }

func (d innerOnlyClient) HTTPClient() any { return d.inner }

func TestIsOpenAIClientClosedTruthTable(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		client any
		want   bool
	}{
		{closedFlag{true}, true},
		{closedFlag{false}, false},
		{duckClient{&yes, closedFlag{false}}, true},
		{duckClient{&no, closedFlag{true}}, true},
		{duckClient{&no, closedFlag{false}}, false},
		{duckClient{nil, closedFlag{true}}, true},
		{duckClient{nil, nil}, false},
		{innerOnlyClient{closedFlag{true}}, true},
		{struct{}{}, false},
		{nil, false},
	}
	for i, c := range cases {
		if got := isOpenAIClientClosed(c.client); got != c.want {
			t.Fatalf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

type cancelClient struct{ cancel func() }

func (c cancelClient) Cancel() { c.cancel() }

func TestCrossGoroutineCancelStopsRealProviderProcess(t *testing.T) {
	proc := exec.Command("sleep", "60")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Process.Kill(); _ = proc.Wait() }()
	sweeps := 0
	prev := forceCloseTCPSockets
	forceCloseTCPSockets = func(any) int { sweeps++; return 0 }
	t.Cleanup(func() { forceCloseTCPSockets = prev })
	a := &AIAgent{provider: "test"}
	done := make(chan struct{})
	go func() {
		a.abortRequestOpenAIClient(cancelClient{func() { _ = proc.Process.Signal(syscall.SIGTERM) }}, "interrupt_abort")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("abort did not return")
	}
	exited := make(chan error, 1)
	go func() { exited <- proc.Wait() }()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("provider process still running")
	}
	if sweeps != 0 {
		t.Fatalf("socket sweeps %d", sweeps)
	}
}

func TestClientsWithoutCancelGoThroughTheSocketSweep(t *testing.T) {
	sweeps := 0
	prev := forceCloseTCPSockets
	forceCloseTCPSockets = func(any) int { sweeps++; return 0 }
	t.Cleanup(func() { forceCloseTCPSockets = prev })
	(&AIAgent{provider: "test"}).abortRequestOpenAIClient(&fakeOpenAI{}, "interrupt_abort")
	if sweeps != 1 {
		t.Fatalf("socket sweeps %d", sweeps)
	}
}

func TestRequestSlotReusesCleanFinishAndRebuildsAfterAbort(t *testing.T) {
	installFakeResponseHooks(t)
	constructed := installFakeOpenAI(t)
	prev := forceCloseTCPSockets
	forceCloseTCPSockets = func(any) int { return 1 }
	t.Cleanup(func() { forceCloseTCPSockets = prev })
	a := lifecycleAgent(t)
	a.client = &fakeOpenAI{}
	first, err := a.CreateRequestOpenAIClient("chat_completion_request", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.CloseRequestOpenAIClient(first, "request_complete")
	second, _ := a.CreateRequestOpenAIClient("chat_completion_request", nil)
	if second != first || len(*constructed) != 1 {
		t.Fatalf("clean finish not reused, constructions %d", len(*constructed))
	}
	_ = a.AbortRequestOpenAIClient(second, "interrupt_abort")
	_ = a.CloseRequestOpenAIClient(second, "interrupt_abort")
	third, _ := a.CreateRequestOpenAIClient("chat_completion_request", map[string]any{"messages": []any{map[string]any{"content": []any{map[string]any{"type": "image_url"}}}}})
	if third == second || len(*constructed) != 2 || !first.(*fakeOpenAI).closed {
		t.Fatalf("poisoned client reused or left open, constructions %d", len(*constructed))
	}
	a.closeCachedRequestOpenAIClient("agent_close")
	if a.requestClientCache["in_use"] != false || a.requestClientCache["client"] != nil {
		t.Fatalf("slot %v", a.requestClientCache)
	}
}

func TestRouteIdentityHeaders(t *testing.T) {
	h, err := routermintHeaders()
	if err != nil || !strings.HasPrefix(h["User-Agent"], "IcebathAgent/") {
		t.Fatalf("routermint %v %v", h, err)
	}
	q := qwenPortalHeaders()
	if !strings.HasPrefix(q["User-Agent"], "QwenCode/0.14.1 (") || q["X-DashScope-UserAgent"] != q["User-Agent"] || q["X-DashScope-AuthType"] != "qwen-oauth" || q["X-DashScope-CacheControl"] != "enable" {
		t.Fatalf("qwen %v", q)
	}
	if !apiKwargsHaveImageParts(map[string]any{"input": []any{map[string]any{"content": []any{map[string]any{"type": "input_image"}}}}}) || apiKwargsHaveImageParts(map[string]any{"messages": []any{map[string]any{"type": "text"}}}) {
		t.Fatal("image part detection")
	}
}
