package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Loopback TCP pair, the client side the attempt reads and the server side the test watches for EOF
func socketPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	reader, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	writer := <-accepted
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	return reader, writer
}

// Pool and stream wrapper layers over the raw socket whose close must never run from the monitor
type wrappedConn struct {
	net.Conn
	inner net.Conn
}

func (w *wrappedConn) NetConn() net.Conn { return w.inner }

func (w *wrappedConn) Close() error { panic("monitor must never close") }

func wrapperShape(raw net.Conn) net.Conn {
	return &wrappedConn{Conn: raw, inner: &wrappedConn{Conn: raw, inner: raw}}
}

func assertShutDown(t *testing.T, writer net.Conn) {
	t.Helper()
	_ = writer.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := writer.Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("the killed attempt's socket was not shut down: n=%d err=%v", n, err)
	}
}

func killTestCall(agent *fakeChatAgent) *streamingCall {
	c := newStreamingCall(context.Background(), agent, map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	c.staleTimeout = 5
	c.callDone, c.workerDone = make(chan struct{}), make(chan struct{})
	return c
}

func killTestAgent(t *testing.T) *fakeChatAgent {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return &fakeChatAgent{provider: "custom", apiMode: "chat_completions", baseURL: "http://127.0.0.1:1/v1", model: "m"}
}

func TestShutdownReachesSocketThroughRealHttpxWrapperShape(t *testing.T) {
	reader, writer := socketPair(t)
	c := killTestCall(killTestAgent(t))
	resp := &http.Response{Header: http.Header{}}
	c.attemptResponse, c.attemptConn = resp, func() net.Conn { return wrapperShape(reader) }
	streamingCallShutdownAttemptSocket(c, resp)
	assertShutDown(t, writer)
	other := &http.Response{Header: http.Header{}}
	streamingCallShutdownAttemptSocket(c, other)
}

// Provider that holds the first stream open without a byte and completes every later one
type silentFirstRequest struct {
	mu          sync.Mutex
	completions int
	release     chan struct{}
	server      *httptest.Server
}

func newSilentFirstRequest(t *testing.T) *silentFirstRequest {
	w := &silentFirstRequest{release: make(chan struct{})}
	w.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		w.mu.Lock()
		w.completions++
		first := w.completions == 1
		w.mu.Unlock()
		rw.Header().Set("content-type", "text/event-stream")
		rw.WriteHeader(http.StatusOK)
		rw.(http.Flusher).Flush()
		if first {
			select {
			case <-w.release:
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
			return
		}
		fmt.Fprint(rw, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"reconnected"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(rw, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(rw, "data: [DONE]\n\n")
		rw.(http.Flusher).Flush()
	}))
	t.Cleanup(func() {
		close(w.release)
		w.server.Close()
	})
	return w
}

func (w *silentFirstRequest) requests() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.completions
}

func TestWedgedStreamUnwindsWithinItsStaleBudgetAndReconnects(t *testing.T) {
	wire := newSilentFirstRequest(t)
	agent := killTestAgent(t)
	t.Setenv("ICEBATH_STREAM_STALE_TIMEOUT", "1")
	t.Setenv("ICEBATH_STREAM_RETRIES", "1")
	t.Setenv("ICEBATH_STREAM_READ_TIMEOUT", "20")
	agent.baseURL, agent.plat = wire.server.URL+"/v1", "cli"
	agent.create = func(string, map[string]any) (any, error) {
		return newAuxOpenAIClient("test-key", wire.server.URL+"/v1", auxOpenAIOptions{}), nil
	}
	started := time.Now()
	response, err := InterruptibleStreamingAPICall(context.Background(), agent, map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("streaming call failed: %v", err)
	}
	if wire.requests() < 2 {
		t.Fatalf("the killed attempt never reconnected (requests=%d); the reader stayed parked instead of unwinding", wire.requests())
	}
	choices, _ := response.(map[string]any)["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "reconnected" {
		t.Fatalf("content = %#v", message["content"])
	}
	if elapsed >= 12*time.Second {
		t.Fatalf("stream took %.1fs to recover — the reader was not unwedged by the kill", elapsed.Seconds())
	}
}

func TestInterruptAbortShutsDownTheAttemptsOwnSocket(t *testing.T) {
	reader, writer := socketPair(t)
	c := killTestCall(killTestAgent(t))
	resp := &http.Response{Header: http.Header{}}
	c.attemptResponse, c.attemptConn = resp, func() net.Conn { return wrapperShape(reader) }
	c.inline = true
	c.abortForInterrupt(1.0)
	if !c.requestCancelled.Load() {
		t.Fatal("request not marked cancelled")
	}
	assertShutDown(t, writer)
}

