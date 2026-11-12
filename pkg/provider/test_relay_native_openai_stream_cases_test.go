package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/tools"
)

const openAIRelayChunkPrefix = `data: {"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"test/model",`

// SSE body of the given chunk bodies closed by the done frame
func openAIRelaySSE(chunkBodies ...string) string {
	var b strings.Builder
	for _, body := range chunkBodies {
		b.WriteString(openAIRelayChunkPrefix + body + "}\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// Field of a map shaped stream chunk or response part
func openAIRelayField(v any, key string) any {
	field, _ := bridgeAttr(v, key)
	return field
}

// Streams body through Relay with its finalizer completing before the consumer takes the chosen chunk, returning the result and finalized response
func openAIStreamThroughRelay(t *testing.T, body string, finalizeBefore func(chunk any) bool) (any, any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	f := relayLLMInstallFake(t, "icebath-home")
	t.Setenv("ICEBATH_STREAM_RETRIES", "0")
	agent := &fakeChatAgent{provider: "test-provider", apiMode: "chat_completions", baseURL: "https://example.com/v1", model: "test/model", plat: "cli"}
	client := NewOpenAIClient("test-key", srv.URL+"/v1", OpenAIClientOptions{})
	agent.create = func(string, map[string]any) (any, error) { return client, nil }
	var mu sync.Mutex
	var ends []any
	finalizedFirst := false
	streamExecute := f.relay.LLM.StreamExecute
	f.relay.LLM.StreamExecute = func(c context.Context, operation string, request any, provider tools.RelayProviderStreamCallback, observe func(context.Context, any) error, finalizer func(context.Context) (any, error), kwargs map[string]any) (any, error) {
		var once sync.Once
		var final any
		var finalErr error
		runFinalizer := func(ctx context.Context) (any, error) {
			once.Do(func() {
				final, finalErr = finalizer(ctx)
				if operation == "openai.chat_completions" && finalErr == nil {
					mu.Lock()
					ends = append(ends, final)
					mu.Unlock()
				}
			})
			return final, finalErr
		}
		forced := func(ctx context.Context, chunk any) error {
			if err := observe(ctx, chunk); err != nil {
				return err
			}
			if !finalizeBefore(chunk) {
				return nil
			}
			if _, err := runFinalizer(ctx); err != nil {
				return err
			}
			mu.Lock()
			finalizedFirst = true
			mu.Unlock()
			return nil
		}
		return streamExecute(c, operation, request, provider, forced, runFinalizer, kwargs)
	}
	ctx := pyctx.With(context.Background(), pyctx.New())
	lease := tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: agent.SessionID(), Platform: "cli"})
	turn, err := tools.RelayCoordinator.BeginTurn(ctx, lease, "openai-relay-turn", "openai-relay-task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Host.RetainManagedExecution("test.openai_relay"); err != nil {
		t.Fatal(err)
	}
	result, err := func() (any, error) {
		defer func() {
			lease.Host.ReleaseManagedExecution("test.openai_relay")
			tools.RelayCoordinator.EndTurn(ctx, turn, "success")
			tools.RelayCoordinator.ReleaseConversation(lease)
			tools.ResetRelayRuntimeForTests()
		}()
		return InterruptibleStreamingAPICall(ctx, agent, map[string]any{
			"model": "test/model", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
	}()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !finalizedFirst {
		t.Fatal("Relay's finalizer did not run before the consumer took the chosen chunk")
	}
	if len(ends) != 1 {
		t.Fatalf("llm end responses %d", len(ends))
	}
	return result, ends[0]
}

func TestOpenaiStreamUsageReachesRelayParentEvent(t *testing.T) {
	body := openAIRelaySSE(
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":null}]`,
		`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`,
		`"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}`,
	)
	result, llmEnd := openAIStreamThroughRelay(t, body, func(chunk any) bool {
		choices, _ := openAIRelayField(chunk, "choices").([]any)
		return len(choices) == 0 && openAIRelayField(chunk, "usage") != nil
	})
	if got := fmt.Sprint(openAIRelayField(result, "usage")); got != "map[completion_tokens:10 prompt_tokens:100 total_tokens:110]" {
		t.Fatalf("result usage %s", got)
	}
	if got := fmt.Sprint(openAIRelayField(llmEnd, "usage")); got != "map[completion_tokens:10 prompt_tokens:100 total_tokens:110]" {
		t.Fatalf("relay end usage %s", got)
	}
	if got := openAIRelayMessage(llmEnd)["content"]; got != "done" {
		t.Fatalf("relay end message %#v", got)
	}
}

// First choice of a chat completion response and its message
func openAIRelayChoice(response any) (map[string]any, map[string]any) {
	choices, _ := openAIRelayField(response, "choices").([]any)
	if len(choices) == 0 {
		return map[string]any{}, map[string]any{}
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return choice, message
}

// Message of the first choice of a chat completion response
func openAIRelayMessage(response any) map[string]any {
	_, message := openAIRelayChoice(response)
	return message
}

// Name and arguments of each tool call of a response message
func openAIRelayToolCalls(t *testing.T, response any) [][2]string {
	t.Helper()
	calls, _ := openAIRelayMessage(response)["tool_calls"].([]any)
	out := [][2]string{}
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		arguments, _ := fn["arguments"].(string)
		out = append(out, [2]string{name, arguments})
	}
	return out
}

func TestOpenaiStreamFinalToolCallDeltaReachesRelayParentEvent(t *testing.T) {
	body := openAIRelaySSE(
		`"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\": "}}]},"finish_reason":null}]`,
		`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp/x\"}"}}]},"finish_reason":"tool_calls"}]`,
	)
	result, llmEnd := openAIStreamThroughRelay(t, body, func(chunk any) bool {
		choices, _ := openAIRelayField(chunk, "choices").([]any)
		return len(choices) > 0 && openAIRelayField(choices[0], "finish_reason") == "tool_calls"
	})
	hermesCalls := openAIRelayToolCalls(t, result)
	if len(hermesCalls) == 0 || hermesCalls[0] != [2]string{"read_file", `{"path": "/tmp/x"}`} {
		t.Fatalf("result tool calls %v", hermesCalls)
	}
	if choice, _ := openAIRelayChoice(result); choice["finish_reason"] != "tool_calls" {
		t.Fatalf("result finish_reason %#v", choice["finish_reason"])
	}
	if content := openAIRelayMessage(llmEnd)["content"]; content != nil {
		t.Fatalf("relay end message %#v", content)
	}
	relayCalls := openAIRelayToolCalls(t, llmEnd)
	if len(relayCalls) != 1 || relayCalls[0][0] != "read_file" {
		t.Fatalf("relay end tool calls %v", relayCalls)
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(relayCalls[0][1]), &arguments); err != nil || !reflect.DeepEqual(arguments, map[string]any{"path": "/tmp/x"}) {
		t.Fatalf("relay end arguments %q err %v", relayCalls[0][1], err)
	}
	if choice, _ := openAIRelayChoice(llmEnd); choice["finish_reason"] != "tool_calls" {
		t.Fatalf("relay end finish_reason %#v", choice["finish_reason"])
	}
}
