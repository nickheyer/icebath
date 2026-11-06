package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ---- think scrubber (tests/agent/test_think_scrubber.py) ----

func driveScrubber(deltas ...string) string {
	s := NewStreamingThinkScrubber()
	var out []string
	for _, d := range deltas {
		out = append(out, s.Feed(d))
	}
	out = append(out, s.Flush())
	return strings.Join(out, "")
}

func TestThinkScrubberClosedPairs(t *testing.T) {
	if got := driveScrubber("<think>reasoning</think>Hello world"); got != "Hello world" {
		t.Fatalf("got %q", got)
	}
	for _, tag := range []string{"think", "thinking", "reasoning", "thought", "REASONING_SCRATCHPAD"} {
		if got := driveScrubber(fmt.Sprintf("<%s>x</%s>Hello", tag, tag)); got != "Hello" {
			t.Fatalf("%s: got %q", tag, got)
		}
	}
}

func TestThinkScrubberUnterminatedOpen(t *testing.T) {
	if got := driveScrubber("<think>reasoning text with no close"); got != "" {
		t.Fatalf("got %q", got)
	}
	text := "Use the <think> element for reasoning"
	if got := driveScrubber(text); got != text {
		t.Fatalf("got %q", got)
	}
}

func TestThinkScrubberOrphanClose(t *testing.T) {
	if got := driveScrubber("Hello</think>world"); got != "Helloworld" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkScrubberPartialTagsAcrossDeltas(t *testing.T) {
	if got := driveScrubber("<", "think>reasoning</think>done"); got != "done" {
		t.Fatalf("got %q", got)
	}
	if got := driveScrubber("word<", "think>prose</think>more"); got != "wordmore" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkScrubberMiniMaxScenario(t *testing.T) {
	if got := driveScrubber("<think>", "Let me check their config", "</think>", "done"); got != "done" {
		t.Fatalf("got %q", got)
	}
	if got := driveScrubber("<think>", "The user wants", " to know something"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkScrubberResetAndReentry(t *testing.T) {
	s := NewStreamingThinkScrubber()
	s.Feed("<think>hanging")
	s.Reset()
	if got := s.Feed("Hello world") + s.Flush(); got != "Hello world" {
		t.Fatalf("got %q", got)
	}
	s = NewStreamingThinkScrubber()
	s.Feed("word<")
	s.Reset()
	if got := s.Feed("fresh content") + s.Flush(); got != "fresh content" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkScrubberFlushBehaviour(t *testing.T) {
	s := NewStreamingThinkScrubber()
	if got := s.Feed("word"); got != "word" {
		t.Fatalf("got %q", got)
	}
	if got := s.Flush(); got != "" {
		t.Fatalf("flush got %q", got)
	}
	var out []string
	for _, d := range []string{"<think>", "secret reasoning", "</think>", "Visible answer"} {
		out = append(out, s.Feed(d))
	}
	out = append(out, s.Flush())
	if got := strings.Join(out, ""); got != "Visible answer" {
		t.Fatalf("got %q", got)
	}
	s = NewStreamingThinkScrubber()
	s.Feed("word<")
	if got := s.Flush(); got != "<" {
		t.Fatalf("flush got %q", got)
	}
	if got := s.Feed("<think>hidden</think>Hello") + s.Flush(); got != "Hello" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkScrubberRealisticStreaming(t *testing.T) {
	var deltas []string
	for _, r := range "<think>x</think>Hello world" {
		deltas = append(deltas, string(r))
	}
	if got := driveScrubber(deltas...); got != "Hello world" {
		t.Fatalf("got %q", got)
	}
	if got := driveScrubber("<think>", "User wants to know things", "</think>", "Let me check their config."); got != "Let me check their config." {
		t.Fatalf("got %q", got)
	}
	if got := driveScrubber("Hello ", "world ", "how ", "are ", "you?"); got != "Hello world how are you?" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkScrubberChineseReasoningTags(t *testing.T) {
	if got := driveScrubber("<思", "考>让我想想", "……</思考>", "\n答案是 42"); got != "\n答案是 42" {
		t.Fatalf("got %q", got)
	}
	out := StripThinkBlocks("<反思>内部推理</反思>最终答案\n<推理>未闭合的推理")
	if strings.Contains(out, "内部推理") || strings.Contains(out, "未闭合") || !strings.Contains(out, "最终答案") {
		t.Fatalf("got %q", out)
	}
}

func TestThinkScrubberLastHidden(t *testing.T) {
	s := NewStreamingThinkScrubber()
	s.Feed("<think>secret</think>Hello")
	if s.LastHidden() != "secret" {
		t.Fatalf("hidden %q", s.LastHidden())
	}
	s = NewStreamingThinkScrubber()
	s.Feed("<think>")
	s.Feed("partial")
	if s.LastHidden() != "partial" {
		t.Fatalf("hidden %q", s.LastHidden())
	}
}

// ---- repetition guard (tests/agent/test_repetition_guard.py) ----

const incidentEcho = "好，你幫我更改成 Google Gemini 4 31B。"

func TestRepetitionGuardIncidentShape(t *testing.T) {
	text := strings.Repeat("We need to verify the model setting.\n"+incidentEcho+"\n", 800)
	if !IsRepetitionDominated(text) {
		t.Fatal("line path should flag")
	}
	text = strings.Repeat(incidentEcho, 2000)
	if len([]rune(text)) < MinFragmentLength || !IsRepetitionDominated(text) {
		t.Fatal("window path should flag")
	}
}

func TestRepetitionGuardMultilineParagraphRun(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	const alphabet = "abcdefghijklmnopqrstuvwxyz "
	var lines []string
	for i := 0; i < 5; i++ {
		var b strings.Builder
		for j := 0; j < 151; j++ {
			b.WriteByte(alphabet[rng.Intn(len(alphabet))])
		}
		lines = append(lines, b.String())
	}
	paragraph := strings.Join(lines, "\n") + "\n"
	for _, n := range []int{100, 1000, 10000} {
		if !IsRepetitionDominated(strings.Repeat(paragraph, n)) {
			t.Fatalf("repeat %d should flag", n)
		}
	}
}

func TestRepetitionGuardDominantRunWithUniqueEnds(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "Step %d: I will now carefully re-check the configuration file for the error again.\n", i)
	}
	if !IsRepetitionDominated(b.String()) {
		t.Fatal("counter loop should flag")
	}
	paragraph := "A deliberately long repeated paragraph has enough distinct text to make its period exceed the guard's minimum anchor length.\nIt also spans multiple lines, matching the real incident shape.\n"
	repeated := strings.Repeat(paragraph, 20)
	text := strings.Repeat("unique introduction ", 20) + repeated + strings.Repeat(" unique ending", 20)
	if float64(len(repeated)) <= float64(len(text))*0.5 || !IsRepetitionDominated(text) {
		t.Fatal("dominant run should flag")
	}
}

func TestRepetitionGuardNotFlagged(t *testing.T) {
	var parts []string
	for i := 0; i < 1200; i++ {
		parts = append(parts, fmt.Sprintf("Sentence number %d describes a distinct topic with unique words such as quasar-%d and nebula-%d to keep every window distinct.", i, i, i))
	}
	if IsRepetitionDominated(strings.Join(parts, " ")) {
		t.Fatal("unique prose flagged")
	}
	if IsRepetitionDominated(strings.Repeat("A. ", 50)) || IsRepetitionDominated("hello ") {
		t.Fatal("short fragment flagged")
	}
	var filler []string
	for i := 0; i < 3000; i++ {
		filler = append(filler, fmt.Sprintf("unique filler token %d", i))
	}
	text := strings.Join(filler, " ") + strings.Repeat("\n"+incidentEcho+"\n", 30)
	if IsRepetitionDominated(text) {
		t.Fatal("scattered repeat flagged")
	}
	if IsRepetitionDominated("") {
		t.Fatal("empty flagged")
	}
}

func TestRunawayRepetitionDistinctRowsDelivered(t *testing.T) {
	var rows strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&rows, "| %05d | status: pending review by the on-call engineer before the next release window |\n", i)
	}
	if IsRunawayRepetition(rows.String()) {
		t.Fatal("distinct rows must not be a runaway")
	}
	loop := strings.Repeat("De bot en de werkers blijven lopen. Ik zoek het punt waar de tekst binnenkomt. Daar stop ik de herhaling. De bot en de werkers blijven lopen. Ik zoek dat punt nu. ", 200)
	if !IsRunawayRepetition(loop) {
		t.Fatal("cycle must be a runaway")
	}
	w := NewRunawayStreamWatch()
	asked := strings.Repeat("Hello world, this is a sentence the user asked me to repeat many times.\n", 50)
	if w.Feed(asked) {
		t.Fatal("asked for repeat below the scale floor must not trip")
	}
	w = NewRunawayStreamWatch()
	tripped := false
	for i := 0; i < 400 && !tripped; i++ {
		tripped = w.Feed("De bot en de werkers blijven lopen. Ik zoek het punt waar de tekst binnenkomt. ")
	}
	if !tripped {
		t.Fatal("looping channel must trip the watch")
	}
}

// ---- tool call accumulator (tests/agent/test_chat_completion_helpers_tool_calls.py) ----

func accCalls(t *testing.T, deltas ...[3]string) [][2]string {
	t.Helper()
	acc := newCCToolCallAccumulator()
	for _, d := range deltas {
		fn := map[string]any{"arguments": d[2]}
		if d[1] != "" {
			fn["name"] = d[1]
		}
		acc.Feed(map[string]any{"index": float64(0), "id": d[0], "function": fn})
	}
	entries := acc.Materialize()
	var out [][2]string
	for _, idx := range acc.Slots() {
		out = append(out, [2]string{entries[idx].Name, entries[idx].Arguments})
	}
	return out
}

func TestToolCallAccumulatorNamelessFragmentsStayOneCall(t *testing.T) {
	got := accCalls(t, [3]string{"701601222", "terminal", ""}, [3]string{"tool-call-1", "", `{"com`}, [3]string{"tool-call-2", "", `mand": `}, [3]string{"tool-call-3", "", `"ls"}`})
	want := [][2]string{{"terminal", `{"command": "ls"}`}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v", got)
	}
}

func TestToolCallAccumulatorSameNameSplitsAfterCompleteArguments(t *testing.T) {
	got := accCalls(t,
		[3]string{"mm-1", "terminal", `{"command": `},
		[3]string{"mm-2", "terminal", `"echo \"}{\""}`},
		[3]string{"mm-3", "terminal", `{"command": "pwd",}`},
		[3]string{"mm-4", "terminal", `{"command": "ls"}`},
		[3]string{"mm-5", "read_file", "{}"})
	want := [][2]string{{"terminal", `{"command": "echo \"}{\""}`}, {"terminal", `{"command": "pwd",}`}, {"terminal", `{"command": "ls"}`}, {"read_file", "{}"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v", got)
	}
}

// ---- reasoning summaries (tests/agent/test_reasoning_summaries.py) ----

func streamGlued(deltas ...string) string {
	var parts []string
	for _, d := range deltas {
		prev := ""
		if len(parts) > 0 {
			prev = parts[len(parts)-1]
		}
		parts = append(parts, separateGluedReasoningBlocks(prev, d))
	}
	return strings.Join(parts, "")
}

func TestSeparateGluedReasoningBlocks(t *testing.T) {
	text := streamGlued("**Investigating likely culprit PRs**", "**Inspecting message schema and tool_calls content**", "**Analyzing interrupted tool call impact**")
	if strings.Contains(text, "****") {
		t.Fatalf("glued: %q", text)
	}
	want := "**Investigating likely culprit PRs**\n\n**Inspecting message schema and tool_calls content**\n\n**Analyzing interrupted tool call impact**"
	if text != want {
		t.Fatalf("got %q", text)
	}
	text = streamGlued("**Simulating a greeting stream**\n\nIt feels like a streaming interaction!", "**Simulating a greeting stream**\n\nI want to meet the request.")
	if strings.Contains(text, "interaction!**") || !strings.Contains(text, "interaction!\n\n**Simulating") {
		t.Fatalf("got %q", text)
	}
	deltas := []string{"Looking at", " the session", " logs, I see", " one bold word."}
	if got := streamGlued(deltas...); got != strings.Join(deltas, "") {
		t.Fatalf("got %q", got)
	}
	if got := separateGluedReasoningBlocks("I see the ", "**signature**"); got != "**signature**" {
		t.Fatalf("got %q", got)
	}
	if got := separateGluedReasoningBlocks("weighing", "**"); got != "**" {
		t.Fatalf("got %q", got)
	}
	if got := separateGluedReasoningBlocks("**Closing**", "plain head"); got != "plain head" {
		t.Fatalf("got %q", got)
	}
	if got := separateGluedReasoningBlocks("", "**first**"); got != "**first**" {
		t.Fatalf("got %q", got)
	}
	if got := separateGluedReasoningBlocks("**first**", ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

// ---- streamed reasoning details (tests/agent/test_streamed_reasoning_details.py) ----

func TestStreamedReasoningDetailsMergeAndBackfill(t *testing.T) {
	var acc []map[string]any
	acc = appendStreamedReasoningDetail(acc, map[string]any{"type": "reasoning.text", "text": "The user "})
	acc = appendStreamedReasoningDetail(acc, map[string]any{"type": "reasoning.text", "text": "wants X.", "signature": "sig1"})
	acc = appendStreamedReasoningDetail(acc, map[string]any{"type": "reasoning.encrypted", "data": "AAAA"})
	acc = appendStreamedReasoningDetail(acc, map[string]any{"type": "reasoning.encrypted", "data": "BBBB"})
	acc = appendStreamedReasoningDetail(acc, map[string]any{"type": "reasoning.summary", "summary": "s1 "})
	acc = appendStreamedReasoningDetail(acc, map[string]any{"type": "reasoning.summary", "summary": "s2"})
	var types []string
	for _, d := range acc {
		types = append(types, stringOf(d["type"]))
	}
	if fmt.Sprint(types) != "[reasoning.text reasoning.encrypted reasoning.encrypted reasoning.summary]" {
		t.Fatalf("types %v", types)
	}
	if acc[0]["text"] != "The user wants X." || acc[0]["signature"] != "sig1" || len(acc[0]) != 3 {
		t.Fatalf("first %v", acc[0])
	}
	if acc[3]["summary"] != "s1 s2" {
		t.Fatalf("summary %v", acc[3])
	}
	if streamedReasoningDetailText(map[string]any{"type": "reasoning.encrypted", "data": "x", "text": "not readable"}) != "" {
		t.Fatal("opaque detail leaked text")
	}
}

// ---- stream max lifetime (tests/agent/test_stream_max_lifetime.py) ----

func TestResolveStreamMaxLifetimeNeverUndercutsStale(t *testing.T) {
	if got := resolveStreamMaxLifetime(60, 180); got != 180 {
		t.Fatalf("got %v", got)
	}
	if got := resolveStreamMaxLifetime(60, inf()); got != 60 {
		t.Fatalf("got %v", got)
	}
	if got := resolveStreamMaxLifetime(0, 180); got != 0 {
		t.Fatalf("got %v", got)
	}
}

func inf() float64  { return 1 / zero() }
func zero() float64 { return 0 }

// ---- argument repair (agent/message_sanitization.py) ----

func TestRepairToolCallArguments(t *testing.T) {
	cases := map[string]string{
		`{"a": 1,}`:                    `{"a": 1}`,
		`{"items": [{"n": 1}, {"n": 2`: `{"items": [{"n": 1}, {"n": 2}]}`,
		`{"a": [{"b": 1}, {"c": 2}}`:   `{"a": [{"b": 1}, {"c": 2}]}`,
		`{"a": 1}}}`:                   `{"a": 1}`,
		"None":                         "{}",
		"":                             "{}",
		`{"path": "x`:                  "{}",
		`{"a": 1}`:                     `{"a":1}`,
	}
	for in, want := range cases {
		if got := RepairToolCallArguments(in, "t"); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
	got := RepairToolCallArguments("{\"code\": \"line1\nline2\"}", "t")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil || parsed["code"] != "line1\nline2" {
		t.Fatalf("control char repair got %q (%v)", got, err)
	}
}

// ---- wire fixtures ----

type wireRequest struct {
	Path    string
	Query   string
	Headers http.Header
	Body    map[string]any
}

type wireServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []wireRequest
}

func (w *wireServer) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.requests)
}

func (w *wireServer) request(i int) wireRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.requests[i]
}

// Fake chat completions endpoint recording every request before the handler answers it
func newWireServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int, body map[string]any)) *wireServer {
	t.Helper()
	t.Setenv(home.EnvHome, t.TempDir())
	ws := &wireServer{}
	ws.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		ws.mu.Lock()
		ws.requests = append(ws.requests, wireRequest{Path: r.URL.Path, Query: r.URL.RawQuery, Headers: r.Header.Clone(), Body: body})
		n := len(ws.requests)
		ws.mu.Unlock()
		handler(w, r, n, body)
	}))
	t.Cleanup(ws.srv.Close)
	return ws
}