// Waits until the monitor's interrupt abort reached the request client, the connect window /stop lands in
func awaitInterruptAbort(t *testing.T, agent *fakeChatAgent) {
	agent.mu.Lock()
	agent.interrupted = true
	agent.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		agent.mu.Lock()
		calls := append(append([]clientCall(nil), agent.aborts...), agent.aAborts...)
		agent.mu.Unlock()
		for _, call := range calls {
			if call.reason == "stream_interrupt_abort" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("monitor never ran the interrupt abort")
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("socket shut down") }

func (failingBody) Close() error { return nil }

// Chat client whose create returns headers only after the interrupt abort already swept
type lateChatClient struct {
	t      *testing.T
	agent  *fakeChatAgent
	reader net.Conn
}

func (l *lateChatClient) CreateChatCompletion(context.Context, map[string]any) (any, error) {
	awaitInterruptAbort(l.t, l.agent)
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: failingBody{}}
	return &auxSDKStream{resp: resp, lines: bufio.NewReader(resp.Body), idle: newAuxIdleTimer(auxPhaseLimit{}, func() {}), cancel: func() {}, conn: wrapperShape(l.reader)}, nil
}

// Anthropic client whose messages.stream opens only after the interrupt abort already swept
type lateAnthropicClient struct {
	t      *testing.T
	agent  *fakeChatAgent
	reader net.Conn
}

type lateMessageStream struct {
	resp *http.Response
	conn net.Conn
}

func (s *lateMessageStream) Next(context.Context) (any, error) {
	return nil, &AuxTransportError{Class: "ReadError", Message: "socket shut down"}
}

func (s *lateMessageStream) Close() error                 { return nil }
func (s *lateMessageStream) HTTPResponse() *http.Response { return s.resp }
func (s *lateMessageStream) AttemptConn() net.Conn        { return s.conn }
func (s *lateMessageStream) Snapshot() map[string]any     { return nil }

func (l *lateAnthropicClient) MessagesStream(context.Context, map[string]any) (anthMessageStream, error) {
	awaitInterruptAbort(l.t, l.agent)
	return &lateMessageStream{resp: &http.Response{StatusCode: 200, Header: http.Header{}}, conn: wrapperShape(l.reader)}, nil
}

func TestInterruptDuringConnectWindowShutsDownTheLateSocket(t *testing.T) {
	for _, mode := range []string{"chat_completions", "anthropic_messages"} {
		t.Run(mode, func(t *testing.T) {
			reader, writer := socketPair(t)
			agent := killTestAgent(t)
			agent.apiMode = mode
			agent.create = func(string, map[string]any) (any, error) {
				return &lateChatClient{t: t, agent: agent, reader: reader}, nil
			}
			agent.anthropicCreate = func(string) (any, error) {
				return &lateAnthropicClient{t: t, agent: agent, reader: reader}, nil
			}
			_, err := InterruptibleStreamingAPICall(context.Background(), agent, map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}, nil)
			var interrupted *InterruptedError
			if !errors.As(err, &interrupted) {
				t.Fatalf("err = %v, want InterruptedError", err)
			}
			assertShutDown(t, writer)
		})
	}
}

func TestProviderStreamErrorFromJSONDecodeErrorKeepsTextAndFlagsEmptyFrames(t *testing.T) {
	resp := &http.Response{Header: http.Header{"X-Request-Id": {"req-plain-text"}}}
	err := providerStreamErrorFromJSONDecodeError(&AuxTransportError{Class: "JSONDecodeError", Doc: "  request validation failed: unsupported reasoning_effort  "}, resp)
	pse := err.(*ProviderStreamError)
	eo := pse.Body["error"].(map[string]any)
	if pse.StatusCode != 0 || eo["code"] != ProviderStreamNonJSONErrorCode || eo["message"] != "request validation failed: unsupported reasoning_effort" ||
		pse.RawText != "request validation failed: unsupported reasoning_effort" || pse.Headers.Get("X-Request-Id") != "req-plain-text" {
		t.Fatalf("non-JSON frame = %#v", pse)
	}
	empty := providerStreamErrorFromJSONDecodeError(&AuxTransportError{Class: "JSONDecodeError", Doc: ""}, nil).(*ProviderStreamError)
	if !isProviderStreamEmptyFrameError(empty) || empty.RawText != "" {
		t.Fatalf("empty frame = %#v", empty)
	}
}

func TestSdkStreamTimeoutAppliesEachPhaseLimit(t *testing.T) {
	req, err := auxSDKBuildRequest(&auxOpenAIClient{}, map[string]any{"timeout": sdkStreamTimeout(30, 120, 1800, 30), "stream": true})
	if err != nil {
		t.Fatal(err)
	}
	want := auxSDKTimeouts{pool: auxPhaseLimit{30 * time.Second, true}, connect: auxPhaseLimit{30 * time.Second, true},
		write: auxPhaseLimit{1800 * time.Second, true}, read: auxPhaseLimit{120 * time.Second, true}}
	if req.timeout != want {
		t.Fatalf("timeouts = %#v", req.timeout)
	}
}
