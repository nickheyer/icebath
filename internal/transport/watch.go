package transport

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// How often a silent stream gets a liveness line, and how often the watch looks at it
	heartbeat = 30 * time.Second
	poll      = 300 * time.Millisecond
	// Silence after which the person is shown what the stream waits on
	waitShown = 60 * time.Second
	// Time before a watchdog fires inside which the notice says still waiting
	nearDeadline = 15 * time.Second
	// Silence of an interrupted wait for a first answer that counts as a stall
	interruptedStall = 30 * time.Second
)

// Watch over one streamed request: it drops the request when it stays silent past its stale limit, its connection stays quiet past its
// read limit or it streams past its lifetime, and tells the person how the wait goes
type Watch struct {
	Timeouts Timeouts
	// Model asked, for the lines shown, and rough tokens of the request's context
	Model  string
	Tokens int
	// Where the lines shown go
	Emit provider.StreamFunc

	mu      sync.Mutex
	started time.Time
	kill    string
	after   float64
	cancel  context.CancelFunc
}

// Request cut for staying silent past its stale limit, which is worth asking again
type StallError struct {
	Silence time.Duration
	Model   string
	// Limit of a request that wasn't streamed, 0 for a stream
	Limit time.Duration
}

func (e *StallError) Error() string {
	if e.Limit > 0 {
		return fmt.Sprintf("The provider sent nothing for %s, past the silence limit of %s, so the request was stopped. Silent request limit, an "+
			"option of Connections, gives it longer.", strs.Span(seconds(e.Silence)), strs.Span(seconds(e.Limit)))
	}
	return fmt.Sprintf("%s sent nothing for %s, so icebath closed the connection. Model silence limit, an option of Connections, gives it "+
		"longer.", cmp.Or(e.Model, "The model"), strs.Span(seconds(e.Silence)))
}

func (e *StallError) Timeout() bool { return true }

// Reply cut for streaming past transports.stream_max_lifetime
type LifetimeError struct {
	Elapsed time.Duration
	Cap     time.Duration
}

func (e *LifetimeError) Error() string {
	return fmt.Sprintf("The reply was still coming after %s, past the stream time limit of %s, so it was cut. Stream time limit, an option of "+
		"Connections, gives this model longer.", strs.Span(seconds(e.Elapsed)), strs.Span(seconds(e.Cap)))
}

func (e *LifetimeError) Timeout() bool { return true }

// Stream whose connection stayed quiet past its read limit
type QuietError struct {
	After time.Duration
}

func (e *QuietError) Error() string {
	return fmt.Sprintf("The provider's reply sent nothing for %s, so reading it stopped.", strs.Span(seconds(e.After)))
}

func (e *QuietError) Timeout() bool { return true }

// Interrupted request that had waited long on a first answer, which counts toward the session's stalls
type WaitedError struct {
	Waited time.Duration
	Err    error
}

func (e *WaitedError) Error() string { return e.Err.Error() }

func (e *WaitedError) Unwrap() error { return e.Err }

func (w *Watch) emit(kind provider.EventKind, text string) {
	if w.Emit != nil {
		_ = w.Emit(provider.StreamEvent{Kind: kind, Text: text})
	}
}

func (w *Watch) model() string {
	if w.Model == "" {
		return "unknown"
	}
	return w.Model
}

// Records the kill that cuts the attempt, the first one winning unless a lifetime cut comes after
func (w *Watch) fire(reason string, elapsed float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.kill == "" || reason == "lifetime" {
		w.kill, w.after = reason, elapsed
	}
	if w.cancel != nil {
		w.cancel()
	}
}

func (w *Watch) killed() (string, float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.kill, w.after
}

// Arms the watch over the wait for the answer's headers, which the cancel cuts: the stale limit counted from the attempt's start and the read
// limit; the stop it gives ends the wait
func (w *Watch) Headers(cancel context.CancelFunc) func() {
	w.mu.Lock()
	w.started, w.cancel = time.Now(), cancel
	w.mu.Unlock()
	slog.Debug(fmt.Sprintf("stream watchdog armed: stale=%s cap=%s model=%s", window(w.Timeouts.Stale), window(w.Timeouts.Lifetime), w.model()))
	read := time.AfterFunc(w.Timeouts.Read, func() { w.fire("read", w.Timeouts.Read.Seconds()) })
	stale := func() {}
	if w.Timeouts.Stale > 0 && w.Timeouts.Stale != Forever {
		timer := time.AfterFunc(w.Timeouts.Stale, func() {
			elapsed := time.Since(w.started)
			w.stalled(elapsed)
			w.fire("stale", elapsed.Seconds())
		})
		stale = func() { timer.Stop() }
	}
	return func() {
		read.Stop()
		stale()
	}
}

// Seconds of a watchdog window for the log, no limit said in words
func window(d time.Duration) string {
	if d == Forever || d == 0 {
		return "inf"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 0, 64)
}

