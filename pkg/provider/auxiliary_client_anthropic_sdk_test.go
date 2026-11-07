package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Runs the adapter with the Messages create captured and returns the api kwargs it built
func auxCaptureAnthropicKwargs(t *testing.T, extraBody map[string]any, asyncCall bool, topLevel map[string]any) map[string]any {
	t.Helper()
	captured := map[string]any{}
	old := auxCreateAnthropicMessageFn
	t.Cleanup(func() { auxCreateAnthropicMessageFn = old })
	auxCreateAnthropicMessageFn = func(_ context.Context, _ *anthropicSDKClient, apiKwargs map[string]any, _ func(any) error) (map[string]any, error) {
		for k, v := range apiKwargs {
			captured[k] = v
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}, "stop_reason": "end_turn"}, nil
	}
	kwargs := map[string]any{"model": "claude-sonnet-4-6", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": 64}
	for k, v := range topLevel {
		kwargs[k] = v
	}
	if extraBody != nil {
		kwargs["extra_body"] = extraBody
	}
	sync := newAnthropicAuxiliaryClientFrom(&anthropicSDKClient{}, "claude-sonnet-4-6", "sk-ant-test", "", false)
	var client AuxClient = sync
	if asyncCall {
		client = NewAsyncAnthropicAuxiliaryClient(sync)
	}
	if _, err := client.CreateChatCompletion(context.Background(), kwargs); err != nil {
		t.Fatal(err)
	}
	if _, ok := captured["response_format"]; ok {
		t.Fatalf("raw response_format leaked: %v", captured)
	}
	if eb, ok := captured["extra_body"].(map[string]any); ok {
		if _, leaked := eb["response_format"]; leaked {
			t.Fatalf("raw response_format leaked into extra_body: %v", eb)
		}
	}
	return captured
}

// Output format of captured api kwargs
func auxOutputFormat(kwargs map[string]any) any {
	cfg, _ := kwargs["output_config"].(map[string]any)
	return cfg["format"]
}

func TestAnthropicStructuredOutputTranslation(t *testing.T) {
	titleSchema := map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}, "required": []any{"title"}}
	okSchema := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}
	ebSchema := map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}
	topSchema := map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}}}
	rf := func(schema map[string]any, extra map[string]any) map[string]any {
		js := map[string]any{"schema": schema}
		for k, v := range extra {
			js[k] = v
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	}
	t.Run("json schema response format uses native output config", func(t *testing.T) {
		got := auxCaptureAnthropicKwargs(t, map[string]any{"response_format": rf(titleSchema, map[string]any{"name": "thread_title", "strict": false})}, false, nil)
		if !reflect.DeepEqual(auxOutputFormat(got), map[string]any{"type": "json_schema", "schema": titleSchema}) {
			t.Fatalf("got %v", got["output_config"])
		}
	})
	t.Run("json object response format uses permissive object schema", func(t *testing.T) {
		got := auxCaptureAnthropicKwargs(t, map[string]any{"response_format": map[string]any{"type": "json_object"}}, false, nil)
		if !reflect.DeepEqual(auxOutputFormat(got), map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}) {
			t.Fatalf("got %v", got["output_config"])
		}
	})
	t.Run("response format merges with adaptive thinking effort", func(t *testing.T) {
		got := auxCaptureAnthropicKwargs(t, map[string]any{"reasoning": map[string]any{"enabled": true, "effort": "high"},
			"response_format": rf(okSchema, nil)}, false, nil)
		if !reflect.DeepEqual(got["output_config"], map[string]any{"effort": "high", "format": map[string]any{"type": "json_schema", "schema": okSchema}}) {
			t.Fatalf("output_config %v", got["output_config"])
		}
		if !reflect.DeepEqual(got["thinking"], map[string]any{"type": "adaptive", "display": "summarized"}) {
			t.Fatalf("thinking %v", got["thinking"])
		}
	})
	t.Run("unrelated extra body keys still pass through", func(t *testing.T) {
		got := auxCaptureAnthropicKwargs(t, map[string]any{"response_format": map[string]any{"type": "json_object"},
			"metadata": map[string]any{"user_id": "thread-autotitle"}, "vendor_option": true}, false, nil)
		if !reflect.DeepEqual(got["extra_body"], map[string]any{"metadata": map[string]any{"user_id": "thread-autotitle"}, "vendor_option": true}) {
			t.Fatalf("extra_body %v", got["extra_body"])
		}
	})
	t.Run("async anthropic adapter uses the same translation", func(t *testing.T) {
		got := auxCaptureAnthropicKwargs(t, map[string]any{"response_format": rf(map[string]any{"type": "object"}, nil)}, true, nil)
		if !reflect.DeepEqual(auxOutputFormat(got), map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}) {
			t.Fatalf("got %v", got["output_config"])
		}
	})
	t.Run("top level response format kwarg is translated not dropped", func(t *testing.T) {
		got := auxCaptureAnthropicKwargs(t, nil, false, map[string]any{"response_format": rf(titleSchema, map[string]any{"name": "session_title"})})
		if !reflect.DeepEqual(auxOutputFormat(got), map[string]any{"type": "json_schema", "schema": titleSchema}) {
			t.Fatalf("got %v", got["output_config"])
		}
	})
	t.Run("extra body response format wins over top level kwarg", func(t *testing.T) {
		got := auxCaptureAnthropicKwargs(t, map[string]any{"response_format": rf(ebSchema, nil)}, false, map[string]any{"response_format": rf(topSchema, nil)})
		if !reflect.DeepEqual(auxOutputFormat(got), map[string]any{"type": "json_schema", "schema": ebSchema}) {
			t.Fatalf("got %v", got["output_config"])
		}
	})
}

