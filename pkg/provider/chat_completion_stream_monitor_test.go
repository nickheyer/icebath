package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	ctxutil "github.com/nickheyer/icebath/pkg/utils/ctx"
)

const streamAbandonedText = "request abandoned"

// Messages event source a test scripts, consulted once per Next call
type scriptedAnthEvents struct {
	next func() (*anthEvent, error)
}

func (e *scriptedAnthEvents) Next() (*anthEvent, error) { return e.next() }

// Anthropic transport whose stream events come from a per attempt factory instead of the response body
type anthEventsOverride struct {
	anthTransport
	open func(resp *http.Response) anthEventSource
}

func (o *anthEventsOverride) Events(resp *http.Response) anthEventSource { return o.open(resp) }

// Typed Anthropic client against a server that opens every stream, its events scripted per attempt, with a 0.3s stale window
func scriptedAnthropicClient(t *testing.T, open func(resp *http.Response) anthEventSource) *anthropicClient {
	t.Helper()
	srv := newAnthServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
	})
	setSettingsForTest(t, map[string]any{"agent.stream_stale_timeout": 0.3})
	c := srv.client(srv.route("https://example.invalid", "test-key"))
	c.transport = &anthEventsOverride{anthTransport: c.transport, open: open}
	return c
}

// Messages stream of the agent wire whose events a test scripts, consulted once per Next call
type scriptedMessageStream struct {
	next  func(ctx context.Context) (any, error)
	final map[string]any
}

func (s *scriptedMessageStream) Next(ctx context.Context) (any, error) { return s.next(ctx) }
func (s *scriptedMessageStream) Close() error                          { return nil }
func (s *scriptedMessageStream) HTTPResponse() *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}}
}
func (s *scriptedMessageStream) AttemptConn() net.Conn    { return nil }
func (s *scriptedMessageStream) Snapshot() map[string]any { return s.final }

// Anthropic client opening one scripted Messages stream per attempt and counting the opens
type scriptedMessagesClient struct {
	open   func() (anthMessageStream, error)
	opened atomic.Int32
}

func (c *scriptedMessagesClient) MessagesStream(context.Context, map[string]any) (anthMessageStream, error) {
	c.opened.Add(1)
	return c.open()
}

// Events of one text block carrying the given deltas
func messageTextEvents(texts ...string) []map[string]any {
	evs := []map[string]any{{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}}
	for _, text := range texts {
		evs = append(evs, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	}
	return evs
}

// Stream replaying events, then parking on release where neither the abort nor the socket can reach it
func wedgedMessageStream(release <-chan struct{}, events ...map[string]any) *scriptedMessageStream {
	i := 0
	return &scriptedMessageStream{next: func(context.Context) (any, error) {
		if i < len(events) {
			i++
			return events[i-1], nil
		}
		select {
		case <-release:
		case <-time.After(30 * time.Second):
		}
		return nil, io.EOF
	}}
}

// Streams whose first read waits for the abort of their attempt, or out the wait when the abort is ignored, then drops the connection
type droppingStreams struct {
	mu      sync.Mutex
	aborted chan struct{}
}

func (d *droppingStreams) open(wait time.Duration, ignoreAbort bool) *scriptedMessageStream {
	aborted := make(chan struct{})
	d.mu.Lock()
	d.aborted = aborted
	d.mu.Unlock()
	return &scriptedMessageStream{next: func(context.Context) (any, error) {
		if ignoreAbort {
			time.Sleep(wait)
		} else {
			select {
			case <-aborted:
			case <-time.After(wait):
			}
		}
		return nil, &AuxTransportError{Class: "ConnectError", Message: "connection dropped after close()"}
	}}
}

// Wakes the read of the stream opened last, the request client abort of the agent
func (d *droppingStreams) abort(any, string) {
	d.mu.Lock()
	if d.aborted != nil {
		close(d.aborted)
		d.aborted = nil
	}
	d.mu.Unlock()
}

// Swaps the stream reconnect backoff for a recorder of the requested waits
func recordStreamBackoff(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	prev := waitStreamRetryBackoffFn
	waitStreamRetryBackoffFn = func(_ ChatCompletionAgent, delay time.Duration) { waits = append(waits, delay) }
	t.Cleanup(func() { waitStreamRetryBackoffFn = prev })
	return &waits
}

type streamCallOutcome struct {
	resp any
	err  error
}

type abandonHarness struct {
	t      *testing.T
	agent  *fakeChatAgent
	client *scriptedMessagesClient
}

// Anthropic agent whose every stream attempt opens the scripted stream, with a 0.3s stale window and give up five
func newAbandonHarness(t *testing.T, open func() (anthMessageStream, error)) *abandonHarness {
	t.Helper()
	t.Setenv(home.EnvHome, t.TempDir())
	setSettingsForTest(t, map[string]any{"agent.stream_stale_timeout": 0.3})
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "5")
	client := &scriptedMessagesClient{open: open}
	agent := &fakeChatAgent{provider: "custom", apiMode: "anthropic_messages", baseURL: "https://example.com/v1", model: "claude-opus-4-7", plat: "cli",
		anthropicCreate: func(string) (any, error) { return client, nil }}
	return &abandonHarness{t: t, agent: agent, client: client}
}

