package provider

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Stream whose first Next records its goroutine, signals started and blocks until closed
type auxBlockingChatStream struct {
	started   chan struct{}
	startOnce sync.Once
	closed    chan struct{}
	closeOnce sync.Once
	worker    atomic.Uint64
}

func newAuxBlockingChatStream() *auxBlockingChatStream {
	return &auxBlockingChatStream{started: make(chan struct{}), closed: make(chan struct{})}
}

func (s *auxBlockingChatStream) Next(context.Context) (map[string]any, error) {
	s.worker.Store(pythread.GoroutineID())
	s.startOnce.Do(func() { close(s.started) })
	select {
	case <-s.closed:
	case <-time.After(5 * time.Second):
	}
	return nil, errors.New("transport closed")
}

func (s *auxBlockingChatStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func (s *auxBlockingChatStream) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// Waits for the channel or fails the test with the message
func auxAwaitSignal(t *testing.T, ch <-chan struct{}, wait time.Duration, failure string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(wait):
		t.Fatal(failure)
	}
}

// Chat client whose streamed create hands back one blocking stream
type auxCancelGenericClient struct {
	auxEffectiveStamp
	stream *auxBlockingChatStream
	closed atomic.Bool
}

func (c *auxCancelGenericClient) BaseURL() string { return "https://example.test/v1" }
func (c *auxCancelGenericClient) APIKey() string  { return "test" }
func (c *auxCancelGenericClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return c.stream, nil
}

func (c *auxCancelGenericClient) Close() error {
	c.closed.Store(true)
	return c.stream.Close()
}

// Codex SDK double whose Responses create runs the installed function
type auxCancelCodexRealClient struct {
	auxEffectiveStamp
	stream *auxBlockingChatStream
	create func(kwargs map[string]any) (any, error)
	closed atomic.Bool
}

func (c *auxCancelCodexRealClient) BaseURL() string { return "https://example.test/codex" }
func (c *auxCancelCodexRealClient) APIKey() string  { return "test" }
func (c *auxCancelCodexRealClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	return nil, errors.New("chat completions unused")
}

func (c *auxCancelCodexRealClient) CreateResponse(_ context.Context, kwargs map[string]any) (any, error) {
	if c.create != nil {
		return c.create(kwargs)
	}
	return c.stream, nil
}

func (c *auxCancelCodexRealClient) Close() error {
	c.closed.Store(true)
	return c.stream.Close()
}

