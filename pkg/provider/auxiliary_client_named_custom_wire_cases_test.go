package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
)

const auxGatewayKeyCmdConfigYAML = "model:\n  provider: icebath-gw\n  default: main-model\n" +
	"providers:\n  icebath-gw:\n    base_url: http://127.0.0.1:1/openai/v1\n    api_mode: chat_completions\n" +
	"    key_cmd: printf vk-test-1234\n    extra_headers:\n      x-gw-session: aux-session-tag\n" +
	"auxiliary:\n  compression:\n    provider: icebath-gw\n    model: aux-model\n"

const auxKeyedGroqConfigYAML = "model:\n  default: vendor/model\n  provider: groq\n" +
	"providers:\n  groq:\n    name: groq\n    api: https://api.groq.com/openai/v1\n    api_key: k\n"

const auxProxyExtraBodyBaseURL = "https://proxy.example/v1"

// Headers the SDK client puts on a chat completions request after its own key refresh
func auxWireHeaders(t *testing.T, client AuxClient) http.Header {
	t.Helper()
	var sdk *auxOpenAIClient
	switch c := client.(type) {
	case *auxOpenAIClient:
		sdk = c
	case *auxAsyncOpenAIClient:
		sdk = c.auxOpenAIClient
	default:
		t.Fatalf("expected an SDK client, got %T", client)
	}
	key, err := sdk.refreshAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, sdk.BaseURL()+"/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	sdk.applyHeaders(req, key, &auxSDKRequest{path: "/chat/completions"}, 0)
	return req.Header
}

func TestBareNamedAuxCredentialSurvivesAsyncRebuildAsyncTaskClientCarriesKeyCmdBearerAndExtraHeaders(t *testing.T) {
	ctx := auxMainTestHome(t, auxGatewayKeyCmdConfigYAML)
	client, _, err := ResolveProviderClient(ctx, "icebath-gw", "aux-model", ProviderClientOptions{AsyncMode: true, Task: "compression"})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		t.Fatal("client is nil")
	}
	headers := auxWireHeaders(t, client)
	if headers.Get("Authorization") != "Bearer vk-test-1234" || headers.Get("x-gw-session") != "aux-session-tag" {
		t.Fatalf("headers %v", headers)
	}
}

func TestBareNamedAuxCredentialSurvivesAsyncRebuildSyncToAsyncRebuildKeepsCredentialAndHeaders(t *testing.T) {
	ctx := auxMainTestHome(t, auxGatewayKeyCmdConfigYAML)
	syncClient, model, err := ResolveProviderClient(ctx, "icebath-gw", "aux-model", ProviderClientOptions{Task: "compression"})
	if err != nil {
		t.Fatal(err)
	}
	if syncClient == nil {
		t.Fatal("client is nil")
	}
	if got := auxWireHeaders(t, syncClient).Get("Authorization"); got != "Bearer vk-test-1234" {
		t.Fatalf("sync authorization %q", got)
	}
	asyncClient, _ := auxToAsyncClient(ctx, syncClient, model, false)
	headers := auxWireHeaders(t, asyncClient)
	if headers.Get("Authorization") != "Bearer vk-test-1234" || headers.Get("x-gw-session") != "aux-session-tag" {
		t.Fatalf("headers %v", headers)
	}
}

// Swaps the response validator for a passthrough the way hermes tests patch _validate_llm_response
func auxPassthroughValidate(t *testing.T) {
	t.Helper()
	prev := auxValidateLLMResponseFn
	t.Cleanup(func() { auxValidateLLMResponseFn = prev })
	auxValidateLLMResponseFn = func(_ context.Context, resp any, _, _, _ string) (any, error) { return resp, nil }
}

