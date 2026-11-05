package provider

import (
	"context"
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

// Recorded request of a fake Gemini endpoint
type gemRecorded struct {
	URL     string
	Headers http.Header
	Body    map[string]any
}

// Fake Gemini endpoint behind an httptest server, every request rerouted to it with its original URL recorded
type gemFake struct {
	mu       sync.Mutex
	server   *httptest.Server
	requests []gemRecorded
}

func newGemFake(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int)) *gemFake {
	t.Helper()
	f := &gemFake{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n := len(f.requests) - 1
		f.mu.Unlock()
		handler(w, r, n)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// Transport rewriting every request to the fake server after recording it
func (f *gemFake) RoundTrip(r *http.Request) (*http.Response, error) {
	data, _ := io.ReadAll(r.Body)
	rec := gemRecorded{URL: r.URL.String(), Headers: r.Header.Clone()}
	if len(data) > 0 {
		rec.Body = gemMap(string(data))
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	f.mu.Unlock()
	target, _ := url.Parse(f.server.URL)
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host, out.Host = target.Scheme, target.Host, target.Host
	out.Body = io.NopCloser(strings.NewReader(string(data)))
	out.ContentLength = int64(len(data))
	return http.DefaultTransport.RoundTrip(out)
}

func (f *gemFake) all() []gemRecorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gemRecorded{}, f.requests...)
}

// Writes a JSON body with a status
func gemWriteJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// Writes SSE data frames, one per event
func gemWriteSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	for _, e := range events {
		_, _ = io.WriteString(w, "data: "+e+"\n\n")
	}
}

// Native client on a fake endpoint with an isolated home
func gemClient(t *testing.T, f *gemFake, baseURL, apiKey, model string) *geminiNativeClient {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c, err := newGeminiNativeClient(Route{Provider: "gemini", Model: model, BaseURL: baseURL, APIKey: apiKey, Mode: ModeGeminiNative, Auth: AuthAPIKey, Transport: f})
	if err != nil {
		t.Fatal(err)
	}
	return c.(*geminiNativeClient)
}

func gemUserRequest(model, text string) *Request {
	return &Request{Model: model, Messages: []*state.Message{{Role: "user", Content: text}}}
}

func TestGeminiFollowupUserTurnIsNotMergedIntoFunctionResponseTurn(t *testing.T) {
	contents, _ := buildGeminiContents(gemList(`[
		{"role": "user", "content": "Load the skill"},
		{"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "skill_view", "arguments": "{\"name\":\"icebath-agent\"}"}}]},
		{"role": "tool", "tool_call_id": "call_1", "content": "loaded"},
		{"role": "user", "content": "Continue"}]`), false, false)
	gemEq(t, gemRoles(contents), `["user","model","user","model","user"]`)
	if _, ok := gemAt(contents[2], "parts", 0).(map[string]any)["functionResponse"]; !ok {
		t.Fatalf("no functionResponse: %v", contents[2])
	}
	gemEq(t, contents[3]["parts"], `[{"text": "[The previous response was interrupted before it completed.]"}]`)
	gemEq(t, contents[4]["parts"], `[{"text": "Continue"}]`)
}

func TestGeminiParallelToolResultsMergeIntoOneUserContent(t *testing.T) {
	contents, _ := buildGeminiContents(gemList(`[
		{"role": "user", "content": "Read a.txt and b.txt"},
		{"role": "assistant", "content": "", "tool_calls": [
			{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\": \"a.txt\"}"}},
			{"id": "call_2", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\": \"b.txt\"}"}}]},
		{"role": "tool", "tool_call_id": "call_1", "content": "AAA"},
		{"role": "tool", "tool_call_id": "call_2", "content": "BBB"}]`), false, false)
	gemEq(t, gemRoles(contents), `["user","model","user"]`)
	var outputs []any
	for _, p := range contents[2]["parts"].([]any) {
		if fr, ok := p.(map[string]any)["functionResponse"]; ok {
			outputs = append(outputs, gemAt(fr, "response", "output"))
		}
	}
	gemEq(t, outputs, `["AAA","BBB"]`)
}

