package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/nickheyer/icebath/pkg/home"
)

const auxAttributionCodexURL = "https://chatgpt.com/backend-api/codex"

// One request the attribution wire captured with its decoded JSON body
type auxWireRequest struct {
	req  *http.Request
	body map[string]any
}

// Transport double answering every Responses request with the attribution fixture, streamed as SSE when asked
type auxAttributionWire struct {
	mu       sync.Mutex
	requests []auxWireRequest
}

func auxAttributionResponse() map[string]any {
	return map[string]any{"id": "resp_attribution_test", "object": "response", "created_at": 0, "status": "completed", "model": "gpt-5.4",
		"output": []any{map[string]any{"type": "message", "id": "msg_test", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "ok", "annotations": []any{}}}}}}
}

// Request body bytes with a zstd encoded Codex body decoded
func auxWireBody(req *http.Request) []byte {
	if req.Body == nil {
		return nil
	}
	data, _ := io.ReadAll(req.Body)
	if req.Header.Get("Content-Encoding") == "zstd" {
		dec, err := zstd.NewReader(bytes.NewReader(data))
		if err == nil {
			data, _ = io.ReadAll(dec)
			dec.Close()
		}
	}
	return data
}

func (w *auxAttributionWire) RoundTrip(req *http.Request) (*http.Response, error) {
	var body map[string]any
	_ = json.Unmarshal(auxWireBody(req), &body)
	w.mu.Lock()
	w.requests = append(w.requests, auxWireRequest{req: req, body: body})
	w.mu.Unlock()
	response := auxAttributionResponse()
	if body["stream"] == true {
		var sb strings.Builder
		for _, event := range []map[string]any{
			{"type": "response.output_item.done", "output_index": 0, "item": response["output"].([]any)[0]},
			{"type": "response.completed", "response": response},
		} {
			data, _ := json.Marshal(event)
			fmt.Fprintf(&sb, "data: %s\n\n", data)
		}
		sb.WriteString("data: [DONE]\n\n")
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(sb.String())), Request: req}, nil
	}
	data, _ := json.Marshal(response)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(data)), Request: req}, nil
}

func (w *auxAttributionWire) last(t *testing.T) auxWireRequest {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.requests) == 0 {
		t.Fatal("no request reached the wire")
	}
	return w.requests[len(w.requests)-1]
}

// Replaces only the HTTP transport of every auxiliary OpenAI client built during the test
func auxInstallAttributionWire(t *testing.T) *auxAttributionWire {
	t.Helper()
	w := &auxAttributionWire{}
	prev := auxBuildKeepaliveHTTPClientFn
	auxBuildKeepaliveHTTPClientFn = func(string, bool, TLSVerify) *http.Client { return &http.Client{Transport: w} }
	t.Cleanup(func() { auxBuildKeepaliveHTTPClientFn = prev })
	return w
}

// Asserts the icebath identity headers and account id of one captured request
func auxAssertWireIdentity(t *testing.T, r auxWireRequest, accountID string) {
	t.Helper()
	h := r.req.Header
	if h.Get("originator") != "icebath-agent" || h.Get("User-Agent") != "IcebathAgent/"+Version || h.Get("ChatGPT-Account-ID") != accountID {
		t.Fatalf("identity headers %v", h)
	}
	if _, leaked := r.body["extra_headers"]; leaked {
		t.Fatalf("extra_headers leaked into the body %v", r.body)
	}
}

func auxAttributionJWT(accountID string) string { return auxMakeCodexJWT(accountID, "", "") }

// Responses create through the raw Codex client
func auxWireResponsesCreate(t *testing.T, ctx context.Context, client AuxClient, model string) {
	t.Helper()
	responses, ok := client.(auxResponsesClient)
	if !ok {
		t.Fatalf("client %T has no responses surface", client)
	}
	if _, err := responses.CreateResponse(ctx, map[string]any{"model": model, "input": "test"}); err != nil {
		t.Fatal(err)
	}
}

// Chat completion through the wrapped Codex client asserting the ok answer
func auxWireChatCreate(t *testing.T, ctx context.Context, client AuxClient, model string) {
	t.Helper()
	result, err := client.CreateChatCompletion(ctx, map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "test"}}})
	if err != nil || ExtractContentOrReasoning(result, -1) != "ok" {
		t.Fatalf("result %v err %v", result, err)
	}
}

