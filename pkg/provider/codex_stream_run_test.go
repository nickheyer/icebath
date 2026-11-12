package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Agent double carrying the optional Codex stream hooks on top of the chat agent fake
type codexStreamAgent struct {
	*fakeChatAgent
	cmu          sync.Mutex
	deltas       []string
	reasoning    []string
	commentary   []string
	parts        []string
	interim      bool
	backend      bool
	claim        func() int
	isCurrent    func(token int) bool
	primary      any
	ensured      []string
	firstChunk   *float64
	onDelta      func(text string)
	tracksChunks bool
}

func newCodexStreamAgent() *codexStreamAgent {
	return &codexStreamAgent{fakeChatAgent: &fakeChatAgent{provider: "openai-codex", model: "gpt-5-codex",
		baseURL: "https://chatgpt.com/backend-api/codex"}, tracksChunks: true}
}

func (a *codexStreamAgent) FireStreamDelta(text string) {
	a.cmu.Lock()
	a.deltas = append(a.deltas, text)
	hook := a.onDelta
	a.cmu.Unlock()
	if hook != nil {
		hook(text)
	}
}
func (a *codexStreamAgent) FireReasoningDelta(text string) {
	a.cmu.Lock()
	a.reasoning = append(a.reasoning, text)
	a.cmu.Unlock()
}
func (a *codexStreamAgent) FireStreamedCodexCommentary(text string) {
	a.cmu.Lock()
	a.commentary = append(a.commentary, text)
	a.cmu.Unlock()
}
func (a *codexStreamAgent) HasInterimAssistantCallback() bool { return a.interim }
func (a *codexStreamAgent) IsCodexBackend() bool              { return a.backend }
func (a *codexStreamAgent) ClaimStreamWriter() int {
	if a.claim == nil {
		return 0
	}
	return a.claim()
}
func (a *codexStreamAgent) StreamWriterIsCurrent(token int) bool {
	if a.isCurrent == nil {
		return true
	}
	return a.isCurrent(token)
}
func (a *codexStreamAgent) EnsurePrimaryOpenAIClient(_ context.Context, reason string) (any, error) {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	a.ensured = append(a.ensured, reason)
	return a.primary, nil
}
func (a *codexStreamAgent) SetLastAPIFirstChunkAt(at float64) {
	a.cmu.Lock()
	a.firstChunk = &at
	a.cmu.Unlock()
}
func (a *codexStreamAgent) LastAPIFirstChunkAt() *float64 {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	if !a.tracksChunks {
		return nil
	}
	return a.firstChunk
}
func (a *codexStreamAgent) ResetCodexStreamedTextParts() {
	a.cmu.Lock()
	a.parts = []string{}
	a.cmu.Unlock()
}
func (a *codexStreamAgent) AppendCodexStreamedTextPart(text string) {
	a.cmu.Lock()
	a.parts = append(a.parts, text)
	a.cmu.Unlock()
}
func (a *codexStreamAgent) CodexStreamedTextParts() []string {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	return append([]string(nil), a.parts...)
}
func (a *codexStreamAgent) RetryStatusMessages() []string {
	a.fakeChatAgent.mu.Lock()
	defer a.fakeChatAgent.mu.Unlock()
	return append([]string(nil), a.statuses...)
}
func (a *codexStreamAgent) setInterrupted(on bool) {
	a.fakeChatAgent.mu.Lock()
	a.interrupted = on
	a.fakeChatAgent.mu.Unlock()
}
func (a *codexStreamAgent) snapshot() (deltas, reasoning, commentary []string) {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	return append([]string(nil), a.deltas...), append([]string(nil), a.reasoning...), append([]string(nil), a.commentary...)
}

// Raw Responses SSE stream double yielding its events then the tail
type codexFakeStream struct {
	mu         sync.Mutex
	events     []map[string]any
	pos        int
	before     func(i int)
	tail       func() (map[string]any, error)
	closeErr   error
	closeCalls int
	closers    []int64
	onClose    func()
}

func newCodexFakeStream(events ...map[string]any) *codexFakeStream {
	return &codexFakeStream{events: events}
}

func (s *codexFakeStream) Next(context.Context) (map[string]any, error) {
	s.mu.Lock()
	if s.pos < len(s.events) {
		i := s.pos
		s.pos++
		before := s.before
		s.mu.Unlock()
		if before != nil {
			before(i)
		}
		return s.events[i], nil
	}
	tail := s.tail
	s.mu.Unlock()
	if tail != nil {
		return tail()
	}
	return nil, io.EOF
}

func (s *codexFakeStream) Close() error {
	s.mu.Lock()
	s.closeCalls++
	s.closers = append(s.closers, codexGoroutineID())
	hook := s.onClose
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.closeErr
}

func (s *codexFakeStream) closeState() (int, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCalls, append([]int64(nil), s.closers...)
}

// Raw stream double exposing an attempt connection the drain watchdog may shut down
type codexSocketStream struct {
	*codexFakeStream
	conn net.Conn
}

func (s *codexSocketStream) auxAttemptConn() net.Conn { return s.conn }

// SDK client double recording every Responses create call
type codexFakeClient struct {
	mu      sync.Mutex
	baseURL string
	calls   []map[string]any
	create  func(n int, kwargs map[string]any) (any, error)
}

func (c *codexFakeClient) BaseURL() string { return c.baseURL }
func (c *codexFakeClient) CreateResponse(_ context.Context, kwargs map[string]any) (any, error) {
	c.mu.Lock()
	c.calls = append(c.calls, kwargs)
	n := len(c.calls)
	c.mu.Unlock()
	return c.create(n, kwargs)
}
func (c *codexFakeClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func codexStreamClient(streams ...func() any) *codexFakeClient {
	return &codexFakeClient{create: func(n int, _ map[string]any) (any, error) {
		v := streams[min(n, len(streams))-1]()
		if err, ok := v.(error); ok {
			return nil, err
		}
		return v, nil
	}}
}

func codexGoroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(strings.TrimPrefix(string(buf[:n]), "goroutine "))
	id, _ := strconv.ParseInt(fields[0], 10, 64)
	return id
}

func codexStreamHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func codexSetDrainTimeout(t *testing.T, seconds float64) {
	t.Helper()
	prev := codexStreamDrainTimeoutFn
	codexStreamDrainTimeoutFn = func(context.Context) float64 { return seconds }
	t.Cleanup(func() { codexStreamDrainTimeoutFn = prev })
}