func (w *wireServer) route(provider string) Route {
	return Route{Provider: provider, Model: "m", BaseURL: w.srv.URL + "/v1", APIKey: "test-key", Mode: ModeChatCompletions}
}

func sseFrames(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	for _, f := range frames {
		_, _ = io.WriteString(w, f)
		if fl != nil {
			fl.Flush()
		}
	}
}

func dataFrame(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

func chunk(delta map[string]any, finish any, extra map[string]any) string {
	c := map[string]any{"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "served-model",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	for k, v := range extra {
		c[k] = v
	}
	return dataFrame(c)
}

type eventLog struct {
	mu     sync.Mutex
	events []StreamEvent
}

func (l *eventLog) fn(ev StreamEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
	return nil
}

func (l *eventLog) texts(kind EventKind) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, ev := range l.events {
		if ev.Kind == kind {
			out = append(out, ev.Text)
		}
	}
	return out
}

func (l *eventLog) kinds() []EventKind {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []EventKind
	for _, ev := range l.events {
		out = append(out, ev.Kind)
	}
	return out
}

func userRequest(text string) *Request {
	return &Request{Model: "m", Messages: []*state.Message{{Role: "user", Content: text}}, Stream: true}
}

func newTestClient(t *testing.T, route Route) *chatCompletionsClient {
	t.Helper()
	c, err := newChatCompletionsClient(route)
	if err != nil {
		t.Fatal(err)
	}
	return c.(*chatCompletionsClient)
}

