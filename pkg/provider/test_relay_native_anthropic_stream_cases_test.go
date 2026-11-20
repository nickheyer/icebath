package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/tools"
)

const anthropicRelayStreamBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-test","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicSdkStreamRunsThroughRelayManagedExecution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, anthropicRelayStreamBody)
	}))
	t.Cleanup(srv.Close)
	f := relayLLMInstallFake(t, "icebath-home")
	agent := &fakeChatAgent{provider: "anthropic", apiMode: "anthropic_messages", baseURL: "https://api.anthropic.com", model: "claude-test", plat: "cli"}
	ctx := pyctx.With(context.Background(), pyctx.New())
	client, err := BuildAnthropicClient(ctx, "test-key", srv.URL, 60, false)
	if err != nil {
		t.Fatal(err)
	}
	agent.anthropicCreate = func(string) (any, error) { return client, nil }
	var managed []string
	streamExecute := f.relay.LLM.StreamExecute
	f.relay.LLM.StreamExecute = func(c context.Context, operation string, request any, provider tools.RelayProviderStreamCallback, observe func(context.Context, any) error, finalizer func(context.Context) (any, error), kwargs map[string]any) (any, error) {
		managed = append(managed, operation)
		return streamExecute(c, operation, request, provider, observe, finalizer, kwargs)
	}
	lease := tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: agent.SessionID(), Platform: "cli"})
	turn, err := tools.RelayCoordinator.BeginTurn(ctx, lease, "anthropic-relay-turn", "anthropic-relay-task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Host.RetainManagedExecution("test.anthropic_relay"); err != nil {
		t.Fatal(err)
	}
	result, err := func() (any, error) {
		defer func() {
			lease.Host.ReleaseManagedExecution("test.anthropic_relay")
			tools.RelayCoordinator.EndTurn(ctx, turn, "success")
			tools.RelayCoordinator.ReleaseConversation(lease)
			tools.ResetRelayRuntimeForTests()
		}()
		return InterruptibleStreamingAPICall(ctx, agent, map[string]any{
			"model": "claude-test", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}, nil)
	}()
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 1 {
		t.Fatalf("stream did not run through Relay managed execution %v", managed)
	}
	message, _ := result.(map[string]any)
	content, _ := message["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("result %#v", result)
	}
	first, _ := content[0].(map[string]any)
	if first["text"] != "hello" {
		t.Fatalf("content %#v", content)
	}
	if message["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason %#v", message["stop_reason"])
	}
}
