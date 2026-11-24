package httputil

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Phases of one exchange a Timeout limits
const (
	// Waiting for a free connection from the pool
	PhasePool = "pool"
	// Name lookup, dialing, a proxy tunnel and the TLS handshake
	PhaseConnect = "connect"
	// Sending the request, the limit restarting each time the connection takes more of the body
	PhaseWrite = "write"
	// Waiting for the response, then each read of its body, the limit restarting with every read and not running while the caller holds the body between reads
	PhaseRead = "read"
)

// Limits of the phases of one exchange, a zero or negative limit never firing
type Timeout struct{ Connect, Read, Write, Pool time.Duration }

// Timeout giving every phase the same limit, none at all when d is not positive
func EachPhase(d time.Duration) Timeout { return Timeout{Connect: d, Read: d, Write: d, Pool: d} }

// Whether no phase has a limit
func (t Timeout) unlimited() bool {
	return t.Connect <= 0 && t.Read <= 0 && t.Write <= 0 && t.Pool <= 0
}

func (t Timeout) of(phase string) time.Duration {
	switch phase {
	case PhaseConnect:
		return t.Connect
	case PhaseWrite:
		return t.Write
	case PhaseRead:
		return t.Read
	}
	return t.Pool
}

// Phase of an exchange that outlasted its limit
type TimeoutError struct {
	Phase string
	// Limit that ran out, zero when unknown
	Limit time.Duration
}

// The phase that timed out and its limit, such as "read timed out after 30s"
func (e *TimeoutError) Error() string {
	var b strings.Builder
	switch e.Phase {
	case PhasePool:
		b.WriteString("waiting for a free connection")
	case "":
		b.WriteString("request")
	default:
		b.WriteString(e.Phase)
	}
	b.WriteString(" timed out")
	if e.Limit > 0 {
		b.WriteString(" after ")
		b.WriteString(e.Limit.String())
	}
	return b.String()
}

// Always true: the error is a timeout
func (e *TimeoutError) Timeout() bool { return true }

// Always true: the same request may succeed when sent again
func (e *TimeoutError) Temporary() bool { return true }

// Whether target is context.DeadlineExceeded or os.ErrDeadlineExceeded, the deadline errors a phase limit stands for
func (e *TimeoutError) Is(target error) bool {
	return target == context.DeadlineExceeded || target == os.ErrDeadlineExceeded
}

// Cancels one exchange when its current phase outlasts the limit of that phase; the deadline moves on every phase change, on progress within the write phase and on every body read
type watchdog struct {
	limits Timeout
	cancel context.CancelFunc

	mu        sync.Mutex
	timer     *time.Timer
	fireAt    time.Time
	deadline  time.Time
	phase     string
	limit     time.Duration
	connected bool
	// The response is with the caller, so only a body read in progress runs the read limit
	responded bool
	done      bool
	err       *TimeoutError
}

// Watchdog in the pool phase, cancel called when a limit runs out
func newWatchdog(limits Timeout, cancel context.CancelFunc) *watchdog {
	w := &watchdog{limits: limits, cancel: cancel}
	w.mu.Lock()
	w.enterLocked(PhasePool)
	w.mu.Unlock()
	return w
}

// Moves to the phase and restarts its limit
func (w *watchdog) enter(phase string) {
	w.mu.Lock()
	w.enterLocked(phase)
	w.mu.Unlock()
}

// Restarts the limit when the exchange is still in the phase, the progress a write makes
func (w *watchdog) progress(phase string) {
	w.mu.Lock()
	if w.phase == phase {
		w.enterLocked(phase)
	}
	w.mu.Unlock()
}

// Starts waiting for the response, ignored once the response is with the caller so a request write finishing late cannot run the read limit between body reads
func (w *watchdog) awaitResponse() {
	w.mu.Lock()
	if !w.responded {
		w.enterLocked(PhaseRead)
	}
	w.mu.Unlock()
}

// Hands the response to the caller: the read limit stops running until the first body read
func (w *watchdog) respond() {
	w.mu.Lock()
	w.responded = true
	w.pauseLocked()
	w.mu.Unlock()
}

// Ends one body read that returned without failing, the read limit not running until the next one
func (w *watchdog) pause() {
	w.mu.Lock()
	w.pauseLocked()
	w.mu.Unlock()
}

// Read phase with no running deadline; a pending timer finds the zero deadline and stands down
func (w *watchdog) pauseLocked() {
	if w.done {
		return
	}
	w.phase, w.limit, w.deadline = PhaseRead, w.limits.Read, time.Time{}
}