func TestGeminiConsecutiveUserMessagesMergeForAlternation(t *testing.T) {
	contents, _ := buildGeminiContents(gemList(`[{"role": "user", "content": "first"}, {"role": "user", "content": "second"}, {"role": "assistant", "content": "ok"}]`), false, false)
	gemEq(t, gemRoles(contents), `["user","model"]`)
}

func TestGeminiSchemaBearingToolResultIsWrappedAsOpaqueText(t *testing.T) {
	schema := `{"$defs": {"SetCookieParam": {"type": "object"}}, "properties": {"cookies": {"$ref": "#/$defs/SetCookieParam"}}}`
	out := translateToolResultToGemini(map[string]any{"role": "tool", "tool_call_id": "call_1", "name": "tool_describe", "content": schema}, nil, true, false)
	resp := gemAt(out, "functionResponse", "response").(map[string]any)
	if _, ok := resp["$defs"]; ok {
		t.Fatal("schema forwarded as structured response")
	}
	if !strings.Contains(resp["output"].(string), "#/$defs/SetCookieParam") {
		t.Fatalf("output %v", resp)
	}
}

func TestGeminiPlainJSONToolResultRemainsStructured(t *testing.T) {
	out := translateToolResultToGemini(map[string]any{"role": "tool", "tool_call_id": "call_2", "name": "some_tool", "content": `{"status": "ok", "count": 3}`}, nil, false, false)
	gemEq(t, gemAt(out, "functionResponse", "response"), `{"status": "ok", "count": 3}`)
}

func TestGeminiDeeplyNestedRefIsDetected(t *testing.T) {
	out := translateToolResultToGemini(map[string]any{"role": "tool", "tool_call_id": "call_3", "name": "some_tool", "content": `{"a": [{"b": {"c": [{"$ref": "#/$defs/Deep"}]}}]}`}, nil, false, false)
	if !strings.Contains(gemAt(out, "functionResponse", "response", "output").(string), "#/$defs/Deep") {
		t.Fatalf("nested ref not demoted: %v", out)
	}
}

func TestGeminiTopLevelJSONArrayIsWrappedAsOpaqueText(t *testing.T) {
	out := translateToolResultToGemini(map[string]any{"role": "tool", "tool_call_id": "call_4", "name": "some_tool", "content": `[{"$ref": "#/$defs/SetCookieParam", "type": "object"}]`}, nil, false, false)
	resp := gemAt(out, "functionResponse", "response").(map[string]any)
	if _, ok := resp["output"]; !ok {
		t.Fatalf("array not wrapped: %v", resp)
	}
	if _, ok := resp["$ref"]; ok {
		t.Fatal("ref leaked")
	}
}

func TestGeminiRefValueWithoutPointerPrefixRemainsStructured(t *testing.T) {
	payload := `{"$ref": "not-a-pointer", "status": "ok"}`
	out := translateToolResultToGemini(map[string]any{"role": "tool", "tool_call_id": "call_5", "name": "some_tool", "content": payload}, nil, false, false)
	gemEq(t, gemAt(out, "functionResponse", "response"), payload)
}

func TestGeminiTranslateNativeResponseSurfacesReasoningAndToolCalls(t *testing.T) {
	resp := translateGeminiResponse(gemMap(`{"candidates": [{"content": {"parts": [{"thought": true, "text": "thinking..."},
		{"functionCall": {"name": "search", "args": {"q": "icebath"}}}]}, "finishReason": "STOP"}],
		"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15}}`), "gemini-2.5-flash")
	choice := gemAt(resp, "choices", 0)
	if gemAt(choice, "finish_reason") != "tool_calls" || gemAt(choice, "message", "reasoning") != "thinking..." {
		t.Fatalf("choice %v", choice)
	}
	if gemAt(choice, "message", "tool_calls", 0, "function", "name") != "search" {
		t.Fatalf("tool call %v", choice)
	}
	gemEq(t, gemJS(gemAt(choice, "message", "tool_calls", 0, "function", "arguments").(string)), `{"q": "icebath"}`)
}