func TestNewIdentityIsLimitedToTheOfficialEndpoint(t *testing.T) {
	cases := []struct {
		baseURL    string
		attributed bool
	}{
		{auxAttributionCodexURL, true},
		{auxAttributionCodexURL + "/", true},
		{auxAttributionCodexURL + "/responses", true},
		{"https://CHATGPT.COM:443/backend-api/codex", true},
		{"http://chatgpt.com/backend-api/codex", false},
		{"https://chatgpt.com:8443/backend-api/codex", false},
		{"https://api.openai.com/v1", false},
		{"https://proxy.example/backend-api/codex", false},
		{"https://chatgpt.com.example/backend-api/codex", false},
		{"https://subdomain.chatgpt.com/backend-api/codex", false},
		{"https://chatgpt.com/backend-api/codex-other", false},
		{"https://chatgpt.com/backend-api/other", false},
		{"https://chatgpt.com:invalid/backend-api/codex", false},
	}
	for _, c := range cases {
		headers := codexCloudflareHeaders(auxAttributionJWT("acct-attribution-test"), c.baseURL)
		wantOriginator, wantAgent := "codex_cli_rs", "codex_cli_rs/0.0.0 (Icebath Agent)"
		if c.attributed {
			wantOriginator, wantAgent = "icebath-agent", "IcebathAgent/"+Version
		}
		if headers["originator"] != wantOriginator || headers["User-Agent"] != wantAgent {
			t.Fatalf("%s: headers %v", c.baseURL, headers)
		}
	}
}

func TestAuxiliaryRawAndAsyncClientsSendExpectedHeaders(t *testing.T) {
	ctx := auxCodexSingletonHome(t, auxAttributionJWT("acct-attribution-test"))
	wire := auxInstallAttributionWire(t)
	wrapped, model, err := auxBuildCodexClient(ctx, "gpt-5.4")
	if err != nil || wrapped == nil {
		t.Fatalf("wrapped %v %v", wrapped, err)
	}
	defer wrapped.Close()
	raw, rawModel, err := ResolveProviderClient(ctx, "openai-codex", "gpt-5.4", ProviderClientOptions{RawCodex: true})
	if err != nil || raw == nil {
		t.Fatalf("raw %v %v", raw, err)
	}
	defer raw.Close()
	auxWireChatCreate(t, ctx, wrapped, model)
	auxAssertWireIdentity(t, wire.last(t), "acct-attribution-test")
	auxWireResponsesCreate(t, ctx, raw, rawModel)
	auxAssertWireIdentity(t, wire.last(t), "acct-attribution-test")
	asyncWrapped, _ := auxToAsyncClient(ctx, wrapped, model, false)
	auxWireChatCreate(t, ctx, asyncWrapped, model)
	auxAssertWireIdentity(t, wire.last(t), "acct-attribution-test")
	asyncRaw, _ := auxToAsyncClient(ctx, raw, rawModel, false)
	defer asyncRaw.Close()
	auxWireResponsesCreate(t, ctx, asyncRaw, rawModel)
	auxAssertWireIdentity(t, wire.last(t), "acct-attribution-test")
}

func TestCredentialPoolCustomEndpointKeepsExistingIdentity(t *testing.T) {
	ctx := auxMainTestHome(t, "")
	wire := auxInstallAttributionWire(t)
	entry := &PooledCredential{Provider: "openai-codex", AccessToken: auxAttributionJWT("acct-attribution-test"), BaseURL: "https://proxy.example/backend-api/codex"}
	prev := auxSelectPoolEntryFn
	auxSelectPoolEntryFn = func(context.Context, string) (bool, *PooledCredential) { return true, entry }
	t.Cleanup(func() { auxSelectPoolEntryFn = prev })
	client, model, err := auxBuildCodexClient(ctx, "gpt-5.4")
	if err != nil || client == nil {
		t.Fatalf("client %v %v", client, err)
	}
	defer client.Close()
	auxWireChatCreate(t, ctx, client, model)
	last := wire.last(t)
	h := last.req.Header
	if last.req.URL.Host != "proxy.example" || h.Get("originator") != "codex_cli_rs" || h.Get("User-Agent") != "codex_cli_rs/0.0.0 (Icebath Agent)" ||
		h.Get("ChatGPT-Account-ID") != "acct-attribution-test" {
		t.Fatalf("host %q headers %v", last.req.URL.Host, h)
	}
}

func TestLegacyDisabledSettingCannotDisableAttributionForNewClients(t *testing.T) {
	ctx := auxCodexSingletonHome(t, auxAttributionJWT("acct-attribution-test"))
	wire := auxInstallAttributionWire(t)
	legacy := func(enabled bool) {
		writeConfig(t, home.Dir(ctx), map[string]any{"telemetry": map[string]any{"usage_attribution": map[string]any{"enabled": enabled}}})
	}
	legacy(true)
	old, _, err := ResolveProviderClient(ctx, "openai-codex", "gpt-5.4", ProviderClientOptions{RawCodex: true})
	if err != nil || old == nil {
		t.Fatalf("old %v %v", old, err)
	}
	defer old.Close()
	legacy(false)
	fresh, _, err := ResolveProviderClient(ctx, "openai-codex", "gpt-5.4", ProviderClientOptions{RawCodex: true})
	if err != nil || fresh == nil {
		t.Fatalf("new %v %v", fresh, err)
	}
	defer fresh.Close()
	auxWireResponsesCreate(t, ctx, old, "gpt-5.4")
	auxAssertWireIdentity(t, wire.last(t), "acct-attribution-test")
	auxWireResponsesCreate(t, ctx, fresh, "gpt-5.4")
	auxAssertWireIdentity(t, wire.last(t), "acct-attribution-test")
}