// Runs one streaming call of the agent, the kwargs naming no model so no reasoning stale floor applies
func (h *abandonHarness) start(ctx context.Context) <-chan streamCallOutcome {
	out := make(chan streamCallOutcome, 1)
	go func() {
		resp, err := InterruptibleStreamingAPICall(ctx, h.agent, map[string]any{}, nil)
		out <- streamCallOutcome{resp: resp, err: err}
	}()
	return out
}

func (h *abandonHarness) wait(out <-chan streamCallOutcome, limit time.Duration) streamCallOutcome {
	h.t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(limit):
		h.t.Fatal("the call never ended: the same wedged attempt was re-killed forever")
	}
	return streamCallOutcome{}
}

func logCount(c *logCapture, needle string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, line := range c.lines {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

func assertAbandonTimeout(t *testing.T, err error) {
	t.Helper()
	if err == nil || TimeoutPhase(err) != TimeoutPhaseRead || !errors.Is(err, ErrStreamStalled) || !strings.Contains(err.Error(), streamAbandonedText) {
		t.Fatalf("want a retryable abandon timeout, got %T %v", err, err)
	}
}

func TestWedgedWorkerIsAbandonedWithARetryableTimeout(t *testing.T) {
	logs := captureLogs(t)
	release := make(chan struct{})
	defer close(release)
	h := newAbandonHarness(t, func() (anthMessageStream, error) { return wedgedMessageStream(release), nil })
	o := h.wait(h.start(context.Background()), 10*time.Second)
	assertAbandonTimeout(t, o.err)
	if n := logCount(logs, "Stream stale for"); n != 3 {
		t.Fatalf("stale kills logged %d, want three", n)
	}
	if n := logCount(logs, "abandoning the request"); n != 1 {
		t.Fatalf("abandon logged %d", n)
	}
	if h.agent.retry.StaleStreak() != 1 {
		t.Fatalf("one wedged attempt must still count once, streak %d", h.agent.retry.StaleStreak())
	}
}

func TestWorkerThatUnwindsAfterAKillStillRetries(t *testing.T) {
	logs := captureLogs(t)
	recordStreamBackoff(t)
	drops := &droppingStreams{}
	h := newAbandonHarness(t, func() (anthMessageStream, error) { return drops.open(10*time.Second, false), nil })
	t.Setenv("ICEBATH_STREAM_RETRIES", "2")
	h.agent.onAbort = drops.abort
	o := h.wait(h.start(context.Background()), 15*time.Second)
	if o.err == nil || strings.Contains(o.err.Error(), streamAbandonedText) {
		t.Fatalf("want the exhausted stale error, got %v", o.err)
	}
	if n := h.client.opened.Load(); n != 3 {
		t.Fatalf("each kill should lead to a fresh attempt (1 + 2 retries), saw %d", n)
	}
	if logCount(logs, "abandoning the request") != 0 {
		t.Fatal("an unwinding worker was abandoned")
	}
}

func TestAbandonLimitOfOneAbandonsOnTheFirstKill(t *testing.T) {
	logs := captureLogs(t)
	prev := staleAbandonKills
	staleAbandonKills = 1
	t.Cleanup(func() { staleAbandonKills = prev })
	release := make(chan struct{})
	defer close(release)
	h := newAbandonHarness(t, func() (anthMessageStream, error) { return wedgedMessageStream(release), nil })
	o := h.wait(h.start(context.Background()), 10*time.Second)
	assertAbandonTimeout(t, o.err)
	if logCount(logs, "Stream stale for") != 1 || logCount(logs, "abandoning the request") != 1 {
		t.Fatalf("a limit of one must not wait a second window: kills %d abandons %d", logCount(logs, "Stream stale for"), logCount(logs, "abandoning the request"))
	}
}

func TestAbandonAfterInvisibleDeltasRetriesInsteadOfAnEmptyStub(t *testing.T) {
	logs := captureLogs(t)
	release := make(chan struct{})
	defer close(release)
	h := newAbandonHarness(t, func() (anthMessageStream, error) {
		return wedgedMessageStream(release, messageTextEvents("  \n")...), nil
	})
	o := h.wait(h.start(context.Background()), 10*time.Second)
	if o.resp != nil {
		t.Fatalf("an empty partial stub was returned: %+v %v", o.resp, o.err)
	}
	assertAbandonTimeout(t, o.err)
	if logCount(logs, "before any visible text was delivered") != 1 {
		t.Fatal("the invisible delta reset was not logged")
	}
}

func TestWedgedInlineCallStopsReportingActivity(t *testing.T) {
	logs := captureLogs(t)
	prev := directAPICallFor
	directAPICallFor = func(context.Context, string, string, APIMode) bool { return true }
	t.Cleanup(func() { directAPICallFor = prev })
	release := make(chan struct{})
	var releaseOnce sync.Once
	unwedge := func() { releaseOnce.Do(func() { close(release) }) }
	defer unwedge()
	h := newAbandonHarness(t, func() (anthMessageStream, error) { return wedgedMessageStream(release), nil })
	t.Setenv("ICEBATH_STREAM_RETRIES", "0")
	out := h.start(context.Background())
	select {
	case o := <-out:
		t.Fatalf("an inline call runs on the caller and is not abandoned: %v", o.err)
	case <-time.After(2500 * time.Millisecond):
	}
	kills := logCount(logs, "Stream stale for")
	touches := h.agent.snapshotStrings(&h.agent.touches)
	notices := h.agent.snapshotStrings(&h.agent.diagWaits)
	unwedge()
	if o := h.wait(out, 5*time.Second); o.err == nil {
		t.Fatalf("the wedged attempt succeeded: %+v", o.resp)
	}
	if kills <= 3 {
		t.Fatalf("kills %d", kills)
	}
	wantNotice, wantTouch := "⚠ no output from provider for 0s — reconnecting...", "stale stream detected after 0s, reconnecting"
	if len(notices) != 2 || notices[0] != wantNotice || notices[1] != wantNotice || len(touches) != 2 || touches[0] != wantTouch || touches[1] != wantTouch {
		t.Fatalf("kills past the limit still reported activity: %q %q", notices, touches)
	}
	if logCount(logs, "no longer reporting it as active") != 1 || logCount(logs, "abandoning the request") != 0 {
		t.Fatalf("hold logged %d abandon logged %d", logCount(logs, "no longer reporting it as active"), logCount(logs, "abandoning the request"))
	}
}

func TestHeldInlineCallHeartbeatIsWithheldUntilARealChunk(t *testing.T) {
	var events []StreamEvent
	s := &ccStreamState{req: &Request{Model: "m"}, fn: func(ev StreamEvent) error { events = append(events, ev); return nil }}
	now := time.Unix(1000, 0)
	w := &streamWatchdog{lastChunk: now, withheldAt: now}
	m := newStreamWaitMonitor(s, "", 180, now)
	m.w = w
	if err := m.heartbeat(10); err != nil {
		t.Fatal(err)
	}
	if err := m.heartbeat(90); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("the heartbeat kept reporting a wedged inline call as active: %+v", events)
	}
	w.lastChunk = w.withheldAt.Add(time.Second)
	if err := m.heartbeat(10); err != nil || len(events) != 1 || events[0].Kind != EventActivity {
		t.Fatalf("a real chunk did not lift the hold: %+v %v", events, err)
	}
}

func TestAbandonAfterVisibleTextReturnsAStubClassifiedAsTheAbandon(t *testing.T) {
	logs := captureLogs(t)
	release := make(chan struct{})
	defer close(release)
	h := newAbandonHarness(t, func() (anthMessageStream, error) {
		return wedgedMessageStream(release, messageTextEvents("Hello there")...), nil
	})
	h.agent.st().streamedText = "Hello there"
	o := h.wait(h.start(context.Background()), 10*time.Second)
	cut, _ := o.resp.(map[string]any)
	if o.err != nil || cut == nil || cut["id"] != PartialStreamStubID || cut["stop_reason"] != "max_tokens" {
		t.Fatalf("want the partial delivery stub, got %+v %v", o.resp, o.err)
	}
	blocks, _ := cut["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "Hello there" {
		t.Fatalf("stub content %+v", cut["content"])
	}
	fromAbandon := false
	logs.mu.Lock()
	for _, line := range logs.lines {
		fromAbandon = fromAbandon || (strings.Contains(line, "returning length-truncated cut") && strings.Contains(line, streamAbandonedText))
	}
	logs.mu.Unlock()
	if !fromAbandon {
		t.Fatal("the stub is not classified from the abandon")
	}
}

func TestAbandonJoinsTheWorkerForRelayTeardownLikeStop(t *testing.T) {
	release := make(chan struct{})
	h := newAbandonHarness(t, func() (anthMessageStream, error) { return wedgedMessageStream(release), nil })
	relayLLMInstallFake(t, "stream-abandon-profile")
	setSettingsForTest(t, map[string]any{"agent.stream_stale_timeout": 0.3})
	ctx := ctxutil.WithStore(context.Background(), ctxutil.NewStore())
	host := tools.GetRelayRuntime(ctx, true, tools.CurrentRelayProfileKey(ctx))
	if host == nil {
		t.Fatal("no runtime")
	}
	if err := host.RetainManagedExecution("test.stream-abandon"); err != nil {
		t.Fatal(err)
	}
	defer host.ReleaseManagedExecution("test.stream-abandon")
	started := time.Now()
	time.AfterFunc(1500*time.Millisecond, func() { close(release) })
	o := h.wait(h.start(ctx), 10*time.Second)
	assertAbandonTimeout(t, o.err)
	if took := time.Since(started); took < 1500*time.Millisecond || took > 2800*time.Millisecond {
		t.Fatalf("the abandon did not join the worker for Relay teardown, took %v", took)
	}
}

func TestWedgedWorkerUnderTheLifetimeCapIsAbandonedOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stale, cap float64
	}{{"stale_off", 0, 1}, {"cap_at_stale", 0.3, 0.3}} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			release := make(chan struct{})
			defer close(release)
			h := newAbandonHarness(t, func() (anthMessageStream, error) { return wedgedMessageStream(release), nil })
			setSettingsForTest(t, map[string]any{"agent.stream_stale_timeout": tc.stale, "agent.stream_max_lifetime": tc.cap})
			o := h.wait(h.start(context.Background()), 10*time.Second)
			capped := tc.stale == 0
			var cut *StreamLifetimeExceededError
			if errors.As(o.err, &cut) != capped {
				t.Fatalf("outcome %T %v", o.err, o.err)
			}
			if !capped {
				assertAbandonTimeout(t, o.err)
			}
			wantCap, wantStale, wantStreak := 0, 3, 1
			if capped {
				wantCap, wantStale, wantStreak = 3, 0, 0
			}
			if logCount(logs, "Stream still running after") != wantCap || logCount(logs, "Stream stale for") != wantStale {
				t.Fatalf("cap kills %d stale kills %d", logCount(logs, "Stream still running after"), logCount(logs, "Stream stale for"))
			}
			if logCount(logs, "abandoning the request") != 1 {
				t.Fatal("the wedged attempt was not abandoned once")
			}
			if h.agent.retry.StaleStreak() != wantStreak {
				t.Fatalf("cap kills must not feed the stale breaker, streak %d", h.agent.retry.StaleStreak())
			}
		})
	}
}

