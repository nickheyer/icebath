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
	d.Relay = func(_ context.Context, _ *Env, _ string, args map[string]any, _ ToolHookIDs, pipeline func(map[string]any) (string, error)) (string, map[string]any, error) {
		result, err := pipeline(args)
		if _, again := pipeline(args); again != nil {
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
	d.Relay = func(_ context.Context, _ *Env, _ string, args map[string]any, _ ToolHookIDs, pipeline func(map[string]any) (string, error)) (string, map[string]any, error) {
		relayArgs := copyArgs(args, "relay", true)
		result, err := pipeline(relayArgs)
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
