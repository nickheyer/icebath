package transport

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func stream(t *testing.T, handler http.HandlerFunc, request *provider.Request) (*provider.Response, []provider.StreamEvent, error) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	host := transport.New()
	Register(&host.Registry)
	client, err := host.Client(provider.Route{Provider: "test", BaseURL: server.URL + "/v1/", APIKey: "secret", Mode: provider.ModeChatCompletions, Headers: map[string]string{"X-Title": "icebath"}})
	if err != nil {
		t.Fatalf("Client() error = %v", err)
	}
	var events []provider.StreamEvent
	response, err := client.Stream(t.Context(), request, func(event provider.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	return response, events, err
}

func TestStreamAssemblesTheReply(t *testing.T) {
	var body map[string]any
	var header http.Header
	var path string
	response, events, err := stream(t, func(w http.ResponseWriter, r *http.Request) {
		path, header = r.URL.Path, r.Header.Clone()
		payload, _ := io.ReadAll(r.Body)
		json.Unmarshal(payload, &body)
		for _, line := range []string{
			`{"choices":[{"delta":{"reasoning_content":"think "}}]}`,
			`{"choices":[{"delta":{"reasoning":"more"}}]}`,
			`{"choices":[{"delta":{"content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"terminal","arguments":"{\"comm"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}},{"index":1,"id":"call_2","function":{"name":"read_file","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
	}, &provider.Request{
		Model: "model-x",
		Messages: []*sessionsv1.Message{
			{Role: sessionsv1.Role_ROLE_SYSTEM, Text: "be brief"},
			{Role: sessionsv1.Role_ROLE_USER, Text: "list files"},
			{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: "ok", ToolCalls: []*sessionsv1.ToolCall{{Id: "c0", Name: "terminal", Arguments: `{"command":"pwd"}`}}},
			{Role: sessionsv1.Role_ROLE_TOOL, ToolCallId: "c0", Text: "/work"},
		},
		Tools:     []map[string]any{{"type": "function", "function": jsonutil.ObjectOf("name", "terminal", "description", "Run", "parameters", jsonutil.ObjectOf("type", "object"))}},
		Reasoning: provider.Reasoning{Effort: "high"},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if path != "/v1/chat/completions" || header.Get("Authorization") != "Bearer secret" || header.Get("X-Title") != "icebath" || header.Get("Content-Type") != "application/json" {
		t.Fatalf("request went to %s with %v", path, header)
	}
	wantBody := map[string]any{
		"model": "model-x", "stream": true, "reasoning_effort": "high",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "list files"},
			map[string]any{"role": "assistant", "content": "ok", "tool_calls": []any{map[string]any{"id": "c0", "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"command":"pwd"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "c0", "content": "/work"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "terminal", "description": "Run", "parameters": map[string]any{"type": "object"}}}},
	}
	if !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("request body = %v\nwant           %v", body, wantBody)
	}
	message := response.Message
	if response.FinishReason != "tool_calls" || message.GetRole() != sessionsv1.Role_ROLE_ASSISTANT || message.GetText() != "Hello" || message.GetReasoning() != "think more" {
		t.Fatalf("Stream() = %q, %v", response.FinishReason, message)
	}
	if calls := message.GetToolCalls(); len(calls) != 2 || calls[0].GetId() != "call_1" || calls[0].GetName() != "terminal" || calls[0].GetArguments() != `{"command":"ls"}` || calls[1].GetName() != "read_file" {
		t.Fatalf("tool calls = %v", message.GetToolCalls())
	}
	wantEvents := []provider.StreamEvent{
		{Kind: provider.EventReasoningDelta, Text: "think "}, {Kind: provider.EventReasoningDelta, Text: "more"},
		{Kind: provider.EventTextDelta, Text: "Hel"}, {Kind: provider.EventTextDelta, Text: "lo"},
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %v, want %v", events, wantEvents)
	}
}

func TestStreamOmitsWhatTheRequestLeavesOut(t *testing.T) {
	var body map[string]any
	_, _, err := stream(t, func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		json.Unmarshal(payload, &body)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n")
	}, &provider.Request{Model: "m", Messages: []*sessionsv1.Message{{Role: sessionsv1.Role_ROLE_USER, Text: "hi"}}})
	if err != nil {
		t.Fatalf("Stream() of a body that ends without [DONE] error = %v", err)
	}
	if _, ok := body["tools"]; ok {
		t.Error("request sent tools with none offered")
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("request sent reasoning_effort with none set")
	}
}

func TestStreamReadsACompressedReply(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "gzip" {
			http.Error(w, "expected the route header", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		packed := gzip.NewWriter(w)
		fmt.Fprint(packed, "data: {\"choices\":[{\"delta\":{\"content\":\"packed\"},\"finish_reason\":\"stop\"}]}\n\n")
		packed.Close()
	}))
	defer server.Close()
	route := provider.Route{BaseURL: server.URL, Headers: map[string]string{"Accept-Encoding": "gzip"}}
	response, err := (&client{route: route}).Stream(t.Context(), &provider.Request{Model: "m"}, func(provider.StreamEvent) error { return nil })
	if err != nil || response.Message.GetText() != "packed" {
		t.Fatalf("Stream() of a gzip reply = %v, %v, want it unpacked", response, err)
	}
}

func TestStreamClassifiesFailures(t *testing.T) {
	request := &provider.Request{Model: "m"}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		status  int
		kind    provider.ErrorKind
		message string
		retry   time.Duration
	}{
		{"rate limited", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			http.Error(w, `{"error":"slow down"}`, http.StatusTooManyRequests)
		}, 429, provider.ErrRateLimit, `{"error":"slow down"}`, 7 * time.Second},
		{"unauthorized", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "bad key", http.StatusUnauthorized) }, 401, provider.ErrAuth, "bad key", 0},
		{"server", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusBadGateway) }, 502, provider.ErrServer, "down", 0},
		{"error event", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "data: {\"error\":{\"message\":\"overloaded\"}}\n\n")
		}, 0, provider.ErrServer, "overloaded", 0},
		{"cut short", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n")
		}, 0, provider.ErrEmptyResponse, "the reply ended before it finished", 0},
	}
	for _, c := range cases {
		_, _, err := stream(t, c.handler, request)
		failure, ok := errors.AsType[*provider.APIError](err)
		if !ok {
			t.Errorf("%s: error = %v, want an APIError", c.name, err)
			continue
		}
		if failure.StatusCode != c.status || failure.Kind != c.kind || failure.Message != c.message || failure.RetryAfter != c.retry || failure.Provider != "test" || failure.Model != "m" {
			t.Errorf("%s: failure = %+v", c.name, failure)
		}
	}
	_, _, err := stream(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":3,\"id\":\"x\"}]}}]}\n\n")
	}, request)
	if err == nil || !strings.Contains(err.Error(), "tool call 3 out of order") {
		t.Errorf("out of order tool call error = %v", err)
	}
}

func TestStreamStopsWhenTheListenerRefuses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"b\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer server.Close()
	stop := errors.New("stop")
	seen := 0
	_, err := (&client{route: provider.Route{BaseURL: server.URL}}).Stream(t.Context(), &provider.Request{Model: "m"}, func(provider.StreamEvent) error {
		seen++
		return stop
	})
	if !errors.Is(err, stop) || seen != 1 {
		t.Fatalf("Stream() error = %v after %d events, want the listener's error after 1", err, seen)
	}
}
