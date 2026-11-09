package agent

import (
	"context"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Raw Responses create stream double recording every pull label and its close
type codexAgentStream struct {
	mu       sync.Mutex
	events   []map[string]any
	labels   []string
	pos      int
	before   func(i int)
	produced []string
	closed   bool
}

func (s *codexAgentStream) Next(context.Context) (map[string]any, error) {
	s.mu.Lock()
	if s.pos >= len(s.events) {
		s.mu.Unlock()
		return nil, io.EOF
	}
	i := s.pos
	s.pos++
	if i < len(s.labels) {
		s.produced = append(s.produced, s.labels[i])
	}
	before := s.before
	s.mu.Unlock()
	if before != nil {
		before(i)
	}
	return s.events[i], nil
}

func (s *codexAgentStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *codexAgentStream) state() ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.produced...), s.closed
}

// Responses client double handing every create call to its hook
type codexAgentClient struct {
	mu     sync.Mutex
	calls  []map[string]any
	create func(kwargs map[string]any) (any, error)
}

func (c *codexAgentClient) BaseURL() string { return "" }

func (c *codexAgentClient) CreateResponse(_ context.Context, kwargs map[string]any) (any, error) {
	c.mu.Lock()
	c.calls = append(c.calls, kwargs)
	c.mu.Unlock()
	return c.create(kwargs)
}

// Client whose every create returns the given stream
func codexAgentClientOf(stream *codexAgentStream) *codexAgentClient {
	return &codexAgentClient{create: func(map[string]any) (any, error) { return stream, nil }}
}

// Codex responses agent on OpenRouter as the stream test fixtures build it
func newCodexOpenRouterAgent(t *testing.T) *AIAgent {
	t.Helper()
	a := newFixtureAgent(t, AIAgentOptions{APIKey: "test-key", BaseURL: "https://openrouter.ai/api/v1", Model: "test/model"})
	a.apiMode = "codex_responses"
	a.interruptRequested.Store(false)
	return a
}