func (w *watchdog) enterLocked(phase string) {
	if w.done {
		return
	}
	w.phase = phase
	w.limit = w.limits.of(phase)
	if w.limit <= 0 {
		w.deadline = time.Time{}
		return
	}
	w.deadline = time.Now().Add(w.limit)
	switch {
	case w.timer == nil:
		w.timer = time.AfterFunc(w.limit, w.fire)
		w.fireAt = w.deadline
	case w.fireAt.IsZero() || w.deadline.Before(w.fireAt):
		w.timer.Reset(w.limit)
		w.fireAt = w.deadline
	}
}

// Timer callback: cancels the exchange once the deadline has passed, re-arms for a deadline that moved later
func (w *watchdog) fire() {
	w.mu.Lock()
	if w.done || w.deadline.IsZero() {
		w.fireAt = time.Time{}
		w.mu.Unlock()
		return
	}
	if wait := time.Until(w.deadline); wait > 0 {
		w.timer.Reset(wait)
		w.fireAt = w.deadline
		w.mu.Unlock()
		return
	}
	w.done = true
	w.fireAt = time.Time{}
	w.err = &TimeoutError{Phase: w.phase, Limit: w.limit}
	w.mu.Unlock()
	w.cancel()
}

// Stops watching, the exchange finished or was abandoned
func (w *watchdog) stop() {
	w.mu.Lock()
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
}

// Timeout that cancelled the exchange, nil when no limit ran out
func (w *watchdog) timedOut() *TimeoutError {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Client error with its cause replaced by the timeout when a limit cancelled the exchange
func (w *watchdog) clientError(err error) error {
	te := w.timedOut()
	if te == nil {
		return err
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return &url.Error{Op: ue.Op, URL: ue.URL, Err: te}
	}
	return te
}

// Start of a connection attempt, ignored once the request holds a connection so a dial racing in the background cannot move the phase back
func (w *watchdog) connecting() {
	w.mu.Lock()
	if !w.connected && w.phase != PhaseConnect {
		w.enterLocked(PhaseConnect)
	}
	w.mu.Unlock()
}

// Request of the exchange: the trace that moves the phases and, under a write limit, a body whose reads count as write progress
func (w *watchdog) request(ctx context.Context, req *http.Request) *http.Request {
	if w.limits.unlimited() {
		return req.WithContext(ctx)
	}
	trace := &httptrace.ClientTrace{
		GetConn: func(string) {
			w.mu.Lock()
			w.connected = false
			w.enterLocked(PhasePool)
			w.mu.Unlock()
		},
		DNSStart:     func(httptrace.DNSStartInfo) { w.connecting() },
		ConnectStart: func(string, string) { w.connecting() },
		GotConn: func(httptrace.GotConnInfo) {
			w.mu.Lock()
			w.connected = true
			w.enterLocked(PhaseWrite)
			w.mu.Unlock()
		},
		WroteRequest:         func(httptrace.WroteRequestInfo) { w.awaitResponse() },
		GotFirstResponseByte: func() { w.awaitResponse() },
	}
	out := req.WithContext(httptrace.WithClientTrace(ctx, trace))
	if w.limits.Write <= 0 {
		return out
	}
	if out.Body != nil && out.Body != http.NoBody {
		out.Body = &progressBody{ReadCloser: out.Body, w: w}
	}
	if get := out.GetBody; get != nil {
		out.GetBody = func() (io.ReadCloser, error) {
			rc, err := get()
			if err != nil || rc == nil || rc == http.NoBody {
				return rc, err
			}
			return &progressBody{ReadCloser: rc, w: w}, nil
		}
	}
	return out
}

// Request body whose reads restart the write limit, each read meaning the connection took the bytes before it
type progressBody struct {
	io.ReadCloser
	w *watchdog
}

func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.w.progress(PhaseWrite)
	}
	return n, err
}

// Response body each of whose reads must return within the read limit, time between reads not counted, Close ending the exchange
type watchedBody struct {
	rc     io.ReadCloser
	w      *watchdog
	cancel context.CancelFunc
}

func (b *watchedBody) Read(p []byte) (int, error) {
	b.w.enter(PhaseRead)
	n, err := b.rc.Read(p)
	switch {
	case err == nil:
		b.w.pause()
	case err == io.EOF:
		b.w.stop()
	default:
		b.w.stop()
		if te := b.w.timedOut(); te != nil {
			return n, te
		}
	}
	return n, err
}

func (b *watchedBody) Close() error {
	b.w.stop()
	err := b.rc.Close()
	b.cancel()
	return err
}
