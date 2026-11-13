package tools

import (
	"context"
	"fmt"
	"testing"
)

func TestManagedToolPipelineRejectsSecondDispatch(t *testing.T) {
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	d := NewDispatcher(NewRegistry())
	var duplicateErrors []string
	d.Relay = func(rctx context.Context, _ *Env, _ string, args map[string]any, _ ToolHookIDs, pipeline func(context.Context, map[string]any) (string, error)) (string, map[string]any, error) {
		result, err := pipeline(rctx, args)
		if _, again := pipeline(rctx, args); again != nil {
			duplicateErrors = append(duplicateErrors, again.Error())
		}
		return result, args, err
	}
	e := NewExecutor(host, d)
	var dispatched []map[string]any
	outcome, err := e.runToolExecutionMiddleware(context.Background(), managedInput{name: "terminal", args: map[string]any{"command": "true"},
		taskID: "task-1", toolCallID: "call-1", execute: func(_ context.Context, args map[string]any) (string, error) {
			dispatched = append(dispatched, args)
			return "ok", nil
		}})
	if err != nil || outcome.Result != "ok" || fmt.Sprint(dispatched) != "[map[command:true]]" || len(duplicateErrors) != 1 {
		t.Fatalf("outcome %+v err %v dispatched %v duplicates %v", outcome, err, dispatched, duplicateErrors)
	}
}

func copyArgs(args map[string]any, extra ...any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	for i := 0; i+1 < len(extra); i += 2 {
		out[extra[i].(string)] = extra[i+1]
	}
	return out
}

func TestToolRequestOriginalArgsIsThePreRelayCall(t *testing.T) {
	host := &fakeExecutorHost{env: newTestToolEnv(t), sessionID: "s1"}
	d := NewDispatcher(NewRegistry())
	d.Relay = func(rctx context.Context, _ *Env, _ string, args map[string]any, _ ToolHookIDs, pipeline func(context.Context, map[string]any) (string, error)) (string, map[string]any, error) {
		relayArgs := copyArgs(args, "relay", true)
		result, err := pipeline(rctx, relayArgs)
		return result, relayArgs, err
	}
	var seenArgs, seenOriginal string
	d.ToolRequestMiddleware = []ToolRequestMiddleware{
		func(_ context.Context, _ *Env, _ string, args, _ map[string]any, _ ToolHookIDs) RequestMiddlewareDirective {
			return RequestMiddlewareDirective{Args: copyArgs(args, "steps", []any{"first"}), Source: "first"}
		},
		func(_ context.Context, _ *Env, _ string, args, original map[string]any, _ ToolHookIDs) RequestMiddlewareDirective {
			if steps, ok := args["steps"].([]any); ok {
				args["steps"] = append(steps, "meddler")
			}
			if input, ok := original["input"].([]any); ok {
				original["input"] = append(input, "meddler")
			}
			return RequestMiddlewareDirective{}
		},
		func(_ context.Context, _ *Env, _ string, args, original map[string]any, _ ToolHookIDs) RequestMiddlewareDirective {
			seenArgs, seenOriginal = fmt.Sprint(args), fmt.Sprint(original)
			return RequestMiddlewareDirective{Args: copyArgs(args, "second", true), Source: "second"}
		},
	}
	e := NewExecutor(host, d)
	var dispatched []string
	_, err := e.runToolExecutionMiddleware(context.Background(), managedInput{name: "fixture", args: map[string]any{"input": []any{"original"}},
		taskID: "t1", toolCallID: "c1", execute: func(_ context.Context, args map[string]any) (string, error) {
			dispatched = append(dispatched, fmt.Sprint(args))
			return "", nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if seenArgs != "map[input:[original] relay:true steps:[first]]" || seenOriginal != "map[input:[original]]" {
		t.Fatalf("second middleware saw %s original %s", seenArgs, seenOriginal)
	}
	if fmt.Sprint(dispatched) != "[map[input:[original] relay:true second:true steps:[first]]]" {
		t.Fatalf("dispatched %v", dispatched)
	}
}

func TestManagedToolPipelineRunsThroughTheSessionRelayRuntime(t *testing.T) {
	f, ctx, _ := relayToolsTurn(t)
	f.requests = append(f.requests, func(_ string, args map[string]any) map[string]any { return copyArgs(args, "relay", true) })
	var seenMetadata []map[string]any
	managed := f.relay.Tools.Execute
	f.relay.Tools.Execute = func(c context.Context, name string, args any, callback RelayProviderCallback, options map[string]any) (any, error) {
		metadata, _ := options["metadata"].(map[string]any)
		seenMetadata = append(seenMetadata, metadata)
		return managed(c, name, args, callback, options)
	}
	host := &fakeExecutorHost{env: newTestToolEnv(t), sessionID: "session-1"}
	e := NewExecutor(host, NewDispatcher(NewRegistry()))
	var dispatched []map[string]any
	var guardDepth []int
	outcome, err := e.runToolExecutionMiddleware(ctx, managedInput{name: "terminal", args: map[string]any{"command": "true"},
		taskID: "task-1", toolCallID: "call-1", execute: func(c context.Context, args map[string]any) (string, error) {
			dispatched = append(dispatched, args)
			guardDepth = append(guardDepth, relayManagedCallbackDepthVar.Get(c).(int))
			return "ok", nil
		}})
	if err != nil || outcome.Result != "ok" || !outcome.Dispatched {
		t.Fatalf("outcome %+v err %v", outcome, err)
	}
	if fmt.Sprint(dispatched) != "[map[command:true relay:true]]" || fmt.Sprint(outcome.Args) != "map[command:true relay:true]" {
		t.Fatalf("the relay rewrite did not reach the dispatched call, dispatched %v args %v", dispatched, outcome.Args)
	}
	if fmt.Sprint(guardDepth) != "[1]" {
		t.Fatalf("the tool ran outside the managed callback guard, depths %v", guardDepth)
	}
	want := "[map[api_request_id: task_id:task-1 tool_call_id:call-1 turn_id:]]"
	if fmt.Sprint(seenMetadata) != want {
		t.Fatalf("relay metadata %v, want %s", seenMetadata, want)
	}
}
