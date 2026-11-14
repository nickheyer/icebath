package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type nsRecorder struct {
	mu       sync.Mutex
	notices  []string
	touches  []string
	statuses []string
}

func (r *nsRecorder) sink(ev StreamEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Kind {
	case EventWaitNotice:
		r.notices = append(r.notices, ev.Text)
	case EventActivity:
		r.touches = append(r.touches, ev.Text)
	case EventStatus:
		r.statuses = append(r.statuses, ev.Text)
	}
}

func nsWaiter(rec *nsRecorder) *nonStreamWaiter {
	wd := codexWatchdogs{stale: 600, ttfbEnabled: true, ttfb: 120, idleEnabled: true, idle: 180}
	return newNonStreamWaiter("test-model", 1000, wd, rec.sink)
}

func nsOffset(base float64, v *float64) *float64 {
	if v == nil {
		return nil
	}
	out := base + *v
	return &out
}

func nsF(v float64) *float64 { return &v }

func TestWaitNoticeTracksCurrentAttemptSilence(t *testing.T) {
	cases := []struct {
		event, progress, retry *float64
		expected               string
	}{
		{nsF(59), nsF(59), nil, ""},
		{nsF(59), nil, nil, ""},
		{nsF(0), nsF(0), nil, "provider stream active; 60s without stream events"},
		{nsF(0), nil, nil, "provider stream active; 60s without stream events"},
		{nsF(1), nil, nil, ""},
		{nil, nil, nil, "60s waiting for the first provider event"},
		{nsF(10), nsF(10), nsF(59), ""},
		{nsF(0), nsF(0), nsF(0), "60s waiting for the first provider event after reconnect"},
		{nsF(0), nsF(0), nsF(1), ""},
	}
	for i, tc := range cases {
		rec := &nsRecorder{}
		w := nsWaiter(rec)
		snap := nonStreamSnapshot{lastEvent: nsOffset(w.callStart, tc.event), lastProgress: nsOffset(w.callStart, tc.progress), retryStarted: nsOffset(w.callStart, tc.retry)}
		w.emitWaitNotice(30, true, snap)
		w.emitWaitNotice(59, true, snap)
		if len(rec.notices) != 0 || len(rec.touches) == 0 {
			t.Fatalf("case %d early notices %q touches %q", i, rec.notices, rec.touches)
		}
		if tc.event == nil && tc.retry == nil && strings.Contains(rec.touches[len(rec.touches)-1], "receiving") {
			t.Fatalf("case %d touch %q", i, rec.touches[len(rec.touches)-1])
		}
		w.emitWaitNotice(60, true, snap)
		if tc.expected == "" {
			if len(rec.notices) != 0 || len(rec.touches) == 0 {
				t.Fatalf("case %d quiet heartbeat lost, notices %q", i, rec.notices)
			}
			continue
		}
		if len(rec.notices) != 1 || !strings.Contains(rec.notices[0], tc.expected) || !strings.Contains(rec.notices[0], "auto-reconnect:") {
			t.Fatalf("case %d notices %q", i, rec.notices)
		}
	}
}

func TestResumedEventsClearOnlyThisRequestsWaitNotice(t *testing.T) {
	rec := &nsRecorder{}
	w := nsWaiter(rec)
	for tick := 1; tick <= 204; tick++ {
		now := 1000 + float64(tick)*0.3
		snap := nonStreamSnapshot{}
		if tick >= 201 {
			snap.lastEvent = nsF(now)
		}
		w.poll(now, snap)
	}
	if len(rec.notices) != 2 || !strings.Contains(rec.notices[0], "waiting for the first provider event") || rec.notices[1] != "" {
		t.Fatalf("notices %q", rec.notices)
	}
}

