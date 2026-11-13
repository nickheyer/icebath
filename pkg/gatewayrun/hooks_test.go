package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Hook dir with HOOK.yaml and handler.py like the create hook helper
func createHook(t *testing.T, hooksDir, hookName, events, handlerCode string) string {
	t.Helper()
	dir := filepath.Join(hooksDir, hookName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: " + hookName + "\ndescription: Test hook\nevents: " + events + "\n"
	if err := os.WriteFile(filepath.Join(dir, "HOOK.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "handler.py"), []byte(handlerCode), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Lines the hook handler appended to its results file
func hookResults(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Python string literal of s
func pyQuote(s string) string {
	out := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' || s[i] == '\'' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
}

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
