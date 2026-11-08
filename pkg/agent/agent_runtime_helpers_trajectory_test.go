package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
)

func trajectoryValues(t *testing.T, traj []any) []string {
	t.Helper()
	out := make([]string, len(traj))
	for i, turn := range traj {
		obj := turn.(*pyjson.Object)
		from, _ := obj.Get("from")
		value, _ := obj.Get("value")
		s, _ := value.(string)
		out[i] = from.(string) + ":" + s
	}
	return out
}

func TestConvertToTrajectoryFormatToolRunAndThinkBlocks(t *testing.T) {
	a := &AIAgent{}
	messages := []map[string]any{
		{"role": "user", "content": "find it"},
		{"role": "assistant", "content": "<REASONING_SCRATCHPAD>look</REASONING_SCRATCHPAD>", "reasoning": "native",
			"tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "search", "arguments": `{"q": "ü"}`}},
				map[string]any{"id": "c2", "function": map[string]any{"name": "read", "arguments": "not json"}}}},
		{"role": "tool", "tool_call_id": "c1", "content": `{"hits": 1}`},
		{"role": "tool", "content": "plain"},
		{"role": "assistant", "content": "done"},
		{"role": "user", "content": "thanks"},
	}
	traj, err := a.convertToTrajectoryFormat(messages, "find it")
	if err != nil {
		t.Fatal(err)
	}
	got := trajectoryValues(t, traj)
	if !strings.HasPrefix(got[0], "system:You are a function calling AI model.") || !strings.Contains(got[0], "<tools>\n[]\n</tools>") ||
		!strings.Contains(got[0], "{'title': 'FunctionCall'") {
		t.Fatalf("system %q", got[0])
	}
	want := []string{
		"human:find it",
		"gpt:<think>\nnative\n</think>\n<think>look</think>\n<tool_call>\n{\"name\": \"search\", \"arguments\": {\"q\": \"ü\"}}\n</tool_call>\n<tool_call>\n{\"name\": \"read\", \"arguments\": {}}\n</tool_call>",
		"tool:<tool_response>\n{\"tool_call_id\": \"c1\", \"name\": \"search\", \"content\": {\"hits\": 1}}\n</tool_response>\n<tool_response>\n{\"tool_call_id\": \"\", \"name\": \"read\", \"content\": \"plain\"}\n</tool_response>",
		"gpt:<think>\n</think>\ndone",
		"human:thanks",
	}
	if strings.Join(got[1:], "\n||\n") != strings.Join(want, "\n||\n") {
		t.Fatalf("trajectory\n%s", strings.Join(got[1:], "\n||\n"))
	}
}

func TestConvertToTrajectoryFormatReplacesImagesAndRaisesOnMissingRole(t *testing.T) {
	a := &AIAgent{}
	traj, err := a.convertToTrajectoryFormat([]map[string]any{{"role": "user"}, {"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:x"}}, map[string]any{"type": "text", "text": "hi"}}}}, "q")
	if err != nil {
		t.Fatal(err)
	}
	value, _ := traj[2].(*pyjson.Object).Get("value")
	if first := value.([]any)[0].(map[string]any); first["text"] != "[screenshot]" {
		t.Fatalf("image part %v", value)
	}
	if _, err := a.convertToTrajectoryFormat([]map[string]any{{"role": "user"}, {"content": "x"}}, "q"); err == nil || err.Error() != "KeyError: 'role'" {
		t.Fatalf("err %v", err)
	}
}

func TestDumpAPIRequestDebugWritesARedactedDump(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	logs := t.TempDir()
	a := &AIAgent{logsDir: logs, sessionID: "../evil", baseURL: "https://api.example.com/v1/", apiMode: "chat_completions"}
	apiErr := &provider.APIError{StatusCode: 400, Body: `{"error": {"message": "bad"}}`, RequestID: "req_1"}
	path := a.dumpAPIRequestDebug(map[string]any{"model": "m", "timeout": 5.0, "skip": nil,
		"messages": []any{map[string]any{"role": "user", "content": "key sk-abcdefghijklmnopqrstuvwxyz0123456789"}}}, "max_retries_exhausted", apiErr)
	if path == "" || filepath.Dir(path) != logs || !strings.HasPrefix(filepath.Base(path), "request_dump_") {
		t.Fatalf("path %q", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var dump map[string]any
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatal(err)
	}
	req := dump["request"].(map[string]any)
	body := req["body"].(map[string]any)
	if req["url"] != "https://api.example.com/v1/chat/completions" || req["headers"].(map[string]any)["Authorization"] != "Bearer None" {
		t.Fatalf("request %v", req)
	}
	if _, has := body["timeout"]; has || body["skip"] != nil || strings.Contains(string(raw), "sk-abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("body must drop transport keys and redact secrets: %s", raw)
	}
	info := dump["error"].(map[string]any)
	if info["status_code"] != 400.0 || info["request_id"] != "req_1" || info["response_status"] != 400.0 {
		t.Fatalf("error info %v", info)
	}
	blocker := filepath.Join(logs, "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a.logsDir = blocker
	if got := a.dumpAPIRequestDebug(map[string]any{"odd": make(chan int)}, "x", errors.New("e")); got != "" {
		t.Fatalf("an unwritable dump reports no file, got %q", got)
	}
}

func TestDirectNativeAnthropicToolCacheCapability(t *testing.T) {
	for _, c := range []struct {
		mode, url string
		want      bool
	}{
		{"anthropic_messages", "https://api.anthropic.com", true},
		{"anthropic_messages", "https://API.Anthropic.com./v1", true},
		{"anthropic_messages", "https://proxy.test/api.anthropic.com", false},
		{"chat_completions", "https://api.anthropic.com", false},
	} {
		a := &AIAgent{apiMode: c.mode, baseURL: c.url}
		if got := a.directNativeAnthropicToolCacheCapability(); got != c.want {
			t.Errorf("%s %s: %v", c.mode, c.url, got)
		}
	}
}

func TestAPIRequestPayloadForHookDropsTransportKeys(t *testing.T) {
	a := &AIAgent{}
	payload := a.apiRequestPayloadForHook(map[string]any{"model": "m", "timeout": 3, "http_client": "x", "api_key": "secret"}).(map[string]any)
	body := payload["body"].(map[string]any)
	if payload["method"] != "POST" || body["model"] != "m" || body["timeout"] != nil || body["http_client"] != nil || body["api_key"] == "secret" {
		t.Fatalf("payload %v", payload)
	}
}