// ---- streaming behaviour ----

func TestStreamToolCallAccumulationAcrossDeltas(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		sseFrames(w,
			chunk(map[string]any{"role": "assistant", "content": nil}, nil, nil),
			chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "terminal", "arguments": ""}}}}, nil, nil),
			chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": `{"command": "ls`}}}}, nil, nil),
			chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": ` -la"}`}}}}, nil, nil),
			chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 1, "id": "call_2", "function": map[string]any{"name": "read_file", "arguments": `{"path": "a.txt"}`}}}}, nil, nil),
			chunk(map[string]any{}, "tool_calls", nil),
			dataFrame(map[string]any{"id": "chatcmpl-1", "choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "prompt_tokens_details": map[string]any{"cached_tokens": 4}}}),
			"data: [DONE]\n\n")
	})
	log := &eventLog{}
	resp, err := newTestClient(t, ws.route("custom")).Stream(context.Background(), userRequest("hi"), log.fn)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "tool_calls" || len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("finish %q calls %v", resp.FinishReason, resp.Message.ToolCalls)
	}
	if tc := resp.Message.ToolCalls[0]; tc.ID != "call_1" || tc.Function.Name != "terminal" || tc.Function.Arguments != `{"command": "ls -la"}` {
		t.Fatalf("first call %+v", tc)
	}
	if tc := resp.Message.ToolCalls[1]; tc.ID != "call_2" || tc.Function.Name != "read_file" {
		t.Fatalf("second call %+v", tc)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 || resp.Usage.TotalTokens != 15 || resp.Usage.CacheReadTokens != 4 {
		t.Fatalf("usage %+v", resp.Usage)
	}
	if resp.Model != "served-model" || resp.RequestID != "chatcmpl-1" {
		t.Fatalf("model %q id %q", resp.Model, resp.RequestID)
	}
	kinds := log.kinds()
	starts, deltas, dones := 0, 0, 0
	for _, k := range kinds {
		switch k {
		case EventToolCallStart:
			starts++
		case EventToolCallDelta:
			deltas++
		case EventToolCallDone:
			dones++
		}
	}
	if starts != 2 || deltas != 3 || dones != 2 || kinds[len(kinds)-1] != EventDone {
		t.Fatalf("events %v", kinds)
	}
	body := ws.request(0).Body
	if body["stream"] != true {
		t.Fatalf("stream flag missing: %v", body)
	}
	if so, ok := body["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Fatalf("stream_options missing: %v", body)
	}
	if auth := ws.request(0).Headers.Get("Authorization"); auth != "Bearer test-key" {
		t.Fatalf("auth %q", auth)
	}
}

