package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Fresh registry holding one core tool per handler
func registryWith(handlers map[string]Handler) *Dispatcher {
	r := NewRegistry()
	for name, h := range handlers {
		r.Register(Def{Name: name, Toolset: "core", Schema: map[string]any{"name": name, "description": "Test tool " + name,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}}}, Handler: h})
	}
	return NewDispatcher(r)
}

func constHandler(out string) Handler { return func(context.Context, *Call) string { return out } }

func recordingEnv() (*Env, *recordingHandler) {
	h := &recordingHandler{}
	return &Env{Log: slog.New(h)}, h
}

func (h *recordingHandler) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.records))
	for i, r := range h.records {
		out[i] = r.Message
	}
	return out
}

func decodeRegistryResult(t *testing.T, s string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("result %q is not a JSON object: %v", s, err)
	}
	return out
}

func TestRegistryRegisterDispatchAndUnknownTool(t *testing.T) {
	d := registryWith(map[string]Handler{"alpha": constHandler(`{"ok": true}`)})
	if out := decodeRegistryResult(t, d.RegistryDispatch(context.Background(), &Call{Name: "alpha", Args: map[string]any{}})); len(out) != 1 || out["ok"] != true {
		t.Fatalf("alpha %v", out)
	}
	if out := decodeRegistryResult(t, d.RegistryDispatch(context.Background(), &Call{Name: "nonexistent", Args: map[string]any{}})); out["error"] != "Unknown tool: nonexistent" {
		t.Fatalf("unknown %v", out)
	}
}

func TestToolErrorBounding(t *testing.T) {
	marker := "… [truncated]"
	if out := decodeRegistryResult(t, toolErrorJSON("Missing required parameter: query", nil)); out["error"] != "Missing required parameter: query" {
		t.Fatalf("short %v", out)
	}
	if out := decodeRegistryResult(t, toolErrorJSON("bad input", map[string]any{"success": false})); out["error"] != "bad input" || out["success"] != false {
		t.Fatalf("extra kwargs %v", out)
	}
	if e := decodeRegistryResult(t, toolErrorJSON("boom: "+strings.Repeat("X", 5000), nil))["error"].(string); !strings.HasSuffix(e, marker) || pystr.Len(e) > maxToolErrorChars+pystr.Len(marker) {
		t.Fatalf("oversized length %d", pystr.Len(e))
	}
	if msg := strings.Repeat("Y", maxToolErrorChars); decodeRegistryResult(t, toolErrorJSON(msg, nil))["error"] != msg {
		t.Fatal("message at the limit truncated")
	}
	env, logs := recordingEnv()
	body := "boom: " + strings.Repeat("Z", 5000)
	if got := boundModelErrorText(env, body); pystr.Len(got) >= 5000 || !strings.Contains(strings.Join(logs.messages(), "\n"), body[:5000]) {
		t.Fatalf("context length %d logs %d", pystr.Len(got), len(logs.messages()))
	}
	env, logs = recordingEnv()
	huge := "boom: " + strings.Repeat("Z", 500_000)
	boundModelErrorText(env, huge)
	for _, m := range logs.messages() {
		if pystr.Len(m) >= maxLoggedErrorChars+200 || strings.Contains(m, huge) {
			t.Fatalf("log line length %d", pystr.Len(m))
		}
	}
	wide := strings.Repeat("é", maxToolErrorChars+5)
	if got := boundModelErrorText(nil, wide); got != strings.Repeat("é", maxToolErrorChars)+marker {
		t.Fatalf("code point cut length %d", pystr.Len(got))
	}
	if got := boundModelErrorText(nil, strings.Repeat("é", maxToolErrorChars)); got != strings.Repeat("é", maxToolErrorChars) {
		t.Fatal("two byte message at the code point limit truncated")
	}
}

func TestDispatchBoundsDirectErrorResults(t *testing.T) {
	marker := "… [truncated]"
	small := `{"error": "not found", "success": false}`
	bigData := `{"data": "` + strings.Repeat("D", 50_000) + `"}`
	plain := strings.Repeat("plain text ", 10_000)
	nested := `{"error": {"detail": "` + strings.Repeat("E", 5_000) + `"}}`
	d := registryWith(map[string]Handler{
		"direct":   constHandler(`{"status": "error", "error": "boom: ` + strings.Repeat("X", 50_000) + `", "tool_calls_made": 3, "duration_seconds": 1.2}`),
		"wide":     constHandler(`{"error": "` + strings.Repeat("é", 3000) + `", "n": 1}`),
		"small":    constHandler(small),
		"big_data": constHandler(bigData),
		"plain":    constHandler(plain),
		"nested":   constHandler(nested),
	})
	run := func(name string) string {
		return d.RegistryDispatch(context.Background(), &Call{Name: name, Args: map[string]any{}})
	}
	out := run("direct")
	result := decodeRegistryResult(t, out)
	e, _ := result["error"].(string)
	if !strings.HasSuffix(e, marker) || pystr.Len(e) > maxToolErrorChars+pystr.Len(marker) || result["status"] != "error" || result["tool_calls_made"] != float64(3) || result["duration_seconds"] != 1.2 {
		t.Fatalf("direct %v", result)
	}
	if !strings.HasPrefix(out, `{"status": "error", "error": "boom: XXX`) || !strings.HasSuffix(out, `[truncated]", "tool_calls_made": 3, "duration_seconds": 1.2}`) {
		t.Fatalf("direct result lost json.dumps key order or separators %q...%q", out[:40], out[len(out)-60:])
	}
	if out := run("wide"); out != `{"error": "`+strings.Repeat("é", maxToolErrorChars)+marker+`", "n": 1}` {
		t.Fatalf("non ascii error cut by bytes, length %d", pystr.Len(out))
	}
	for name, want := range map[string]string{"small": small, "big_data": bigData, "plain": plain, "nested": nested} {
		if got := run(name); got != want {
			t.Fatalf("%s result changed", name)
		}
	}
}

func TestDispatchExceptionLogging(t *testing.T) {
	body := "upstream said: " + strings.Repeat("Q", 200_000)
	d := registryWith(map[string]Handler{"boom": func(context.Context, *Call) string { panic(errors.New(body)) }})
	env, logs := recordingEnv()
	result := decodeRegistryResult(t, d.RegistryDispatch(context.Background(), &Call{Name: "boom", Args: map[string]any{}, Env: env}))
	messages := logs.messages()
	if len(messages) == 0 {
		t.Fatal("dispatch should log the failure")
	}
	for _, m := range messages {
		if pystr.Len(m) >= maxLoggedErrorChars+200 || strings.Contains(m, body) {
			t.Fatalf("log line length %d", pystr.Len(m))
		}
	}
	if e, _ := result["error"].(string); pystr.Len(e) >= maxToolErrorChars+200 || !strings.HasPrefix(e, "[TOOL_ERROR] Tool execution failed: errorString: upstream said: ") {
		t.Fatalf("error %.120q", e)
	}
}
