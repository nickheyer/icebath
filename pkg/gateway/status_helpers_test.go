package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Replaces a seam for the duration of the test
func patch[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// Ordered record of alternating keys and values
func rec(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func strs(items ...string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}

func writeRecord(t *testing.T, path string, r *pyjson.Object) {
	t.Helper()
	text, err := pyjson.Dumps(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readRecord(t *testing.T, path string) *pyjson.Object {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pyjson.Loads(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*pyjson.Object)
}

// Temp process home for the status tests, the runtime snapshot reloaded for it
func statusHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	clearRunningPidCache()
	return dir
}

func fixedInt(v any) func(int) any { return func(int) any { return v } }

func noCmdline(int) (string, bool) { return "", false }

func cmdline(s string) func(int) (string, bool) { return func(int) (string, bool) { return s, true } }
