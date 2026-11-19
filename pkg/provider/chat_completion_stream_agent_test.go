package provider

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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

// Chat completions agent on a scripted request client under a private home
func scriptedStreamAgent(t *testing.T) *fakeChatAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &fakeChatAgent{provider: "custom", apiMode: "chat_completions", baseURL: "https://example.com/v1", model: "test/model", plat: "cli"}
}

func scriptedStreamKwargs() map[string]any {
	return map[string]any{"model": "test/model", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
}

// Chat completions agent a test re-points to another route while its request is in flight
type switchedRouteAgent struct {
	*fakeChatAgent
	switched atomic.Bool
}

func (a *switchedRouteAgent) Model() string {
	if a.switched.Load() {
		return "kimi-k2.6"
	}
	return a.fakeChatAgent.Model()
}

func (a *switchedRouteAgent) Provider() string {
	if a.switched.Load() {
		return "kimi-coding"
	}
	return a.fakeChatAgent.Provider()
}

func (a *switchedRouteAgent) BaseURL() string {
	if a.switched.Load() {
		return "https://api.moonshot.ai/v1"
	}
	return a.fakeChatAgent.BaseURL()
}

func TestStreamRetryIsHandedBackOnceTheRouteSwitchedUnderTheRequest(t *testing.T) {
	recordStreamBackoff(t)
	agent := &switchedRouteAgent{fakeChatAgent: scriptedStreamAgent(t)}
	var sent []string
	client := &fakeChatClient{create: func(_ context.Context, kw map[string]any) (any, error) {
		sent = append(sent, stringOf(kw["model"])+" via "+agent.BaseURL())
		agent.switched.Store(true)
		return nil, &AuxTransportError{Class: "ReadError", Message: "stale stream killed"}
	}}
	agent.create = func(string, map[string]any) (any, error) { return client, nil }
	_, err := InterruptibleStreamingAPICall(t.Context(), agent, scriptedStreamKwargs(), nil)
	var dropped *AuxTransportError
	if !errors.As(err, &dropped) || dropped.Class != "ReadError" || !reflect.DeepEqual(sent, []string{"test/model via https://example.com/v1"}) {
		t.Fatalf("sent %q err %v", sent, err)
	}
	same := scriptedStreamAgent(t)
	var models []string
	sameClient := &fakeChatClient{create: func(_ context.Context, kw map[string]any) (any, error) {
		models = append(models, stringOf(kw["model"]))
		if len(models) == 1 {
			return nil, &AuxTransportError{Class: "ReadError", Message: "stale stream killed"}
		}
		return &scriptedChatStream{chunks: []map[string]any{chatContentChunk("ok"), chatFinishChunk("stop")}}, nil
	}}
	same.create = func(string, map[string]any) (any, error) { return sameClient, nil }
	resp, err := InterruptibleStreamingAPICall(t.Context(), same, scriptedStreamKwargs(), nil)
	if err != nil || streamFinalText(resp) != "ok" || !reflect.DeepEqual(models, []string{"test/model", "test/model"}) {
		t.Fatalf("unswitched route sent %q, resp %v err %v", models, resp, err)
	}
}

func TestStreamAttemptsFireTheFirstDeltaOnceAndAreBracketedByTheEmitters(t *testing.T) {
	recordStreamBackoff(t)
	agent := scriptedStreamAgent(t)
	client := &fakeChatClient{}
	client.create = func(context.Context, map[string]any) (any, error) {
		if client.calls.Load() == 1 {
			return nil, &AuxTransportError{Class: "ConnectTimeout", Message: "connect"}
		}
		return &scriptedChatStream{chunks: []map[string]any{chatContentChunk("he"), chatContentChunk("llo"), chatFinishChunk("stop")}}, nil
	}
	agent.create = func(string, map[string]any) (any, error) { return client, nil }
	first := 0
	resp, err := InterruptibleStreamingAPICall(t.Context(), agent, scriptedStreamKwargs(), func() { first++ })
	if err != nil || streamFinalText(resp) != "hello" || first != 1 {
		t.Fatalf("resp %v err %v first %d", resp, err, first)
	}
	st := agent.st()
	st.mu.Lock()
	events, deltas := append([]string(nil), st.events...), append([]string(nil), st.deltas...)
	st.mu.Unlock()
	want := []string{"start", "end text= error=connect", "start", "end text=hello finished"}
	if !reflect.DeepEqual(events, want) || !reflect.DeepEqual(deltas, []string{"he", "llo"}) {
		t.Fatalf("events %q deltas %q", events, deltas)
	}
}

// Chat stream raising the agent interrupt once its last chunk was read
type interruptAtEndStream struct {
	scriptedChatStream
	agent *fakeChatAgent
}

func (s *interruptAtEndStream) Next(ctx context.Context) (map[string]any, error) {
	chunk, err := s.scriptedChatStream.Next(ctx)
	if err != nil {
		s.agent.setInterrupted(true)
	}
	return chunk, err
}

func TestStreamThatFinishesAfterTheInterruptRaisesTheInterrupt(t *testing.T) {
	agent := scriptedStreamAgent(t)
	client := &fakeChatClient{create: func(context.Context, map[string]any) (any, error) {
		return &interruptAtEndStream{agent: agent, scriptedChatStream: scriptedChatStream{chunks: []map[string]any{chatContentChunk("late"), chatFinishChunk("stop")}}}, nil
	}}
	agent.create = func(string, map[string]any) (any, error) { return client, nil }
	resp, err := InterruptibleStreamingAPICall(t.Context(), agent, scriptedStreamKwargs(), nil)
	var ie *InterruptedError
	if resp != nil || !errors.As(err, &ie) || !strings.HasPrefix(ie.Message, "Agent interrupted during streaming API call") {
		t.Fatalf("resp %v err %v", resp, err)
	}
}

func TestStreamOptionsRejectionRetriesWithoutUsageExtension(t *testing.T) {
	agent := scriptedStreamAgent(t)
	var sent []map[string]any
	client := &fakeChatClient{create: func(_ context.Context, kw map[string]any) (any, error) {
		sent = append(sent, kw)
		if len(sent) == 1 {
			return nil, &APIError{StatusCode: 400, Message: "extra_forbidden: stream_options", Kind: ErrInvalidRequest, Body: `{"error":{"message":"extra fields not permitted: stream_options"}}`}
		}
		return &scriptedChatStream{chunks: []map[string]any{chatContentChunk("ok"), chatFinishChunk("stop")}}, nil
	}}
	agent.create = func(string, map[string]any) (any, error) { return client, nil }
	resp, err := InterruptibleStreamingAPICall(t.Context(), agent, scriptedStreamKwargs(), nil)
	if err != nil || streamFinalText(resp) != "ok" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if !agent.RetrySession().StreamOptionsUnsupported || len(sent) != 2 {
		t.Fatalf("stream options latch not set: %+v after %d requests", agent.RetrySession(), len(sent))
	}
	if _, has := sent[0]["stream_options"]; !has {
		t.Fatalf("first request carried no stream_options: %v", sent[0])
	}
	if _, has := sent[1]["stream_options"]; has {
		t.Fatalf("compat retry still sent stream_options: %v", sent[1])
	}
}
