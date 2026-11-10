package fakellm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, s *Server, body map[string]any, key string) (*http.Response, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, s.BaseURL()+"/chat/completions", bytes.NewReader(raw))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func mainBody(stream bool) map[string]any {
	return map[string]any{"model": ModelID, "stream": stream, "messages": []any{}, "tools": []any{map[string]any{"type": "function"}}}
}

func TestScriptIsConsumedInOrderThenTheDefaultTextAnswers(t *testing.T) {
	s, err := New([]any{ToolCall{Name: "read_file", Args: map[string]any{"path": "/n"}}, Text{Text: "done"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	var first, second, third struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
			FinishReason string `json:"finish_reason"`
		}
		Usage map[string]any
	}
	_, raw := post(t, s, mainBody(false), "")
	json.Unmarshal(raw, &first)
	_, raw = post(t, s, mainBody(false), "")
	json.Unmarshal(raw, &second)
	_, raw = post(t, s, mainBody(false), "")
	json.Unmarshal(raw, &third)
	call := first.Choices[0].Message.ToolCalls
	if len(call) != 1 || call[0].ID != "call_fake_1" || call[0].Function.Name != "read_file" || call[0].Function.Arguments != `{"path":"/n"}` ||
		first.Choices[0].FinishReason != "tool_calls" || first.Choices[0].Message.Content != nil || first.Usage["total_tokens"] != 110.0 {
		t.Fatalf("first %s", raw)
	}
	if *second.Choices[0].Message.Content != "done" || second.Choices[0].FinishReason != "stop" || second.Usage["total_tokens"] != 120.0 {
		t.Fatalf("second %+v", second)
	}
	if *third.Choices[0].Message.Content != "ok" {
		t.Fatalf("third %+v", third)
	}
	_, raw = post(t, s, map[string]any{"messages": []any{}}, "")
	if !strings.Contains(string(raw), "Fake summary of the earlier conversation.") {
		t.Fatalf("aux %s", raw)
	}
	if len(s.MainRequests()) != 3 || len(s.AuxRequests()) != 1 || s.Requests()[0].Response != "ToolCall" || s.Requests()[3].Kind != "aux" {
		t.Fatalf("recorded %d main %d aux", len(s.MainRequests()), len(s.AuxRequests()))
	}
	if err := s.WaitForRequests(3, time.Second, "main"); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForRequests(9, 50*time.Millisecond, "main"); err == nil || err.Error() != "expected 9 main requests, saw 4 total" {
		t.Fatalf("got %v", err)
	}
}

func streamEvents(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &event); err != nil {
			t.Fatal(err)
		}
		out = append(out, event)
	}
	return out
}

func deltaOf(event map[string]any) map[string]any {
	return event["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
}

func TestStreamingSplitsTextAndToolCallsIntoChunks(t *testing.T) {
	note := "lead in"
	s, err := New([]any{Text{Text: "twelve chars", Reasoning: "why", ChunkChars: 5},
		ToolCall{Name: "a", Args: map[string]any{"k": 1.0}, Parallel: []Call{{Name: "b"}}, Text: &note}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	resp, raw := post(t, s, mainBody(true), "")
	if resp.Header.Get("Content-Type") != "text/event-stream" || !strings.HasSuffix(string(raw), "data: [DONE]\n\n") {
		t.Fatalf("stream %q %s", resp.Header.Get("Content-Type"), raw)
	}
	events := streamEvents(t, raw)
	var content, reasoning []string
	for _, e := range events {
		if c, ok := deltaOf(e)["content"].(string); ok && deltaOf(e)["role"] == nil {
			content = append(content, c)
		}
		if r, ok := deltaOf(e)["reasoning_content"].(string); ok {
			reasoning = append(reasoning, r)
		}
	}
	last := events[len(events)-1]
	if strings.Join(content, "|") != "twelv|e cha|rs" || strings.Join(reasoning, "|") != "why" ||
		last["choices"].([]any)[0].(map[string]any)["finish_reason"] != "stop" || last["usage"] == nil {
		t.Fatalf("content %q reasoning %q last %v", content, reasoning, last)
	}
	_, raw = post(t, s, mainBody(true), "")
	events = streamEvents(t, raw)
	var names, arguments []string
	for _, e := range events {
		calls, _ := deltaOf(e)["tool_calls"].([]any)
		for _, c := range calls {
			fn := c.(map[string]any)["function"].(map[string]any)
			if name, ok := fn["name"].(string); ok {
				names = append(names, name)
			}
			if args, _ := fn["arguments"].(string); args != "" {
				arguments = append(arguments, args)
			}
		}
	}
	if strings.Join(names, ",") != "a,b" || strings.Join(arguments, "|") != `{"k":1}|{}` || deltaOf(events[1])["content"] != "lead in" {
		t.Fatalf("names %q arguments %q", names, arguments)
	}
}

func TestFaultsKeysAndModelsListing(t *testing.T) {
	after := 2.5
	s, err := New([]any{Error{Status: 429, Message: "slow down", RetryAfter: &after}, Raw{Body: "this is not json"},
		DropMidStream{Text: "partial answer", AfterChars: 7}, Hang{Seconds: 0.05}}, Options{APIKeys: []string{"k1", "k2"}, RecordGet: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	resp, raw := post(t, s, mainBody(false), "nope")
	if resp.StatusCode != 401 || !strings.Contains(string(raw), "invalid api key") {
		t.Fatalf("auth %d %s", resp.StatusCode, raw)
	}
	resp, raw = post(t, s, mainBody(false), "k2")
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "2.5" || !strings.Contains(string(raw), "slow down") {
		t.Fatalf("error %d %s", resp.StatusCode, raw)
	}
	if _, raw = post(t, s, mainBody(false), "k1"); string(raw) != "this is not json" {
		t.Fatalf("raw %s", raw)
	}
	_, raw = post(t, s, mainBody(true), "k1")
	if events := streamEvents(t, raw); len(events) != 2 || deltaOf(events[1])["content"] != "partial" || strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("drop %s", raw)
	}
	body, _ := json.Marshal(mainBody(false))
	req, _ := http.NewRequest(http.MethodPost, s.BaseURL()+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer k1")
	if hung, err := (&http.Client{Timeout: 5 * time.Second}).Do(req); err == nil {
		hung.Body.Close()
		t.Fatal("a hang answered")
	}
	got, err := http.Get(s.BaseURL() + "/models")
	if err != nil {
		t.Fatal(err)
	}
	listing, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if !strings.Contains(string(listing), `"id":"fake-model"`) || s.Requests()[len(s.Requests())-1].Kind != "get" {
		t.Fatalf("models %s", listing)
	}
	if s.DropUnconsumed() != 0 {
		t.Fatal("script not consumed")
	}
}

func TestWriteHomeRoutesTheMainModelToTheServer(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := WriteHome(home, "http://127.0.0.1:9/v1", "", "plugins:\n  enabled: [p]\n", 2); err != nil {
		t.Fatal(err)
	}
	config, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
	want := "model:\n  provider: custom\n  base_url: http://127.0.0.1:9/v1\n  default: fake-model\n  context_length: 128000\nagent:\n  api_max_retries: 1\n" +
		"telemetry:\n  shared_metrics:\n    enabled: false\n    send: false\n    offer_version: 2\nplugins:\n  enabled: [p]\n"
	env, _ := os.ReadFile(filepath.Join(home, ".env"))
	if string(config) != want || string(env) != "OPENAI_API_KEY=sk-fake-e2e\n" {
		t.Fatalf("config %q env %q", config, env)
	}
}
