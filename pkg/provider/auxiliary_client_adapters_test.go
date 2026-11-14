package provider

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type auxFakeEventStream struct {
	events []map[string]any
	next   int
	closed bool
}

func (s *auxFakeEventStream) Next(ctx context.Context) (map[string]any, error) {
	if s.next >= len(s.events) {
		return nil, io.EOF
	}
	s.next++
	return s.events[s.next-1], nil
}

func (s *auxFakeEventStream) Close() error {
	s.closed = true
	return nil
}

type auxFakeResponsesClient struct {
	base     string
	captured map[string]any
	stream   *auxFakeEventStream
}

func (f *auxFakeResponsesClient) BaseURL() string { return f.base }
func (f *auxFakeResponsesClient) APIKey() string  { return "codex-key" }
func (f *auxFakeResponsesClient) Close() error    { return nil }
func (f *auxFakeResponsesClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	return nil, errors.New("chat completions unused")
}

// Captures the effective wire body with extra_body folded in and streams one completed assistant message
func (f *auxFakeResponsesClient) CreateResponse(ctx context.Context, kwargs map[string]any) (any, error) {
	f.captured = map[string]any{}
	for k, v := range kwargs {
		f.captured[k] = v
	}
	if extra, ok := kwargs["extra_body"].(map[string]any); ok {
		for k, v := range extra {
			f.captured[k] = v
		}
	}
	f.stream = &auxFakeEventStream{events: []map[string]any{
		{"type": "response.created"},
		{"type": "response.output_item.done", "item": map[string]any{"type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "summary text"}}}},
		{"type": "response.completed", "response": map[string]any{"status": "completed", "id": "resp_test",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
	}}
	return f.stream, nil
}

func newAuxCodexTestAdapter(baseURL, model string) (*auxCodexCompletionsAdapter, *auxFakeResponsesClient) {
	client := &auxFakeResponsesClient{base: baseURL}
	return &auxCodexCompletionsAdapter{client: client, model: model}, client
}

func auxUserHi() []any { return []any{map[string]any{"role": "user", "content": "hi"}} }

func TestCodexAdapterReasoningTranslation(t *testing.T) {
	ctx := context.Background()
	t.Run("test_reasoning_effort_low_passed_through", func(t *testing.T) {
		adapter, client := newAuxCodexTestAdapter("https://chatgpt.com/backend-api/codex/", "gpt-5.3-codex")
		if _, err := adapter.create(ctx, map[string]any{"messages": auxUserHi(), "extra_body": map[string]any{"reasoning": map[string]any{"effort": "low"}}}); err != nil {
			t.Fatal(err)
		}
		if got := client.captured["reasoning"]; !reflect.DeepEqual(got, map[string]any{"effort": "low", "summary": "auto"}) {
			t.Fatalf("low effort %v", got)
		}
	})
	t.Run("test_disabled_reasoning_is_sent_as_none_and_chat_era_models_get_no_field", func(t *testing.T) {
		adapter, client := newAuxCodexTestAdapter("https://api.openai.com/v1/", "gpt-5.3-codex")
		if _, err := adapter.create(ctx, map[string]any{"messages": auxUserHi(), "extra_body": map[string]any{"reasoning": map[string]any{"enabled": false}}}); err != nil {
			t.Fatal(err)
		}
		if got := client.captured["reasoning"]; !reflect.DeepEqual(got, map[string]any{"effort": "none"}) {
			t.Fatalf("disabled reasoning %v", got)
		}
		if _, has := client.captured["include"]; has {
			t.Fatal("disabled reasoning sent include")
		}
		adapter, client = newAuxCodexTestAdapter("https://api.openai.com/v1/", "gpt-4o-mini")
		if _, err := adapter.create(ctx, map[string]any{"model": "gpt-4o-mini", "messages": auxUserHi(), "extra_body": map[string]any{"reasoning": map[string]any{"effort": "medium"}}}); err != nil {
			t.Fatal(err)
		}
		if _, has := client.captured["reasoning"]; has {
			t.Fatal("chat era model got a reasoning field")
		}
	})
	t.Run("test_no_extra_body_means_no_reasoning_keys", func(t *testing.T) {
		adapter, client := newAuxCodexTestAdapter("https://chatgpt.com/backend-api/codex/", "gpt-5.3-codex")
		if _, err := adapter.create(ctx, map[string]any{"messages": auxUserHi()}); err != nil {
			t.Fatal(err)
		}
		_, hasReasoning := client.captured["reasoning"]
		_, hasInclude := client.captured["include"]
		if hasReasoning || hasInclude {
			t.Fatalf("no extra_body sent reasoning keys %v", client.captured)
		}
	})
	t.Run("test_reasoning_effort_null_falls_back_to_medium", func(t *testing.T) {
		adapter, client := newAuxCodexTestAdapter("https://chatgpt.com/backend-api/codex/", "gpt-5.3-codex")
		if _, err := adapter.create(ctx, map[string]any{"messages": auxUserHi(), "extra_body": map[string]any{"reasoning": map[string]any{"effort": nil}}}); err != nil {
			t.Fatal(err)
		}
		if got := client.captured["reasoning"]; !reflect.DeepEqual(got, map[string]any{"effort": "medium", "summary": "auto"}) {
			t.Fatalf("null effort %v", got)
		}
		if got := client.captured["include"]; !reflect.DeepEqual(got, []any{"reasoning.encrypted_content"}) {
			t.Fatalf("null effort include %v", got)
		}
	})
}

func TestCodexAdapterServiceTier(t *testing.T) {
	ctx := context.Background()
	adapter, client := newAuxCodexTestAdapter("https://chatgpt.com/backend-api/codex", "gpt-5.6-luna")
	if _, err := adapter.create(ctx, map[string]any{"messages": auxUserHi(), "extra_body": map[string]any{"service_tier": "priority"}}); err != nil {
		t.Fatal(err)
	}
	if client.captured["service_tier"] != "priority" {
		t.Fatalf("codex backend tier %v", client.captured["service_tier"])
	}
	adapter, client = newAuxCodexTestAdapter("https://api.x.ai/v1", "grok-4.6")
	if _, err := adapter.create(ctx, map[string]any{"messages": auxUserHi(), "extra_body": map[string]any{"service_tier": "priority"}}); err != nil {
		t.Fatal(err)
	}
	if _, has := client.captured["service_tier"]; has {
		t.Fatal("xai kept service_tier")
	}
}

func TestCodexAdapterShapesChatCompletion(t *testing.T) {
	adapter, client := newAuxCodexTestAdapter("https://chatgpt.com/backend-api/codex/", "gpt-5.3-codex")
	out, err := adapter.create(context.Background(), map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "be terse"}, map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if client.captured["instructions"] != "be terse" || client.captured["stream"] != true || client.captured["store"] != false {
		t.Fatalf("wire %v", client.captured)
	}
	if _, leaked := client.captured["_issuer_kind"]; leaked {
		t.Fatal("private issuer key reached the wire")
	}
	if !client.stream.closed {
		t.Fatal("attempt stream was not released")
	}
	resp := out.(map[string]any)
	choice := resp["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "summary text" || message["role"] != "assistant" || message["tool_calls"] != nil || choice["finish_reason"] != "stop" {
		t.Fatalf("shaped %v", resp)
	}
	if usage := resp["usage"].(map[string]any); usage["prompt_tokens"] != 1 || usage["total_tokens"] != 2 {
		t.Fatalf("usage %v", usage)
	}
}

func TestTranslateAnthropicResponseFormat(t *testing.T) {
	kwargs := map[string]any{}
	schema := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	auxTranslateAnthropicResponseFormat(kwargs, map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x", "schema": schema}})
	if want := map[string]any{"format": map[string]any{"type": "json_schema", "schema": schema}}; !reflect.DeepEqual(kwargs["output_config"], want) {
		t.Fatalf("json_schema %v", kwargs)
	}
	kwargs = map[string]any{"output_config": map[string]any{"effort": "high"}}
	auxTranslateAnthropicResponseFormat(kwargs, map[string]any{"type": "json_object"})
	if want := map[string]any{"effort": "high", "format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}}; !reflect.DeepEqual(kwargs["output_config"], want) {
		t.Fatalf("json_object %v", kwargs)
	}
	for _, rf := range []any{nil, "json", map[string]any{"type": "text"}, map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x"}}} {
		kwargs = map[string]any{}
		auxTranslateAnthropicResponseFormat(kwargs, rf)
		if len(kwargs) != 0 {
			t.Fatalf("%v translated to %v", rf, kwargs)
		}
	}
}

func TestEndpointSpeaksAnthropicMessages(t *testing.T) {
	for url, want := range map[string]bool{
		"https://api.minimax.io/anthropic":    true,
		"https://open.bigmodel.cn/anthropic/": true,
		"https://proxy.example/anthropic/v1":  true,
		"https://api.anthropic.com":           true,
		"https://api.kimi.com/coding/v1":      true,
		"https://api.kimi.com/v1":             false,
		"https://openrouter.ai/api/v1":        false,
		"":                                    false,
	} {
		got, err := auxEndpointSpeaksAnthropicMessages(url)
		if err != nil || got != want {
			t.Fatalf("%q got %v %v", url, got, err)
		}
	}
}

func TestMaybeWrapAnthropicLeavesNonMessagesClients(t *testing.T) {
	ctx := context.Background()
	plain := &auxFakeResponsesClient{base: "https://openrouter.ai/api/v1"}
	for _, tc := range []struct {
		client  AuxClient
		base    string
		apiMode string
	}{
		{plain, "https://openrouter.ai/api/v1", ""},
		{plain, "https://api.minimax.io/anthropic", "chat_completions"},
		{&auxProbeClient{}, "https://api.anthropic.com", "anthropic_messages"},
		{NewBedrockAuxiliaryClient("us-east-1", "m"), "https://api.anthropic.com", ""},
		{NewCodexAuxiliaryClient(plain, "m"), "https://api.anthropic.com", "anthropic_messages"},
	} {
		got, err := auxMaybeWrapAnthropic(ctx, tc.client, "m", "k", tc.base, tc.apiMode)
		if err != nil || got != tc.client {
			t.Fatalf("%T %q %q rewrapped to %T %v", tc.client, tc.base, tc.apiMode, got, err)
		}
	}
}

func TestBedrockAuxiliaryClientIdentity(t *testing.T) {
	c := NewBedrockAuxiliaryClient("eu-west-1", "anthropic.claude")
	if c.BaseURL() != "https://bedrock-runtime.eu-west-1.amazonaws.com" || c.APIKey() != "aws-sdk" || c.Close() != nil {
		t.Fatalf("identity %q %q", c.BaseURL(), c.APIKey())
	}
	async := NewAsyncBedrockAuxiliaryClient(c)
	if async.BaseURL() != c.BaseURL() || async.APIKey() != "aws-sdk" {
		t.Fatal("async twin lost the identity")
	}
}

func TestScopedRuntimeMainNormalizesAndFeedsCacheScope(t *testing.T) {
	keyFn := func() (string, error) { return "minted", nil }
	ctx, runtime := ScopedRuntimeMain(context.Background(), map[string]any{
		"provider": " OpenAI-Codex ", "requested_provider": "Codex", "model": " gpt-5.3-codex ", "api_key": keyFn,
		"session_id": "sess-1", "cache_scope": "  ", "base_url": 7, "unknown": "x",
	})
	if runtime["provider"] != "openai-codex" || runtime["requested_provider"] != "codex" || runtime["model"] != "gpt-5.3-codex" ||
		runtime["session_id"] != "sess-1" || len(runtime) != 5 {
		t.Fatalf("normalized %v", runtime)
	}
	if auxRuntimeMainValue(ctx, "cache_scope") != "" || auxRuntimeMainValue(ctx, "session_id") != "sess-1" {
		t.Fatal("runtime value reads")
	}
	adapter, client := newAuxCodexTestAdapter("https://chatgpt.com/backend-api/codex/", "gpt-5.3-codex")
	if _, err := adapter.create(ctx, map[string]any{"messages": auxUserHi()}); err != nil {
		t.Fatal(err)
	}
	want := contentCacheKey("You are a helpful assistant.", nil, cacheScopeFromSessionID("sess-1"))
	if got, has := client.captured["prompt_cache_key"]; want != "" && (!has || got != want) {
		t.Fatalf("prompt_cache_key %v want %q", got, want)
	}
	cleared := ClearRuntimeMain(ctx)
	if auxRuntimeMain(cleared) != nil || auxRuntimeMainValue(cleared, "session_id") != "" {
		t.Fatal("clear kept the binding")
	}
	if _, empty := ScopedRuntimeMain(context.Background(), nil); len(empty) != 0 {
		t.Fatalf("unbound scoped runtime %v", empty)
	}
}

// Responses client whose create flips the host cancel, outlives the watchdog, then surfaces the explicit cancellation
type auxCancellingResponsesClient struct {
	auxFakeResponsesClient
	cancelled *atomic.Bool
	hold      time.Duration
}

func (c *auxCancellingResponsesClient) CreateResponse(ctx context.Context, kwargs map[string]any) (any, error) {
	c.cancelled.Store(true)
	time.Sleep(c.hold)
	return nil, &AuxiliaryExplicitCancellation{}
}

func TestCodexAdapterExplicitCancellationIsNeverATimeout(t *testing.T) {
	var cancelled atomic.Bool
	check := AuxCancelFunc(func() (bool, error) { return cancelled.Load(), nil })
	ctx := AuxInterruptProtection(context.Background(), true, check, nil)
	client := &auxCancellingResponsesClient{auxFakeResponsesClient: auxFakeResponsesClient{base: "https://chatgpt.com/backend-api/codex/"},
		cancelled: &cancelled, hold: 300 * time.Millisecond}
	adapter := &auxCodexCompletionsAdapter{client: client, model: "gpt-5.3-codex"}
	_, err := adapter.create(ctx, map[string]any{"messages": auxUserHi(), "timeout": 0.05})
	var explicit *AuxiliaryExplicitCancellation
	var timeout *AuxTimeoutError
	if !errors.As(err, &explicit) || errors.As(err, &timeout) {
		t.Fatalf("explicit cancel after the watchdog fired surfaced as %T %v", err, err)
	}
	cancelled.Store(true)
	_, err = adapter.create(ctx, map[string]any{"messages": auxUserHi()})
	if !errors.As(err, &explicit) {
		t.Fatalf("cancel before the request surfaced as %T %v", err, err)
	}
}

func TestAuxKwargDictsFollowsPythonIteration(t *testing.T) {
	var attr *auxAttributeError
	var typeErr *auxTypeError
	if _, err := auxKwargDicts(map[string]any{"messages": []any{"hi"}}, "messages", false); !errors.As(err, &attr) || err.Error() != "'str' object has no attribute 'get'" {
		t.Fatalf("non dict item %v", err)
	}
	if _, err := auxKwargDicts(map[string]any{"messages": nil}, "messages", false); !errors.As(err, &typeErr) || err.Error() != "'NoneType' object is not iterable" {
		t.Fatalf("None messages %v", err)
	}
	if got, err := auxKwargDicts(map[string]any{"tools": nil}, "tools", true); got != nil || err != nil {
		t.Fatalf("None tools %v %v", got, err)
	}
	if got, err := auxKwargDicts(map[string]any{}, "messages", false); len(got) != 0 || err != nil {
		t.Fatalf("absent messages %v %v", got, err)
	}
	adapter, _ := newAuxCodexTestAdapter("https://chatgpt.com/backend-api/codex/", "gpt-5.3-codex")
	if _, err := adapter.create(context.Background(), map[string]any{"messages": []any{"hi"}}); !errors.As(err, &attr) {
		t.Fatalf("codex adapter non dict message %v", err)
	}
}