func TestKeyedCustomProviderReasoningWireKeyedEntrySendsTopLevelReasoningEffort(t *testing.T) {
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	reasoning := map[string]any{"enabled": true, "effort": "medium"}
	base := "https://api.groq.com/openai/v1"
	for _, provider := range []string{"groq", "main", "custom:groq"} {
		t.Run(provider, func(t *testing.T) {
			ctx := auxMainTestHome(t, auxKeyedGroqConfigYAML)
			var kwargs map[string]any
			if provider == "groq" {
				client := &auxCallFakeClient{base: base, key: "k",
					create: func(context.Context, map[string]any) (any, error) { return auxCallOK("ok"), nil }}
				auxSwapCachedClient(t, client, "vendor/model")
				auxPassthroughValidate(t)
				if _, err := CallLLM(ctx, map[string]any{"provider": provider, "model": "vendor/model", "messages": msgs,
					"reasoning_config": reasoning, "base_url": base}); err != nil {
					t.Fatal(err)
				}
				kwargs = client.sent[len(client.sent)-1]
			} else {
				var err error
				kwargs, err = auxBuildCallKwargs(ctx, provider, "vendor/model", msgs, auxCallKwargsOptions{ReasoningConfig: reasoning, BaseURL: base})
				if err != nil {
					t.Fatal(err)
				}
			}
			if kwargs["reasoning_effort"] != "default" {
				t.Fatalf("kwargs %v", kwargs)
			}
			extra, _ := kwargs["extra_body"].(map[string]any)
			if _, has := extra["reasoning"]; has {
				t.Fatalf("extra_body %v", extra)
			}
		})
	}
}

func TestKeyedCustomProviderReasoningWireProfileBackedAndUnknownProvidersKeepTheirWire(t *testing.T) {
	ctx := auxMainTestHome(t, auxKeyedGroqConfigYAML)
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	reasoning := map[string]any{"enabled": true, "effort": "medium"}
	nested := map[string]any{"reasoning": map[string]any{"enabled": true, "effort": "medium"}}
	check := func(kwargs map[string]any) {
		t.Helper()
		got, _ := json.Marshal(kwargs["extra_body"])
		want, _ := json.Marshal(nested)
		if !bytes.Equal(got, want) {
			t.Fatalf("extra_body %s want %s", got, want)
		}
		if _, has := kwargs["reasoning_effort"]; has {
			t.Fatalf("kwargs %v", kwargs)
		}
	}
	kwargs, err := auxBuildCallKwargs(ctx, "openrouter", "vendor/model", msgs, auxCallKwargsOptions{ReasoningConfig: reasoning, BaseURL: "https://openrouter.ai/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	check(kwargs)
	kwargs, err = auxBuildCallKwargs(ctx, "someunknown", "vendor/model", msgs, auxCallKwargsOptions{ReasoningConfig: reasoning})
	if err != nil {
		t.Fatal(err)
	}
	check(kwargs)
}

// Transport answering every request with one assistant message, the hermes Completions.create patch
type auxOKChatTransport struct{}

func (auxOKChatTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	body, _ := json.Marshal(auxCallOK("ok"))
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
}

// Routes every SDK client at the OK transport and records the create kwargs the SDK receives
func auxRecordCreateKwargs(t *testing.T) *[]map[string]any {
	t.Helper()
	var mu sync.Mutex
	sent := &[]map[string]any{}
	prevHTTP, prevBuild := auxBuildKeepaliveHTTPClientFn, auxSDKBuildRequestFn
	t.Cleanup(func() { auxBuildKeepaliveHTTPClientFn, auxSDKBuildRequestFn = prevHTTP, prevBuild })
	auxBuildKeepaliveHTTPClientFn = func(string, bool, TLSVerify) *http.Client { return &http.Client{Transport: auxOKChatTransport{}} }
	auxSDKBuildRequestFn = func(c *auxOpenAIClient, kwargs map[string]any) (*auxSDKRequest, error) {
		mu.Lock()
		*sent = append(*sent, kwargs)
		mu.Unlock()
		return auxSDKBuildRequest(c, kwargs)
	}
	return sent
}

// Config of the proxy entry carrying extra_body beside a second named entry and the auxiliary section
func auxProxyExtraBodyConfigYAML(auxiliary string) string {
	return "model:\n  default: some-model\n  provider: custom:my-proxy\n" +
		"custom_providers:\n  - name: my-proxy\n    base_url: " + auxProxyExtraBodyBaseURL + "\n    model: some-model\n    api_key: sk-x\n" +
		"    extra_body:\n      user: proxy-user\n      metadata:\n        tier: entry\n" +
		"  - name: other-proxy\n    base_url: https://other.example/v1\n    api_key: sk-y\n    extra_body:\n      user: other-user\n" +
		"auxiliary:" + auxiliary
}

// Extra body the approval call hands the transport for the config, the hermes _sent_extra_body helper
func auxSentExtraBody(t *testing.T, ctx context.Context, mainRuntime map[string]any, call map[string]any) map[string]any {
	t.Helper()
	ShutdownCachedClients(ctx)
	t.Cleanup(func() { ShutdownCachedClients(ctx) })
	sent := auxRecordCreateKwargs(t)
	auxPassthroughValidate(t)
	kwargs := map[string]any{"task": "approval", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if mainRuntime != nil {
		kwargs["main_runtime"] = mainRuntime
	}
	for k, v := range call {
		kwargs[k] = v
	}
	if _, err := CallLLM(ctx, kwargs); err != nil {
		t.Fatal(err)
	}
	if len(*sent) == 0 {
		t.Fatal("request never reached the transport")
	}
	body, _ := (*sent)[len(*sent)-1]["extra_body"].(map[string]any)
	if body == nil {
		body = map[string]any{}
	}
	return body
}

func TestAuxInheritsCustomProviderExtraBodyApprovalWithEmptyTaskExtraBodyInheritsEntryBody(t *testing.T) {
	cases := map[string]map[string]any{
		"config-main":  nil,
		"live-runtime": {"provider": "custom", "model": "some-model", "base_url": auxProxyExtraBodyBaseURL, "api_key": "sk-x"},
	}
	for name, runtime := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := auxMainTestHome(t, auxProxyExtraBodyConfigYAML("\n  approval:\n    extra_body: {}\n"))
			body := auxSentExtraBody(t, ctx, runtime, nil)
			if body["user"] != "proxy-user" {
				t.Fatalf("extra_body %v", body)
			}
		})
	}
}