// Stale kill outcome of one client against a server that opens the stream and then goes silent
func assertKillNotInterrupt(t *testing.T, err error) {
	t.Helper()
	var apiErr *APIError
	if err == nil || errors.Is(err, context.Canceled) || TimeoutPhase(err) != TimeoutPhaseRead || !errors.As(err, &apiErr) || apiErr.Kind != ErrTimeout || !strings.Contains(err.Error(), "Stream timed out: no chunks received") {
		t.Fatalf("a watchdog kill under a live request context must be the stale kill, got %T %v", err, err)
	}
}

func silentStreamServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWatchdogKillWithALiveRequestContextIsNeverAnInterrupt(t *testing.T) {
	t.Run("chat_completions", func(t *testing.T) {
		t.Setenv(home.EnvHome, t.TempDir())
		setSettingsForTest(t, map[string]any{"agent.stream_stale_timeout": 0.3})
		srv := silentStreamServer(t)
		c := newTestClient(t, Route{Provider: "custom", Model: "m", BaseURL: srv.URL + "/v1", APIKey: "k", Mode: ModeChatCompletions})
		_, err := c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
		assertKillNotInterrupt(t, err)
	})
	t.Run("anthropic_messages", func(t *testing.T) {
		c := scriptedAnthropicClient(t, func(resp *http.Response) anthEventSource {
			return &scriptedAnthEvents{next: func() (*anthEvent, error) {
				<-resp.Request.Context().Done()
				return nil, errors.New("peer closed connection without sending complete message body")
			}}
		})
		_, err := c.Stream(context.Background(), &Request{Messages: userRequest("hi").Messages}, func(StreamEvent) error { return nil })
		assertKillNotInterrupt(t, err)
	})
	t.Run("gemini_native", func(t *testing.T) {
		t.Setenv(home.EnvHome, t.TempDir())
		setSettingsForTest(t, map[string]any{"agent.stream_stale_timeout": 0.3})
		srv := silentStreamServer(t)
		c, err := NewClient(Route{Provider: "gemini", Model: "gemini-3.7-flash", BaseURL: srv.URL + "/v1beta", APIKey: "k", Mode: ModeGeminiNative})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
		assertKillNotInterrupt(t, err)
	})
}

func TestMonitorLoopPanicIsLoggedAndReraised(t *testing.T) {
	logs := captureLogs(t)
	w := &streamWatchdog{}
	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("panic not re-raised: %v", r)
		}
		if logCount(logs, "stream watchdog monitor loop aborted abnormally") != 1 {
			t.Fatal("abnormal exit not logged")
		}
	}()
	func() {
		defer w.logAbnormalExit()
		panic("boom")
	}()
}
