package provider

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Source of one completed Messages stream ending in message_stop
func completedAnthSource() anthEventSource {
	evs := append(anthTextEvents("ok"),
		&anthEvent{Type: "content_block_stop", Payload: map[string]any{"type": "content_block_stop", "index": 0}},
		&anthEvent{Type: "message_delta", Payload: map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 5}}},
		&anthEvent{Type: "message_stop", Payload: map[string]any{"type": "message_stop"}})
	i := 0
	return &scriptedAnthEvents{next: func() (*anthEvent, error) {
		if i >= len(evs) {
			return nil, errors.New("EOF")
		}
		i++
		return evs[i-1], nil
	}}
}

// Source whose first read waits for the attempt abort, or for wait when ignoreAbort, then drops the connection
func droppingAnthSource(resp *http.Response, wait time.Duration, ignoreAbort bool) anthEventSource {
	return &scriptedAnthEvents{next: func() (*anthEvent, error) {
		if ignoreAbort {
			time.Sleep(wait)
		} else {
			select {
			case <-resp.Request.Context().Done():
			case <-time.After(wait):
			}
		}
		return nil, errors.New("connection dropped after close()")
	}}
}

// Runs call, the breaker check then the reconnect loop, the unit hermes drives through _interruptible_streaming_api_call
func (h *abandonHarness) call(ctx context.Context) <-chan streamCallOutcome {
	out := make(chan streamCallOutcome, 1)
	noSleep := func(context.Context, time.Duration) error { return nil }
	opts := DoOptions{Session: h.session, Log: slog.Default(), Now: time.Now, Sleep: noSleep, Stream: func(StreamEvent) error { return nil }}
	l := &doLoop{ctx: withStaleStreak(ctx, h.session), opts: opts, client: h.client, req: &Request{Messages: userRequest("hi").Messages},
		route: h.client.Route(), session: h.session, log: opts.Log, now: opts.Now, sleep: noSleep, maxRetries: 1}
	h.loop = l
	go func() {
		resp, _, err := l.call()
		out <- streamCallOutcome{resp: resp, err: err}
	}()
	return out
}

func TestInterruptedPreResponseWaitAdvancesStreak(t *testing.T) {
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "2")
	session := &RetrySession{}
	ctx := withStaleStreak(t.Context(), session)
	if recordInterruptedProviderWait(ctx, 29.9, false) || recordInterruptedProviderWait(ctx, 45.0, true) || session.StaleStreak() != 0 {
		t.Fatalf("neutral cancellations counted, streak %d", session.StaleStreak())
	}
	if !recordInterruptedProviderWait(ctx, 45.0, false) || session.StaleStreak() != 1 {
		t.Fatalf("pre response wait streak %d", session.StaleStreak())
	}
	if !recordInterruptedProviderWait(ctx, 60.0, false) {
		t.Fatal("second pre response wait not counted")
	}
	if err := checkStaleGiveup(session); err == nil || !strings.Contains(err.Error(), "2 consecutive stale attempts") {
		t.Fatalf("breaker %v", err)
	}
}

func TestShortCircuitsWhenStreakAtThreshold(t *testing.T) {
	h := newAbandonHarness(t, func(resp *http.Response) anthEventSource { return completedAnthSource() })
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "3")
	h.session.staleStreak.Store(3)
	err := h.wait(h.call(t.Context()), 10*time.Second).err
	if err == nil || !strings.Contains(err.Error(), "unresponsive") {
		t.Fatalf("err %v", err)
	}
	if h.override.opened.Load() != 0 || h.session.StaleStreak() != 3 {
		t.Fatalf("opened %d streak %d", h.override.opened.Load(), h.session.StaleStreak())
	}
}

func TestSuccessResetsStreak(t *testing.T) {
	h := newAbandonHarness(t, func(resp *http.Response) anthEventSource { return completedAnthSource() })
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "3")
	h.session.staleStreak.Store(2)
	o := h.wait(h.call(t.Context()), 20*time.Second)
	if o.err != nil || o.resp == nil || h.session.StaleStreak() != 0 {
		t.Fatalf("resp %v err %v streak %d", o.resp, o.err, h.session.StaleStreak())
	}
}

func TestStaleKillIncrementsStreak(t *testing.T) {
	h := newAbandonHarness(t, func(resp *http.Response) anthEventSource { return droppingAnthSource(resp, 5*time.Second, false) })
	t.Setenv(StreamStaleTimeoutEnv, "0.1")
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "50")
	o := h.wait(h.call(t.Context()), 30*time.Second)
	if o.err == nil || h.session.StaleStreak() < 1 {
		t.Fatalf("err %v streak %d", o.err, h.session.StaleStreak())
	}
}

func TestStaleWindowCoversTheHeaderWaitOfAnAttempt(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	t.Setenv(StreamStaleTimeoutEnv, "0.2")
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "50")
	t.Setenv(StreamRetriesEnv, "0")
	srv := newAnthServer(t, func(w http.ResponseWriter, body map[string]any) { time.Sleep(3 * time.Second) })
	c := srv.client(srv.route("https://example.invalid", "test-key"))
	h := &abandonHarness{t: t, session: &RetrySession{}, client: c}
	logs := captureLogs(t)
	begin := time.Now()
	o := h.wait(h.call(t.Context()), 20*time.Second)
	var te *TimeoutPhaseError
	if !errors.As(o.err, &te) || time.Since(begin) > 2500*time.Millisecond || logCount(logs, "Stream stale for") != 1 || h.session.StaleStreak() != 1 {
		t.Fatalf("err %v after %v kills %d streak %d", o.err, time.Since(begin), logCount(logs, "Stream stale for"), h.session.StaleStreak())
	}
}

func TestOneAttemptCountsOnceHoweverOftenTheTimerFires(t *testing.T) {
	for _, slow := range []string{"worker_start", "worker_unwind"} {
		t.Run(slow, func(t *testing.T) {
			h := newAbandonHarness(t, func(resp *http.Response) anthEventSource {
				if slow == "worker_start" {
					src := droppingAnthSource(resp, 1200*time.Millisecond, false)
					started := false
					return &scriptedAnthEvents{next: func() (*anthEvent, error) {
						if !started {
							started = true
							time.Sleep(time.Second)
						}
						return src.Next()
					}}
				}
				return droppingAnthSource(resp, 1200*time.Millisecond, true)
			})
			t.Setenv(StreamStaleTimeoutEnv, "0.1")
			t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "50")
			t.Setenv("ICEBATH_STREAM_RETRIES", "0")
			logs := captureLogs(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if o := h.wait(h.call(ctx), 30*time.Second); o.err == nil {
				t.Fatal("wedged attempt succeeded")
			}
			if kills := logCount(logs, "Stream stale for"); kills < 2 {
				t.Fatalf("scenario needs the timer to re-fire; saw %d kill(s)", kills)
			}
			if h.session.StaleStreak() != 1 {
				t.Fatalf("streak %d", h.session.StaleStreak())
			}
		})
	}
}