func TestStreamChoicelessLastOneAndUsage(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		sseFrames(w,
			chunk(map[string]any{"content": "Hello"}, nil, nil),
			dataFrame(map[string]any{"id": "x", "choices": []any{}, "lastOne": true, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 1, "completion_tokens_details": map[string]any{"reasoning_tokens": 2}}}))
	})
	log := &eventLog{}
	resp, err := newTestClient(t, ws.route("custom")).Stream(context.Background(), userRequest("hi"), log.fn)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "stop" || resp.Message.Text() != "Hello" {
		t.Fatalf("resp %+v", resp)
	}
	if resp.Usage.ReasoningTokens != 2 || resp.Usage.TotalTokens != 4 {
		t.Fatalf("usage %+v", resp.Usage)
	}
	if texts := log.texts(EventTextDelta); strings.Join(texts, "") != "Hello" {
		t.Fatalf("deltas %v", texts)
	}
}

func TestStreamReasoningPassthroughAndDetailDeglue(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if n == 1 {
			sseFrames(w,
				chunk(map[string]any{"reasoning_content": "I should "}, nil, nil),
				chunk(map[string]any{"reasoning_content": "answer.", "reasoning_details": []any{map[string]any{"type": "reasoning.text", "text": "answer.", "signature": "sigZ"}}}, nil, nil),
				chunk(map[string]any{"content": "Hello!"}, "stop", nil),
				"data: [DONE]\n\n")
			return
		}
		sseFrames(w,
			chunk(map[string]any{"reasoning_details": []any{map[string]any{"type": "reasoning.summary", "summary": "**One**"}}}, nil, nil),
			chunk(map[string]any{"reasoning_details": []any{map[string]any{"type": "reasoning.summary", "summary": "**Two**"}}}, nil, nil),
			chunk(map[string]any{"content": "Answer"}, "stop", nil),
			"data: [DONE]\n\n")
	})
	c := newTestClient(t, ws.route("custom"))
	log := &eventLog{}
	resp, err := c.Stream(context.Background(), userRequest("hi"), log.fn)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log.texts(EventReasoningDelta), ""); got != "I should answer." {
		t.Fatalf("reasoning deltas %q", got)
	}
	if resp.Message.Text() != "Hello!" || resp.Message.Reasoning != "I should answer.\n\nanswer." || resp.Message.ReasoningContent != "I should answer." {
		t.Fatalf("message %+v", resp.Message)
	}
	var details []map[string]any
	if err := json.Unmarshal(resp.Message.ReasoningDetails, &details); err != nil || len(details) != 1 || details[0]["text"] != "answer." || details[0]["signature"] != "sigZ" {
		t.Fatalf("details %s", resp.Message.ReasoningDetails)
	}
	log = &eventLog{}
	resp, err = c.Stream(context.Background(), userRequest("hi"), log.fn)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log.texts(EventReasoningDelta), ""); got != "**One**\n\n**Two**" {
		t.Fatalf("deglued reasoning %q", got)
	}
	if resp.Message.ReasoningContent != "**One****Two**" || resp.Message.Reasoning != "**One****Two**" {
		t.Fatalf("details only reasoning %+v", resp.Message)
	}
}

