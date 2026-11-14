package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Calls the stream hooks of the agent recorded for the assertions
type streamFake struct {
	mu                        sync.Mutex
	codexFirst                func()
	events                    []string
	deltas, reasoning, tools  []string
	consumers, wantsReasoning bool
	streamingDisabled         bool
	requestID                 string
	subagent                  bool
	fallbackIndex             int
	streamedText              string
	suppressed                []string
	warnings                  bool
	firstChunkAt              float64
}

func (a *fakeChatAgent) st() *streamFake {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.strm == nil {
		a.strm = &streamFake{consumers: true}
	}
	return a.strm
}

func (a *fakeChatAgent) record(event string) {
	s := a.st()
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (a *fakeChatAgent) EmitStreamStart() { a.record("start") }
func (a *fakeChatAgent) EmitStreamEnd(finalText string, finished bool, errText *string) {
	e := "end text=" + finalText
	if finished {
		e += " finished"
	}
	if errText != nil {
		e += " error=" + *errText
	}
	a.record(e)
}
func (a *fakeChatAgent) SetCodexOnFirstDelta(fn func()) { a.st().codexFirst = fn }
func (a *fakeChatAgent) FireStreamDelta(text string) {
	s := a.st()
	s.mu.Lock()
	s.deltas = append(s.deltas, text)
	s.mu.Unlock()
}
func (a *fakeChatAgent) FireToolGenStarted(name string) {
	s := a.st()
	s.mu.Lock()
	s.tools = append(s.tools, name)
	s.mu.Unlock()
}
func (a *fakeChatAgent) FireReasoningDelta(text string) {
	s := a.st()
	s.mu.Lock()
	s.reasoning = append(s.reasoning, text)
	s.mu.Unlock()
}
func (a *fakeChatAgent) HasStreamConsumers() bool    { return a.st().consumers }
func (a *fakeChatAgent) WantsReasoningDeltas() bool  { return a.st().wantsReasoning }
func (a *fakeChatAgent) DisableStreaming()           { a.st().streamingDisabled = true }
func (a *fakeChatAgent) CurrentAPIRequestID() string { return a.st().requestID }
func (a *fakeChatAgent) IsSubagent() bool            { return a.st().subagent }
func (a *fakeChatAgent) FallbackIndex() int          { return a.st().fallbackIndex }
func (a *fakeChatAgent) CurrentStreamedAssistantText() string {
	s := a.st()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamedText
}
func (a *fakeChatAgent) RouteSuppressedStreamText(text string) {
	s := a.st()
	s.mu.Lock()
	s.suppressed = append(s.suppressed, text)
	s.mu.Unlock()
}
func (a *fakeChatAgent) ResetStreamDeliveryTracking() {
	s := a.st()
	s.mu.Lock()
	s.streamedText = ""
	s.mu.Unlock()
}
func (a *fakeChatAgent) WarningPresentationEnabled() bool { return a.st().warnings }
func (a *fakeChatAgent) LogPrefix() string                { return "" }
func (a *fakeChatAgent) SetLastAPIFirstChunkAt(at float64) {
	s := a.st()
	s.mu.Lock()
	s.firstChunkAt = at
	s.mu.Unlock()
}

func TestStreamFinalTextReadsChoicesContentAndParts(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "chat"}}}}, "chat"},
		{map[string]any{"content": "plain"}, "plain"},
		{map[string]any{"content": []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "tool_use"}, map[string]any{"text": "b"}}}, "ab"},
		{map[string]any{"choices": []any{}}, ""},
		{&bedrockConverseResult{Content: "bedrock"}, "bedrock"},
		{&Response{Message: &state.Message{Role: "assistant", Content: "typed"}}, "typed"},
		{(*Response)(nil), ""},
		{nil, ""},
	}
	for i, c := range cases {
		if got := streamFinalText(c.in); got != c.want {
			t.Errorf("case %d got %q want %q", i, got, c.want)
		}
	}
}