// Lines shown once a stream is dropped for its silence
func (w *Watch) stalled(silence time.Duration) {
	slog.Warn(fmt.Sprintf("Stream stale for %.0fs (threshold %.0fs) — no chunks received. model=%s context=~%s tokens. Killing connection.",
		silence.Seconds(), w.Timeouts.Stale.Seconds(), w.model(), strs.Thousands(w.Tokens)))
	w.emit(provider.EventStatus, fmt.Sprintf("%s sent nothing for %s, with about %s tokens of session to read. Connecting again.", w.model(),
		strs.Span(seconds(silence)), strs.Thousands(w.Tokens)))
	w.emit(provider.EventWait, fmt.Sprintf("Nothing from the provider for %s. Connecting again.", strs.Span(seconds(silence))))
	w.emit(provider.EventActivity, fmt.Sprintf("connecting again after %s of silence", strs.Span(seconds(silence))))
}

// Error of an attempt cut by the watch, the cause of one it didn't cut; the context's own when the caller cancelled
func (w *Watch) Failed(ctx context.Context, cause error) error {
	reason, elapsed := w.killed()
	switch reason {
	case "stale":
		return &TimeoutError{Phase: PhaseRead, Err: &StallError{Silence: Seconds(elapsed), Model: w.model()}}
	case "lifetime":
		return &TimeoutError{Phase: PhaseRead, Err: &LifetimeError{Elapsed: Seconds(elapsed), Cap: w.Timeouts.Lifetime}}
	case "read":
		return &TimeoutError{Phase: PhaseRead, Err: &QuietError{After: Seconds(elapsed)}}
	}
	if ctx.Err() != nil {
		if silence := time.Since(w.started); silence >= interruptedStall && silence <= w.Timeouts.Stale {
			slog.Warn(fmt.Sprintf("Interrupted provider wait counted as stale after %.0fs with no output.", silence.Seconds()))
			return &WaitedError{Waited: silence, Err: ctx.Err()}
		}
		return ctx.Err()
	}
	return Timed(cause, PhaseRead)
}

// Reads a stream's frames under the watch, handing each to handle until it ends the stream or the stream ends; handle says whether the
// frame was output, which restarts the silence clock, and whether reading stops there. The read and the context are cut by the cancel the
// watch's header wait was given
func (w *Watch) Run(ctx context.Context, next func() (*Frame, error), handle func(*Frame) (live, stop bool, err error)) error {
	type read struct {
		frame *Frame
		err   error
	}
	frames := make(chan read, 32)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			frame, err := next()
			select {
			case frames <- read{frame, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	stale := time.NewTimer(w.Timeouts.Stale)
	defer stale.Stop()
	readTimer := time.NewTimer(w.Timeouts.Read)
	defer readTimer.Stop()
	w.mu.Lock()
	started := w.started
	w.mu.Unlock()
	if started.IsZero() {
		started = time.Now()
	}
	var life <-chan time.Time
	if w.Timeouts.Lifetime > 0 {
		timer := time.NewTimer(w.Timeouts.Lifetime - time.Since(started))
		defer timer.Stop()
		life = timer.C
	}
	stale.Reset(w.Timeouts.Stale - time.Since(started))
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	last, beat, first := started, time.Now(), time.Time{}
	var notice wait
	noticeAt := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return w.Failed(ctx, ctx.Err())
		case now := <-ticker.C:
			if !noticeAt.IsZero() && last.After(noticeAt) {
				w.emit(provider.EventWait, "")
				noticeAt, notice = time.Time{}, wait{}
			}
			if now.Sub(beat) < heartbeat {
				continue
			}
			beat = now
			silence := now.Sub(last)
			if silence < waitShown {
				w.emit(provider.EventActivity, fmt.Sprintf("waiting for the reply, nothing yet after %s", strs.Span(seconds(silence))))
				continue
			}
			phase := "first_chunk"
			if !first.IsZero() {
				phase = "post_chunk"
			}
			var deadline *Deadline
			if w.Timeouts.Stale != Forever {
				deadline = &Deadline{Label: "stream stale", Remaining: w.Timeouts.Stale - silence}
			}
			if !notice.emits(phase, deadline) {
				w.emit(provider.EventActivity, "waiting for the reply, "+fmt.Sprintf(waitPhases[phase], strs.Span(seconds(silence))))
				continue
			}
			noticeAt = beat
			w.emit(provider.EventWait, WaitText(w.model(), silence, phase, deadline))
		case <-stale.C:
			silence := time.Since(last)
			w.stalled(silence)
			w.fire("stale", silence.Seconds())
			return w.Failed(ctx, nil)
		case <-readTimer.C:
			w.fire("read", w.Timeouts.Read.Seconds())
			return w.Failed(ctx, nil)
		case now := <-life:
			if now.Sub(last) >= w.Timeouts.Stale {
				w.stalled(now.Sub(last))
				w.fire("stale", now.Sub(last).Seconds())
				return w.Failed(ctx, nil)
			}
			elapsed := now.Sub(started)
			slog.Warn(fmt.Sprintf("Stream still running after %.0fs (stream_max_lifetime %.0fs) — killing connection. model=%s", elapsed.Seconds(),
				w.Timeouts.Lifetime.Seconds(), w.model()))
			w.emit(provider.EventStatus, fmt.Sprintf("⚠ The reply from %s was still coming after %s, past the stream time limit of %s, so it is cut.",
				w.model(), strs.Span(seconds(elapsed)), strs.Span(seconds(w.Timeouts.Lifetime))))
			w.fire("lifetime", elapsed.Seconds())
			return w.Failed(ctx, nil)
		case r := <-frames:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) {
					return nil
				}
				return w.Failed(ctx, r.err)
			}
			reset(readTimer, w.Timeouts.Read)
			live, stop, err := handle(r.frame)
			if live {
				last = time.Now()
				if first.IsZero() {
					first = last
				}
				reset(stale, w.Timeouts.Stale)
			}
			if err != nil || stop {
				w.fire("", 0)
				return err
			}
		}
	}
}

