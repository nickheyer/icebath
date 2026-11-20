package provider

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/tools"
)

// The AIAgent of the reuse race cases, a chat completions agent on the test model
func reuseRaceAgent(t *testing.T) *fakeChatAgent {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &fakeChatAgent{provider: "custom", apiMode: "chat_completions", baseURL: "https://example.com/v1", model: "test/model"}
}

// One streamed chat chunk carrying content
func reuseRaceChunk(content any) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content, "tool_calls": nil, "reasoning_content": nil, "reasoning": nil},
			"finish_reason": nil}},
		"model": "test/model", "usage": nil,
	}
}

// Token stream that raises the agent interrupt between its two chunks and counts its closes
type reuseRaceStream struct {
	mu          sync.Mutex
	agent       *fakeChatAgent
	closeRaises bool
	served      int
	closeCalls  int
}

func (s *reuseRaceStream) Next(context.Context) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served++
	switch s.served {
	case 1:
		return reuseRaceChunk("partial "), nil
	case 2:
		s.agent.mu.Lock()
		s.agent.interrupted = true
		s.agent.mu.Unlock()
		return reuseRaceChunk("never processed"), nil
	}
	return nil, io.EOF
}

func (s *reuseRaceStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	if s.closeRaises {
		return errors.New("close failed")
	}
	return nil
}

func (s *reuseRaceStream) closes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCalls
}

// Wire client whose chat completions create answers the stream
type reuseRaceClient struct{ stream *reuseRaceStream }

func (c *reuseRaceClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return c.stream, nil
}

// The managed relay turn of the agent's session with managed execution retained
func reuseRaceManagedTurn(t *testing.T, sessionID string) *relayLLMFake {
	t.Helper()
	f := relayLLMInstallFake(t, "profile")
	ctx := pyctx.With(context.Background(), pyctx.New())
	lease := tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: sessionID, Platform: "cli"})
	turn, err := tools.RelayCoordinator.BeginTurn(ctx, lease, "request-client-reuse-turn", "request-client-reuse-task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Host.RetainManagedExecution("test.request_client_reuse"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		lease.Host.ReleaseManagedExecution("test.request_client_reuse")
		tools.RelayCoordinator.EndTurn(ctx, turn, "success")
		tools.RelayCoordinator.ReleaseConversation(lease)
	})
	return f
}

func TestWorkerInterruptBreakClosesStream(t *testing.T) {
	agent := reuseRaceAgent(t)
	stream := &reuseRaceStream{agent: agent}
	client := &reuseRaceClient{stream: stream}
	agent.create = func(string, map[string]any) (any, error) { return client, nil }
	_, err := InterruptibleStreamingAPICall(context.Background(), agent, map[string]any{}, nil)
	var interrupted *InterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("want InterruptedError, got %v", err)
	}
	if n := stream.closes(); n != 1 {
		t.Fatalf("close calls %d", n)
	}
}

func TestRelayManagedCloseFailurePoisonsRequestClient(t *testing.T) {
	agent := reuseRaceAgent(t)
	stream := &reuseRaceStream{agent: agent, closeRaises: true}
	client := &reuseRaceClient{stream: stream}
	f := reuseRaceManagedTurn(t, agent.SessionID())
	var managed []string
	streamExecute := f.relay.LLM.StreamExecute
	f.relay.LLM.StreamExecute = func(c context.Context, operation string, request any, provider tools.RelayProviderStreamCallback, observe func(context.Context, any) error, finalizer func(context.Context) (any, error), kwargs map[string]any) (any, error) {
		managed = append(managed, operation)
		return streamExecute(c, operation, request, provider, observe, finalizer, kwargs)
	}
	agent.create = func(string, map[string]any) (any, error) { return client, nil }
	_, err := InterruptibleStreamingAPICall(context.Background(), agent, map[string]any{
		"model": "test/model", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	var interrupted *InterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("want InterruptedError, got %v", err)
	}
	if n := stream.closes(); n != 1 {
		t.Fatalf("close calls %d", n)
	}
	if len(managed) != 1 || managed[0] != "openai.chat_completions" {
		t.Fatalf("stream did not run through Relay %v", managed)
	}
	agent.mu.Lock()
	aborts := append([]clientCall(nil), agent.aborts...)
	agent.mu.Unlock()
	if len(aborts) != 1 || aborts[0].client != any(client) || aborts[0].reason != "interrupt_stream_close_failed" {
		t.Fatalf("abort reasons %+v", aborts)
	}
}