func codexRequestKwargs() map[string]any {
	return map[string]any{"model": "gpt-5-codex", "instructions": "You are Hermes.",
		"input": []any{map[string]any{"role": "user", "content": "Ping"}}, "tools": nil, "store": false}
}

func codexStreamEv(eventType string, kv ...any) map[string]any {
	m := map[string]any{"type": eventType}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func codexStreamCompleted(kv ...any) map[string]any {
	resp := map[string]any{"status": "completed"}
	for i := 0; i+1 < len(kv); i += 2 {
		resp[kv[i].(string)] = kv[i+1]
	}
	return codexStreamEv("response.completed", "response", resp)
}

func codexTextMessage(text string) map[string]any {
	return map[string]any{"type": "message", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text}}}
}

func codexMustResult(t *testing.T, out any, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("run_codex_stream: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("response %T %v", out, out)
	}
	return m
}

func codexConsume(t *testing.T, events []map[string]any, o codexConsumeOptions) map[string]any {
	t.Helper()
	list := make([]any, len(events))
	for i, e := range events {
		list[i] = e
	}
	out, err := consumeCodexEventStream(context.Background(), codexSliceEvents(list), "gpt-5-codex", o)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunCodexStreamStripsRelayAddedRetentionAtConsumerWire(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	agent.backend = true
	client := codexStreamClient(func() any { return newCodexFakeStream(codexStreamCompleted()) })
	agent.primary = client
	original := codexRequestKwargs()
	meta := map[string]any{"api_mode": "codex_responses"}
	body := relayRequestBody(original, meta)
	content, baseline := map[string]any{}, map[string]any{}
	for k, v := range body {
		content[k], baseline[k] = v, v
	}
	content["prompt_cache_retention"] = "24h"
	relayed := relayProviderRequest(original, &tools.RelayLLMRequest{Content: content, Headers: map[string]any{}}, body, baseline, meta)
	if relayed["prompt_cache_retention"] != "24h" {
		t.Fatalf("relayed %v", relayed)
	}
	logs := captureLogs(t)
	if _, err := RunCodexStream(context.Background(), agent, relayed, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, has := client.calls[0]["prompt_cache_retention"]; has {
		t.Fatalf("captured %v", client.calls[0])
	}
	if !codexLogged(logs, "Dropped unsupported prompt_cache_retention at consumer Codex wire boundary") {
		t.Fatalf("logs %v", logs.lines)
	}
}

func codexLogged(logs *logCapture, fragment string) bool {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, line := range logs.lines {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func TestRunCodexStreamStripsNestedRequestOverrideRetention(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	agent.backend = true
	client := codexStreamClient(func() any { return newCodexFakeStream(codexStreamCompleted()) })
	agent.primary = client
	transport := &responsesAPITransport{codex: newCodexTransport()}
	request, err := transport.BuildKwargs("gpt-5.6-sol", []map[string]any{
		{"role": "system", "content": "You are Hermes."}, {"role": "user", "content": "Ping"},
	}, []map[string]any{}, map[string]any{"is_codex_backend": true, "base_url": "https://chatgpt.com/backend-api/codex",
		"request_overrides": map[string]any{"extra_body": map[string]any{"prompt_cache_retention": "24h"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"prompt_cache_retention": "24h"}
	if !reflect.DeepEqual(request["extra_body"], want) {
		t.Fatalf("extra_body %v", request["extra_body"])
	}
	captureLogs(t)
	if _, err := RunCodexStream(context.Background(), agent, request, nil, nil); err != nil {
		t.Fatal(err)
	}
	captured := client.calls[0]
	extra, _ := captured["extra_body"].(map[string]any)
	if _, has := captured["prompt_cache_retention"]; has {
		t.Fatalf("captured %v", captured)
	}
	if _, has := extra["prompt_cache_retention"]; has {
		t.Fatalf("extra_body %v", extra)
	}
	if _, has := extra["input"]; !has {
		t.Fatalf("extra_body lacks input %v", extra)
	}
	if !reflect.DeepEqual(request["extra_body"], want) {
		t.Fatalf("caller extra_body mutated %v", request["extra_body"])
	}
}

func TestConsumerCodexWireGuardStripsNestedExtraBodyRetention(t *testing.T) {
	agent := newCodexStreamAgent()
	agent.backend, agent.model = true, "gpt-5.6-sol"
	extraBody := map[string]any{"prompt_cache_retention": "24h", "unrelated": "keep"}
	request := map[string]any{"model": "gpt-5.6-sol", "prompt_cache_key": "cache-key-sentinel", "extra_body": extraBody}
	captureLogs(t)
	sanitized := sanitizeConsumerCodexRequest(agent, request)
	extra, _ := sanitized["extra_body"].(map[string]any)
	if _, has := extra["prompt_cache_retention"]; has {
		t.Fatalf("sanitized %v", sanitized)
	}
	if extra["unrelated"] != "keep" || extraBody["prompt_cache_retention"] != "24h" || sanitized["prompt_cache_key"] != "cache-key-sentinel" {
		t.Fatalf("sanitized %v caller %v", sanitized, extraBody)
	}
}

func TestConsumerCodexWireGuardPreservesNestedRetentionOnCompatibleEndpoint(t *testing.T) {
	agent := newCodexStreamAgent()
	request := map[string]any{"model": "openai.gpt-5.5", "extra_body": map[string]any{"prompt_cache_retention": "24h"}}
	sanitized := sanitizeConsumerCodexRequest(agent, request)
	if sanitized["extra_body"].(map[string]any)["prompt_cache_retention"] != "24h" {
		t.Fatalf("sanitized %v", sanitized)
	}
}

func TestRunCodexStreamReturnsCollectedItemsWhenStreamEndsWithoutTerminal(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	item := codexTextMessage("no terminal frame")
	client := codexStreamClient(func() any {
		return newCodexFakeStream(codexStreamEv("response.created"), codexStreamEv("response.output_item.done", "item", item))
	})
	agent.primary = client
	out, err := RunCodexStream(context.Background(), agent, codexRequestKwargs(), nil, nil)
	resp := codexMustResult(t, out, err)
	if client.callCount() != 1 || client.calls[0]["stream"] != true {
		t.Fatalf("calls %v", client.calls)
	}
	if resp["status"] != "completed" || !reflect.DeepEqual(resp["output"], []any{item}) {
		t.Fatalf("response %v", resp)
	}
}

func TestConsumeCodexStreamRoutesCommentaryPhaseDeltasToReasoning(t *testing.T) {
	commentaryItem := map[string]any{"type": "message", "phase": "commentary", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "I’ll call the tool now."}}}
	functionItem := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "terminal", "arguments": "{}"}
	var streamed, reasoning []string
	resp := codexConsume(t, []map[string]any{
		codexStreamEv("response.created"),
		codexStreamEv("response.output_item.added", "item", map[string]any{"type": "message", "phase": "commentary"}),
		codexStreamEv("response.output_text.delta", "delta", "I’ll call the tool now."),
		codexStreamEv("response.output_item.done", "item", commentaryItem),
		codexStreamEv("response.output_item.added", "item", map[string]any{"type": "function_call"}),
		codexStreamEv("response.output_item.done", "item", functionItem),
		codexStreamCompleted(),
	}, codexConsumeOptions{onTextDelta: func(s string) { streamed = append(streamed, s) },
		onReasoningDelta: func(s string) { reasoning = append(reasoning, s) }})
	if len(streamed) != 0 || !reflect.DeepEqual(reasoning, []string{"I’ll call the tool now."}) {
		t.Fatalf("streamed %v reasoning %v", streamed, reasoning)
	}
	if !reflect.DeepEqual(resp["output"], []any{commentaryItem, functionItem}) || resp["output_text"] != "" {
		t.Fatalf("response %v", resp)
	}
}

func TestConsumeCodexStreamCollectsRefusalDeltasAsText(t *testing.T) {
	resp := codexConsume(t, []map[string]any{
		codexStreamEv("response.created"),
		codexStreamEv("response.refusal.delta", "delta", "I can't"),
		codexStreamEv("response.refusal.delta", "delta", " help with that."),
		codexStreamCompleted(),
	}, codexConsumeOptions{})
	output, _ := resp["output"].([]any)
	if resp["output_text"] != "I can't help with that." || len(output) == 0 || output[0].(map[string]any)["type"] != "message" {
		t.Fatalf("response %v", resp)
	}
}

func TestConsumeCodexStreamSeparatesCommentaryFromAnalysis(t *testing.T) {
	commentaryItem := map[string]any{"type": "message", "phase": "commentary", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "I'll inspect the repo first."}}}
	var streamed, reasoning, commentary []string
	resp := codexConsume(t, []map[string]any{
		codexStreamEv("response.output_item.added", "item", map[string]any{"type": "message", "phase": "commentary"}),
		codexStreamEv("response.output_text.delta", "delta", "I'll inspect "),
		codexStreamEv("response.output_text.delta", "delta", "the repo first."),
		codexStreamEv("response.output_item.done", "item", commentaryItem),
		codexStreamEv("response.reasoning_text.delta", "delta", "Need inspect files privately."),
		codexStreamCompleted(),
	}, codexConsumeOptions{onTextDelta: func(s string) { streamed = append(streamed, s) },
		onReasoningDelta:    func(s string) { reasoning = append(reasoning, s) },
		onCommentaryMessage: func(s string) { commentary = append(commentary, s) }})
	if !reflect.DeepEqual(commentary, []string{"I'll inspect the repo first."}) || !reflect.DeepEqual(reasoning, []string{"Need inspect files privately."}) || len(streamed) != 0 {
		t.Fatalf("commentary %v reasoning %v streamed %v", commentary, reasoning, streamed)
	}
	if !reflect.DeepEqual(resp["output"], []any{commentaryItem}) {
		t.Fatalf("output %v", resp["output"])
	}
}

func TestConsumeCodexStreamSeparatesReasoningSummaryParts(t *testing.T) {
	var reasoning []string
	codexConsume(t, []map[string]any{
		codexStreamEv("response.created"),
		codexStreamEv("response.reasoning_summary_text.delta", "summary_index", float64(0), "delta", "**Investigating culprit PRs**"),
		codexStreamEv("response.reasoning_summary_text.delta", "summary_index", float64(1), "delta", "**Inspecting message schema**"),
		codexStreamEv("response.reasoning_summary_text.delta", "summary_index", float64(1), "delta", " and tool_calls content"),
		codexStreamCompleted(),
	}, codexConsumeOptions{onReasoningDelta: func(s string) { reasoning = append(reasoning, s) }})
	joined := strings.Join(reasoning, "")
	if strings.Contains(joined, "****") || joined != "**Investigating culprit PRs**\n\n**Inspecting message schema** and tool_calls content" {
		t.Fatalf("joined %q", joined)
	}
}

func TestConsumeCodexStreamLeavesUnindexedReasoningUntouched(t *testing.T) {
	var reasoning []string
	codexConsume(t, []map[string]any{
		codexStreamEv("response.created"),
		codexStreamEv("response.reasoning_text.delta", "delta", "Need to "),
		codexStreamEv("response.reasoning_text.delta", "delta", "inspect files."),
		codexStreamCompleted(),
	}, codexConsumeOptions{onReasoningDelta: func(s string) { reasoning = append(reasoning, s) }})
	if joined := strings.Join(reasoning, ""); joined != "Need to inspect files." {
		t.Fatalf("joined %q", joined)
	}
}

func TestRunCodexStreamReturnsTerminalResponseWhenPostTerminalDrainFails(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	usage := map[string]any{"input_tokens": float64(10), "output_tokens": float64(6), "total_tokens": float64(16)}
	client := codexStreamClient(func() any {
		s := newCodexFakeStream(codexStreamEv("response.output_item.done", "item", codexTextMessage("All done.")),
			codexStreamCompleted("usage", usage, "id", "resp_post_terminal_1"))
		s.tail = func() (map[string]any, error) {
			return nil, &AuxTransportError{Class: "RemoteProtocolError", Message: "connection dropped during drain"}
		}
		return s
	})
	agent.primary = client
	captureLogs(t)
	out, err := RunCodexStream(context.Background(), agent, codexRequestKwargs(), nil, nil)
	resp := codexMustResult(t, out, err)
	if client.callCount() != 1 || resp["status"] != "completed" || !reflect.DeepEqual(resp["usage"], usage) || resp["id"] != "resp_post_terminal_1" {
		t.Fatalf("calls %d response %v", client.callCount(), resp)
	}
}

func TestRunCodexStreamSkipsPostTerminalDrainWithoutSocket(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	usage := map[string]any{"input_tokens": float64(10), "output_tokens": float64(6), "total_tokens": float64(16)}
	var postReads int
	var stream *codexFakeStream
	client := codexStreamClient(func() any {
		stream = newCodexFakeStream(codexStreamEv("response.output_item.done", "item", codexTextMessage("All done.")),
			codexStreamCompleted("usage", usage, "id", "resp_held_open"))
		stream.tail = func() (map[string]any, error) {
			postReads++
			return nil, io.EOF
		}
		return stream
	})
	agent.primary = client
	codexSetDrainTimeout(t, 0.01)
	out, err := RunCodexStream(context.Background(), agent, codexRequestKwargs(), nil, nil)
	resp := codexMustResult(t, out, err)
	closes, _ := stream.closeState()
	if client.callCount() != 1 || resp["status"] != "completed" || !reflect.DeepEqual(resp["usage"], usage) || resp["id"] != "resp_held_open" || postReads != 0 || closes == 0 {
		t.Fatalf("calls %d response %v post reads %d closes %d", client.callCount(), resp, postReads, closes)
	}
}

// Managed stream double whose close releases the raw stream and then raises
type codexClosingManagedStream struct {
	raw *codexFakeStream
}

func (s *codexClosingManagedStream) Next(ctx context.Context) (any, error) {
	event, err := s.raw.Next(ctx)
	if err != nil {
		return nil, err
	}
	return event, nil
}
func (s *codexClosingManagedStream) Close() error {
	_ = s.raw.Close()
	return &tools.RelayError{Class: "RuntimeError", Message: "Cannot close a running event loop"}
}
func (s *codexClosingManagedStream) finalResponse() any { return nil }

func TestRunCodexStreamOwnerCloseDoesNotRetryRawWhenManagedCloseRaises(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	usage := map[string]any{"input_tokens": float64(10), "output_tokens": float64(6), "total_tokens": float64(16)}
	var raw *codexFakeStream
	client := codexStreamClient(func() any {
		raw = newCodexFakeStream(codexStreamEv("response.output_item.done", "item", codexTextMessage("All done.")),
			codexStreamCompleted("usage", usage, "id", "resp_managed"))
		return raw
	})
	agent.primary = client
	prev := codexRelayStreamFn
	codexRelayStreamFn = func(ctx context.Context, request map[string]any, factory func(ctx context.Context, request map[string]any) (any, error), o ManagedLlmStreamOptions) (codexManagedStream, error) {
		created, err := factory(ctx, request)
		if err != nil {
			return nil, err
		}
		if err := o.OnStreamCreated(ctx, created); err != nil {
			return nil, err
		}
		return &codexClosingManagedStream{raw: created.(*codexFakeStream)}, nil
	}
	t.Cleanup(func() { codexRelayStreamFn = prev })
	codexSetDrainTimeout(t, 0.01)
	owner := codexGoroutineID()
	out, err := RunCodexStream(context.Background(), agent, codexRequestKwargs(), nil, nil)
	resp := codexMustResult(t, out, err)
	_, closers := raw.closeState()
	if resp["id"] != "resp_managed" || !reflect.DeepEqual(closers, []int64{owner}) {
		t.Fatalf("response %v closers %v owner %d", resp, closers, owner)
	}
}

type codexTimelineEntry struct {
	kind string
	gid  int64
}

func TestRunCodexStreamPostTerminalTimeoutKeepsCloseOnReaderThread(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	owner := codexGoroutineID()
	woken, blocked := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var timeline []codexTimelineEntry
	record := func(kind string) {
		mu.Lock()
		timeline = append(timeline, codexTimelineEntry{kind, codexGoroutineID()})
		mu.Unlock()
	}
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	prevShutdown := codexShutdownSocketFn
	var wakeOnce sync.Once
	codexShutdownSocketFn = func(conn net.Conn) {
		if conn != c1 {
			t.Errorf("shutdown of %v", conn)
		}
		record("shutdown")
		wakeOnce.Do(func() { close(woken) })
	}
	t.Cleanup(func() { codexShutdownSocketFn = prevShutdown })
	usage := map[string]any{"input_tokens": float64(10), "output_tokens": float64(6), "total_tokens": float64(16)}
	client := codexStreamClient(func() any {
		s := newCodexFakeStream(codexStreamEv("response.output_item.done", "item", codexTextMessage("All done.")),
			codexStreamCompleted("usage", usage, "id", "resp_owner_close"))
		s.tail = func() (map[string]any, error) {
			close(blocked)
			select {
			case <-woken:
			case <-time.After(2 * time.Second):
				return nil, errors.New("watchdog never shut the socket down")
			}
			return nil, io.EOF
		}
		s.onClose = func() { record("close") }
		return &codexSocketStream{codexFakeStream: s, conn: c1}
	})
	agent.primary = client
	codexSetDrainTimeout(t, 0.01)
	out, err := RunCodexStream(context.Background(), agent, codexRequestKwargs(), nil, nil)
	resp := codexMustResult(t, out, err)
	select {
	case <-blocked:
	default:
		t.Fatal("drain never blocked on the post terminal read")
	}
	mu.Lock()
	defer mu.Unlock()
	firstShutdown, firstClose := -1, -1
	for i, e := range timeline {
		switch e.kind {
		case "shutdown":
			if e.gid == owner {
				t.Fatalf("shutdown ran on the owner goroutine %v", timeline)
			}
			if firstShutdown < 0 {
				firstShutdown = i
			}
		case "close":
			if e.gid != owner {
				t.Fatalf("close ran off the owner goroutine %v", timeline)
			}
			if firstClose < 0 {
				firstClose = i
			}
		}
	}
	if resp["id"] != "resp_owner_close" || firstShutdown < 0 || firstClose < 0 || firstShutdown > firstClose {
		t.Fatalf("response %v timeline %v", resp, timeline)
	}
}

func TestRunCodexStreamPostTerminalCleanDrainNeverShutdowns(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	var mu sync.Mutex
	var shutdowns int
	prevShutdown := codexShutdownSocketFn
	codexShutdownSocketFn = func(net.Conn) {
		mu.Lock()
		shutdowns++
		mu.Unlock()
	}
	t.Cleanup(func() { codexShutdownSocketFn = prevShutdown })
	usage := map[string]any{"input_tokens": float64(8), "output_tokens": float64(4), "total_tokens": float64(12)}
	client := codexStreamClient(func() any {
		s := newCodexFakeStream(codexStreamEv("response.output_item.done", "item", codexTextMessage("Done.")),
			codexStreamCompleted("usage", usage, "id", "resp_clean_drain"))
		return &codexSocketStream{codexFakeStream: s, conn: c1}
	})
	agent.primary = client
	codexSetDrainTimeout(t, 1.0)
	out, err := RunCodexStream(context.Background(), agent, codexRequestKwargs(), nil, nil)
	resp := codexMustResult(t, out, err)
	mu.Lock()
	defer mu.Unlock()
	if resp["id"] != "resp_clean_drain" || shutdowns != 0 {
		t.Fatalf("response %v shutdowns %d", resp, shutdowns)
	}
}

type codexTestToken struct{ n int }

// Lazy stream clearing the agent request token just before the delta at retireAfter
func codexRetiringStream(agent *codexStreamAgent, deltas []string, retireAfter int) *codexFakeStream {
	events := []map[string]any{codexStreamEv("response.created")}
	for _, d := range deltas {
		events = append(events, codexStreamEv("response.output_text.delta", "delta", d))
	}
	s := newCodexFakeStream(events...)
	s.before = func(i int) {
		if i-1 == retireAfter {
			agent.SetActiveCodexStreamRequestToken(nil)
		}
	}
	return s
}

func TestRunCodexStreamRetiredRequestRaisesInsteadOfPartialFinal(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	agent.SetActiveCodexStreamRequestToken(&codexTestToken{1})
	client := &codexFakeClient{create: func(_ int, kwargs map[string]any) (any, error) {
		if kwargs["stream"] != true {
			t.Errorf("stream %v", kwargs["stream"])
		}
		return codexRetiringStream(agent, []string{"1. Create ", "(6/6)", " [END-BILLING"}, 2), nil
	}}
	agent.primary = client
	_, err := RunCodexStream(context.Background(), agent, codexRequestKwargs(), nil, nil)
	var timeout *TimeoutPhaseError
	if !errors.As(err, &timeout) || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("err %T %v", err, err)
	}
}

func TestRunCodexStreamRetiredRequestStopsFiringCallbacks(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	agent.SetActiveCodexStreamRequestToken(&codexTestToken{1})
	agent.primary = &codexFakeClient{create: func(int, map[string]any) (any, error) {
		return codexRetiringStream(agent, []string{"keep", "DROPPED"}, 1), nil
	}}
	_, err := RunCodexStream(context.Background(), agent, codexRequestKwargs(), nil, nil)
	var timeout *TimeoutPhaseError
	if !errors.As(err, &timeout) {
		t.Fatalf("err %T %v", err, err)
	}
	if deltas, _, _ := agent.snapshot(); !reflect.DeepEqual(deltas, []string{"keep"}) {
		t.Fatalf("deltas %v", deltas)
	}
}

// The issue 103673 shape, APIConnectionError over ReadError over ReadError with the request attached
func codexPrestreamTransportError(url string, content []byte) error {
	req := &FailedStreamRequest{URL: url, Content: content}
	inner := &AuxTransportError{Class: "ReadError", Message: "receive failed", Request: req}
	mid := &AuxTransportError{Class: "ReadError", Message: "receive failed", Err: inner, Request: req}
	return &AuxSDKError{Class: "APIConnectionError", Message: "Connection error.", Err: mid}
}

func codexCompletedStream() *codexFakeStream {
	usage := map[string]any{"input_tokens": float64(10), "output_tokens": float64(6), "total_tokens": float64(16)}
	return newCodexFakeStream(codexStreamEv("response.output_item.done", "item", codexTextMessage("Recovered.")),
		codexStreamCompleted("usage", usage, "id", "resp_prestream_retry_1"))
}

func TestRunCodexStreamRetriesPrestreamApiconnectionerror(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	client := codexStreamClient(
		func() any {
			return codexPrestreamTransportError("https://chatgpt.com/backend-api/codex/responses", []byte(`{"model":"gpt-5-codex"}`))
		},
		func() any { return codexCompletedStream() })
	agent.primary = client
	out, err := RunCodexStream(context.Background(), agent, codexRequestKwargs(), nil, nil)
	resp := codexMustResult(t, out, err)
	if client.callCount() != 2 || resp["status"] != "completed" || resp["id"] != "resp_prestream_retry_1" {
		t.Fatalf("calls %d response %v", client.callCount(), resp)
	}
}

func TestRunCodexStreamPrestreamExhaustionBuffersOneUserLineWithHostAttemptsSize(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	body := []byte(`{"model":"gpt-5-codex","input":"` + strings.Repeat("x", 829*1024) + `"}`)
	agent.primary = codexStreamClient(func() any {
		return codexPrestreamTransportError("https://api.example.com/backend-api/codex/responses", body)
	})
	captureLogs(t)
	for outer := 0; outer < 2; outer++ {
		_, err := RunCodexStream(context.Background(), agent, codexRequestKwargs(), nil, nil)
		if !codexIsAPIConnectionError(err) {
			t.Fatalf("outer %d err %T %v", outer, err, err)
		}
	}
	lines := agent.RetryStatusMessages()
	if len(lines) != 1 {
		t.Fatalf("lines %v", lines)
	}
	size := fmt.Sprintf("request %d KB", int(math.RoundToEven(float64(len(body))/1024)))
	if !strings.Contains(lines[0], "api.example.com") || !strings.Contains(lines[0], "after 2 attempts") || !strings.Contains(lines[0], size) {
		t.Fatalf("line %q want %q", lines[0], size)
	}
}

// Duck typed agent whose writer fence reports supersession after the given number of checks
func codexSupersedingAgent(supersedeAfterChecks int) *codexStreamAgent {
	agent := newCodexStreamAgent()
	agent.interim, agent.tracksChunks = true, false
	checks := 0
	agent.claim = func() int { return 1 }
	agent.isCurrent = func(int) bool {
		checks++
		return checks <= supersedeAfterChecks
	}
	return agent
}

func codexMessageAdded(phase any, itemID string) map[string]any {
	return codexStreamEv("response.output_item.added", "item", map[string]any{"type": "message", "role": "assistant", "phase": phase, "id": itemID})
}

func codexSupersessionCompleted() map[string]any {
	return codexStreamCompleted("id", "resp_1", "usage", nil, "output", []any{}, "incomplete_details", nil, "error", nil)
}

func TestSupersededStreamAssemblesCompleteFinalAndFencesLiveDeltas(t *testing.T) {
	codexStreamHome(t)
	agent := codexSupersedingAgent(1)
	client := codexStreamClient(func() any {
		return newCodexFakeStream(codexMessageAdded(nil, "m1"),
			codexStreamEv("response.output_text.delta", "delta", "I've added the live"),
			codexStreamEv("response.output_text.delta", "delta", " tail."),
			codexStreamEv("response.reasoning_text.delta", "delta", "late reasoning"),
			codexSupersessionCompleted())
	})
	captureLogs(t)
	out, err := RunCodexStream(context.Background(), agent, map[string]any{"model": "gpt-5.6-terra"}, client, nil)
	resp := codexMustResult(t, out, err)
	deltas, reasoning, _ := agent.snapshot()
	if resp["status"] != "completed" || resp["output_text"] != "I've added the live tail." {
		t.Fatalf("response %v", resp)
	}
	if !reflect.DeepEqual(agent.CodexStreamedTextParts(), []string{"I've added the live", " tail."}) || !reflect.DeepEqual(deltas, []string{"I've added the live"}) || len(reasoning) != 0 {
		t.Fatalf("parts %v deltas %v reasoning %v", agent.CodexStreamedTextParts(), deltas, reasoning)
	}
}

func TestSupersededStreamFencesCommentaryAndFirstDeltaButKeepsFinal(t *testing.T) {
	codexStreamHome(t)
	agent := codexSupersedingAgent(0)
	client := codexStreamClient(func() any {
		return newCodexFakeStream(codexMessageAdded("commentary", "c1"),
			codexStreamEv("response.output_text.delta", "delta", "Let me check.", "item_id", "c1"),
			codexStreamEv("response.output_item.done", "item", map[string]any{"type": "message", "role": "assistant", "phase": "commentary", "id": "c1",
				"content": []any{map[string]any{"type": "output_text", "text": "Let me check."}}}),
			codexMessageAdded(nil, "m1"),
			codexStreamEv("response.output_text.delta", "delta", "Done.", "item_id", "m1"),
			codexSupersessionCompleted())
	})
	var firstDelta []bool
	captureLogs(t)
	out, err := RunCodexStream(context.Background(), agent, map[string]any{"model": "gpt-5.6-terra"}, client, func() { firstDelta = append(firstDelta, true) })
	resp := codexMustResult(t, out, err)
	deltas, _, commentary := agent.snapshot()
	if resp["status"] != "completed" || resp["output_text"] != "Done." || len(commentary) != 0 || len(deltas) != 0 || len(firstDelta) != 0 {
		t.Fatalf("response %v commentary %v deltas %v first %v", resp, commentary, deltas, firstDelta)
	}
}

func codexTimingAgent() *codexStreamAgent {
	agent := newCodexStreamAgent()
	agent.model = "timing-fixture"
	return agent
}

func codexTimingEvents() *codexFakeStream {
	return newCodexFakeStream(
		map[string]any{"type": "response.created", "response": map[string]any{"id": "fixture", "status": "in_progress"}},
		map[string]any{"type": "response.output_text.delta", "delta": "Yes."},
		map[string]any{"type": "response.completed", "response": map[string]any{"id": "fixture", "status": "completed"}})
}

func TestCodexStreamRecordsFirstLifecycleEventBeforeText(t *testing.T) {
	codexStreamHome(t)
	agent := codexTimingAgent()
	ticks := []float64{100, 200, 300}
	var tickMu sync.Mutex
	prevClock := agentNonStreamClock
	agentNonStreamClock = func() time.Time {
		tickMu.Lock()
		defer tickMu.Unlock()
		if len(ticks) == 0 {
			t.Error("clock read past the three fixture ticks")
			return time.Unix(400, 0)
		}
		next := ticks[0]
		ticks = ticks[1:]
		return time.Unix(int64(next), 0)
	}
	t.Cleanup(func() { agentNonStreamClock = prevClock })
	client := codexStreamClient(
		func() any { return &AuxTransportError{Class: "ConnectError", Message: "transient connect failure"} },
		func() any { return codexTimingEvents() })
	out, err := RunCodexStream(context.Background(), agent, map[string]any{"model": "timing-fixture", "input": "Say Yes."}, client, nil)
	resp := codexMustResult(t, out, err)
	if resp["output_text"] != "Yes." || resp["status"] != "completed" || client.callCount() != 2 {
		t.Fatalf("response %v calls %d", resp, client.callCount())
	}
	if first := agent.LastAPIFirstChunkAt(); first == nil || *first != 100.0 {
		t.Fatalf("first chunk %v", first)
	}
}

func TestCodexStreamWithoutAcceptedEventKeepsTimingUnset(t *testing.T) {
	for _, retireBeforeEvent := range []bool{false, true} {
		t.Run(fmt.Sprintf("retire_before_event=%v", retireBeforeEvent), func(t *testing.T) {
			codexStreamHome(t)
			agent := codexTimingAgent()
			agent.SetActiveCodexStreamRequestToken(&codexTestToken{1})
			client := codexStreamClient(func() any {
				if !retireBeforeEvent {
					return newCodexFakeStream()
				}
				s := newCodexFakeStream(codexStreamEv("response.created"))
				s.before = func(int) { agent.SetActiveCodexStreamRequestToken(&codexTestToken{2}) }
				return s
			})
			_, err := RunCodexStream(context.Background(), agent, map[string]any{"model": "timing-fixture"}, client, nil)
			var timeout *TimeoutPhaseError
			if !codexIsRuntimeError(err) && !errors.As(err, &timeout) {
				t.Fatalf("err %T %v", err, err)
			}
			if first := agent.LastAPIFirstChunkAt(); first != nil {
				t.Fatalf("first chunk %v", *first)
			}
		})
	}
}

func codexCloseFailingStream(delta string) *codexFakeStream {
	s := newCodexFakeStream(codexStreamEv("response.output_text.delta", "delta", delta),
		codexStreamCompleted("usage", nil, "id", "resp_1"))
	s.closeErr = errors.New("close failed")
	return s
}

func TestCodexStreamCloseFailurePoisonsSlot(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	stream := codexCloseFailingStream("partial ")
	client := codexStreamClient(func() any { return stream })
	out, err := RunCodexStream(context.Background(), agent, map[string]any{"model": "test/model"}, client, nil)
	resp := codexMustResult(t, out, err)
	closes, _ := stream.closeState()
	if resp["output_text"] != "partial " || closes != 1 || !reflect.DeepEqual(agent.reasons(&agent.aborts), []string{"codex_stream_close_failed"}) {
		t.Fatalf("response %v closes %d aborts %v", resp, closes, agent.reasons(&agent.aborts))
	}
}

func TestCodexStreamCloseFailureOnPrimaryClientDoesNotAbort(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	stream := codexCloseFailingStream("hello")
	agent.primary = codexStreamClient(func() any { return stream })
	out, err := RunCodexStream(context.Background(), agent, map[string]any{"model": "test/model"}, nil, nil)
	resp := codexMustResult(t, out, err)
	closes, _ := stream.closeState()
	if resp["output_text"] != "hello" || closes != 1 || len(agent.reasons(&agent.aborts)) != 0 {
		t.Fatalf("response %v closes %d aborts %v", resp, closes, agent.reasons(&agent.aborts))
	}
}

func TestCreateReceivesInputViaExtraBody(t *testing.T) {
	codexStreamHome(t)
	t.Setenv(sdkTransformEscapeHatchEnv, "")
	agent := newCodexStreamAgent()
	agent.model = "gpt-5.6-sol"
	client := codexStreamClient(func() any {
		return newCodexFakeStream(codexStreamCompleted("id", "r1", "output", []any{}, "usage", nil))
	})
	kwargs := map[string]any{"model": "gpt-5.6-sol", "instructions": "You are Hermes.",
		"input": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Ping"}}},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"},
		},
		"tools": []any{map[string]any{"type": "function", "name": "terminal", "parameters": map[string]any{}}},
		"store": false, "stream": true, "timeout": 1800.0}
	if _, err := RunCodexStream(context.Background(), agent, kwargs, client, nil); err != nil {
		t.Fatal(err)
	}
	created := client.calls[0]
	extra, _ := created["extra_body"].(map[string]any)
	input, _ := extra["input"].([]any)
	tools, _ := extra["tools"].([]any)
	_, hasInput := created["input"]
	_, hasTools := created["tools"]
	if hasInput || hasTools || created["stream"] != true || len(input) == 0 || len(tools) == 0 {
		t.Fatalf("created %v", created)
	}
	if input[0].(map[string]any)["role"] != "user" || tools[0].(map[string]any)["name"] != "terminal" {
		t.Fatalf("extra_body %v", extra)
	}
}