func reset(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// Watchdog that would drop a silent request and the time until it fires
type Deadline struct {
	Label     string
	Remaining time.Duration
}

// What the person was last shown of a wait: its phase, the watchdog it named, and whether it said it was near
type wait struct {
	phase, label string
	labelled     bool
	near         bool
}

// Whether the wait gets a new notice: the first of a silence, one whose phase or watchdog changed, and once near the deadline
func (n *wait) emits(phase string, deadline *Deadline) bool {
	label, labelled := "", deadline != nil
	if labelled {
		label = deadline.Label
	}
	near := deadline != nil && deadline.Remaining <= nearDeadline
	emit := n.phase != phase || n.labelled != labelled || n.label != label || near && !n.near
	n.phase, n.label, n.labelled = phase, label, labelled
	if near {
		n.near = true
	}
	return emit
}

var waitPhases = map[string]string{
	"first_event":  "%s with no answer yet",
	"reconnect":    "%s with no answer since connecting again",
	"pre_progress": "%s with the reply open but no progress",
	"post_event":   "%s with the reply open but quiet",
	"first_chunk":  "%s with no text yet",
	"post_chunk":   "%s since its last text",
}

// Line naming what a silent request waits on and the watchdog that would drop it
func WaitText(model string, silence time.Duration, phase string, deadline *Deadline) string {
	lead := "Waiting for"
	if deadline != nil && deadline.Remaining <= nearDeadline {
		lead = "Still waiting for"
	}
	text := fmt.Sprintf("%s %s, ", lead, model) + fmt.Sprintf(waitPhases[phase], strs.Span(seconds(silence))) + "."
	if deadline != nil {
		text += fmt.Sprintf(" icebath connects again in %s.", strs.Span(seconds(max(0, deadline.Remaining))))
	}
	return text
}

// Waits on a request that isn't streamed: shows what it waits on while it is silent and drops it once silent past its stale limit, the
// call given the context the drop cancels
func Await[T any](ctx context.Context, w *Watch, call func(context.Context) (T, error)) (T, error) {
	var zero T
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	w.mu.Lock()
	w.started, w.cancel = time.Now(), cancel
	w.mu.Unlock()
	w.emit(provider.EventActivity, "waiting for the provider to answer")
	go func() {
		value, err := call(ctx)
		done <- result{value, err}
	}()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var notice wait
	polls := 0
	for {
		select {
		case r := <-done:
			return r.value, r.err
		case now := <-ticker.C:
			silence := now.Sub(w.started)
			if polls++; polls%100 == 0 {
				var deadline *Deadline
				if w.Timeouts.Stale != Forever {
					deadline = &Deadline{Label: "wall-clock stale", Remaining: w.Timeouts.Stale - silence}
				}
				switch {
				case silence < waitShown:
					w.emit(provider.EventActivity, "waiting for the provider to answer")
				case notice.emits("first_event", deadline):
					w.emit(provider.EventWait, WaitText(w.model(), silence, "first_event", deadline))
				default:
					w.emit(provider.EventActivity, fmt.Sprintf("waiting for the provider to answer, nothing yet after %s", strs.Span(seconds(silence))))
				}
			}
			if w.Timeouts.Stale == Forever || silence <= w.Timeouts.Stale {
				continue
			}
			slog.Warn(fmt.Sprintf("Non-streaming API call stale for %.0fs (threshold %.0fs). model=%s context=~%s tokens. Killing connection.",
				silence.Seconds(), w.Timeouts.Stale.Seconds(), w.model(), strs.Thousands(w.Tokens)))
			w.emit(provider.EventStatus, fmt.Sprintf("%s sent nothing for %s, so the request is stopped.", w.model(), strs.Span(seconds(silence))))
			w.emit(provider.EventActivity, fmt.Sprintf("stopped a request silent for %s", strs.Span(seconds(silence))))
			cancel()
			select {
			case r := <-done:
				if r.err == nil || !errors.Is(r.err, context.Canceled) {
					return r.value, r.err
				}
			case <-time.After(2 * time.Second):
			}
			return zero, &TimeoutError{Phase: PhaseRead, Err: &StallError{Silence: silence, Model: w.model(), Limit: w.Timeouts.Stale}}
		}
	}
}

// Length of time rounded to whole seconds, at least one, as a person reads a wait
func seconds(d time.Duration) time.Duration {
	return max(d.Round(time.Second), time.Second)
}
