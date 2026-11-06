package tools

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type relayToolsTestError struct{ msg string }

func (e *relayToolsTestError) Error() string { return e.msg }

type relayToolsFakeContext struct{ args any }

type relayToolsFakeNext func(ctx context.Context, args any) (*RelayToolExecutionResult, error)

// Tool pipeline double running request rewrites then execution wrappers around the dispatch callback
type relayToolsFake struct {
	*relayFake
	requests    []func(name string, args map[string]any) map[string]any
	executions  []func(ctx context.Context, rctx relayToolsFakeContext, next relayToolsFakeNext) (*RelayToolExecutionResult, map[string]any, error)
	annotations []map[string]any
}

func newRelayToolsFake() *relayToolsFake {
	f := &relayToolsFake{relayFake: newRelayFake(nil, nil)}
	f.relay.ToolExecutionResult = func(result any) (*RelayToolExecutionResult, error) {
		return &RelayToolExecutionResult{Result: result}, nil
	}
	f.relay.Tools = &NemoRelayTools{Execute: f.execute}
	return f
}

func (f *relayToolsFake) execute(_ context.Context, name string, args any, callback RelayProviderCallback, _ map[string]any) (any, error) {
	return RelayAwaitable(func(ctx context.Context) (any, error) {
		final := args
		for _, rewrite := range f.requests {
			final = rewrite(name, final.(map[string]any))
		}
		chain := relayToolsFakeNext(func(c context.Context, a any) (*RelayToolExecutionResult, error) {
			v, err := callback(c, a)
			if err != nil {
				return nil, err
			}
			return v.(*RelayToolExecutionResult), nil
		})
		for _, wrap := range f.executions {
			inner, wrap := chain, wrap
			chain = func(c context.Context, a any) (*RelayToolExecutionResult, error) {
				result, annotation, err := wrap(c, relayToolsFakeContext{args: a}, inner)
				f.annotations = append(f.annotations, annotation)
				return result, err
			}
		}
		return chain(ctx, final)
	}), nil
}

// The relay_turn fixture over the tool fake with managed execution retained
func relayToolsTurn(t *testing.T) (*relayToolsFake, context.Context, *ConversationLease) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), "profile"))
	f := newRelayToolsFake()
	RegisterNemoRelayBinding(func() (*NemoRelay, error) { return f.relay, nil })
	ResetRelayRuntimeForTests()
	ctx := pyctx.With(context.Background(), pyctx.New())
	lease := RelayCoordinator.AcquireConversation(ctx, RelayConversation{ProfileKey: CurrentRelayProfileKey(ctx), SessionID: "session-1", Platform: "cli"})
	turn, err := RelayCoordinator.BeginTurn(ctx, lease, "turn-1", "task-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Host.RetainManagedExecution("test.relay_tools"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		lease.Host.ReleaseManagedExecution("test.relay_tools")
		RelayCoordinator.EndTurn(ctx, turn, "success")
		RelayCoordinator.ReleaseConversation(lease)
		ResetRelayRuntimeForTests()
		RegisterNemoRelayBinding(nil)
	})
	return f, ctx, lease
}

func TestRelayToolAdapterBypassesRelayWithoutAnActiveConsumer(t *testing.T) {
	f, ctx, _ := relayToolsTurn(t)
	runtime := GetRelayRuntime(ctx, true, CurrentRelayProfileKey(ctx))
	if runtime == nil {
		t.Fatal("no runtime")
	}
	runtime.ReleaseManagedExecution("test.relay_tools")
	runtime.ReleaseManagedExecution(RelayPluginsExecutionConsumer)
	f.relay.Tools.Execute = func(context.Context, string, any, RelayProviderCallback, map[string]any) (any, error) {
		t.Fatal("inactive Relay must not manage the tool call")
		return nil, nil
	}
	args := map[string]any{"command": "pwd"}
	result, final, err := RelayExecuteTool(ctx, "terminal", args, func(_ context.Context, v map[string]any) (any, error) { return v, nil }, "session-1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.ValueOf(result).Pointer() != reflect.ValueOf(args).Pointer() || reflect.ValueOf(final).Pointer() != reflect.ValueOf(args).Pointer() {
		t.Fatal("bypass did not return the caller's own arguments")
	}
}

