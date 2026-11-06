package provider

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

type fakeCall struct {
	resp   *Response
	err    error
	events []StreamEvent
}

type fakeClient struct {
	mu    sync.Mutex
	route Route
	calls int
	queue []fakeCall
	last  *Request
}

func (f *fakeClient) Route() Route { return f.route }

func (f *fakeClient) next() fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.queue) == 0 {
		return fakeCall{resp: &Response{Message: &state.Message{Role: "assistant", Content: "ok"}, FinishReason: "stop", Model: f.route.Model, Provider: f.route.Provider}}
	}
	c := f.queue[0]
	f.queue = f.queue[1:]
	return c
}

func (f *fakeClient) Complete(ctx context.Context, req *Request) (*Response, error) {
	f.last = req
	c := f.next()
	return c.resp, c.err
}

func (f *fakeClient) Stream(ctx context.Context, req *Request, fn StreamFunc) (*Response, error) {
	f.last = req
	c := f.next()
	for _, ev := range c.events {
		if err := fn(ev); err != nil {
			return nil, err
		}
	}
	return c.resp, c.err
}

func (f *fakeClient) CountTokens(ctx context.Context, req *Request) (int, error) { return 0, nil }

func asDoError(err error, target **DoError) bool { return errors.As(err, target) }

func noSleep(ctx context.Context, d time.Duration) error { return ctx.Err() }

type recordingSleep struct {
	waits []time.Duration
	now   time.Time
}

func newRecordingSleep() *recordingSleep { return &recordingSleep{now: time.Unix(1_700_000_000, 0)} }

func (r *recordingSleep) sleep(ctx context.Context, d time.Duration) error {
	r.waits = append(r.waits, d)
	r.now = r.now.Add(d)
	return ctx.Err()
}

func (r *recordingSleep) clock() time.Time { return r.now }

type fakeHooks struct {
	pre  []*PreRequestPayload
	post []*PostRequestPayload
	errs []*ErrorPayload
}

func (h *fakeHooks) Has(name string) bool                                 { return true }
func (h *fakeHooks) PreRequest(ctx context.Context, p *PreRequestPayload) { h.pre = append(h.pre, p) }
func (h *fakeHooks) PostRequest(ctx context.Context, p *PostRequestPayload) {
	h.post = append(h.post, p)
}
func (h *fakeHooks) Error(ctx context.Context, p *ErrorPayload) { h.errs = append(h.errs, p) }

func testRoute() Route {
	return Route{Provider: "openrouter", Model: "primary/model", BaseURL: "https://openrouter.ai/api/v1", APIKey: "test-key-1234567890", Mode: ModeChatCompletions}
}

func TestDoSucceedsFirstTryAndFiresHooks(t *testing.T) {
	client := &fakeClient{route: testRoute()}
	hooks := &fakeHooks{}
	resp, err := Do(t.Context(), client, &Request{Model: "primary/model", Messages: []*state.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}}}, DoOptions{Hooks: hooks, Sleep: noSleep})
	if err != nil || resp == nil || resp.Message.Text() != "ok" {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if len(hooks.pre) != 1 || hooks.pre[0].SystemPrompt != "sys" || hooks.pre[0].MessageCount != 2 {
		t.Fatalf("pre hook %+v", hooks.pre)
	}
	if len(hooks.post) != 1 || hooks.post[0].FinishReason != "stop" || hooks.post[0].AssistantContentChars != 2 {
		t.Fatalf("post hook %+v", hooks.post)
	}
	if len(hooks.errs) != 0 {
		t.Fatal("no error hook expected")
	}
}