func TestWithStreamEmittersBracketsSuccessAndFailure(t *testing.T) {
	a := &fakeChatAgent{}
	resp, err := WithStreamEmitters(a, func() (any, error) { return map[string]any{"content": "done"}, nil })
	if err != nil || resp == nil {
		t.Fatal(resp, err)
	}
	_, err = WithStreamEmitters(a, func() (any, error) { return nil, errors.New("boom") })
	if err == nil || err.Error() != "boom" {
		t.Fatal(err)
	}
	want := []string{"start", "end text=done finished", "start", "end text= error=boom"}
	if !reflect.DeepEqual(a.st().events, want) {
		t.Fatalf("%q", a.st().events)
	}
	if _, err := WithStreamEmitters(nil, func() (any, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
}

type codexPassAgent struct {
	*fakeChatAgent
	parked bool
}

func (a *codexPassAgent) InterruptibleAPICall(ctx context.Context, apiKwargs map[string]any) (any, error) {
	a.parked = a.CodexOnFirstDelta() != nil
	return map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "codex text"}}}}, nil
}

func TestStreamCodexPassthroughParksFirstDeltaAndBracketsTheCall(t *testing.T) {
	a := &codexPassAgent{fakeChatAgent: &fakeChatAgent{apiMode: "codex_responses"}}
	if _, err := StreamCodexPassthrough(t.Context(), a, map[string]any{"model": "gpt-5"}, func() {}); err != nil {
		t.Fatal(err)
	}
	if !a.parked || a.CodexOnFirstDelta() != nil {
		t.Fatalf("parked %v after %v", a.parked, a.CodexOnFirstDelta() != nil)
	}
	if want := []string{"start", "end text=codex text finished"}; !reflect.DeepEqual(a.st().events, want) {
		t.Fatalf("%q", a.st().events)
	}
}