func TestRelayRequestRewriteReachesAuthorizedCallbackOnce(t *testing.T) {
	f, ctx, _ := relayToolsTurn(t)
	var callbackArgs []map[string]any
	f.requests = append(f.requests, func(_ string, args map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range args {
			out[k] = v
		}
		out["path"] = "/approved/path"
		return out
	})
	f.executions = append(f.executions, func(c context.Context, rctx relayToolsFakeContext, next relayToolsFakeNext) (*RelayToolExecutionResult, map[string]any, error) {
		result, err := next(c, rctx.args)
		if err != nil {
			return nil, nil, err
		}
		wrapped := map[string]any{"wrapped": true}
		for k, v := range result.Result.(map[string]any) {
			wrapped[k] = v
		}
		return &RelayToolExecutionResult{Result: wrapped}, map[string]any{"audit": "annotation-canary"}, nil
	})
	result, observed, err := RelayExecuteTool(ctx, "write_file", map[string]any{"path": "/original/path"}, func(_ context.Context, args map[string]any) (any, error) {
		callbackArgs = append(callbackArgs, args)
		return map[string]any{"ok": true}, nil
	}, "session-1", "", map[string]any{"tool_call_id": "call-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(callbackArgs, []map[string]any{{"path": "/approved/path"}}) || !reflect.DeepEqual(observed, map[string]any{"path": "/approved/path"}) {
		t.Fatalf("callback args %v, observed %v", callbackArgs, observed)
	}
	text, ok := result.(string)
	if !ok {
		t.Fatalf("result %#v is not a string", result)
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pyjson.Plain(decoded), map[string]any{"ok": true, "wrapped": true}) || strings.Contains(text, "annotation-canary") {
		t.Fatalf("result %s", text)
	}
	if !reflect.DeepEqual(f.annotations, []map[string]any{{"audit": "annotation-canary"}}) {
		t.Fatalf("annotations %v", f.annotations)
	}
}

func TestRelayToolCallIDUsesCanonicalRelayArgument(t *testing.T) {
	f, ctx, _ := relayToolsTurn(t)
	captured := map[string]any{}
	f.relay.Tools.Execute = func(_ context.Context, _ string, args any, callback RelayProviderCallback, kwargs map[string]any) (any, error) {
		return RelayAwaitable(func(c context.Context) (any, error) {
			for k, v := range kwargs {
				captured[k] = v
			}
			result, err := callback(c, args)
			if _, ok := result.(*RelayToolExecutionResult); !ok {
				t.Errorf("callback returned %#v", result)
			}
			return result, err
		}), nil
	}
	original := map[string]any{"ok": true}
	result, observed, err := RelayExecuteTool(ctx, "write_file", map[string]any{"path": "/tmp/output"}, func(context.Context, map[string]any) (any, error) { return original, nil }, "session-1", "call-42", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.ValueOf(result).Pointer() != reflect.ValueOf(original).Pointer() {
		t.Fatal("result is not the tool's own value")
	}
	if !reflect.DeepEqual(observed, map[string]any{"path": "/tmp/output"}) || captured["tool_call_id"] != "call-42" {
		t.Fatalf("observed %v, kwargs %v", observed, captured)
	}
}

func TestRelayToolErrorIsPreservedFromRelayWrapperSuffix(t *testing.T) {
	f, ctx, _ := relayToolsTurn(t)
	toolErr := &relayToolsTestError{msg: "dispatch failed"}
	f.relay.Tools.Execute = func(_ context.Context, _ string, args any, callback RelayProviderCallback, _ map[string]any) (any, error) {
		return RelayAwaitable(func(c context.Context) (any, error) {
			v, err := callback(c, args)
			if err != nil {
				return nil, &RelayError{Class: "RuntimeError", Message: "internal error: relayToolsTestError: " + err.Error() + " (worker trace)"}
			}
			return v, nil
		}), nil
	}
	_, _, err := RelayExecuteTool(ctx, "terminal", map[string]any{"command": "false"}, func(context.Context, map[string]any) (any, error) { return nil, toolErr }, "session-1", "", nil)
	if err != error(toolErr) {
		t.Fatalf("error %v is not the tool error", err)
	}
}