func TestStreamThinkScrubForwardsHiddenReasoning(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		sseFrames(w,
			chunk(map[string]any{"content": "<think>"}, nil, nil),
			chunk(map[string]any{"content": "secret plan"}, nil, nil),
			chunk(map[string]any{"content": "</think>"}, nil, nil),
			chunk(map[string]any{"content": "\nHello"}, "stop", nil),
			"data: [DONE]\n\n")
	})
	log := &eventLog{}
	resp, err := newTestClient(t, ws.route("custom")).Stream(context.Background(), userRequest("hi"), log.fn)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(log.texts(EventTextDelta), ""); got != "Hello" {
		t.Fatalf("text %q", got)
	}
	if got := strings.Join(log.texts(EventReasoningDelta), ""); got != "secret plan" {
		t.Fatalf("hidden %q", got)
	}
	if resp.Message.Reasoning != "secret plan" || !strings.Contains(resp.Message.Text(), "<think>") {
		t.Fatalf("message %+v", resp.Message)
	}
}

func TestStreamOptionsRejectionRetriesWithoutThem(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if _, has := body["stream_options"]; has {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"Unrecognized request argument supplied: stream_options","type":"invalid_request_error"}}`)
			return
		}
		sseFrames(w, chunk(map[string]any{"content": "ok"}, "stop", nil), "data: [DONE]\n\n")
	})
	c := newTestClient(t, ws.route("custom"))
	resp, err := c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
	if err != nil || resp.Message.Text() != "ok" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if ws.count() != 2 {
		t.Fatalf("requests %d", ws.count())
	}
	if _, has := ws.request(1).Body["stream_options"]; has {
		t.Fatal("compat retry still sent stream_options")
	}
	if _, err := c.Stream(context.Background(), userRequest("again"), func(StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, has := ws.request(2).Body["stream_options"]; has || ws.count() != 3 {
		t.Fatalf("rejection not sticky: %d requests", ws.count())
	}
}

func TestStreamMetadataOffOmitsStreamOptions(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		sseFrames(w, chunk(map[string]any{"content": "ok"}, "stop", nil), "data: [DONE]\n\n")
	})
	req := userRequest("hi")
	req.Metadata = map[string]string{StreamOptionsOffKey: "off"}
	if _, err := newTestClient(t, ws.route("custom")).Stream(context.Background(), req, func(StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, has := ws.request(0).Body["stream_options"]; has {
		t.Fatal("stream_options sent despite metadata off")
	}
}

func TestStreamEmptyFrameAndNonJSONCodes(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("x-request-id", "req-9")
		if n == 1 {
			sseFrames(w, "event: ping\n\n")
			return
		}
		sseFrames(w, ": keepalive\n\n", "data: oops not json\n\n")
	})
	c := newTestClient(t, ws.route("custom"))
	_, err := c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != ProviderStreamEmptyFrameErrorCode {
		t.Fatalf("empty frame err %v", err)
	}
	var hc HeaderCarrier
	if !errors.As(err, &hc) || hc.ResponseHeaders().Get("x-request-id") != "req-9" {
		t.Fatalf("headers not carried: %v", err)
	}
	_, err = c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
	if !errors.As(err, &apiErr) || apiErr.Code != ProviderStreamNonJSONErrorCode || !strings.Contains(apiErr.Message, "oops not json") {
		t.Fatalf("non json err %v", err)
	}
}

func TestStreamInStreamErrorChunk(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		sseFrames(w, dataFrame(map[string]any{"choices": nil, "error_type": "validation_error", "error_message": "messages must not be empty"}))
	})
	_, err := newTestClient(t, ws.route("custom")).Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "messages must not be empty") || apiErr.Code != "validation_error" {
		t.Fatalf("err %v", err)
	}
	var empty *EmptyStreamError
	if errors.As(err, &empty) {
		t.Fatal("in stream error surfaced as empty stream")
	}
}

func TestStreamPartialStubs(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		switch n {
		case 1:
			sseFrames(w,
				chunk(map[string]any{"content": "Let me run "}, nil, nil),
				chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "function": map[string]any{"name": "terminal", "arguments": `{"command": "ls`}}}}, nil, nil))
		case 2:
			sseFrames(w, chunk(map[string]any{"content": "half an answer"}, nil, nil))
		default:
			sseFrames(w)
		}
	})
	c := newTestClient(t, ws.route("custom"))
	log := &eventLog{}
	resp, err := c.Stream(context.Background(), userRequest("hi"), log.fn)
	if err != nil {
		t.Fatal(err)
	}
	if !IsPartialStreamStub(resp) || resp.FinishReason != "length" || !resp.Truncated || !PartialStreamCleanEOF(resp) {
		t.Fatalf("stub %+v", resp)
	}
	if names := PartialStreamDroppedToolNames(resp); fmt.Sprint(names) != "[terminal]" {
		t.Fatalf("dropped %v", names)
	}
	if len(resp.Message.ToolCalls) != 0 {
		t.Fatal("incomplete tool call executed")
	}
	if texts := log.texts(EventTextDelta); len(texts) != 1 || texts[0] != "Let me run " {
		t.Fatalf("text before the tool call must stream: %v", texts)
	}
	resp, err = c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
	if err != nil || !IsPartialStreamStub(resp) || resp.Message.Text() != "half an answer" || len(PartialStreamDroppedToolNames(resp)) != 0 {
		t.Fatalf("text drop stub %+v err %v", resp, err)
	}
	_, err = c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
	var empty *EmptyStreamError
	if !errors.As(err, &empty) {
		t.Fatalf("empty stream err %v", err)
	}
}

