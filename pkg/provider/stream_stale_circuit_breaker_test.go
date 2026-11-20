package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// One completed Messages stream ending in message_stop with its final message
func completedMessageStream() *scriptedMessageStream {
	events := append(messageTextEvents("ok"), map[string]any{"type": "message_stop"})
	i := 0
	return &scriptedMessageStream{
		final: map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}},
		next: func(context.Context) (any, error) {
			if i >= len(events) {
				return nil, io.EOF
			}
			i++
			return events[i-1], nil
		}}
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
	h := newAbandonHarness(t, func() (anthMessageStream, error) { return completedMessageStream(), nil })
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "3")
	h.agent.retry.staleStreak.Store(3)
	err := h.wait(h.start(t.Context()), 10*time.Second).err
	if err == nil || !strings.Contains(err.Error(), "unresponsive") {
		t.Fatalf("err %v", err)
	}
	if h.client.opened.Load() != 0 || h.agent.retry.StaleStreak() != 3 {
		t.Fatalf("opened %d streak %d", h.client.opened.Load(), h.agent.retry.StaleStreak())
	}
}

func TestSuccessResetsStreak(t *testing.T) {
	h := newAbandonHarness(t, func() (anthMessageStream, error) { return completedMessageStream(), nil })
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "3")
	h.agent.retry.staleStreak.Store(2)
	o := h.wait(h.start(t.Context()), 20*time.Second)
	if o.err != nil || o.resp == nil || h.agent.retry.StaleStreak() != 0 {
		t.Fatalf("resp %v err %v streak %d", o.resp, o.err, h.agent.retry.StaleStreak())
	}
}

func TestStaleKillIncrementsStreak(t *testing.T) {
	recordStreamBackoff(t)
	drops := &droppingStreams{}
	h := newAbandonHarness(t, func() (anthMessageStream, error) { return drops.open(5*time.Second, false), nil })
	t.Setenv(StreamStaleTimeoutEnv, "0.1")
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "50")
	h.agent.onAbort = drops.abort
	o := h.wait(h.start(t.Context()), 30*time.Second)
	if o.err == nil || h.agent.retry.StaleStreak() < 1 {
		t.Fatalf("err %v streak %d", o.err, h.agent.retry.StaleStreak())
	}
}

func TestStaleWindowCoversTheHeaderWaitOfAnAttempt(t *testing.T) {
	t.Setenv(home.EnvHome, t.TempDir())
	t.Setenv(StreamStaleTimeoutEnv, "0.2")
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "50")
	srv := newAnthServer(t, func(w http.ResponseWriter, body map[string]any) { time.Sleep(3 * time.Second) })
	c := srv.client(srv.route("https://example.invalid", "test-key"))
	session := &RetrySession{}
	logs := captureLogs(t)
	begin := time.Now()
	_, err := c.Stream(withStaleStreak(t.Context(), session), &Request{Messages: userRequest("hi").Messages}, func(StreamEvent) error { return nil })
	var te *TimeoutPhaseError
	if !errors.As(err, &te) || time.Since(begin) > 2500*time.Millisecond || logCount(logs, "Stream stale for") != 1 || session.StaleStreak() != 1 {
		t.Fatalf("err %v after %v kills %d streak %d", err, time.Since(begin), logCount(logs, "Stream stale for"), session.StaleStreak())
	}
}

func TestOneAttemptCountsOnceHoweverOftenTheTimerFires(t *testing.T) {
	for _, slow := range []string{"worker_start", "worker_unwind"} {
		t.Run(slow, func(t *testing.T) {
			drops := &droppingStreams{}
			h := newAbandonHarness(t, func() (anthMessageStream, error) {
				if slow == "worker_start" {
					time.Sleep(1500 * time.Millisecond)
					return drops.open(1500*time.Millisecond, false), nil
				}
				return drops.open(1500*time.Millisecond, true), nil
			})
			t.Setenv(StreamStaleTimeoutEnv, "0.1")
			t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "50")
			t.Setenv("ICEBATH_STREAM_RETRIES", "0")
			if slow == "worker_start" {
				h.agent.onAbort = drops.abort
			}
			logs := captureLogs(t)
			if o := h.wait(h.start(t.Context()), 30*time.Second); o.err == nil {
				t.Fatal("wedged attempt succeeded")
			}
			if kills := logCount(logs, "Stream stale for"); kills < 2 {
				t.Fatalf("scenario needs the timer to re-fire; saw %d kill(s)", kills)
			}
			if h.agent.retry.StaleStreak() != 1 {
				t.Fatalf("streak %d", h.agent.retry.StaleStreak())
			}
		})
	}
}
