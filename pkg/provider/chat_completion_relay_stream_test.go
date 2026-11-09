package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Writer fence double whose claim hands out a fresh token and fences every older one
type relayTestFence struct {
	mu      sync.Mutex
	current int
}

func (f *relayTestFence) ClaimStreamWriter() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current++
	return f.current
}

func (f *relayTestFence) StreamWriterIsCurrent(token int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return token == f.current
}

func relayTestFenceCtx(ctx context.Context, fence any) context.Context {
	return withStreamCall(ctx, &streamCall{attempt: 1, hooks: streamHooks{fence: fence, identity: streamRelayIdentity{SessionID: "session-1", CallRole: "primary"}}})
}

func relayTestTextSink(texts *[]string) StreamFunc {
	return func(ev StreamEvent) error {
		if ev.Kind == EventTextDelta {
			*texts = append(*texts, ev.Text)
		}
		return nil
	}
}

func TestConsumeLoopStopsWhenSupersededMidStream(t *testing.T) {
	fence := &relayTestFence{}
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, chunk(map[string]any{"content": "first"}, nil, nil))
		fl.Flush()
		time.Sleep(100 * time.Millisecond)
		fence.ClaimStreamWriter()
		_, _ = io.WriteString(w, chunk(map[string]any{"content": "-stale-tail"}, "stop", map[string]any{"model": "m"}))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	c := newTestClient(t, ws.route("openrouter"))
	var delivered []string
	_, err := c.Stream(relayTestFenceCtx(context.Background(), fence), &Request{Model: "test/model", Messages: []*state.Message{{Role: "user", Content: "hi"}}}, relayTestTextSink(&delivered))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(delivered, ""); got != "first" || strings.Contains(got, "-stale-tail") {
		t.Fatalf("superseded stream delivered %q", got)
	}
}