func TestStreamRunawayRepetitionIsCut(t *testing.T) {
	cycle := "De bot en de werkers blijven lopen. Ik zoek het punt waar de tekst binnenkomt. Daar stop ik de herhaling. De bot en de werkers blijven lopen. Ik zoek dat punt nu. "
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for i := 0; i < 100000; i++ {
			if _, err := io.WriteString(w, chunk(map[string]any{"content": cycle}, nil, nil)); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	})
	start := time.Now()
	resp, err := newTestClient(t, ws.route("custom")).Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "length" || !ResponseRunawayRepetition(resp) || !resp.Truncated {
		t.Fatalf("runaway not marked: %+v", resp)
	}
	if ws.count() != 1 || time.Since(start) > 20*time.Second {
		t.Fatalf("requests %d elapsed %v", ws.count(), time.Since(start))
	}
}

func TestStreamStaleKillIsReadTimeout(t *testing.T) {
	t.Setenv(StreamStaleTimeoutEnv, "0.3")
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, chunk(map[string]any{"role": "assistant"}, nil, nil))
		fl.Flush()
		for i := 0; i < 20; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	})
	log := &eventLog{}
	start := time.Now()
	_, err := newTestClient(t, ws.route("custom")).Stream(context.Background(), userRequest("hi"), log.fn)
	if err == nil || TimeoutPhase(err) != TimeoutPhaseRead {
		t.Fatalf("err %v phase %q", err, TimeoutPhase(err))
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrTimeout {
		t.Fatalf("kind %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("stale kill took %v", time.Since(start))
	}
	statuses := log.texts(EventStatus)
	if len(statuses) != 1 || !strings.Contains(statuses[0], "No response from provider for 0s (model: m, context: ~") {
		t.Fatalf("status %v", statuses)
	}
}

func TestStreamLifetimeCapCutsADrip(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		short := strings.Contains(fmt.Sprint(body["messages"]), "short")
		count := 160
		if short {
			count = 1
		}
		for i := 0; i < count; i++ {
			if _, err := io.WriteString(w, chunk(map[string]any{"content": "."}, nil, nil)); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		_, _ = io.WriteString(w, chunk(map[string]any{}, "stop", nil)+"data: [DONE]\n\n")
		fl.Flush()
	})
	if err := os.WriteFile(home.ConfigPath(os.Getenv(home.EnvHome)), []byte("agent:\n  stream_max_lifetime: 1.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(StreamStaleTimeoutEnv, "1.0")
	c := newTestClient(t, ws.route("custom"))
	for i := 0; i < 2; i++ {
		start := time.Now()
		_, err := c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
		var cut *StreamLifetimeExceededError
		if !errors.As(err, &cut) || !strings.Contains(err.Error(), "agent.stream_max_lifetime") {
			t.Fatalf("err %v", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("cap took %v", time.Since(start))
		}
	}
	resp, err := c.Stream(context.Background(), userRequest("short"), func(StreamEvent) error { return nil })
	if err != nil || resp.Message.Text() != "." {
		t.Fatalf("healthy request after cap kills: %+v %v", resp, err)
	}
}

func TestStreamRouterShimHeldBackAndRejected(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		sseFrames(w,
			chunk(map[string]any{"content": "Connect timeout, "}, nil, nil),
			chunk(map[string]any{"content": "please try again later."}, "stop", nil),
			"data: [DONE]\n\n")
	})
	log := &eventLog{}
	_, err := newTestClient(t, ws.route("custom")).Stream(context.Background(), userRequest("hi"), log.fn)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrEmptyResponse {
		t.Fatalf("err %v", err)
	}
	if texts := log.texts(EventTextDelta); len(texts) != 0 {
		t.Fatalf("shim text leaked: %v", texts)
	}
}

func TestStreamAdoptsFinalJSONResponse(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-final", "model": "m", "choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "<think>why</think>Final text", "reasoning_content": "why"}}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2}})
	})
	c := newTestClient(t, ws.route("custom"))
	log := &eventLog{}
	resp, err := c.Stream(context.Background(), userRequest("hi"), log.fn)
	if err != nil || resp.RequestID != "chatcmpl-final" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if got := strings.Join(log.texts(EventTextDelta), ""); got != "Final text" {
		t.Fatalf("replayed text %q", got)
	}
	if got := strings.Join(log.texts(EventReasoningDelta), ""); got != "why" {
		t.Fatalf("replayed reasoning %q", got)
	}
	if _, err := c.Stream(context.Background(), userRequest("hi"), log.fn); err != nil {
		t.Fatal(err)
	}
	if _, has := ws.request(1).Body["stream"]; has {
		t.Fatal("client did not latch non streaming after a final response")
	}
}

