package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

// OpenAI shaped wire client whose request stalls until its sockets die
type e2eStallingWire struct {
	requestStarted, socketsShutDown chan struct{}
	startOnce, shutOnce             sync.Once
	closeCalls                      atomic.Int32
	timeout                         atomic.Value
}

func newE2eStallingWire() *e2eStallingWire {
	return &e2eStallingWire{requestStarted: make(chan struct{}), socketsShutDown: make(chan struct{})}
}

func (w *e2eStallingWire) CreateChatCompletion(ctx context.Context, kw map[string]any) (any, error) {
	if kw["timeout"] != nil {
		w.timeout.Store(kw["timeout"])
	}
	w.startOnce.Do(func() { close(w.requestStarted) })
	<-w.socketsShutDown
	return nil, errors.New("ConnectionError: socket shut down")
}

func (w *e2eStallingWire) Close() error {
	w.closeCalls.Add(1)
	return nil
}

func (w *e2eStallingWire) shutDown() { w.shutOnce.Do(func() { close(w.socketsShutDown) }) }

func TestE2eCronTurnIsBoundedThroughTheRealAgentRouting(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv(provider.APICallStaleTimeoutEnv, "0.3")
	wire := newE2eStallingWire()
	a := &AIAgent{platform: "cron", apiMode: "chat_completions", provider: "openrouter", model: "some/model",
		clientKwargs: map[string]any{"api_key": "***", "base_url": "https://openrouter.ai/api/v1"}}
	a.setBaseURL("https://openrouter.ai/api/v1")
	prevCtor, prevClose := openAIClientConstructor, forceCloseTCPSockets
	openAIClientConstructor = func(map[string]any) (any, error) { return wire, nil }
	forceCloseTCPSockets = func(any) int { wire.shutDown(); return 1 }
	t.Cleanup(func() { openAIClientConstructor, forceCloseTCPSockets = prevCtor, prevClose })
	_, err := a.InterruptibleAPICall(context.Background(), map[string]any{"model": a.model, "messages": []any{}})
	var te *provider.TimeoutPhaseError
	if !errors.As(err, &te) {
		t.Fatalf("err %v", err)
	}
	if wire.timeout.Load() != 0.3 {
		t.Fatalf("stale budget reached the wire as %v", wire.timeout.Load())
	}
	select {
	case <-wire.requestStarted:
	default:
		t.Fatal("request never reached the wire")
	}
	select {
	case <-wire.socketsShutDown:
	default:
		t.Fatal("sockets never shut down")
	}
	if wire.closeCalls.Load() != 1 {
		t.Fatalf("close calls %d", wire.closeCalls.Load())
	}
}