func TestCodexSingleWriterCodexStreamClaimsWriterAndStopsWhenSuperseded(t *testing.T) {
	a := newCodexOpenRouterAgent(t)
	var mu sync.Mutex
	var delivered []string
	a.streamDeltaCallback = func(text string) {
		mu.Lock()
		delivered = append(delivered, text)
		mu.Unlock()
	}
	a.streamCallback = nil
	stream := &codexAgentStream{events: []map[string]any{
		{"type": "response.output_text.delta", "delta": "first", "item_id": "i1"},
		{"type": "response.output_text.delta", "delta": "-stale-tail", "item_id": "i1"},
		{"type": "response.completed", "response": map[string]any{"id": "r1", "status": "completed", "output": []any{}, "usage": nil}},
	}}
	stream.before = func(i int) {
		if i == 1 {
			a.ClaimStreamWriter()
		}
	}
	if _, err := a.RunCodexStream(context.Background(), map[string]any{"model": "gpt-5.3-codex"}, codexAgentClientOf(stream), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	joined := strings.Join(delivered, "")
	mu.Unlock()
	if joined != "first" || strings.Contains(joined, "-stale-tail") {
		t.Fatalf("delivered %q", joined)
	}
}

func TestCodexSingleWriterCodexInterruptClosesStreamWithoutDrainingProvider(t *testing.T) {
	a := newCodexOpenRouterAgent(t)
	a.streamDeltaCallback = func(string) { a.interruptRequested.Store(true) }
	a.streamCallback = nil
	stream := &codexAgentStream{labels: []string{"first", "lookahead", "terminal"}, events: []map[string]any{
		{"type": "response.output_text.delta", "delta": "first", "item_id": "i1"},
		{"type": "response.output_text.delta", "delta": "-unused", "item_id": "i1"},
		{"type": "response.completed", "response": map[string]any{"id": "r1", "status": "completed", "output": []any{}, "usage": nil}},
	}}
	if _, err := a.RunCodexStream(context.Background(), map[string]any{"model": "gpt-5.3-codex"}, codexAgentClientOf(stream), nil); err != nil {
		t.Fatal(err)
	}
	produced, closed := stream.state()
	if !reflect.DeepEqual(produced, []string{"first", "lookahead"}) || !closed {
		t.Fatalf("produced %v closed %v", produced, closed)
	}
}

func TestCodexStreamCallbacksCodexTextDeltaFiresCallback(t *testing.T) {
	var mu sync.Mutex
	var deltas []string
	a := newFixtureAgent(t, AIAgentOptions{APIKey: "test-key", BaseURL: "https://openrouter.ai/api/v1", Model: "test/model",
		StreamDeltaCallback: func(text string) {
			mu.Lock()
			deltas = append(deltas, text)
			mu.Unlock()
		}})
	a.apiMode = "codex_responses"
	a.interruptRequested.Store(false)
	stream := &codexAgentStream{events: []map[string]any{
		{"type": "response.created"},
		{"type": "response.output_text.delta", "delta": "Hello from Codex!"},
		{"type": "response.completed", "response": map[string]any{"status": "completed", "id": "r1", "usage": nil}},
	}}
	if _, err := a.RunCodexStream(context.Background(), map[string]any{}, codexAgentClientOf(stream), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, d := range deltas {
		found = found || d == "Hello from Codex!"
	}
	if !found {
		t.Fatalf("deltas %v", deltas)
	}
}

func TestWireGuardScopesRetentionDropByRealEndpoint(t *testing.T) {
	cases := []struct {
		baseURL, model string
		expectDropped  bool
	}{
		{"https://chatgpt.com/backend-api/codex", "gpt-5.6-sol", true},
		{"https://chatgpt.com/backend-api/codex", "gpt-5-codex", true},
		{"https://api.meta.ai/v1", "gpt-5.6-sol", false},
		{"https://bedrock-mantle.us-east-1.api.aws/v1", "openai.gpt-5.5", false},
		{"https://api.openai.com/v1", "gpt-5.6-sol", false},
		{"https://chatgpt.com/backend-api/other", "gpt-5.6-sol", false},
	}
	for _, tc := range cases {
		t.Run(tc.baseURL+" "+tc.model, func(t *testing.T) {
			a := newFixtureAgent(t, AIAgentOptions{Model: tc.model, APIMode: "codex_responses", BaseURL: tc.baseURL,
				APIKey: "codex-token", MaxIterations: 4})
			request := map[string]any{"model": tc.model, "prompt_cache_key": "cache-key-sentinel", "prompt_cache_retention": "24h",
				"input": []any{map[string]any{"role": "user", "content": "hi"}}}
			sanitized := provider.SanitizeConsumerCodexRequest(a, request)
			if _, kept := sanitized["prompt_cache_retention"]; kept == tc.expectDropped {
				t.Fatalf("retention kept %v, expected dropped %v", kept, tc.expectDropped)
			}
			if sanitized["prompt_cache_key"] != "cache-key-sentinel" || request["prompt_cache_retention"] != "24h" {
				t.Fatalf("sanitized %v request %v", sanitized, request)
			}
			sanitized["stream"] = true
			if _, leaked := request["stream"]; leaked {
				t.Fatal("sanitized mapping is the caller's request")
			}
		})
	}
}

func TestRunCodexStreamDeliversRedactedCommentaryOnce(t *testing.T) {
	a := newFixtureAgent(t, AIAgentOptions{Model: "gpt-5-codex", BaseURL: "https://chatgpt.com/backend-api/codex", APIKey: "codex-token",
		MaxIterations: 4})
	type delivery struct {
		text            string
		alreadyStreamed bool
	}
	var mu sync.Mutex
	var delivered []delivery
	var reasoningStreamed []string
	a.interimAssistantCallback = func(text string, alreadyStreamed bool) {
		mu.Lock()
		delivered = append(delivered, delivery{text, alreadyStreamed})
		mu.Unlock()
	}
	a.reasoningCallback = func(text string) {
		mu.Lock()
		reasoningStreamed = append(reasoningStreamed, text)
		mu.Unlock()
	}
	secret := "sk-" + strings.Repeat("A", 32)
	commentaryText := "Using credential " + secret + ". I'll inspect the repo."
	commentaryItem := map[string]any{"type": "message", "phase": "commentary", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": commentaryText}}}
	functionItem := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "terminal", "arguments": "{}"}
	a.client = &codexAgentClient{create: func(kwargs map[string]any) (any, error) {
		if kwargs["stream"] != true {
			t.Errorf("stream kwarg %v", kwargs["stream"])
		}
		return &codexAgentStream{events: []map[string]any{
			{"type": "response.output_item.added", "item": map[string]any{"type": "message", "phase": "commentary"}},
			{"type": "response.output_text.delta", "delta": commentaryText},
			{"type": "response.output_item.done", "item": commentaryItem},
			{"type": "response.reasoning_text.delta", "delta": "Private scratchpad."},
			{"type": "response.output_item.added", "item": map[string]any{"type": "function_call"}},
			{"type": "response.output_item.done", "item": functionItem},
			{"type": "response.completed", "response": map[string]any{"status": "completed"}},
		}}, nil
	}}
	request := map[string]any{"model": "gpt-5-codex", "instructions": "You are Hermes.",
		"input": []any{map[string]any{"role": "user", "content": "Ping"}}, "tools": nil, "store": false}
	response, err := a.RunCodexStream(context.Background(), request, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(delivered) != 1 || delivered[0].alreadyStreamed || strings.Contains(delivered[0].text, secret) ||
		!strings.Contains(delivered[0].text, "Using credential") || !reflect.DeepEqual(reasoningStreamed, []string{"Private scratchpad."}) {
		t.Fatalf("delivered %+v reasoning %v", delivered, reasoningStreamed)
	}
	mu.Unlock()
	normalized, err := provider.GetTransport("codex_responses").NormalizeResponse(response, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.emitInterimAssistantMessage(a.buildAssistantMessage(normalized, normalized.FinishReason))
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 1 {
		t.Fatalf("delivered again %+v", delivered)
	}
}