func TestTransportFailureLogsExactRequestBytesAndClassChain(t *testing.T) {
	codexStreamHome(t)
	content := []byte(`{"input":"payload"}`)
	req := &FailedStreamRequest{URL: "https://example.invalid/responses", Content: content}
	connectionError := &AuxSDKError{Class: "APIConnectionError", Message: "Connection error.",
		Err: &AuxTransportError{Class: "RemoteProtocolError", Message: "server disconnected without sending a response", Request: req}}
	client := &codexFakeClient{create: func(int, map[string]any) (any, error) { return nil, connectionError }}
	agent := newCodexStreamAgent()
	agent.model = "gpt-5.6-sol"
	logs := captureLogs(t)
	_, err := RunCodexStream(context.Background(), agent, map[string]any{"model": "gpt-5.6-sol"}, client, nil)
	if err != connectionError {
		t.Fatalf("err %T %v", err, err)
	}
	logs.mu.Lock()
	var message string
	for _, line := range logs.lines {
		if strings.HasPrefix(line, "Codex Responses request failed") {
			message = line
		}
	}
	logs.mu.Unlock()
	for _, want := range []string{fmt.Sprintf("serialized_request_body_bytes=%d", len(content)), "stream_opened=false",
		"exception_chain=APIConnectionError <- RemoteProtocolError", "attempt=2/2"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q lacks %q", message, want)
		}
	}
	if strings.Contains(message, "payload") || strings.Contains(message, string(content)) || strings.Contains(message, "example.invalid") {
		t.Fatalf("message %q leaks the request", message)
	}
}