// Completed Codex event stream carrying one ok message
func auxCancelSuccessStream() *auxFakeEventStream {
	return &auxFakeEventStream{events: []map[string]any{
		{"type": "response.output_item.done", "item": map[string]any{"type": "message",
			"content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
		{"type": "response.completed", "response": map[string]any{"status": "completed", "id": "success", "usage": nil}},
	}}
}

// Runs the protected invoke on its own goroutine, cancels once the transport started and returns the outcome and wake latency
func auxCancelSilentRequest(t *testing.T, client AuxClient, started <-chan struct{}, invoke func(ctx context.Context, client AuxClient) (any, error)) (error, time.Duration) {
	t.Helper()
	cancel := &auxTestCancelEvent{}
	result := make(chan error, 1)
	go func() {
		ctx := AuxInterruptProtection(context.Background(), true, nil, cancel)
		_, err := invoke(ctx, client)
		result <- err
	}()
	auxAwaitSignal(t, started, 5*time.Second, "request never entered its silent transport")
	cancelledAt := time.Now()
	cancel.set.Store(true)
	select {
	case err := <-result:
		return err, time.Since(cancelledAt)
	case <-time.After(5 * time.Second):
		t.Fatal("explicit cancellation did not wake the silent request")
	}
	return nil, 0
}

func auxInvokeGeneric(ctx context.Context, client AuxClient) (any, error) {
	return auxRelaySyncCompletion(ctx, client, map[string]any{"model": "test", "messages": []any{}, "timeout": 30}, "", "",
		func(ctx context.Context, request map[string]any) (any, error) {
			return auxCreateWithProgress(ctx, client, request, "compression", true)
		})
}

func auxIsExplicitCancellation(err error) bool {
	var cancelled *AuxiliaryExplicitCancellation
	return errors.As(err, &cancelled)
}

func TestProtectedSilentProviderIsIsolatedAndRaisesFrozenExplicitCancel(t *testing.T) {
	stream := newAuxBlockingChatStream()
	client := &auxCancelGenericClient{stream: stream}
	defer stream.Close()
	err, elapsed := auxCancelSilentRequest(t, client, stream.started, auxInvokeGeneric)
	var cancelled *AuxiliaryExplicitCancellation
	if !errors.As(err, &cancelled) || cancelled.Cause() != "explicit_host_cancel" {
		t.Fatalf("outcome %v", err)
	}
	if client.closed.Load() || elapsed >= 750*time.Millisecond {
		t.Fatalf("closed=%v elapsed=%v", client.closed.Load(), elapsed)
	}
}

func TestCodexSilentStreamIsIsolatedWithoutClosingSharedClient(t *testing.T) {
	stream := newAuxBlockingChatStream()
	real := &auxCancelCodexRealClient{stream: stream}
	client := NewCodexAuxiliaryClient(real, "gpt-test")
	defer stream.Close()
	err, elapsed := auxCancelSilentRequest(t, client, stream.started, auxInvokeGeneric)
	if !auxIsExplicitCancellation(err) || real.closed.Load() || elapsed >= 750*time.Millisecond {
		t.Fatalf("outcome %v closed=%v elapsed=%v", err, real.closed.Load(), elapsed)
	}
}

func TestCancelledCodexOrphanTimeoutPreservesCachedSharedClient(t *testing.T) {
	auxIsolateClientCache(t)
	ownerStream := newAuxBlockingChatStream()
	real := &auxCancelCodexRealClient{stream: ownerStream}
	real.create = func(kwargs map[string]any) (any, error) {
		if real.closed.Load() {
			return nil, errors.New("shared client was closed")
		}
		if kwargs["model"] == "owner" {
			return ownerStream, nil
		}
		return auxCancelSuccessStream(), nil
	}
	wrapper := NewCodexAuxiliaryClient(real, "gpt-test")
	key := auxClientCacheKey{Home: home.Key(home.Dir(context.Background())), Provider: "openai-codex"}
	auxClientCacheMu.Lock()
	auxClientCacheSetLocked(key, auxCachedClient{Client: wrapper, DefaultModel: "gpt-test"})
	auxClientCacheMu.Unlock()
	cancel := &auxTestCancelEvent{}
	outcome := make(chan error, 1)
	go func() {
		ctx := AuxInterruptProtection(context.Background(), true, nil, cancel)
		_, err := auxRelaySyncCompletion(ctx, wrapper, map[string]any{"model": "owner", "messages": []any{}, "timeout": 0.12}, "", "", nil)
		outcome <- err
	}()
	defer ownerStream.Close()
	auxAwaitSignal(t, ownerStream.started, time.Second, "owner never entered its stream")
	cancel.set.Store(true)
	select {
	case err := <-outcome:
		if !auxIsExplicitCancellation(err) {
			t.Fatalf("owner outcome %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner did not return")
	}
	cancel.set.Store(false)
	if ownerStream.isClosed() {
		t.Fatal("owner stream closed before the orphan timer fired")
	}
	concurrent, err := auxRelaySyncCompletion(context.Background(), wrapper, map[string]any{"model": "concurrent", "messages": []any{}, "timeout": 1}, "", "", nil)
	if err != nil || ExtractContentOrReasoning(concurrent, -1) != "ok" {
		t.Fatalf("concurrent %v %v", concurrent, err)
	}
	auxAwaitSignal(t, ownerStream.closed, time.Second, "orphan timer never woke the owner stream")
	time.Sleep(30 * time.Millisecond)
	if real.closed.Load() {
		t.Fatal("the process shared client was closed")
	}
	auxClientCacheMu.Lock()
	cached := auxClientCache[key].Client
	auxClientCacheMu.Unlock()
	if cached != AuxClient(wrapper) {
		t.Fatalf("cached client %v", cached)
	}
	successive, err := auxRelaySyncCompletion(context.Background(), wrapper, map[string]any{"model": "successive", "messages": []any{}, "timeout": 1}, "", "", nil)
	if err != nil || ExtractContentOrReasoning(successive, -1) != "ok" {
		t.Fatalf("successive %v %v", successive, err)
	}
}

// Cancel source holding the watchdog goroutine at its snapshot until the test releases it
type auxRacingCancelSource struct {
	winner               string
	owner                atomic.Uint64
	worker               *atomic.Uint64
	requestCancelled     atomic.Bool
	timerReadStarted     chan struct{}
	startOnce            sync.Once
	allowTimerReadReturn chan struct{}
	timerReadStalled     atomic.Bool
}

func (s *auxRacingCancelSource) IsSet() bool {
	id := pythread.GoroutineID()
	if s.winner == "timeout" && s.worker.Load() != 0 && id != s.owner.Load() && id != s.worker.Load() {
		wasSet := s.requestCancelled.Load()
		s.startOnce.Do(func() { close(s.timerReadStarted) })
		select {
		case <-s.allowTimerReadReturn:
		case <-time.After(time.Second):
			s.timerReadStalled.Store(true)
		}
		return wasSet
	}
	return s.requestCancelled.Load()
}

func auxCodexLinearizedOutcomeCase(t *testing.T, winner string) {
	stream := newAuxBlockingChatStream()
	real := &auxCancelCodexRealClient{stream: stream}
	wrapper := NewCodexAuxiliaryClient(real, "gpt-test")
	source := &auxRacingCancelSource{winner: winner, worker: &stream.worker,
		timerReadStarted: make(chan struct{}), allowTimerReadReturn: make(chan struct{})}
	outcome := make(chan error, 1)
	go func() {
		source.owner.Store(pythread.GoroutineID())
		ctx := AuxInterruptProtection(context.Background(), true, nil, source)
		_, err := auxRelaySyncCompletion(ctx, wrapper, map[string]any{"model": "owner", "messages": []any{}, "timeout": 0.08}, "", "", nil)
		outcome <- err
	}()
	defer stream.Close()
	auxAwaitSignal(t, stream.started, time.Second, "owner never entered its stream")
	if winner == "timeout" {
		auxAwaitSignal(t, source.timerReadStarted, time.Second, "the watchdog never read the cancel source")
		source.requestCancelled.Store(true)
		close(source.allowTimerReadReturn)
	} else {
		source.requestCancelled.Store(true)
	}
	var err error
	select {
	case err = <-outcome:
	case <-time.After(time.Second):
		t.Fatal("owner did not return")
	}
	if source.timerReadStalled.Load() {
		t.Fatal("the watchdog read was never released")
	}
	var timedOut *AuxTimeoutError
	if winner == "timeout" {
		if !real.closed.Load() || !errors.As(err, &timedOut) || auxIsExplicitCancellation(err) {
			t.Fatalf("timeout outcome %v closed=%v", err, real.closed.Load())
		}
		return
	}
	if !auxIsExplicitCancellation(err) {
		t.Fatalf("cancel outcome %v", err)
	}
	auxAwaitSignal(t, stream.closed, time.Second, "cancelled timer did not wake its stream")
	if real.closed.Load() {
		t.Fatal("the shared client was closed")
	}
}

func TestCodexTimeoutAndExplicitCancelHaveOneLinearizedOutcome(t *testing.T) {
	for _, winner := range []string{"timeout", "cancel"} {
		t.Run(winner, func(t *testing.T) { auxCodexLinearizedOutcomeCase(t, winner) })
	}
}

func TestAnthropicSilentStreamIsIsolatedWithoutClosingSharedClient(t *testing.T) {
	stream := newAuxBlockingChatStream()
	prev := auxCreateAnthropicMessageFn
	auxCreateAnthropicMessageFn = func(ctx context.Context, _ *anthropicSDKClient, _ map[string]any, _ func(event any) error) (map[string]any, error) {
		return stream.Next(ctx)
	}
	t.Cleanup(func() { auxCreateAnthropicMessageFn = prev })
	real := &anthropicSDKClient{transport: &http.Transport{}}
	client := newAnthropicAuxiliaryClientFrom(real, "claude-test", "test-key", "https://api.anthropic.test", false)
	defer stream.Close()
	err, elapsed := auxCancelSilentRequest(t, client, stream.started, auxInvokeGeneric)
	if !auxIsExplicitCancellation(err) || real.IsClosed() || elapsed >= 750*time.Millisecond {
		t.Fatalf("outcome %v closed=%v elapsed=%v", err, real.IsClosed(), elapsed)
	}
}

func TestBedrockSilentNonstreamRequestIsIsolatedWithoutCloseWakeup(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce sync.Once
	prev := callBedrockConverseFn
	callBedrockConverseFn = func(context.Context, string, bedrockConverseInput) (*bedrockConverseResult, error) {
		startOnce.Do(func() { close(started) })
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return &bedrockConverseResult{Content: "cancelled response", FinishReason: "end_turn",
			Usage: bedrockConverseUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}, nil
	}
	t.Cleanup(func() { callBedrockConverseFn = prev })
	ResetBedrockClientCache()
	t.Cleanup(ResetBedrockClientCache)
	runtime := newAWSServiceClient(awsClientConfig{service: "bedrock-runtime", signingName: "bedrock", region: "us-test-1"})
	key := bedrockClientKey{service: "bedrock-runtime", region: "us-test-1"}
	bedrockClientsMu.Lock()
	bedrockClients[key] = runtime
	bedrockClientsMu.Unlock()
	client := NewBedrockAuxiliaryClient("us-test-1", "bedrock-test")
	defer close(release)
	err, elapsed := auxCancelSilentRequest(t, client, started, auxInvokeGeneric)
	bedrockClientsMu.Lock()
	cached := bedrockClients[key]
	bedrockClientsMu.Unlock()
	if !auxIsExplicitCancellation(err) || cached != runtime || elapsed >= 750*time.Millisecond {
		t.Fatalf("outcome %v cached=%p elapsed=%v", err, cached, elapsed)
	}
}

func TestUnprotectedSyncCompletionStaysOnCallingThread(t *testing.T) {
	caller := pythread.GoroutineID()
	var observed []uint64
	client := &auxCallFakeClient{create: func(context.Context, map[string]any) (any, error) {
		observed = append(observed, pythread.GoroutineID())
		return map[string]any{"choices": []any{}}, nil
	}}
	if _, err := auxRelaySyncCompletion(context.Background(), client, map[string]any{"model": "test", "messages": []any{}}, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 || observed[0] != caller {
		t.Fatalf("observed %v caller %d", observed, caller)
	}
}

func TestIsolatedProviderWorkerInheritsProtectionAndProgressHook(t *testing.T) {
	caller := pythread.GoroutineID()
	var mu sync.Mutex
	var progress []string
	var thread atomic.Uint64
	var protected atomic.Bool
	client := &auxCallFakeClient{create: func(ctx context.Context, _ map[string]any) (any, error) {
		thread.Store(pythread.GoroutineID())
		protected.Store(auxInterruptProtected(ctx))
		auxNotifyAuxProgress(ctx)
		return map[string]any{"choices": []any{}}, nil
	}}
	ctx := AuxProgressHook(context.Background(), func() error {
		mu.Lock()
		progress = append(progress, "tick")
		mu.Unlock()
		return nil
	})
	ctx = AuxInterruptProtection(ctx, true, nil, &auxTestCancelEvent{})
	if _, err := auxRelaySyncCompletion(ctx, client, map[string]any{"model": "test", "messages": []any{}}, "", "", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	ticked := len(progress) > 0
	mu.Unlock()
	if !protected.Load() || thread.Load() == caller || !ticked {
		t.Fatalf("protected=%v thread=%d caller=%d progress=%v", protected.Load(), thread.Load(), caller, progress)
	}
}

type auxIsolatedProviderTestKey struct{}

func TestIsolatedProviderWorkerInheritsCallerContextvars(t *testing.T) {
	env := &tools.Env{Home: t.TempDir(), Values: &sync.Map{}}
	tools.SetCurrentSessionKey(env, "session-from-caller")
	observed := sync.Map{}
	client := &auxCallFakeClient{create: func(ctx context.Context, _ map[string]any) (any, error) {
		observed.Store("arbitrary", ctx.Value(auxIsolatedProviderTestKey{}))
		observed.Store("session_key", tools.CurrentSessionKey(tools.ToolEnvFrom(ctx)))
		return map[string]any{"choices": []any{}}, nil
	}}
	ctx := context.WithValue(context.Background(), auxIsolatedProviderTestKey{}, "caller-value")
	ctx = tools.WithToolEnv(ctx, env)
	ctx = AuxInterruptProtection(ctx, true, nil, &auxTestCancelEvent{})
	if _, err := auxRelaySyncCompletion(ctx, client, map[string]any{"model": "test", "messages": []any{}}, "", "", nil); err != nil {
		t.Fatal(err)
	}
	arbitrary, _ := observed.Load("arbitrary")
	sessionKey, _ := observed.Load("session_key")
	if arbitrary != "caller-value" || sessionKey != "session-from-caller" {
		t.Fatalf("arbitrary=%v session_key=%v", arbitrary, sessionKey)
	}
}

func TestHardCancelWinsWhenProviderResultIsPublishedInSameRace(t *testing.T) {
	cancel := &auxTestCancelEvent{}
	client := &auxCallFakeClient{create: func(context.Context, map[string]any) (any, error) {
		cancel.set.Store(true)
		return map[string]any{"choices": []any{}}, nil
	}}
	ctx := AuxInterruptProtection(context.Background(), true, nil, cancel)
	_, err := auxRelaySyncCompletion(ctx, client, map[string]any{"model": "test", "messages": []any{}}, "", "", nil)
	if !auxIsExplicitCancellation(err) {
		t.Fatalf("outcome %v", err)
	}
}

func TestUnrelatedInterruptedErrorIsNotReclassifiedAsExplicitCancel(t *testing.T) {
	client := &auxCallFakeClient{create: func(context.Context, map[string]any) (any, error) {
		return nil, &InterruptedError{Message: "provider syscall interrupted"}
	}}
	ctx := AuxInterruptProtection(context.Background(), true, nil, &auxTestCancelEvent{})
	_, err := auxRelaySyncCompletion(ctx, client, map[string]any{"model": "test", "messages": []any{}}, "", "", nil)
	var interrupted *InterruptedError
	if !errors.As(err, &interrupted) || interrupted.Message != "provider syscall interrupted" || auxIsExplicitCancellation(err) {
		t.Fatalf("outcome %v", err)
	}
}