func TestStreamPromptProgressStatus(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		sseFrames(w,
			dataFrame(map[string]any{"id": "p", "prompt_progress": map[string]any{"total": 100, "processed": 50}, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}}}),
			chunk(map[string]any{"content": "done"}, "stop", nil),
			"data: [DONE]\n\n")
	})
	log := &eventLog{}
	resp, err := newTestClient(t, ws.route("llamacpp")).Stream(context.Background(), userRequest("hi"), log.fn)
	if err != nil || resp.Message.Text() != "done" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if ws.request(0).Body["return_progress"] != true {
		t.Fatalf("return_progress missing: %v", ws.request(0).Body)
	}
	if statuses := log.texts(EventWaitNotice); len(statuses) != 1 || statuses[0] != "⚙ processing prompt — 50%" {
		t.Fatalf("status %v", statuses)
	}
}

func TestStreamContextCancelReturnsCanceled(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := newTestClient(t, ws.route("custom")).Stream(ctx, userRequest("hi"), func(StreamEvent) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

// ---- blocking behaviour ----

func TestCompleteNormalizesRefusalUsageAndToolIDs(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "req-1")
		w.WriteHeader(200)
		switch n {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "m", "choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "", "refusal": "I cannot help with that."}}},
				"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "prompt_cache_hit_tokens": 5}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-2", "model": "m", "choices": []any{map[string]any{"index": 0, "finish_reason": "STOP",
				"message": map[string]any{"role": "assistant", "content": nil, "reasoning_content": "thinking",
					"tool_calls": []any{map[string]any{"type": "function", "function": map[string]any{"name": "terminal", "arguments": map[string]any{"command": "ls"}},
						"extra_content": map[string]any{"google": map[string]any{"thought_signature": "sig-1"}}}}}}}})
		}
	})
	c := newTestClient(t, ws.route("custom"))
	req := userRequest("hi")
	req.Stream = false
	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "content_filter" || resp.Message.Text() != "I cannot help with that." {
		t.Fatalf("refusal not promoted: %+v", resp)
	}
	if resp.Usage.CacheReadTokens != 5 || resp.Usage.TotalTokens != 10 || resp.RequestID != "req-1" || resp.Headers.Get("x-request-id") != "req-1" {
		t.Fatalf("usage %+v id %q", resp.Usage, resp.RequestID)
	}
	if _, has := ws.request(0).Body["stream"]; has {
		t.Fatal("blocking request carried stream")
	}
	resp, err = c.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.FinishReason != "stop" || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("resp %+v", resp)
	}
	tc := resp.Message.ToolCalls[0]
	if tc.ID != tools.DeterministicCallID("terminal", `{"command":"ls"}`, 0) || tc.Function.Arguments != `{"command":"ls"}` {
		t.Fatalf("tool call %+v", tc)
	}
	if resp.Message.Reasoning != "thinking" || resp.Message.ReasoningContent != "thinking" {
		t.Fatalf("reasoning %+v", resp.Message)
	}
	var details []map[string]any
	if err := json.Unmarshal(resp.Message.ReasoningDetails, &details); err != nil || len(details) != 1 || details[0]["type"] != nativeCarrierType {
		t.Fatalf("carrier record %s", resp.Message.ReasoningDetails)
	}
	carriers := nativeToolCallCarriers(details[0])
	if carriers[tc.ID] == nil || thoughtSignature(carriers[tc.ID]["extra_content"]) != "sig-1" {
		t.Fatalf("tool carriers %v", carriers)
	}
}

func TestCompleteReversesXAIToolSearchAlias(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": "m", "choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls",
			"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": xaiToolSearchAlias, "arguments": "{}"}}}}}}})
	})
	route := ws.route("xai")
	req := userRequest("hi")
	req.Stream = false
	req.Tools = []map[string]any{{"type": "function", "function": map[string]any{"name": "tool_search", "parameters": map[string]any{"type": "object"}}}}
	resp, err := newTestClient(t, route).Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := ws.request(0).Body["tools"].([]any)
	if len(tools) != 1 || chatToolName(tools[0].(map[string]any)) != xaiToolSearchAlias {
		t.Fatalf("wire tools %v", tools)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Message.ToolCalls[0].Function.Name != "tool_search" {
		t.Fatalf("alias not reversed: %+v", resp.Message.ToolCalls)
	}
}

func TestCompleteRouterShimIsEmptyResponse(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": routerTimeoutShim}}}})
	})
	req := userRequest("hi")
	req.Stream = false
	_, err := newTestClient(t, ws.route("custom")).Complete(context.Background(), req)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrEmptyResponse || !apiErr.Retryable() {
		t.Fatalf("err %v", err)
	}
}