func TestAuxInheritsCustomProviderExtraBodyTaskAndCallerKeysWinOverEntry(t *testing.T) {
	ctx := auxMainTestHome(t, auxProxyExtraBodyConfigYAML("\n  approval:\n    extra_body:\n      metadata:\n        tier: task\n"))
	body := auxSentExtraBody(t, ctx, nil, map[string]any{"extra_body": map[string]any{"user": "caller-user"}})
	if body["user"] != "caller-user" {
		t.Fatalf("extra_body %v", body)
	}
	got, _ := json.Marshal(body["metadata"])
	if string(got) != `{"tier":"task"}` {
		t.Fatalf("metadata %s", got)
	}
}

func TestAuxInheritsCustomProviderExtraBodyOtherDestinationsDoNotInherit(t *testing.T) {
	ctx := auxMainTestHome(t, auxProxyExtraBodyConfigYAML(" {}\n"))
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	openrouter, err := auxBuildCallKwargs(ctx, "openrouter", "vendor/model", msgs, auxCallKwargsOptions{BaseURL: "https://openrouter.ai/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if extra, _ := openrouter["extra_body"].(map[string]any); extra["user"] != nil {
		t.Fatalf("openrouter extra_body %v", extra)
	}
	other, err := auxBuildCallKwargs(ctx, "custom:other-proxy", "m", msgs, auxCallKwargsOptions{BaseURL: "https://other.example/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	if extra, _ := other["extra_body"].(map[string]any); extra["user"] != "other-user" {
		t.Fatalf("other extra_body %v", other["extra_body"])
	}
	stray, err := auxBuildCallKwargs(ctx, "custom", "some-model", msgs, auxCallKwargsOptions{BaseURL: "https://stray.example/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if extra, _ := stray["extra_body"].(map[string]any); extra["user"] != nil {
		t.Fatalf("stray extra_body %v", extra)
	}
}
