package provider

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/tools"
)

// The relay_turn fixture with a tool pipeline that runs the dispatch callback and wraps its result
func relayNestedTurn(t *testing.T) (*relayLLMFake, context.Context) {
	t.Helper()
	f := relayLLMInstallFake(t, "profile")
	f.relay.ToolExecutionResult = func(result any) (*tools.RelayToolExecutionResult, error) {
		return &tools.RelayToolExecutionResult{Result: result}, nil
	}
	f.relay.Tools.Execute = func(_ context.Context, _ string, args any, callback tools.RelayProviderCallback, _ map[string]any) (any, error) {
		return tools.RelayAwaitable(func(ctx context.Context) (any, error) { return callback(ctx, args) }), nil
	}
	ctx := pyctx.With(context.Background(), pyctx.New())
	lease := tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: "session-1", Platform: "cli"})
	turn, err := tools.RelayCoordinator.BeginTurn(ctx, lease, "turn-1", "task-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Host.RetainManagedExecution("test.nested_relay"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		lease.Host.ReleaseManagedExecution("test.nested_relay")
		tools.RelayCoordinator.EndTurn(ctx, turn, "success")
		tools.RelayCoordinator.ReleaseConversation(lease)
	})
	return f, ctx
}

// Chat completion payload answering content
func relayNestedCompletion(id, content string) map[string]any {
	return map[string]any{
		"id": id, "object": "chat.completion",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
	}
}

// Auxiliary vision call on a worker goroutine holding a copy of the caller context, as vision_analyze makes it
func relayNestedAuxCallFromWorker(ctx context.Context) (map[string]any, error) {
	provider := func(context.Context, map[string]any) (any, error) {
		return tools.RelayAwaitable(func(context.Context) (any, error) {
			runtime.Gosched()
			return relayNestedCompletion("aux-1", "nested"), nil
		}), nil
	}
	type outcome struct {
		value any
		err   error
	}
	done := make(chan outcome, 1)
	worker := pyctx.With(ctx, pyctx.CopyContext(ctx))
	go func() {
		v, err := relayExecuteCurrentAsync(worker, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "look"}}, "model": "m"},
			provider, "nested-prov", "m", map[string]any{"api_mode": "chat_completions", "api_request_id": "req-nested", "call_role": "auxiliary:vision"}, false)
		done <- outcome{v, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			return nil, o.err
		}
		result, ok := o.value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("aux call answered %T", o.value)
		}
		return result, nil
	case <-time.After(30 * time.Second):
		return nil, errors.New("nested aux call deadlocked (#77244)")
	}
}

// Depth of managed callbacks the context sits inside, zero when no guard ever set it
func relayNestedCallbackDepth(ctx context.Context) any {
	for _, item := range pyctx.Current(ctx).Items() {
		if item.Var.Name() == "hermes_relay_managed_callback_depth" {
			return item.Value
		}
	}
	return 0
}

func TestNestedAuxLlmCallInsideManagedToolDoesNotCrossLoops(t *testing.T) {
	f, ctx := relayNestedTurn(t)
	original := f.relay.LLM.Execute
	names := relayLLMCaptureExecuteNames(f)
	theTool := func(c context.Context, _ map[string]any) (any, error) {
		result, err := relayNestedAuxCallFromWorker(c)
		if err != nil {
			return nil, err
		}
		message := result["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		return map[string]any{"analysis": message["content"]}, nil
	}
	result, _, err := tools.RelayExecuteTool(ctx, "vision_analyze", map[string]any{"image_url": "/tmp/x.png"}, theTool, "session-1", "", map[string]any{"api_request_id": "req-tool"})
	f.relay.LLM.Execute = original
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(result), "nested") {
		t.Fatalf("tool result %v", result)
	}
	if slices.Contains(*names, "nested-prov") {
		t.Fatalf("nested call went through the managed pipeline %v", *names)
	}
}

func TestMainTurnLlmCallStaysManaged(t *testing.T) {
	f, ctx := relayNestedTurn(t)
	original := f.relay.LLM.Execute
	names := relayLLMCaptureExecuteNames(f)
	out, err := RelayExecute(ctx, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "model": "m"},
		func(context.Context, map[string]any) (any, error) {
			return relayNestedCompletion("main-1", "main"), nil
		},
		RelayExecuteOptions{Name: "main-prov", ModelName: "m", SessionID: relayLLMSID("session-1"),
			Metadata: map[string]any{"api_mode": "chat_completions", "api_request_id": "req-main", "call_role": "primary"}})
	f.relay.LLM.Execute = original
	if err != nil || out == nil {
		t.Fatalf("main call %v %v", out, err)
	}
	if !slices.Contains(*names, "openai.chat_completions") {
		t.Fatalf("main call bypassed the managed pipeline %v", *names)
	}
}

func TestGuardResetsAfterManagedCallbackReturns(t *testing.T) {
	f, ctx := relayNestedTurn(t)
	original := f.relay.LLM.Execute
	names := relayLLMCaptureExecuteNames(f)
	defer func() { f.relay.LLM.Execute = original }()
	if _, _, err := tools.RelayExecuteTool(ctx, "noop_tool", map[string]any{}, func(context.Context, map[string]any) (any, error) {
		return map[string]any{"ok": true}, nil
	}, "session-1", "", map[string]any{"api_request_id": "req-tool2"}); err != nil {
		t.Fatal(err)
	}
	if depth := relayNestedCallbackDepth(ctx); depth != 0 {
		t.Fatalf("callback depth %v after the tool returned", depth)
	}
	if _, err := RelayExecute(ctx, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "model": "m"},
		func(context.Context, map[string]any) (any, error) {
			return relayNestedCompletion("after-1", "after"), nil
		},
		RelayExecuteOptions{Name: "after-prov", ModelName: "m", SessionID: relayLLMSID("session-1"),
			Metadata: map[string]any{"api_mode": "chat_completions", "api_request_id": "req-after", "call_role": "primary"}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(*names, "openai.chat_completions") {
		t.Fatalf("call after the tool bypassed the managed pipeline %v", *names)
	}
}
