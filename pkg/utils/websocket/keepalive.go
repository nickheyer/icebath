package websocketutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Pings the peer after each ping interval of silence and fails the connection with CloseInternalError when nothing at all comes from it within the ping timeout; the time the reader waits for Receive to take a message is not silence; stops once the connection is closing or closed
func (c *Conn) keepalive() {
	interval, timeout := c.lim.pingInterval, c.lim.pingTimeout
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		if !c.wait(timer) {
			return
		}
		if c.stalled.Load() {
			timer.Reset(interval)
			continue
		}
		if idle := c.act.idle(); idle < interval {
			timer.Reset(interval - idle)
			continue
		}
		sentAt := c.act.now()
		if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(timeout)); err != nil {
			c.pingFailed(err)
			return
		}
		timer.Reset(timeout)
		if !c.wait(timer) {
			return
		}
		if !c.act.since(sentAt) && !c.stalled.Load() {
			c.fail(CloseInternalError, "keepalive ping timeout", ErrKeepaliveTimeout)
			return
		}
		timer.Reset(max(interval-c.act.idle(), 0))
	}
}

// Ends the connection on a keepalive ping that could not be written, one that ran out of the ping timeout counting as a keepalive timeout
func (c *Conn) pingFailed(err error) {
	if errors.Is(err, websocket.ErrCloseSent) {
		return
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		c.fail(CloseInternalError, "keepalive ping timeout", fmt.Errorf("%w: %w", ErrKeepaliveTimeout, err))
		return
	}
	c.finish(err)
}

// Waits for the timer, false once the connection is closing or closed
func (c *Conn) wait(timer *time.Timer) bool {
	select {
	case <-timer.C:
		return true
	case <-c.closing:
		return false
	case <-c.done:
		return false
	}
}

// Time of the last bytes from the peer as an offset from a fixed start, so the monotonic clock measures it
type activity struct {
	start time.Time
	last  atomic.Int64
}

func newActivity() *activity { return &activity{start: time.Now()} }

// Records bytes from the peer at this moment
func (a *activity) mark() { a.last.Store(int64(time.Since(a.start))) }

// Offset of this moment from the start
func (a *activity) now() time.Duration { return time.Since(a.start) }

// Time since the last bytes from the peer
func (a *activity) idle() time.Duration { return time.Since(a.start) - time.Duration(a.last.Load()) }

// Whether bytes came from the peer at or after the offset
func (a *activity) since(at time.Duration) bool { return time.Duration(a.last.Load()) >= at }

// Dial function whose connections mark the activity on every read that brings bytes, below TLS and any proxy, so a frame still arriving counts as life
func (a *activity) wrap(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &liveConn{Conn: conn, act: a}, nil
	}
}

// Network connection that marks the activity each time a read returns bytes from the peer
type liveConn struct {
	net.Conn
	act *activity
}

func (l *liveConn) Read(p []byte) (int, error) {
	n, err := l.Conn.Read(p)
	if n > 0 {
		l.act.mark()
	}
	return n, err
}
