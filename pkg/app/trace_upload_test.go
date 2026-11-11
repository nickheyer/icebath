package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func traceSampleMessages() []map[string]any {
	return []map[string]any{
		{"role": "system", "content": "you are icebath"},
		{"role": "user", "content": "list files"},
		{"role": "assistant", "content": "Listing.", "tool_calls": []any{
			map[string]any{"id": "call_1", "function": map[string]any{"name": "terminal", "arguments": `{"command": "ls"}`}},
		}},
		{"role": "tool", "tool_call_id": "call_1", "tool_name": "terminal", "content": "a.txt\nb.txt"},
		{"role": "assistant", "content": "Two files."},
	}
}

func traceLines(t *testing.T, jsonl string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(jsonl), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		out = append(out, m)
	}
	return out
}

func TestConverterEmitsToolUseAndToolResult(t *testing.T) {
	jsonl, err := buildTraceJSONL(context.Background(), traceSampleMessages(), "s1", "m", "", true)
	if err != nil {
		t.Fatal(err)
	}
	lines := traceLines(t, jsonl)
	if lines[0]["type"] != "user" || lines[1]["type"] != "assistant" || lines[2]["type"] != "user" {
		t.Fatalf("types %v %v %v", lines[0]["type"], lines[1]["type"], lines[2]["type"])
	}
	blocks := lines[1]["message"].(map[string]any)["content"].([]any)
	hasText := false
	var toolUse []map[string]any
	for _, b := range blocks {
		block := b.(map[string]any)
		hasText = hasText || block["type"] == "text"
		if block["type"] == "tool_use" {
			toolUse = append(toolUse, block)
		}
	}
	if !hasText || len(toolUse) == 0 || toolUse[0]["name"] != "terminal" {
		t.Fatalf("blocks %v", blocks)
	}
	if input := toolUse[0]["input"].(map[string]any); len(input) != 1 || input["command"] != "ls" {
		t.Fatalf("input %v", input)
	}
	tr := lines[2]["message"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" || !strings.Contains(tr["content"].(string), "a.txt") {
		t.Fatalf("tool result %v", tr)
	}
}

func TestConverterRedactsSecretsByDefault(t *testing.T) {
	msgs := []map[string]any{{"role": "user", "content": "key OPENAI_API_KEY=sk-abc123def456ghi789jklmno end"}}
	jsonl, err := buildTraceJSONL(context.Background(), msgs, "s1", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsonl, "sk-abc123def456ghi789jklmno") {
		t.Fatalf("secret kept %s", jsonl)
	}
}

func TestConverterKeepsSecretsWhenRedactDisabled(t *testing.T) {
	secret := "sk-abc123def456ghi789jklmno"
	msgs := []map[string]any{{"role": "user", "content": "key OPENAI_API_KEY=" + secret + " end"}}
	jsonl, err := buildTraceJSONL(context.Background(), msgs, "s1", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonl, secret) {
		t.Fatalf("secret dropped %s", jsonl)
	}
}

// Loopback Hub answering whoami, repo create, preupload and commit while recording the calls
type traceFakeHub struct {
	mu      sync.Mutex
	creates []map[string]any
	commits [][]map[string]any
}

func (h *traceFakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/whoami-v2":
		io.WriteString(w, `{"name": "alice"}`)
	case r.Method == "POST" && r.URL.Path == "/api/repos/create":
		var m map[string]any
		json.Unmarshal(body, &m)
		h.creates = append(h.creates, m)
		io.WriteString(w, `{"url": "https://huggingface.co/datasets/alice/icebath-traces"}`)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/preupload/main"):
		var m struct{ Files []struct{ Path string } }
		json.Unmarshal(body, &m)
		files := []map[string]any{}
		for _, f := range m.Files {
			files = append(files, map[string]any{"path": f.Path, "uploadMode": "regular", "shouldIgnore": false})
		}
		json.NewEncoder(w).Encode(map[string]any{"files": files})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commit/main"):
		var lines []map[string]any
		sc := bufio.NewScanner(bytes.NewReader(body))
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var m map[string]any
			json.Unmarshal(sc.Bytes(), &m)
			lines = append(lines, m)
		}
		h.commits = append(h.commits, lines)
		io.WriteString(w, `{"commitUrl": "https://huggingface.co/datasets/alice/icebath-traces/commit/1", "commitOid": "1"}`)
	default:
		http.NotFound(w, r)
	}
}

func TestUploadHappyPathMocked(t *testing.T) {
	h := sessionsTestHome(t)
	t.Setenv("HF_TOKEN", "hf_test")
	hub := &traceFakeHub{}
	srv := httptest.NewServer(hub)
	defer srv.Close()
	t.Setenv("HF_ENDPOINT", srv.URL)
	db := sessionsTestDB(t, h)
	if err := db.CreateSession(state.NewSession{ID: "20260531_abc", Source: "cli", Model: "claude-x"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range traceSampleMessages()[1:] {
		if _, err := db.AppendMessage(context.Background(), "20260531_abc", m, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	msg, err := uploadSessionTrace(context.Background(), "20260531_abc", t.TempDir(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "huggingface.co/datasets/alice/icebath-traces") {
		t.Fatalf("status %q", msg)
	}
	if len(hub.creates) != 1 {
		t.Fatalf("creates %v", hub.creates)
	}
	create := hub.creates[0]
	if create["name"] != "icebath-traces" || create["organization"] != "alice" || create["type"] != "dataset" || create["visibility"] != "private" {
		t.Fatalf("create %v", create)
	}
	if len(hub.commits) != 1 || len(hub.commits[0]) != 2 {
		t.Fatalf("commits %v", hub.commits)
	}
	file := hub.commits[0][1]["value"].(map[string]any)
	if file["path"] != "sessions/20260531_abc.jsonl" {
		t.Fatalf("file %v", file)
	}
	raw, err := base64.StdEncoding.DecodeString(file["content"].(string))
	if err != nil {
		t.Fatal(err)
	}
	first := traceLines(t, string(raw))[0]
	if (first["type"] != "user" && first["type"] != "assistant") || first["sessionId"] != "20260531_abc" {
		t.Fatalf("first line %v", first)
	}
}