func TestChatParserFailureClosesManagedStream(t *testing.T) {
	c := newTestClient(t, Route{Provider: "openrouter", Model: "test/model", BaseURL: "http://127.0.0.1:9/v1", APIKey: "k", Mode: ModeChatCompletions})
	ctx := context.Background()
	req := &Request{Model: "test/model", Messages: []*state.Message{{Role: "user", Content: "hi"}}}
	tm := c.streamTimeouts(req, wireSettings{}, 0)
	s := c.newStreamState(ctx, req, &ccPrepared{}, func(StreamEvent) error { return nil }, tm)
	raw := &relayLLMRawStream{chunks: []any{relayLLMTestOpaque{}}}
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	src := &managedRelaySource{ctx: attemptCtx, open: func() (*ManagedLlmStream, error) {
		return NewManagedLlmStream(attemptCtx, map[string]any{}, relayLLMFactory(raw), ManagedLlmStreamOptions{ChunkAdapter: func(v any) any { return v }})
	}}
	s.relay = &ccManagedRelay{src: src, acc: newRelayChatAccumulator()}
	_, err := c.consumeStream(ctx, attemptCtx, cancel, &ccKill{}, s.relay, nil, s, tm)
	if err == nil || !strings.Contains(err.Error(), "has no attribute 'choices'") {
		t.Fatalf("a chunk without choices must fail the parse, got %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw.mu.Lock()
		calls := raw.closeCalls
		raw.mu.Unlock()
		if calls == 1 {
			break
		}
		if calls > 1 || time.Now().After(deadline) {
			t.Fatalf("managed stream closed %d times", calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Stream intercept upper casing every text delta the relayed provider stream carries
func relayTestUpperIntercept(f *relayLLMFake, upper func(chunk map[string]any) map[string]any) {
	f.registerLLMStreamExecution("icebath-test-stream", 1, func(_ context.Context, name string, request *tools.RelayLLMRequest, sctx relayLLMFakeStreamContext, next relayLLMFakeNext) (*tools.RelayAsyncIterator, error) {
		return relayLLMTestGen(func(ctx context.Context, yield func(any) bool) error {
			upstream, err := next(ctx, request)
			if err != nil {
				return err
			}
			for {
				item, err := upstream.Next(ctx)
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				if !yield(upper(relayJSONableDict(item))) {
					return nil
				}
			}
		}), nil
	})
}

func TestManagedChatStreamDeliversPostInterceptChunksAndRecordsTheRelayResponse(t *testing.T) {
	ws := newWireServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		sseFrames(w, chunk(map[string]any{"role": "assistant", "content": "hello"}, nil, nil), chunk(map[string]any{"content": " world"}, "stop", nil), "data: [DONE]\n\n")
	})
	c := newTestClient(t, ws.route("openrouter"))
	f, ctx, _ := relayLLMTurn(t)
	relayTestUpperIntercept(f, func(m map[string]any) map[string]any {
		out := relayCopyMap(m)
		choices, _ := out["choices"].([]any)
		if len(choices) > 0 {
			choice := relayCopyMap(choices[0].(map[string]any))
			delta, _ := relayDictCopy(choice["delta"])
			if text, ok := delta["content"].(string); ok {
				delta["content"] = strings.ToUpper(text)
			}
			choice["delta"] = delta
			out["choices"] = []any{choice}
		}
		return out
	})
	defer f.deregister("icebath-test-stream")
	fence := &relayTestFence{}
	var delivered []string
	resp, err := c.Stream(relayTestFenceCtx(ctx, fence), &Request{Model: "m", Messages: []*state.Message{{Role: "user", Content: "hi"}}}, relayTestTextSink(&delivered))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(delivered, ""); got != "HELLO WORLD" || resp.Message.Content != "HELLO WORLD" {
		t.Fatalf("post intercept text: delivered %q content %q", got, resp.Message.Content)
	}
	if fence.current != 1 {
		t.Fatalf("managed attempt claimed the writer %d times", fence.current)
	}
	if len(ws.requests) != 1 || ws.request(0).Body["stream"] != true {
		t.Fatalf("provider requests %+v", ws.requests)
	}
}

func TestManagedAnthropicStreamMergesTheRewriteOverTheProviderMessage(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		srv := newAnthServer(t, anthReplySSE(anthSSEStart(), anthSSEBlockStart(0, anthText("")), anthSSETextDelta(0, "done"), anthSSEBlockStop(0),
			anthSSEMessageDelta("end_turn"), anthSSEStop()))
		c := srv.client(srv.route("", "sk-ant-api03-test"))
		f, ctx, _ := relayLLMTurn(t)
		relayTestUpperIntercept(f, func(m map[string]any) map[string]any {
			out := relayCopyMap(m)
			if delta, ok := relayDictCopy(out["delta"]); ok && rewrite && delta["type"] == "text_delta" {
				delta["text"] = strings.ToUpper(asStr(delta["text"]))
				out["delta"] = delta
			}
			return out
		})
		var delivered []string
		resp, err := c.Stream(relayTestFenceCtx(ctx, &relayTestFence{}), anthUserRequest("claude-test", "hi"), relayTestTextSink(&delivered))
		f.deregister("icebath-test-stream")
		want := "done"
		if rewrite {
			want = "DONE"
		}
		if err != nil || resp.Message.Content != want || strings.Join(delivered, "") != want || resp.FinishReason != "stop" {
			t.Fatalf("rewrite %v: %v %+v delivered %q", rewrite, err, resp, delivered)
		}
	}
}

func TestStreamReasoningDetailsSurviveReplayThroughTheRelayCollector(t *testing.T) {
	details := []any{
		map[string]any{"type": "opaque.native", "version": 1.0, "messages": []any{
			map[string]any{"type": "thinking", "signature": " signed+/=\n", "thinking": "α"},
			map[string]any{"type": "text", "text": "hello"},
			map[string]any{"type": "redacted_thinking", "data": "AA+/=="}},
			"projection": map[string]any{"content": "hello world", "tool_calls": []any{}}},
		map[string]any{"type": "opaque.future", "index": 7.0, "unknown": []any{nil, false, map[string]any{"signature": "\r\n"}}},
	}
	for _, delivery := range []string{"final", "multiple", "absent"} {
		want := details
		if delivery == "absent" {
			want = nil
		}
		deltas := []map[string]any{{"role": "assistant", "content": "hello"}, {"content": " world"}}
		switch delivery {
		case "multiple":
			deltas[0]["reasoning_details"] = details[:1]
			deltas = append(deltas, map[string]any{"reasoning_details": []any{}}, map[string]any{"reasoning_details": details[1:]})
		case "final":
			deltas = append(deltas, map[string]any{"reasoning_details": details})
		}
		acc := newRelayChatAccumulator()
		for i, delta := range deltas {
			var finish any
			if i == len(deltas)-1 {
				finish = "stop"
			}
			raw, _ := json.Marshal(map[string]any{"id": "local-proof", "object": "chat.completion.chunk", "created": 1, "model": "test-model",
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			var decoded map[string]any
			_ = json.Unmarshal(raw, &decoded)
			acc.Observe(decoded)
		}
		recorded := acc.Finalize()
		recorded["choices"].([]any)[0].(map[string]any)["index"] = 0
		recorded["id"], recorded["created"], recorded["object"] = "local-proof", 1, "chat.completion"
		message := recorded["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if message["content"] != "hello world" {
			t.Fatalf("%s: content %v", delivery, message["content"])
		}
		if got := message["reasoning_details"]; (want == nil && got != nil) || (want != nil && !reflect.DeepEqual(got, want)) {
			t.Fatalf("%s: recorded reasoning_details %#v", delivery, got)
		}
		c := newTestClient(t, Route{Provider: "custom", Model: "test-model", BaseURL: "http://127.0.0.1:9/v1", APIKey: "k", Mode: ModeChatCompletions})
		resp, err := c.normalizeResponse(recorded, 200, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var normalized []any
		if len(resp.Message.ReasoningDetails) > 0 {
			if err := json.Unmarshal(resp.Message.ReasoningDetails, &normalized); err != nil {
				t.Fatal(err)
			}
		}
		if (want == nil && normalized != nil) || (want != nil && !reflect.DeepEqual(normalized, want)) {
			t.Fatalf("%s: normalized reasoning_details %#v", delivery, normalized)
		}
	}
}

func TestNewRelayChatAccumulatorFoldsRelayedChunks(t *testing.T) {
	acc := NewRelayChatAccumulator()
	acc.Observe(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "hel"}}}})
	acc.Observe(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "lo"}, "finish_reason": "stop"}}})
	out := acc.Finalize()
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices %v", out)
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello" || choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Fatalf("finalized %v", out)
	}
}