// Messages server answering streams with the given SSE text and plain creates with a fixed message
func auxAnthropicServer(t *testing.T, sse string, streamStatus int, streamBody string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		bodies = append(bodies, body)
		if body["stream"] == true {
			if streamStatus != 0 {
				w.WriteHeader(streamStatus)
				_, _ = io.WriteString(w, streamBody)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sse)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_plain","type":"message","role":"assistant","content":[{"type":"text","text":"plain"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

// SSE text of one streamed text message, with or without its message_stop
func auxAnthropicSSE(withStop bool) string {
	ev := func(name, data string) string { return fmt.Sprintf("event: %s\ndata: %s\n\n", name, data) }
	out := ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}`) +
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"streamed"}}`) +
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_details":{"kind":"natural"}},"usage":{"output_tokens":2}}`)
	if withStop {
		out += ev("message_stop", `{"type":"message_stop"}`)
	}
	return out
}

func TestCreateAnthropicMessageAggregatesStreamWithCallbacks(t *testing.T) {
	srv, bodies := auxAnthropicServer(t, auxAnthropicSSE(true), 0, "")
	client, err := buildAnthropicClient(context.Background(), "sk-ant-api-key", srv.URL+"/v1/", 30, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.BaseURL() != srv.URL {
		t.Fatalf("base %q", client.BaseURL())
	}
	events := 0
	msg, err := createAnthropicMessage(context.Background(), client, map[string]any{"model": "claude-sonnet-4-6", "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "extra_body": map[string]any{"vendor": "x"}, "stream": false},
		func(any) error { events++; return errors.New("callback noise is swallowed") })
	if err != nil {
		t.Fatal(err)
	}
	content, _ := msg["content"].([]any)
	block, _ := content[0].(map[string]any)
	if block["text"] != "streamed" || !reflect.DeepEqual(msg["stop_details"], map[string]any{"kind": "natural"}) || events != 6 {
		t.Fatalf("msg %v events %d", msg, events)
	}
	if (*bodies)[0]["vendor"] != "x" || (*bodies)[0]["stream"] != true || (*bodies)[0]["extra_body"] != nil {
		t.Fatalf("request body %v", (*bodies)[0])
	}
}

func TestCreateAnthropicMessageStopsOnDeadlineCallback(t *testing.T) {
	srv, _ := auxAnthropicServer(t, auxAnthropicSSE(true), 0, "")
	client, _ := buildAnthropicClient(context.Background(), "sk-ant-api-key", srv.URL, 0, false)
	defer client.Close()
	_, err := createAnthropicMessage(context.Background(), client, map[string]any{"model": "claude-sonnet-4-6", "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, func(any) error { return &AuxTimeoutError{Message: "timed out"} })
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("got %v", err)
	}
}

func TestCreateAnthropicMessageRequiresMessageStop(t *testing.T) {
	srv, _ := auxAnthropicServer(t, auxAnthropicSSE(false), 0, "")
	client, _ := buildAnthropicClient(context.Background(), "sk-ant-api-key", srv.URL, 0, false)
	defer client.Close()
	_, err := createAnthropicMessage(context.Background(), client, map[string]any{"model": "claude-sonnet-4-6", "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	var empty *EmptyStreamError
	if !errors.As(err, &empty) || !strings.Contains(err.Error(), "message_stop") {
		t.Fatalf("got %v", err)
	}
}

func TestCreateAnthropicMessageFallsBackWhenStreamingUnsupported(t *testing.T) {
	srv, bodies := auxAnthropicServer(t, "", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"stream mode is not supported for this model"}}`)
	client, _ := buildAnthropicClient(context.Background(), func() string { return "entra-token" }, srv.URL, 0, false)
	defer client.Close()
	msg, err := createAnthropicMessage(context.Background(), client, map[string]any{"model": "claude-sonnet-4-6", "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true}, nil)
	if err != nil || msg["id"] != "msg_plain" || len(*bodies) != 2 {
		t.Fatalf("msg %v err %v bodies %d", msg, err, len(*bodies))
	}
	if _, streamed := (*bodies)[1]["stream"]; streamed {
		t.Fatalf("fallback create kept stream: %v", (*bodies)[1])
	}
}

func TestBuildAnthropicClientExportHandsBackSDKClient(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(auxAnthropicSSE(true)))
	}))
	defer srv.Close()
	built, err := BuildAnthropicClient(context.Background(), "sk-ant-api-key", srv.URL+"/v1/", 30, false)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := built.(*anthropicSDKClient)
	if !ok {
		t.Fatalf("the export must hand back the SDK client, got %T", built)
	}
	defer client.Close()
	if client.BaseURL() != srv.URL {
		t.Fatalf("base URL %q keeps the /v1 suffix", client.BaseURL())
	}
	msg, err := createAnthropicMessage(context.Background(), client, map[string]any{"model": "claude-x", "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "streamed" || gotKey != "sk-ant-api-key" {
		t.Fatalf("message %v x-api-key %q", msg, gotKey)
	}
}

func TestBuildAnthropicBedrockClientExportBuildsTheRegionClient(t *testing.T) {
	got, gerr := BuildAnthropicBedrockClient(context.Background(), "us-west-2", 7)
	want, werr := buildAnthropicBedrockClient(context.Background(), "us-west-2", 7)
	if (gerr == nil) != (werr == nil) {
		t.Fatalf("errors differ %v %v", gerr, werr)
	}
	if gerr == nil {
		if _, ok := got.(*anthropicSDKClient); !ok || want == nil {
			t.Fatalf("client %T", got)
		}
	}
}

func TestAgentCreateAnthropicMessageHonoursTheStreamPreferenceAndOffersTheResponse(t *testing.T) {
	var streamed []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		streamed = append(streamed, body["stream"] == true)
		w.Header().Set("x-nous-credits-remaining-micros", "9")
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(auxAnthropicSSE(true)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"created"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()
	client, err := buildAnthropicClient(context.Background(), "sk-ant-api-key", srv.URL, 30, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	kwargs := func() map[string]any {
		return map[string]any{"model": "claude-sonnet-4-6", "max_tokens": 8, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	}
	var seen []string
	onResponse := func(resp any) {
		seen = append(seen, resp.(*http.Response).Header.Get("x-nous-credits-remaining-micros"))
		panic("hook noise is swallowed")
	}
	msg, err := CreateAnthropicMessage(context.Background(), client, kwargs(), "[agent] ", true, onResponse)
	if err != nil || msg.(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "streamed" || !reflect.DeepEqual(seen, []string{"9"}) {
		t.Fatalf("streamed message %v %v hook %v", msg, err, seen)
	}
	msg, err = CreateAnthropicMessage(context.Background(), client, kwargs(), "[agent] ", false, onResponse)
	if err != nil || msg.(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "created" || len(seen) != 1 {
		t.Fatalf("created message %v %v hook %v", msg, err, seen)
	}
	if !reflect.DeepEqual(streamed, []bool{true, false}) {
		t.Fatalf("stream flags %v", streamed)
	}
	if _, err := CreateAnthropicMessage(context.Background(), struct{}{}, kwargs(), "", true, nil); err == nil {
		t.Fatal("client without messages accepted")
	}
}
