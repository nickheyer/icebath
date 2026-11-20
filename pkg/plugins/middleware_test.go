package plugins

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Registers first, meddler and second for both request kinds, recording what second saw
func requestChainPlugin(t *testing.T) (*Manager, map[string][2]any) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "request_chain"}, m)
	seen := map[string][2]any{}
	for _, kk := range [][2]string{{LLMRequestMiddleware, "request"}, {ToolRequestMiddleware, "args"}} {
		kind, key := kk[0], kk[1]
		c.RegisterMiddleware(kind, func(kw map[string]any) (any, error) {
			next := copyKwargs(kw[key].(map[string]any))
			next["steps"] = []any{"first"}
			return map[string]any{key: next, "source": "first"}, nil
		})
		c.RegisterMiddleware(kind, func(kw map[string]any) (any, error) {
			payload := kw[key].(map[string]any)
			steps, _ := payload["steps"].([]any)
			payload["steps"] = append(steps, "meddler")
			orig := kw["original_"+key].(map[string]any)
			orig["input"] = append(orig["input"].([]any), "meddler")
			return nil, nil
		})
		c.RegisterMiddleware(kind, func(kw map[string]any) (any, error) {
			seen[kind] = [2]any{safeCopy(kw[key]), safeCopy(kw["original_"+key])}
			next := copyKwargs(kw[key].(map[string]any))
			next["second"] = true
			return map[string]any{key: next, "source": "second"}, nil
		})
	}
	return m, seen
}

func applyKind(m *Manager, kind string, payload map[string]any) RequestMiddlewareResult {
	if kind == LLMRequestMiddleware {
		return m.ApplyLLMRequestMiddleware(payload, map[string]any{})
	}
	result, err := m.ApplyToolRequestMiddleware(context.Background(), "fixture", payload, nil, map[string]any{})
	if err != nil {
		panic(err)
	}
	return result
}

func TestRequestMiddlewareRewritesCompose(t *testing.T) {
	for _, kind := range []string{LLMRequestMiddleware, ToolRequestMiddleware} {
		m, seen := requestChainPlugin(t)
		result := applyKind(m, kind, map[string]any{"input": []any{"original"}})
		if !reflect.DeepEqual(seen[kind][0], map[string]any{"input": []any{"original"}, "steps": []any{"first"}}) {
			t.Fatalf("%s second saw %#v", kind, seen[kind][0])
		}
		want := map[string]any{"input": []any{"original"}, "steps": []any{"first"}, "second": true}
		if !reflect.DeepEqual(result.Payload, want) || !result.Changed {
			t.Fatalf("%s payload %#v", kind, result.Payload)
		}
		var sources []any
		for _, e := range result.Trace {
			sources = append(sources, e["source"])
		}
		if !reflect.DeepEqual(sources, []any{"first", "second"}) {
			t.Fatal(sources)
		}
	}
}

func TestRequestMiddlewareInPlaceMutationDoesNotLeak(t *testing.T) {
	for _, kind := range []string{LLMRequestMiddleware, ToolRequestMiddleware} {
		m, seen := requestChainPlugin(t)
		original := map[string]any{"input": []any{"original"}}
		result := applyKind(m, kind, original)
		for _, s := range result.Payload.(map[string]any)["steps"].([]any) {
			if s == "meddler" {
				t.Fatalf("%s meddler leaked", kind)
			}
		}
		pristine := map[string]any{"input": []any{"original"}}
		if !reflect.DeepEqual(seen[kind][1], pristine) || !reflect.DeepEqual(result.OriginalPayload, pristine) || !reflect.DeepEqual(original, pristine) {
			t.Fatalf("%s original leaked %#v %#v", kind, seen[kind][1], result.OriginalPayload)
		}
	}
}

func TestToolRequestWithoutMiddlewareReturnsArgsUnchanged(t *testing.T) {
	m := NewManager(t.TempDir())
	args := map[string]any{"a": 1}
	result, err := m.ApplyToolRequestMiddleware(context.Background(), "fixture", args, nil, map[string]any{"skip_relay": true, "session_id": "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || !reflect.DeepEqual(result.Payload, args) || len(result.Trace) != 0 {
		t.Fatalf("%#v", result)
	}
}

func onExecMisdeclared(map[string]any) (any, error) {
	return nil, errors.New("on_exec() missing 1 required positional argument: 'tool_data'")
}

func TestMiddlewareFailureWarnsOnceAndUnloadForgetsIt(t *testing.T) {
	logs := captureLogs(t)
	m := NewManager(t.TempDir())
	add := func() {
		m.middleware["agent_tool_execution"] = append(m.middleware["agent_tool_execution"], &middlewareEntry{fn: onExecMisdeclared, name: "onExecMisdeclared"})
	}
	add()
	for i := 0; i < 3; i++ {
		m.InvokeMiddleware("agent_tool_execution", "", map[string]any{"tool_name": "x", "args": map[string]any{"path": i}})
	}
	m.resetAfterUnloadAll(nil)
	if len(m.hookFailuresReported) != 0 {
		t.Fatal("failures not forgotten")
	}
	add()
	m.InvokeMiddleware("agent_tool_execution", "", map[string]any{"tool_name": "x", "args": map[string]any{}})
	warnings := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "onExecMisdeclared") {
			warnings++
			if !strings.Contains(line, "Middleware 'agent_tool_execution'") {
				t.Fatal(line)
			}
		}
	}
	if warnings != 2 {
		t.Fatalf("%d warnings\n%s", warnings, logs)
	}
}

func TestExecutionChainMiddlewareFailureWarnsOnce(t *testing.T) {
	logs := captureLogs(t)
	m := NewManager(t.TempDir())
	m.middleware[ToolExecutionMiddleware] = []*middlewareEntry{{fn: onExecMisdeclared, name: "onExecMisdeclared"}}
	for i := 0; i < 4; i++ {
		got, err := m.RunToolExecutionMiddleware("read_file", map[string]any{"path": i}, func(any) (any, error) { return "ran", nil }, nil)
		if err != nil || got != "ran" {
			t.Fatal(got, err)
		}
	}
	warnings, debugs := 0, 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, "onExecMisdeclared") {
			continue
		}
		if strings.Contains(line, "level=WARN") {
			warnings++
		} else if strings.Contains(line, "level=DEBUG") {
			debugs++
		}
	}
	if warnings != 1 || debugs != 3 {
		t.Fatalf("warnings %d debugs %d", warnings, debugs)
	}
}

func TestExecutionChainNextCallIsSingleUseAndDownstreamErrorsPassThrough(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "exec"}, m)
	c.RegisterMiddleware(ToolExecutionMiddleware, func(kw map[string]any) (any, error) {
		next := kw["next_call"].(NextCall)
		first, err := next(nil)
		if err != nil {
			return nil, err
		}
		if _, err := next(nil); err == nil || !strings.Contains(err.Error(), "more than once") {
			return nil, errors.New("second next_call allowed")
		}
		return first, nil
	})
	got, err := m.RunToolExecutionMiddleware("t", map[string]any{}, func(any) (any, error) { return "ok", nil }, nil)
	if err != nil || got != "ok" {
		t.Fatal(got, err)
	}
	boom := errors.New("tool exploded")
	if _, err := m.RunToolExecutionMiddleware("t", map[string]any{}, func(any) (any, error) { return nil, boom }, nil); err != boom {
		t.Fatalf("downstream error rewrapped: %v", err)
	}
	if !m.Unload("exec") || m.HasMiddleware(ToolExecutionMiddleware) {
		t.Fatal("unload left middleware registered")
	}
}