// Encodes one Converse stream event as an AWS event stream frame
func bedrockEventFrame(t *testing.T, kind string, payload string) []byte {
	t.Helper()
	frame, err := awsEncodeEventStreamMessage([]awsEventStreamHeader{
		{Name: ":event-type", Type: awsHeaderString, Value: kind},
		{Name: ":content-type", Type: awsHeaderString, Value: "application/json"},
		{Name: ":message-type", Type: awsHeaderString, Value: "event"},
	}, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// Event stream body of a partial answer, closed by messageStop when terminal
func bedrockPartialAnswerBody(t *testing.T, terminal bool) []byte {
	body := append(bedrockEventFrame(t, "messageStart", `{"role":"assistant"}`), bedrockEventFrame(t, "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"partial answer"}}`)...)
	body = append(body, bedrockEventFrame(t, "contentBlockStop", `{"contentBlockIndex":0}`)...)
	if terminal {
		body = append(body, bedrockEventFrame(t, "messageStop", `{"stopReason":"end_turn"}`)...)
	}
	return body
}

func TestBedrockStreamCallDeliversThroughTheAgentHooksBetweenTheEmitters(t *testing.T) {
	body := bedrockPartialAnswerBody(t, true)
	bedrockStreamServer(t, func(w http.ResponseWriter) bool {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(body)
		return false
	})
	a := &fakeChatAgent{provider: "bedrock", apiMode: "bedrock_converse", model: bedrockRetryModel}
	first := 0
	resp, err := BedrockStreamCall(t.Context(), a, map[string]any{"__bedrock_region__": "us-east-1", "modelId": bedrockRetryModel,
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"text": "hi"}}}}}, func() { first++ })
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := resp.(*bedrockConverseResult); !ok || r.Content != "partial answer" {
		t.Fatalf("%#v", resp)
	}
	s := a.st()
	if first != 1 || !reflect.DeepEqual(s.deltas, []string{"partial answer"}) || !reflect.DeepEqual(s.events, []string{"start", "end text=partial answer finished"}) {
		t.Fatalf("first %d deltas %q events %q", first, s.deltas, s.events)
	}
	a.st().consumers = false
	a.st().events = nil
	if _, err := BedrockStreamCall(t.Context(), a, map[string]any{"modelId": bedrockRetryModel, "messages": []any{}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(s.deltas) != 1 {
		t.Fatalf("text fired without a stream consumer %q", s.deltas)
	}
}

func TestBedrockStreamCallDenialDisablesStreamingAndPrintsTheNotice(t *testing.T) {
	bedrockStreamServer(t, func(w http.ResponseWriter) bool {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-amzn-ErrorType", "AccessDeniedException")
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"message":"User is not authorized to perform: bedrock:InvokeModelWithResponseStream"}`))
		return false
	})
	a := &fakeChatAgent{provider: "bedrock", apiMode: "bedrock_converse", model: bedrockRetryModel}
	resp, err := BedrockStreamCall(t.Context(), a, map[string]any{"__bedrock_region__": "us-east-1", "modelId": bedrockRetryModel, "messages": []any{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := resp.(*bedrockConverseResult); !ok || r.Content != "hi" {
		t.Fatalf("%#v", resp)
	}
	printed := a.s().printed
	if !a.st().streamingDisabled || len(printed) != 1 || !strings.Contains(printed[0], "InvokeModelWithResponseStream") {
		t.Fatalf("disabled %v printed %q", a.st().streamingDisabled, printed)
	}
}

func TestBedrockStreamCallInterruptRaisesInterruptedError(t *testing.T) {
	bedrockStreamServer(t, func(w http.ResponseWriter) bool {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(bedrockEventFrame(t, "messageStart", `{"role":"assistant"}`))
		w.(http.Flusher).Flush()
		return true
	})
	a := &fakeChatAgent{provider: "bedrock", apiMode: "bedrock_converse", model: bedrockRetryModel}
	go func() {
		time.Sleep(400 * time.Millisecond)
		a.mu.Lock()
		a.interrupted = true
		a.mu.Unlock()
	}()
	_, err := BedrockStreamCall(t.Context(), a, map[string]any{"modelId": bedrockRetryModel, "messages": []any{}}, nil)
	var ie *InterruptedError
	if !errors.As(err, &ie) || !strings.HasPrefix(ie.Message, "Agent interrupted during Bedrock API call") {
		t.Fatalf("%v", err)
	}
	if want := []string{"start", "end text= error=" + ie.Message}; !reflect.DeepEqual(a.st().events, want) {
		t.Fatalf("%q", a.st().events)
	}
}

// Typed client whose stream attempts run the scripted function
type scriptedStreamClient struct {
	route  Route
	calls  int
	stream func(ctx context.Context, call int, fn StreamFunc) (*Response, error)
}

func (c *scriptedStreamClient) Route() Route { return c.route }
func (c *scriptedStreamClient) Complete(context.Context, *Request) (*Response, error) {
	return nil, errors.New("blocking call in a stream test")
}
func (c *scriptedStreamClient) Stream(ctx context.Context, req *Request, fn StreamFunc) (*Response, error) {
	c.calls++
	return c.stream(ctx, c.calls, fn)
}
func (c *scriptedStreamClient) CountTokens(context.Context, *Request) (int, error) { return 0, nil }

func scriptedLoop(ctx context.Context, c *scriptedStreamClient, opts DoOptions) *doLoop {
	if opts.Stream == nil {
		opts.Stream = func(StreamEvent) error { return nil }
	}
	l := &doLoop{ctx: ctx, opts: opts, client: c, req: &Request{Model: c.route.Model}, route: c.route, session: &RetrySession{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now, sleep: func(context.Context, time.Duration) error { return nil }, maxRetries: 3}
	l.st = &RetryState{Route: c.route, Request: l.req, Client: c, MaxRetries: 3}
	return l
}

func TestStreamRetryIsHandedBackOnceTheRouteSwitchedUnderTheRequest(t *testing.T) {
	stall := func(context.Context, int, StreamFunc) (*Response, error) {
		return nil, &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: errors.New("stalled")}
	}
	route := Route{Provider: "openrouter", Model: "old/model", BaseURL: "https://openrouter.ai/api/v1", Mode: ModeChatCompletions}
	c := &scriptedStreamClient{route: route, stream: stall}
	l := scriptedLoop(t.Context(), c, DoOptions{LiveRoute: func() Route {
		if c.calls > 0 {
			return Route{Provider: "anthropic", Model: "claude-opus-4.8", BaseURL: "https://api.anthropic.com", Mode: ModeAnthropicMessages}
		}
		return route
	}})
	_, _, err := l.streamWithReconnects()
	if TimeoutPhase(err) == "" || c.calls != 1 {
		t.Fatalf("calls %d err %v", c.calls, err)
	}
	same := &scriptedStreamClient{route: route, stream: stall}
	if _, _, err := scriptedLoop(t.Context(), same, DoOptions{LiveRoute: func() Route { return route }}).streamWithReconnects(); err == nil || same.calls != streamRetries()+1 {
		t.Fatalf("unswitched route retried %d times, err %v", same.calls, err)
	}
}

type recordingEmitters struct{ events []string }

func (r *recordingEmitters) EmitStreamStart() { r.events = append(r.events, "start") }
func (r *recordingEmitters) EmitStreamEnd(finalText string, finished bool, errText *string) {
	e := "end " + finalText
	if !finished {
		e += " unfinished"
	}
	if errText != nil {
		e += " " + *errText
	}
	r.events = append(r.events, e)
}

func TestStreamAttemptsFireTheFirstDeltaOnceAndAreBracketedByTheEmitters(t *testing.T) {
	route := Route{Provider: "openrouter", Model: "m", Mode: ModeChatCompletions}
	c := &scriptedStreamClient{route: route, stream: func(_ context.Context, call int, fn StreamFunc) (*Response, error) {
		if call == 1 {
			return nil, &TimeoutPhaseError{Phase: TimeoutPhaseConnect, Err: errors.New("connect")}
		}
		for _, text := range []string{"he", "llo"} {
			if err := fn(StreamEvent{Kind: EventTextDelta, Text: text}); err != nil {
				return nil, err
			}
		}
		return &Response{Message: &state.Message{Role: "assistant", Content: "hello"}}, nil
	}}
	em := &recordingEmitters{}
	first := 0
	resp, _, err := scriptedLoop(t.Context(), c, DoOptions{StreamEmitters: em, OnFirstDelta: func() { first++ }}).streamWithReconnects()
	if err != nil || resp.Message.Text() != "hello" || first != 1 {
		t.Fatalf("resp %v err %v first %d", resp, err, first)
	}
	want := []string{"start", "end  unfinished connect", "start", "end hello"}
	if len(em.events) != 4 || em.events[0] != want[0] || !strings.HasPrefix(em.events[1], "end  unfinished") || em.events[2] != want[2] || em.events[3] != want[3] {
		t.Fatalf("%q", em.events)
	}
}

func TestStreamThatFinishesAfterTheInterruptRaisesThePostWorkerInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &scriptedStreamClient{route: Route{Provider: "openrouter", Model: "m", Mode: ModeChatCompletions}, stream: func(context.Context, int, StreamFunc) (*Response, error) {
		cancel()
		return &Response{Message: &state.Message{Role: "assistant", Content: "late"}}, nil
	}}
	em := &recordingEmitters{}
	_, _, err := scriptedLoop(ctx, c, DoOptions{StreamEmitters: em}).streamWithReconnects()
	var ie *InterruptedError
	if !errors.As(err, &ie) || ie.Message != "Agent interrupted during streaming API call (post-worker)" {
		t.Fatalf("%v", err)
	}
}
