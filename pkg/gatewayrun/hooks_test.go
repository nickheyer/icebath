package gatewayrun

import (
	"context"
	"reflect"
	"testing"
)

func TestCollectsSyncReturnValues(t *testing.T) {
	reg := NewHookRegistry()
	reg.handlers["command:status"] = []hookHandler{
		func(context.Context, string, map[string]any) (any, error) {
			return map[string]any{"decision": "allow"}, nil
		},
		func(context.Context, string, map[string]any) (any, error) {
			return map[string]any{"decision": "deny", "message": "nope"}, nil
		},
	}
	got := reg.emitCollect(context.Background(), "command:status", map[string]any{})
	want := []any{map[string]any{"decision": "allow"}, map[string]any{"decision": "deny", "message": "nope"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestDropsNoneReturnValues(t *testing.T) {
	reg := NewHookRegistry()
	reg.handlers["command:x"] = []hookHandler{
		func(context.Context, string, map[string]any) (any, error) { return nil, nil },
		func(context.Context, string, map[string]any) (any, error) {
			return map[string]any{"decision": "deny"}, nil
		},
		func(context.Context, string, map[string]any) (any, error) { return nil, nil },
	}
	got := reg.emitCollect(context.Background(), "command:x", map[string]any{})
	if !reflect.DeepEqual(got, []any{map[string]any{"decision": "deny"}}) {
		t.Fatalf("got %v", got)
	}
}
