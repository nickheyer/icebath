package provider

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/tools"
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
	open   func(resp *http.Response) anthEventSource
	opened atomic.Int32
}

func (o *anthEventsOverride) Events(resp *http.Response) anthEventSource {
	o.opened.Add(1)
	return o.open(resp)
}

func anthTextEvents(texts ...string) []*anthEvent {
	evs := []*anthEvent{
		{Type: "message_start", Payload: map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-sonnet-4-20250514", "content": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 0}}}},
		{Type: "content_block_start", Payload: map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}},
	}
	for _, text := range texts {
		evs = append(evs, &anthEvent{Type: "content_block_delta", Payload: map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}})
	}
	return evs
}

// Source replaying events, then parking on release where neither the abort nor the socket can reach it
func wedgedAnthSource(release <-chan struct{}, evs ...*anthEvent) anthEventSource {
	i := 0
	return &scriptedAnthEvents{next: func() (*anthEvent, error) {
		if i < len(evs) {
			i++
			return evs[i-1], nil
		}
		select {
		case <-release:
		case <-time.After(30 * time.Second):
		}
		return &anthEvent{Type: "message_stop", Payload: map[string]any{"type": "message_stop"}}, nil
	}}
}

type streamCallOutcome struct {
	resp *Response
	err  error
}

type abandonHarness struct {
	t        *testing.T
	session  *RetrySession
	override *anthEventsOverride
	client   *anthropicClient
	loop     *doLoop
	mu       sync.Mutex
	events   []StreamEvent
}

// Anthropic client against a server that opens every stream, with a 0.3s stale window and give up five
func newAbandonHarness(t *testing.T, open func(resp *http.Response) anthEventSource) *abandonHarness {
	t.Helper()
	t.Setenv(home.EnvHome, t.TempDir())
	t.Setenv(StreamStaleTimeoutEnv, "0.3")
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "5")
	srv := newAnthServer(t, func(w http.ResponseWriter, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
	})
	c := srv.client(srv.route("https://example.invalid", "test-key"))
	o := &anthEventsOverride{anthTransport: c.transport, open: open}
	c.transport = o
	return &abandonHarness{t: t, session: &RetrySession{}, override: o, client: c}
}

