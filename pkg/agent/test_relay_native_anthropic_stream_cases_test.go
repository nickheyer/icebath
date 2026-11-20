package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/relayhost/relaytest"
	"github.com/nickheyer/icebath/pkg/tools"
)

const anthropicRelayStream = `event: message_start
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

func TestAnthropicSDKStreamRunsThroughRelayManagedExecution(t *testing.T) {
	relaytest.Use(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(h, "icebath-home"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicRelayStream)
	}))
	t.Cleanup(srv.Close)
	tools.ResetRelayRuntimeForTests()
	env := &tools.Env{Home: h, Cwd: h}
	ctx := pyctx.With(tools.WithToolEnv(context.Background(), env), pyctx.New())
	a, err := NewAIAgent(ctx, AIAgentOptions{APIKey: "test-key", BaseURL: srv.URL, Provider: "anthropic", Model: "claude-test",
		QuietMode: true, SkipContextFiles: true, SkipMemory: true, Cwd: h, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.apiMode, a.sessionID = "anthropic_messages", "anthropic-relay-session"
	a.interruptRequested.Store(false)
	lease := tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: tools.CurrentRelayProfileKey(ctx),
		SessionID: a.sessionID, Platform: "cli"})
	turn, err := tools.RelayCoordinator.BeginTurn(ctx, lease, "anthropic-relay-turn", "anthropic-relay-task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Host.RetainManagedExecution("test.anthropic_relay"); err != nil {
		t.Fatal(err)
	}
	var result any
	func() {
		defer func() {
			lease.Host.ReleaseManagedExecution("test.anthropic_relay")
			tools.RelayCoordinator.EndTurn(ctx, turn, "success")
			tools.RelayCoordinator.ReleaseConversation(lease)
			tools.ResetRelayRuntimeForTests()
		}()
		result, err = a.interruptibleStreamingAPICall(ctx, map[string]any{"model": "claude-test", "max_tokens": 16,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	}()
	if err != nil {
		t.Fatal(err)
	}
	message, _ := result.(map[string]any)
	var text any
	switch content := message["content"].(type) {
	case []any:
		if len(content) > 0 {
			block, _ := content[0].(map[string]any)
			text = block["text"]
		}
	case []map[string]any:
		if len(content) > 0 {
			text = content[0]["text"]
		}
	}
	if text != "hello" || message["stop_reason"] != "end_turn" {
		t.Fatalf("result %s", pyrepr.Repr(result))
	}
}
