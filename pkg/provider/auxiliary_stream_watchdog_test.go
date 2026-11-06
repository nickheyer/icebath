package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// Chunk stream whose blocked read wakes with an error when its pinned attempt is shut down
type auxGatedStream struct {
	chunks   chan map[string]any
	shut     chan struct{}
	once     sync.Once
	release  func()
	shutdown int
	mu       sync.Mutex
}

func newAuxGatedStream() *auxGatedStream {
	return &auxGatedStream{chunks: make(chan map[string]any, 8), shut: make(chan struct{})}
}

func (s *auxGatedStream) auxPinAttempt(onRelease func()) func() {
	s.release = onRelease
	return func() {
		s.mu.Lock()
		s.shutdown++
		s.mu.Unlock()
		s.once.Do(func() { close(s.shut) })
	}
}

func (s *auxGatedStream) Next(ctx context.Context) (map[string]any, error) {
	select {
	case c, ok := <-s.chunks:
		if !ok {
			if s.release != nil {
				s.release()
			}
			return nil, io.EOF
		}
		return c, nil
	case <-s.shut:
		return nil, errors.New("peer closed connection without sending complete message body")
	}
}

func (s *auxGatedStream) Close() error { return nil }

type auxTestAcc struct {
	content string
	finish  string
}

func (a *auxTestAcc) Feed(chunk map[string]any) bool {
	if c, ok := chunk["content"].(string); ok && c != "" {
		a.content += c
		return true
	}
	if f, ok := chunk["finish_reason"].(string); ok {
		a.finish = f
	}
	return false
}

func (a *auxTestAcc) HasFinishReason() bool { return a.finish != "" }

func TestChatStreamWatchdogCutsASilentStreamAtItsWindow(t *testing.T) {
	s := newAuxGatedStream()
	s.chunks <- map[string]any{"content": "partial"}
	started := time.Now()
	err := auxConsumeChatStream(context.Background(), s, &auxTestAcc{}, &auxStreamWindows{Window: 0.2})
	var timeout *AuxTimeoutError
	if !errors.As(err, &timeout) || !strings.Contains(err.Error(), "stalled: no new output for 0.2s") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(started) > 3*time.Second || timeout.Unwrap() == nil {
		t.Fatalf("cut late or without its cause: %v", time.Since(started))
	}
}

func TestChatStreamWatchdogNoOutputIsANoProgressTimeout(t *testing.T) {
	s := newAuxGatedStream()
	err := auxConsumeChatStream(context.Background(), s, &auxTestAcc{}, &auxStreamWindows{Window: 0.1, FirstTokenWindow: 0.2, HasFirstToken: true})
	if err == nil || !strings.Contains(err.Error(), "produced no output within 0.2s (no-progress timeout") {
		t.Fatalf("err = %v", err)
	}
}

func TestChatStreamWatchdogLetsAModelThinkThroughItsFirstTokenWindow(t *testing.T) {
	s := newAuxGatedStream()
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.chunks <- map[string]any{"content": "partial"}
		close(s.chunks)
	}()
	acc := &auxTestAcc{}
	err := auxConsumeChatStream(context.Background(), s, acc, &auxStreamWindows{Window: 0.1, FirstTokenWindow: 2, HasFirstToken: true})
	if err != nil || acc.content != "partial" {
		t.Fatalf("err = %v, content %q", err, acc.content)
	}
}

func TestChatStreamWatchdogKeepsAResponseWhoseTerminalChunkArrived(t *testing.T) {
	s := newAuxGatedStream()
	s.chunks <- map[string]any{"content": "partial"}
	s.chunks <- map[string]any{"finish_reason": "stop"}
	acc := &auxTestAcc{}
	if err := auxConsumeChatStream(context.Background(), s, acc, &auxStreamWindows{Window: 0.2}); err != nil {
		t.Fatalf("a stall after the terminal chunk is teardown, got %v", err)
	}
	if acc.content != "partial" || acc.finish != "stop" {
		t.Fatalf("acc = %+v", acc)
	}
}

func TestChatStreamWatchdogNeverShutsDownAReleasedAttempt(t *testing.T) {
	s := newAuxGatedStream()
	s.chunks <- map[string]any{"content": "a"}
	close(s.chunks)
	if err := auxConsumeChatStream(context.Background(), s, &auxTestAcc{}, &auxStreamWindows{Window: 0.05}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdown != 0 {
		t.Fatalf("released attempt was shut down %d times", s.shutdown)
	}
}

func TestChatStreamWatchdogPassesCancellationThrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &auxCancelStream{}
	err := auxConsumeChatStream(ctx, s, &auxTestAcc{}, &auxStreamWindows{Window: 5})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

type auxCancelStream struct{}

func (auxCancelStream) Next(ctx context.Context) (map[string]any, error) { return nil, ctx.Err() }
func (auxCancelStream) Close() error                                     { return nil }

func TestChatStreamWindowsCapAtTheRequestTimeout(t *testing.T) {
	ctx := modelsTestHome(t)
	local := &auxProbeClient{baseURL: "http://127.0.0.1:8080/v1"}
	w := auxChatStreamWindows(ctx, local, map[string]any{"model": "m", "timeout": 20.0}, "")
	if w.Window != 20 || w.HasFirstToken {
		t.Fatalf("local endpoint leaves prefill to the request timeout with the window capped, got %+v", w)
	}
	w = auxChatStreamWindows(ctx, local, map[string]any{"model": "m", "timeout": 7}, "")
	if w.Window != 7 {
		t.Fatalf("window capped at the request timeout, got %+v", w)
	}
}
