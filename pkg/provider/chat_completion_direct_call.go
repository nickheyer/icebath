package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

var directAPIActivityHeartbeat = 15 * time.Second

type inlineNonStreamKey struct{}

// Inline marker of a blocking request and whether it takes the stale budget as its socket backstop
type inlineNonStreamMark struct {
	hardTimeout bool
}

// Context marking a blocking request that runs inline, a request with its own timeout keeping it
func withInlineNonStream(ctx context.Context, hardTimeout bool) context.Context {
	return context.WithValue(ctx, inlineNonStreamKey{}, inlineNonStreamMark{hardTimeout: hardTimeout})
}

// Whether the blocking request of this context runs inline
func inlineNonStream(ctx context.Context) bool {
	_, ok := ctx.Value(inlineNonStreamKey{}).(inlineNonStreamMark)
	return ok
}

// Whether the inline request takes the stale budget as its socket backstop
func inlineHardTimeoutArmed(ctx context.Context) bool {
	m, _ := ctx.Value(inlineNonStreamKey{}).(inlineNonStreamMark)
	return m.hardTimeout
}

// Socket backstop of an inline blocking call, the stale budget when finite and positive, zero when disarmed
func inlineNonstreamHardTimeout(stale float64) time.Duration {
	if math.IsInf(stale, 0) || math.IsNaN(stale) || stale <= 0 {
		return 0
	}
	return secondsDuration(stale)
}

// Lifecycle of one inline blocking request where the first of done, cancelled and stale owns the outcome
type inlineRequest struct {
	mu        sync.Mutex
	done      bool
	stale     bool
	cancelled bool
	abort     context.CancelFunc
}

// Claims the stale transition once, never after completion or an interrupt, and aborts the request
func (r *inlineRequest) staleKill(ctx context.Context) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return false
	}
	if ctx.Err() != nil {
		r.cancelled = true
	}
	if r.cancelled {
		r.abort()
		return false
	}
	newly := !r.stale
	if newly {
		r.stale = true
		bumpStaleStreak(ctx)
	}
	r.abort()
	return newly
}

// Records completion so a late timer neither reports nor bumps the stale streak
func (r *inlineRequest) markDone() {
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
}

// Whether the stale timer killed the request
func (r *inlineRequest) wasStale() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stale
}

// Runs a blocking request on the calling goroutine with an activity heartbeat, a one shot stale kill and a socket backstop
func runInlineNonStreamRequest(ctx context.Context, model string, estTokens int, stale float64, call func(context.Context) (*Response, error)) (*Response, error) {
	surface := nonStreamEmitter(ctx, nil)
	touch := func() { surface(StreamEvent{Kind: EventActivity, Text: "waiting for non-streaming API response"}) }
	touch()
	callStart := time.Now()
	reqCtx, abort := context.WithCancel(ctx)
	defer abort()
	if hard := inlineNonstreamHardTimeout(stale); hard > 0 && inlineHardTimeoutArmed(ctx) {
		var cancelHard context.CancelFunc
		reqCtx, cancelHard = context.WithTimeout(reqCtx, hard)
		defer cancelHard()
	}
	r := &inlineRequest{abort: abort}
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(directAPIActivityHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-t.C:
				func() {
					defer func() { _ = recover() }()
					touch()
				}()
			}
		}
	}()
	onStale := func() {
		if !r.staleKill(ctx) {
			return
		}
		elapsed := time.Since(callStart).Seconds()
		reportStaleNonstreamKill(surface, model, estTokens, elapsed, stale, true, "")
		touchStaleKillActivity(surface, elapsed)
	}
	var watchdog *time.Timer
	fired := make(chan struct{})
	if hard := inlineNonstreamHardTimeout(stale); hard > 0 {
		watchdog = time.AfterFunc(hard, func() {
			defer close(fired)
			onStale()
		})
	}
	resp, err := call(reqCtx)
	if watchdog != nil && !watchdog.Stop() {
		<-fired
	}
	if err != nil && ctx.Err() == nil && errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		onStale()
	}
	r.markDone()
	close(hbStop)
	select {
	case <-hbDone:
	case <-time.After(2 * time.Second):
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if r.wasStale() {
			return nil, &TimeoutPhaseError{Phase: TimeoutPhaseRead, Err: fmt.Errorf("Non-streaming API call timed out after %ds with no response (threshold: %ds)",
				int(time.Since(callStart).Seconds()), int(stale))}
		}
		return nil, err
	}
	resetStaleStreak(ctx)
	return resp, nil
}

// Clears the stale breaker of the context's retry session after a responsive call
func resetStaleStreak(ctx context.Context) {
	if s, _ := ctx.Value(staleStreakKey{}).(*RetrySession); s != nil {
		s.ResetStaleStreak()
	}
}
