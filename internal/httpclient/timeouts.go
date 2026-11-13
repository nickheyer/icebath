package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http/httptrace"
	"net/url"
	"sync"
	"time"
)

// Limit of each request phase, zero meaning unbounded, Exchange bounding one request from dial to its final byte
type Timeouts struct {
	Connect  time.Duration
	TLS      time.Duration
	Write    time.Duration
	Header   time.Duration
	Idle     time.Duration
	Pool     time.Duration
	Exchange time.Duration
}

// Every phase bounded by the same duration and no exchange limit, the shape of a single request timeout
func EachPhase(d time.Duration) Timeouts {
	return Timeouts{Connect: d, TLS: d, Write: d, Header: d, Idle: d, Pool: d}
}

// Connection reuse per target host, zero fields taking 20 idle, 100 open and five idle seconds
type Pool struct {
	MaxIdle     int
	MaxOpen     int
	IdleTimeout time.Duration
}

// Pool with defaults put into its zero fields
func (p Pool) filled() Pool {
	if p.MaxIdle <= 0 {
		p.MaxIdle = 20
	}
	if p.MaxOpen <= 0 {
		p.MaxOpen = 100
	}
	if p.IdleTimeout <= 0 {
		p.IdleTimeout = 5 * time.Second
	}
	return p
}

type exchangeKey struct{}

// One request attempt with the phase it is in and the watchdog bounding that phase
type exchange struct {
	host     string
	forward  *url.URL
	limits   Timeouts
	parent   context.Context
	bounded  context.Context
	cancel   context.CancelFunc
	release  context.CancelFunc
	mu       sync.Mutex
	phase    Phase
	timer    *time.Timer
	deadline time.Time
	armed    time.Duration
	fired    Phase
	once     sync.Once
}

// Records that a dial for the attempt reached a phase the dial bounds itself, ending the pool wait
func markPhase(ctx context.Context, p Phase) {
	if x, ok := ctx.Value(exchangeKey{}).(*exchange); ok {
		x.advance(p)
	}
}

// Moves a connection wait forward to a dial phase, ignored once the attempt holds a connection
func (x *exchange) advance(p Phase) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.fired != 0 || x.phase >= PhaseWrite || p <= x.phase {
		return
	}
	x.phase, x.armed, x.deadline = p, 0, time.Time{}
	if x.timer != nil {
		x.timer.Stop()
	}
}

// Enters a phase the transport reports, bounding it by d when d is positive
func (x *exchange) arm(p Phase, d time.Duration) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.fired != 0 {
		return
	}
	x.phase = p
	x.armed = d
	if d <= 0 {
		x.deadline = time.Time{}
		if x.timer != nil {
			x.timer.Stop()
		}
		return
	}
	x.deadline = time.Now().Add(d)
	if x.timer == nil {
		x.timer = time.AfterFunc(d, x.fire)
		return
	}
	x.timer.Reset(d)
}

// Cancels the attempt when the armed phase outlived its deadline, ignoring stale timer runs
func (x *exchange) fire() {
	x.mu.Lock()
	if x.fired != 0 || x.deadline.IsZero() || time.Now().Before(x.deadline) {
		x.mu.Unlock()
		return
	}
	x.fired = x.phase
	x.mu.Unlock()
	x.cancel()
}

// Stops the watchdog and releases the attempt context once
func (x *exchange) finish() {
	x.once.Do(func() {
		x.mu.Lock()
		x.deadline = time.Time{}
		if x.timer != nil {
			x.timer.Stop()
		}
		x.mu.Unlock()
		x.cancel()
		if x.release != nil {
			x.release()
		}
	})
}

// Hooks moving the attempt through its phases as the transport reports progress
func (x *exchange) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { x.arm(PhasePool, x.limits.Pool) },
		GotConn: func(httptrace.GotConnInfo) { x.arm(PhaseWrite, 0) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				x.arm(PhaseHeader, x.limits.Header)
			}
		},
	}
}

// Failure of the attempt tagged with the phase it happened in, a fired watchdog reported as its timeout
func (x *exchange) wrap(err error) error {
	var known *Error
	if errors.As(err, &known) {
		return err
	}
	x.mu.Lock()
	fired, phase, armed := x.fired, x.phase, x.armed
	x.mu.Unlock()
	if fired != 0 {
		return &Error{Phase: fired, Host: x.host, Err: &deadlineError{after: armed}}
	}
	if phase == 0 {
		phase = PhasePool
	}
	if x.parent.Err() == nil && errors.Is(x.bounded.Err(), context.DeadlineExceeded) {
		return &Error{Phase: phase, Host: x.host, Err: &deadlineError{after: x.limits.Exchange}}
	}
	var pe *ProxyError
	switch {
	case errors.As(err, &pe):
		phase = PhaseProxy
	case x.forward != nil && phase >= PhaseConnect && phase <= PhaseTLS:
		phase = PhaseProxy
		err = &ProxyError{Proxy: proxyLabel(x.forward), Err: err}
	}
	return &Error{Phase: phase, Host: x.host, Err: err}
}

// Response body whose reads restart the idle deadline and whose end releases the attempt
type watchedBody struct {
	rc io.ReadCloser
	x  *exchange
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	switch {
	case err == nil:
		b.x.arm(PhaseBody, b.x.limits.Idle)
	case err == io.EOF:
		b.x.finish()
	default:
		err = b.x.wrap(err)
		b.x.finish()
	}
	return n, err
}

func (b *watchedBody) Close() error {
	err := b.rc.Close()
	b.x.finish()
	return err
}