func TestHTTPErrorCarriesRetryAfterAndHeaders(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("x-ratelimit-remaining-requests", "0")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error","code":"rate_limited"}}`)
	})
	req := userRequest("hi")
	req.Stream = false
	_, err := newTestClient(t, ws.route("custom")).Complete(context.Background(), req)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrRateLimit || apiErr.StatusCode != 429 || apiErr.RetryAfter < 6*time.Second || apiErr.RetryAfter > 7*time.Second {
		t.Fatalf("err %v", err)
	}
	var hc HeaderCarrier
	if !errors.As(err, &hc) || hc.ResponseHeaders().Get("x-ratelimit-remaining-requests") != "0" {
		t.Fatalf("headers missing: %v", err)
	}
	_, err = newTestClient(t, ws.route("custom")).Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 || !errors.As(err, &hc) {
		t.Fatalf("stream err %v", err)
	}
}

func TestCompleteRequestTimeoutIsReadPhase(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	req := userRequest("hi")
	req.Stream = false
	req.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := newTestClient(t, ws.route("custom")).Complete(context.Background(), req)
	if TimeoutPhase(err) != TimeoutPhaseRead || time.Since(start) > 3*time.Second {
		t.Fatalf("err %v phase %q after %v", err, TimeoutPhase(err), time.Since(start))
	}
}

func TestCountTokensEstimatesWireBody(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	c := newTestClient(t, Route{Provider: "custom", Model: "m", BaseURL: "http://127.0.0.1:9/v1", APIKey: "k", Mode: ModeChatCompletions})
	n, err := c.CountTokens(context.Background(), userRequest(strings.Repeat("word ", 400)))
	if err != nil || n < 400 {
		t.Fatalf("tokens %d err %v", n, err)
	}
}

// ---- azure shaping ----

func TestAzureEndpointURL(t *testing.T) {
	got, err := ccEndpointURL(Route{BaseURL: "https://res.openai.azure.com/openai/v1?api-version=2024-10-21", Deployment: "gpt-4o"}, "/chat/completions")
	if err != nil || got != "https://res.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21" {
		t.Fatalf("got %q err %v", got, err)
	}
	got, err = ccEndpointURL(Route{BaseURL: "https://res.openai.azure.com/openai/deployments/dep", Deployment: "other", APIVersion: "2025-04-01-preview"}, "/chat/completions")
	if err != nil || got != "https://res.openai.azure.com/openai/deployments/dep/chat/completions?api-version=2025-04-01-preview" {
		t.Fatalf("got %q err %v", got, err)
	}
	got, err = ccEndpointURL(Route{BaseURL: "https://gateway.example.com/v1/?key=abc&x=1", Deployment: "ignored"}, "/chat/completions")
	if err != nil || got != "https://gateway.example.com/v1/chat/completions?key=abc&x=1" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := ccEndpointURL(Route{BaseURL: "not a url"}, "/chat/completions"); err == nil {
		t.Fatal("relative base accepted")
	}
}

func TestAzureAuthHeaders(t *testing.T) {
	h, strip, err := ccAuthHeaders(context.Background(), Route{BaseURL: "https://res.openai.azure.com/openai/v1", APIKey: "key-1"})
	if err != nil || h["api-key"] != "key-1" || h["Authorization"] != "Bearer key-1" || len(strip) != 0 {
		t.Fatalf("api key headers %v strip %v err %v", h, strip, err)
	}
	calls := 0
	h, strip, err = ccAuthHeaders(context.Background(), Route{BaseURL: "https://res.services.ai.azure.com/openai/v1", APIKey: "stale",
		TokenSource: func(context.Context) (string, error) { calls++; return fmt.Sprintf("tok-%d", calls), nil }})
	if err != nil || h["Authorization"] != "Bearer tok-1" || h["api-key"] != "" || fmt.Sprint(strip) != "[Authorization Api-Key X-Api-Key]" {
		t.Fatalf("entra headers %v strip %v err %v", h, strip, err)
	}
	if h, strip, err = ccAuthHeaders(context.Background(), Route{BaseURL: "https://x", TokenSource: func(context.Context) (string, error) { return "", nil }}); err != nil || len(h) != 0 || len(strip) != 3 {
		t.Fatalf("empty mint must send without auth %v %v %v", h, strip, err)
	}
	if _, _, err = ccAuthHeaders(context.Background(), Route{BaseURL: "https://x", TokenSource: func(context.Context) (string, error) { return "", errors.New("chain exhausted") }}); err == nil {
		t.Fatal("mint error swallowed")
	}
	h, _, err = ccAuthHeaders(context.Background(), Route{BaseURL: "https://api.openai.com/v1", APIKey: "k"})
	if err != nil || h["Authorization"] != "Bearer k" || h["api-key"] != "" {
		t.Fatalf("plain headers %v", h)
	}
}

func TestAzureTokenSourceRefreshesPerRequestAndStripsAPIKey(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		sseFrames(w, chunk(map[string]any{"content": "ok"}, "stop", nil), "data: [DONE]\n\n")
	})
	calls := 0
	route := ws.route("custom")
	route.Headers = map[string]string{"api-key": "placeholder", "X-Custom": "1"}
	route.TokenSource = func(context.Context) (string, error) { calls++; return fmt.Sprintf("tok-%d", calls), nil }
	c := newTestClient(t, route)
	for i := 0; i < 2; i++ {
		if _, err := c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		h := ws.request(i).Headers
		if h.Get("Authorization") != fmt.Sprintf("Bearer tok-%d", i+1) || h.Get("api-key") != "" || h.Get("X-Custom") != "1" {
			t.Fatalf("request %d headers %v", i, h)
		}
	}
}
