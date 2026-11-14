package provider

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func inlineHarness(t *testing.T) (context.Context, *RetrySession, func() []StreamEvent) {
	t.Helper()
	s := &RetrySession{}
	var mu sync.Mutex
	var events []StreamEvent
	ctx := withStaleStreak(withNonStreamSurface(withInlineNonStream(t.Context(), true), func(ev StreamEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}), s)
	return ctx, s, func() []StreamEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]StreamEvent(nil), events...)
	}
}

func TestStalledInlineCallIsAbortedAndFeedsTheStaleBreaker(t *testing.T) {
	ctx, s, events := inlineHarness(t)
	logs := captureLogs(t)
	prevLog := nonStreamLog
	nonStreamLog = slog.Default()
	t.Cleanup(func() { nonStreamLog = prevLog })
	_, err := runNonStreamRequest(ctx, "m", 1200, 0.2, func(ctx context.Context) (*Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "Non-streaming API call timed out after 0s with no response (threshold: 0s)") {
		t.Fatalf("err %v", err)
	}
	if s.StaleStreak() != 1 || logCount(logs, "Inline non-streaming API call stale for") != 1 {
		t.Fatalf("streak %d", s.StaleStreak())
	}
	var status, killed bool
	for _, ev := range events() {
		status = status || (ev.Kind == EventStatus && strings.Contains(ev.Text, "No response from provider for 0s (non-streaming, model: m). Aborting call."))
		killed = killed || (ev.Kind == EventActivity && ev.Text == "stale non-streaming call killed after 0s")
	}
	if !status || !killed {
		t.Fatalf("events %+v", events())
	}
}

func TestHealthyInlineCallIsUntouchedAndResetsTheBreaker(t *testing.T) {
	ctx, s, events := inlineHarness(t)
	s.staleStreak.Store(2)
	var deadline time.Duration
	resp, err := runNonStreamRequest(ctx, "m", 0, 30, func(ctx context.Context) (*Response, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("the inline call carries no hard read timeout")
		}
		deadline = time.Until(d)
		return &Response{FinishReason: "stop"}, nil
	})
	if err != nil || resp.FinishReason != "stop" || s.StaleStreak() != 0 {
		t.Fatal(resp, err, s.StaleStreak())
	}
	if deadline <= 29*time.Second || deadline > 30*time.Second {
		t.Fatalf("hard timeout %v", deadline)
	}
	if ev := events(); len(ev) != 1 || ev[0].Kind != EventActivity || ev[0].Text != "waiting for non-streaming API response" {
		t.Fatalf("events %+v", ev)
	}
}

func TestInfiniteInlineBudgetLeavesTheWatchdogDisarmed(t *testing.T) {
	ctx, _, _ := inlineHarness(t)
	_, err := runNonStreamRequest(ctx, "m", 0, math.Inf(1), func(ctx context.Context) (*Response, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("an infinite budget injected a hard timeout")
		}
		return &Response{}, nil
	})
	if err != nil || inlineNonstreamHardTimeout(math.Inf(1)) != 0 || inlineNonstreamHardTimeout(0) != 0 {
		t.Fatal(err)
	}
}

func TestInlineInterruptIsNotMisclassifiedAsStaleness(t *testing.T) {
	ctx, s, _ := inlineHarness(t)
	ctx, cancel := context.WithCancel(ctx)
	_, err := runNonStreamRequest(ctx, "m", 0, 0.3, func(ctx context.Context) (*Response, error) {
		cancel()
		time.Sleep(500 * time.Millisecond)
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || s.StaleStreak() != 0 {
		t.Fatal(err, s.StaleStreak())
	}
}

func TestLateInlineStaleTimerAfterCompletionIsInert(t *testing.T) {
	ctx, s, _ := inlineHarness(t)
	r := &inlineRequest{abort: func() {}}
	r.markDone()
	if r.staleKill(ctx) || r.wasStale() || s.StaleStreak() != 0 {
		t.Fatal("a late timer acted after completion")
	}
}

func TestInlineCallKeepsActivityAliveDuringASlowWait(t *testing.T) {
	prev := directAPIActivityHeartbeat
	directAPIActivityHeartbeat = 20 * time.Millisecond
	t.Cleanup(func() { directAPIActivityHeartbeat = prev })
	ctx, _, events := inlineHarness(t)
	if _, err := runNonStreamRequest(ctx, "m", 0, 30, func(context.Context) (*Response, error) {
		time.Sleep(150 * time.Millisecond)
		return &Response{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	n := len(events())
	time.Sleep(80 * time.Millisecond)
	if n < 3 || len(events()) != n {
		t.Fatalf("heartbeats %d then %d", n, len(events()))
	}
}

func TestDoRunsTheBlockingCallInlineForDirectCallRoutes(t *testing.T) {
	prev := directAPICallFor
	directAPICallFor = func(context.Context, string, string, APIMode) bool { return true }
	t.Cleanup(func() { directAPICallFor = prev })
	route := testRoute()
	client := &inlineProbeClient{fakeClient: &fakeClient{route: route, queue: []fakeCall{{resp: &Response{FinishReason: "stop"}}}}}
	if _, err := Do(t.Context(), client, &Request{Model: route.Model}, DoOptions{Sleep: noSleep}); err != nil || !client.inline || !client.armed {
		t.Fatal(err, client.inline, client.armed)
	}
}

type inlineProbeClient struct {
	*fakeClient
	inline, armed bool
}

func (c *inlineProbeClient) Complete(ctx context.Context, req *Request) (*Response, error) {
	c.inline, c.armed = inlineNonStream(ctx), inlineHardTimeoutArmed(ctx)
	return c.fakeClient.Complete(ctx, req)
}

func TestInlineCallDoesNotOverrideAnExplicitTimeout(t *testing.T) {
	ctx := withInlineNonStream(t.Context(), false)
	if _, err := runNonStreamRequest(ctx, "m", 0, 30, func(ctx context.Context) (*Response, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("the stale budget replaced the request's own timeout")
		}
		return &Response{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	prev := directAPICallFor
	directAPICallFor = func(context.Context, string, string, APIMode) bool { return true }
	t.Cleanup(func() { directAPICallFor = prev })
	route := testRoute()
	client := &inlineProbeClient{fakeClient: &fakeClient{route: route, queue: []fakeCall{{resp: &Response{}}}}}
	if _, err := Do(t.Context(), client, &Request{Model: route.Model, Timeout: time.Minute}, DoOptions{Sleep: noSleep}); err != nil || !client.inline || client.armed {
		t.Fatal(err, client.inline, client.armed)
	}
}