// Runs the reconnect loop of one streaming call, the unit hermes drives through _interruptible_streaming_api_call
func (h *abandonHarness) start(ctx context.Context) <-chan streamCallOutcome {
	out := make(chan streamCallOutcome, 1)
	noSleep := func(context.Context, time.Duration) error { return nil }
	opts := DoOptions{Session: h.session, Log: slog.Default(), Now: time.Now, Sleep: noSleep, Stream: func(ev StreamEvent) error {
		h.mu.Lock()
		h.events = append(h.events, ev)
		h.mu.Unlock()
		return nil
	}}
	req := &Request{Messages: userRequest("hi").Messages}
	l := &doLoop{ctx: withStaleStreak(ctx, h.session), opts: opts, client: h.client, req: req, route: h.client.Route(), session: h.session,
		log: opts.Log, now: opts.Now, sleep: noSleep, maxRetries: 1}
	h.loop = l
	go func() {
		resp, _, err := l.streamWithReconnects()
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

func (h *abandonHarness) activity() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, ev := range h.events {
		if ev.Kind == EventActivity || ev.Kind == EventWaitNotice {
			out = append(out, string(ev.Kind)+":"+ev.Text)
		}
	}
	return out
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
	var abandoned *StreamAbandonedError
	var apiErr *APIError
	if !errors.As(err, &abandoned) || !errors.As(err, &apiErr) || apiErr.Kind != ErrTimeout || !strings.Contains(err.Error(), streamAbandonedText) {
		t.Fatalf("want a retryable abandon timeout, got %T %v", err, err)
	}
}

func TestWedgedWorkerIsAbandonedWithARetryableTimeout(t *testing.T) {
	logs := captureLogs(t)
	release := make(chan struct{})
	defer close(release)
	h := newAbandonHarness(t, func(*http.Response) anthEventSource { return wedgedAnthSource(release) })
	o := h.wait(h.start(context.Background()), 10*time.Second)
	assertAbandonTimeout(t, o.err)
	if n := logCount(logs, "Stream stale for"); n != 3 {
		t.Fatalf("stale kills logged %d, want three", n)
	}
	if n := logCount(logs, "abandoning the request"); n != 1 {
		t.Fatalf("abandon logged %d", n)
	}
	if h.session.StaleStreak() != 1 {
		t.Fatalf("one wedged attempt must still count once, streak %d", h.session.StaleStreak())
	}
}

func TestWorkerThatUnwindsAfterAKillStillRetries(t *testing.T) {
	logs := captureLogs(t)
	t.Setenv("ICEBATH_STREAM_RETRIES", "2")
	h := newAbandonHarness(t, func(resp *http.Response) anthEventSource {
		return &scriptedAnthEvents{next: func() (*anthEvent, error) {
			<-resp.Request.Context().Done()
			return nil, errors.New("peer closed connection without sending complete message body")
		}}
	})
	o := h.wait(h.start(context.Background()), 15*time.Second)
	if o.err == nil || strings.Contains(o.err.Error(), streamAbandonedText) {
		t.Fatalf("want the exhausted stale error, got %v", o.err)
	}
	if n := h.override.opened.Load(); n != 3 {
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
	h := newAbandonHarness(t, func(*http.Response) anthEventSource { return wedgedAnthSource(release) })
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
	h := newAbandonHarness(t, func(*http.Response) anthEventSource { return wedgedAnthSource(release, anthTextEvents("  \n")...) })
	o := h.wait(h.start(context.Background()), 10*time.Second)
	var partial *PartialStreamError
	if o.resp != nil || errors.As(o.err, &partial) {
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
	defer close(release)
	h := newAbandonHarness(t, func(*http.Response) anthEventSource { return wedgedAnthSource(release) })
	ctx, cancel := context.WithCancel(context.Background())
	out := h.start(ctx)
	select {
	case o := <-out:
		t.Fatalf("an inline call runs on the caller and is not abandoned: %v", o.err)
	case <-time.After(2500 * time.Millisecond):
	}
	kills := logCount(logs, "Stream stale for")
	touches := h.activity()
	cancel()
	o := h.wait(out, 5*time.Second)
	if !errors.Is(o.err, context.Canceled) {
		t.Fatalf("interrupt did not end the inline call: %v", o.err)
	}
	if kills <= 3 {
		t.Fatalf("kills %d", kills)
	}
	want := []string{"wait_notice:⚠ no output from provider for 0s — reconnecting...", "activity:stale stream detected after 0s, reconnecting"}
	if len(touches) != 4 || touches[0] != want[0] || touches[1] != want[1] || touches[2] != want[0] || touches[3] != want[1] {
		t.Fatalf("kills past the limit still reported activity: %q", touches)
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
	release := make(chan struct{})
	defer close(release)
	h := newAbandonHarness(t, func(*http.Response) anthEventSource {
		return wedgedAnthSource(release, anthTextEvents("Hello there")...)
	})
	o := h.wait(h.start(context.Background()), 10*time.Second)
	var partial *PartialStreamError
	if !errors.As(o.err, &partial) {
		t.Fatalf("want the partial delivery stub, got %T %v", o.err, o.err)
	}
	if apiErr := h.loop.asAPIError(partial.Err); apiErr.Kind != ErrTimeout || !strings.Contains(partial.Err.Error(), streamAbandonedText) {
		t.Fatalf("the stub is not classified from the abandon: %v %v", apiErr.Kind, partial.Err)
	}
}

func TestAbandonJoinsTheWorkerForRelayTeardownLikeStop(t *testing.T) {
	release := make(chan struct{})
	h := newAbandonHarness(t, func(*http.Response) anthEventSource { return wedgedAnthSource(release) })
	relayLLMInstallFake(t, "stream-abandon-profile")
	ctx := pyctx.With(context.Background(), pyctx.New())
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
		t.Fatalf("the abandon did not join the reader for Relay teardown, took %v", took)
	}
}

func TestWedgedWorkerUnderTheLifetimeCapIsAbandonedOnce(t *testing.T) {
	for _, tc := range []struct{ name, stale, cap string }{{"stale_off", "inf", "1"}, {"cap_at_stale", "0.3", "0.3"}} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			release := make(chan struct{})
			defer close(release)
			h := newAbandonHarness(t, func(*http.Response) anthEventSource { return wedgedAnthSource(release) })
			t.Setenv(StreamStaleTimeoutEnv, tc.stale)
			if err := os.WriteFile(home.ConfigPath(os.Getenv(home.EnvHome)), []byte("agent:\n  stream_max_lifetime: "+tc.cap+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			o := h.wait(h.start(context.Background()), 10*time.Second)
			capped := tc.stale == "inf"
			var cut *StreamLifetimeExceededError
			var apiErr *APIError
			if errors.As(o.err, &cut) != capped || !errors.As(o.err, &apiErr) || apiErr.Kind != ErrTimeout {
				t.Fatalf("outcome %T %v", o.err, o.err)
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
			if h.session.StaleStreak() != wantStreak {
				t.Fatalf("cap kills must not feed the stale breaker, streak %d", h.session.StaleStreak())
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
		t.Setenv(StreamStaleTimeoutEnv, "0.3")
		srv := silentStreamServer(t)
		c := newTestClient(t, Route{Provider: "custom", Model: "m", BaseURL: srv.URL + "/v1", APIKey: "k", Mode: ModeChatCompletions})
		_, err := c.Stream(context.Background(), userRequest("hi"), func(StreamEvent) error { return nil })
		assertKillNotInterrupt(t, err)
	})
	t.Run("anthropic_messages", func(t *testing.T) {
		h := newAbandonHarness(t, func(resp *http.Response) anthEventSource {
			return &scriptedAnthEvents{next: func() (*anthEvent, error) {
				<-resp.Request.Context().Done()
				return nil, errors.New("peer closed connection without sending complete message body")
			}}
		})
		_, err := h.client.Stream(context.Background(), &Request{Messages: userRequest("hi").Messages}, func(StreamEvent) error { return nil })
		assertKillNotInterrupt(t, err)
	})
	t.Run("gemini_native", func(t *testing.T) {
		t.Setenv(home.EnvHome, t.TempDir())
		t.Setenv(StreamStaleTimeoutEnv, "0.3")
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