func TestGeminiNativeClientUsesXGoogAPIKeyAndNativeModelsEndpoint(t *testing.T) {
	f := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		gemWriteJSON(w, 200, `{"candidates": [{"content": {"parts": [{"text": "hello"}]}, "finishReason": "STOP"}],
			"usageMetadata": {"promptTokenCount": 1, "candidatesTokenCount": 1, "totalTokenCount": 2}}`)
	})
	c := gemClient(t, f, "https://generativelanguage.googleapis.com/v1beta", "AIza-test", "gemini-2.5-flash")
	resp, err := c.Complete(context.Background(), gemUserRequest("gemini-2.5-flash", "Hello"))
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.all()
	if reqs[0].URL != "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("url %s", reqs[0].URL)
	}
	if reqs[0].Headers.Get("x-goog-api-key") != "AIza-test" || reqs[0].Headers.Get("Authorization") != "" {
		t.Fatalf("headers %v", reqs[0].Headers)
	}
	if resp.Message.Content != "hello" {
		t.Fatalf("content %v", resp.Message.Content)
	}
}

func TestGeminiNormalizeBaseURLGuaranteesVersionSegment(t *testing.T) {
	for _, tc := range [][2]string{
		{"", "https://generativelanguage.googleapis.com/v1beta"},
		{"https://generativelanguage.googleapis.com", "https://generativelanguage.googleapis.com/v1beta"},
		{"http://localhost:4000/gemini///", "http://localhost:4000/gemini/v1beta"},
		{"https://proxy.example/gemini/v1alpha/", "https://proxy.example/gemini/v1alpha"},
		{"https://proxy.example/v1", "https://proxy.example/v1"},
		{"https://generativelanguage.googleapis.com/v1beta/openai", "https://generativelanguage.googleapis.com/v1beta"},
	} {
		if got := normalizeGeminiBaseURL(tc[0]); got != tc[1] {
			t.Errorf("normalize(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}

func TestGeminiNativeClientRejectsEmptyAPIKeyWithActionableMessage(t *testing.T) {
	for _, bad := range []string{"", "   "} {
		_, err := newGeminiNativeClient(Route{Provider: "gemini", Model: "gemini-2.5-flash", APIKey: bad, Mode: ModeGeminiNative})
		if err == nil || !strings.Contains(err.Error(), "GOOGLE_API_KEY") || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
			t.Fatalf("key %q: %v", bad, err)
		}
	}
}

func TestGeminiNativeClientStreamsConcurrentlyFromOneClient(t *testing.T) {
	f := newGemFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		gemWriteSSE(w, `{"candidates": [{"content": {"parts": [{"text": "hi"}]}, "finishReason": "STOP"}]}`)
	})
	c := gemClient(t, f, "https://generativelanguage.googleapis.com/v1beta", "AIza-test", "gemini-2.5-flash")
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var texts []string
			resp, err := c.Stream(context.Background(), gemUserRequest("gemini-2.5-flash", "Hello"), func(ev StreamEvent) error {
				if ev.Kind == EventTextDelta {
					texts = append(texts, ev.Text)
				}
				return nil
			})
			if err == nil && (strings.Join(texts, "") != "hi" || resp.Message.Content != "hi") {
				err = &json.UnsupportedValueError{Str: "collected " + strings.Join(texts, "|")}
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Index and arguments of every tool call delta in translated chunks
func gemToolCallDeltas(chunks []map[string]any) [][2]any {
	var out [][2]any
	for _, c := range chunks {
		calls, _ := gemAt(c, "choices", 0, "delta", "tool_calls").([]any)
		for _, tc := range calls {
			out = append(out, [2]any{gemAt(tc, "index"), gemAt(tc, "function", "arguments")})
		}
	}
	return out
}

// One functionCall stream event
func gemFCEvent(name string, args string, callID, finish string) map[string]any {
	fc := map[string]any{"name": name, "args": gemMap(args)}
	if callID != "" {
		fc["id"] = callID
	}
	cand := map[string]any{"content": map[string]any{"parts": []any{map[string]any{"functionCall": fc}}}}
	if finish != "" {
		cand["finishReason"] = finish
	}
	return map[string]any{"candidates": []any{cand}}
}

func TestGeminiStreamEventTranslationEmitsToolCallDeltaWithStableIndex(t *testing.T) {
	slots := newGeminiToolSlots()
	event := gemMap(`{"candidates": [{"content": {"parts": [{"functionCall": {"name": "search", "args": {"q": "abc"}}}]}, "finishReason": "STOP"}]}`)
	first := translateGeminiStreamEvent(event, "gemini-2.5-flash", slots)
	second := translateGeminiStreamEvent(event, "gemini-2.5-flash", slots)
	tc1, tc2 := gemAt(first[0], "choices", 0, "delta", "tool_calls", 0), gemAt(second[0], "choices", 0, "delta", "tool_calls", 0)
	if gemAt(tc1, "index") != 0 || gemAt(tc2, "index") != 0 || gemAt(tc1, "id") != gemAt(tc2, "id") {
		t.Fatalf("slots %v %v", tc1, tc2)
	}
	if gemAt(tc1, "function", "arguments") != `{"q": "abc"}` || gemAt(tc2, "function", "arguments") != "" {
		t.Fatalf("args %v %v", tc1, tc2)
	}
	if gemAt(first[len(first)-1], "choices", 0, "finish_reason") != "tool_calls" {
		t.Fatalf("finish %v", first[len(first)-1])
	}
}

func TestGeminiDistinctSameToolCallsInSeparateEventsGetTheirOwnSlots(t *testing.T) {
	slots := newGeminiToolSlots()
	var out []map[string]any
	for _, args := range []string{`{"q": "A"}`, `{"q": "B"}`, `{"q": "B"}`} {
		out = append(out, translateGeminiStreamEvent(gemFCEvent("search", args, "", ""), "gemini-2.5-flash", slots)...)
	}
	deltas := gemToolCallDeltas(out)
	gemEq(t, deltas, `[[0, "{\"q\": \"A\"}"], [1, "{\"q\": \"B\"}"], [1, ""]]`)
	for _, d := range deltas[:2] {
		gemJS(d[1].(string))
	}
}

func TestGeminiProviderCallIDIsTheSlotIdentity(t *testing.T) {
	slots := newGeminiToolSlots()
	out := translateGeminiStreamEvent(gemFCEvent("search", `{"q": "A"}`, "c1", ""), "gemini-3-pro", slots)
	out = append(out, translateGeminiStreamEvent(gemFCEvent("search", `{"q": "A"}`, "c2", ""), "gemini-3-pro", slots)...)
	resend := gemMap(`{"candidates": [{"content": {"parts": [{"text": "x"}, {"functionCall": {"name": "search", "args": {"q": "A"}, "id": "c1"}, "thoughtSignature": "sig"}]}}]}`)
	out = append(out, translateGeminiStreamEvent(resend, "gemini-3-pro", slots)...)
	gemEq(t, gemToolCallDeltas(out), `[[0, "{\"q\": \"A\"}"], [1, "{\"q\": \"A\"}"], [0, ""]]`)
	var ids []any
	for _, c := range out {
		calls, _ := gemAt(c, "choices", 0, "delta", "tool_calls").([]any)
		for _, tc := range calls {
			ids = append(ids, gemAt(tc, "id"))
		}
	}
	gemEq(t, ids, `["c1","c2","c1"]`)
}