func TestDoRetriesServerErrorThenSucceeds(t *testing.T) {
	client := &fakeClient{route: testRoute(), queue: []fakeCall{{err: &APIError{StatusCode: 500, Message: "upstream down", Kind: ErrServer}}}}
	sleeper := newRecordingSleep()
	var buffered []string
	hooks := &fakeHooks{}
	resp, err := Do(t.Context(), client, &Request{Model: "primary/model"}, DoOptions{Sleep: sleeper.sleep, Now: sleeper.clock, BufferStatus: func(s string) { buffered = append(buffered, s) }, Hooks: hooks})
	if err != nil || resp == nil {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if client.calls != 2 {
		t.Fatalf("calls %d", client.calls)
	}
	if len(sleeper.waits) != 1 || sleeper.waits[0] < 2*time.Second || sleeper.waits[0] > 3*time.Second {
		t.Fatalf("backoff waits %v", sleeper.waits)
	}
	if len(hooks.errs) != 1 || hooks.errs[0].StatusCode != 500 || hooks.errs[0].Reason != ReasonServerError || !hooks.errs[0].Retryable || hooks.errs[0].ErrorType != "APIError" {
		t.Fatalf("error hook %+v", *hooks.errs[0])
	}
	joined := strings.Join(buffered, "\n")
	if !strings.Contains(joined, "Attempt 1/3 failed") || !strings.Contains(joined, "Retrying in") {
		t.Fatalf("buffered %q", buffered)
	}
}

func TestDoHonoursRetryAfterOnRateLimit(t *testing.T) {
	client := &fakeClient{route: testRoute(), queue: []fakeCall{{err: &APIError{StatusCode: 429, Message: "rate limited", Kind: ErrRateLimit, RetryAfter: 2 * time.Second}}}}
	sleeper := newRecordingSleep()
	var buffered []string
	if _, err := Do(t.Context(), client, &Request{Model: "primary/model"}, DoOptions{Sleep: sleeper.sleep, Now: sleeper.clock, BufferStatus: func(s string) { buffered = append(buffered, s) }}); err != nil {
		t.Fatal(err)
	}
	if len(sleeper.waits) != 1 || sleeper.waits[0] != 2*time.Second {
		t.Fatalf("waits %v", sleeper.waits)
	}
	if !strings.Contains(strings.Join(buffered, "\n"), "⏱️ Rate limited. Waiting 2.0s (attempt 2/3)") {
		t.Fatalf("buffered %q", buffered)
	}
}

func TestDoNonRetryableAuthErrorIsTerminal(t *testing.T) {
	client := &fakeClient{route: testRoute(), queue: []fakeCall{{err: &APIError{StatusCode: 401, Message: "Invalid API key", Kind: ErrAuth, Body: `{"error":{"message":"Invalid API key"}}`}}}}
	var status, guidance []string
	_, err := Do(t.Context(), client, &Request{Model: "primary/model"}, DoOptions{Sleep: noSleep, Status: func(s string) { status = append(status, s) }, Verbose: func(l ...string) { guidance = append(guidance, l...) }})
	var de *DoError
	if !asDoError(err, &de) || de.Failure != FailNonRetryable || de.Reason != ReasonAuth || de.Retryable {
		t.Fatalf("err %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("calls %d", client.calls)
	}
	if de.Summary != "HTTP 401: Invalid API key" {
		t.Fatalf("summary %q", de.Summary)
	}
	if !strings.Contains(strings.Join(status, "\n"), "❌ OpenRouter rejected the request and retrying won't help: HTTP 401: Invalid API key") {
		t.Fatalf("status %q", status)
	}
	joined := strings.Join(guidance, "\n")
	if !strings.Contains(joined, "Your API key was rejected") || !strings.Contains(joined, "https://openrouter.ai/settings/credits") {
		t.Fatalf("guidance %q", guidance)
	}
}

func TestDoExhaustsRetries(t *testing.T) {
	serverErr := &APIError{StatusCode: 500, Message: "boom", Kind: ErrServer}
	client := &fakeClient{route: testRoute(), queue: []fakeCall{{err: serverErr}, {err: serverErr}, {err: serverErr}}}
	var status []string
	_, err := Do(t.Context(), client, &Request{Model: "primary/model"}, DoOptions{Sleep: noSleep, MaxRetries: 2, Status: func(s string) { status = append(status, s) }})
	var de *DoError
	if !asDoError(err, &de) || de.Failure != FailExhausted || de.Attempts != 2 {
		t.Fatalf("err %v", err)
	}
	if client.calls != 2 {
		t.Fatalf("calls %d", client.calls)
	}
	if !strings.Contains(strings.Join(status, "\n"), "❌ API failed after 2 retries — HTTP 500: boom") {
		t.Fatalf("status %q", status)
	}
}

func TestDoAutoRecoveryLadderReentersLoop(t *testing.T) {
	serverErr := &APIError{StatusCode: 503, Message: "overloaded", Kind: ErrServer}
	client := &fakeClient{route: testRoute(), queue: []fakeCall{{err: serverErr}, {err: serverErr}}}
	sleeper := newRecordingSleep()
	var status []string
	resp, err := Do(t.Context(), client, &Request{Model: "primary/model"}, DoOptions{Sleep: sleeper.sleep, Now: sleeper.clock, MaxRetries: 2, AutoRecoveryCycles: 1, Platform: "cli", Status: func(s string) { status = append(status, s) }})
	if err != nil || resp == nil {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if client.calls != 3 {
		t.Fatalf("calls %d", client.calls)
	}
	if !strings.Contains(strings.Join(status, "\n"), "retrying automatically in") || !strings.Contains(strings.Join(status, "\n"), "press Esc to stop") {
		t.Fatalf("status %q", status)
	}
}

func TestDoEagerFallbackOnRateLimit(t *testing.T) {
	primary := testRoute()
	fallback := Route{Provider: "openai", Model: "gpt-4o", BaseURL: "https://api.openai.com/v1", APIKey: "fb-key", Mode: ModeChatCompletions}
	primaryClient := &fakeClient{route: primary, queue: []fakeCall{{err: &APIError{StatusCode: 429, Message: "rate limited", Kind: ErrRateLimit}}}}
	fallbackClient := &fakeClient{route: fallback}
	chain := NewChain(ChainOptions{Primary: primary, Fallbacks: []Route{fallback}})
	var buffered []string
	resp, err := Do(t.Context(), primaryClient, &Request{Model: primary.Model}, DoOptions{Chain: chain, Sleep: noSleep,
		ClientFor:    func(ctx context.Context, r Route) (Client, error) { return fallbackClient, nil },
		BufferStatus: func(s string) { buffered = append(buffered, s) }})
	if err != nil || resp == nil || resp.Provider != "openai" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if primaryClient.calls != 1 || fallbackClient.calls != 1 {
		t.Fatalf("calls primary %d fallback %d", primaryClient.calls, fallbackClient.calls)
	}
	if !chain.Activated() || chain.Current().Provider != "openai" {
		t.Fatalf("chain not switched: %+v", chain.Current())
	}
	wantModel := NormalizeModelForProvider("gpt-4o", "openai")
	if fallbackClient.last.Model != wantModel {
		t.Fatalf("request not rebuilt for the fallback model: %q", fallbackClient.last.Model)
	}
	joined := strings.Join(buffered, "\n")
	if !strings.Contains(joined, "⚠️ Rate limited — switching to fallback provider...") || !strings.Contains(joined, "⚠️ Model fallback: primary/model via openrouter unavailable (rate limit); using "+wantModel+" via openai.") {
		t.Fatalf("buffered %q", buffered)
	}
	if !strings.Contains(joined, "Primary retry eligible in ~60 s") {
		t.Fatalf("cooldown notice missing: %q", buffered)
	}
}

func TestDoRotatesCredentialBeforeFallback(t *testing.T) {
	primary := testRoute()
	rotated := primary
	rotated.APIKey = "second-key"
	first := &fakeClient{route: primary, queue: []fakeCall{{err: &APIError{StatusCode: 429, Message: "rate limited", Kind: ErrRateLimit}}}}
	second := &fakeClient{route: rotated}
	rotator := &fakeRotator{next: rotated}
	resp, err := Do(t.Context(), first, &Request{Model: primary.Model}, DoOptions{Rotator: rotator, Sleep: noSleep,
		ClientFor: func(ctx context.Context, r Route) (Client, error) { return second, nil }})
	if err != nil || resp == nil {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if rotator.calls != 1 || second.calls != 1 {
		t.Fatalf("rotator %d second %d", rotator.calls, second.calls)
	}
}

type fakeRotator struct {
	next  Route
	calls int
	ok    bool
}

func (r *fakeRotator) Rotate(ctx context.Context, route Route, apiErr *APIError) (Route, bool) {
	r.calls++
	if r.calls == 1 {
		return r.next, true
	}
	return route, false
}

func TestDoInterruptedByContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeClient{route: testRoute(), queue: []fakeCall{{err: func() error { cancel(); return context.Canceled }()}}}
	_, err := Do(ctx, client, &Request{Model: "primary/model"}, DoOptions{Sleep: noSleep})
	var de *DoError
	if !asDoError(err, &de) || de.Failure != FailInterrupted {
		t.Fatalf("err %v", err)
	}
}

func TestDoDeadlineExpires(t *testing.T) {
	client := &fakeClient{route: testRoute(), queue: []fakeCall{{err: context.DeadlineExceeded}}}
	_, err := Do(context.Background(), client, &Request{Model: "primary/model"}, DoOptions{Sleep: noSleep, Deadline: time.Nanosecond})
	var de *DoError
	if !asDoError(err, &de) || de.Failure != FailDeadline {
		t.Fatalf("err %v", err)
	}
	var expired *DeadlineExpiredError
	if !errors.As(err, &expired) || expired.Label != "api_call" {
		t.Fatalf("expected the deadline cause, got %v", err)
	}
}

func TestDoStreamReconnectsAfterTransientDrop(t *testing.T) {
	route := testRoute()
	client := &fakeClient{route: route, queue: []fakeCall{
		{err: &APIError{Message: "connection reset by peer", Kind: ErrNetwork}},
		{events: []StreamEvent{{Kind: EventTextDelta, Text: "hel"}, {Kind: EventTextDelta, Text: "lo"}}, resp: &Response{Message: &state.Message{Role: "assistant", Content: "hello"}, FinishReason: "stop"}},
	}}
	sleeper := newRecordingSleep()
	var text strings.Builder
	var buffered []string
	resp, err := Do(t.Context(), client, &Request{Model: route.Model}, DoOptions{Sleep: sleeper.sleep, Now: sleeper.clock, BufferStatus: func(s string) { buffered = append(buffered, s) },
		Stream: func(ev StreamEvent) error {
			if ev.Kind == EventTextDelta {
				text.WriteString(ev.Text)
			}
			return nil
		}})
	if err != nil || resp == nil || text.String() != "hello" {
		t.Fatalf("resp %v err %v text %q", resp, err, text.String())
	}
	if client.calls != 2 || len(sleeper.waits) != 1 || sleeper.waits[0] != time.Second {
		t.Fatalf("calls %d waits %v", client.calls, sleeper.waits)
	}
	if !strings.Contains(strings.Join(buffered, "\n"), "openrouter stream drop (APIError) — attempt 1/3 dropped, reconnecting") {
		t.Fatalf("buffered %q", buffered)
	}
}

func TestDoStreamFailureAfterDeliveryIsPartial(t *testing.T) {
	route := testRoute()
	client := &fakeClient{route: route, queue: []fakeCall{{events: []StreamEvent{{Kind: EventTextDelta, Text: "partial"}}, err: &APIError{Message: "connection lost", Kind: ErrNetwork}}}}
	_, err := Do(t.Context(), client, &Request{Model: route.Model}, DoOptions{Sleep: noSleep, Stream: func(StreamEvent) error { return nil }})
	var de *DoError
	if !asDoError(err, &de) || de.Failure != FailPartialStream || !de.DeltasSent {
		t.Fatalf("err %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("calls %d", client.calls)
	}
}

func TestDoStreamOptionsRejectionRetriesWithoutUsageExtension(t *testing.T) {
	route := testRoute()
	client := &fakeClient{route: route, queue: []fakeCall{{err: &APIError{StatusCode: 400, Message: "extra_forbidden: stream_options", Kind: ErrInvalidRequest, Body: `{"error":{"message":"extra fields not permitted: stream_options"}}`}}}}
	session := &RetrySession{}
	resp, err := Do(t.Context(), client, &Request{Model: route.Model}, DoOptions{Sleep: noSleep, Session: session, Stream: func(StreamEvent) error { return nil }})
	if err != nil || resp == nil {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if !session.StreamOptionsUnsupported || client.last.Metadata[StreamOptionsOffKey] != "off" {
		t.Fatalf("stream options latch not set: %+v %v", session, client.last.Metadata)
	}
}

func TestDoUnmasksStreaming5xxWithNonStreamingProbe(t *testing.T) {
	route := testRoute()
	client := &fakeClient{route: route, queue: []fakeCall{
		{err: &APIError{StatusCode: 500, Message: "something went wrong", Kind: ErrServer}},
		{err: &APIError{StatusCode: 400, Message: "tools[0].function.name is required", Kind: ErrInvalidRequest}},
	}}
	_, err := Do(t.Context(), client, &Request{Model: route.Model}, DoOptions{Sleep: noSleep, Stream: func(StreamEvent) error { return nil }})
	var de *DoError
	if !asDoError(err, &de) || de.APIError == nil || de.APIError.StatusCode != 400 {
		t.Fatalf("expected the probe's 400 to replace the 5xx, got %v", err)
	}
}

func TestDoFastModeUnprovisionedDropsSpeed(t *testing.T) {
	route := Route{Provider: "anthropic", Model: "claude-opus-5", BaseURL: "https://api.anthropic.com", APIKey: "k", Mode: ModeAnthropicMessages}
	h := http.Header{}
	h.Set("anthropic-fast-input-tokens-limit", "0")
	h.Set("anthropic-fast-output-tokens-limit", "0")
	fastErr := &APIError{StatusCode: 429, Message: "rate limit of 0 fast mode input tokens", Kind: ErrRateLimit, Err: &headerErr{msg: "fast", h: h}}
	client := &fakeClient{route: route, queue: []fakeCall{{err: fastErr}}}
	fast := NewFastWindow("priority", map[string]any{"speed": "fast"})
	var lines []string
	resp, err := Do(t.Context(), client, &Request{Model: route.Model, ExtraBody: map[string]any{"speed": "fast"}}, DoOptions{Sleep: noSleep, Fast: fast, Verbose: func(l ...string) { lines = append(lines, l...) }})
	if err != nil || resp == nil {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if !fast.FastModeUnavailable("claude-opus-5") || !strings.Contains(strings.Join(lines, "\n"), "standard speed") {
		t.Fatalf("fast mode not marked: %v %q", fast.FastModeUnavailable("claude-opus-5"), lines)
	}
	if _, has := client.last.ExtraBody["speed"]; has {
		t.Fatal("speed should be dropped from the retried request")
	}
}

func TestFastModeUnprovisionedDetectsOnlyZeroLimitFast429s(t *testing.T) {
	limit := func(v string) http.Header {
		h := http.Header{}
		h.Set("anthropic-fast-input-tokens-limit", v)
		h.Set("anthropic-fast-output-tokens-limit", v)
		return h
	}
	fastKwargs := map[string]any{"speed": "fast"}
	if !FastModeUnprovisioned(&APIError{StatusCode: 429}, fastKwargs, limit("0")) {
		t.Fatal("zero limit fast 429 should be unprovisioned")
	}
	if FastModeUnprovisioned(&APIError{StatusCode: 429}, fastKwargs, limit("2000000")) {
		t.Fatal("real limit is transient")
	}
	if FastModeUnprovisioned(&APIError{StatusCode: 429}, map[string]any{}, limit("0")) {
		t.Fatal("request must have asked for fast speed")
	}
	if FastModeUnprovisioned(&APIError{StatusCode: 500}, fastKwargs, limit("0")) {
		t.Fatal("non 429 never matches")
	}
}

func TestFastWindowPolicy(t *testing.T) {
	clock := time.Unix(1000, 0)
	fw := NewFastWindow("auto", map[string]any{"extra_body": map[string]any{"keep": 1}})
	fw.now = func() time.Time { return clock }
	fw.BeginTurn(nil)
	if !fw.Open() {
		t.Fatal("auto window should open on a user turn")
	}
	clock = clock.Add(61 * time.Second)
	if fw.Open() {
		t.Fatal("window should expire")
	}
	fw.BeginTurn([]*state.Message{{Role: "user", Content: "x"}})
	if !fw.Open() {
		t.Fatal("auto re-opens on the next turn")
	}
	cold := NewFastWindow("cold", nil)
	cold.now = func() time.Time { return clock }
	cold.BeginTurn([]*state.Message{{Role: "user", Content: "x"}})
	if cold.Open() {
		t.Fatal("cold with prior history must not open")
	}
	cold.BeginTurn(nil)
	if !cold.Open() {
		t.Fatal("cold first turn opens")
	}
	static := NewFastWindow("priority", map[string]any{"speed": "fast"})
	static.BeginTurn(nil)
	if static.Open() {
		t.Fatal("static tiers have no bounded window")
	}
	route := Route{Provider: "anthropic", Model: "claude-opus-5", Mode: ModeAnthropicMessages}
	if got := static.EffectiveRequestOverrides(route, ""); got["speed"] != "fast" {
		t.Fatalf("static overrides %v", got)
	}
	if !static.MarkFastModeUnavailable("claude-opus-5") || static.MarkFastModeUnavailable("claude-opus-5") {
		t.Fatal("mark must succeed once per model")
	}
	if got := static.EffectiveRequestOverrides(route, ""); len(got) != 0 {
		t.Fatalf("speed should be dropped for the unavailable model: %v", got)
	}
	route.Model = "claude-opus-5-5"
	if got := static.EffectiveRequestOverrides(route, ""); got["speed"] != "fast" {
		t.Fatalf("other model keeps speed: %v", got)
	}
	if static.RequestOverrides["speed"] != "fast" {
		t.Fatal("request overrides must never be mutated")
	}
}

func TestServiceTierParsing(t *testing.T) {
	cases := map[string]string{"auto": "auto", "COLD": "cold", "fast": "priority", "": "", "bogus": "", "ultra": "ultrafast", "on": "priority", "normal": ""}
	for raw, want := range cases {
		if got := ParseServiceTier(raw); got != want {
			t.Fatalf("ParseServiceTier(%q) = %q want %q", raw, got, want)
		}
	}
	if _, err := ParseExactServiceTier("bogus"); err == nil {
		t.Fatal("unknown word must error")
	}
	if tier, err := ParseExactServiceTier(""); err != nil || tier != "" {
		t.Fatalf("empty pins normal: %q %v", tier, err)
	}
	if ServiceTierWord("priority") != "fast" || ServiceTierWord("") != "normal" || ServiceTierWord("auto") != "auto" {
		t.Fatal("service tier words")
	}
}

func TestClampTimeout(t *testing.T) {
	if _, ok := ClampTimeout(0); ok {
		t.Fatal("zero is unbounded")
	}
	if _, ok := ClampTimeout(-5); ok {
		t.Fatal("negative is unbounded")
	}
	if d, ok := ClampTimeout(420); !ok || d != 420*time.Second {
		t.Fatalf("normal value %v %v", d, ok)
	}
	if d, ok := ClampTimeout(1e18); !ok || d != MaxSafeTimeout {
		t.Fatalf("oversized value %v %v", d, ok)
	}
	if _, ok := ClampTimeout(math.NaN()); ok {
		t.Fatal("NaN is unbounded")
	}
	if _, ok := ClampTimeoutValue("not-a-number"); ok {
		t.Fatal("junk is unbounded")
	}
	if _, ok := ClampTimeoutValue(nil); ok {
		t.Fatal("nil is unbounded")
	}
	if d, ok := ClampTimeoutValue("12"); !ok || d != 12*time.Second {
		t.Fatalf("numeric string %v %v", d, ok)
	}
}

type fakeClock struct {
	now   time.Time
	slept []time.Duration
}

func useFakeClock(t *testing.T) *fakeClock {
	t.Helper()
	clock := &fakeClock{now: time.Unix(0, 0)}
	oldNow, oldSleep := deadlineNow, deadlineSleep
	deadlineNow = func() time.Time { return clock.now }
	deadlineSleep = func(ctx context.Context, d time.Duration) error {
		clock.slept = append(clock.slept, d)
		clock.now = clock.now.Add(d)
		return nil
	}
	t.Cleanup(func() { deadlineNow, deadlineSleep = oldNow, oldSleep })
	return clock
}

func TestPollUntil(t *testing.T) {
	clock := useFakeClock(t)
	if got := PollUntil(func() (int, bool) { return 4242, true }, 10*time.Second, 500*time.Millisecond); got != 4242 || len(clock.slept) != 0 {
		t.Fatalf("truthy result %v slept %v", got, clock.slept)
	}
	clock.slept = nil
	if got := PollUntil(func() (bool, bool) { return false, false }, 2*time.Second, 500*time.Millisecond); got {
		t.Fatal("falsy at deadline")
	}
	total := time.Duration(0)
	for _, s := range clock.slept {
		total += s
	}
	if total != 2*time.Second {
		t.Fatalf("slept %v", clock.slept)
	}
	clock.now, clock.slept = time.Unix(0, 0), nil
	got := PollUntil(func() (bool, bool) { ok := clock.now.Sub(time.Unix(0, 0)) >= 5*time.Second; return ok, ok }, 5*time.Second, 3*time.Second)
	if !got || len(clock.slept) != 2 || clock.slept[0] != 3*time.Second || clock.slept[1] != 2*time.Second {
		t.Fatalf("deadline condition %v slept %v", got, clock.slept)
	}
	clock.slept = nil
	calls := 0
	PollUntil(func() (any, bool) { calls++; return nil, false }, 0, time.Second)
	if calls != 1 || len(clock.slept) != 0 {
		t.Fatalf("zero timeout calls %d slept %v", calls, clock.slept)
	}
}

func TestResolveTimeout(t *testing.T) {
	const env = "ICEBATH_TEST_DEADLINE_X"
	os.Unsetenv(env)
	if d, ok := ResolveTimeout(config.Tree{}, "a.b", 42*time.Second, true, env); !ok || d != 42*time.Second {
		t.Fatalf("default %v %v", d, ok)
	}
	t.Setenv(env, "17.5")
	if d, ok := ResolveTimeout(config.Tree{}, "a.b", 42*time.Second, true, env); !ok || d != 17500*time.Millisecond {
		t.Fatalf("env %v %v", d, ok)
	}
	cfg := config.Tree{"timeouts": map[string]any{"a": map[string]any{"b": 99}}}
	if d, ok := ResolveTimeout(cfg, "a.b", 42*time.Second, true, env); !ok || d != 99*time.Second {
		t.Fatalf("config %v %v", d, ok)
	}
	cfg = config.Tree{"timeouts": map[string]any{"tools": map[string]any{"concurrent_batch": 300}}}
	if d, ok := ResolveTimeout(cfg, "tools.concurrent_batch", 420*time.Second, true, ""); !ok || d != 300*time.Second {
		t.Fatalf("dotted %v %v", d, ok)
	}
	cfg = config.Tree{"timeouts": map[string]any{"a": map[string]any{"b": 0}}}
	if _, ok := ResolveTimeout(cfg, "a.b", 42*time.Second, true, ""); ok {
		t.Fatal("zero config means unbounded")
	}
	cfg = config.Tree{"timeouts": map[string]any{"a": map[string]any{"b": "soon"}}}
	if d, ok := ResolveTimeout(cfg, "a.b", 42*time.Second, true, env); !ok || d != 17500*time.Millisecond {
		t.Fatalf("invalid config falls through to env %v %v", d, ok)
	}
	t.Setenv(env, "banana")
	if d, ok := ResolveTimeout(config.Tree{}, "a.b", 42*time.Second, true, env); !ok || d != 42*time.Second {
		t.Fatalf("invalid env falls through %v %v", d, ok)
	}
	os.Unsetenv(env)
	cfg = config.Tree{"timeouts": map[string]any{"a": map[string]any{"b": true}}}
	if d, ok := ResolveTimeout(cfg, "a.b", 42*time.Second, true, ""); !ok || d != 42*time.Second {
		t.Fatalf("bool rejected %v %v", d, ok)
	}
	cfg = config.Tree{"timeouts": map[string]any{"a": map[string]any{"b": math.NaN()}}}
	if d, ok := ResolveTimeout(cfg, "a.b", 42*time.Second, true, ""); !ok || d != 42*time.Second {
		t.Fatalf("NaN falls through %v %v", d, ok)
	}
	if d, ok := ResolveTimeout(nil, "a.b", 5*time.Second, true, ""); !ok || d != 5*time.Second {
		t.Fatalf("nil config uses default %v %v", d, ok)
	}
}

func TestRunBounded(t *testing.T) {
	res, err := RunBounded(context.Background(), func(ctx context.Context) (any, error) { return 7, nil }, 0, false, "op", nil, nil)
	if err != nil || res.Value != 7 || res.TimedOut || res.Bounded {
		t.Fatalf("unbounded %+v %v", res, err)
	}
	boom := errors.New("boom")
	if _, err := RunBounded(context.Background(), func(ctx context.Context) (any, error) { return nil, boom }, time.Second, true, "op", nil, nil); !errors.Is(err, boom) {
		t.Fatalf("operation error must propagate: %v", err)
	}
	marked := &suspectBackend{}
	onTimeout := 0
	release := make(chan struct{})
	defer close(release)
	res, err = RunBounded(context.Background(), func(ctx context.Context) (any, error) { <-release; return nil, nil }, 20*time.Millisecond, true, "slow", func() { onTimeout++ }, marked)
	if err != nil || !res.TimedOut || onTimeout != 1 || marked.reasons != 1 {
		t.Fatalf("timeout %+v err %v onTimeout %d marked %d", res, err, onTimeout, marked.reasons)
	}
	if !strings.Contains(marked.last, "slow timed out after 0.0s") {
		t.Fatalf("mark reason %q", marked.last)
	}
	res, err = RunBounded(context.Background(), func(ctx context.Context) (any, error) { return "fast", nil }, time.Second, true, "quick", nil, marked)
	if err != nil || res.TimedOut || res.Value != "fast" || marked.reasons != 1 {
		t.Fatalf("completed op must never mark: %+v %v %d", res, err, marked.reasons)
	}
}

type suspectBackend struct {
	reasons int
	last    string
}

func (s *suspectBackend) MarkSuspect(reason string) { s.reasons++; s.last = reason }
func (s *suspectBackend) EnsureHealthy() bool       { return true }

func TestIsLocalValidationError(t *testing.T) {
	if !IsLocalValidationError(&LocalValidationError{Message: "bad request shape"}) {
		t.Fatal("local validation error")
	}
	if IsLocalValidationError(&APIError{StatusCode: 400, Kind: ErrInvalidRequest}) {
		t.Fatal("provider 400 is not local")
	}
	if IsLocalValidationError(&TimeoutPhaseError{Phase: TimeoutPhaseRead}) {
		t.Fatal("timeouts are not local")
	}
	if IsLocalValidationError(errors.New("expected value at line 1 column 2")) {
		t.Fatal("stream parse errors are not local")
	}
	if !IsLocalValidationError(errors.New("list content fed into a str regex helper")) {
		t.Fatal("plain local error")
	}
}

func TestFormatDurationCompactAndResetHint(t *testing.T) {
	if formatDurationCompact(45) != "45s" || formatDurationCompact(780) != "13m" || formatDurationCompact(3600*2+60*5) != "2h 5m" || formatDurationCompact(3600*3) != "3h" || formatDurationCompact(86400*1.5) != "1.5d" {
		t.Fatal("duration compact")
	}
	now := time.Now()
	if hint := ResetHint(&APIError{Message: "resets in 13 minutes"}, now); hint != "~13m" {
		t.Fatalf("reset hint %q", hint)
	}
	if hint := ResetHint(&APIError{Message: "plain"}, now); hint != "" {
		t.Fatalf("no reset hint expected, got %q", hint)
	}
}
