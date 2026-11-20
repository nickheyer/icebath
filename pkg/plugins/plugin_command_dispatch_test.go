package plugins

import (
	"context"
	"reflect"
	"testing"
)

// test_handlers_get_only_the_context_they_declare over peer signatures, plus the opaque legacy call
func TestCommandCallArgsBySignature(t *testing.T) {
	context := map[string]any{"source": "src", "session_id": "sid", "session_key": "key", "platform": "telegram"}
	p := func(name string, kind int) wireParam { return wireParam{Name: name, Kind: kind} }
	cases := []struct {
		name string
		sig  []wireParam
		want map[string]any
	}{
		{"opaque", nil, nil},
		{"legacy", []wireParam{p("raw_args", paramPositionalOrKeyword)}, map[string]any{}},
		{"named", []wireParam{p("raw_args", paramPositionalOrKeyword), p("session_id", paramKeywordOnly), p("platform", paramKeywordOnly)}, map[string]any{"session_id": "sid", "platform": "telegram"}},
		{"catch_all", []wireParam{p("args", paramPositionalOrKeyword), p("kwargs", paramVarKeyword)}, context},
		{"positional_only", []wireParam{p("raw_args", paramPositionalOnly), p("session_id", paramPositionalOnly)}, map[string]any{}},
		{"first_named_source", []wireParam{p("source", paramPositionalOrKeyword), p("kwargs", paramVarKeyword)}, map[string]any{"session_id": "sid", "session_key": "key", "platform": "telegram"}},
		{"first_positional_only", []wireParam{p("source", paramPositionalOnly), p("kwargs", paramVarKeyword)}, context},
		{"keyword_only_first", []wireParam{p("session_key", paramKeywordOnly)}, map[string]any{"session_key": "key"}},
	}
	for _, c := range cases {
		args, kwargs := commandCallArgs(c.sig, "a", context)
		if !reflect.DeepEqual(args, []any{"a"}) || !reflect.DeepEqual(kwargs, c.want) {
			t.Fatalf("%s got %v %v", c.name, args, kwargs)
		}
	}
}

// An in process handler reads the context its surface passed, a nested direct call gets none after a claim
func TestInProcessCommandContext(t *testing.T) {
	var seen map[string]any
	handler := func(ctx context.Context, raw string) (any, error) {
		seen = PluginCommandContext(ctx)
		_, claimed := claimCommandContext(ctx)
		if !reflect.DeepEqual(claimed, seen) {
			t.Fatalf("claim %v", claimed)
		}
		if again := PluginCommandContext(ctx); len(again) != 0 {
			t.Fatalf("claimed context leaked %v", again)
		}
		return raw + "!", nil
	}
	out, err := InvokePluginCommand(context.Background(), handler, "go", map[string]any{"session_id": "s1"})
	if err != nil || out != "go!" || seen["session_id"] != "s1" {
		t.Fatalf("got %v %v %v", out, err, seen)
	}
	if out, _ := InvokePluginCommand(context.Background(), func(ctx context.Context, raw string) (any, error) { return len(PluginCommandContext(ctx)), nil }, "", nil); out != 0 {
		t.Fatalf("no context %v", out)
	}
}