// Client dying before any stream event on the first call and completing on the second, recording each input
func codexZeroEventThenCompletedClient(seen *[]any) *codexFakeClient {
	return &codexFakeClient{create: func(_ int, kwargs map[string]any) (any, error) {
		input := kwargs["input"]
		if input == nil {
			if extra, ok := kwargs["extra_body"].(map[string]any); ok {
				input = extra["input"]
			}
		}
		*seen = append(*seen, input)
		if len(*seen) == 1 {
			return nil, &AuxTransportError{Class: "ConnectError", Message: "no first byte"}
		}
		return newCodexFakeStream(codexStreamEv("response.output_item.done", "item", codexTextMessage("ok")),
			codexStreamCompleted("usage", map[string]any{"input_tokens": float64(1), "output_tokens": float64(1), "total_tokens": float64(2)}, "id", "r1")), nil
	}}
}

func codexOversizedKwargs(size int) map[string]any {
	return map[string]any{"model": "gpt-5-codex", "instructions": "You are Hermes.", "store": false, "tools": nil,
		"input": []any{
			map[string]any{"role": "user", "content": "look"},
			map[string]any{"type": "function_call", "call_id": "call_browser", "name": "browser_exec", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_browser", "output": strings.Repeat("x", size)},
			map[string]any{"role": "user", "content": "continue"},
		}}
}

// Request input items of either list shape the wire receives
func codexInputItems(input any) []map[string]any {
	if items, ok := input.([]map[string]any); ok {
		return items
	}
	list, _ := input.([]any)
	return mapsOfList(list)
}

func codexZeroEventLogs(logs *logCapture) []string {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	var out []string
	for _, line := range logs.lines {
		if strings.Contains(line, "zero-event") {
			out = append(out, line)
		}
	}
	return out
}

func TestZeroEventRetryPrunesOversizedToolOutputAndLogsSizeDelta(t *testing.T) {
	h := codexStreamHome(t)
	agent := newCodexStreamAgent()
	var seen []any
	agent.primary = codexZeroEventThenCompletedClient(&seen)
	logs := captureLogs(t)
	out, err := RunCodexStream(context.Background(), agent, codexOversizedKwargs(760_396), nil, nil)
	resp := codexMustResult(t, out, err)
	if resp["status"] != "completed" || len(seen) != 2 {
		t.Fatalf("response %v seen %d", resp, len(seen))
	}
	first, _ := json.Marshal(seen[0])
	second, _ := json.Marshal(seen[1])
	if len(second) >= len(first)/10 {
		t.Fatalf("sizes %d -> %d", len(first), len(second))
	}
	retried, _ := codexInputItems(seen[1])[2]["output"].(string)
	if !strings.Contains(retried, tools.PersistedOutputTag) || len(retried) >= 10_000 {
		t.Fatalf("retried output %q", retried[:min(len(retried), 200)])
	}
	spilled, err := os.ReadFile(filepath.Join(h, "cache", "spillover", "call_browser.txt"))
	if err != nil || string(spilled) != strings.Repeat("x", 760_396) {
		t.Fatalf("spill %d %v", len(spilled), err)
	}
	if codexInputItems(seen[0])[2]["output"] != strings.Repeat("x", 760_396) {
		t.Fatal("caller kwargs mutated")
	}
	prune := codexZeroEventLogs(logs)
	if len(prune) != 1 || !strings.Contains(prune[0], "attempt 1/2") || !regexp.MustCompile(`serialized_input_bytes=\d+ -> \d+`).MatchString(prune[0]) {
		t.Fatalf("prune logs %v", prune)
	}
}

func TestZeroEventRetryWithoutPrunableOutputLogsUnchangedResend(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	var seen []any
	agent.primary = codexZeroEventThenCompletedClient(&seen)
	kwargs := codexOversizedKwargs(300_000)
	input := kwargs["input"].([]any)
	input[2].(map[string]any)["output"] = []any{"not-a-string"}
	input[0].(map[string]any)["content"] = strings.Repeat("u", 300_000)
	logs := captureLogs(t)
	out, err := RunCodexStream(context.Background(), agent, kwargs, nil, nil)
	resp := codexMustResult(t, out, err)
	if resp["status"] != "completed" || len(seen) != 2 || !reflect.DeepEqual(seen[0], seen[1]) {
		t.Fatalf("response %v seen %d", resp, len(seen))
	}
	prune := codexZeroEventLogs(logs)
	if len(prune) != 1 || !strings.Contains(prune[0], "unchanged") || !strings.Contains(prune[0], "attempt 1/2") || !regexp.MustCompile(`serialized_input_bytes=\d+`).MatchString(prune[0]) {
		t.Fatalf("prune logs %v", prune)
	}
}

func codexOpenRouterAgent() *codexStreamAgent {
	agent := newCodexStreamAgent()
	agent.provider, agent.model, agent.baseURL = "openrouter", "test/model", "https://openrouter.ai/api/v1"
	return agent
}

func TestCodexStreamCallbacksCodexRemoteProtocolErrorRetriesThenRaises(t *testing.T) {
	codexStreamHome(t)
	agent := codexOpenRouterAgent()
	client := codexStreamClient(func() any {
		return &AuxTransportError{Class: "RemoteProtocolError", Message: "peer closed connection without sending complete message body"}
	})
	captureLogs(t)
	_, err := RunCodexStream(context.Background(), agent, map[string]any{}, client, nil)
	var transport *AuxTransportError
	if !errors.As(err, &transport) || transport.Class != "RemoteProtocolError" || client.callCount() != 2 {
		t.Fatalf("err %T %v calls %d", err, err, client.callCount())
	}
}

func TestCodexStreamCallbacksCodexCreateStreamFallbackRefreshesActivityOnEveryEvent(t *testing.T) {
	codexStreamHome(t)
	agent := codexOpenRouterAgent()
	events := []map[string]any{
		codexStreamEv("response.output_text.delta", "delta", "Hello"),
		codexStreamEv("response.output_item.done", "item", map[string]any{"type": "message"}),
		codexStreamEv("response.completed", "response", map[string]any{"output": []any{map[string]any{"type": "message",
			"content": []any{map[string]any{"type": "output_text", "text": "Hello"}}}}}),
	}
	client := codexStreamClient(func() any { return newCodexFakeStream(events...) })
	if _, err := RunCodexStream(context.Background(), agent, map[string]any{"model": "test/model", "instructions": "hi", "input": []any{}}, client, nil); err != nil {
		t.Fatal(err)
	}
	agent.fakeChatAgent.mu.Lock()
	touches := 0
	for _, desc := range agent.touches {
		if desc == "receiving stream response" {
			touches++
		}
	}
	agent.fakeChatAgent.mu.Unlock()
	if touches != len(events) {
		t.Fatalf("touches %d", touches)
	}
}

func TestCodexStreamWireErrorEventSurfacesStreamErrorEvent(t *testing.T) {
	codexStreamHome(t)
	providerMessage := "You do not have an active Grok subscription"
	agent := newCodexStreamAgent()
	client := codexStreamClient(func() any {
		return newCodexFakeStream(codexStreamEv("error", "message", providerMessage, "code", "forbidden"))
	})
	_, err := RunCodexStream(context.Background(), agent, map[string]any{}, client, nil)
	var streamErr *CodexStreamErrorEvent
	if !errors.As(err, &streamErr) || !strings.Contains(err.Error(), providerMessage) {
		t.Fatalf("err %T %v", err, err)
	}
	if body, _ := streamErr.Body["error"].(map[string]any); body["message"] != providerMessage {
		t.Fatalf("body %v", streamErr.Body)
	}
}

func TestCodexStreamWireErrorEventNestedEnvelopeAttrStyle(t *testing.T) {
	codexStreamHome(t)
	agent := newCodexStreamAgent()
	client := codexStreamClient(func() any {
		return newCodexFakeStream(codexStreamEv("error", "message", nil, "code", nil, "param", nil, "error",
			map[string]any{"type": "rate_limit_error", "code": "rate_limit_exceeded", "message": "Slow down", "param": nil}))
	})
	_, err := RunCodexStream(context.Background(), agent, map[string]any{}, client, nil)
	var streamErr *CodexStreamErrorEvent
	if !errors.As(err, &streamErr) || !strings.Contains(err.Error(), "Slow down") || streamErr.Code != "rate_limit_exceeded" {
		t.Fatalf("err %T %v", err, err)
	}
	if body, _ := streamErr.Body["error"].(map[string]any); body["code"] != "rate_limit_exceeded" || body["param"] != nil || body["type"] != "error" {
		t.Fatalf("body %v", streamErr.Body)
	}
}

func TestCodexStreamAssemblerKeepsTheServedTier(t *testing.T) {
	done := map[string]any{"type": "response.completed", "response": map[string]any{"id": "r1", "status": "completed", "service_tier": "default",
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}}
	events := []any{map[string]any{"type": "response.output_text.delta", "delta": "PASS"}, done}
	out, err := consumeCodexEventStream(context.Background(), codexSliceEvents(events), "gpt-6-astra", codexConsumeOptions{})
	if err != nil || out["service_tier"] != "default" {
		t.Fatalf("response %v err %v", out, err)
	}
}
