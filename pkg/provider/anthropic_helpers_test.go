package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

type anthCaptured struct {
	URL    string
	Header http.Header
	Body   map[string]any
}

type anthServer struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []anthCaptured
	replies []func(w http.ResponseWriter, body map[string]any)
}

// Fake Messages endpoint recording every request and answering from a reply queue, the last reply repeating
func newAnthServer(t *testing.T, replies ...func(w http.ResponseWriter, body map[string]any)) *anthServer {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	s := &anthServer{t: t, replies: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		n := len(s.reqs) - 1
		if n >= len(s.replies) {
			n = len(s.replies) - 1
		}
		var reply func(http.ResponseWriter, map[string]any)
		if n >= 0 {
			reply = s.replies[n]
		}
		s.mu.Unlock()
		if reply == nil {
			anthReplyJSON(200, anthMessage("end_turn", anthText("ok")))(w, body)
			return
		}
		reply(w, body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// RoundTrip records the request under its original URL then forwards it to the fake server
func (s *anthServer) RoundTrip(req *http.Request) (*http.Response, error) {
	var raw []byte
	if req.Body != nil {
		raw, _ = io.ReadAll(req.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.mu.Lock()
	s.reqs = append(s.reqs, anthCaptured{URL: req.URL.String(), Header: req.Header.Clone(), Body: body})
	s.mu.Unlock()
	target, _ := url.Parse(s.srv.URL)
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = target.Scheme, target.Host, target.Host
	out.Body = io.NopCloser(bytes.NewReader(raw))
	return http.DefaultTransport.RoundTrip(out)
}

func (s *anthServer) route(baseURL, key string) Route {
	return Route{Provider: "anthropic", Model: "claude-sonnet-4-20250514", BaseURL: baseURL, APIKey: key, Mode: ModeAnthropicMessages, Transport: s}
}

func (s *anthServer) client(route Route) *anthropicClient {
	s.t.Helper()
	c, err := newAnthropicClient(route)
	if err != nil {
		s.t.Fatalf("newAnthropicClient: %v", err)
	}
	return c.(*anthropicClient)
}

func (s *anthServer) requests() []anthCaptured {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]anthCaptured(nil), s.reqs...)
}

func (s *anthServer) last() anthCaptured {
	s.t.Helper()
	reqs := s.requests()
	if len(reqs) == 0 {
		s.t.Fatal("no request reached the fake Messages endpoint")
	}
	return reqs[len(reqs)-1]
}

func anthText(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

func anthToolUse(id, name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

func anthMessage(stop string, blocks ...map[string]any) map[string]any {
	content := make([]any, 0, len(blocks))
	for _, b := range blocks {
		content = append(content, b)
	}
	return map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "model": "claude-sonnet-4-20250514",
		"content": content, "stop_reason": stop, "usage": map[string]any{"input_tokens": 100, "output_tokens": 50}}
}

func anthReplyJSON(status int, payload any) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}
}

// SSE reply writing each event as an event and data frame pair
func anthReplySSE(events ...map[string]any) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		var b strings.Builder
		for _, ev := range events {
			data, _ := json.Marshal(ev)
			b.WriteString("event: " + asStr(ev["type"]) + "\ndata: " + string(data) + "\n\n")
		}
		_, _ = w.Write([]byte(b.String()))
	}
}

func anthUserRequest(model, text string) *Request {
	return &Request{Model: model, Messages: []*state.Message{{Role: "user", Content: text}}, MaxTokens: 1024}
}

// Decodes a JSON literal into plain data so tests read like the Python dict fixtures
func anthJSON(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("bad JSON fixture: %v\n%s", err, text)
	}
	return v
}

func anthMsgs(t *testing.T, text string) []map[string]any {
	t.Helper()
	list, _ := anthJSON(t, text).([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		out = append(out, item.(map[string]any))
	}
	return out
}

func anthBlocks(v any) []map[string]any {
	list, _ := asList(v)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := asMap(item); ok {
			out = append(out, m)
		}
	}
	return out
}

func anthFirstRole(msgs []map[string]any, role string) map[string]any {
	for _, m := range msgs {
		if asStr(m["role"]) == role {
			return m
		}
	}
	return nil
}

func anthJSONEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func anthMustEqualJSON(t *testing.T, got, want any) {
	t.Helper()
	if !anthJSONEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Fatalf("mismatch\n got  %s\n want %s", gj, wj)
	}
}

// Every text block of the payload or nested tool result whose text is empty or whitespace
func anthBlankTextBlocks(messages []map[string]any) []string {
	var out []string
	blank := func(b map[string]any) bool {
		s, ok := b["text"].(string)
		return asStr(b["type"]) == "text" && !(ok && strings.TrimSpace(s) != "")
	}
	for i, m := range messages {
		for j, b := range anthBlocks(m["content"]) {
			if blank(b) {
				out = append(out, asStr(m["role"])+" content "+string(rune('0'+i))+string(rune('0'+j)))
			}
			if asStr(b["type"]) == "tool_result" {
				for k, inner := range anthBlocks(b["content"]) {
					if blank(inner) {
						out = append(out, asStr(m["role"])+" tool_result "+string(rune('0'+i))+string(rune('0'+k)))
					}
				}
			}
		}
	}
	return out
}
