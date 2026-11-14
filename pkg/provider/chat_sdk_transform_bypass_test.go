package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

const chatBypassSSE = "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\"," +
	"\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
	"data: [DONE]\n\n"

// Production shaped chat body with content parts, a tool_calls turn, a tool result, tool schemas and extra_body
func chatBypassWireBody() map[string]any {
	toolsList := []any{}
	for _, name := range []string{"tool_0", "tool_1", "tool_2"} {
		toolsList = append(toolsList, map[string]any{"type": "function", "function": map[string]any{"name": name, "description": "d",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"p": map[string]any{"type": "string"}}, "required": []any{"p"}}}})
	}
	return map[string]any{
		"model": "icebath-4-70b",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are Icebath."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "look at this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://e.example/i.png", "detail": "low"}},
			}},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "terminal", "arguments": `{"cmd":"ls"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "total 0"},
		},
		"tools":          toolsList,
		"tool_choice":    "auto",
		"stream":         true,
		"temperature":    0.7,
		"stream_options": map[string]any{"include_usage": true},
		"extra_body":     map[string]any{"reasoning": map[string]any{"effort": "high"}, "provider": map[string]any{"order": []any{"nous"}}},
	}
}

// A real SDK client whose server records the request bytes of each streamed send
func chatBypassRecorder(t *testing.T) (*auxOpenAIClient, func(map[string]any) string) {
	t.Helper()
	var content []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, chatBypassSSE)
	}))
	t.Cleanup(srv.Close)
	client := newAuxOpenAIClient("k", srv.URL+"/v1", auxOpenAIOptions{HTTPClient: srv.Client()})
	send := func(kwargs map[string]any) string {
		t.Helper()
		content = nil
		out, err := client.CreateChatCompletion(context.Background(), kwargs)
		if err != nil {
			t.Fatal(err)
		}
		if s, ok := out.(interface {
			Next(context.Context) (any, error)
		}); ok {
			for {
				if _, err := s.Next(context.Background()); err != nil {
					break
				}
			}
		}
		if content == nil {
			t.Fatal("no request recorded")
		}
		return string(content)
	}
	return client, send
}

func TestChatSDKBypassBulkFieldsRideInExtraBodyAndTheWireBytesAreIdentical(t *testing.T) {
	client, send := chatBypassRecorder(t)
	body := chatBypassWireBody()
	moved := BypassChatSDKRequestTransform(copyMap(body), client)
	extra := moved["extra_body"].(map[string]any)
	if m, _ := moved["messages"].([]any); m == nil || len(m) != 0 {
		t.Fatalf("messages slot %v", moved["messages"])
	}
	if tl, _ := moved["tools"].([]any); tl == nil || len(tl) != 0 {
		t.Fatalf("tools slot %v", moved["tools"])
	}
	if !reflect.DeepEqual(extra["messages"], body["messages"]) || !reflect.DeepEqual(extra["tools"], body["tools"]) ||
		!reflect.DeepEqual(extra["reasoning"], body["extra_body"].(map[string]any)["reasoning"]) {
		t.Fatalf("extra body %v", extra)
	}
	if a, b := send(moved), send(copyMap(body)); a != b {
		t.Fatalf("wire bytes differ\n%s\n%s", a, b)
	}
}

func TestChatSDKBypassEscapeHatchAndNonSDKFacadesKeepTheTypedPath(t *testing.T) {
	client, _ := chatBypassRecorder(t)
	kwargs := chatBypassWireBody()
	same := func(out map[string]any) bool {
		return reflect.ValueOf(out).Pointer() == reflect.ValueOf(kwargs).Pointer()
	}
	if !same(BypassChatSDKRequestTransform(kwargs, &auxProbeClient{})) {
		t.Fatal("a chat shaped facade that is not the SDK keeps the kwargs")
	}
	t.Setenv(sdkTransformEscapeHatchEnv, "1")
	if !same(BypassChatSDKRequestTransform(kwargs, client)) {
		t.Fatal("the escape hatch keeps the kwargs")
	}
}

// Shallow copy of a request map, the dict(body) of the Python test
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