func TestNonStreamStaleKillCountsTowardBreakerAndTimesOut(t *testing.T) {
	rec := &nsRecorder{}
	session := &RetrySession{}
	ctx := withNonStreamSurface(withStaleStreak(context.Background(), session), rec.sink)
	start := time.Now()
	resp, err := runNonStreamRequest(ctx, "m", 1234, 0.5, func(ctx context.Context) (*Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	var te *TimeoutPhaseError
	if resp != nil || !errors.As(err, &te) || err.Error() == "" || !strings.Contains(te.Err.Error(), "Non-streaming API call timed out after 0s with no response (threshold: 0s)") {
		t.Fatalf("resp %v err %v", resp, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("kill took %s", time.Since(start))
	}
	if session.StaleStreak() != 1 {
		t.Fatalf("stale streak %d", session.StaleStreak())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.statuses) != 1 || rec.statuses[0] != "⚠️ No response from provider for 0s (non-streaming, model: m). Aborting call." {
		t.Fatalf("statuses %q", rec.statuses)
	}
	if len(rec.touches) != 2 || rec.touches[0] != "waiting for non-streaming API response" || rec.touches[1] != "stale non-streaming call killed after 0s" {
		t.Fatalf("touches %q", rec.touches)
	}
}

func TestNonStreamRequestReturnsResultAndInterrupts(t *testing.T) {
	want := &Response{Model: "m"}
	got, err := runNonStreamRequest(context.Background(), "m", 0, 600, func(context.Context) (*Response, error) { return want, nil })
	if err != nil || got != want {
		t.Fatalf("got %v err %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(400*time.Millisecond, cancel)
	_, err = runNonStreamRequest(ctx, "m", 0, 600, func(ctx context.Context) (*Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupt err %v", err)
	}
}

func TestInterruptedProviderWaitCountsOnlyPreResponseStalls(t *testing.T) {
	session := &RetrySession{}
	ctx := withStaleStreak(context.Background(), session)
	if recordInterruptedProviderWait(ctx, 29.9, false) || recordInterruptedProviderWait(ctx, 120, true) {
		t.Fatal("early or mid response interrupt counted")
	}
	if !recordInterruptedProviderWait(ctx, 30, false) || session.StaleStreak() != 1 {
		t.Fatalf("stall not counted, streak %d", session.StaleStreak())
	}
}

func TestNonStreamingSuccessFeedsTheHeaderCapture(t *testing.T) {
	window := cwHeaders(cwDefaultH())
	if !chatCompletionCapturesHeaders("nous", cwState(cwDefaults()), window) {
		t.Fatal("window session and response did not capture")
	}
	var seen []string
	rl := func(h http.Header) { seen = append(seen, "rl") }
	credits := func(h http.Header) { seen = append(seen, "credits") }
	none := func(http.Header) {}
	runHeaderCaptures(window, rl, credits, none, none)
	if strings.Join(seen, ",") != "rl,credits" {
		t.Fatalf("captures %q", seen)
	}
	seen = nil
	runHeaderCaptures(window, func(http.Header) { panic("bad header") }, credits, none, none)
	if strings.Join(seen, ",") != "credits" {
		t.Fatalf("captures after a failing one %q", seen)
	}
	if chatCompletionCapturesHeaders("example", cwState(cwDefaults()), window) {
		t.Fatal("another provider captured")
	}
	capture := &CreditsCapture{}
	capture.SetRoute("nous", "m", "https://inference-api.nousresearch.com/v1")
	seeded := cwState(cwDefaults())
	capture.adoptCredits(seeded)
	chatCompletionCapturingHeaders("nous", capture, window)
	if got := capture.Credits(); got == nil || got == seeded || !got.FromHeader {
		t.Fatalf("credits state not refreshed from the response: %+v", got)
	}
}

func TestNonStreamingNonWindowKeepsMainsBehaviour(t *testing.T) {
	grant := cwWith(cwHeaders(cwDefaultH()), "x-nous-credits-denominator-kind", "subscription_cap")
	remaining := http.Header{}
	remaining.Set("x-nous-credits-remaining-micros", "1")
	for i, h := range []http.Header{{}, grant, remaining} {
		if chatCompletionCapturesHeaders("nous", cwState(cwDefaults()), h) {
			t.Fatalf("non window response %d captured", i)
		}
	}
	window := cwHeaders(cwDefaultH())
	for i, state := range []*CreditsState{nil, cwState(cwDefaults(), func(s *CreditsState) { s.DenominatorKind = "subscription_cap" })} {
		if chatCompletionCapturesHeaders("nous", state, window) {
			t.Fatalf("session %d not on a window plan captured", i)
		}
	}
}
